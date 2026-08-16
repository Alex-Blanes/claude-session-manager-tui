package main

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"unicode/utf8"

	gopty "github.com/aymanbagabas/go-pty"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/welcomra1n/claude-session-manager-tui/internal/vt"
)

const (
	vtAttrReverse   int16 = 1 << iota
	vtAttrUnderline
	vtAttrBold
	vtAttrGfx
	vtAttrItalic
	vtAttrBlink
	vtAttrWrap
)

type TerminalWidget struct {
	*tview.Box
	vt      vt.Terminal
	pty     gopty.Pty
	cmd     *gopty.Cmd
	mu      sync.Mutex
	cols    int
	rows    int
	running bool
	onExit  func()
	app     *tview.Application
}

func NewTerminalWidget(app *tview.Application) *TerminalWidget {
	tw := &TerminalWidget{
		Box:  tview.NewBox(),
		cols: 80,
		rows: 24,
		app:  app,
	}
	tw.SetBorder(true).SetTitle(" Terminal ")
	return tw
}

func (tw *TerminalWidget) SetOnExit(fn func()) {
	tw.onExit = fn
}

func defaultShell() string {
	if runtime.GOOS == "windows" {
		if ps, err := exec.LookPath("pwsh.exe"); err == nil {
			return ps
		}
		return "powershell.exe"
	}
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/sh"
}

func (tw *TerminalWidget) StartShell(dir string) error {
	return tw.StartCommand(defaultShell(), dir)
}

func (tw *TerminalWidget) StartCommand(command, dir string, args ...string) error {
	tw.mu.Lock()
	defer tw.mu.Unlock()

	if tw.running {
		tw.stopLocked()
	}

	p, err := gopty.New()
	if err != nil {
		return err
	}

	_, _, w, h := tw.GetInnerRect()
	if w > 0 && h > 0 {
		tw.cols = w
		tw.rows = h
	}
	p.Resize(tw.cols, tw.rows)

	vt := vt.New(vt.WithSize(tw.cols, tw.rows), vt.WithWriter(p))

	resolved := command
	if lp, lerr := exec.LookPath(command); lerr == nil {
		resolved = lp
	}
	cmd := p.Command(resolved, args...)
	cmd.Dir = dir
	env := os.Environ()
	hasTerm := false
	for i, e := range env {
		if len(e) > 5 && e[:5] == "TERM=" {
			env[i] = "TERM=xterm-256color"
			hasTerm = true
			break
		}
	}
	if !hasTerm {
		env = append(env, "TERM=xterm-256color")
	}
	cmd.Env = env

	if err := cmd.Start(); err != nil {
		p.Close()
		return err
	}

	tw.pty = p
	tw.vt = vt
	tw.cmd = cmd
	tw.running = true

	go tw.readLoop()
	go tw.waitLoop()

	return nil
}

func (tw *TerminalWidget) readLoop() {
	tw.mu.Lock()
	p := tw.pty
	vt := tw.vt
	tw.mu.Unlock()

	reader := bufio.NewReaderSize(p, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			vt.Write(buf[:n])
			tw.app.QueueUpdateDraw(func() {})
		}
		if err != nil {
			return
		}
	}
}

func (tw *TerminalWidget) waitLoop() {
	if tw.cmd != nil {
		tw.cmd.Wait()
	}
	tw.mu.Lock()
	tw.running = false
	tw.mu.Unlock()
	if tw.onExit != nil {
		tw.app.QueueUpdate(func() {
			tw.onExit()
		})
	}
}

func (tw *TerminalWidget) stopLocked() {
	if tw.cmd != nil && tw.cmd.Process != nil {
		tw.cmd.Process.Kill()
	}
	if tw.pty != nil {
		tw.pty.Close()
	}
	tw.running = false
}

func (tw *TerminalWidget) Stop() {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	tw.stopLocked()
}

func (tw *TerminalWidget) IsRunning() bool {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	return tw.running
}

func (tw *TerminalWidget) writeInput(data []byte) {
	if tw.pty != nil {
		io.WriteString(tw.pty, string(data))
	}
}

func runewidth(r rune) int {
	if r == 0 {
		return 1
	}
	if (r >= 0x1100 && r <= 0x115F) ||
		(r >= 0x2E80 && r <= 0x9FFF) ||
		(r >= 0xAC00 && r <= 0xD7AF) ||
		(r >= 0xF900 && r <= 0xFAFF) ||
		(r >= 0xFE10 && r <= 0xFE6F) ||
		(r >= 0xFF01 && r <= 0xFF60) ||
		(r >= 0xFFE0 && r <= 0xFFE6) ||
		(r >= 0x20000 && r <= 0x2FFFD) ||
		(r >= 0x30000 && r <= 0x3FFFD) {
		return 2
	}
	return 1
}

func vtColorToTcell(c vt.Color) tcell.Color {
	if c == vt.DefaultFG {
		return tcell.ColorDefault
	}
	if c == vt.DefaultBG {
		return tcell.ColorDefault
	}
	if c == vt.DefaultCursor {
		return tcell.ColorDefault
	}
	if c.ANSI() {
		ansiMap := [16]tcell.Color{
			tcell.ColorBlack, tcell.ColorMaroon, tcell.ColorGreen, tcell.ColorOlive,
			tcell.ColorNavy, tcell.ColorPurple, tcell.ColorTeal, tcell.ColorSilver,
			tcell.ColorGray, tcell.ColorRed, tcell.ColorLime, tcell.ColorYellow,
			tcell.ColorBlue, tcell.ColorFuchsia, tcell.ColorAqua, tcell.ColorWhite,
		}
		return ansiMap[int(c)]
	}
	return tcell.NewRGBColor(
		int32((c>>16)&0xFF),
		int32((c>>8)&0xFF),
		int32(c&0xFF),
	)
}

func (tw *TerminalWidget) Draw(screen tcell.Screen) {
	tw.Box.DrawForSubclass(screen, tw)
	x, y, w, h := tw.GetInnerRect()

	tw.mu.Lock()
	vt := tw.vt
	p := tw.pty
	tw.mu.Unlock()

	if vt == nil {
		for row := 0; row < h; row++ {
			for col := 0; col < w; col++ {
				screen.SetContent(x+col, y+row, ' ', nil, tcell.StyleDefault)
			}
		}
		msg := "Pick a session and it will run here"
		runes := []rune(msg)
		msgW := 0
		for _, r := range runes {
			if r > 0x2E7F {
				msgW += 2
			} else {
				msgW += 1
			}
		}
		if w > msgW {
			startX := x + (w-msgW)/2
			startY := y + h/2
			style := tcell.StyleDefault.Foreground(tcell.ColorGray)
			col := startX
			for _, r := range runes {
				screen.SetContent(col, startY, r, nil, style)
				if r > 0x2E7F {
					col += 2
				} else {
					col += 1
				}
			}
		}
		return
	}

	if w != tw.cols || h != tw.rows {
		tw.cols = w
		tw.rows = h
		vt.Resize(w, h)
		if p != nil {
			p.Resize(w, h)
		}
	}

	vt.Lock()
	cols, rows := vt.Size()

	for row := 0; row < h && row < rows; row++ {
		col := 0
		for col < w && col < cols {
			g := vt.Cell(col, row)
			ch := g.Char
			if ch == 0 {
				screen.SetContent(x+col, y+row, ' ', nil, tcell.StyleDefault)
				col++
				continue
			}
			style := tcell.StyleDefault.
				Foreground(vtColorToTcell(g.FG)).
				Background(vtColorToTcell(g.BG))
			if g.Mode&vtAttrBold != 0 {
				style = style.Bold(true)
			}
			if g.Mode&vtAttrUnderline != 0 {
				style = style.Underline(true)
			}
			if g.Mode&vtAttrItalic != 0 {
				style = style.Italic(true)
			}
			if g.Mode&vtAttrReverse != 0 {
				style = style.Reverse(true)
			}
			cw := 1
			if ch >= 0x1100 {
				if (ch >= 0x1100 && ch <= 0x115F) ||
					(ch >= 0x2E80 && ch <= 0x9FFF) ||
					(ch >= 0xAC00 && ch <= 0xD7AF) ||
					(ch >= 0xF900 && ch <= 0xFAFF) ||
					(ch >= 0xFE10 && ch <= 0xFE6F) ||
					(ch >= 0xFF01 && ch <= 0xFF60) ||
					(ch >= 0xFFE0 && ch <= 0xFFE6) {
					cw = 2
				}
			}
			screen.SetContent(x+col, y+row, ch, nil, style)
			col += cw
		}
	}

	curVisible := vt.CursorVisible()
	cur := vt.Cursor()
	vt.Unlock()

	if curVisible && tw.HasFocus() {
		if cur.X >= 0 && cur.X < w && cur.Y >= 0 && cur.Y < h {
			screen.ShowCursor(x+cur.X, y+cur.Y)
		}
	}
}

func (tw *TerminalWidget) InputHandler() func(event *tcell.EventKey, setFocus func(p tview.Primitive)) {
	return tw.WrapInputHandler(func(event *tcell.EventKey, setFocus func(p tview.Primitive)) {
		if !tw.running || tw.pty == nil {
			return
		}

		var data []byte

		switch event.Key() {
		case tcell.KeyEnter:
			data = []byte("\r")
		case tcell.KeyBackspace, tcell.KeyBackspace2:
			data = []byte{0x7f}
		case tcell.KeyTab:
			data = []byte("\t")
		case tcell.KeyEscape:
			data = []byte{0x1b}
		case tcell.KeyUp:
			data = []byte("\x1b[A")
		case tcell.KeyDown:
			data = []byte("\x1b[B")
		case tcell.KeyRight:
			data = []byte("\x1b[C")
		case tcell.KeyLeft:
			data = []byte("\x1b[D")
		case tcell.KeyHome:
			data = []byte("\x1b[H")
		case tcell.KeyEnd:
			data = []byte("\x1b[F")
		case tcell.KeyPgUp:
			data = []byte("\x1b[5~")
		case tcell.KeyPgDn:
			data = []byte("\x1b[6~")
		case tcell.KeyInsert:
			data = []byte("\x1b[2~")
		case tcell.KeyDelete:
			data = []byte("\x1b[3~")
		case tcell.KeyF1:
			data = []byte("\x1bOP")
		case tcell.KeyF2:
			data = []byte("\x1bOQ")
		case tcell.KeyF3:
			data = []byte("\x1bOR")
		case tcell.KeyF4:
			data = []byte("\x1bOS")
		case tcell.KeyF5:
			data = []byte("\x1b[15~")
		case tcell.KeyF6:
			data = []byte("\x1b[17~")
		case tcell.KeyF7:
			data = []byte("\x1b[18~")
		case tcell.KeyF8:
			data = []byte("\x1b[19~")
		case tcell.KeyF9:
			data = []byte("\x1b[20~")
		case tcell.KeyF10:
			data = []byte("\x1b[21~")
		case tcell.KeyF11:
			data = []byte("\x1b[23~")
		case tcell.KeyF12:
			data = []byte("\x1b[24~")
		case tcell.KeyCtrlA:
			data = []byte{0x01}
		case tcell.KeyCtrlB:
			data = []byte{0x02}
		case tcell.KeyCtrlC:
			data = []byte{0x03}
		case tcell.KeyCtrlD:
			data = []byte{0x04}
		case tcell.KeyCtrlE:
			data = []byte{0x05}
		case tcell.KeyCtrlF:
			data = []byte{0x06}
		case tcell.KeyCtrlG:
			data = []byte{0x07}
		case tcell.KeyCtrlK:
			data = []byte{0x0b}
		case tcell.KeyCtrlL:
			data = []byte{0x0c}
		case tcell.KeyCtrlN:
			data = []byte{0x0e}
		case tcell.KeyCtrlO:
			data = []byte{0x0f}
		case tcell.KeyCtrlP:
			data = []byte{0x10}
		case tcell.KeyCtrlR:
			data = []byte{0x12}
		case tcell.KeyCtrlS:
			data = []byte{0x13}
		case tcell.KeyCtrlT:
			data = []byte{0x14}
		case tcell.KeyCtrlU:
			data = []byte{0x15}
		case tcell.KeyCtrlW:
			data = []byte{0x17}
		case tcell.KeyCtrlZ:
			data = []byte{0x1a}
		case tcell.KeyRune:
			var buf [4]byte
			n := utf8.EncodeRune(buf[:], event.Rune())
			data = buf[:n]
		}

		if len(data) > 0 {
			tw.writeInput(data)
		}
	})
}

func (tw *TerminalWidget) MouseHandler() func(action tview.MouseAction, event *tcell.EventMouse, setFocus func(p tview.Primitive)) (consumed bool, capture tview.Primitive) {
	return tw.WrapMouseHandler(func(action tview.MouseAction, event *tcell.EventMouse, setFocus func(p tview.Primitive)) (consumed bool, capture tview.Primitive) {
		switch action {
		case tview.MouseLeftClick:
			setFocus(tw)
			return true, nil
		case tview.MouseScrollUp:
			if tw.running {
				tw.writeInput([]byte("\x1b[5~"))
			}
			return true, nil
		case tview.MouseScrollDown:
			if tw.running {
				tw.writeInput([]byte("\x1b[6~"))
			}
			return true, nil
		}
		return false, nil
	})
}

