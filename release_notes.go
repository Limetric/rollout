package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// Release notes are per-locale text attached to a track release. Play caps them
// at 500 characters and rejects the whole commit when one locale is over — with
// a message that does not say which one — so the limit is enforced here, by
// locale, before anything is staged.

// maxReleaseNoteRunes is Play's per-locale release-note limit.
const maxReleaseNoteRunes = 500

// releaseNote is the API's Track.Release.releaseNotes entry.
type releaseNote struct {
	Language string `json:"language"`
	Text     string `json:"text"`
}

// parseReleaseNotes turns `--notes en-US=Bug fixes` pairs into release notes.
func parseReleaseNotes(pairs []string) ([]releaseNote, error) {
	notes := make([]releaseNote, 0, len(pairs))
	for _, pair := range pairs {
		locale, text, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("release note %q is not <locale>=<text> — for example: --notes en-US=\"Bug fixes\"", pair)
		}
		locale = strings.TrimSpace(locale)
		if locale == "" {
			return nil, fmt.Errorf("release note %q has no locale — for example: --notes en-US=\"Bug fixes\"", pair)
		}
		notes = append(notes, releaseNote{Language: locale, Text: text})
	}
	return notes, nil
}

// readReleaseNotesDir reads release notes from a directory, accepting both
// layouts people actually have.
//
// The flat layout is `<dir>/<locale>.txt`. The fastlane `supply` layout is
// `<dir>/<locale>/changelogs/<versionCode>.txt`, which is what almost every
// existing Android release pipeline already has on disk — asking those users to
// reshape their metadata tree to adopt this tool would be a poor trade.
//
// versionCode selects which changelog to read in the fastlane layout; when it
// is empty the highest-numbered changelog wins, which is the one that belongs
// to the release being made.
func readReleaseNotesDir(dir, versionCode string) ([]releaseNote, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read release notes directory %q: %w", dir, err)
	}

	var notes []releaseNote
	for _, entry := range entries {
		switch {
		case entry.IsDir():
			note, err := readFastlaneChangelog(filepath.Join(dir, entry.Name()), entry.Name(), versionCode)
			if err != nil {
				return nil, err
			}
			if note != nil {
				notes = append(notes, *note)
			}
		case strings.HasSuffix(entry.Name(), ".txt"):
			locale := strings.TrimSuffix(entry.Name(), ".txt")
			text, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				return nil, fmt.Errorf("read release notes for %s: %w", locale, err)
			}
			notes = append(notes, releaseNote{Language: locale, Text: strings.TrimRight(string(text), "\n")})
		}
	}
	if len(notes) == 0 {
		return nil, fmt.Errorf("no release notes found in %q — expected <locale>.txt files, or the fastlane layout <locale>/changelogs/<versionCode>.txt", dir)
	}
	// Stable order so a preview reads the same way twice.
	sort.Slice(notes, func(i, j int) bool { return notes[i].Language < notes[j].Language })
	return notes, nil
}

// readFastlaneChangelog reads one locale's changelog from a fastlane metadata
// tree. A locale directory with no changelogs is skipped rather than failing:
// a metadata tree commonly holds locales that have listing text but no notes
// for this particular version.
func readFastlaneChangelog(localeDir, locale, versionCode string) (*releaseNote, error) {
	changelogs := filepath.Join(localeDir, "changelogs")
	entries, err := os.ReadDir(changelogs)
	if err != nil {
		return nil, nil // not a fastlane locale directory
	}

	var candidates []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".txt") {
			continue
		}
		candidates = append(candidates, strings.TrimSuffix(entry.Name(), ".txt"))
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	chosen := versionCode
	if chosen == "" {
		// Highest version code: the changelog that belongs to the release
		// being made, not whichever one the filesystem listed first.
		sort.Slice(candidates, func(i, j int) bool { return versionCodeLess(candidates[i], candidates[j]) })
		chosen = candidates[len(candidates)-1]
	}
	text, err := os.ReadFile(filepath.Join(changelogs, chosen+".txt"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // this locale has no notes for that version
		}
		return nil, fmt.Errorf("read release notes for %s: %w", locale, err)
	}
	return &releaseNote{Language: locale, Text: strings.TrimRight(string(text), "\n")}, nil
}

// versionCodeLess orders version codes numerically where it can and
// lexicographically otherwise, so a directory holding "9" and "10" sorts the
// way a human means.
func versionCodeLess(a, b string) bool {
	na, aok := parseVersionCode(a)
	nb, bok := parseVersionCode(b)
	if aok && bok {
		return na < nb
	}
	return a < b
}

// validateReleaseNotes enforces Play's per-locale limit and rejects duplicates.
//
// Naming the locale is the whole point: the API rejects the commit with a
// message about "releaseNotes" and no indication of which of twenty locales is
// over, after an edit has already been opened.
func validateReleaseNotes(notes []releaseNote) error {
	seen := map[string]bool{}
	for _, note := range notes {
		if note.Language == "" {
			return fmt.Errorf("a release note has no locale")
		}
		if seen[note.Language] {
			return fmt.Errorf("release notes for %s were given twice", note.Language)
		}
		seen[note.Language] = true
		if n := utf8.RuneCountInString(note.Text); n > maxReleaseNoteRunes {
			return fmt.Errorf("release notes for %s are %d characters; Play's limit is %d", note.Language, n, maxReleaseNoteRunes)
		}
	}
	return nil
}

// releaseNoteLocales lists the locales a set of notes covers, for previews.
func releaseNoteLocales(notes []releaseNote) []string {
	locales := make([]string, 0, len(notes))
	for _, note := range notes {
		locales = append(locales, note.Language)
	}
	return locales
}
