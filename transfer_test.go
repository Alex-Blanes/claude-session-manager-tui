package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncodeCwdMatchesRealFolderNames(t *testing.T) {
	// Taken from actual folders under ~/.claude/projects.
	cases := map[string]string{
		`C:\Users\alex.blanes`: "C--Users-alex-blanes",
		`C:\Users\alex.blanes\Nextcloud\Área Interna\11 Secretariado\Documentos\2026-08-03 - Congreso PCE`: "C--Users-alex-blanes-Nextcloud--rea-Interna-11-Secretariado-Documentos-2026-08-03---Congreso-PCE",
		`D:\Users\alex.blanes\Codigo fuente\Personal\session-manager-tui`:                                  "D--Users-alex-blanes-Codigo-fuente-Personal-session-manager-tui",
	}
	for in, want := range cases {
		if got := encodeCwd(in); got != want {
			t.Errorf("encodeCwd(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestRetargetCWDKeepsLineValid(t *testing.T) {
	from := `C:\Users\alex.blanes`
	to := `C:\Users\alex-`
	line := `{"type":"user","cwd":"C:\\Users\\alex.blanes","message":{"text":"mira C:\\Users\\alex.blanes\\x"}}`

	got := retargetCWD(line, from, to)
	if !json.Valid([]byte(got)) {
		t.Fatalf("rewritten line is not valid JSON: %s", got)
	}
	var m struct {
		CWD     string `json:"cwd"`
		Message struct {
			Text string `json:"text"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatal(err)
	}
	if m.CWD != to {
		t.Errorf("cwd = %q, want %q", m.CWD, to)
	}
	// The conversation itself must not be rewritten: that would forge what was said.
	if !strings.Contains(m.Message.Text, `alex.blanes`) {
		t.Errorf("the message text was rewritten too: %q", m.Message.Text)
	}
	// A line with no cwd, and a no-op rename, come back untouched.
	plain := `{"type":"summary"}`
	if retargetCWD(plain, from, to) != plain {
		t.Error("a line without cwd should be left alone")
	}
	if retargetCWD(line, from, from) != line {
		t.Error("rewriting to the same path should be a no-op")
	}
}

func TestRetargetCWDFollowsSubdirectories(t *testing.T) {
	// The congress session spent 63 of its 586 lines in a subdirectory. Replacing
	// only the exact path left those pointing at the machine it came from.
	from := `C:\Users\alex.blanes\Nextcloud\Congreso PCE`
	to := `C:\Users\alex-\Nextcloud\Congreso PCE`

	sub := `{"cwd":"C:\\Users\\alex.blanes\\Nextcloud\\Congreso PCE\\herramientas"}`
	got := retargetCWD(sub, from, to)
	if !json.Valid([]byte(got)) {
		t.Fatalf("not valid JSON: %s", got)
	}
	var m struct {
		CWD string `json:"cwd"`
	}
	json.Unmarshal([]byte(got), &m)
	if want := to + `\herramientas`; m.CWD != want {
		t.Errorf("cwd = %q, want %q", m.CWD, want)
	}

	// A sibling that merely starts with the same text must be left alone.
	sibling := `{"cwd":"C:\\Users\\alex.blanes\\Nextcloud\\Congreso PCE 2024"}`
	if got := retargetCWD(sibling, from, to); got != sibling {
		t.Errorf("rewrote a different directory that shares a prefix:\n %s", got)
	}
}

func TestHashIgnoresTheWorkingDirectory(t *testing.T) {
	// Import rewrites cwd, so a session that goes out and comes back must still
	// hash the same or every round trip would look like a divergence.
	here := []string{`{"cwd":"C:\\Users\\alex.blanes\\p","t":"hola"}`}
	there := []string{`{"cwd":"C:\\Users\\alex-\\p","t":"hola"}`}
	if hashLines(here) != hashLines(there) {
		t.Error("the same conversation on two machines should hash alike")
	}
	if compareTranscripts(here, there) != ancestrySame {
		t.Error("a round trip should read as the same session")
	}
	// Real differences must still show.
	changed := []string{`{"cwd":"C:\\Users\\alex-\\p","t":"adios"}`}
	if hashLines(here) == hashLines(changed) {
		t.Error("a different conversation should hash differently")
	}
	// A line with no cwd survives untouched.
	if got := blankCWD(`{"t":"x"}`); got != `{"t":"x"}` {
		t.Errorf("blankCWD mangled a line without cwd: %s", got)
	}
}

func TestCompareTranscriptsTellsAReturnFromADivergence(t *testing.T) {
	base := []string{"a", "b", "c"}
	grown := []string{"a", "b", "c", "d"}
	other := []string{"a", "b", "x"}

	cases := []struct {
		name            string
		local, incoming []string
		want            ancestry
	}{
		{"identical", base, base, ancestrySame},
		{"went out and came back longer", base, grown, ancestryIncomingNewer},
		{"here is ahead", grown, base, ancestryLocalNewer},
		{"both grew differently", grown, other, ancestryDiverged},
		{"empty local", nil, base, ancestryIncomingNewer},
	}
	for _, c := range cases {
		if got := compareTranscripts(c.local, c.incoming); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestSafeJoinRefusesEscapes(t *testing.T) {
	root := t.TempDir()
	// What matters is that nothing lands outside root — rejecting the entry and
	// containing it are both fine. "/etc/passwd" is absolute on Linux but merely
	// root-relative on Windows, so the two platforms take different routes to
	// the same guarantee.
	for _, bad := range []string{`../evil.txt`, `..\evil.txt`, `a/../../evil.txt`, `C:\Windows\evil.txt`, `/etc/passwd`} {
		got, err := safeJoin(root, bad)
		if err != nil {
			continue
		}
		if !strings.HasPrefix(got, root+string(os.PathSeparator)) {
			t.Errorf("safeJoin(%q) escaped root: %q", bad, got)
		}
	}
	good, err := safeJoin(root, "sub/dir/file.txt")
	if err != nil {
		t.Fatalf("safeJoin rejected a legitimate entry: %v", err)
	}
	if !strings.HasPrefix(good, root) {
		t.Errorf("%q escaped %q", good, root)
	}
}

func TestInsideSyncedFolder(t *testing.T) {
	synced := []string{
		`C:\Users\alex.blanes\Nextcloud\Área Interna\x`,
		`C:\Users\alex.blanes\OneDrive - AV\Documentos`,
		`C:\Users\alex.blanes\Dropbox\ADDVALUE-MSIN`,
	}
	for _, p := range synced {
		if _, ok := insideSyncedFolder(p); !ok {
			t.Errorf("%q should be flagged as synced", p)
		}
	}
	for _, p := range []string{`D:\Users\alex.blanes\Codigo fuente\Personal`, `C:\Users\alex.blanes\.claude`} {
		if root, ok := insideSyncedFolder(p); ok {
			t.Errorf("%q wrongly flagged as synced (%s)", p, root)
		}
	}
}

func TestReadLinesDropsBlanksAndCRLF(t *testing.T) {
	f := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(f, []byte("{\"a\":1}\r\n\r\n{\"b\":2}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := readLines(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0] != `{"a":1}` || lines[1] != `{"b":2}` {
		t.Errorf("got %q", lines)
	}
}
