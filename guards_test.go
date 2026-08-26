package main

import (
	"context"
	"os"
	"strings"
	"testing"
)

func fraction(f float64) *float64 { return &f }

func TestLoadSafetyConfigFromFileAndEnv(t *testing.T) {
	isolateState(t)
	original := configPath
	configPath = writeConfig(t, `
[play.safety]
production_lock = true
max_rollout_fraction = 0.2
blocked_operations = ["delete_listing", "halt_release"]
`)
	t.Cleanup(func() { configPath = original })

	cfg := loadSafetyConfig()
	if !cfg.ProductionLock || cfg.MaxRolloutFraction != 0.2 {
		t.Fatalf("file settings did not load: %+v", cfg)
	}
	if len(cfg.BlockedOperations) != 2 {
		t.Fatalf("blocked operations = %v", cfg.BlockedOperations)
	}

	// The environment wins, exactly like the rest of the configuration.
	t.Setenv("PLAY_PRODUCTION_LOCK", "0")
	t.Setenv("PLAY_MAX_ROLLOUT_FRACTION", "0.5")
	t.Setenv("PLAY_BLOCKED_OPS", "update_listing")
	cfg = loadSafetyConfig()
	if cfg.ProductionLock {
		t.Error("PLAY_PRODUCTION_LOCK=0 should turn the lock off")
	}
	if cfg.MaxRolloutFraction != 0.5 {
		t.Errorf("max rollout fraction = %v", cfg.MaxRolloutFraction)
	}
	// The env list replaces the file list rather than adding to it: a CI job
	// that names its own blocks should not silently inherit a laptop's.
	if len(cfg.BlockedOperations) != 1 || cfg.BlockedOperations[0] != "update_listing" {
		t.Errorf("blocked operations = %v", cfg.BlockedOperations)
	}
}

// TestUnparseableGuardsAreIgnoredLoudly: inventing a cap the user did not write
// is worse than running without one they thought they set, so it warns.
func TestUnparseableGuardsAreIgnoredLoudly(t *testing.T) {
	isolateState(t)
	warnings := captureWarnings(t)
	original := configPath
	configPath = writeConfig(t, "")
	t.Cleanup(func() { configPath = original })

	t.Setenv("PLAY_MAX_ROLLOUT_FRACTION", "twenty percent")
	t.Setenv("PLAY_PRODUCTION_LOCK", "maybe")
	cfg := loadSafetyConfig()
	if cfg.MaxRolloutFraction != 0 || cfg.ProductionLock {
		t.Errorf("unparseable guards should not take effect: %+v", cfg)
	}
	for _, want := range []string{"PLAY_MAX_ROLLOUT_FRACTION", "PLAY_PRODUCTION_LOCK"} {
		if !strings.Contains(warnings.String(), want) {
			t.Errorf("expected a warning naming %s:\n%s", want, warnings.String())
		}
	}
}

// TestOutOfRangeRolloutCapIsIgnored: a cap outside (0, 1] cannot be satisfied
// by any real rollout and would block every release write.
func TestOutOfRangeRolloutCapIsIgnored(t *testing.T) {
	isolateState(t)
	captureWarnings(t)
	original := configPath
	configPath = writeConfig(t, "[play.safety]\nmax_rollout_fraction = 20\n")
	t.Cleanup(func() { configPath = original })

	if cfg := loadSafetyConfig(); cfg.MaxRolloutFraction != 0 {
		t.Errorf("max rollout fraction = %v, want it ignored", cfg.MaxRolloutFraction)
	}
}

func TestCheckRolloutFraction(t *testing.T) {
	capped := SafetyConfig{MaxRolloutFraction: 0.2}
	tests := []struct {
		name     string
		fraction *float64
		cfg      SafetyConfig
		wantErr  bool
	}{
		{"no cap allows anything", fraction(1), SafetyConfig{}, false},
		{"under the cap", fraction(0.1), capped, false},
		{"at the cap", fraction(0.2), capped, false},
		{"over the cap", fraction(0.5), capped, true},
		{"completing a rollout is over the cap", fraction(1), capped, true},
		{"a write with no fraction is unaffected", nil, capped, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkRolloutFraction(tc.fraction, tc.cfg)
			if tc.wantErr != (err != nil) {
				t.Fatalf("checkRolloutFraction = %v", err)
			}
			if err != nil && !strings.Contains(err.Error(), "max_rollout_fraction") {
				t.Errorf("error should name the setting: %v", err)
			}
		})
	}
}

func TestCheckBlockedOperation(t *testing.T) {
	cfg := SafetyConfig{BlockedOperations: []string{"halt_release", " delete_listing "}}
	for _, tool := range []string{"halt_release", "delete_listing"} {
		if err := checkBlockedOperation(tool, cfg); err == nil {
			t.Errorf("%s should be blocked", tool)
		}
	}
	if err := checkBlockedOperation("update_release", cfg); err != nil {
		t.Errorf("update_release should be allowed: %v", err)
	}
}

func TestRequiresDoubleConfirmation(t *testing.T) {
	tests := []struct {
		name string
		w    pendingWrite
		cfg  SafetyConfig
		want bool
	}{
		{
			name: "an ordinary staged rollout takes one confirmation",
			w:    pendingWrite{Tool: "update_release", Track: productionTrack, RolloutFraction: fraction(0.1)},
			want: false,
		},
		{
			// A halt pulls a release from users who already have it.
			name: "halting a rollout",
			w:    pendingWrite{Tool: "halt_release", Track: productionTrack},
			want: true,
		},
		{
			name: "deleting a listing",
			w:    pendingWrite{Tool: "delete_listing"},
			want: true,
		},
		{
			name: "deleting images",
			w:    pendingWrite{Tool: "delete_images"},
			want: true,
		},
		{
			name: "completing a production rollout",
			w:    pendingWrite{Tool: "complete_release", Track: productionTrack},
			want: true,
		},
		{
			// Reaching 100% by fraction is the same act as completing.
			name: "a production write to the full fraction",
			w:    pendingWrite{Tool: "update_release", Track: productionTrack, RolloutFraction: fraction(1)},
			want: true,
		},
		{
			// Finishing an internal-track rollout affects testers, not users.
			name: "completing a non-production rollout",
			w:    pendingWrite{Tool: "complete_release", Track: "internal"},
			want: false,
		},
		{
			name: "the production lock escalates any production write",
			w:    pendingWrite{Tool: "update_release", Track: productionTrack, RolloutFraction: fraction(0.05)},
			cfg:  SafetyConfig{ProductionLock: true},
			want: true,
		},
		{
			name: "the production lock leaves other tracks alone",
			w:    pendingWrite{Tool: "update_release", Track: "beta"},
			cfg:  SafetyConfig{ProductionLock: true},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := requiresDoubleConfirmation(tc.w, tc.cfg); got != tc.want {
				t.Errorf("requiresDoubleConfirmation = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBlockedOperationCannotBeStaged: refusing before a token exists means the
// user is never handed a confirmation they cannot use.
func TestBlockedOperationCannotBeStaged(t *testing.T) {
	isolateState(t)
	t.Setenv("PLAY_BLOCKED_OPS", "halt_release")

	_, err := stageWrite(pendingWrite{
		Platform: playPlatformName, Tool: "halt_release",
		PackageName: "com.example.app", Summary: "halt production",
	})
	if err == nil {
		t.Fatal("a blocked operation should not be stageable")
	}
	if !strings.Contains(err.Error(), "blocked_operations") {
		t.Errorf("error should name the setting: %v", err)
	}
}

// TestBlockAddedAfterPreviewStillStops is the guard the confirm path exists
// for: a ten-minute token must not outrun a policy change.
func TestBlockAddedAfterPreviewStillStops(t *testing.T) {
	isolateState(t)
	p := stagePlayWrite(t, pendingWrite{
		Tool: "update_release", PackageName: "com.example.app",
		Summary: "set production to 42 at 10%", Track: productionTrack,
	})

	t.Setenv("PLAY_BLOCKED_OPS", "update_release")
	applier := &fakeApplier{}

	if _, err := applyConfirmed(context.Background(), applier, "update_release", p.Token); err == nil {
		t.Fatal("a newly blocked operation must not apply")
	}
	if len(applier.applied) != 0 {
		t.Error("nothing may be applied once the operation is blocked")
	}

	// `rollout confirm` must enforce it too — and on a peek, so the token
	// survives for after the block is lifted.
	p2 := stagePlayWriteAllowingBlocks(t, pendingWrite{
		Tool: "update_release", PackageName: "com.example.app",
		Summary: "set production to 42 at 10%", Track: productionTrack,
	})
	if _, err := runConfirm(context.Background(), applier, p2.Token); err == nil {
		t.Fatal("`rollout confirm` must enforce a new block too")
	}
	if _, err := peekMutation(p2.Token); err != nil {
		t.Errorf("a blocked confirm must not burn the token: %v", err)
	}
}

// TestTightenedRolloutCapStopsAStagedWrite: the cap in force now is the one
// that counts.
func TestTightenedRolloutCapStopsAStagedWrite(t *testing.T) {
	isolateState(t)
	p := stagePlayWrite(t, pendingWrite{
		Tool: "update_release", PackageName: "com.example.app",
		Summary: "set production to 42 at 50%", Track: productionTrack,
		RolloutFraction: fraction(0.5),
	})

	t.Setenv("PLAY_MAX_ROLLOUT_FRACTION", "0.2")
	applier := &fakeApplier{}
	_, err := applyConfirmed(context.Background(), applier, "update_release", p.Token)
	if err == nil || !strings.Contains(err.Error(), "max_rollout_fraction") {
		t.Fatalf("err = %v, want the cap to stop the write", err)
	}
	if len(applier.applied) != 0 {
		t.Error("nothing may be applied over the cap")
	}
}

// TestProductionLockTurnedOnAfterPreviewEscalates: the write is not refused,
// but it now takes the second confirmation the operator asked for.
func TestProductionLockTurnedOnAfterPreviewEscalates(t *testing.T) {
	isolateState(t)
	p := stagePlayWrite(t, pendingWrite{
		Tool: "update_release", PackageName: "com.example.app",
		Summary: "set production to 42 at 10%", Track: productionTrack,
		RolloutFraction: fraction(0.1),
	})
	if p.RequiresDouble {
		t.Fatal("an ordinary staged rollout should take one confirmation")
	}

	t.Setenv("PLAY_PRODUCTION_LOCK", "1")
	applier := &fakeApplier{}
	res, err := applyConfirmed(context.Background(), applier, "update_release", p.Token)
	if err != nil {
		t.Fatalf("applyConfirmed: %v", err)
	}
	if res.Applied {
		t.Fatal("the production lock should have escalated this write")
	}
	if res.ConfirmToken == "" {
		t.Fatal("expected a second token")
	}
	if len(applier.applied) != 0 {
		t.Error("nothing may be applied before the second confirmation")
	}

	if _, err := applyConfirmed(context.Background(), applier, "update_release", res.ConfirmToken); err != nil {
		t.Fatalf("second confirm: %v", err)
	}
	if len(applier.applied) != 1 {
		t.Errorf("expected exactly one apply, got %d", len(applier.applied))
	}
}

// TestRolloutCapRefusesAtStagingTime: over-cap writes never get a token.
func TestRolloutCapRefusesAtStagingTime(t *testing.T) {
	isolateState(t)
	t.Setenv("PLAY_MAX_ROLLOUT_FRACTION", "0.2")

	_, err := stageWrite(pendingWrite{
		Platform: playPlatformName, Tool: "complete_release",
		PackageName: "com.example.app", Summary: "complete the rollout",
		Track: productionTrack, RolloutFraction: fraction(1),
	})
	if err == nil {
		t.Fatal("a write over the cap should not be stageable")
	}
}

// stagePlayWriteAllowingBlocks stages a write with the guard rails temporarily
// relaxed, so a test can then tighten them and confirm the token.
func stagePlayWriteAllowingBlocks(t *testing.T, w pendingWrite) *PendingMutation {
	t.Helper()
	blocked := os.Getenv("PLAY_BLOCKED_OPS")
	t.Setenv("PLAY_BLOCKED_OPS", "")
	p := stagePlayWrite(t, w)
	t.Setenv("PLAY_BLOCKED_OPS", blocked)
	return p
}
