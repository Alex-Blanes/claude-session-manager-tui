package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// ── Data types ──────────────────────────────────────────────────────────────

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
}

type Message struct {
	Type    string
	Content string
}

type rawLine struct {
	Type      string          `json:"type"`
	Message   json.RawMessage `json:"message,omitempty"`
	GitBranch string          `json:"gitBranch,omitempty"`
	CWD       string          `json:"cwd,omitempty"`
}

type msgEnvelope struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// ── Path decoding ───────────────────────────────────────────────────────────
// Claude Code encodes project paths by replacing '/' with '-'.
// Since directory names can also contain '-', we walk the filesystem
// to find the correct path.

func decodePath(enc string) string {
	if enc == "" {
		return ""
	}
	if runtime.GOOS == "windows" {
		return decodeWindowsPath(enc)
	}
	if result := resolveEncoded("/", enc[1:]); result != "" {
		return result
	}
	return "/" + strings.ReplaceAll(enc[1:], "-", "/")
}

// On Windows there is no leading '/', and the drive colon is encoded too:
// "C:\Users\bob" becomes "C--Users-bob". Split off the drive, then walk the
// rest exactly as on Unix.
// ponytail: UNC shares ("--host-share-...") aren't handled. We can't tell
// where the host name ends without listing "\\", which isn't listable. Add a
// server-name hint in config if network projects ever matter.
func decodeWindowsPath(enc string) string {
	drive, rest, ok := strings.Cut(enc, "--")
	if !ok || len(drive) != 1 {
		return enc
	}
	base := drive + `:\`
	if result := resolveEncoded(base, rest); result != "" {
		return result
	}
	return base + strings.ReplaceAll(rest, "-", `\`)
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
// name a '.', a space and an accented letter are all indistinguishable from a
// literal dash: "alex-blanes" is really "alex.blanes", and "Documentaci-n" is
// "Documentación". Comparison is per rune so multi-byte characters line up.
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
	p = strings.TrimRight(p, `/\`)
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// esc escapes '[' so tview doesn't interpret them as color tags.
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

// trunc shortens s to n characters for a single-line row, stopping at the first
// line break.
func trunc(s string, n int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncBlock(s, n)
}

// truncBlock shortens s to n characters but keeps its line breaks, for previews
// that are allowed more than one line. It counts runes, so a cut never lands in
// the middle of a multi-byte character and turns "máquina" into mojibake.
func truncBlock(s string, n int) string {
	if n < 1 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 3 {
		return string(r[:n])
	}
	return string(r[:n-3]) + "..."
}

// previewBudget is how many characters the info panel spends on the first
// message and the recent ones together.
const previewBudget = 1200

// maxRecentMsgs caps how many recent messages the panel will list, however
// short they are, so the first message never scrolls out of sight.
const maxRecentMsgs = 8

// recentUserMsgs returns the most recent user messages that fit in budget,
// oldest first. A single long message still takes the whole budget; several
// short ones show up together, because a run of one-liners tells you far more
// about where a session got to than the last of them on its own.
func recentUserMsgs(s *Session, budget int) []string {
	var out []string
	for i := len(s.Messages) - 1; i >= 0 && budget > 0 && len(out) < maxRecentMsgs; i-- {
		if s.Messages[i].Type != "user" {
			continue
		}
		text := strings.TrimSpace(s.Messages[i].Content)
		if !isMeaningfulMsg(text) {
			continue
		}
		text = truncBlock(text, budget)
		budget -= len([]rune(text))
		out = append(out, text)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// splitBudget divides a preview budget between two messages, handing the slack
// from whichever one is short to the other instead of cutting both at the same
// arbitrary length.
func splitBudget(a, b, total int) (int, int) {
	if a+b <= total {
		return a, b
	}
	half := total / 2
	switch {
	case a <= half:
		return a, total - a
	case b <= half:
		return total - b, b
	default:
		return half, total - half
	}
}

// isMeaningfulMsg filters out slash commands, system meta messages, and noise.
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
	// Claude Code injects its own turns as user messages: hook output, task
	// notifications, reminders, the echo of a slash command. They are not
	// something the person typed, so they don't belong in a preview of what
	// the session was about.
	for _, tag := range []string{
		"<task-notification>", "<system-reminder>", "<command-name>",
		"<command-message>", "<local-command-stdout>", "<user-prompt-submit-hook>",
		"[Request interrupted", "API Error", "<bash-input>", "<bash-stdout>",
	} {
		if strings.HasPrefix(first, tag) {
			return false
		}
	}
	// Compaction leaves its own two markers behind, one of them wrapped in
	// ANSI dim codes, so match anywhere in the line rather than at the start.
	for _, marker := range []string{
		"This session is being continued from a previous conversation",
		"Compacted (ctrl+o",
	} {
		if strings.Contains(first, marker) {
			return false
		}
	}
	if strings.HasPrefix(first, "/") {
		word := strings.Fields(first)[0]
		if !strings.Contains(word[1:], "/") { // not a file path
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

// ── Live sessions ───────────────────────────────────────────────────────────

// liveSession is the part of `claude agents --json` we care about.
type liveSession struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Status    string `json:"status"`
	Name      string `json:"name"`
}

var (
	liveMu   sync.RWMutex
	liveByID = map[string]liveSession{}
)

// refreshLive asks Claude Code which sessions are running right now. Asking it
// directly is exact where reading process command lines is not: a session
// started without --resume carries no session id on its command line, and
// wmic, the usual way to read them on Windows, was removed in Windows 11 24H2.
// The call takes a couple of seconds, so it must never run on the UI goroutine.
func refreshLive() {
	out, err := exec.Command("claude", "agents", "--json").Output()
	if err != nil {
		return // keep the last snapshot; a failed call is not proof of idleness
	}
	var list []liveSession
	if json.Unmarshal(out, &list) != nil {
		return
	}
	m := make(map[string]liveSession, len(list))
	for _, s := range list {
		m[s.SessionID] = s
	}
	liveMu.Lock()
	liveByID = m
	liveMu.Unlock()
}

func liveFor(id string) (liveSession, bool) {
	liveMu.RLock()
	defer liveMu.RUnlock()
	s, ok := liveByID[id]
	return s, ok
}

// liveMarker flags a running session in the list, so you can see it is open
// somewhere else before you touch it.
func liveMarker(id string) string {
	live, ok := liveFor(id)
	if !ok {
		return "  "
	}
	if live.Status == "busy" {
		return "[#ff8800]●[-] "
	}
	return "[#00c853]●[-] "
}

// ── Session loading ─────────────────────────────────────────────────────────

func loadSession(path string) *Session {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	dir := filepath.Base(filepath.Dir(path))
	ppath := decodePath(dir)
	sess := &Session{
		ID:          strings.TrimSuffix(filepath.Base(path), ".jsonl"),
		ProjectDir:  ppath,
		ProjectName: lastSegment(ppath),
		SessionFile: path,
		ModTime:     info.ModTime(),
		FileSize:    info.Size(),
	}

	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 10<<20)

	for sc.Scan() {
		var raw rawLine
		if json.Unmarshal(sc.Bytes(), &raw) != nil || (raw.Type != "user" && raw.Type != "assistant") {
			continue
		}
		var env msgEnvelope
		if json.Unmarshal(raw.Message, &env) != nil {
			continue
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

	// The transcript records the working directory verbatim, so prefer it over
	// the name we reconstructed from the encoded project folder. Decoding is a
	// guess -- every non-alphanumeric character arrives as '-' -- while this is
	// the path Claude Code actually ran in.
	if sess.CWD != "" {
		sess.ProjectDir = sess.CWD
		sess.ProjectName = lastSegment(sess.CWD)
	}

	// Filter out empty sessions and one-shot -p sessions (likely AI summary calls)
	if sess.MessageCount == 0 || (sess.UserMsgCount <= 1 && sess.AsstMsgCount <= 1) {
		return nil
	}
	return sess
}

func discoverSessions() []*Session {
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, ".claude", "projects")
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
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
				if s := loadSession(filepath.Join(dir, f.Name())); s != nil {
					out = append(out, s)
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out
}

// ── Terminal backend detection & integration ────────────────────────────────

type termBackend int

const (
	backendITerm2 termBackend = iota
	backendTerminalApp
	backendTmux
	backendKitty
	backendWezTerm
	backendWindowsTerminal
	backendWinConsole
	backendFallback
)

func (b termBackend) String() string {
	names := [...]string{"iTerm2", "Terminal.app", "tmux", "Kitty", "WezTerm", "Windows Terminal", "Console", "fallback"}
	if int(b) < len(names) {
		return names[b]
	}
	return "unknown"
}

var activeBackend termBackend

func detectBackend() termBackend {
	// Windows first: none of the Unix backends can launch anything here, and
	// the generic fallback shells out to sh, which usually isn't on PATH.
	if runtime.GOOS == "windows" {
		// Detect Windows Terminal by wt being installed, not by WT_SESSION.
		// That variable is only inherited by a shell running inside Terminal,
		// so it is empty when csm is launched from Explorer or a bare console
		// -- and `wt -w 0` still puts the tab in the most recently used
		// Terminal window from there. Gating on the variable silently
		// downgraded those launches to a detached console window.
		if _, err := exec.LookPath("wt.exe"); err == nil {
			return backendWindowsTerminal
		}
		return backendWinConsole
	}
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
	}
	if os.Getenv("KITTY_PID") != "" {
		return backendKitty
	}
	if _, err := exec.LookPath("tmux"); err == nil {
		return backendTmux
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
	case backendWindowsTerminal:
		return wtOpen(command, dir, inTab)
	case backendWinConsole:
		return winConsoleOpen(command, dir)
	default:
		var runErr error
		app.Suspend(func() {
			cmd := exec.Command("sh", "-c", fmt.Sprintf("cd '%s' && %s", escapeShell(dir), command))
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
		// Open a new tab in the frontmost window
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
	// Open a new window
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

// interactiveShell is the command that opens a plain shell in a new tab.
// On Windows it's empty on purpose: wt and cmd open their default profile,
// which is what the user configured.
func interactiveShell() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/sh"
}

// winShell prefers PowerShell 7, falling back to the built-in Windows PowerShell.
func winShell() string {
	if _, err := exec.LookPath("pwsh"); err == nil {
		return "pwsh"
	}
	return "powershell"
}

// wtWindow names a place wt can put a tab. Windows Terminal has no way to list
// the windows that exist, so there is nothing to enumerate and offer: an
// unknown id just creates another window, which is the thing we're avoiding.
// What it does guarantee is that a *named* window is reused when it exists and
// created once when it doesn't, so a dedicated window is the reliable way to
// stop scattering sessions across new windows.
type wtWindow struct {
	Label  string
	Window string // wt -w value: "0" this window, "-1" new, or a window name
	Split  bool
}

// "0" is the most recently used Terminal window, which is the one you were
// last looking at whether or not csm itself is running inside it.
var wtWindows = []wtWindow{
	{"New tab in the last window used", "0", false},
	{"Split the last window used", "0", true},
	{"Claude window (reused every time)", "claude", false},
	{"A brand new window", "-1", false},
}

// wtOpen keeps the old behaviour: a tab or a split in the current window.
func wtOpen(command, dir string, inTab bool) error {
	return wtOpenIn(command, dir, wtWindow{Window: "0", Split: !inTab})
}

// wtOpenIn runs command in the requested Windows Terminal window. The working
// directory comes from wt's own -d rather than the command string, so nothing
// here needs shell quoting.
func wtOpenIn(command, dir string, target wtWindow) error {
	verb := "nt"
	if target.Split {
		verb = "sp"
	}
	window := target.Window
	if window == "" {
		window = "0"
	}
	// A -d value ending in '\' would escape the closing quote Go wraps it in.
	args := []string{"-w", window, verb, "-d", strings.TrimRight(dir, `\`)}
	if command != "" {
		args = append(args, winShell(), "-NoExit", "-Command", command)
	}
	return exec.Command("wt.exe", args...).Run()
}

// winConsoleOpen opens a separate console window when Windows Terminal isn't
// hosting us. A bare console has no tabs, so inTab has no meaning here.
// `start` is a cmd builtin; its first quoted argument is the window title.
func winConsoleOpen(command, dir string) error {
	args := []string{"/c", "start", "", winShell()}
	if command != "" {
		args = append(args, "-NoExit", "-Command", command)
	}
	cmd := exec.Command("cmd", args...)
	cmd.Dir = dir
	return cmd.Run()
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

// ── Main ────────────────────────────────────────────────────────────────────

func main() {
	// The tab title is the only thing that tells this window from the ones it
	// opens. CSI 22;2t stacks the terminal's own title so 23;2t restores it.
	fmt.Print("\033[22;2t\033]2;csm - Claude sessions\007")
	defer fmt.Print("\033[23;2t")

	fmt.Print("Loading sessions...")
	sessions := discoverSessions()
	fmt.Print("\r\033[2K")

	activeBackend = detectBackend()
	app := tview.NewApplication()
	summaryCache := make(map[string]string)

	// ── Widgets ──

	sessionList := tview.NewList().
		ShowSecondaryText(true).
		SetHighlightFullLine(true)
	sessionList.SetBorder(true).
		SetTitle(" Sessions ").
		SetTitleAlign(tview.AlignLeft).
		SetBorderColor(tcell.ColorGreen)

	infoView := tview.NewTextView().
		SetDynamicColors(true).
		SetWordWrap(true).
		SetScrollable(true)
	infoView.SetBorder(true).
		SetTitle(" Session Info ").
		SetTitleAlign(tview.AlignLeft).
		SetBorderColor(tcell.ColorDodgerBlue)

	convView := tview.NewTextView().
		SetDynamicColors(true).
		SetRegions(true).
		SetWordWrap(true).
		SetScrollable(true)
	convView.SetBorder(true).
		SetTitle(" Conversation Preview ").
		SetTitleAlign(tview.AlignLeft).
		SetBorderColor(tcell.ColorDodgerBlue)

	searchInput := tview.NewInputField().
		SetLabel(" / ").
		SetLabelColor(tcell.ColorYellow).
		SetFieldBackgroundColor(tcell.ColorDefault).
		SetFieldTextColor(tcell.ColorWhite)

	statusBar := tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignCenter)

	// ── Layout: left (session list) | right (info + conversation) ──

	leftPane := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(sessionList, 0, 1, true)

	rightPane := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(infoView, 0, 2, false).
		AddItem(convView, 0, 3, false)

	mainBody := tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(leftPane, 0, 2, true).
		AddItem(rightPane, 0, 5, false)

	mainLayout := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(mainBody, 0, 1, true).
		AddItem(statusBar, 1, 0, false)

	// ── Session display ──

	// Where the conversation preview is parked, and which of its messages are
	// prompts, so { and } can walk between them.
	convMsgCount, convMsgIdx := 0, 0
	var convPrompts []int

	showSessionInfo := func(idx int) {
		if idx < 0 || idx >= len(sessions) {
			return
		}
		s := sessions[idx]

		var b strings.Builder
		fmt.Fprintf(&b, "[yellow]Project:[-]     %s\n", esc(s.ProjectName))
		fmt.Fprintf(&b, "[yellow]Path:[-]        %s\n", esc(s.ProjectDir))
		fmt.Fprintf(&b, "[yellow]Session:[-]     [green]%s[-]\n", s.ID)
		if live, ok := liveFor(s.ID); ok {
			fmt.Fprintf(&b, "[yellow]Running:[-]     [#ff8800]open in another terminal — pid %d, %s[-]\n", live.PID, live.Status)
		}
		fmt.Fprintf(&b, "[yellow]Modified:[-]    %s\n", s.ModTime.Format("2006-01-02 15:04:05"))
		fmt.Fprintf(&b, "[yellow]Size:[-]        %s\n", fmtSize(s.FileSize))
		fmt.Fprintf(&b, "[yellow]Messages:[-]    %d (%d user, %d asst)\n", s.MessageCount, s.UserMsgCount, s.AsstMsgCount)
		if s.GitBranch != "" {
			fmt.Fprintf(&b, "[yellow]Git Branch:[-]  %s\n", esc(s.GitBranch))
		}
		if s.CWD != "" {
			fmt.Fprintf(&b, "[yellow]CWD:[-]         %s\n", esc(s.CWD))
		}
		// Measure the tail at full budget first, so a short one hands its slack
		// to the opening message rather than the other way round.
		tailLen := 0
		for _, m := range recentUserMsgs(s, previewBudget) {
			tailLen += len([]rune(m))
		}
		firstN, tailN := splitBudget(len([]rune(s.FirstUserMsg)), tailLen, previewBudget)

		if s.FirstUserMsg != "" {
			fmt.Fprintf(&b, "\n[yellow]First msg:[-]\n  %s\n", esc(truncBlock(s.FirstUserMsg, firstN)))
		}
		if tail := recentUserMsgs(s, tailN); len(tail) > 0 {
			label := "Last msg"
			if len(tail) > 1 {
				label = fmt.Sprintf("Last %d msgs", len(tail))
			}
			fmt.Fprintf(&b, "\n[yellow]%s:[-]\n", label)
			for _, m := range tail {
				fmt.Fprintf(&b, "  [#888888]·[-] %s\n", esc(m))
			}
		}
		if summary, ok := summaryCache[s.ID]; ok {
			fmt.Fprintf(&b, "\n[aqua]── AI Summary ──[-]\n%s\n", esc(summary))
		} else {
			fmt.Fprintf(&b, "\n[gray]Press [yellow]i[-][gray] to generate AI summary[-]\n")
		}
		infoView.SetText(b.String())
		infoView.ScrollToBeginning()

		// Each message is its own region so j/k can jump between them instead
		// of scrolling by line.
		var conv strings.Builder
		convPrompts = convPrompts[:0]
		for i, msg := range s.Messages {
			content := truncBlock(msg.Content, 500)
			who := "[cyan]<<< Assistant:[-]"
			if msg.Type == "user" {
				who = "[green]>>> User:[-]"
				convPrompts = append(convPrompts, i)
			}
			fmt.Fprintf(&conv, "[\"m%d\"]%s\n%s\n\n[\"\"]", i, who, esc(content))
		}
		convView.SetText(conv.String())
		convMsgCount = len(s.Messages)
		convMsgIdx = 0
		convView.Highlight()
		convView.ScrollToBeginning()
	}

	// ── Filtered session list ──

	var filteredIdx []int

	defaultStatus := func() string {
		return fmt.Sprintf(
			"[green]%d sessions[-] [gray](%s)[-] | [#00c853]●[-] open elsewhere (Enter branches) | [yellow]Enter[-] resume | [yellow]n[-] new | [yellow]s[-] split | [yellow]i[-] summary | [yellow]/[-] search | [yellow]r[-] refresh | [yellow]q[-] quit",
			len(sessions), activeBackend,
		)
	}

	populateList := func(filter string) {
		sessionList.Clear()
		filteredIdx = nil
		lowerFilter := strings.ToLower(filter)

		var recent, older []int
		for i, s := range sessions {
			if lowerFilter != "" {
				haystack := strings.ToLower(s.ProjectName + " " + s.FirstUserMsg + " " + s.LastUserMsg + " " + s.GitBranch)
				if !strings.Contains(haystack, lowerFilter) {
					continue
				}
			}
			if time.Since(s.ModTime) < 7*24*time.Hour {
				recent = append(recent, i)
			} else {
				older = append(older, i)
			}
		}

		addItems := func(items []int, color string) {
			for _, si := range items {
				s := sessions[si]
				label := fmt.Sprintf("%s%s(%s) %s[-]", liveMarker(s.ID), color, s.ModTime.Format("01/02 15:04"), esc(s.ProjectName))
				desc := esc(trunc(s.FirstUserMsg, 60))
				if desc == "" {
					desc = fmt.Sprintf("%d messages", s.MessageCount)
				}
				sessionList.AddItem(label, "  [#aaaaaa]"+desc+"[-]", 0, nil)
				filteredIdx = append(filteredIdx, si)
			}
		}

		addItems(recent, "[#00ff00]")
		if len(recent) > 0 && len(older) > 0 {
			sessionList.AddItem("[#444444]────────────────────────────────[-]", "", 0, nil)
			filteredIdx = append(filteredIdx, -1)
		}
		addItems(older, "[#666666]")
		if len(filteredIdx) > 0 {
			sessionList.SetCurrentItem(0)
			showSessionInfo(filteredIdx[0])
		}
		if len(filteredIdx) == 0 {
			infoView.SetText("[gray]No matching sessions[-]")
			convView.SetText("")
		}
		if filter == "" {
			statusBar.SetText(defaultStatus())
		} else {
			statusBar.SetText(fmt.Sprintf(
				"[green]%d/%d sessions[-] | [yellow]Esc[-] clear | [yellow]Enter[-] tab | [yellow]s[-] split | [yellow]i[-] summary",
				len(filteredIdx), len(sessions),
			))
		}
	}
	populateList("")

	sessionList.SetChangedFunc(func(idx int, _, _ string, _ rune) {
		if idx >= 0 && idx < len(filteredIdx) && filteredIdx[idx] >= 0 {
			showSessionInfo(filteredIdx[idx])
		}
	})

	// ── Actions ──

	// Assigned further down, once the focus helpers it restores exist.
	var askWhere func(s *Session)

	// launch opens the session. target is only meaningful under Windows
	// Terminal; the other backends keep their own tab/split behaviour.
	launch := func(s *Session, inTab bool, target *wtWindow) {
		// Resuming a session that is already running in another terminal
		// interleaves both sides into one transcript and loses work. Branch
		// off it instead: same context, its own session id, nothing clobbered.
		cmd := fmt.Sprintf("claude --resume %s", s.ID)
		live, isLive := liveFor(s.ID)
		if isLive {
			cmd += " --fork-session"
		}

		var err error
		where := activeBackend.String()
		if target != nil {
			err = wtOpenIn(cmd, s.ProjectDir, *target)
			where = target.Label
		} else {
			err = openInTerminal(cmd, s.ProjectDir, inTab, app)
			if inTab {
				where += " tab"
			} else {
				where += " split"
			}
		}
		if err != nil {
			statusBar.SetText(fmt.Sprintf("[red]Failed (%s): %v[-]", activeBackend, err))
			return
		}
		if isLive {
			statusBar.SetText(fmt.Sprintf("[yellow]%s is already open (pid %d) — opened a fork: %s[-]",
				esc(s.ProjectName), live.PID, where))
		} else {
			statusBar.SetText(fmt.Sprintf("[green]Opened %s: %s[-]", esc(s.ProjectName), where))
		}
		go refreshLive()
	}

	openSession := func(idx int, inTab bool) {
		if idx < 0 || idx >= len(filteredIdx) || filteredIdx[idx] < 0 {
			return
		}
		s := sessions[filteredIdx[idx]]
		if activeBackend == backendWindowsTerminal {
			askWhere(s)
			return
		}
		launch(s, inTab, nil)
	}

	// gotoMsg parks the preview on one message, so you can walk a session's
	// shape before deciding whether to open it.
	gotoMsg := func(i int) {
		if convMsgCount == 0 {
			return
		}
		if i < 0 {
			i = 0
		}
		if i >= convMsgCount {
			i = convMsgCount - 1
		}
		convMsgIdx = i
		convView.Highlight(fmt.Sprintf("m%d", i)).ScrollToHighlight()
		statusBar.SetText(fmt.Sprintf(
			"[gray]msg [white]%d/%d[-][gray] | [yellow]{ }[-][gray] prompt | [yellow]PgUp/PgDn[-][gray] half | [yellow]Ctrl+Home/End[-][gray] ends | [yellow]j k[-][gray] line | [yellow]q[-][gray] list[-]",
			i+1, convMsgCount))
	}

	// gotoPrompt walks between prompts, the way { and } do in Claude Code's
	// own transcript view. Landing on the last one when there is no next is
	// deliberate: it doubles as "jump to the end".
	gotoPrompt := func(dir int) {
		if len(convPrompts) == 0 {
			return
		}
		if dir > 0 {
			for _, m := range convPrompts {
				if m > convMsgIdx {
					gotoMsg(m)
					return
				}
			}
			gotoMsg(convPrompts[len(convPrompts)-1])
			return
		}
		for i := len(convPrompts) - 1; i >= 0; i-- {
			if convPrompts[i] < convMsgIdx {
				gotoMsg(convPrompts[i])
				return
			}
		}
		gotoMsg(convPrompts[0])
	}

	// pageLines is the viewport height divided by frac: 1 for a full page, 2
	// for the half page ^u and ^d move.
	pageLines := func(frac int) int {
		_, _, _, h := convView.GetInnerRect()
		if h < 2 {
			h = 2
		}
		return h / frac
	}

	scrollBy := func(lines int) {
		row, col := convView.GetScrollOffset()
		if row+lines < 0 {
			lines = -row
		}
		convView.ScrollTo(row+lines, col)
	}

	requestSummary := func() {
		idx := sessionList.GetCurrentItem()
		if idx < 0 || idx >= len(filteredIdx) || filteredIdx[idx] < 0 {
			return
		}
		s := sessions[filteredIdx[idx]]
		if _, ok := summaryCache[s.ID]; ok {
			showSessionInfo(filteredIdx[idx])
			return
		}
		sessionID := s.ID
		sessionIdx := filteredIdx[idx]
		statusBar.SetText(fmt.Sprintf("[yellow]Generating summary for %s...[-]", esc(s.ProjectName)))
		infoView.SetText(infoView.GetText(false) + "\n[yellow]Generating AI summary...[-]")
		go func() {
			summary, err := generateSummary(s)
			app.QueueUpdateDraw(func() {
				if err != nil {
					statusBar.SetText(fmt.Sprintf("[red]Summary failed: %v[-]", err))
					return
				}
				summaryCache[sessionID] = summary
				curIdx := sessionList.GetCurrentItem()
				if curIdx >= 0 && curIdx < len(filteredIdx) && filteredIdx[curIdx] == sessionIdx {
					showSessionInfo(sessionIdx)
				}
				statusBar.SetText(fmt.Sprintf("[green]Summary ready: %s[-]", esc(s.ProjectName)))
			})
		}()
	}

	sessionList.SetSelectedFunc(func(idx int, _, _ string, _ rune) {
		openSession(idx, true)
	})

	// ── Focus & search ──

	focusables := []tview.Primitive{sessionList, convView}
	focusIdx := 0
	searching := false
	currentFilter := ""

	// Which sessions are live takes a couple of seconds to find out, so fetch
	// it off the UI goroutine and redraw once it lands. Press r to refresh it.
	go func() {
		refreshLive()
		app.QueueUpdateDraw(func() { populateList(currentFilter) })
	}()

	updateBorders := func() {
		if focusIdx == 0 {
			sessionList.SetBorderColor(tcell.ColorGreen)
			convView.SetBorderColor(tcell.ColorDodgerBlue)
		} else {
			sessionList.SetBorderColor(tcell.ColorDodgerBlue)
			convView.SetBorderColor(tcell.ColorGreen)
		}
	}

	askWhere = func(s *Session) {
		labels := make([]string, 0, len(wtWindows)+1)
		for _, w := range wtWindows {
			labels = append(labels, w.Label)
		}
		labels = append(labels, "Cancel")

		modal := tview.NewModal().
			SetText(fmt.Sprintf("Open %s where?", s.ProjectName)).
			AddButtons(labels).
			SetDoneFunc(func(i int, _ string) {
				app.SetRoot(mainLayout, true)
				app.SetFocus(focusables[focusIdx])
				updateBorders()
				if i >= 0 && i < len(wtWindows) {
					launch(s, true, &wtWindows[i])
				}
			})
		app.SetRoot(modal, true)
	}

	showSearch := func() {
		searching = true
		leftPane.Clear()
		leftPane.AddItem(searchInput, 1, 0, false)
		leftPane.AddItem(sessionList, 0, 1, false)
		app.SetFocus(searchInput)
	}

	hideSearch := func() {
		searching = false
		leftPane.Clear()
		leftPane.AddItem(sessionList, 0, 1, true)
		searchInput.SetText("")
		currentFilter = ""
		populateList("")
		focusIdx = 0
		app.SetFocus(sessionList)
		updateBorders()
	}

	searchInput.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEscape {
			hideSearch()
		} else if key == tcell.KeyEnter {
			focusIdx = 0
			app.SetFocus(sessionList)
			updateBorders()
		}
	})

	searchInput.SetChangedFunc(func(text string) {
		currentFilter = text
		populateList(text)
	})

	// ── Key bindings ──

	app.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if searching && app.GetFocus() == searchInput {
			return ev
		}

		switch ev.Key() {
		case tcell.KeyTab:
			focusIdx = (focusIdx + 1) % len(focusables)
			app.SetFocus(focusables[focusIdx])
			updateBorders()
			return nil
		case tcell.KeyPgUp, tcell.KeyPgDn:
			// Half a screen, the same distance Claude Code moves in its own
			// conversation view.
			if focusIdx == 1 && !searching {
				n := pageLines(2)
				if ev.Key() == tcell.KeyPgUp {
					n = -n
				}
				scrollBy(n)
				return nil
			}
		case tcell.KeyHome, tcell.KeyEnd:
			// Modifiers are ignored on purpose so Ctrl+Home and Ctrl+End, the
			// pair Claude Code documents, land here alongside the bare keys.
			if focusIdx == 1 && !searching {
				if ev.Key() == tcell.KeyHome {
					convView.ScrollToBeginning()
				} else {
					convView.ScrollToEnd()
				}
				return nil
			}
		case tcell.KeyCtrlD, tcell.KeyCtrlU, tcell.KeyCtrlF, tcell.KeyCtrlB:
			if focusIdx == 1 && !searching {
				switch ev.Key() {
				case tcell.KeyCtrlD:
					scrollBy(pageLines(2))
				case tcell.KeyCtrlU:
					scrollBy(-pageLines(2))
				case tcell.KeyCtrlF:
					scrollBy(pageLines(1))
				case tcell.KeyCtrlB:
					scrollBy(-pageLines(1))
				}
				return nil
			}
		case tcell.KeyEscape:
			if searching {
				hideSearch()
				return nil
			}
		case tcell.KeyRune:
			if focusIdx == 0 {
				switch ev.Rune() {
				case 'q':
					app.Stop()
					return nil
				case '/':
					showSearch()
					return nil
				case 'n': // New tab
					home, _ := os.UserHomeDir()
					err := openInTerminal(interactiveShell(), home, true, app)
					if err != nil {
						statusBar.SetText(fmt.Sprintf("[red]Failed (%s): %v[-]", activeBackend, err))
					} else {
						statusBar.SetText("[green]Opened new tab[-]")
					}
					return nil
				case 's':
					openSession(sessionList.GetCurrentItem(), false)
					return nil
				case 'i':
					requestSummary()
					return nil
				case 'r':
					go func() {
						app.QueueUpdateDraw(func() {
							statusBar.SetText("[yellow]Refreshing...[-]")
						})
						fresh := discoverSessions()
						refreshLive()
						app.QueueUpdateDraw(func() {
							sessions = fresh
							populateList(currentFilter)
						})
					}()
					return nil
				}
			} else if focusIdx == 1 {
				// Same keys as Claude Code's transcript view, so there is
				// nothing new to learn: { } between prompts, j k by line,
				// space/b by page, g G to the ends.
				switch ev.Rune() {
				case '}':
					gotoPrompt(1)
					return nil
				case '{':
					gotoPrompt(-1)
					return nil
				case 'g':
					convView.ScrollToBeginning()
					return nil
				case 'G':
					convView.ScrollToEnd()
					return nil
				case 'j':
					scrollBy(1)
					return nil
				case 'k':
					scrollBy(-1)
					return nil
				case ' ':
					scrollBy(pageLines(1))
					return nil
				case 'b':
					scrollBy(-pageLines(1))
					return nil
				case 'q':
					focusIdx = 0
					app.SetFocus(focusables[0])
					updateBorders()
					return nil
				}
			}
		}
		return ev
	})

	if err := app.SetRoot(mainLayout, true).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
