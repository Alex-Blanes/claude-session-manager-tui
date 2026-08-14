package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Claude Code encodes every non-alphanumeric character as '-', so decoding has
// to work out which dashes were really dots, underscores, spaces or accents.
// These are shapes that occur in real ~/.claude/projects directories.
func TestResolveEncodedWildcards(t *testing.T) {
	base := t.TempDir()
	for _, dir := range []string{
		filepath.Join("alex.blanes", "Documentación"),
		filepath.Join("alex.blanes", "AV Consulting - General"),
		filepath.Join("alex.blanes", "JV_LarExtractor"),
		"alex-blanes", // literal dash: an exact match must still win
	} {
		if err := os.MkdirAll(filepath.Join(base, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		encoded string
		want    string
	}{
		{"alex-blanes-Documentaci-n", filepath.Join("alex.blanes", "Documentación")},
		{"alex-blanes-AV-Consulting---General", filepath.Join("alex.blanes", "AV Consulting - General")},
		{"alex-blanes-JV-LarExtractor", filepath.Join("alex.blanes", "JV_LarExtractor")},
		{"alex-blanes", "alex-blanes"},
	}
	for _, c := range cases {
		got := resolveEncoded(base, c.encoded)
		if want := filepath.Join(base, c.want); got != want {
			t.Errorf("resolveEncoded(%q)\n got %q\nwant %q", c.encoded, got, want)
		}
	}
}

func TestResolveEncodedNoMatch(t *testing.T) {
	if got := resolveEncoded(t.TempDir(), "does-not-exist"); got != "" {
		t.Errorf("expected no result for a path that isn't there, got %q", got)
	}
}

// The Windows drive prefix is three characters ("C--"), not two: the colon and
// the separator both encode to '-'.
func TestDecodePathWindowsDrivePrefix(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only encoding")
	}
	// Drive Z: doesn't exist, so this exercises the textual fallback and shows
	// the remainder is not left with a stray leading separator.
	if got, want := decodePath("Z--Users-bob"), `Z:\Users\bob`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
