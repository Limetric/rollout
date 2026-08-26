package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestParseReleaseNotes(t *testing.T) {
	notes, err := parseReleaseNotes([]string{"en-US=Bug fixes", "nl-NL=Foutoplossingen = beter"})
	if err != nil {
		t.Fatalf("parseReleaseNotes: %v", err)
	}
	if len(notes) != 2 || notes[0].Language != "en-US" || notes[0].Text != "Bug fixes" {
		t.Fatalf("unexpected notes: %+v", notes)
	}
	// Only the first `=` separates; release notes contain equals signs.
	if notes[1].Text != "Foutoplossingen = beter" {
		t.Errorf("text was truncated at the second =: %q", notes[1].Text)
	}

	for _, bad := range []string{"Bug fixes", "=Bug fixes"} {
		if _, err := parseReleaseNotes([]string{bad}); err == nil {
			t.Errorf("%q should have been rejected", bad)
		}
	}
}

func TestReadReleaseNotesDirFlatLayout(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "en-US.txt"), "Bug fixes\n")
	writeFile(t, filepath.Join(dir, "nl-NL.txt"), "Foutoplossingen\n")

	notes, err := readReleaseNotesDir(dir, "")
	if err != nil {
		t.Fatalf("readReleaseNotesDir: %v", err)
	}
	if len(notes) != 2 {
		t.Fatalf("got %+v", notes)
	}
	// Sorted, so a preview reads the same way twice.
	if notes[0].Language != "en-US" || notes[1].Language != "nl-NL" {
		t.Errorf("notes are not sorted: %+v", notes)
	}
	// The trailing newline every editor adds is not part of the note.
	if notes[0].Text != "Bug fixes" {
		t.Errorf("trailing newline survived: %q", notes[0].Text)
	}
}

// TestReadReleaseNotesDirFastlaneLayout: almost every existing Android pipeline
// already has this tree on disk, and asking those users to reshape it would be
// a poor trade for adopting this tool.
func TestReadReleaseNotesDirFastlaneLayout(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "en-US", "changelogs", "9.txt"), "Old news")
	writeFile(t, filepath.Join(dir, "en-US", "changelogs", "10.txt"), "New news")
	writeFile(t, filepath.Join(dir, "nl-NL", "changelogs", "10.txt"), "Nieuw nieuws")
	// A locale with listing text but no changelogs is common and must not fail
	// the whole read.
	writeFile(t, filepath.Join(dir, "de-DE", "title.txt"), "Beispiel")

	t.Run("picks the named version code", func(t *testing.T) {
		notes, err := readReleaseNotesDir(dir, "9")
		if err != nil {
			t.Fatalf("readReleaseNotesDir: %v", err)
		}
		if len(notes) != 1 || notes[0].Text != "Old news" {
			t.Fatalf("unexpected notes: %+v", notes)
		}
	})

	t.Run("defaults to the highest version code", func(t *testing.T) {
		notes, err := readReleaseNotesDir(dir, "")
		if err != nil {
			t.Fatalf("readReleaseNotesDir: %v", err)
		}
		if len(notes) != 2 {
			t.Fatalf("unexpected notes: %+v", notes)
		}
		// 10 > 9 numerically, which lexicographic order gets wrong.
		if notes[0].Text != "New news" {
			t.Errorf("picked the wrong changelog: %+v", notes)
		}
	})
}

func TestReadReleaseNotesDirErrors(t *testing.T) {
	if _, err := readReleaseNotesDir(filepath.Join(t.TempDir(), "nope"), ""); err == nil {
		t.Error("a missing directory should be an error")
	}
	empty := t.TempDir()
	err := func() error { _, err := readReleaseNotesDir(empty, ""); return err }()
	if err == nil || !strings.Contains(err.Error(), "fastlane") {
		t.Errorf("an empty directory should name both layouts: %v", err)
	}
}

// TestValidateReleaseNotesNamesTheLocale: the API rejects the commit with a
// message about "releaseNotes" and no indication which of twenty locales is
// over, after an edit has already been opened.
func TestValidateReleaseNotesNamesTheLocale(t *testing.T) {
	long := strings.Repeat("x", maxReleaseNoteRunes+1)
	err := validateReleaseNotes([]releaseNote{{Language: "en-US", Text: "fine"}, {Language: "de-DE", Text: long}})
	if err == nil {
		t.Fatal("expected an over-length note to be rejected")
	}
	if !strings.Contains(err.Error(), "de-DE") {
		t.Errorf("error should name the locale: %v", err)
	}

	// The limit is runes, not bytes: 500 emoji is legal, 500 bytes of them is
	// not the same thing.
	if err := validateReleaseNotes([]releaseNote{{Language: "ja-JP", Text: strings.Repeat("あ", maxReleaseNoteRunes)}}); err != nil {
		t.Errorf("a 500-rune note should be accepted: %v", err)
	}

	if err := validateReleaseNotes([]releaseNote{{Language: "en-US", Text: "a"}, {Language: "en-US", Text: "b"}}); err == nil {
		t.Error("duplicate locales should be rejected")
	}
	if err := validateReleaseNotes([]releaseNote{{Text: "no locale"}}); err == nil {
		t.Error("a note without a locale should be rejected")
	}
}

func TestValidateRolloutForStatus(t *testing.T) {
	half := 0.5
	full := 1.0
	tests := []struct {
		name    string
		status  string
		rollout *float64
		wantErr string
	}{
		{"a staged rollout needs a fraction", statusInProgress, nil, "--rollout"},
		{"a staged rollout takes a fraction", statusInProgress, &half, ""},
		// "roll out to 100%" is `completed`, not `inProgress` at 1.0 — the
		// most common way people get this wrong.
		{"a fraction of 1 is not a staged rollout", statusInProgress, &full, "completed"},
		{"completed takes no fraction", statusCompleted, &half, "does not take"},
		{"completed alone is fine", statusCompleted, nil, ""},
		{"draft takes no fraction", statusDraft, &half, "does not take"},
		{"draft alone is fine", statusDraft, nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRolloutForStatus(tc.status, tc.rollout)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseVersionCodes(t *testing.T) {
	codes, err := parseVersionCodes([]string{"42,43", " 44 "})
	if err != nil {
		t.Fatalf("parseVersionCodes: %v", err)
	}
	if len(codes) != 3 || codes[0] != "42" || codes[2] != "44" {
		t.Fatalf("unexpected codes: %v", codes)
	}

	// A version *name* typed where a version code belongs is the mistake worth
	// catching: the API's error names neither the value nor the field.
	_, err = parseVersionCodes([]string{"1.2.3"})
	if err == nil || !strings.Contains(err.Error(), "versionCode") {
		t.Errorf("err = %v, want one explaining what a version code is", err)
	}
	if _, err := parseVersionCodes(nil); err == nil {
		t.Error("an empty list should be rejected")
	}
}

func TestParseStatus(t *testing.T) {
	for _, in := range []string{"draft", "DRAFT", " inProgress ", "completed", "halted"} {
		if _, err := parseStatus(in); err != nil {
			t.Errorf("parseStatus(%q): %v", in, err)
		}
	}
	err := func() error { _, err := parseStatus("live"); return err }()
	if err == nil || !strings.Contains(err.Error(), "inProgress") {
		t.Errorf("err = %v, want one listing the valid statuses", err)
	}
}
