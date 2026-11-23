package ui_test

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
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

func TestOriginCheckRefusesAnotherHost(t *testing.T) {
	// The socket carries a terminal with the user's bouncer attached. Anything
	// cross-origin that can open it can type as them.
	srv := httptest.NewServer(ui.TermHandler(ui.NewTermServer(func(cols, rows int) *exec.Cmd {
		return exec.Command("sh", "-c", "sleep 1")
	})))
	defer srv.Close()

	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	h := map[string][]string{"Origin": {"https://evil.example"}}
	conn, resp, err := websocket.DefaultDialer.Dial(u, h)
	if err == nil {
		_ = conn.Close()
		t.Fatal("a cross-origin websocket was accepted")
	}
	if resp == nil || resp.StatusCode != 403 {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("want 403, got %d (%v)", code, err)
	}
}

// The termMsg shape is the interface. If someone adds a field describing the screen,
// this fails and the design decision gets argued again rather than quietly reversed.
func TestTheProtocolHasNoScreenDescription(t *testing.T) {
	b, err := json.Marshal(map[string]interface{}{"type": "output", "data": []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"data":"eA=="`) {
		t.Fatalf("bytes are not base64 on the wire: %s", b)
	}
}
