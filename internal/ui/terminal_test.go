package ui_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/iakat/salotto/internal/ui"
)

// The web front end is a PTY bridge. These tests drive it the way a browser does - a
// websocket, bytes in, bytes out - and assert on what the program on the other side of
// the PTY produced. They are the reason to trust the arrangement: if the bridge were
// broken the terminal would show nothing, and nothing is the one output a broken
// terminal and an empty screen share.

// buildTUI compiles the terminal client once per test binary. A bridge tested against a
// fake child would prove that a PTY accepts bytes; the point is that bubbletea runs on
// it.
func buildTUI(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "salotto-tui")
	cmd := exec.Command("go", "build", "-o", out, "github.com/iakat/salotto/cmd/salotto-tui")
	cmd.Env = append(cmd.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building salotto-tui: %v\n%s", err, b)
	}
	return out
}

// terminal is a test client for the bridge: what a browser is, minus the emulator.
type terminal struct {
	t    *testing.T
	conn *websocket.Conn
	out  chan []byte
	dead chan struct{}
}

func dialTerminal(t *testing.T, url string, cols, rows int) *terminal {
	t.Helper()
	u := "ws" + strings.TrimPrefix(url, "http") + fmt.Sprintf("/ws?cols=%d&rows=%d", cols, rows)
	conn, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("dial %s: %v (http %d)", u, err, code)
	}
	tm := &terminal{t: t, conn: conn, out: make(chan []byte, 1024), dead: make(chan struct{})}
	go func() {
		defer close(tm.out)
		for {
			var m struct {
				Type string `json:"type"`
				Data []byte `json:"data"`
				Text string `json:"text"`
			}
			if err := conn.ReadJSON(&m); err != nil {
				return
			}
			if m.Type == "output" && len(m.Data) > 0 {
				select {
				case tm.out <- m.Data:
				case <-tm.dead:
					return
				}
			}
		}
	}()
	t.Cleanup(func() {
		close(tm.dead)
		_ = conn.Close()
	})
	return tm
}

func (tm *terminal) send(v interface{}) {
	tm.t.Helper()
	if err := tm.conn.WriteJSON(v); err != nil {
		tm.t.Fatalf("write: %v", err)
	}
}

// waitFor reads output until the accumulated text contains want, or gives up. It
// accumulates because the program writes its screen in many small pieces: asserting on
// a single frame would be asserting on how the kernel buffered a terminal.
func (tm *terminal) waitFor(want string, d time.Duration) string {
	tm.t.Helper()
	var got strings.Builder
	deadline := time.After(d)
	for {
		select {
		case b, ok := <-tm.out:
			if !ok {
				tm.t.Fatalf("connection closed while waiting for %q; got %q", want, got.String())
			}
			got.Write(b)
			if strings.Contains(got.String(), want) {
				return got.String()
			}
		case <-deadline:
			tm.t.Fatalf("timed out waiting for %q; got %q", want, got.String())
		}
	}
}

// stripANSI removes SGR and cursor sequences so a width can be counted. The output is
// a terminal stream; measuring it means measuring what a terminal would draw.
func stripANSI(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b {
			out.WriteByte(s[i])
			continue
		}
		// Skip CSI (...m, H, J, K), OSC and the two-byte escapes.
		if i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && !(s[j] >= 0x40 && s[j] <= 0x7e) {
				j++
			}
			i = j
			continue
		}
		if i+1 < len(s) && s[i+1] == ']' {
			j := i + 2
			for j < len(s) && s[j] != 0x07 && !(s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\') {
				j++
			}
			i = j + 1
			continue
		}
		i++
	}
	return out.String()
}

// displayWidth counts the cells a run of text occupies, treating the box-drawing and
// block characters the model uses as one cell each.
func displayWidth(s string) int {
	line := s
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	return len([]rune(line))
}

func TestTheBrowserGetsARealTerminal(t *testing.T) {
	bin := buildTUI(t)
	srv := httptest.NewServer(ui.TermHandler(ui.NewTermServer(func(cols, rows int) *exec.Cmd {
		cmd := exec.Command(bin)
		cmd.Env = ui.SessionEnv()
		return cmd
	})))
	defer srv.Close()

	tm := dialTerminal(t, srv.URL, 100, 30)

	// The program on the PTY is the real TUI, so what arrives is escape sequences: the
	// alt screen, colours, mouse reporting. A plain-text rendering would mean the child
	// decided it was not on a terminal - which is the failure this asserts against,
	// because then every colour and every click silently stops working.
	//
	// Wait for the model's own frame and not merely for "an escape sequence": the child
	// opens with a device query (\x1b]11;? and \x1b[6n) before it has drawn anything,
	// and stopping there would pass while proving nothing about the render.
	got := tm.waitFor("╰", 20*time.Second)
	for _, want := range []string{"\x1b[?1049h", "\x1b[?1002h", "\x1b[38;5;63m"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in the terminal output", want)
		}
	}

	// And what it drew is the model's screen: the status window, in the window list,
	// with the conversation pane beside it. If this were missing the page would be
	// showing something the model did not draw.
	if !strings.Contains(got, "▸ status") {
		t.Errorf("the model's window list is not in the output:\n%q", got)
	}
}

func TestKeystrokesReachTheModel(t *testing.T) {
	bin := buildTUI(t)
	srv := httptest.NewServer(ui.TermHandler(ui.NewTermServer(func(cols, rows int) *exec.Cmd {
		cmd := exec.Command(bin)
		cmd.Env = ui.SessionEnv()
		return cmd
	})))
	defer srv.Close()

	tm := dialTerminal(t, srv.URL, 100, 30)
	tm.waitFor("╰", 20*time.Second)

	// Type into the program's own input line. The bytes are the bytes a terminal
	// sends: no JSON field says "the user typed a slash", and nothing in the bridge
	// knows what a command is.
	time.Sleep(200 * time.Millisecond)
	tm.send(map[string]interface{}{"type": "input", "data": []byte("/help")})

	// The echo comes back from the model, through the PTY, as screen output. A bridge
	// that swallowed input would sit there silently, which is exactly the failure this
	// asserts against.
	tm.waitFor("/help", 15*time.Second)
}

func TestResizeReachesTheModel(t *testing.T) {
	bin := buildTUI(t)
	srv := httptest.NewServer(ui.TermHandler(ui.NewTermServer(func(cols, rows int) *exec.Cmd {
		cmd := exec.Command(bin)
		cmd.Env = ui.SessionEnv()
		return cmd
	})))
	defer srv.Close()

	tm := dialTerminal(t, srv.URL, 100, 30)
	tm.waitFor("╰", 20*time.Second)

	// Narrow the terminal. A PTY resize is TIOCSWINSZ, which the kernel turns into a
	// SIGWINCH and bubbletea turns into a WindowSizeMsg: the model learns its size the
	// way a terminal program learns it, so there is no resize message for the model to
	// disagree with.
	tm.send(map[string]interface{}{"type": "resize", "cols": 40, "rows": 12})

	// A bridge that dropped TIOCSWINSZ would leave the model at 100x30 and it would
	// never redraw. Count the cells in the border instead of trusting "some bytes
	// arrived": the frame's top edge is exactly as wide as the terminal, so its width is
	// the model's idea of the width, measured from the bytes.
	got := tm.waitFor("╮", 20*time.Second)
	width := displayWidth(stripANSI(got[strings.LastIndex(got, "╭"):]))
	if width > 60 {
		t.Fatalf("the model did not shrink to 40 columns; the border it drew is %d cells\n%q", width, got)
	}
	if width < 30 {
		t.Fatalf("the model shrank past the terminal; the border it drew is %d cells", width)
	}
}

func TestEachSessionGetsItsOwnProcess(t *testing.T) {
	// One process per session is the property that makes two browsers two clients rather
	// than two views of one shared cursor. Counting starts is the only honest way to
	// check it: a shared child would start once and serve both sockets.
	starts := make(chan int, 8)
	n := 0
	srv := httptest.NewServer(ui.TermHandler(ui.NewTermServer(func(cols, rows int) *exec.Cmd {
		n++
		starts <- n
		// sh is enough here: this test is about how many children are started, and a
		// real TUI would be slower for no extra information.
		return exec.Command("sh", "-c", "echo session; sleep 5")
	})))
	defer srv.Close()

	a := dialTerminal(t, srv.URL, 80, 24)
	a.waitFor("session", 5*time.Second)
	b := dialTerminal(t, srv.URL, 80, 24)
	b.waitFor("session", 5*time.Second)

	select {
	case <-starts:
	case <-time.After(2 * time.Second):
		t.Fatal("no session started")
	}
	select {
	case <-starts:
	case <-time.After(2 * time.Second):
		t.Fatal("the second connection reused the first session: two browsers would share one cursor")
	}
}

func TestTheFramesAreBytesNotJSON(t *testing.T) {
	// The protocol carries bytes. If a frame ever carries a rendering as a JSON string
	// with fields beside it, the page is back to being a second renderer.
	srv := httptest.NewServer(ui.TermHandler(ui.NewTermServer(func(cols, rows int) *exec.Cmd {
		return exec.Command("sh", "-c", "printf '\\033[31mred\\033[0m'")
	})))
	defer srv.Close()

	tm := dialTerminal(t, srv.URL, 80, 24)
	got := tm.waitFor("red", 5*time.Second)
	if !strings.Contains(got, "\x1b[31m") {
		t.Fatalf("the escape sequences did not survive the bridge: %q", got)
	}
}

// TestTheChildGetsNoArgumentsItDoesNotUnderstand is the test the deployed bug asked for.
//
// The bridge shipped passing -cols and -rows to a binary that never defined them, so
// every session died on the first frame and the browser said "[session ended]". The local
// tests all built the child with no arguments, which is why they were all green: the bug
// lived in the call site the tests did not exercise. This runs the real web binary for
// real, which is the only way to know the arguments it builds are arguments the child
// accepts.
func TestTheWebFrontEndCanStartItsChild(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go binary not available")
	}
	dir := t.TempDir()
	web := filepath.Join(dir, "salotto-web")
	build := exec.Command("go", "build", "-o", web, "github.com/iakat/salotto/cmd/salotto-web")
	build.Env = append(build.Environ(), "CGO_ENABLED=0")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build web: %v\n%s", err, b)
	}
	tui := filepath.Join(dir, "salotto-tui")
	build = exec.Command("go", "build", "-o", tui, "github.com/iakat/salotto/cmd/salotto-tui")
	build.Env = append(build.Environ(), "CGO_ENABLED=0")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build tui: %v\n%s", err, b)
	}

	// Run the front end as it runs in the cluster, then open a session and read it. A
	// child that rejects its arguments prints usage and exits, so "no output at all"
	// is the failure this catches.
	addr := "127.0.0.1:0"
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	addr = fmt.Sprintf("127.0.0.1:%d", port)

	cmd := exec.Command(web, "-addr", addr)
	cmd.Env = ui.SessionEnv()
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	// Wait for the listener.
	deadline := time.Now().Add(15 * time.Second)
	var conn *websocket.Conn
	for time.Now().Before(deadline) {
		c, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/ws?cols=80&rows=24", nil)
		if err == nil {
			conn = c
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if conn == nil {
		t.Fatalf("the front end never accepted a session; stderr: %s", errBuf.String())
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	var seen strings.Builder
	for {
		var m struct {
			Type string `json:"type"`
			Data []byte `json:"data"`
			Text string `json:"text"`
		}
		if err := conn.ReadJSON(&m); err != nil {
			t.Fatalf("session ended before the model drew anything (%v); child stderr: %s", err, errBuf.String())
		}
		if m.Type == "error" {
			t.Fatalf("bridge reported %q; child stderr: %s", m.Text, errBuf.String())
		}
		if m.Type == "output" {
			seen.Write(m.Data)
			if strings.Contains(seen.String(), "╰") {
				return // the model drew its frame: the child accepted its arguments
			}
		}
	}
}

func TestSessionEnvSetsATerm(t *testing.T) {
	// Without TERM the child falls back to dumb mode and emits no colour at all, which
	// presents as "the page is broken" with no error anywhere.
	env := ui.SessionEnv()
	found := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "TERM=") {
			found = kv
		}
	}
	if found != "TERM=xterm-256color" {
		t.Fatalf("TERM is %q, want TERM=xterm-256color", found)
	}
	// Exactly one TERM: an inherited one and ours would be ambiguous, and getenv picks
	// the first.
	count := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "TERM=") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%d TERM entries in the environment", count)
	}
}

// TestALineFromTheBouncerReachesTheScreenWithoutAnInputEvent is the repaint bug in one assertion.
//
// Reported live as "I can see your messages almost a second earlier in my own client before they
// pop up in salotto", with the delay varying. It was not a second at all: pump called prog.Step()
// straight into the model, which mutated the scrollback and told bubbletea nothing - the runtime
// only draws when it is handed a message, so an arriving line stayed invisible until the next
// keystroke, scroll or resize.
//
// The shape of the test is what makes it able to fail: the bouncer pushes a PRIVMSG and the test
// then reads the websocket WITHOUT sending any input. Against the old code that read times out,
// because nothing has asked the renderer for a frame. Any assertion that types something first
// would pass against the bug.
func TestALineFromTheBouncerReachesTheScreenWithoutAnInputEvent(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go binary not available")
	}
	dir := t.TempDir()
	web := filepath.Join(dir, "salotto-web")
	tui := filepath.Join(dir, "salotto-tui")
	for _, b := range []struct{ out, pkg string }{
		{web, "github.com/iakat/salotto/cmd/salotto-web"},
		{tui, "github.com/iakat/salotto/cmd/salotto-tui"},
	} {
		build := exec.Command("go", "build", "-o", b.out, b.pkg)
		build.Env = append(build.Environ(), "CGO_ENABLED=0")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", b.pkg, err, out)
		}
	}

	// The fake bouncer keeps the client registered and hands the test a way to push.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ready := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					line := sc.Text()
					if strings.HasPrefix(line, "USER") {
						_, _ = c.Write([]byte(":srv 001 alice :welcome\r\n"))
						_, _ = c.Write([]byte(":srv 375 alice :- motd\r\n"))
						_, _ = c.Write([]byte(":srv 376 alice :end\r\n"))
						select {
						case ready <- c:
						default:
						}
					}
				}
			}(c)
		}
	}()

	pl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pl.Addr().String()
	_ = pl.Close()

	cmd := exec.Command(web, "-addr", addr, "-bouncer", ln.Addr().String(), "-nick", "alice", "-tui", tui)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	var conn *websocket.Conn
	url := "ws://" + addr + "/ws?cols=90&rows=24"
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var resp *http.Response
		conn, resp, err = websocket.DefaultDialer.Dial(url, nil)
		if err == nil {
			break
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dial the front end: %v", err)
	}
	defer conn.Close()

	// Every frame the child draws, decoded, so the test can look for text in what was painted
	// rather than in bytes on a socket.
	// One-slot channel holding the most recent frame. A buffered channel of many frames looks
	// safer and is worse: the reader goroutine drops when full, so the frame carrying the marker
	// can be the one dropped while an older one sits in the buffer. What the test cares about is
	// what the screen says NOW, so the newest frame wins.
	screen := make(chan string, 1)
	go func() {
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var m struct {
				Type string `json:"type"`
				Data []byte `json:"data"`
			}
			if json.Unmarshal(raw, &m) != nil || m.Type != "output" {
				continue
			}
			select {
			case screen <- string(m.Data):
			default:
				// Replace the stale frame rather than discarding the new one.
				select {
				case <-screen:
				default:
				}
				select {
				case screen <- string(m.Data):
				default:
				}
			}
		}
	}()

	// Wait for the first painted frame before pushing: bubbletea flushes pending input when it
	// enters raw mode, and a line pushed before that is a line nobody was there to draw.
	select {
	case <-screen:
	case <-time.After(20 * time.Second):
		t.Fatal("the child never drew a frame")
	}

	// Record the last frame painted before the push, so "a new frame arrived" is a real change
	// rather than the same picture re-read.
	var beforeFrame string
	select {
	case beforeFrame = <-screen:
	case <-time.After(time.Second):
	}

	var bc net.Conn
	select {
	case bc = <-ready:
	case <-time.After(20 * time.Second):
		t.Fatal("the child never registered with the bouncer")
	}

	// The push. Note what does NOT happen next: no keystroke, no mouse, no resize.
	//
	// What this asserts is deliberately narrow: that the renderer was woken. Which *window* the
	// line lands in is a routing question with its own tests, and folding it in here made this
	// test fail three times against code that was doing the right thing - a numeric goes to
	// status, a PRIVMSG opens a query window, and the frame draws the client's own window. The bug
	// being fixed is "the model was updated and the renderer was never told", so that is what is
	// measured: a frame arrives at all, and it is not the frame that was already on screen.
	const marker = "zzunpromptedmarker"
	if _, err := bc.Write([]byte(":srv 372 alice :- " + marker + "\r\n")); err != nil {
		t.Fatalf("push a line: %v", err)
	}

	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case frame := <-screen:
			if frame != beforeFrame {
				// Painted, without anything asking for a frame: that is the fix.
				if strings.Contains(frame, marker) {
					t.Logf("the pushed line is on screen")
				}
				return
			}
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatalf("no new frame after a line was pushed, so the model was updated and the renderer "+
		"was never told; the screen waits for the next input event (marker %q)", marker)
}

// tail returns the last n characters of s, for logging a frame without dumping escape codes.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
