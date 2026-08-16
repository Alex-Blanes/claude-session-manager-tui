package main

import (
	"archive/zip"
	"bufio"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const backupRetentionDays = 30

const usage = `csm — Claude Code session manager

  csm                      browse and open sessions (the TUI)
  csm export <id> [flags]  pack a session into a .zip
  csm import <zip> [flags] unpack one, remapped to this machine's paths

Move the .zip yourself (scp, a share, a USB stick). Run each subcommand with
-h for its flags.`

// ── Ancestry ─────────────────────────────────────────────────────────────────

type ancestry int

const (
	ancestryFresh ancestry = iota // nothing here by that id
	ancestrySame
	ancestryIncomingNewer // what we have is a prefix of what arrived
	ancestryLocalNewer    // the other way round
	ancestryDiverged      // both grew from a common start, differently
)

func compareTranscripts(local, incoming []string) ancestry {
	n := min(len(local), len(incoming))
	if hashLines(local[:n]) != hashLines(incoming[:n]) {
		return ancestryDiverged
	}
	switch {
	case len(local) == len(incoming):
		return ancestrySame
	case len(incoming) > len(local):
		return ancestryIncomingNewer
	default:
		return ancestryLocalNewer
	}
}

// ── Export ───────────────────────────────────────────────────────────────────

func runExport(args []string) error {
	fs := flag.NewFlagSet("csm export", flag.ContinueOnError)
	out := fs.String("o", "", "output file (default <id>.zip here)")
	noWork := fs.Bool("no-work-dir", false, "leave the working directory out of the package")
	forceWork := fs.Bool("force-work-dir", false, "pack the working directory even if it sits in a synced folder")
	extract := fs.String("extract", "", "what to do with the original afterwards: backup, delete or keep (asks if omitted)")
	id, rest := splitPositional(args)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if id == "" && fs.NArg() == 1 {
		id = fs.Arg(0)
	}
	if id == "" {
		return fmt.Errorf("usage: csm export <session-id> [-o file.zip] [--no-work-dir] [--extract backup|delete|keep]")
	}

	src, projectDir, err := findSessionFile(id)
	if err != nil {
		return err
	}
	lines, err := readLines(src)
	if err != nil {
		return err
	}
	originCWD := cwdOf(lines)

	dest := *out
	if dest == "" {
		dest = id + ".zip"
	}
	zf, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer zf.Close()
	zw := zip.NewWriter(zf)

	if _, err := addFile(zw, src, "transcript.jsonl"); err != nil {
		return err
	}
	histDir := filepath.Join(claudeDir(), "file-history", id)
	nHist, _, err := addTree(zw, histDir, "file-history")
	if err != nil {
		return err
	}
	toolDir := filepath.Join(claudeDir(), "projects", projectDir, id)
	nTool, _, err := addTree(zw, toolDir, "tool-results")
	if err != nil {
		return err
	}

	man := Manifest{
		Schema:     transferSchema,
		SessionID:  id,
		ExportedAt: time.Now().UTC(),
		Host:       hostname(),
		OriginCWD:  originCWD,
		ProjectDir: projectDir,
		Lines:      len(lines),
		FirstLine:  hashLines(lines[:min(1, len(lines))]),
		Whole:      hashLines(lines),
	}

	if !*noWork && originCWD != "" {
		info, err := packWorkDir(zw, originCWD, *forceWork)
		if err != nil {
			return err
		}
		man.WorkDir = info
	}

	blob, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return err
	}
	w, err := zw.Create(manifestName)
	if err != nil {
		return err
	}
	if _, err := w.Write(blob); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}

	st, _ := os.Stat(dest)
	fmt.Printf("Exported %s -> %s (%.1f MB)\n", id, dest, float64(st.Size())/(1<<20))
	fmt.Printf("  transcript: %d lines, from %s\n", man.Lines, originCWD)
	if nHist > 0 {
		fmt.Printf("  file-history: %d files\n", nHist)
	}
	if nTool > 0 {
		fmt.Printf("  tool-results: %d files\n", nTool)
	}
	if man.WorkDir != nil {
		fmt.Printf("  working directory: %d files, %.1f MB (%s)\n",
			man.WorkDir.Files, float64(man.WorkDir.Bytes)/(1<<20), man.WorkDir.Source)
	}

	mode := *extract
	if mode == "" {
		mode = askExtractMode()
	}
	return applyExtract(mode, id, src, histDir, toolDir)
}

// packWorkDir adds the working directory, unless it lives in a synced folder.
// Walking one of those hydrates every on-demand placeholder, which quietly pulls
// the whole tree down from the cloud — and if the other machine syncs the same
// account, the files are already there anyway.
func packWorkDir(zw *zip.Writer, dir string, force bool) (*WorkDirInfo, error) {
	if root, synced := insideSyncedFolder(dir); synced && !force {
		fmt.Printf("  working directory skipped: %s is inside %s (synced).\n", dir, root)
		fmt.Printf("    The other machine likely syncs it already. --force-work-dir overrides.\n")
		return nil, nil
	}
	if _, err := os.Stat(dir); err != nil {
		fmt.Printf("  working directory skipped: %s is not here\n", dir)
		return nil, nil
	}
	rel, source, err := workDirFiles(dir)
	if err != nil {
		return nil, err
	}
	info := &WorkDirInfo{Source: source}
	for _, r := range rel {
		n, err := addFile(zw, filepath.Join(dir, r), filepath.Join("workdir", r))
		if err != nil {
			continue // a file that vanished mid-walk is not worth failing the export
		}
		info.Files++
		info.Bytes += n
	}
	if info.Bytes > workDirWarnMB<<20 {
		fmt.Printf("  note: working directory is %.0f MB\n", float64(info.Bytes)/(1<<20))
	}
	return info, nil
}

func askExtractMode() string {
	fmt.Print("\nOriginal session: [b]ackup and purge in 30 days, [d]elete now, [k]eep as is? ")
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return "keep"
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "b", "backup":
		return "backup"
	case "d", "delete":
		return "delete"
	default:
		return "keep"
	}
}

func applyExtract(mode, id, transcript, histDir, toolDir string) error {
	switch mode {
	case "", "keep":
		return nil
	case "delete":
		os.Remove(transcript)
		os.RemoveAll(histDir)
		os.RemoveAll(toolDir)
		fmt.Println("Original removed.")
		return nil
	case "backup":
		stamp := time.Now().Format("2006-01-02")
		dest := filepath.Join(claudeDir(), "backups", "csm", stamp+"-"+id)
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return err
		}
		if err := os.Rename(transcript, filepath.Join(dest, filepath.Base(transcript))); err != nil {
			return err
		}
		os.Rename(histDir, filepath.Join(dest, "file-history"))
		os.Rename(toolDir, filepath.Join(dest, "tool-results"))
		fmt.Printf("Original moved to %s, purged after %d days.\n", dest, backupRetentionDays)
		return nil
	default:
		return fmt.Errorf("unknown --extract mode %q: use backup, delete or keep", mode)
	}
}

// purgeBackups runs at startup. Anything past its retention has already served
// its purpose as a safety net.
func purgeBackups() {
	root := filepath.Join(claudeDir(), "backups", "csm")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -backupRetentionDays)
	for _, e := range entries {
		info, err := e.Info()
		if err == nil && info.ModTime().Before(cutoff) {
			os.RemoveAll(filepath.Join(root, e.Name()))
		}
	}
}

// ── Import ───────────────────────────────────────────────────────────────────

func runImport(args []string) error {
	fs := flag.NewFlagSet("csm import", flag.ContinueOnError)
	targetCWD := fs.String("cwd", "", "working directory this session belongs to on this machine")
	dryRun := fs.Bool("dry-run", false, "report what would happen and write nothing")
	asNew := fs.Bool("as-new", false, "on divergence, import under a fresh id instead of refusing")
	force := fs.Bool("force", false, "allow writing into a working directory that already has files")
	pkg, rest := splitPositional(args)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if pkg == "" && fs.NArg() == 1 {
		pkg = fs.Arg(0)
	}
	if pkg == "" {
		return fmt.Errorf("usage: csm import <file.zip> [--cwd <dir>] [--dry-run] [--as-new] [--force]")
	}

	zr, err := zip.OpenReader(pkg)
	if err != nil {
		return err
	}
	defer zr.Close()

	man, err := readManifest(&zr.Reader)
	if err != nil {
		return err
	}
	if man.Schema > transferSchema {
		return fmt.Errorf("package schema %d is newer than this csm understands (%d)", man.Schema, transferSchema)
	}

	dest := *targetCWD
	if dest == "" {
		if _, err := os.Stat(man.OriginCWD); err == nil {
			dest = man.OriginCWD
		} else {
			return fmt.Errorf("the original path %s does not exist here; pass --cwd with the directory this session belongs to", man.OriginCWD)
		}
	}
	dest, err = filepath.Abs(dest)
	if err != nil {
		return err
	}

	id := man.SessionID
	incoming, err := transcriptFromZip(&zr.Reader)
	if err != nil {
		return err
	}

	state := ancestryFresh
	existing, existingProject, findErr := findSessionFile(id)
	if findErr == nil {
		local, err := readLines(existing)
		if err != nil {
			return err
		}
		state = compareTranscripts(local, incoming)
	}

	fmt.Printf("Session %s from %s (%s)\n", id, man.Host, man.ExportedAt.Local().Format("2006-01-02 15:04"))
	fmt.Printf("  origin cwd: %s\n", man.OriginCWD)
	fmt.Printf("  target cwd: %s  -> projects\\%s\n", dest, encodeCwd(dest))

	switch state {
	case ancestrySame:
		fmt.Println("  already here and identical: nothing to do.")
		return nil
	case ancestryLocalNewer:
		return fmt.Errorf("the copy here has %d lines and the package %d, with the same start: this machine is ahead, importing would lose work", len(mustLines(existing)), len(incoming))
	case ancestryDiverged:
		if !*asNew {
			return fmt.Errorf("a session with this id exists here and the two have diverged; re-run with --as-new to keep both")
		}
		id = newSessionID()
		fmt.Printf("  diverged: importing under a new id, %s\n", id)
	case ancestryIncomingNewer:
		fmt.Printf("  came back with %d new lines: updating (the copy here goes to backups first)\n", len(incoming)-len(mustLines(existing)))
	case ancestryFresh:
		fmt.Println("  new here.")
	}

	if *dryRun {
		fmt.Println("  --dry-run: nothing written.")
		return nil
	}

	if state == ancestryIncomingNewer {
		stamp := time.Now().Format("2006-01-02-150405")
		bdir := filepath.Join(claudeDir(), "backups", "csm", stamp+"-"+id+"-replaced")
		if err := os.MkdirAll(bdir, 0o755); err != nil {
			return err
		}
		if err := os.Rename(existing, filepath.Join(bdir, filepath.Base(existing))); err != nil {
			return err
		}
		os.Rename(filepath.Join(claudeDir(), "projects", existingProject, man.SessionID), filepath.Join(bdir, "tool-results"))
	}

	projectDir := filepath.Join(claudeDir(), "projects", encodeCwd(dest))
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		return err
	}
	target := filepath.Join(projectDir, id+".jsonl")
	if err := writeTranscript(target, incoming, man.OriginCWD, dest); err != nil {
		return err
	}
	fmt.Printf("  transcript -> %s\n", target)

	if err := restoreTrees(&zr.Reader, id, projectDir, dest, *force); err != nil {
		return err
	}
	fmt.Println("Done. It shows up in csm and in claude --resume as any other session.")
	return nil
}

func readManifest(zr *zip.Reader) (*Manifest, error) {
	for _, f := range zr.File {
		if f.Name != manifestName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		var m Manifest
		if err := json.NewDecoder(rc).Decode(&m); err != nil {
			return nil, err
		}
		return &m, nil
	}
	return nil, fmt.Errorf("no %s in the package: is this a csm export?", manifestName)
}

func transcriptFromZip(zr *zip.Reader) ([]string, error) {
	for _, f := range zr.File {
		if f.Name != "transcript.jsonl" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		var lines []string
		sc := bufio.NewScanner(rc)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) != "" {
				lines = append(lines, sc.Text())
			}
		}
		return lines, sc.Err()
	}
	return nil, fmt.Errorf("no transcript.jsonl in the package")
}

// writeTranscript rewrites the recorded cwd and nothing else. The paths quoted
// inside the conversation stay as they were said — rewriting those would forge
// the transcript.
func writeTranscript(path string, lines []string, from, to string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, l := range lines {
		if _, err := w.WriteString(retargetCWD(l, from, to) + "\n"); err != nil {
			return err
		}
	}
	return w.Flush()
}

func restoreTrees(zr *zip.Reader, id, projectDir, workTarget string, force bool) error {
	histRoot := filepath.Join(claudeDir(), "file-history", id)
	toolRoot := filepath.Join(projectDir, id)

	workRoot := workTarget
	if hasFiles(workTarget) && !force {
		workRoot = workTarget + ".imported"
	}
	warned := false

	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		var root, rel string
		switch {
		case strings.HasPrefix(f.Name, "file-history/"):
			root, rel = histRoot, strings.TrimPrefix(f.Name, "file-history/")
		case strings.HasPrefix(f.Name, "tool-results/"):
			root, rel = toolRoot, strings.TrimPrefix(f.Name, "tool-results/")
		case strings.HasPrefix(f.Name, "workdir/"):
			root, rel = workRoot, strings.TrimPrefix(f.Name, "workdir/")
			if workRoot != workTarget && !warned {
				fmt.Printf("  %s already has files: working directory goes to %s (--force writes in place)\n", workTarget, workRoot)
				warned = true
			}
		default:
			continue
		}
		dest, err := safeJoin(root, rel)
		if err != nil {
			return err
		}
		if err := writeZipEntry(f, dest); err != nil {
			return err
		}
	}
	return nil
}

// ── Small helpers ────────────────────────────────────────────────────────────

// splitPositional pulls a leading non-flag argument aside so flags can come
// after it. Go's flag package stops parsing at the first positional, which
// would silently ignore everything typed after the session id.
func splitPositional(args []string) (first string, rest []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func hasFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}

func mustLines(path string) []string {
	l, _ := readLines(path)
	return l
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func newSessionID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
