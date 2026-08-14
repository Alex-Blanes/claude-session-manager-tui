package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// ── Version & update check ──────────────────────────────────────────────────

var version = "dev"

func currentVersion() string {
	return strings.TrimPrefix(version, "v")
}

func fuzzyMatch(haystack, needle string) bool {
	if strings.Contains(haystack, needle) {
		return true
	}
	hi := 0
	for ni := 0; ni < len(needle); ni++ {
		found := false
		for hi < len(haystack) {
			if haystack[hi] == needle[ni] {
				hi++
				found = true
				break
			}
			hi++
		}
		if !found {
			return false
		}
	}
	return true
}

type githubRelease struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

func checkForUpdate() (newVersion, url string, hasUpdate bool) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("https://api.github.com/repos/welcomra1n/session-manager-tui/releases/latest")
	if err != nil {
		return "", "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", false
	}
	var release githubRelease
	if json.Unmarshal(body, &release) != nil {
		return "", "", false
	}
	tag := strings.TrimPrefix(release.TagName, "v")
	if tag != "" && tag != currentVersion() {
		return tag, release.HTMLURL, true
	}
	return "", "", false
}

func updateNotice(newVer string) string {
	var cmd string
	switch runtime.GOOS {
	case "darwin":
		cmd = "brew upgrade csm"
	case "windows":
		cmd = "scoop update csm"
	default:
		cmd = "csm --update"
	}
	return fmt.Sprintf("[yellow]⬆ v%s 사용 가능 · %s 또는 u키[-]", newVer, cmd)
}

func selfUpdate() error {
	newVer, _, has := checkForUpdate()
	if !has {
		fmt.Println("이미 최신 버전입니다:", currentVersion())
		return nil
	}
	fmt.Printf("업데이트 발견: %s → %s\n", currentVersion(), newVer)

	// Determine binary name for this platform
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	binName := fmt.Sprintf("csm-%s-%s", goos, goarch)
	if goos == "windows" {
		binName += ".exe"
	}

	dlURL := fmt.Sprintf("https://github.com/welcomra1n/session-manager-tui/releases/download/v%s/%s", newVer, binName)
	fmt.Println("다운로드:", dlURL)

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(dlURL)
	if err != nil {
		return fmt.Errorf("다운로드 실패: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("다운로드 실패: HTTP %d", resp.StatusCode)
	}

	// Get current executable path
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("실행 파일 경로 확인 실패: %v", err)
	}
	// Resolve symlinks (ignore error — Windows shims may not resolve)
	if resolved, err := filepath.EvalSymlinks(execPath); err == nil {
		execPath = resolved
	}

	// Write to temp file first
	tmpFile := execPath + ".tmp"
	f, err := os.OpenFile(tmpFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return fmt.Errorf("임시 파일 생성 실패: %v", err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmpFile)
		return fmt.Errorf("쓰기 실패: %v", err)
	}
	f.Close()

	// Replace old binary — move old out first if direct rename fails
	if err := os.Rename(tmpFile, execPath); err != nil {
		oldBackup := execPath + ".old"
		os.Remove(oldBackup)
		if mvErr := os.Rename(execPath, oldBackup); mvErr != nil {
			os.Remove(tmpFile)
			return fmt.Errorf("교체 실패: %v (backup: %v)", err, mvErr)
		}
		if err2 := os.Rename(tmpFile, execPath); err2 != nil {
			os.Rename(oldBackup, execPath) // restore
			os.Remove(tmpFile)
			return fmt.Errorf("교체 실패: %v", err2)
		}
		os.Remove(oldBackup)
	}

	fmt.Printf("✅ 업데이트 완료: v%s\n", newVer)
	return nil
}

// ── Config file ─────────────────────────────────────────────────────────────

type Config struct {
	ExpiryDays      int    `json:"expiry_days"`
	RefreshInterval int    `json:"refresh_interval"`
	DefaultSort     string `json:"default_sort"`
	DefaultTerminal string `json:"default_terminal"`
	CompactMode     bool   `json:"compact_mode"`
}

func defaultConfig() Config {
	return Config{
		ExpiryDays:      30,
		RefreshInterval: 10,
		DefaultSort:     "date",
		DefaultTerminal: "auto",
		CompactMode:     false,
	}
}

func configPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "csm-config.json")
}

func loadConfig() Config {
	cfg := defaultConfig()
	data, err := os.ReadFile(configPath())
	if err != nil {
		return cfg
	}
	json.Unmarshal(data, &cfg)
	if cfg.ExpiryDays <= 0 {
		cfg.ExpiryDays = 30
	}
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = 10
	}
	return cfg
}

func saveConfig(cfg Config) {
	data, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(configPath(), data, 0644)
}

// ── Constants ───────────────────────────────────────────────────────────────

var sessionExpiryDays = 30

// ── Data types ──────────────────────────────────────────────────────────────

type Provider int

const (
	ProviderClaude Provider = iota
	ProviderCodex
)

func (p Provider) Icon() string {
	switch p {
	case ProviderCodex:
		return "\xf0\x9f\xa4\x96" // 🤖 (Codex)
	default:
		return "\xf0\x9f\xa7\xa0" // 🧠 (Claude)
	}
}

func (p Provider) Label() string {
	switch p {
	case ProviderCodex:
		return "Codex"
	default:
		return "Claude"
	}
}

type Session struct {
	ID           string
	ProjectDir   string
	ProjectName  string
	SessionFile  string
	ModTime      time.Time
	FileSize     int64
	MessageCount int
	UserMsgCount int
	AsstMsgCount int
	FirstUserMsg string
	LastUserMsg  string
	GitBranch    string
	CWD          string
	Messages     []Message
	Alias        string   // user-defined alias for the session
	Selected     bool     // multi-select checkbox state
	Provider     Provider // Claude or Codex
	Entrypoint   string   // cli, claude-desktop, web, etc.
	Active       bool     // session is currently open by another process
	Pinned       bool     // pinned in Codex desktop
	InputTokens  int64
	OutputTokens int64
	Loaded       bool
}

type Message struct {
	Type    string
	Content string
}

type rawLine struct {
	Type       string          `json:"type"`
	Message    json.RawMessage `json:"message,omitempty"`
	GitBranch  string          `json:"gitBranch,omitempty"`
	CWD        string          `json:"cwd,omitempty"`
	Entrypoint string          `json:"entrypoint,omitempty"`
}

// ── Korean Dubeolsik → English key mapping ──────────────────────────────────

var korToEng = map[rune]rune{
	// Jamo (자모)
	'ㅂ': 'q', 'ㅈ': 'w', 'ㄷ': 'e', 'ㄱ': 'r', 'ㅅ': 't',
	'ㅛ': 'y', 'ㅕ': 'u', 'ㅑ': 'i', 'ㅐ': 'o', 'ㅔ': 'p',
	'ㅁ': 'a', 'ㄴ': 's', 'ㅇ': 'd', 'ㄹ': 'f', 'ㅎ': 'g',
	'ㅗ': 'h', 'ㅓ': 'j', 'ㅏ': 'k', 'ㅣ': 'l',
	'ㅋ': 'z', 'ㅌ': 'x', 'ㅊ': 'c', 'ㅍ': 'v', 'ㅠ': 'b',
	'ㅜ': 'n', 'ㅡ': 'm',
	// Shift variants
	'ㅃ': 'Q', 'ㅉ': 'W', 'ㄸ': 'E', 'ㄲ': 'R', 'ㅆ': 'T',
	'ㅒ': 'O', 'ㅖ': 'P',
}

// korSyllableToEng decomposes a Korean syllable to its initial consonant
// and maps it to the English key. This handles IME-composed characters.
func korSyllableToEng(r rune) (rune, bool) {
	// Direct jamo match
	if eng, ok := korToEng[r]; ok {
		return eng, true
	}
	// Decompose Korean syllable (가=0xAC00) to initial consonant
	if r >= 0xAC00 && r <= 0xD7A3 {
		// Korean syllable block: (initial * 21 + medial) * 28 + final
		idx := r - 0xAC00
		initial := idx / (21 * 28)
		// Initial consonants in order: ㄱㄲㄴㄷㄸㄹㅁㅂㅃㅅㅆㅇㅈㅉㅊㅋㅌㅍㅎ
		initials := []rune{'ㄱ', 'ㄲ', 'ㄴ', 'ㄷ', 'ㄸ', 'ㄹ', 'ㅁ', 'ㅂ', 'ㅃ', 'ㅅ', 'ㅆ', 'ㅇ', 'ㅈ', 'ㅉ', 'ㅊ', 'ㅋ', 'ㅌ', 'ㅍ', 'ㅎ'}
		if int(initial) < len(initials) {
			if eng, ok := korToEng[initials[initial]]; ok {
				return eng, true
			}
		}
	}
	return r, false
}

func toEngKey(r rune) rune {
	if eng, ok := korSyllableToEng(r); ok {
		return eng
	}
	return r
}

// isWide returns true if rune takes 2 cells in terminal (CJK, emoji, etc.)
func isWide(r rune) bool {
	return (r >= 0x1100 && r <= 0x115F) || // Hangul Jamo
		(r >= 0x2E80 && r <= 0x9FFF) || // CJK
		(r >= 0xAC00 && r <= 0xD7AF) || // Hangul Syllables
		(r >= 0xF900 && r <= 0xFAFF) || // CJK Compatibility
		(r >= 0xFE30 && r <= 0xFE6F) || // CJK Forms
		(r >= 0xFF01 && r <= 0xFF60) || // Fullwidth
		(r >= 0x1F000 && r <= 0x1FFFF) // Emoji
}

// visibleWidth counts display width (ignoring tview color tags, CJK=2)
func visibleWidth(s string) int {
	w := 0
	inTag := false
	for _, r := range s {
		if r == '[' {
			inTag = true
			continue
		}
		if inTag {
			if r == ']' {
				inTag = false
			}
			continue
		}
		if isWide(r) {
			w += 2
		} else {
			w++
		}
	}
	return w
}

// padRight pads a string to n visible cells, truncating if too long
func padRight(s string, n int) string {
	w := visibleWidth(s)
	if w > n {
		// Truncate
		cur := 0
		inTag := false
		var result strings.Builder
		for _, r := range s {
			if r == '[' {
				inTag = true
				result.WriteRune(r)
				continue
			}
			if inTag {
				result.WriteRune(r)
				if r == ']' {
					inTag = false
				}
				continue
			}
			rw := 1
			if isWide(r) {
				rw = 2
			}
			if cur+rw > n-1 {
				result.WriteRune('…')
				return result.String()
			}
			result.WriteRune(r)
			cur += rw
		}
		return result.String()
	}
	if w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}


// activeInfo stores environment and PID for a running session
type activeInfo struct {
	Env string // "local" or "ssh"
	PID string // process ID for killing
}

var cachedActiveIDs map[string]activeInfo
var cachedActiveIDsTime time.Time

func refreshActiveIDs() map[string]activeInfo {
	if time.Since(cachedActiveIDsTime) < 5*time.Second && cachedActiveIDs != nil {
		return cachedActiveIDs
	}
	ids := make(map[string]activeInfo)

	detectEnv := func(line string) string {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "ssh") || strings.Contains(lower, "sshd") {
			return "ssh"
		}
		return "local"
	}

	extractPID := func(line string) string {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			return fields[0]
		}
		return ""
	}

	if runtime.GOOS == "windows" {
		// Windows: use wmic to find processes
		if out, err := exec.Command("cmd", "/c", "wmic process where \"commandline like '%--resume%'\" get processid,commandline /format:list").CombinedOutput(); err == nil {
			lines := strings.Split(string(out), "\n")
			var cmdLine, pid string
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "CommandLine=") {
					cmdLine = line[len("CommandLine="):]
				} else if strings.HasPrefix(line, "ProcessId=") {
					pid = line[len("ProcessId="):]
				}
				if cmdLine != "" && pid != "" {
					if idx := strings.Index(cmdLine, "--resume"); idx >= 0 {
						rest := strings.TrimSpace(cmdLine[idx+len("--resume"):])
						fields := strings.Fields(rest)
						if len(fields) > 0 {
							ids[fields[0]] = activeInfo{Env: "local", PID: pid}
						}
					} else if idx := strings.Index(cmdLine, "resume"); idx >= 0 {
						rest := strings.TrimSpace(cmdLine[idx+len("resume"):])
						fields := strings.Fields(rest)
						for _, f := range fields {
							if !strings.HasPrefix(f, "-") {
								ids[f] = activeInfo{Env: "local", PID: pid}
								break
							}
						}
					}
					cmdLine, pid = "", ""
				}
			}
		}
	} else {
		// Unix: use pgrep
		// Check claude processes: claude --resume <ID>
		if out, err := exec.Command("pgrep", "-afl", "claude.*--resume").CombinedOutput(); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				if idx := strings.Index(line, "--resume"); idx >= 0 {
					rest := strings.TrimSpace(line[idx+len("--resume"):])
					fields := strings.Fields(rest)
					if len(fields) > 0 {
						ids[fields[0]] = activeInfo{Env: detectEnv(line), PID: extractPID(line)}
					}
				}
			}
		}
		// Check codex processes: codex resume <ID>
		if out, err := exec.Command("pgrep", "-afl", "codex.*resume").CombinedOutput(); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				if idx := strings.Index(line, "resume"); idx >= 0 {
					rest := strings.TrimSpace(line[idx+len("resume"):])
					fields := strings.Fields(rest)
					if len(fields) > 0 {
						for _, f := range fields {
							if !strings.HasPrefix(f, "-") {
								ids[f] = activeInfo{Env: detectEnv(line), PID: extractPID(line)}
								break
							}
						}
					}
				}
			}
		}
	}
	cachedActiveIDs = ids
	cachedActiveIDsTime = time.Now()
	return ids
}

func isSessionActiveByProcess(sessionID string) bool {
	ids := refreshActiveIDs()
	_, exists := ids[sessionID]
	return exists
}

func sessionActiveEnv(sessionID string) string {
	ids := refreshActiveIDs()
	if info, ok := ids[sessionID]; ok {
		return info.Env
	}
	return ""
}

func killSession(sessionID string) error {
	var pids []string
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", fmt.Sprintf("wmic process where \"commandline like '%%%s%%'\" get processid /format:list", sessionID)).CombinedOutput(); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "ProcessId=") {
					pid := line[len("ProcessId="):]
					if pid != "" {
						pids = append(pids, pid)
					}
				}
			}
		}
	} else {
		if out, err := exec.Command("pgrep", "-f", sessionID).CombinedOutput(); err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				pid := strings.TrimSpace(line)
				if pid != "" {
					pids = append(pids, pid)
				}
			}
		}
	}
	if len(pids) == 0 {
		return fmt.Errorf("활성 프로세스 없음")
	}
	if runtime.GOOS == "windows" {
		for _, pid := range pids {
			exec.Command("taskkill", "/PID", pid, "/F").Run()
		}
	} else {
		args := append([]string{"-TERM"}, pids...)
		exec.Command("kill", args...).Run()
	}
	cachedActiveIDsTime = time.Time{}
	return nil
}

func entrypointIcon(ep string) string {
	switch ep {
	case "cli":
		return "CLI"
	case "claude-desktop":
		return "DSK"
	case "web", "claude-ai":
		return "WEB"
	default:
		if ep != "" {
			return ep
		}
		return "  "
	}
}

func entrypointLabel(ep string) string {
	switch ep {
	case "cli":
		return "CLI"
	case "claude-desktop":
		return "Desktop"
	case "web", "claude-ai":
		return "Web"
	default:
		if ep != "" {
			return ep
		}
		return ""
	}
}

type msgEnvelope struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Usage   *msgUsage       `json:"usage,omitempty"`
}

type msgUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// ── Path decoding ───────────────────────────────────────────────────────────

func decodePath(enc string) string {
	if enc == "" {
		return ""
	}
	// Windows: both the drive colon and the separator are encoded, so
	// "C:\Users\bob" arrives as "C--Users-bob" — a three-character prefix,
	// not two. Splitting on the first "--" keeps the remainder clean.
	// ponytail: UNC shares ("--host-share-...") aren't handled. We can't tell
	// where the host name ends without listing "\\", which isn't listable.
	if drive, rest, ok := strings.Cut(enc, "--"); ok && len(drive) == 1 {
		root := drive + ":"
		if result := resolveEncoded(root+string(filepath.Separator), rest); result != "" {
			return result
		}
		return root + string(filepath.Separator) + strings.ReplaceAll(rest, "-", string(filepath.Separator))
	}
	// Unix: encoded path starts with "-" (representing "/")
	if result := resolveEncoded("/", enc[1:]); result != "" {
		return result
	}
	return "/" + strings.ReplaceAll(enc[1:], "-", "/")
}

func resolveEncoded(base, remaining string) string {
	if remaining == "" {
		return base
	}
	parts := strings.Split(remaining, "-")
	for segLen := len(parts); segLen >= 1; segLen-- {
		segment := strings.Join(parts[:segLen], "-")
		rest := ""
		if segLen < len(parts) {
			rest = strings.Join(parts[segLen:], "-")
		}
		// A segment can match more than one real directory; keep trying until
		// one of them resolves the whole remainder.
		for _, candidate := range candidateDirs(base, segment) {
			if rest == "" {
				return candidate
			}
			if result := resolveEncoded(candidate, rest); result != "" {
				return result
			}
		}
	}
	return ""
}

// candidateDirs lists the subdirectories of base that an encoded segment could
// name: the literal spelling first, then any name that matches with '-'
// standing in for a character that was encoded away.
func candidateDirs(base, segment string) []string {
	var out []string
	exact := filepath.Join(base, segment)
	if info, err := os.Stat(exact); err == nil && info.IsDir() {
		out = append(out, exact)
	}
	for _, m := range matchSegment(base, segment) {
		if m != exact {
			out = append(out, m)
		}
	}
	return out
}

// matchSegment finds the subdirectories of base whose names match the encoded
// segment, treating '-' as a wildcard for any single character. Claude Code
// encodes every non-alphanumeric character as '-', so by the time we see the
// name a '.', '_', a space and an accented letter are all indistinguishable
// from a literal dash: "alex-blanes" is really "alex.blanes", "JV-LarExtractor"
// is "JV_LarExtractor" and "Documentaci-n" is "Documentación". Comparison is
// per rune so multi-byte characters line up.
func matchSegment(base, segment string) []string {
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []string
	want := []rune(segment)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		got := []rune(e.Name())
		if len(got) != len(want) {
			continue
		}
		matches := true
		for i := range want {
			if want[i] != '-' && want[i] != got[i] {
				matches = false
				break
			}
		}
		if matches {
			out = append(out, filepath.Join(base, e.Name()))
		}
	}
	return out
}

// ── Text helpers ────────────────────────────────────────────────────────────

func lastSegment(p string) string {
	p = filepath.ToSlash(p)
	parts := strings.Split(p, "/")
	return parts[len(parts)-1]
}

func esc(s string) string { return strings.ReplaceAll(s, "[", "[[]") }

func fmtSize(b int64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func trunc(s string, n int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > n {
		return s[:n-3] + "..."
	}
	return s
}

func isMeaningfulMsg(s string) bool {
	if len(s) < 3 {
		return false
	}
	first := s
	if idx := strings.IndexAny(s, "\r\n"); idx >= 0 {
		first = strings.TrimSpace(s[:idx])
	}
	if first == "" {
		return false
	}
	if strings.HasPrefix(first, "Caveat:") {
		return false
	}
	if strings.HasPrefix(first, "/") {
		word := strings.Fields(first)[0]
		if !strings.Contains(word[1:], "/") {
			return false
		}
	}
	for _, prefix := range []string{"Set model to", "model", "Model set to"} {
		if first == prefix || strings.HasPrefix(first, prefix+" ") {
			return false
		}
	}
	return true
}

// ── JSONL parsing ───────────────────────────────────────────────────────────

var metaTags = []string{
	"<local-command-caveat>", "</local-command-caveat>",
	"<command-name>", "</command-name>",
	"<command-message>", "</command-message>",
	"<command-args>", "</command-args>",
	"<local-command-stdout>", "</local-command-stdout>",
	"<system-reminder>", "</system-reminder>",
}

func cleanMeta(s string) string {
	for _, tag := range metaTags {
		s = strings.ReplaceAll(s, tag, "")
	}
	return strings.TrimSpace(s)
}

func extractText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return cleanMeta(s)
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, cleanMeta(b.Text))
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// ── Codex session loading ───────────────────────────────────────────────────

type codexIndexEntry struct {
	ID         string `json:"id"`
	ThreadName string `json:"thread_name"`
	UpdatedAt  string `json:"updated_at"`
	CWD        string `json:"cwd"`
}

type codexLine struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type codexPayload struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type codexContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func extractCodexText(raw json.RawMessage) string {
	var blocks []codexContentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "input_text" || b.Type == "output_text" {
				if t := strings.TrimSpace(b.Text); t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	return ""
}

func findCodexSessionFile(id string) string {
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, ".codex", "sessions")
	pattern := filepath.Join(base, "**", "rollout-*-"+id+".jsonl")
	matches, _ := filepath.Glob(pattern)
	if len(matches) > 0 {
		return matches[0]
	}
	// Walk year/month/day directories
	years, _ := os.ReadDir(base)
	for _, y := range years {
		if !y.IsDir() {
			continue
		}
		months, _ := os.ReadDir(filepath.Join(base, y.Name()))
		for _, m := range months {
			if !m.IsDir() {
				continue
			}
			days, _ := os.ReadDir(filepath.Join(base, y.Name(), m.Name()))
			for _, d := range days {
				if !d.IsDir() {
					continue
				}
				files, _ := os.ReadDir(filepath.Join(base, y.Name(), m.Name(), d.Name()))
				for _, f := range files {
					if strings.Contains(f.Name(), id) {
						return filepath.Join(base, y.Name(), m.Name(), d.Name(), f.Name())
					}
				}
			}
		}
	}
	return ""
}

func loadCodexSession(entry codexIndexEntry) *Session {
	sessionFile := findCodexSessionFile(entry.ID)
	if sessionFile == "" {
		return nil
	}

	info, err := os.Stat(sessionFile)
	if err != nil {
		return nil
	}

	cwd := entry.CWD
	home, _ := os.UserHomeDir()
	projectName := lastSegment(cwd)
	if projectName == "" || strings.HasPrefix(projectName, "20") || cwd == home {
		projectName = "미분류"
	}

	sess := &Session{
		ID:          entry.ID,
		ProjectDir:  cwd,
		ProjectName: projectName,
		SessionFile: sessionFile,
		ModTime:     info.ModTime(),
		FileSize:    info.Size(),
		CWD:         cwd,
		Provider:    ProviderCodex,
	}

	// Use file ModTime (more accurate than index updated_at, which may be stale)

	f, err := os.Open(sessionFile)
	if err != nil {
		return nil
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 10<<20)

	for sc.Scan() {
		var line codexLine
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue
		}
		// Read CWD from session_meta
		if line.Type == "session_meta" && line.Payload != nil {
			var meta struct {
				CWD string `json:"cwd"`
			}
			if json.Unmarshal(line.Payload, &meta) == nil && meta.CWD != "" {
				sess.CWD = meta.CWD
				sess.ProjectDir = meta.CWD
				home, _ := os.UserHomeDir()
				name := lastSegment(meta.CWD)
				if strings.HasPrefix(name, "20") || meta.CWD == home {
					name = "미분류"
				}
				sess.ProjectName = name
			}
			continue
		}
		if line.Type != "response_item" || line.Payload == nil {
			continue
		}
		var payload codexPayload
		if json.Unmarshal(line.Payload, &payload) != nil {
			continue
		}
		if payload.Role != "user" && payload.Role != "assistant" {
			continue
		}
		text := extractCodexText(payload.Content)
		if text == "" {
			continue
		}
		// Skip very long system-like messages
		if len(text) > 2000 {
			text = text[:2000] + "..."
		}

		msgType := payload.Role
		sess.Messages = append(sess.Messages, Message{Type: msgType, Content: text})
		sess.MessageCount++

		if msgType == "user" {
			sess.UserMsgCount++
			c := strings.TrimSpace(text)
			if len(c) > 3 && !strings.HasPrefix(c, "#") && !strings.HasPrefix(c, "<") {
				if sess.FirstUserMsg == "" {
					sess.FirstUserMsg = c
				}
				sess.LastUserMsg = c
			}
		} else {
			sess.AsstMsgCount++
		}
	}

	// Use thread_name as session title (Codex desktop shows this as session name)
	if entry.ThreadName != "" {
		sess.Alias = entry.ThreadName
		if len(sess.Alias) > 40 {
			sess.Alias = sess.Alias[:37] + "..."
		}
	}
	if sess.FirstUserMsg == "" && entry.ThreadName != "" {
		sess.FirstUserMsg = entry.ThreadName
	}

	if sess.MessageCount == 0 {
		return nil
	}
	return sess
}

func discoverCodexSessions() []*Session {
	home, _ := os.UserHomeDir()
	indexPath := filepath.Join(home, ".codex", "session_index.jsonl")

	f, err := os.Open(indexPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	// Deduplicate by ID — keep latest entry only
	seen := make(map[string]codexIndexEntry)
	var order []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 10<<20)

	for sc.Scan() {
		var entry codexIndexEntry
		if json.Unmarshal(sc.Bytes(), &entry) != nil {
			continue
		}
		if _, exists := seen[entry.ID]; !exists {
			order = append(order, entry.ID)
		}
		seen[entry.ID] = entry // last write wins (latest update)
	}

	var out []*Session
	for _, id := range order {
		entry := seen[id]
		if s := loadCodexSession(entry); s != nil {
			out = append(out, s)
		}
	}
	return out
}

// ── Session deletion ────────────────────────────────────────────────────────

func trashDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "session-trash")
}

func deleteSession(s *Session) error {
	// Move to trash instead of deleting
	trash := trashDir()
	os.MkdirAll(trash, 0755)

	destName := filepath.Base(s.SessionFile)
	destPath := filepath.Join(trash, destName)
	if err := os.Rename(s.SessionFile, destPath); err != nil {
		// If rename fails (cross-device), copy then delete
		data, readErr := os.ReadFile(s.SessionFile)
		if readErr != nil {
			return readErr
		}
		if writeErr := os.WriteFile(destPath, data, 0644); writeErr != nil {
			return writeErr
		}
		os.Remove(s.SessionFile)
	}

	// Save metadata for restore
	meta := map[string]string{
		"originalPath": s.SessionFile,
		"provider":     fmt.Sprintf("%d", s.Provider),
		"id":           s.ID,
		"deletedAt":    time.Now().Format(time.RFC3339),
	}
	metaData, _ := json.Marshal(meta)
	os.WriteFile(destPath+".meta", metaData, 0644)

	// For Codex sessions, also remove from session_index.jsonl
	if s.Provider == ProviderCodex {
		removeFromCodexIndex(s.ID)
	}
	return nil
}

func cleanOldTrash() {
	items := listTrash()
	for _, item := range items {
		if t, err := time.Parse(time.RFC3339, item["deletedAt"]); err == nil {
			if time.Since(t) > 30*24*time.Hour {
				permanentDeleteTrash(item)
			}
		}
	}
}

func listTrash() []map[string]string {
	trash := trashDir()
	entries, err := os.ReadDir(trash)
	if err != nil {
		return nil
	}
	var items []map[string]string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".meta") {
			data, err := os.ReadFile(filepath.Join(trash, e.Name()))
			if err != nil {
				continue
			}
			var meta map[string]string
			if json.Unmarshal(data, &meta) == nil {
				meta["trashFile"] = filepath.Join(trash, strings.TrimSuffix(e.Name(), ".meta"))
				items = append(items, meta)
			}
		}
	}
	return items
}

func restoreFromTrash(item map[string]string) error {
	trashFile := item["trashFile"]
	origPath := item["originalPath"]
	// Ensure parent dir exists
	os.MkdirAll(filepath.Dir(origPath), 0755)
	if err := os.Rename(trashFile, origPath); err != nil {
		return err
	}
	os.Remove(trashFile + ".meta")
	return nil
}

func permanentDeleteTrash(item map[string]string) error {
	os.Remove(item["trashFile"])
	os.Remove(item["trashFile"] + ".meta")
	return nil
}

func removeFromCodexIndex(id string) {
	home, _ := os.UserHomeDir()
	indexPath := filepath.Join(home, ".codex", "session_index.jsonl")

	data, err := os.ReadFile(indexPath)
	if err != nil {
		return
	}

	var kept []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var entry codexIndexEntry
		if json.Unmarshal([]byte(line), &entry) == nil && entry.ID == id {
			continue // skip this entry
		}
		kept = append(kept, line)
	}

	os.WriteFile(indexPath, []byte(strings.Join(kept, "\n")+"\n"), 0644)
}

// ── Codex pin detection ─────────────────────────────────────────────────────

func saveCodexPins(pins map[string]bool) {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".codex", ".codex-global-state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var state map[string]json.RawMessage
	if json.Unmarshal(data, &state) != nil {
		return
	}
	var ids []string
	for id := range pins {
		ids = append(ids, id)
	}
	raw, _ := json.Marshal(ids)
	state["pinned-thread-ids"] = raw
	out, _ := json.MarshalIndent(state, "", "  ")
	os.WriteFile(path, out, 0644)
}

func loadCodexPins() map[string]bool {
	home, _ := os.UserHomeDir()
	data, err := os.ReadFile(filepath.Join(home, ".codex", ".codex-global-state.json"))
	if err != nil {
		return nil
	}
	var state map[string]json.RawMessage
	if json.Unmarshal(data, &state) != nil {
		return nil
	}
	raw, ok := state["pinned-thread-ids"]
	if !ok {
		return nil
	}
	var ids []string
	if json.Unmarshal(raw, &ids) != nil {
		return nil
	}
	pins := make(map[string]bool)
	for _, id := range ids {
		pins[id] = true
	}
	return pins
}

// ── Pin persistence (local, works for both Claude and Codex) ────────────────

func pinFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "session-pins.json")
}

func unpinFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "session-unpins.json")
}

func loadUnpins() map[string]bool {
	unpins := make(map[string]bool)
	data, err := os.ReadFile(unpinFilePath())
	if err == nil {
		var ids []string
		if json.Unmarshal(data, &ids) == nil {
			for _, id := range ids {
				unpins[id] = true
			}
		}
	}
	return unpins
}

func saveUnpins(unpins map[string]bool) {
	var ids []string
	for id := range unpins {
		ids = append(ids, id)
	}
	data, _ := json.MarshalIndent(ids, "", "  ")
	os.WriteFile(unpinFilePath(), data, 0644)
}

func loadPins() map[string]bool {
	// Load our local pins
	pins := make(map[string]bool)
	data, err := os.ReadFile(pinFilePath())
	if err == nil {
		var ids []string
		if json.Unmarshal(data, &ids) == nil {
			for _, id := range ids {
				pins[id] = true
			}
		}
	}
	// Also load Codex desktop pins (minus locally unpinned)
	unpins := loadUnpins()
	codexPins := loadCodexPins()
	for id := range codexPins {
		if !unpins[id] {
			pins[id] = true
		}
	}
	return pins
}

func savePins(pins map[string]bool) error {
	var ids []string
	for id := range pins {
		ids = append(ids, id)
	}
	data, err := json.MarshalIndent(ids, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(pinFilePath(), data, 0644)
}

// ── Alias persistence ───────────────────────────────────────────────────────

// ── Project alias persistence ────────────────────────────────────────────────

func projectAliasFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "project-aliases.json")
}

func loadProjectAliases() map[string]string {
	data, err := os.ReadFile(projectAliasFilePath())
	if err != nil {
		return make(map[string]string)
	}
	var aliases map[string]string
	if json.Unmarshal(data, &aliases) != nil {
		return make(map[string]string)
	}
	return aliases
}

func saveProjectAliases(aliases map[string]string) {
	data, _ := json.MarshalIndent(aliases, "", "  ")
	os.WriteFile(projectAliasFilePath(), data, 0644)
}

func aliasFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "session-aliases.json")
}

func loadAliases() map[string]string {
	data, err := os.ReadFile(aliasFilePath())
	if err != nil {
		return make(map[string]string)
	}
	var aliases map[string]string
	if json.Unmarshal(data, &aliases) != nil {
		return make(map[string]string)
	}
	return aliases
}

func saveAliases(aliases map[string]string) error {
	data, err := json.MarshalIndent(aliases, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(aliasFilePath(), data, 0644)
}

// ── Metadata (folders & tags) persistence ───────────────────────────────────

type Metadata struct {
	Folders        []string            `json:"folders"`
	SessionFolders map[string]string   `json:"session_folders"`
	SessionTags    map[string][]string `json:"session_tags"`
	FolderCollapsed map[string]bool    `json:"folder_collapsed"`
	FolderColors   map[string]string   `json:"folder_colors"`
	TempSessions   map[string]bool    `json:"temp_sessions"`
}

func metadataFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "csm-metadata.json")
}

func loadMetadata() *Metadata {
	m := &Metadata{
		Folders:         []string{},
		SessionFolders:  make(map[string]string),
		SessionTags:     make(map[string][]string),
		FolderCollapsed: make(map[string]bool),
		FolderColors:    make(map[string]string),
		TempSessions:    make(map[string]bool),
	}
	data, err := os.ReadFile(metadataFilePath())
	if err != nil {
		return m
	}
	if json.Unmarshal(data, m) != nil {
		return m
	}
	if m.SessionFolders == nil {
		m.SessionFolders = make(map[string]string)
	}
	if m.SessionTags == nil {
		m.SessionTags = make(map[string][]string)
	}
	if m.FolderCollapsed == nil {
		m.FolderCollapsed = make(map[string]bool)
	}
	if m.FolderColors == nil {
		m.FolderColors = make(map[string]string)
	}
	if m.TempSessions == nil {
		m.TempSessions = make(map[string]bool)
	}
	return m
}

func saveMetadata(m *Metadata) {
	data, _ := json.MarshalIndent(m, "", "  ")
	os.WriteFile(metadataFilePath(), data, 0644)
}

func generateUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ── Expiry helpers ──────────────────────────────────────────────────────────

func daysUntilExpiry(s *Session) int {
	expiry := s.ModTime.AddDate(0, 0, sessionExpiryDays)
	return int(time.Until(expiry).Hours() / 24)
}

func expiryIcon(s *Session) string {
	d := daysUntilExpiry(s)
	switch {
	case d < 0:
		return "\xf0\x9f\x92\x80" // 💀 expired
	case d <= 3:
		return "\xf0\x9f\x94\xb4" // 🔴 expiring very soon
	case d <= 7:
		return "\xf0\x9f\x9f\xa1" // 🟡 expiring soon
	default:
		return "\xf0\x9f\x9f\xa2" // 🟢 healthy
	}
}

func expiryLabel(s *Session) string {
	d := daysUntilExpiry(s)
	if d < 0 {
		return fmt.Sprintf("D+%d", -d)
	}
	if d == 0 {
		return "D-Day"
	}
	return fmt.Sprintf("D-%d", d)
}

// ── Session loading ─────────────────────────────────────────────────────────

func loadSessionFast(path string) *Session {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	dir := filepath.Base(filepath.Dir(path))
	ppath := decodePath(dir)
	home, _ := os.UserHomeDir()
	pname := lastSegment(ppath)
	if ppath == home {
		pname = "미분류"
	}
	return &Session{
		ID:          strings.TrimSuffix(filepath.Base(path), ".jsonl"),
		ProjectDir:  ppath,
		ProjectName: pname,
		SessionFile: path,
		ModTime:     info.ModTime(),
		FileSize:    info.Size(),
	}
}

func loadSessionDetail(sess *Session) {
	f, err := os.Open(sess.SessionFile)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 10<<20)

	for sc.Scan() {
		var raw rawLine
		if json.Unmarshal(sc.Bytes(), &raw) != nil {
			continue
		}
		if raw.Type == "custom-title" {
			var ct struct {
				CustomTitle string `json:"customTitle"`
			}
			if json.Unmarshal(sc.Bytes(), &ct) == nil && ct.CustomTitle != "" {
				sess.Alias = ct.CustomTitle
			}
			continue
		}
		if raw.Type != "user" && raw.Type != "assistant" {
			continue
		}
		var env msgEnvelope
		if json.Unmarshal(raw.Message, &env) != nil {
			continue
		}
		if env.Usage != nil {
			sess.InputTokens += env.Usage.InputTokens
			sess.OutputTokens += env.Usage.OutputTokens
		}
		text := extractText(env.Content)
		if text == "" {
			continue
		}

		sess.Messages = append(sess.Messages, Message{Type: raw.Type, Content: text})
		sess.MessageCount++

		if sess.GitBranch == "" && raw.GitBranch != "" {
			sess.GitBranch = raw.GitBranch
		}
		if sess.CWD == "" && raw.CWD != "" {
			sess.CWD = raw.CWD
		}
		if sess.Entrypoint == "" && raw.Entrypoint != "" {
			sess.Entrypoint = raw.Entrypoint
		}

		if raw.Type == "user" {
			sess.UserMsgCount++
			c := strings.TrimSpace(text)
			if isMeaningfulMsg(c) {
				if sess.FirstUserMsg == "" {
					sess.FirstUserMsg = c
				}
				sess.LastUserMsg = c
			}
		} else {
			sess.AsstMsgCount++
		}
	}
	sess.Loaded = true
}

func loadSession(path string) *Session {
	sess := loadSessionFast(path)
	if sess == nil {
		return nil
	}
	loadSessionDetail(sess)
	if sess.MessageCount == 0 || (sess.UserMsgCount <= 1 && sess.AsstMsgCount <= 1) {
		return nil
	}
	return sess
}

func discoverSessionsFast() []*Session {
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, ".claude", "projects")
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	aliases := loadAliases()
	var out []*Session
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(base, e.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if strings.HasSuffix(f.Name(), ".jsonl") {
				if s := loadSessionFast(filepath.Join(dir, f.Name())); s != nil {
					if alias, ok := aliases[s.ID]; ok {
						s.Alias = alias
					}
					out = append(out, s)
				}
			}
		}
	}
	codexSessions := discoverCodexSessions()
	for _, cs := range codexSessions {
		if alias, ok := aliases[cs.ID]; ok {
			cs.Alias = alias
		}
		out = append(out, cs)
	}
	pins := loadPins()
	for _, s := range out {
		if pins[s.ID] {
			s.Pinned = true
		}
	}
	for _, s := range out {
		s.Active = isSessionActiveByProcess(s.ID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out
}

func discoverSessions() []*Session {
	out := discoverSessionsFast()
	valid := out[:0]
	for _, s := range out {
		loadSessionDetail(s)
		if s.MessageCount > 0 && (s.UserMsgCount > 1 || s.AsstMsgCount > 1) {
			valid = append(valid, s)
		}
	}
	return valid
}

// ── Terminal backend detection & integration ────────────────────────────────

type termBackend int

const (
	backendITerm2 termBackend = iota
	backendTerminalApp
	backendTmux
	backendKitty
	backendWezTerm
	backendGhostty
	backendSSH
	backendWindows
	backendTabby
	backendFallback
)

func (b termBackend) String() string {
	names := [...]string{"iTerm2", "Terminal.app", "tmux", "Kitty", "WezTerm", "Ghostty", "SSH", "Windows", "Tabby", "fallback"}
	if int(b) < len(names) {
		return names[b]
	}
	return "unknown"
}

var activeBackend termBackend

func detectBackend() termBackend {
	if os.Getenv("TMUX") != "" {
		return backendTmux
	}
	switch os.Getenv("TERM_PROGRAM") {
	case "iTerm.app":
		return backendITerm2
	case "Apple_Terminal":
		return backendTerminalApp
	case "WezTerm":
		return backendWezTerm
	case "ghostty":
		return backendGhostty
	case "Tabby":
		return backendTabby
	}
	if os.Getenv("KITTY_PID") != "" {
		return backendKitty
	}
	// SSH detection — use tmux to open sessions in new window
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_CLIENT") != "" {
		return backendSSH
	}
	if runtime.GOOS == "windows" {
		return backendWindows
	}
	return backendFallback
}

func escapeShell(s string) string {
	return strings.ReplaceAll(s, "'", "'\\''")
}

func escapeAppleScript(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	return strings.ReplaceAll(s, "\"", "\\\"")
}

func runAppleScript(script string) error {
	return exec.Command("osascript", "-e", script).Run()
}

func openInTerminal(command, dir string, inTab bool, app *tview.Application) error {
	switch activeBackend {
	case backendITerm2:
		return iterm2Open(command, dir, inTab)
	case backendTerminalApp:
		return terminalAppOpen(command, dir, inTab)
	case backendTmux:
		return tmuxOpen(command, dir, inTab)
	case backendKitty:
		return kittyOpen(command, dir, inTab)
	case backendWezTerm:
		return weztermOpen(command, dir, inTab)
	case backendGhostty:
		return ghosttyOpen(command, dir, inTab)
	case backendSSH:
		return sshOpen(command, dir, inTab, app)
	case backendWindows:
		return windowsOpen(command, dir, app)
	case backendTabby:
		return tabbyOpen(command, dir, app)
	default:
		var runErr error
		app.Suspend(func() {
			var cmd *exec.Cmd
			if runtime.GOOS == "windows" {
				args := strings.Fields(command)
				cmd = exec.Command(args[0], args[1:]...)
				cmd.Dir = dir
			} else {
				cmd = exec.Command("sh", "-c", fmt.Sprintf("cd '%s' && %s", escapeShell(dir), command))
			}
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			runErr = cmd.Run()
		})
		return runErr
	}
}

func iterm2Open(command, dir string, inTab bool) error {
	cmd := escapeAppleScript(command)
	d := escapeAppleScript(dir)
	if inTab {
		return runAppleScript(fmt.Sprintf(`tell application "iTerm2"
	tell current window
		set newTab to (create tab with default profile)
		tell current session of newTab
			write text "cd \"%s\" && %s"
		end tell
	end tell
end tell`, d, cmd))
	}
	return runAppleScript(fmt.Sprintf(`tell application "iTerm2"
	tell current session of current window
		set newSession to (split vertically with default profile)
		tell newSession
			write text "cd \"%s\" && %s"
		end tell
	end tell
end tell`, d, cmd))
}

func terminalAppOpen(command, dir string, inTab bool) error {
	cmd := escapeAppleScript(command)
	d := escapeAppleScript(dir)
	if inTab {
		return runAppleScript(fmt.Sprintf(`tell application "System Events"
	tell process "Terminal"
		keystroke "t" using command down
	end tell
end tell
delay 0.3
tell application "Terminal"
	do script "cd \"%s\" && %s" in front window
end tell`, d, cmd))
	}
	return runAppleScript(fmt.Sprintf(`tell application "Terminal"
	activate
	do script "cd \"%s\" && %s"
end tell`, d, cmd))
}

func tmuxOpen(command, dir string, inTab bool) error {
	fullCmd := fmt.Sprintf("cd '%s' && %s", escapeShell(dir), command)
	if inTab {
		return exec.Command("tmux", "new-window", "-c", dir, fullCmd).Run()
	}
	return exec.Command("tmux", "split-window", "-h", "-c", dir, fullCmd).Run()
}

func kittyOpen(command, dir string, inTab bool) error {
	fullCmd := fmt.Sprintf("cd '%s' && %s", escapeShell(dir), command)
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	launchType := "window"
	if inTab {
		launchType = "tab"
	}
	return exec.Command("kitty", "@", "launch", "--type="+launchType, "--cwd="+dir, shell, "-c", fullCmd).Run()
}

func weztermOpen(command, dir string, inTab bool) error {
	fullCmd := fmt.Sprintf("cd '%s' && %s", escapeShell(dir), command)
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	if inTab {
		return exec.Command("wezterm", "cli", "spawn", "--cwd", dir, "--", shell, "-c", fullCmd).Run()
	}
	return exec.Command("wezterm", "cli", "split-pane", "--right", "--cwd", dir, "--", shell, "-c", fullCmd).Run()
}

func ghosttyOpen(command, dir string, inTab bool) error {
	fullCmd := fmt.Sprintf("cd '%s' && exec %s", escapeShell(dir), command)
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.Command("ghostty", "-e", shell, "-c", fullCmd)
	cmd.Dir = dir
	return cmd.Start()
}

func sshOpen(command, dir string, inTab bool, app *tview.Application) error {
	fullCmd := fmt.Sprintf("cd '%s' && %s", escapeShell(dir), command)
	// If tmux is available, use it to open a new window (csm stays running)
	if _, err := exec.LookPath("tmux"); err == nil {
		// Check if we're inside tmux (user started tmux manually)
		if os.Getenv("TMUX") != "" {
			return tmuxOpen(command, dir, inTab)
		}
		// Not in tmux — create a new detached tmux session and attach
		sessionName := fmt.Sprintf("csm-%d", time.Now().UnixNano()%10000)
		if err := exec.Command("tmux", "new-session", "-d", "-s", sessionName, "-c", dir, fullCmd).Run(); err != nil {
			return err
		}
		// Suspend TUI, attach to tmux session, resume TUI when detached/exited
		var runErr error
		app.Suspend(func() {
			cmd := exec.Command("tmux", "attach-session", "-t", sessionName)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			runErr = cmd.Run()
		})
		return runErr
	}
	// No tmux — fallback to suspend
	var runErr error
	app.Suspend(func() {
		cmd := exec.Command("sh", "-c", fullCmd)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		runErr = cmd.Run()
	})
	return runErr
}

func windowsOpen(command, dir string, app *tview.Application) error {
	// Try Windows Terminal (wt.exe) first — opens new tab
	if _, err := exec.LookPath("wt.exe"); err == nil {
		fullCmd := fmt.Sprintf("cd /d \"%s\" && %s", dir, command)
		return exec.Command("wt.exe", "-w", "0", "new-tab", "--startingDirectory", dir, "cmd", "/c", fullCmd).Run()
	}
	// Fallback: suspend TUI and run inline
	var runErr error
	app.Suspend(func() {
		args := strings.Fields(command)
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		runErr = cmd.Run()
	})
	return runErr
}

func findTabbyExe() string {
	for _, name := range []string{"tabby", "Tabby", "tabby.exe", "Tabby.exe"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	for _, envVar := range []string{"LOCALAPPDATA", "PROGRAMFILES", "PROGRAMFILES(X86)"} {
		base := os.Getenv(envVar)
		if base == "" {
			continue
		}
		for _, sub := range []string{
			filepath.Join("Programs", "Tabby", "Tabby.exe"),
			filepath.Join("Tabby", "Tabby.exe"),
		} {
			p := filepath.Join(base, sub)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	if runtime.GOOS == "darwin" {
		p := "/Applications/Tabby.app/Contents/MacOS/Tabby"
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if runtime.GOOS == "windows" {
		out, err := exec.Command("powershell", "-NoProfile", "-Command",
			"(Get-Process Tabby -ErrorAction SilentlyContinue | Select-Object -First 1).Path").Output()
		if err == nil {
			if p := strings.TrimSpace(string(out)); p != "" {
				return p
			}
		}
	}
	return ""
}

func tabbyOpen(command, dir string, app *tview.Application) error {
	tabbyPath := findTabbyExe()
	if tabbyPath != "" {
		// Open new tab in project directory
		openCmd := exec.Command(tabbyPath, "open", dir)
		if err := openCmd.Run(); err == nil {
			// Use osascript (macOS) or PowerShell (Windows) to type command
			time.Sleep(800 * time.Millisecond)
			return tabbyTypeCommand(command)
		}
	}
	// Fallback to inline
	var runErr error
	app.Suspend(func() {
		args := strings.Fields(command)
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		runErr = cmd.Run()
	})
	return runErr
}

func tabbyTypeCommand(command string) error {
	if runtime.GOOS == "darwin" {
		script := fmt.Sprintf(`set the clipboard to "%s"
tell application "System Events"
	tell process "Tabby"
		keystroke "v" using command down
		delay 0.1
		key code 36
		delay 0.3
		key code 48 using {control down, shift down}
	end tell
end tell`, escapeAppleScript(command))
		return exec.Command("osascript", "-e", script).Run()
	}
	if runtime.GOOS == "windows" {
		ps := fmt.Sprintf(`Set-Clipboard '%s'; `+
			`Add-Type -AssemblyName System.Windows.Forms; `+
			`Start-Sleep -Milliseconds 200; `+
			`[System.Windows.Forms.SendKeys]::SendWait('^v{ENTER}'); `+
			`Start-Sleep -Milliseconds 300; `+
			`[System.Windows.Forms.SendKeys]::SendWait('^+{TAB}')`,
			strings.ReplaceAll(command, "'", "''"))
		return exec.Command("powershell", "-NoProfile", "-Command", ps).Run()
	}
	return fmt.Errorf("tabby tab typing not supported on %s", runtime.GOOS)
}

// ── AI Summary ──────────────────────────────────────────────────────────────

func buildConversationDigest(s *Session, budget int) string {
	var b strings.Builder
	for _, msg := range s.Messages {
		content := msg.Content
		if len(content) > 300 {
			content = content[:300] + "..."
		}
		line := fmt.Sprintf("[%s]: %s\n", msg.Type, content)
		if b.Len()+len(line) > budget {
			break
		}
		b.WriteString(line)
	}
	return b.String()
}

func generateSummary(s *Session) (string, error) {
	digest := buildConversationDigest(s, 4000)
	prompt := fmt.Sprintf(
		"Summarize this Claude Code session in 3-5 bullet points. "+
			"Focus on: what the user was trying to accomplish, key decisions made, and current status. Be concise.\n\n"+
			"Project: %s\nPath: %s\n\nConversation:\n%s",
		s.ProjectName, s.ProjectDir, digest,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "claude", "-p", "--model", "haiku", "--bare", "--no-session-persistence")
	cmd.Stdin = strings.NewReader(prompt)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("timed out after 30s")
	}
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, string(out))
	}
	return strings.TrimSpace(string(out)), nil
}

// ── Helper: get session from tree node ──────────────────────────────────────

func nodeSession(node *tview.TreeNode) *Session {
	if node == nil || node.GetReference() == nil {
		return nil
	}
	s, ok := node.GetReference().(*Session)
	if !ok {
		return nil
	}
	return s
}

func nodeRefStr(node *tview.TreeNode) string {
	if node == nil || node.GetReference() == nil {
		return ""
	}
	s, ok := node.GetReference().(string)
	if !ok {
		return ""
	}
	return s
}

// ── Session node text formatting ────────────────────────────────────────────

func highlightText(text, query string) string {
	if query == "" {
		return text
	}
	lower := strings.ToLower(text)
	lowerQ := strings.ToLower(query)
	idx := strings.Index(lower, lowerQ)
	if idx < 0 {
		return text
	}
	return text[:idx] + "[yellow]" + text[idx:idx+len(query)] + "[-][white]" + text[idx+len(query):]
}

// blinkPhase toggles every 500ms for active session indicator
var blinkPhase bool

func activeIconFor(s *Session) string {
	if !s.Active {
		return "  "
	}
	env := sessionActiveEnv(s.ID)
	color := "#39FF14" // 형광 초록 (로컬)
	if env == "ssh" {
		color = "#00F0FF" // 형광 파랑 (원격)
	}
	if blinkPhase {
		return fmt.Sprintf("[%s]●[-]", color)
	}
	return " "
}

func formatTokens(n int64) string {
	if n <= 0 {
		return "0"
	}
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}

func sessionNodeTextCompact(s *Session, searchQuery ...string) string {
	activeIcon := activeIconFor(s)
	title := s.Alias
	if title == "" {
		title = trunc(s.FirstUserMsg, 25)
	}
	if title == "" {
		title = fmt.Sprintf("%d개", s.MessageCount)
	}
	displayTitle := title
	if len(searchQuery) > 0 && searchQuery[0] != "" {
		displayTitle = highlightText(title, searchQuery[0])
	}
	col1 := padRight(activeIcon, 2)
	col2 := padRight(fmt.Sprintf("[white]%s[-]", esc(displayTitle)), 22)
	col3 := padRight(fmt.Sprintf("[#999999]%s[-]", expiryLabel(s)), 5)
	tokenStr := ""
	if total := s.InputTokens + s.OutputTokens; total > 0 {
		tokenStr = fmt.Sprintf(" [#888888]%s[-]", formatTokens(total))
	}
	return fmt.Sprintf("%s%s %s%s", col1, col2, col3, tokenStr)
}

func sessionNodeText(s *Session, searchQuery ...string) string {
	check := " "
	if s.Selected {
		check = "\xe2\x9c\x93" // ✓
	}
	title := s.Alias
	if title == "" {
		title = trunc(s.FirstUserMsg, 40)
	}
	if title == "" {
		title = fmt.Sprintf("%d개 메시지", s.MessageCount)
	}
	epIco := entrypointIcon(s.Entrypoint)
	if s.Provider == ProviderCodex {
		epIco = "DSK"
	}
	dateColor := "[#999999]"
	if daysUntilExpiry(s) < 0 {
		dateColor = "[#FF4444]"
	} else if time.Since(s.ModTime) < 7*24*time.Hour {
		dateColor = "[#00ff00]"
	}
	activeIcon := activeIconFor(s)

	col1 := padRight(fmt.Sprintf("%s%s", check, activeIcon), 4)
	col2 := padRight(fmt.Sprintf("%s%s[-]", dateColor, s.ModTime.Format("01/02 15:04")), 12)
	displayTitle := title
	if len(searchQuery) > 0 && searchQuery[0] != "" {
		displayTitle = highlightText(title, searchQuery[0])
	}
	col3 := padRight(fmt.Sprintf("[white]%s[-]", esc(displayTitle)), 30)
	col4 := padRight(fmt.Sprintf("[#999999]%s[-]", epIco), 4)
	col5 := padRight(fmt.Sprintf("[#999999]%s[-]", expiryLabel(s)), 6)
	tokenStr := ""
	if total := s.InputTokens + s.OutputTokens; total > 0 {
		tokenStr = fmt.Sprintf("  [#888888]%s tok[-]", formatTokens(total))
	}
	return fmt.Sprintf("%s%s  %s  %s  %s%s", col1, col2, col3, col4, col5, tokenStr)
}

// ── Main ────────────────────────────────────────────────────────────────────

func main() {
	// CLI flags
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-v":
			fmt.Printf("csm v%s (%s/%s)\n", currentVersion(), runtime.GOOS, runtime.GOARCH)
			return
		case "--update", "-u":
			if err := selfUpdate(); err != nil {
				fmt.Fprintf(os.Stderr, "업데이트 실패: %v\n", err)
				os.Exit(1)
			}
			return
		case "--config":
			cfg := loadConfig()
			data, _ := json.MarshalIndent(cfg, "", "  ")
			fmt.Println("설정 파일:", configPath())
			fmt.Println(string(data))
			return
		case "--help", "-h":
			fmt.Println("csm — Claude Code + Codex 세션 매니저")
			fmt.Printf("버전: v%s\n\n", currentVersion())
			fmt.Println("사용법:")
			fmt.Println("  csm              TUI 실행")
			fmt.Println("  csm --version    버전 표시")
			fmt.Println("  csm --update     최신 버전으로 업데이트")
			fmt.Println("  csm --config     설정 파일 보기")
			fmt.Println("  csm --help       도움말")
			return
		}
	}

	// Load config
	cfg := loadConfig()
	sessionExpiryDays = cfg.ExpiryDays

	cleanOldTrash()
	sessions := discoverSessionsFast()

	activeBackend = detectBackend()
	app := tview.NewApplication()
	tview.Styles.PrimitiveBackgroundColor = tcell.NewRGBColor(0, 0, 0)
	summaryCache := make(map[string]string)
	aliases := loadAliases()
	localPins := loadPins()
	localUnpins := loadUnpins()
	projectAliases := loadProjectAliases()
	meta := loadMetadata()
	compactMode := cfg.CompactMode
	var updateInfo string // set by background check

	// ── Widgets ──

	selectedStyle := tcell.StyleDefault.Background(tcell.ColorDarkSlateGray).Foreground(tcell.ColorWhite)
	tree := tview.NewTreeView()
	tree.SetBorder(true).
		SetTitle(" 세션 목록 ").
		SetTitleAlign(tview.AlignLeft).
		SetBorderColor(tcell.ColorGreen)
	tree.SetGraphics(false)
	tree.SetTopLevel(1) // hide root

	infoView := tview.NewTextView().
		SetDynamicColors(true).
		SetWordWrap(true).
		SetScrollable(true)
	infoView.SetBorder(true).
		SetTitle(" 세션 정보 ").
		SetTitleAlign(tview.AlignLeft).
		SetBorderColor(tcell.ColorDodgerBlue)

	convView := tview.NewTextView().
		SetDynamicColors(true).
		SetWordWrap(true).
		SetScrollable(true)
	convView.SetBorder(true).
		SetTitle(" 대화 미리보기 ").
		SetTitleAlign(tview.AlignLeft).
		SetBorderColor(tcell.ColorDodgerBlue)

	searchInput := tview.NewInputField().
		SetLabel(" / ").
		SetLabelColor(tcell.ColorYellow).
		SetFieldBackgroundColor(tcell.ColorDefault).
		SetFieldTextColor(tcell.ColorWhite)

	helpBar := tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignCenter)
	helpBar.SetText("[#666666]? 도움말 | Ctrl+] 터미널⇄사이드바 | Esc 종료[-]")

	statusBar := tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignCenter)

	// ── Layout ──

	termWidget := NewTerminalWidget(app)

	previewPane := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(infoView, 0, 1, false).
		AddItem(convView, 0, 2, false)

	leftPane := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(tree, 0, 2, true).
		AddItem(previewPane, 0, 1, false)

	mainBody := tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(leftPane, 45, 0, true).
		AddItem(termWidget, 0, 1, false)

	mainLayout := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(mainBody, 0, 1, true).
		AddItem(helpBar, 1, 0, false).
		AddItem(statusBar, 1, 0, false)

	termFocused := false

	focusTerminal := func() {
		termFocused = true
		app.SetFocus(termWidget)
		leftPane.SetBorderColor(tcell.ColorDefault)
		termWidget.SetBorderColor(tcell.ColorGreen)
	}

	focusSidebar := func() {
		termFocused = false
		app.SetFocus(tree)
		termWidget.SetBorderColor(tcell.ColorDefault)
	}

	previewOpen := true
	var togglePreview func()
	var rebuildLeftPane func(bool)
	searching := false
	currentFilter := ""

	// Sort modes
	type SortMode int
	const (
		SortByDate SortMode = iota
		SortByName
		SortByExpiry
	)
	sortMode := SortByDate
	switch cfg.DefaultSort {
	case "name":
		sortMode = SortByName
	case "expiry":
		sortMode = SortByExpiry
	}
	sortLabels := map[SortMode]string{
		SortByDate:   "날짜순",
		SortByName:   "이름순",
		SortByExpiry: "만료순",
	}
	sortSessions := func() {
		switch sortMode {
		case SortByDate:
			sort.Slice(sessions, func(i, j int) bool { return sessions[i].ModTime.After(sessions[j].ModTime) })
		case SortByName:
			sort.Slice(sessions, func(i, j int) bool {
				ai, aj := sessions[i].Alias, sessions[j].Alias
				if ai == "" {
					ai = sessions[i].FirstUserMsg
				}
				if aj == "" {
					aj = sessions[j].FirstUserMsg
				}
				return strings.ToLower(ai) < strings.ToLower(aj)
			})
		case SortByExpiry:
			sort.Slice(sessions, func(i, j int) bool { return daysUntilExpiry(sessions[i]) < daysUntilExpiry(sessions[j]) })
		}
	}

	// ── Session display ──

	showSessionInfo := func(s *Session) {
		if s == nil {
			return
		}

		var b strings.Builder
		epInfo := entrypointLabel(s.Entrypoint)
		if s.Provider == ProviderCodex {
			epInfo = "Desktop"
		}
		if epInfo != "" {
			fmt.Fprintf(&b, "[yellow]제공자:[-]    %s %s (%s)\n", s.Provider.Icon(), s.Provider.Label(), epInfo)
		} else {
			fmt.Fprintf(&b, "[yellow]제공자:[-]    %s %s\n", s.Provider.Icon(), s.Provider.Label())
		}
		if s.Active {
			env := sessionActiveEnv(s.ID)
			envLabel := "로컬"
			if env == "ssh" {
				envLabel = "원격 (SSH)"
			}
			fmt.Fprintf(&b, "[yellow]상태:[-]        [lime]▶ 활성[-] (%s)\n", envLabel)
		}
		if s.Pinned {
			fmt.Fprintf(&b, "[yellow]고정:[-]        \xf0\x9f\x93\x8c 고정됨\n")
		}
		fmt.Fprintf(&b, "[yellow]프로젝트:[-]     %s\n", esc(s.ProjectName))
		if s.Alias != "" {
			fmt.Fprintf(&b, "[yellow]별칭:[-]       [aqua]%s[-]\n", esc(s.Alias))
		}
		fmt.Fprintf(&b, "[yellow]경로:[-]        %s\n", esc(s.ProjectDir))
		fmt.Fprintf(&b, "[yellow]세션:[-]     [green]%s[-]\n", s.ID)
		fmt.Fprintf(&b, "[yellow]수정일:[-]    %s\n", s.ModTime.Format("2006-01-02 15:04:05"))
		fmt.Fprintf(&b, "[yellow]만료:[-]      %s %s\n", expiryIcon(s), expiryLabel(s))
		fmt.Fprintf(&b, "[yellow]크기:[-]        %s\n", fmtSize(s.FileSize))
		fmt.Fprintf(&b, "[yellow]메시지:[-]    %d (%d 사용자, %d 어시스턴트)\n", s.MessageCount, s.UserMsgCount, s.AsstMsgCount)
		if total := s.InputTokens + s.OutputTokens; total > 0 {
			fmt.Fprintf(&b, "[yellow]토큰:[-]      %s (입력 %s, 출력 %s)\n", formatTokens(total), formatTokens(s.InputTokens), formatTokens(s.OutputTokens))
		}
		if s.GitBranch != "" {
			fmt.Fprintf(&b, "[yellow]브랜치:[-]  %s\n", esc(s.GitBranch))
		}
		if s.CWD != "" {
			fmt.Fprintf(&b, "[yellow]작업경로:[-]         %s\n", esc(s.CWD))
		}
		if s.FirstUserMsg != "" {
			fmt.Fprintf(&b, "\n[yellow]첫 메시지:[-]\n  %s\n", esc(trunc(s.FirstUserMsg, 200)))
		}
		if s.LastUserMsg != "" && s.LastUserMsg != s.FirstUserMsg {
			fmt.Fprintf(&b, "\n[yellow]마지막 메시지:[-]\n  %s\n", esc(trunc(s.LastUserMsg, 200)))
		}
		if summary, ok := summaryCache[s.ID]; ok {
			fmt.Fprintf(&b, "\n[aqua]── AI 요약 ──[-]\n%s\n", esc(summary))
		} else {
			fmt.Fprintf(&b, "\n[gray][yellow]i[-][gray] 키를 눌러 AI 요약 생성[-]\n")
		}
		infoView.SetText(b.String())
		infoView.ScrollToBeginning()

		var conv strings.Builder
		for _, msg := range s.Messages {
			content := msg.Content
			if len(content) > 500 {
				content = content[:497] + "..."
			}
			if msg.Type == "user" {
				conv.WriteString(fmt.Sprintf("[green]>>> 사용자:[-]\n%s\n\n", esc(content)))
			} else {
				conv.WriteString(fmt.Sprintf("[cyan]<<< 어시스턴트:[-]\n%s\n\n", esc(content)))
			}
		}
		convView.SetText(conv.String())
		convView.ScrollToBeginning()
	}

	togglePreview = func() {
		previewOpen = !previewOpen
		rebuildLeftPane(searching)
		if previewOpen {
			cur := tree.GetCurrentNode()
			if s := nodeSession(cur); s != nil {
				showSessionInfo(s)
			}
		}
	}

	// ── Populate tree ──

	defaultStatus := func() string {
		selected := 0
		totalMsgs := 0
		claudeCount := 0
		codexCount := 0
		expiring := 0
		active := 0
		for _, s := range sessions {
			if s.Selected {
				selected++
			}
			totalMsgs += s.MessageCount
			if s.Provider == ProviderCodex {
				codexCount++
			} else {
				claudeCount++
			}
			d := daysUntilExpiry(s)
			if d >= 0 && d <= 3 {
				expiring++
			}
			if s.Active {
				active++
			}
		}
		sel := ""
		if selected > 0 {
			sel = fmt.Sprintf(" | [red]%d개 선택됨[-]", selected)
		}
		expWarn := ""
		if expiring > 0 {
			expWarn = fmt.Sprintf(" | [red]⚠ %d개 만료 임박[-]", expiring)
		}
		activeInfo := ""
		if active > 0 {
			activeInfo = fmt.Sprintf(" | [lime]▶ %d 활성[-]", active)
		}
		updateNote := ""
		if updateInfo != "" {
			updateNote = " | " + updateInfo
		}
		compactNote := ""
		if compactMode {
			compactNote = " | [aqua]컴팩트[-]"
		}
		return fmt.Sprintf(
			"[green]%d개 세션[-] [gray](🧠%d 🤖%d · %d개 메시지 · %s · %s)[-]%s%s%s%s%s",
			len(sessions), claudeCount, codexCount, totalMsgs, activeBackend, sortLabels[sortMode], sel, activeInfo, expWarn, compactNote, updateNote)
	}

	// Track the previously selected session ID to restore after rebuild
	var lastSelectedID string
	sessionNum := 0
	type activeNodeInfo struct {
		Node    *tview.TreeNode
		Session *Session
		Num     int
	}
	var activeNodes []activeNodeInfo

	populateTree := func(filter string) {
		// Remember currently selected session
		if cur := tree.GetCurrentNode(); cur != nil {
			if s := nodeSession(cur); s != nil {
				lastSelectedID = s.ID
			}
		}

		root := tview.NewTreeNode("root")
		tree.SetRoot(root)

		lowerFilter := strings.ToLower(filter)

		var claudeSessions, codexSessions []*Session
		for _, s := range sessions {
			if lowerFilter != "" {
				haystack := strings.ToLower(s.ProjectName + " " + s.Alias + " " + s.FirstUserMsg + " " + s.LastUserMsg + " " + s.GitBranch)
				// Add tags to haystack
				if tags, ok := meta.SessionTags[s.ID]; ok {
					for _, tag := range tags {
						haystack += " #" + tag
					}
				}
				if !fuzzyMatch(haystack, lowerFilter) {
					continue
				}
			}
			if s.Provider == ProviderCodex {
				codexSessions = append(codexSessions, s)
			} else {
				claudeSessions = append(claudeSessions, s)
			}
		}

		var firstSessionNode *tview.TreeNode
		var restoreNode *tview.TreeNode
		sessionNum = 0
		activeNodes = nil

		addProviderGroup := func(provName string, provColor string, icon string, items []*Session, newRef string) {
			if len(items) == 0 {
				return
			}
			// Provider header node
			provNode := tview.NewTreeNode(fmt.Sprintf("%s%s %s[-] (%d)", provColor, provName, icon, len(items)))
			provNode.SetSelectable(false)
			provNode.SetExpanded(true)
			root.AddChild(provNode)

			// "+ New session" node
			newNode := tview.NewTreeNode(fmt.Sprintf("  [#888888]+ 새 %s 세션[-]", provName))
			newNode.SetReference(newRef)
			newNode.SetSelectable(true)
			newNode.SetSelectedTextStyle(selectedStyle)
			provNode.AddChild(newNode)
			if firstSessionNode == nil {
				firstSessionNode = newNode
			}

			// Split into pinned / normal
			var pinned, normal []*Session
			for _, s := range items {
				if s.Pinned {
					pinned = append(pinned, s)
				} else {
					normal = append(normal, s)
				}
			}

			// Pinned group
			if len(pinned) > 0 {
				pinGroupNode := tview.NewTreeNode(fmt.Sprintf("[#444444]고정 \xf0\x9f\x93\x8c (%d)[-]", len(pinned)))
				pinGroupNode.SetSelectable(true)
				pinGroupNode.SetExpanded(true)
				pinGroupNode.SetSelectedTextStyle(selectedStyle)
				provNode.AddChild(pinGroupNode)
				for _, s := range pinned {
					sessionNum++
					numPrefix := fmt.Sprintf("[#666666]%2d[-] ", sessionNum)
					nodeText := sessionNodeText(s, filter)
					if compactMode {
						nodeText = sessionNodeTextCompact(s, filter)
					}
					// Append tags
					if tags, ok := meta.SessionTags[s.ID]; ok && len(tags) > 0 {
						tagStr := ""
						for _, t := range tags {
							tagStr += " #" + t
						}
						nodeText += fmt.Sprintf(" [#CC66FF]%s[-]", tagStr[1:])
					}
					if meta.TempSessions[s.ID] {
						nodeText += " [#FF6B6B]⏳임시[-]"
					}
					sNode := tview.NewTreeNode(numPrefix + nodeText)
					sNode.SetReference(s)
					sNode.SetSelectable(true)
					sNode.SetSelectedTextStyle(selectedStyle)
					pinGroupNode.AddChild(sNode)
					if s.Active {
						activeNodes = append(activeNodes, activeNodeInfo{Node: sNode, Session: s, Num: sessionNum})
					}
					if firstSessionNode == nil {
						firstSessionNode = sNode
					}
					if lastSelectedID != "" && s.ID == lastSelectedID {
						restoreNode = sNode
					}
				}
			}

			// Normal group — grouped by custom folder or project
			if len(normal) > 0 {
				normGroupNode := tview.NewTreeNode(fmt.Sprintf("[#444444]세션 (%d)[-]", len(normal)))
				normGroupNode.SetSelectable(true)
				normGroupNode.SetExpanded(true)
				normGroupNode.SetSelectedTextStyle(selectedStyle)
				provNode.AddChild(normGroupNode)

				// Separate sessions into custom folders vs project groups
				folderMap := make(map[string][]*Session)
				projectOrder := []string{}
				projectMap := make(map[string][]*Session)
				for _, s := range normal {
					if folderName, ok := meta.SessionFolders[s.ID]; ok {
						folderMap[folderName] = append(folderMap[folderName], s)
					} else {
						if _, exists := projectMap[s.ProjectName]; !exists {
							projectOrder = append(projectOrder, s.ProjectName)
						}
						projectMap[s.ProjectName] = append(projectMap[s.ProjectName], s)
					}
				}

				// Add custom folder nodes first, using meta.Folders order
				// Include all defined folders (even empty ones)
				folderSeen := make(map[string]bool)
				for f := range folderMap {
					folderSeen[f] = true
				}
				for _, f := range meta.Folders {
					if !folderSeen[f] {
						folderMap[f] = nil
						folderSeen[f] = true
					}
				}
				// Build ordered list: meta.Folders first, then any extras
				folderOrder := make([]string, 0, len(folderMap))
				metaSet := make(map[string]bool)
				for _, f := range meta.Folders {
					if folderSeen[f] {
						folderOrder = append(folderOrder, f)
						metaSet[f] = true
					}
				}
				var extras []string
				for f := range folderMap {
					if !metaSet[f] {
						extras = append(extras, f)
					}
				}
				sort.Strings(extras)
				folderOrder = append(folderOrder, extras...)
				for _, folderName := range folderOrder {
					folderSessions := folderMap[folderName]
					folderColor := "#CC66FF"
					if c, ok := meta.FolderColors[folderName]; ok && c != "" {
						folderColor = c
					}
					folderNode := tview.NewTreeNode(fmt.Sprintf("[%s]%s[-] (%d)", folderColor, esc(folderName), len(folderSessions)))
					folderNode.SetReference("folder:" + folderName)
					folderNode.SetSelectable(true)
					collapsed := meta.FolderCollapsed[folderName]
					folderNode.SetExpanded(!collapsed)
					folderNode.SetSelectedTextStyle(selectedStyle)
					normGroupNode.AddChild(folderNode)
					for _, s := range folderSessions {
						sessionNum++
						numPrefix := fmt.Sprintf("[#666666]%2d[-] ", sessionNum)
						nodeText := sessionNodeText(s, filter)
						if compactMode {
							nodeText = sessionNodeTextCompact(s, filter)
						}
						// Append tags
						if tags, ok := meta.SessionTags[s.ID]; ok && len(tags) > 0 {
							tagStr := ""
							for _, t := range tags {
								tagStr += " #" + t
							}
							nodeText += fmt.Sprintf(" [#CC66FF]%s[-]", tagStr[1:])
						}
						if meta.TempSessions[s.ID] {
							nodeText += " [#FF6B6B]⏳임시[-]"
						}
						sNode := tview.NewTreeNode(numPrefix + nodeText)
						sNode.SetReference(s)
						sNode.SetSelectable(true)
						sNode.SetSelectedTextStyle(selectedStyle)
						folderNode.AddChild(sNode)
						if s.Active {
							activeNodes = append(activeNodes, activeNodeInfo{Node: sNode, Session: s, Num: sessionNum})
						}
						if firstSessionNode == nil {
							firstSessionNode = sNode
						}
						if lastSelectedID != "" && s.ID == lastSelectedID {
							restoreNode = sNode
						}
					}
				}

				// Sort project groups: 미분류 always last
				sort.SliceStable(projectOrder, func(i, j int) bool {
					if projectOrder[i] == "미분류" {
						return false
					}
					if projectOrder[j] == "미분류" {
						return true
					}
					return false // keep original order
				})
				for _, projName := range projectOrder {
					projSessions := projectMap[projName]
					displayProj := projName
					if pa, ok := projectAliases[projName]; ok {
						displayProj = pa
					}
					projNode := tview.NewTreeNode(fmt.Sprintf("[#888888]%s[-] (%d)", esc(displayProj), len(projSessions)))
					projNode.SetReference("proj:" + projName)
					projNode.SetSelectable(true)
					projNode.SetExpanded(true)
					projNode.SetSelectedTextStyle(selectedStyle)
					normGroupNode.AddChild(projNode)
					for _, s := range projSessions {
						sessionNum++
						numPrefix := fmt.Sprintf("[#666666]%2d[-] ", sessionNum)
						nodeText := sessionNodeText(s, filter)
						if compactMode {
							nodeText = sessionNodeTextCompact(s, filter)
						}
						// Append tags
						if tags, ok := meta.SessionTags[s.ID]; ok && len(tags) > 0 {
							tagStr := ""
							for _, t := range tags {
								tagStr += " #" + t
							}
							nodeText += fmt.Sprintf(" [#CC66FF]%s[-]", tagStr[1:])
						}
						if meta.TempSessions[s.ID] {
							nodeText += " [#FF6B6B]⏳임시[-]"
						}
						sNode := tview.NewTreeNode(numPrefix + nodeText)
						sNode.SetReference(s)
						sNode.SetSelectable(true)
						sNode.SetSelectedTextStyle(selectedStyle)
						projNode.AddChild(sNode)
						if s.Active {
							activeNodes = append(activeNodes, activeNodeInfo{Node: sNode, Session: s, Num: sessionNum})
						}
						if firstSessionNode == nil {
							firstSessionNode = sNode
						}
						if lastSelectedID != "" && s.ID == lastSelectedID {
							restoreNode = sNode
						}
					}
				}
			}
		}

		if len(claudeSessions) == 0 && len(codexSessions) == 0 && filter == "" {
			emptyNode := tview.NewTreeNode("[#888888]세션이 없습니다[-]")
			emptyNode.SetSelectable(false)
			root.AddChild(emptyNode)
			guideNode := tview.NewTreeNode("[#888888]터미널에서 [white]claude[-][#888888] 또는 [white]codex[-][#888888]를 실행하면 세션이 생성됩니다[-]")
			guideNode.SetSelectable(false)
			root.AddChild(guideNode)
		}
		addProviderGroup("Claude", "[#FF8C00]", "\xf0\x9f\xa7\xa0", claudeSessions, "new-claude")
		addProviderGroup("Codex", "[#4A9EFF]", "\xf0\x9f\xa4\x96", codexSessions, "new-codex")

		// Restore selection or pick first
		if restoreNode != nil {
			tree.SetCurrentNode(restoreNode)
			if s := nodeSession(restoreNode); s != nil && previewOpen {
				showSessionInfo(s)
			}
		} else if firstSessionNode != nil {
			tree.SetCurrentNode(firstSessionNode)
			if s := nodeSession(firstSessionNode); s != nil && previewOpen {
				showSessionInfo(s)
			}
		} else {
			infoView.SetText("[gray]검색 결과 없음[-]")
			convView.SetText("")
		}

		if filter == "" {
			statusBar.SetText(defaultStatus())
		} else {
			total := len(claudeSessions) + len(codexSessions)
			statusBar.SetText(fmt.Sprintf(
				"[green]%d/%d개 세션[-] | [yellow]Esc[-] 취소 | [yellow]Enter[-] 열기 | [yellow]i[-] 요약",
				total, len(sessions),
			))
		}
	}
	populateTree("")

	tree.SetChangedFunc(func(node *tview.TreeNode) {
		if previewOpen {
			if s := nodeSession(node); s != nil {
				showSessionInfo(s)
			}
		}
	})

	// ── Actions ──

	newSessionInDir := func(provider Provider, dir string) {
		var cmdName, label string
		var cmdArgs []string
		if provider == ProviderCodex {
			cmdName = "codex"
			cmdArgs = []string{"--sandbox", "danger-full-access"}
			label = "Codex"
		} else {
			cmdName = "claude"
			cmdArgs = []string{"--dangerously-skip-permissions"}
			label = "Claude"
		}
		termWidget.SetOnExit(func() {
			focusSidebar()
			statusBar.SetText("[yellow]세션 종료됨[-]")
			go func() {
				time.Sleep(2 * time.Second)
				fresh := discoverSessions()
				app.QueueUpdateDraw(func() {
					sessions = fresh
					aliases = loadAliases()
					sortSessions()
					populateTree(currentFilter)
				})
			}()
		})
		if err := termWidget.StartCommand(cmdName, dir, cmdArgs...); err != nil {
			statusBar.SetText(fmt.Sprintf("[red]실패: %v[-]", err))
		} else {
			statusBar.SetText(fmt.Sprintf("[green]새 %s 세션 (%s)[-]", label, lastSegment(dir)))
			focusTerminal()
		}
	}

	newSession := func(provider Provider) {
		home, _ := os.UserHomeDir()

		// Collect unique project dirs for this provider
		projectDirs := make(map[string]string) // name → path
		var projectNames []string
		for _, s := range sessions {
			if s.Provider == provider && s.ProjectDir != "" {
				name := s.ProjectName
				if _, exists := projectDirs[name]; !exists {
					// Verify dir exists
					if _, err := os.Stat(s.ProjectDir); err == nil {
						projectDirs[name] = s.ProjectDir
						projectNames = append(projectNames, name)
					}
				}
			}
		}

		// If only home dir or no projects, open directly
		if len(projectDirs) <= 1 {
			newSessionInDir(provider, home)
			return
		}

		// Show project selection list (vertical)
		label := "Claude"
		if provider == ProviderCodex {
			label = "Codex"
		}

		pickList := tview.NewList().
			ShowSecondaryText(false).
			SetHighlightFullLine(true).
			SetSelectedStyle(tcell.StyleDefault.Background(tcell.ColorGreen).Foreground(tcell.ColorWhite))

		// Add home first
		pickList.AddItem("  홈 ("+lastSegment(home)+")", "", 0, nil)
		pickDirs := []string{home}
		for _, name := range projectNames {
			if projectDirs[name] != home {
				pickList.AddItem("  "+name, "", 0, nil)
				pickDirs = append(pickDirs, projectDirs[name])
			}
		}

		pickList.SetSelectedFunc(func(idx int, _, _ string, _ rune) {
			if idx < len(pickDirs) {
				newSessionInDir(provider, pickDirs[idx])
			}
			app.SetRoot(mainLayout, true)
			app.SetFocus(tree)
		})
		pickList.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
			if ev.Key() == tcell.KeyEscape {
				app.SetRoot(mainLayout, true)
				app.SetFocus(tree)
				return nil
			}
			return ev
		})
		pickList.SetBorder(true).
			SetTitle(fmt.Sprintf(" 새 %s 세션 — 프로젝트 선택 ", label)).
			SetTitleAlign(tview.AlignCenter).
			SetBorderColor(tcell.ColorGreen).
			SetBackgroundColor(tcell.ColorDarkSlateGray)

		modalFlex := tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(nil, 0, 1, false).
			AddItem(tview.NewFlex().
				AddItem(nil, 0, 1, false).
				AddItem(pickList, 40, 0, true).
				AddItem(nil, 0, 1, false), len(pickDirs)+2, 0, true).
			AddItem(nil, 0, 1, false)
		pages := tview.NewPages().
			AddPage("main", mainLayout, true, true).
			AddPage("pick", modalFlex, true, true)
		app.SetRoot(pages, true)
		app.SetFocus(pickList)
	}

	openSessionFromNode := func(node *tview.TreeNode, inTab bool) {
		if node == nil {
			return
		}
		// Check for "new session" references
		ref := nodeRefStr(node)
		if ref == "new-claude" {
			newSession(ProviderClaude)
			return
		}
		if ref == "new-codex" {
			newSession(ProviderCodex)
			return
		}
		s := nodeSession(node)
		if s == nil {
			// Toggle expand/collapse for group nodes
			node.SetExpanded(!node.IsExpanded())
			// Persist collapse state for folder nodes
			if ref := nodeRefStr(node); strings.HasPrefix(ref, "folder:") {
				folderName := strings.TrimPrefix(ref, "folder:")
				meta.FolderCollapsed[folderName] = !node.IsExpanded()
				saveMetadata(meta)
			}
			return
		}
		var resumeCmd string
		if s.Provider == ProviderCodex {
			resumeCmd = fmt.Sprintf("codex resume %s --sandbox danger-full-access", s.ID)
		} else {
			resumeCmd = fmt.Sprintf("claude --resume %s --dangerously-skip-permissions", s.ID)
		}
		dir := s.ProjectDir
		if _, statErr := os.Stat(dir); statErr != nil {
			if s.CWD != "" {
				dir = s.CWD
			}
			if _, statErr := os.Stat(dir); statErr != nil {
				dir, _ = os.UserHomeDir()
			}
		}
		args := strings.Fields(resumeCmd)
		if len(args) == 0 {
			return
		}
		termWidget.SetOnExit(func() {
			focusSidebar()
			statusBar.SetText("[yellow]세션 종료됨[-]")
			doRefresh := func() {
				fresh := discoverSessions()
				app.QueueUpdateDraw(func() {
					sessions = fresh
					aliases = loadAliases()
					populateTree(currentFilter)
				})
			}
			go func() {
				time.Sleep(2 * time.Second)
				doRefresh()
			}()
		})
		if err := termWidget.StartCommand(args[0], dir, args[1:]...); err != nil {
			statusBar.SetText(fmt.Sprintf("[red]실패: %v[-]", err))
		} else {
			statusBar.SetText(fmt.Sprintf("[green]%s 열림 (내장 터미널)[-]", esc(s.ProjectName)))
			focusTerminal()
		}
	}

	requestSummary := func() {
		cur := tree.GetCurrentNode()
		s := nodeSession(cur)
		if s == nil {
			return
		}
		if _, ok := summaryCache[s.ID]; ok {
			showSessionInfo(s)
			return
		}
		sessionID := s.ID
		statusBar.SetText(fmt.Sprintf("[yellow]%s 요약 생성 중...[-]", esc(s.ProjectName)))
		infoView.SetText(infoView.GetText(false) + "\n[yellow]AI 요약 생성 중...[-]")
		go func() {
			summary, err := generateSummary(s)
			app.QueueUpdateDraw(func() {
				if err != nil {
					statusBar.SetText(fmt.Sprintf("[red]요약 실패: %v[-]", err))
					return
				}
				summaryCache[sessionID] = summary
				curNode := tree.GetCurrentNode()
				if cs := nodeSession(curNode); cs != nil && cs.ID == sessionID {
					showSessionInfo(cs)
				}
				statusBar.SetText(fmt.Sprintf("[green]요약 완료: %s[-]", esc(s.ProjectName)))
			})
		}()
	}

	tree.SetSelectedFunc(func(node *tview.TreeNode) {
		openSessionFromNode(node, true)
	})

	// ── Focus & search ──

	focusables := []tview.Primitive{tree, convView}
	focusIdx := 0

	updateBorders := func() {
		if focusIdx == 0 {
			tree.SetBorderColor(tcell.ColorGreen)
			convView.SetBorderColor(tcell.ColorDodgerBlue)
		} else {
			tree.SetBorderColor(tcell.ColorDodgerBlue)
			convView.SetBorderColor(tcell.ColorGreen)
		}
	}

	rebuildLeftPane = func(withSearch bool) {
		leftPane.Clear()
		if withSearch {
			leftPane.AddItem(searchInput, 1, 0, false)
			leftPane.AddItem(tree, 0, 2, false)
		} else {
			leftPane.AddItem(tree, 0, 2, true)
		}
		if previewOpen {
			leftPane.AddItem(previewPane, 0, 1, false)
		}
	}

	showSearch := func() {
		searching = true
		rebuildLeftPane(true)
		app.SetFocus(searchInput)
	}

	hideSearch := func() {
		searching = false
		rebuildLeftPane(false)
		searchInput.SetText("")
		currentFilter = ""
		populateTree("")
		focusIdx = 0
		app.SetFocus(tree)
		updateBorders()
	}

	searchInput.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEscape {
			hideSearch()
		} else if key == tcell.KeyEnter {
			focusIdx = 0
			app.SetFocus(tree)
			updateBorders()
		}
	})

	searchInput.SetChangedFunc(func(text string) {
		currentFilter = text
		populateTree(text)
	})

	// ── Key bindings ──

	app.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		// Ctrl+] — toggle focus between sidebar and terminal
		if ev.Key() == tcell.KeyCtrlRightSq {
			if termFocused {
				focusSidebar()
			} else if termWidget.IsRunning() {
				focusTerminal()
			}
			return nil
		}

		// When terminal focused, forward everything except Ctrl+]
		if termFocused {
			return ev
		}

		// Skip key mapping when in input field or modal
		focused := app.GetFocus()
		if _, ok := focused.(*tview.InputField); ok {
			return ev
		}
		if _, ok := focused.(*tview.Button); ok {
			return ev
		}

		switch ev.Key() {
		case tcell.KeyTab:
			if previewOpen {
				focusIdx = (focusIdx + 1) % len(focusables)
				app.SetFocus(focusables[focusIdx])
				updateBorders()
			}
			return nil
		case tcell.KeyEscape:
			if searching {
				hideSearch()
				return nil
			}
			if previewOpen {
				togglePreview()
				return nil
			}
			app.Stop()
			return nil
		case tcell.KeyRune:
			if focusIdx == 0 {
				switch toEngKey(ev.Rune()) {
				case '/':
					showSearch()
					return nil

				case '?': // Help
					col1 := tview.NewTextView().SetDynamicColors(true).SetWordWrap(false)
					col1.SetText(
						"[white]Enter[-] 세션 열기\n" +
							"[white]Space[-] 다중 선택\n" +
							"[white]p[-] 미리보기 토글\n" +
							"[white]m[-] 이름 변경\n" +
							"[white]d[-] 삭제\n" +
							"[white]D[-] 일괄 삭제\n" +
							"[white]k[-] 세션 종료\n" +
							"[white]/[-] 검색\n" +
							"[white]?[-] 도움말")
					col1.SetBackgroundColor(tcell.NewRGBColor(30, 30, 30))

					col2 := tview.NewTextView().SetDynamicColors(true).SetWordWrap(false)
					col2.SetText(
						"[white]n[-] 새 폴더\n" +
							"[white]v[-] 폴더로 이동\n" +
							"[white]V[-] 일괄 폴더 이동\n" +
							"[white]g[-] 태그 관리\n" +
							"[white]G[-] 태그로 필터\n" +
							"[white]C[-] 폴더 색상\n" +
							"[white]</>[-] 폴더 순서\n" +
							"[white]t[-] 고정\n" +
							"[white]s[-] 정렬 변경")
					col2.SetBackgroundColor(tcell.NewRGBColor(30, 30, 30))

					col3 := tview.NewTextView().SetDynamicColors(true).SetWordWrap(false)
					col3.SetText(
						"[white]e[-] 내보내기\n" +
							"[white]E[-] 일괄 내보내기\n" +
							"[white]c[-] 컴팩트 모드\n" +
							"[white]o[-] 폴더 열기\n" +
							"[white]x[-] 휴지통\n" +
							"[white]r[-] 새로고침\n" +
							"[white]u[-] 업데이트\n" +
							"[white]T[-] 임시 세션\n" +
							"[white]P[-] 임시→일반\n" +
							"[white]i[-] AI 요약\n" +
							"[white]Esc[-] 종료")
					col3.SetBackgroundColor(tcell.NewRGBColor(30, 30, 30))

					helpRow := tview.NewFlex().SetDirection(tview.FlexColumn).
						AddItem(col1, 0, 1, false).
						AddItem(col2, 0, 1, false).
						AddItem(col3, 0, 1, false)
					helpRow.SetBackgroundColor(tcell.NewRGBColor(30, 30, 30))

					helpContainer := tview.NewFlex().SetDirection(tview.FlexRow).
						AddItem(helpRow, 0, 1, false)
					helpContainer.SetBorder(true).
						SetBorderColor(tcell.ColorDimGray).
						SetBackgroundColor(tcell.NewRGBColor(30, 30, 30))

					helpWrapper := tview.NewFlex().SetDirection(tview.FlexRow).
						AddItem(mainLayout, 0, 1, false).
						AddItem(helpContainer, 13, 0, false)

					helpWrapper.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
						app.SetRoot(mainLayout, true)
						app.SetFocus(tree)
						return nil
					})
					app.SetRoot(helpWrapper, true)
					app.SetFocus(helpWrapper)
					return nil

				case 'i':
					requestSummary()
					return nil

				case 'p': // Toggle preview panel
					togglePreview()
					return nil

				case 't': // Toggle pin
					cur := tree.GetCurrentNode()
					s := nodeSession(cur)
					if s == nil {
						return nil
					}
					// Also sync with Codex desktop
					codexPins := loadCodexPins()
					if s.Pinned {
						s.Pinned = false
						delete(localPins, s.ID)
						delete(codexPins, s.ID)
						localUnpins[s.ID] = true
						statusBar.SetText(fmt.Sprintf("[yellow]고정 해제: %s[-]", esc(s.ProjectName)))
					} else {
						s.Pinned = true
						localPins[s.ID] = true
						delete(localUnpins, s.ID)
						if s.Provider == ProviderCodex {
							codexPins[s.ID] = true
						}
						statusBar.SetText(fmt.Sprintf("[green]📌 고정: %s[-]", esc(s.ProjectName)))
					}
					savePins(localPins)
					saveUnpins(localUnpins)
					saveCodexPins(codexPins)
					sessions = discoverSessions()
					sortSessions()
					populateTree(currentFilter)
					return nil

				case 's': // Sort toggle
					sortMode = (sortMode + 1) % 3
					sortSessions()
					populateTree(currentFilter)
					statusBar.SetText(fmt.Sprintf("[green]정렬: %s[-]", sortLabels[sortMode]))
					return nil

				case 'r': // Refresh
					go func() {
						app.QueueUpdateDraw(func() {
							statusBar.SetText("[yellow]새로고침 중...[-]")
						})
						fresh := discoverSessions()
						app.QueueUpdateDraw(func() {
							sessions = fresh
							aliases = loadAliases()
							populateTree(currentFilter)
						})
					}()
					return nil

				case ' ': // Toggle multi-select
					cur := tree.GetCurrentNode()
					s := nodeSession(cur)
					if s == nil {
						return nil
					}
					s.Selected = !s.Selected
					if compactMode {
						cur.SetText(sessionNodeTextCompact(s, currentFilter))
					} else {
						cur.SetText(sessionNodeText(s, currentFilter))
					}
					statusBar.SetText(defaultStatus())
					return nil

				case 'm': // Rename (alias) — session, folder, or project group
					cur := tree.GetCurrentNode()

					// Check if it's a folder node
					refStr := nodeRefStr(cur)
					if strings.HasPrefix(refStr, "folder:") {
						folderName := strings.TrimPrefix(refStr, "folder:")
						renameInput := tview.NewInputField().
							SetLabel(" 새 이름: ").
							SetText(folderName).
							SetFieldWidth(40).
							SetFieldBackgroundColor(tcell.ColorDarkSlateGray)
						renameInput.SetBorder(true).
							SetTitle(" 폴더 이름 변경 ").
							SetTitleAlign(tview.AlignCenter)
						renameInput.SetDoneFunc(func(key tcell.Key) {
							if key == tcell.KeyEnter {
								newName := strings.TrimSpace(renameInput.GetText())
								if newName != "" && newName != folderName {
									// Update folder list
									for i, f := range meta.Folders {
										if f == folderName {
											meta.Folders[i] = newName
											break
										}
									}
									// Update session assignments
									for sid, f := range meta.SessionFolders {
										if f == folderName {
											meta.SessionFolders[sid] = newName
										}
									}
									saveMetadata(meta)
									populateTree(currentFilter)
									statusBar.SetText(fmt.Sprintf("[green]폴더 이름 변경: %s → %s[-]", esc(folderName), esc(newName)))
								}
							}
							app.SetRoot(mainLayout, true)
							app.SetFocus(tree)
						})
						modal := tview.NewFlex().SetDirection(tview.FlexRow).
							AddItem(nil, 0, 1, false).
							AddItem(tview.NewFlex().
								AddItem(nil, 0, 1, false).
								AddItem(renameInput, 50, 0, true).
								AddItem(nil, 0, 1, false), 3, 0, true).
							AddItem(nil, 0, 1, false)
						pages := tview.NewPages().
							AddPage("main", mainLayout, true, true).
							AddPage("rename", modal, true, true)
						app.SetRoot(pages, true)
						app.SetFocus(renameInput)
						return nil
					}

					// Check if it's a project group node
					if strings.HasPrefix(refStr, "proj:") {
						projName := strings.TrimPrefix(refStr, "proj:")
						currentAlias := projectAliases[projName]
						renameInput := tview.NewInputField().
							SetLabel(" 새 이름: ").
							SetText(currentAlias).
							SetFieldWidth(40).
							SetFieldBackgroundColor(tcell.ColorDarkSlateGray)
						renameInput.SetBorder(true).
							SetTitle(" 프로젝트 이름 변경 ").
							SetTitleAlign(tview.AlignCenter)
						renameInput.SetDoneFunc(func(key tcell.Key) {
							if key == tcell.KeyEnter {
								newAlias := strings.TrimSpace(renameInput.GetText())
								if newAlias == "" {
									delete(projectAliases, projName)
								} else {
									projectAliases[projName] = newAlias
								}
								saveProjectAliases(projectAliases)
								populateTree(currentFilter)
								if newAlias != "" {
									statusBar.SetText(fmt.Sprintf("[green]프로젝트 이름 변경: %s → %s[-]", esc(projName), esc(newAlias)))
								} else {
									statusBar.SetText("[green]프로젝트 별칭 삭제됨[-]")
								}
							}
							app.SetRoot(mainLayout, true)
							app.SetFocus(tree)
						})
						modal := tview.NewFlex().SetDirection(tview.FlexRow).
							AddItem(nil, 0, 1, false).
							AddItem(tview.NewFlex().
								AddItem(nil, 0, 1, false).
								AddItem(renameInput, 50, 0, true).
								AddItem(nil, 0, 1, false), 3, 0, true).
							AddItem(nil, 0, 1, false)
						pages := tview.NewPages().
							AddPage("main", mainLayout, true, true).
							AddPage("rename", modal, true, true)
						app.SetRoot(pages, true)
						app.SetFocus(renameInput)
						return nil
					}

					s := nodeSession(cur)
					if s == nil {
						return nil
					}
					renameInput := tview.NewInputField().
						SetLabel(" 새 이름: ").
						SetText(s.Alias).
						SetFieldWidth(40).
						SetFieldBackgroundColor(tcell.ColorDarkSlateGray)
					renameInput.SetBorder(true).
						SetTitle(" 세션 이름 변경 ").
						SetTitleAlign(tview.AlignCenter)
					renameInput.SetDoneFunc(func(key tcell.Key) {
						if key == tcell.KeyEnter {
							newAlias := strings.TrimSpace(renameInput.GetText())
							s.Alias = newAlias
							if newAlias == "" {
								delete(aliases, s.ID)
							} else {
								aliases[s.ID] = newAlias
							}
							saveAliases(aliases)
							sessions = discoverSessions()
							populateTree(currentFilter)
							if newAlias != "" {
								statusBar.SetText(fmt.Sprintf("[green]이름 변경: %s[-]", esc(newAlias)))
							} else {
								statusBar.SetText("[green]별칭 삭제됨[-]")
							}
						}
						app.SetRoot(mainLayout, true)
						app.SetFocus(tree)
					})
					modal := tview.NewFlex().SetDirection(tview.FlexRow).
						AddItem(nil, 0, 1, false).
						AddItem(tview.NewFlex().
							AddItem(nil, 0, 1, false).
							AddItem(renameInput, 50, 0, true).
							AddItem(nil, 0, 1, false), 3, 0, true).
						AddItem(nil, 0, 1, false)
					pages := tview.NewPages().
						AddPage("main", mainLayout, true, true).
						AddPage("rename", modal, true, true)
					app.SetRoot(pages, true)
					app.SetFocus(renameInput)
					return nil

				case 'd': // Delete session or group
					cur := tree.GetCurrentNode()

					// Check if it's a folder node
					refStr := nodeRefStr(cur)
					if strings.HasPrefix(refStr, "folder:") {
						folderName := strings.TrimPrefix(refStr, "folder:")
						// Remove folder and unassign all sessions
						var newFolders []string
						for _, f := range meta.Folders {
							if f != folderName {
								newFolders = append(newFolders, f)
							}
						}
						meta.Folders = newFolders
						for sid, f := range meta.SessionFolders {
							if f == folderName {
								delete(meta.SessionFolders, sid)
							}
						}
						saveMetadata(meta)
						populateTree(currentFilter)
						statusBar.SetText(fmt.Sprintf("[green]폴더 삭제: %s (세션은 유지됨)[-]", esc(folderName)))
						return nil
					}

					// Check if any sessions are selected — batch delete
					var selectedSessions []*Session
					for _, ss := range sessions {
						if ss.Selected {
							selectedSessions = append(selectedSessions, ss)
						}
					}
					if len(selectedSessions) > 0 {
						confirmModal := tview.NewModal().
							SetText(fmt.Sprintf("%d개 선택된 세션을 삭제하시겠습니까?", len(selectedSessions))).
							AddButtons([]string{"삭제", "취소"}).
							SetDoneFunc(func(_ int, label string) {
								if label == "삭제" {
									deleted := 0
									for _, s := range selectedSessions {
										if deleteSession(s) == nil {
											delete(aliases, s.ID)
											deleted++
										}
									}
									saveAliases(aliases)
									sessions = discoverSessions()
									sortSessions()
									populateTree(currentFilter)
									statusBar.SetText(fmt.Sprintf("[green]%d개 세션 삭제됨[-]", deleted))
								}
								app.SetRoot(mainLayout, true)
								app.SetFocus(tree)
							})
						confirmModal.SetBackgroundColor(tcell.ColorDarkSlateGray)
						confirmModal.SetButtonBackgroundColor(tcell.NewRGBColor(40, 40, 40))
						confirmModal.SetButtonTextColor(tcell.ColorGray)
						confirmModal.SetButtonActivatedStyle(tcell.StyleDefault.Background(tcell.ColorRed).Foreground(tcell.ColorWhite).Bold(true))
						confirmModal.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
							if ev.Key() == tcell.KeyRune && toEngKey(ev.Rune()) == 'd' {
								deleted := 0
								for _, s := range selectedSessions {
									if deleteSession(s) == nil {
										delete(aliases, s.ID)
										deleted++
									}
								}
								saveAliases(aliases)
								sessions = discoverSessions()
								sortSessions()
								populateTree(currentFilter)
								statusBar.SetText(fmt.Sprintf("[green]%d개 세션 삭제됨[-]", deleted))
								app.SetRoot(mainLayout, true)
								app.SetFocus(tree)
								return nil
							}
							return ev
						})
						app.SetRoot(confirmModal, true)
						return nil
					}

					s := nodeSession(cur)

					// Check if it's a group node with children sessions
					if s == nil && cur != nil {
						children := cur.GetChildren()
						var groupSessions []*Session
						for _, child := range children {
							if cs := nodeSession(child); cs != nil {
								groupSessions = append(groupSessions, cs)
							}
						}
						if len(groupSessions) == 0 {
							return nil
						}
						groupName := cur.GetText()
						confirmModal := tview.NewModal().
							SetText(fmt.Sprintf("%s\n\n%d개 세션을 삭제하시겠습니까?", groupName, len(groupSessions))).
							AddButtons([]string{"삭제", "취소"}).
							SetDoneFunc(func(_ int, label string) {
								if label == "삭제" {
									deletedIDs := map[string]bool{}
									for _, gs := range groupSessions {
										if deleteSession(gs) == nil {
											deletedIDs[gs.ID] = true
											delete(aliases, gs.ID)
										}
									}
									if len(deletedIDs) > 0 {
										filtered := sessions[:0]
										for _, ss := range sessions {
											if !deletedIDs[ss.ID] {
												filtered = append(filtered, ss)
											}
										}
										sessions = filtered
									}
									saveAliases(aliases)
									sortSessions()
									populateTree(currentFilter)
									statusBar.SetText(fmt.Sprintf("[green]%d개 세션 삭제됨[-]", len(deletedIDs)))
								}
								app.SetRoot(mainLayout, true)
								app.SetFocus(tree)
							})
						confirmModal.SetBackgroundColor(tcell.ColorDarkSlateGray)
						confirmModal.SetButtonBackgroundColor(tcell.NewRGBColor(40, 40, 40))
						confirmModal.SetButtonTextColor(tcell.ColorGray)
						confirmModal.SetButtonActivatedStyle(tcell.StyleDefault.Background(tcell.ColorRed).Foreground(tcell.ColorWhite).Bold(true))
						confirmModal.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
							if ev.Key() == tcell.KeyRune && toEngKey(ev.Rune()) == 'd' {
								deletedIDs := map[string]bool{}
								for _, gs := range groupSessions {
									if deleteSession(gs) == nil {
										deletedIDs[gs.ID] = true
										delete(aliases, gs.ID)
									}
								}
								if len(deletedIDs) > 0 {
									filtered := sessions[:0]
									for _, ss := range sessions {
										if !deletedIDs[ss.ID] {
											filtered = append(filtered, ss)
										}
									}
									sessions = filtered
								}
								saveAliases(aliases)
								sortSessions()
								populateTree(currentFilter)
								statusBar.SetText(fmt.Sprintf("[green]%d개 세션 삭제됨[-]", len(deletedIDs)))
								app.SetRoot(mainLayout, true)
								app.SetFocus(tree)
								return nil
							}
							return ev
						})
						app.SetRoot(confirmModal, true)
						return nil
					}

					if s == nil {
						return nil
					}
					displayName := s.ProjectName
					if s.Alias != "" {
						displayName = s.Alias
					}
					confirmModal := tview.NewModal().
						SetText(fmt.Sprintf("세션을 삭제하시겠습니까?\n\n%s\n%s", displayName, s.ID)).
						AddButtons([]string{"삭제", "취소"}).
						SetDoneFunc(func(_ int, label string) {
							if label == "삭제" {
								if err := deleteSession(s); err != nil {
									statusBar.SetText(fmt.Sprintf("[red]삭제 실패: %v[-]", err))
								} else {
									for i, ss := range sessions {
										if ss.ID == s.ID {
											sessions = append(sessions[:i], sessions[i+1:]...)
											break
										}
									}
									delete(aliases, s.ID)
									saveAliases(aliases)
									sortSessions()
									populateTree(currentFilter)
									statusBar.SetText(fmt.Sprintf("[green]삭제됨: %s[-]", esc(displayName)))
								}
							}
							app.SetRoot(mainLayout, true)
							app.SetFocus(tree)
						})
					confirmModal.SetBackgroundColor(tcell.ColorDarkSlateGray)
					confirmModal.SetButtonBackgroundColor(tcell.NewRGBColor(40, 40, 40))
					confirmModal.SetButtonTextColor(tcell.ColorGray)
					confirmModal.SetButtonActivatedStyle(tcell.StyleDefault.Background(tcell.ColorRed).Foreground(tcell.ColorWhite).Bold(true))
					// Allow 'd' key to confirm delete in modal
					confirmModal.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
						if ev.Key() == tcell.KeyRune && toEngKey(ev.Rune()) == 'd' {
							if err := deleteSession(s); err != nil {
								statusBar.SetText(fmt.Sprintf("[red]삭제 실패: %v[-]", err))
							} else {
								for i, ss := range sessions {
									if ss.ID == s.ID {
										sessions = append(sessions[:i], sessions[i+1:]...)
										break
									}
								}
								delete(aliases, s.ID)
								saveAliases(aliases)
								sortSessions()
								populateTree(currentFilter)
								statusBar.SetText(fmt.Sprintf("[green]삭제됨: %s[-]", esc(displayName)))
							}
							app.SetRoot(mainLayout, true)
							app.SetFocus(tree)
							return nil
						}
						return ev
					})
					app.SetRoot(confirmModal, true)
					return nil

				case 'o': // Open session folder in Finder
					cur := tree.GetCurrentNode()
					s := nodeSession(cur)
					if s == nil {
						return nil
					}
					dir := filepath.Dir(s.SessionFile)
					if err := exec.Command("open", dir).Run(); err != nil {
						statusBar.SetText(fmt.Sprintf("[red]폴더 열기 실패: %v[-]", err))
					} else {
						statusBar.SetText(fmt.Sprintf("[green]폴더 열림: %s[-]", esc(dir)))
					}
					return nil

				case 'T': // Temp session
					home, _ := os.UserHomeDir()
					sessionID := generateUUID()
					cmd := fmt.Sprintf("claude --session-id %s --dangerously-skip-permissions", sessionID)
					meta.TempSessions[sessionID] = true
					saveMetadata(meta)
					spinFrames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
					stopSpin := make(chan struct{})
					go func() {
						i := 0
						tick := time.NewTicker(80 * time.Millisecond)
						defer tick.Stop()
						for {
							select {
							case <-stopSpin:
								return
							case <-tick.C:
								frame := spinFrames[i%len(spinFrames)]
								app.QueueUpdateDraw(func() {
									statusBar.SetText(fmt.Sprintf("[yellow]%s 임시 세션 여는 중...[-]", frame))
								})
								i++
							}
						}
					}()
					err := openInTerminal(cmd, home, true, app)
					close(stopSpin)
					if err != nil {
						delete(meta.TempSessions, sessionID)
						saveMetadata(meta)
						statusBar.SetText(fmt.Sprintf("[red]실패: %v[-]", err))
					} else {
						statusBar.SetText("[green]임시 세션 열림 (종료 시 자동 삭제)[-]")
						go func() {
							time.Sleep(2 * time.Second)
							fresh := discoverSessions()
							app.QueueUpdateDraw(func() {
								sessions = fresh
								aliases = loadAliases()
								sortSessions()
								populateTree(currentFilter)
							})
						}()
					}
					return nil

				case 'P': // Persist temp session (convert to normal)
					s := nodeSession(tree.GetCurrentNode())
					if s == nil {
						return nil
					}
					if !meta.TempSessions[s.ID] {
						statusBar.SetText("[yellow]임시 세션이 아닙니다[-]")
						return nil
					}
					delete(meta.TempSessions, s.ID)
					saveMetadata(meta)
					populateTree(currentFilter)
					statusBar.SetText("[green]일반 세션으로 변환됨[-]")
					return nil

				case 'n': // New folder
					folderInput := tview.NewInputField().
						SetLabel(" 폴더 이름: ").
						SetFieldWidth(40).
						SetFieldBackgroundColor(tcell.ColorDarkSlateGray)
					folderInput.SetBorder(true).
						SetTitle(" 새 폴더 ").
						SetTitleAlign(tview.AlignCenter)
					folderInput.SetDoneFunc(func(key tcell.Key) {
						if key == tcell.KeyEnter {
							name := strings.TrimSpace(folderInput.GetText())
							if name != "" {
								// Check duplicate
								exists := false
								for _, f := range meta.Folders {
									if f == name {
										exists = true
										break
									}
								}
								if !exists {
									meta.Folders = append(meta.Folders, name)
									saveMetadata(meta)
									populateTree(currentFilter)
									statusBar.SetText(fmt.Sprintf("[green]폴더 생성: %s[-]", esc(name)))
								} else {
									statusBar.SetText(fmt.Sprintf("[red]이미 존재하는 폴더: %s[-]", esc(name)))
								}
							}
						}
						app.SetRoot(mainLayout, true)
						app.SetFocus(tree)
					})
					modal := tview.NewFlex().SetDirection(tview.FlexRow).
						AddItem(nil, 0, 1, false).
						AddItem(tview.NewFlex().
							AddItem(nil, 0, 1, false).
							AddItem(folderInput, 50, 0, true).
							AddItem(nil, 0, 1, false), 3, 0, true).
						AddItem(nil, 0, 1, false)
					pages := tview.NewPages().
						AddPage("main", mainLayout, true, true).
						AddPage("folder", modal, true, true)
					app.SetRoot(pages, true)
					app.SetFocus(folderInput)
					return nil

				case 'v': // Move session(s) to folder
					// Check if any sessions are selected for batch move
					var selectedSessions []*Session
					for _, ss := range sessions {
						if ss.Selected {
							selectedSessions = append(selectedSessions, ss)
						}
					}
					batchMode := len(selectedSessions) > 0
					var singleSession *Session
					if !batchMode {
						singleSession = nodeSession(tree.GetCurrentNode())
						if singleSession == nil {
							return nil
						}
					}
					if len(meta.Folders) == 0 {
						statusBar.SetText("[red]폴더가 없습니다. n키로 먼저 생성하세요[-]")
						return nil
					}
					list := tview.NewList()
					moveTitle := " 폴더로 이동 "
					if batchMode {
						moveTitle = fmt.Sprintf(" %d개 세션 폴더로 이동 ", len(selectedSessions))
					}
					list.SetBorder(true).
						SetTitle(moveTitle).
						SetTitleAlign(tview.AlignCenter).
						SetBorderColor(tcell.ColorDodgerBlue).
						SetBackgroundColor(tcell.ColorDarkSlateGray)
					list.SetSelectedBackgroundColor(tcell.ColorDodgerBlue)
					// "Remove from folder" option
					list.AddItem("폴더 해제", "프로젝트 기본 그룹으로", 0, func() {
						if batchMode {
							for _, ss := range selectedSessions {
								delete(meta.SessionFolders, ss.ID)
								ss.Selected = false
							}
							saveMetadata(meta)
							populateTree(currentFilter)
							statusBar.SetText(fmt.Sprintf("[green]%d개 세션 폴더 해제됨[-]", len(selectedSessions)))
						} else {
							delete(meta.SessionFolders, singleSession.ID)
							saveMetadata(meta)
							populateTree(currentFilter)
							statusBar.SetText("[green]폴더 해제됨[-]")
						}
						app.SetRoot(mainLayout, true)
						app.SetFocus(tree)
					})
					currentFolder := ""
					if !batchMode {
						currentFolder = meta.SessionFolders[singleSession.ID]
					}
					for _, folder := range meta.Folders {
						f := folder // capture
						label := f
						if !batchMode && f == currentFolder {
							label += " (현재)"
						}
						list.AddItem(label, "", 0, func() {
							if batchMode {
								for _, ss := range selectedSessions {
									meta.SessionFolders[ss.ID] = f
									ss.Selected = false
								}
								saveMetadata(meta)
								populateTree(currentFilter)
								statusBar.SetText(fmt.Sprintf("[green]%d개 세션 → %s[-]", len(selectedSessions), esc(f)))
							} else {
								meta.SessionFolders[singleSession.ID] = f
								saveMetadata(meta)
								populateTree(currentFilter)
								statusBar.SetText(fmt.Sprintf("[green]%s → %s[-]", esc(singleSession.ProjectName), esc(f)))
							}
							app.SetRoot(mainLayout, true)
							app.SetFocus(tree)
						})
					}
					list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
						if ev.Key() == tcell.KeyEscape {
							app.SetRoot(mainLayout, true)
							app.SetFocus(tree)
							return nil
						}
						return ev
					})
					listHeight := len(meta.Folders) + 4
					if listHeight > 15 {
						listHeight = 15
					}
					modal := tview.NewFlex().SetDirection(tview.FlexRow).
						AddItem(nil, 0, 1, false).
						AddItem(tview.NewFlex().
							AddItem(nil, 0, 1, false).
							AddItem(list, 40, 0, true).
							AddItem(nil, 0, 1, false), listHeight, 0, true).
						AddItem(nil, 0, 1, false)
					pages := tview.NewPages().
						AddPage("main", mainLayout, true, true).
						AddPage("move", modal, true, true)
					app.SetRoot(pages, true)
					app.SetFocus(list)
					return nil

				case 'V': // Batch folder move (selected only)
					var selectedSessions []*Session
					for _, ss := range sessions {
						if ss.Selected {
							selectedSessions = append(selectedSessions, ss)
						}
					}
					if len(selectedSessions) == 0 {
						statusBar.SetText("[yellow]선택된 세션이 없습니다. Space로 선택하세요[-]")
						return nil
					}
					if len(meta.Folders) == 0 {
						statusBar.SetText("[red]폴더가 없습니다. n키로 먼저 생성하세요[-]")
						return nil
					}
					list := tview.NewList()
					list.SetBorder(true).
						SetTitle(fmt.Sprintf(" %d개 세션 폴더로 이동 ", len(selectedSessions))).
						SetTitleAlign(tview.AlignCenter).
						SetBorderColor(tcell.ColorDodgerBlue).
						SetBackgroundColor(tcell.ColorDarkSlateGray)
					list.SetSelectedBackgroundColor(tcell.ColorDodgerBlue)
					list.AddItem("폴더 해제", "프로젝트 기본 그룹으로", 0, func() {
						for _, ss := range selectedSessions {
							delete(meta.SessionFolders, ss.ID)
							ss.Selected = false
						}
						saveMetadata(meta)
						populateTree(currentFilter)
						statusBar.SetText(fmt.Sprintf("[green]%d개 세션 폴더 해제됨[-]", len(selectedSessions)))
						app.SetRoot(mainLayout, true)
						app.SetFocus(tree)
					})
					for _, folder := range meta.Folders {
						f := folder
						list.AddItem(f, "", 0, func() {
							for _, ss := range selectedSessions {
								meta.SessionFolders[ss.ID] = f
								ss.Selected = false
							}
							saveMetadata(meta)
							populateTree(currentFilter)
							statusBar.SetText(fmt.Sprintf("[green]%d개 세션 → %s[-]", len(selectedSessions), esc(f)))
							app.SetRoot(mainLayout, true)
							app.SetFocus(tree)
						})
					}
					list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
						if ev.Key() == tcell.KeyEscape {
							app.SetRoot(mainLayout, true)
							app.SetFocus(tree)
							return nil
						}
						return ev
					})
					listHeight := len(meta.Folders) + 4
					if listHeight > 15 {
						listHeight = 15
					}
					modal := tview.NewFlex().SetDirection(tview.FlexRow).
						AddItem(nil, 0, 1, false).
						AddItem(tview.NewFlex().
							AddItem(nil, 0, 1, false).
							AddItem(list, 40, 0, true).
							AddItem(nil, 0, 1, false), listHeight, 0, true).
						AddItem(nil, 0, 1, false)
					pages := tview.NewPages().
						AddPage("main", mainLayout, true, true).
						AddPage("move", modal, true, true)
					app.SetRoot(pages, true)
					app.SetFocus(list)
					return nil

				case 'g': // Tag management
					s := nodeSession(tree.GetCurrentNode())
					if s == nil {
						return nil
					}
					currentTags := ""
					if tags, ok := meta.SessionTags[s.ID]; ok {
						currentTags = strings.Join(tags, ", ")
					}
					// Collect all unique existing tags for hint
					allTagsMap := make(map[string]bool)
					for _, tags := range meta.SessionTags {
						for _, t := range tags {
							allTagsMap[t] = true
						}
					}
					var allTagsList []string
					for t := range allTagsMap {
						allTagsList = append(allTagsList, t)
					}
					sort.Strings(allTagsList)
					tagTitle := " 태그 관리 "
					if len(allTagsList) > 0 {
						hint := strings.Join(allTagsList, ", ")
						if len(hint) > 50 {
							hint = hint[:47] + "..."
						}
						tagTitle = fmt.Sprintf(" 태그 관리 (기존: %s) ", hint)
					}
					tagInput := tview.NewInputField().
						SetLabel(" 태그 (쉼표 구분): ").
						SetText(currentTags).
						SetFieldWidth(40).
						SetFieldBackgroundColor(tcell.ColorDarkSlateGray)
					tagInput.SetBorder(true).
						SetTitle(tagTitle).
						SetTitleAlign(tview.AlignCenter)
					tagInput.SetDoneFunc(func(key tcell.Key) {
						if key == tcell.KeyEnter {
							text := strings.TrimSpace(tagInput.GetText())
							if text == "" {
								delete(meta.SessionTags, s.ID)
							} else {
								parts := strings.Split(text, ",")
								var tags []string
								for _, p := range parts {
									t := strings.TrimSpace(p)
									t = strings.TrimPrefix(t, "#")
									if t != "" {
										tags = append(tags, t)
									}
								}
								if len(tags) > 0 {
									meta.SessionTags[s.ID] = tags
								} else {
									delete(meta.SessionTags, s.ID)
								}
							}
							saveMetadata(meta)
							populateTree(currentFilter)
							statusBar.SetText("[green]태그 업데이트됨[-]")
						}
						app.SetRoot(mainLayout, true)
						app.SetFocus(tree)
					})
					modal := tview.NewFlex().SetDirection(tview.FlexRow).
						AddItem(nil, 0, 1, false).
						AddItem(tview.NewFlex().
							AddItem(nil, 0, 1, false).
							AddItem(tagInput, 60, 0, true).
							AddItem(nil, 0, 1, false), 3, 0, true).
						AddItem(nil, 0, 1, false)
					pages := tview.NewPages().
						AddPage("main", mainLayout, true, true).
						AddPage("tag", modal, true, true)
					app.SetRoot(pages, true)
					app.SetFocus(tagInput)
					return nil

				case 'x': // Trash view
					trashItems := listTrash()
					if len(trashItems) == 0 {
						statusBar.SetText("[yellow]휴지통이 비어있습니다[-]")
						return nil
					}
					trashList := tview.NewList().
						ShowSecondaryText(false).
						SetHighlightFullLine(true).
						SetSelectedStyle(tcell.StyleDefault.Background(tcell.ColorDarkSlateGray).Foreground(tcell.ColorWhite))
					for _, item := range trashItems {
						label := fmt.Sprintf("  [#999999]%s[-]  [white]%s[-]", item["deletedAt"][:10], filepath.Base(item["trashFile"]))
						trashList.AddItem(label, "", 0, nil)
					}
					trashList.SetSelectedFunc(func(idx int, _, _ string, _ rune) {
						if idx >= len(trashItems) {
							return
						}
						item := trashItems[idx]
						actionModal := tview.NewModal().
							SetText(fmt.Sprintf("세션: %s", filepath.Base(item["trashFile"]))).
							AddButtons([]string{"복원", "영구 삭제", "취소"}).
							SetDoneFunc(func(_ int, label string) {
								switch label {
								case "복원":
									if err := restoreFromTrash(item); err != nil {
										statusBar.SetText(fmt.Sprintf("[red]복원 실패: %v[-]", err))
									} else {
										sessions = discoverSessions()
										sortSessions()
										populateTree(currentFilter)
										statusBar.SetText("[green]세션 복원됨[-]")
									}
								case "영구 삭제":
									permanentDeleteTrash(item)
									statusBar.SetText("[green]영구 삭제됨[-]")
								}
								app.SetRoot(mainLayout, true)
								app.SetFocus(tree)
							})
						actionModal.SetBackgroundColor(tcell.ColorDarkSlateGray)
						actionModal.SetButtonBackgroundColor(tcell.NewRGBColor(40, 40, 40))
						actionModal.SetButtonTextColor(tcell.ColorGray)
						actionModal.SetButtonActivatedStyle(tcell.StyleDefault.Background(tcell.ColorDodgerBlue).Foreground(tcell.ColorWhite).Bold(true))
						app.SetRoot(actionModal, true)
					})
					trashList.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
						if ev.Key() == tcell.KeyEscape {
							app.SetRoot(mainLayout, true)
							app.SetFocus(tree)
							return nil
						}
						return ev
					})
					trashList.SetBorder(true).
						SetTitle(fmt.Sprintf(" 휴지통 (%d) ", len(trashItems))).
						SetTitleAlign(tview.AlignCenter).
						SetBorderColor(tcell.ColorRed)
					trashFlex := tview.NewFlex().SetDirection(tview.FlexRow).
						AddItem(nil, 0, 1, false).
						AddItem(tview.NewFlex().
							AddItem(nil, 0, 1, false).
							AddItem(trashList, 60, 0, true).
							AddItem(nil, 0, 1, false), len(trashItems)+2, 0, true).
						AddItem(nil, 0, 1, false)
					pages := tview.NewPages().
						AddPage("main", mainLayout, true, true).
						AddPage("trash", trashFlex, true, true)
					app.SetRoot(pages, true)
					app.SetFocus(trashList)
					return nil

				case 'e': // Export session to markdown
					cur := tree.GetCurrentNode()
					s := nodeSession(cur)
					if s == nil {
						return nil
					}
					home, _ := os.UserHomeDir()
					title := s.Alias
					if title == "" {
						title = trunc(s.FirstUserMsg, 30)
					}
					if title == "" {
						title = s.ID[:8]
					}
					// Sanitize filename
					safeName := strings.Map(func(r rune) rune {
						if r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
							return '_'
						}
						return r
					}, title)
					exportPath := filepath.Join(home, "Desktop", safeName+".md")

					var md strings.Builder
					md.WriteString(fmt.Sprintf("# %s\n\n", title))
					md.WriteString(fmt.Sprintf("- **세션 ID**: %s\n", s.ID))
					md.WriteString(fmt.Sprintf("- **프로젝트**: %s\n", s.ProjectName))
					md.WriteString(fmt.Sprintf("- **날짜**: %s\n", s.ModTime.Format("2006-01-02 15:04:05")))
					md.WriteString(fmt.Sprintf("- **메시지**: %d개\n\n", s.MessageCount))
					md.WriteString("---\n\n")
					for _, msg := range s.Messages {
						if msg.Type == "user" {
							md.WriteString("## 👤 사용자\n\n")
						} else {
							md.WriteString("## 🤖 어시스턴트\n\n")
						}
						md.WriteString(msg.Content + "\n\n")
					}
					if err := os.WriteFile(exportPath, []byte(md.String()), 0644); err != nil {
						statusBar.SetText(fmt.Sprintf("[red]내보내기 실패: %v[-]", err))
					} else {
						statusBar.SetText(fmt.Sprintf("[green]내보내기 완료: %s[-]", esc(exportPath)))
					}
					return nil

				case 'D': // Batch delete selected sessions
					var selected []*Session
					for _, s := range sessions {
						if s.Selected {
							selected = append(selected, s)
						}
					}
					if len(selected) == 0 {
						statusBar.SetText("[yellow]선택된 세션이 없습니다 (Space로 선택)[-]")
						return nil
					}
					confirmModal := tview.NewModal().
						SetText(fmt.Sprintf("%d개 선택된 세션을 삭제하시겠습니까?", len(selected))).
						AddButtons([]string{"삭제", "취소"}).
						SetDoneFunc(func(_ int, label string) {
							if label == "삭제" {
								deleted := 0
								for _, s := range selected {
									if deleteSession(s) == nil {
										delete(aliases, s.ID)
										deleted++
									}
								}
								saveAliases(aliases)
								sessions = discoverSessions()
								sortSessions()
								populateTree(currentFilter)
								statusBar.SetText(fmt.Sprintf("[green]%d개 세션 삭제됨[-]", deleted))
							}
							app.SetRoot(mainLayout, true)
							app.SetFocus(tree)
						})
					confirmModal.SetBackgroundColor(tcell.ColorDarkSlateGray)
					confirmModal.SetButtonBackgroundColor(tcell.NewRGBColor(40, 40, 40))
					confirmModal.SetButtonTextColor(tcell.ColorGray)
					confirmModal.SetButtonActivatedStyle(tcell.StyleDefault.Background(tcell.ColorRed).Foreground(tcell.ColorWhite).Bold(true))
					confirmModal.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
						if ev.Key() == tcell.KeyRune && toEngKey(ev.Rune()) == 'd' {
							deleted := 0
							for _, s := range selected {
								if deleteSession(s) == nil {
									delete(aliases, s.ID)
									deleted++
								}
							}
							saveAliases(aliases)
							sessions = discoverSessions()
							sortSessions()
							populateTree(currentFilter)
							statusBar.SetText(fmt.Sprintf("[green]%d개 세션 삭제됨[-]", deleted))
							app.SetRoot(mainLayout, true)
							app.SetFocus(tree)
							return nil
						}
						return ev
					})
					app.SetRoot(confirmModal, true)
					return nil

				case 'E': // Batch export selected sessions
					var selected []*Session
					for _, s := range sessions {
						if s.Selected {
							selected = append(selected, s)
						}
					}
					if len(selected) == 0 {
						statusBar.SetText("[yellow]선택된 세션이 없습니다 (Space로 선택)[-]")
						return nil
					}
					home, _ := os.UserHomeDir()
					exported := 0
					for _, s := range selected {
						title := s.Alias
						if title == "" {
							title = trunc(s.FirstUserMsg, 30)
						}
						if title == "" {
							title = s.ID[:8]
						}
						safeName := strings.Map(func(r rune) rune {
							if r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
								return '_'
							}
							return r
						}, title)
						exportPath := filepath.Join(home, "Desktop", safeName+".md")

						var md strings.Builder
						md.WriteString(fmt.Sprintf("# %s\n\n", title))
						md.WriteString(fmt.Sprintf("- **세션 ID**: %s\n", s.ID))
						md.WriteString(fmt.Sprintf("- **프로젝트**: %s\n", s.ProjectName))
						md.WriteString(fmt.Sprintf("- **날짜**: %s\n", s.ModTime.Format("2006-01-02 15:04:05")))
						md.WriteString(fmt.Sprintf("- **메시지**: %d개\n\n", s.MessageCount))
						md.WriteString("---\n\n")
						for _, msg := range s.Messages {
							if msg.Type == "user" {
								md.WriteString("## 👤 사용자\n\n")
							} else {
								md.WriteString("## 🤖 어시스턴트\n\n")
							}
							md.WriteString(msg.Content + "\n\n")
						}
						if os.WriteFile(exportPath, []byte(md.String()), 0644) == nil {
							exported++
							s.Selected = false
						}
					}
					populateTree(currentFilter)
					statusBar.SetText(fmt.Sprintf("[green]%d개 세션 내보내기 완료 (바탕화면)[-]", exported))
					return nil

				case 'c': // Compact mode toggle
					compactMode = !compactMode
					populateTree(currentFilter)
					if compactMode {
						statusBar.SetText("[aqua]컴팩트 모드 ON[-]")
					} else {
						statusBar.SetText("[green]컴팩트 모드 OFF[-]")
					}
					return nil

				case 'k': // Kill active session
					cur := tree.GetCurrentNode()
					s := nodeSession(cur)
					if s == nil || !s.Active {
						statusBar.SetText("[yellow]활성 세션이 아닙니다[-]")
						return nil
					}
					displayName := s.Alias
					if displayName == "" {
						displayName = trunc(s.FirstUserMsg, 30)
					}
					confirmModal := tview.NewModal().
						SetText(fmt.Sprintf("세션을 종료하시겠습니까?\n\n%s", displayName)).
						AddButtons([]string{"종료", "취소"}).
						SetDoneFunc(func(_ int, label string) {
							if label == "종료" {
								if err := killSession(s.ID); err != nil {
									statusBar.SetText(fmt.Sprintf("[red]%v[-]", err))
								} else {
									s.Active = false
									populateTree(currentFilter)
									statusBar.SetText(fmt.Sprintf("[green]세션 종료됨: %s[-]", esc(displayName)))
								}
							}
							app.SetRoot(mainLayout, true)
							app.SetFocus(tree)
						})
					confirmModal.SetBackgroundColor(tcell.ColorDarkSlateGray)
					confirmModal.SetButtonBackgroundColor(tcell.NewRGBColor(40, 40, 40))
					confirmModal.SetButtonTextColor(tcell.ColorGray)
					confirmModal.SetButtonActivatedStyle(tcell.StyleDefault.Background(tcell.ColorRed).Foreground(tcell.ColorWhite).Bold(true))
					confirmModal.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
						if ev.Key() == tcell.KeyRune && toEngKey(ev.Rune()) == 'k' {
							if err := killSession(s.ID); err != nil {
								statusBar.SetText(fmt.Sprintf("[red]%v[-]", err))
							} else {
								s.Active = false
								populateTree(currentFilter)
								statusBar.SetText(fmt.Sprintf("[green]세션 종료됨: %s[-]", esc(displayName)))
							}
							app.SetRoot(mainLayout, true)
							app.SetFocus(tree)
							return nil
						}
						return ev
					})
					app.SetRoot(confirmModal, true)
					return nil

				case '<': // Move folder up in order
					cur := tree.GetCurrentNode()
					refStr := nodeRefStr(cur)
					if strings.HasPrefix(refStr, "folder:") {
						folderName := strings.TrimPrefix(refStr, "folder:")
						for i, f := range meta.Folders {
							if f == folderName && i > 0 {
								meta.Folders[i], meta.Folders[i-1] = meta.Folders[i-1], meta.Folders[i]
								saveMetadata(meta)
								populateTree(currentFilter)
								statusBar.SetText(fmt.Sprintf("[green]폴더 순서 변경: %s ↑[-]", esc(folderName)))
								break
							}
						}
					}
					return nil

				case '>': // Move folder down in order
					cur := tree.GetCurrentNode()
					refStr := nodeRefStr(cur)
					if strings.HasPrefix(refStr, "folder:") {
						folderName := strings.TrimPrefix(refStr, "folder:")
						for i, f := range meta.Folders {
							if f == folderName && i < len(meta.Folders)-1 {
								meta.Folders[i], meta.Folders[i+1] = meta.Folders[i+1], meta.Folders[i]
								saveMetadata(meta)
								populateTree(currentFilter)
								statusBar.SetText(fmt.Sprintf("[green]폴더 순서 변경: %s ↓[-]", esc(folderName)))
								break
							}
						}
					}
					return nil

				case 'C': // Cycle folder color
					cur := tree.GetCurrentNode()
					refStr := nodeRefStr(cur)
					if strings.HasPrefix(refStr, "folder:") {
						folderName := strings.TrimPrefix(refStr, "folder:")
						presetColors := []string{"#CC66FF", "#FF6B6B", "#4ECDC4", "#FFE66D", "#95E1D3", "#FF8C00", "#6BCB77", "#4D96FF"}
						currentColor := meta.FolderColors[folderName]
						if currentColor == "" {
							currentColor = "#CC66FF"
						}
						nextIdx := 0
						for i, c := range presetColors {
							if c == currentColor {
								nextIdx = (i + 1) % len(presetColors)
								break
							}
						}
						meta.FolderColors[folderName] = presetColors[nextIdx]
						saveMetadata(meta)
						populateTree(currentFilter)
						statusBar.SetText(fmt.Sprintf("[%s]%s[-] 색상 변경됨", presetColors[nextIdx], esc(folderName)))
					}
					return nil

				case 'G': // Tag-based quick filter
					cur := tree.GetCurrentNode()
					s := nodeSession(cur)
					if s == nil {
						return nil
					}
					tags, ok := meta.SessionTags[s.ID]
					if !ok || len(tags) == 0 {
						statusBar.SetText("[yellow]태그가 없습니다[-]")
						return nil
					}
					if len(tags) == 1 {
						// Single tag: apply directly
						filterText := "#" + tags[0]
						searchInput.SetText(filterText)
						currentFilter = filterText
						searching = true
						rebuildLeftPane(true)
						populateTree(filterText)
						app.SetFocus(tree)
						return nil
					}
					tagList := tview.NewList()
					tagList.SetBorder(true).
						SetTitle(" 태그 필터 선택 ").
						SetTitleAlign(tview.AlignCenter).
						SetBorderColor(tcell.ColorDodgerBlue).
						SetBackgroundColor(tcell.ColorDarkSlateGray)
					tagList.SetSelectedBackgroundColor(tcell.ColorDodgerBlue)
					for _, tag := range tags {
						t := tag // capture
						tagList.AddItem("#"+t, "", 0, func() {
							filterText := "#" + t
							searchInput.SetText(filterText)
							currentFilter = filterText
							searching = true
							rebuildLeftPane(true)
							populateTree(filterText)
							app.SetRoot(mainLayout, true)
							app.SetFocus(tree)
						})
					}
					tagList.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
						if ev.Key() == tcell.KeyEscape {
							app.SetRoot(mainLayout, true)
							app.SetFocus(tree)
							return nil
						}
						return ev
					})
					tagListHeight := len(tags) + 2
					if tagListHeight > 15 {
						tagListHeight = 15
					}
					tagModal := tview.NewFlex().SetDirection(tview.FlexRow).
						AddItem(nil, 0, 1, false).
						AddItem(tview.NewFlex().
							AddItem(nil, 0, 1, false).
							AddItem(tagList, 40, 0, true).
							AddItem(nil, 0, 1, false), tagListHeight, 0, true).
						AddItem(nil, 0, 1, false)
					pages := tview.NewPages().
						AddPage("main", mainLayout, true, true).
						AddPage("tagfilter", tagModal, true, true)
					app.SetRoot(pages, true)
					app.SetFocus(tagList)
					return nil

				case 'u': // Self-update
					if updateInfo == "" {
						statusBar.SetText("[yellow]업데이트 확인 중...[-]")
						go func() {
							newVer, _, has := checkForUpdate()
							app.QueueUpdateDraw(func() {
								if !has {
									statusBar.SetText("[green]이미 최신 버전입니다 (v" + currentVersion() + ")[-]")
								} else {
									updateInfo = updateNotice(newVer)
									statusBar.SetText(fmt.Sprintf("[yellow]새 버전 %s 발견. u를 다시 누르면 업데이트[-]", newVer))
								}
							})
						}()
					} else {
						statusBar.SetText("[yellow]업데이트 중...[-]")
						go func() {
							err := selfUpdate()
							app.QueueUpdateDraw(func() {
								if err != nil {
									statusBar.SetText(fmt.Sprintf("[red]업데이트 실패: %v[-]", err))
								} else {
									updateInfo = ""
									statusBar.SetText("[green]업데이트 완료. csm을 다시 실행해주세요.[-]")
								}
							})
						}()
					}
					return nil
				}
			}
		}
		return ev
	})

	// Background detail loading
	go func() {
		valid := sessions[:0]
		for _, s := range sessions {
			loadSessionDetail(s)
			if s.MessageCount > 0 && (s.UserMsgCount > 1 || s.AsstMsgCount > 1) {
				valid = append(valid, s)
			}
		}
		app.QueueUpdateDraw(func() {
			sessions = valid
			sortSessions()
			populateTree(currentFilter)
		})
	}()

	// Background update check
	go func() {
		if newVer, _, has := checkForUpdate(); has {
			app.QueueUpdateDraw(func() {
				updateInfo = updateNotice(newVer)
				statusBar.SetText(defaultStatus())
			})
		}
	}()

	// Blink timer for active session indicator (text-only update, no tree rebuild)
	go func() {
		ticker := time.NewTicker(800 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			if len(activeNodes) == 0 {
				continue
			}
			blinkPhase = !blinkPhase
			app.QueueUpdateDraw(func() {
				for _, an := range activeNodes {
					numPrefix := fmt.Sprintf("[#666666]%2d[-] ", an.Num)
					nodeText := sessionNodeText(an.Session, currentFilter)
					if compactMode {
						nodeText = sessionNodeTextCompact(an.Session, currentFilter)
					}
					an.Node.SetText(numPrefix + nodeText)
				}
			})
		}
	}()

	// Background polling
	go func() {
		ticker := time.NewTicker(time.Duration(cfg.RefreshInterval) * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			fresh := discoverSessions()
			// Auto-delete closed temp sessions
			cleanedTemp := false
			for id := range meta.TempSessions {
				found := false
				for _, s := range fresh {
					if s.ID == id && s.Active {
						found = true
						break
					}
				}
				if !found {
					// Session closed or gone — find and delete
					for _, s := range fresh {
						if s.ID == id {
							deleteSession(s)
							break
						}
					}
					delete(meta.TempSessions, id)
					cleanedTemp = true
				}
			}
			if cleanedTemp {
				saveMetadata(meta)
				fresh = discoverSessions()
			}
			app.QueueUpdateDraw(func() {
				selectedIDs := map[string]bool{}
				for _, s := range sessions {
					if s.Selected {
						selectedIDs[s.ID] = true
					}
				}
				for _, s := range fresh {
					if selectedIDs[s.ID] {
						s.Selected = true
					}
				}
				sessions = fresh
				aliases = loadAliases()
				populateTree(currentFilter)
			})
		}
	}()

	if err := app.SetRoot(mainLayout, true).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
