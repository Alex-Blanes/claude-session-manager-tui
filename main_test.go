package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Claude Code encodes every non-alphanumeric character as '-', so decoding has
// to guess which dashes were really dots, spaces or accents. These are the
// shapes that actually occur in ~/.claude/projects.
func TestResolveEncodedWildcards(t *testing.T) {
	base := t.TempDir()
	for _, dir := range []string{
		filepath.Join("alex.blanes", "Documentación"),
		filepath.Join("alex.blanes", "AV Consulting - General"),
		filepath.Join("alex-blanes"), // literal dash, must still win by exact match
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
		t.Errorf("expected empty result for a path that isn't there, got %q", got)
	}
}

// decodeWindowsPath has to split the drive off before walking, because there is
// no '/' root to start from.
func TestDecodeWindowsPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only encoding")
	}
	// No such drive, so this exercises the textual fallback.
	if got, want := decodeWindowsPath("Z--Users-bob"), `Z:\Users\bob`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// Not a drive-prefixed name: leave it alone rather than mangle it.
	if got := decodeWindowsPath("no-drive-here"); got != "no-drive-here" {
		t.Errorf("got %q, want it unchanged", got)
	}
}

func TestLastSegment(t *testing.T) {
	cases := map[string]string{
		`C:\Users\alex.blanes\proj`: "proj",
		`C:\Users\alex.blanes\`:     "alex.blanes",
		"/home/bob/proj":            "proj",
		"proj":                      "proj",
	}
	for in, want := range cases {
		if got := lastSegment(in); got != want {
			t.Errorf("lastSegment(%q) = %q, want %q", in, got, want)
		}
	}
}
