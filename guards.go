package main

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// Guard rails sit between a staged write and the API. They are pure functions
// over an explicit SafetyConfig so they are trivially testable, and they are
// re-evaluated on every path that can still apply a write — tightening a guard
// between a preview and its confirmation has to be enforced, or the guard is
// only as strong as the ten-minute token that outran it.

// SafetyConfig holds the write guard rails. Every value is off by default: a
// tool that refuses to ship a release out of the box would be worked around
// rather than configured.
type SafetyConfig struct {
	// ProductionLock escalates every write touching the production track to a
	// second confirmation. PLAY_PRODUCTION_LOCK=1 / [play.safety]
	// production_lock.
	ProductionLock bool
	// MaxRolloutFraction caps the staged-rollout fraction a write may set,
	// as a value in (0, 1]. Zero means no cap. Set to 0.2 in CI and a pipeline
	// can stage and grow a rollout but never finish one.
	MaxRolloutFraction float64
	// BlockedOperations are tool names that are refused outright.
	BlockedOperations []string
}

// PlaySafetyConfig is the `[play.safety]` TOML table.
type PlaySafetyConfig struct {
	ProductionLock     bool     `toml:"production_lock"`
	MaxRolloutFraction float64  `toml:"max_rollout_fraction"`
	BlockedOperations  []string `toml:"blocked_operations"`
}

// loadSafetyConfig resolves the guard rails from the config file and the
// environment, environment last, exactly like the rest of the configuration.
//
// A guard that cannot be parsed is ignored rather than defaulted to something
// stricter or looser: silently inventing a cap the user did not write is worse
// than running without one they thought they had set, and the warning says so.
func loadSafetyConfig() SafetyConfig {
	var file struct {
		Play struct {
			Safety PlaySafetyConfig `toml:"safety"`
		} `toml:"play"`
	}
	// A config file that cannot be read is reported by every other command;
	// here it just means no file-supplied guards.
	_, _ = decodeConfigFile(configPath, &file)

	cfg := SafetyConfig{
		ProductionLock:     file.Play.Safety.ProductionLock,
		MaxRolloutFraction: file.Play.Safety.MaxRolloutFraction,
		BlockedOperations:  file.Play.Safety.BlockedOperations,
	}

	if v := strings.TrimSpace(os.Getenv("PLAY_PRODUCTION_LOCK")); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.ProductionLock = b
		} else {
			warnOnce("PLAY_PRODUCTION_LOCK=%q is not a boolean — ignoring it. Use 1 or 0.", v)
		}
	}
	if v := strings.TrimSpace(os.Getenv("PLAY_MAX_ROLLOUT_FRACTION")); v != "" {
		if f, ok := parseFraction(v); ok {
			cfg.MaxRolloutFraction = f
		} else {
			warnOnce("PLAY_MAX_ROLLOUT_FRACTION=%q is not a fraction between 0 and 1 — ignoring it, so no rollout cap is in force.", v)
		}
	}
	if v := strings.TrimSpace(os.Getenv("PLAY_BLOCKED_OPS")); v != "" {
		cfg.BlockedOperations = nil
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				cfg.BlockedOperations = append(cfg.BlockedOperations, p)
			}
		}
	}
	// A cap outside (0, 1] cannot be satisfied by any real rollout, and would
	// block every release write with a message about a value the user thought
	// was reasonable.
	if cfg.MaxRolloutFraction != 0 && !validFraction(cfg.MaxRolloutFraction) {
		warnOnce("max_rollout_fraction %v is not between 0 and 1 — ignoring it, so no rollout cap is in force.", cfg.MaxRolloutFraction)
		cfg.MaxRolloutFraction = 0
	}
	return cfg
}

// parseFraction accepts a rollout fraction: a finite number in (0, 1].
func parseFraction(v string) (float64, bool) {
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || !validFraction(f) {
		return 0, false
	}
	return f, true
}

func validFraction(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0) && f > 0 && f <= 1
}

// productionTrack is the track name Play gives the live one. It is the only
// track whose writes reach every user at once, which is why the guards single
// it out by name.
const productionTrack = "production"

// checkBlockedOperation refuses a tool the configuration has blocked.
//
// It runs before the confirm branch on every path, so a block added after a
// preview was staged still stops that preview from applying — a guard that only
// looked at fresh calls would be bypassed by any token already in flight.
func checkBlockedOperation(tool string, cfg SafetyConfig) error {
	for _, blocked := range cfg.BlockedOperations {
		if strings.TrimSpace(blocked) == tool {
			return fmt.Errorf("%s is blocked by configuration (blocked_operations / PLAY_BLOCKED_OPS) — remove it there to allow this write", tool)
		}
	}
	return nil
}

// checkRolloutFraction refuses a write that would take a staged rollout past
// the configured cap.
func checkRolloutFraction(fraction *float64, cfg SafetyConfig) error {
	if cfg.MaxRolloutFraction == 0 || fraction == nil {
		return nil
	}
	// Written so a NaN fraction fails rather than slipping through the
	// comparison, which is always false against NaN.
	if !(*fraction <= cfg.MaxRolloutFraction) {
		return fmt.Errorf("a rollout to %s exceeds the configured maximum of %s (max_rollout_fraction / PLAY_MAX_ROLLOUT_FRACTION) — raise the cap to go further",
			formatFraction(*fraction), formatFraction(cfg.MaxRolloutFraction))
	}
	return nil
}

// formatFraction renders a rollout fraction the way the Console does.
func formatFraction(f float64) string {
	return strconv.FormatFloat(f*100, 'g', -1, 64) + "%"
}

// requiresDoubleConfirmation reports whether a write is destructive enough to
// take two confirmations.
//
// The list is short on purpose. Every one of these either removes something the
// API cannot restore, or ends a rollout in a way that cannot be undone by
// running the opposite command: a halted release is not resumable back to the
// users who already lost it, and a completed rollout cannot be un-completed.
func requiresDoubleConfirmation(w pendingWrite, cfg SafetyConfig) bool {
	switch {
	// A halt pulls a release from users who already have it. There is a resume,
	// but it does not undo the hours the release was gone.
	case strings.Contains(w.Tool, "halt"):
		return true
	// Deletions: a store listing or an image set the API cannot bring back.
	case strings.Contains(w.Tool, "delete"):
		return true
	// Finishing a production rollout: every remaining user gets the release,
	// and there is no lower fraction to fall back to.
	case w.Track == productionTrack && completesRollout(w):
		return true
	// The explicit lock: the operator has said production writes are a
	// two-step decision here.
	case cfg.ProductionLock && w.Track == productionTrack:
		return true
	default:
		return false
	}
}

// completesRollout reports whether a write takes a release to every user —
// either by name (complete_release) or by asking for the full fraction.
func completesRollout(w pendingWrite) bool {
	if strings.Contains(w.Tool, "complete") {
		return true
	}
	return w.RolloutFraction != nil && *w.RolloutFraction >= 1
}

// enforceGuards re-applies the guard rails to an already-staged write, just
// before it would be applied.
//
// It runs on both confirm paths — `--confirm <token>` and
// `rollout confirm <token>` — because the configuration in force *now* is the
// one that counts. A blocked operation or a tightened rollout cap added after
// the preview was handed out must still stop it, and a production lock turned
// on in the meantime escalates the write to a second confirmation rather than
// letting the ten-minute token outrun the policy.
func enforceGuards(p *PendingMutation, cfg SafetyConfig) error {
	if err := checkBlockedOperation(p.Tool, cfg); err != nil {
		return err
	}
	if err := checkRolloutFraction(p.RolloutFraction, cfg); err != nil {
		return err
	}
	if !p.DoubleConfirmed && requiresDoubleConfirmation(p.asPendingWrite(), cfg) {
		p.RequiresDouble = true
	}
	return nil
}

// asPendingWrite recovers the guard-relevant facts a staged mutation was
// created from, so the guards read the same shape whether they run at staging
// time or at confirm time.
func (p *PendingMutation) asPendingWrite() pendingWrite {
	return pendingWrite{
		Platform:        p.Platform,
		Tool:            p.Tool,
		PackageName:     p.PackageName,
		Summary:         p.Summary,
		Dispatch:        p.Dispatch,
		Track:           p.Track,
		RolloutFraction: p.RolloutFraction,
	}
}
