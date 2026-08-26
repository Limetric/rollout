package main

import (
	"fmt"
	"strconv"
	"strings"
)

// The shape of a track release, and the client-side rules about it that are
// worth catching before an edit is ever opened.

// Release status values, as the API spells them.
const (
	statusDraft      = "draft"
	statusInProgress = "inProgress"
	statusCompleted  = "completed"
	statusHalted     = "halted"
)

var releaseStatuses = []string{statusDraft, statusInProgress, statusCompleted, statusHalted}

// trackRelease is the API's Track.Release resource. It is built here rather
// than assembled ad hoc so every write produces the same shape.
type trackRelease struct {
	Name                string            `json:"name,omitempty"`
	VersionCodes        []string          `json:"versionCodes,omitempty"`
	Status              string            `json:"status,omitempty"`
	UserFraction        *float64          `json:"userFraction,omitempty"`
	InAppUpdatePriority int               `json:"inAppUpdatePriority,omitempty"`
	ReleaseNotes        []releaseNote     `json:"releaseNotes,omitempty"`
	CountryTargeting    *countryTargeting `json:"countryTargeting,omitempty"`
}

// countryTargeting is how Play scopes a release to a set of countries. There is
// no track-level country update in the Publisher API; availability is a
// property of the release.
type countryTargeting struct {
	Countries []string `json:"countries,omitempty"`
	// IncludeRestOfWorld ships to every country not named above.
	IncludeRestOfWorld bool `json:"includeRestOfWorld,omitempty"`
}

// parseStatus validates a user-supplied release status.
func parseStatus(s string) (string, error) {
	trimmed := strings.TrimSpace(s)
	for _, known := range releaseStatuses {
		if strings.EqualFold(trimmed, known) {
			return known, nil
		}
	}
	return "", fmt.Errorf("invalid status %q — expected one of: %s", s, strings.Join(releaseStatuses, ", "))
}

// validateRolloutForStatus enforces the pairing the API insists on, before an
// edit is opened.
//
// Play rejects `completed` or `draft` carrying a userFraction, and rejects
// `inProgress` without one — both with a generic invalid-argument error at
// commit time, after the whole staging dance. The rules are worth stating here
// because they are also the two mistakes people actually make: "roll out to
// 100%" is `completed` with no fraction, not `inProgress` at 1.0.
func validateRolloutForStatus(status string, rollout *float64) error {
	switch status {
	case statusInProgress, statusHalted:
		if rollout == nil {
			return fmt.Errorf("status %q needs a rollout fraction — pass --rollout 0.1 for 10%% of users", status)
		}
		if *rollout <= 0 || *rollout >= 1 {
			return fmt.Errorf("--rollout must be between 0 and 1 (exclusive); to reach every user use --status %s, which takes no fraction", statusCompleted)
		}
	case statusCompleted, statusDraft:
		if rollout != nil {
			return fmt.Errorf("status %q does not take a rollout fraction — %s reaches every user, and %s reaches none", status, statusCompleted, statusDraft)
		}
	}
	return nil
}

// parseVersionCodes splits and validates a comma-separated version code list.
// A release may carry several (a multi-APK release), but every one of them has
// to be a number: the API's error for a non-numeric code names neither the code
// nor the field.
func parseVersionCodes(list []string) ([]string, error) {
	var codes []string
	for _, item := range list {
		for _, part := range strings.Split(item, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if _, ok := parseVersionCode(part); !ok {
				return nil, fmt.Errorf("version code %q is not a number — version codes are the integer versionCode from the manifest, not a version name like 1.2.3", part)
			}
			codes = append(codes, part)
		}
	}
	if len(codes) == 0 {
		return nil, fmt.Errorf("no version codes given — pass --version-codes 42 (see `rollout play artifacts` for what has been uploaded)")
	}
	return codes, nil
}

func parseVersionCode(s string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// describeRelease renders a release for a preview line, in the words the
// Console uses.
func describeRelease(r trackRelease) string {
	parts := []string{r.Status}
	if len(r.VersionCodes) > 0 {
		parts = append(parts, "version "+strings.Join(r.VersionCodes, "+"))
	}
	if r.UserFraction != nil {
		parts = append(parts, formatFraction(*r.UserFraction)+" of users")
	}
	if r.Name != "" {
		parts = append(parts, "named "+strconv.Quote(r.Name))
	}
	if locales := releaseNoteLocales(r.ReleaseNotes); len(locales) > 0 {
		parts = append(parts, "notes in "+strings.Join(locales, ", "))
	}
	if r.CountryTargeting != nil {
		scope := strings.Join(r.CountryTargeting.Countries, ", ")
		if r.CountryTargeting.IncludeRestOfWorld {
			scope += " plus the rest of the world"
		}
		parts = append(parts, "available in "+strings.TrimPrefix(scope, ", "))
	}
	return strings.Join(parts, ", ")
}
