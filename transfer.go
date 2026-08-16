package main

// Moving a session between machines. Claude Code keys a session's folder by the
// working directory it ran in, so a session copied to another machine lands in a
// folder for a path that does not exist there. Export packs the pieces and
// records where they came from; import recomputes the folder for the machine it
// arrives at.

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	manifestName   = "manifest.json"
	transferSchema = 1
	workDirWarnMB  = 100
)

// Manifest travels inside the zip. The hashes are what let import tell a session
// that merely came back from a trip apart from one that diverged.
type Manifest struct {
	Schema     int          `json:"schema"`
	SessionID  string       `json:"sessionId"`
	ExportedAt time.Time    `json:"exportedAt"`
	Host       string       `json:"host"`
	OriginCWD  string       `json:"originCwd"`
	ProjectDir string       `json:"projectDir"`
	Lines      int          `json:"transcriptLines"`
	FirstLine  string       `json:"transcriptFirstLineSha256"`
	Whole      string       `json:"transcriptSha256"`
	WorkDir    *WorkDirInfo `json:"workdir,omitempty"`
}

type WorkDirInfo struct {
	Files  int    `json:"files"`
	Bytes  int64  `json:"bytes"`
	Source string `json:"source"` // "git ls-files" or "walk"
}

func claudeDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// encodeCwd mirrors Claude Code's folder naming: every character that is not an
// ASCII letter or digit becomes a dash. That is why the decoding in main.go has
// to guess — the mapping loses information on the way in.
//
// ponytail: one dash per rune. Claude Code runs on UTF-16, so a character
// outside the BMP (an emoji in a path) would give it two dashes and us one.
// Not worth a surrogate-pair dance until a path actually contains one.
func encodeCwd(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// findSessionFile looks for <id>.jsonl under every project folder, not just the
// one the current directory would suggest.
func findSessionFile(id string) (path, projectDir string, err error) {
	base := filepath.Join(claudeDir(), "projects")
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", "", err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		candidate := filepath.Join(base, e.Name(), id+".jsonl")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, e.Name(), nil
		}
	}
	return "", "", fmt.Errorf("session %s not found under %s", id, base)
}

// readLines returns the transcript one line at a time. bufio.Scanner is no good
// here: a single line carrying a pasted file or a tool result blows past its
// token limit.
func readLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out, nil
}

// hashLines identifies a transcript by its content with the working directory
// blanked out. Import rewrites that field, so the same session on two machines
// differs there by design — hashing it raw would report every round trip as a
// divergence.
func hashLines(lines []string) string {
	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(blankCWD(l)))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// blankCWD replaces every recorded cwd value with a placeholder.
//
// ponytail: the value ends at the next quote. JSON escapes a quote as \", and
// Windows forbids one in a path, so scanning for the next " is right here and
// only wrong for a POSIX path containing a literal quote.
func blankCWD(line string) string {
	var b strings.Builder
	rest := line
	for {
		i := strings.Index(rest, `"cwd":`)
		if i < 0 {
			break
		}
		v := i + len(`"cwd":`)
		for v < len(rest) && rest[v] == ' ' {
			v++
		}
		if v >= len(rest) || rest[v] != '"' {
			b.WriteString(rest[:v])
			rest = rest[v:]
			continue
		}
		end := strings.IndexByte(rest[v+1:], '"')
		if end < 0 {
			break
		}
		b.WriteString(rest[:i])
		b.WriteString(`"cwd":""`)
		rest = rest[v+1+end+1:]
	}
	b.WriteString(rest)
	return b.String()
}

// cwdOf pulls the working directory out of the transcript. Claude Code records
// it verbatim on the messages, which beats decoding the folder name.
func cwdOf(lines []string) string {
	for _, l := range lines {
		var m struct {
			CWD string `json:"cwd"`
		}
		if json.Unmarshal([]byte(l), &m) == nil && m.CWD != "" {
			return m.CWD
		}
	}
	return ""
}

// retargetCWD swaps the recorded working directory for the one on this machine.
// A textual replacement of the JSON-encoded value keeps every other byte of the
// line intact — re-marshalling would reorder keys and rewrite escapes across a
// multi-megabyte file for no reason.
// A session does not always stay put: this one spent 63 of its 586 lines in a
// subdirectory of the project. So the origin path is replaced as a prefix, and
// only when what follows is the closing quote or a separator — otherwise
// C:\a\b would also rewrite C:\a\bc.
func retargetCWD(line, from, to string) string {
	if from == to || from == "" {
		return line
	}
	oldEsc, newEsc := jsonBody(from), jsonBody(to)
	for _, key := range []string{`"cwd":"`, `"cwd": "`} {
		for _, tail := range []string{`"`, `\\`} {
			line = strings.ReplaceAll(line, key+oldEsc+tail, key+newEsc+tail)
		}
	}
	return line
}

// jsonBody is a string encoded as JSON with the surrounding quotes removed, so
// it can be matched as a prefix of a longer encoded value.
func jsonBody(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

// ── Synced folders ───────────────────────────────────────────────────────────

var syncedRoots = []string{"Nextcloud", "OneDrive", "Dropbox", "Google Drive", "iCloudDrive"}

// insideSyncedFolder reports whether a path lives under a cloud-sync root.
// Walking one of those hydrates every on-demand placeholder it touches, which
// downloads the lot. Packing a working directory is exactly such a walk.
func insideSyncedFolder(p string) (string, bool) {
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		for _, root := range syncedRoots {
			if strings.EqualFold(seg, root) || strings.HasPrefix(seg, root+" -") {
				return seg, true
			}
		}
	}
	return "", false
}

// ── Zip helpers ──────────────────────────────────────────────────────────────

func addFile(zw *zip.Writer, srcPath, name string) (int64, error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return 0, err
	}
	defer src.Close()
	w, err := zw.Create(filepath.ToSlash(name))
	if err != nil {
		return 0, err
	}
	return io.Copy(w, src)
}

// addTree copies a directory into the zip under prefix. A missing directory is
// not an error: most sessions have no tool-results and no file-history.
func addTree(zw *zip.Writer, srcDir, prefix string) (files int, bytes int64, err error) {
	if _, statErr := os.Stat(srcDir); statErr != nil {
		return 0, 0, nil
	}
	err = filepath.Walk(srcDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}
		n, err := addFile(zw, p, filepath.Join(prefix, rel))
		if err != nil {
			return err
		}
		files++
		bytes += n
		return nil
	})
	return files, bytes, err
}

// workDirFiles lists what to pack from the working directory. In a git repo,
// `ls-files -co --exclude-standard` is exactly "tracked, plus untracked that is
// not ignored" — .gitignore honoured without writing a parser.
func workDirFiles(dir string) (rel []string, source string, err error) {
	out, gitErr := exec.Command("git", "-C", dir, "ls-files", "-co", "--exclude-standard").Output()
	if gitErr == nil {
		for _, l := range strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				rel = append(rel, filepath.FromSlash(l))
			}
		}
		return rel, "git ls-files", nil
	}
	err = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		r, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = append(rel, r)
		return nil
	})
	return rel, "walk", err
}

// safeJoin blocks the classic zip traversal: an entry named ..\..\something, or
// an absolute path, writing outside the directory we chose.
func safeJoin(root, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("refusing zip entry that escapes the target: %q", name)
	}
	full := filepath.Join(root, clean)
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	fullAbs, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	if fullAbs != rootAbs && !strings.HasPrefix(fullAbs, rootAbs+string(os.PathSeparator)) {
		return "", fmt.Errorf("refusing zip entry that escapes the target: %q", name)
	}
	return full, nil
}

func writeZipEntry(f *zip.File, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, rc)
	return err
}
