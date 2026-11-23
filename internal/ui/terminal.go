package ui

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

// The web front end is a terminal, not an imitation of one.
//
// bubbletea is a terminal program: it writes escape sequences to a file descriptor and
// reads keystrokes back. The way to serve that over HTTP is the way ttyd, GoTTY and
// gotty do it - give the process a PTY and put a terminal emulator in the browser. Any
// other arrangement is a second renderer: the JSON frame protocol this file replaces
// sent the model's string plus a set of parallel fields (windows, nicks, unread), and
// every consumer had to reassemble a UI out of them. That is precisely where the
// duplicated nicklist, the box inside a box and the input line printed twice came
// from - not from bugs in the reassembly, but from the reassembly existing.
//
// So: one PTY per websocket, the real bubbletea program on the other end, and xterm.js
// drawing the bytes. Click, resize, key and paste all arrive as what they are on a
// terminal - input - and bubbletea handles them, mouse included, with no glue code
// that knows what a window list is.

const (
	// termCols and termRows are the starting geometry. The browser sends its real
	// size immediately after opening; this only has to be sane for the first frame.
	termCols = 100
	termRows = 30
)

// TermServer serves one bubbletea program per connection over a websocket.
type TermServer struct {
	// Command builds the process to run for a session. It is a function because each
	// session gets its own: a shared process would mean a shared cursor, a shared
	// input buffer and a shared scroll position, which is one user per bouncer forced
	// on every browser.
	Command func(cols, rows int) *exec.Cmd

	upgrader websocket.Upgrader
}

// NewTermServer returns a server that runs cmd per session.
func NewTermServer(cmd func(cols, rows int) *exec.Cmd) *TermServer {
	return &TermServer{
		Command: cmd,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			// Same-origin only. The socket carries a live terminal with the user's
			// bouncer attached; anything cross-origin that can open it can type as
			// them.
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				if origin == "" {
					// A non-browser client has no Origin. It also has no session to
					// steal, and the deployment puts this behind the gateway.
					return true
				}
				return sameHost(origin, r.Host)
			},
		},
	}
}

// ServeHTTP upgrades and bridges one session.
func (t *TermServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := t.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	cols, rows := termCols, termRows
	if v := r.URL.Query().Get("cols"); v != "" {
		if n, ok := parsePositive(v); ok && n >= 20 {
			cols = n
		}
	}
	if v := r.URL.Query().Get("rows"); v != "" {
		if n, ok := parsePositive(v); ok && n >= 5 {
			rows = n
		}
	}

	cmd := t.Command(cols, rows)
	if cmd == nil {
		return
	}

	// A PTY makes the child believe it is on a terminal, which is the entire point:
	// bubbletea takes its input mode, its size and its colour profile from that.
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		_ = conn.WriteJSON(termMsg{Type: "error", Text: err.Error()})
		return
	}
	defer func() {
		_ = ptmx.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_, _ = cmd.Process.Wait()
	}()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var writeMu sync.Mutex
	write := func(m termMsg) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteJSON(m)
	}

	// PTY -> browser. Raw bytes, base64'd by encoding/json for a []byte, because the
	// browser's terminal wants the escape sequences exactly as the program wrote them.
	go func() {
		defer cancel()
		buf := make([]byte, 32<<10)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				out := make([]byte, n)
				copy(out, buf[:n])
				if werr := write(termMsg{Type: "output", Data: out}); werr != nil {
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					_ = write(termMsg{Type: "exit"})
				}
				return
			}
		}
	}()

	// browser -> PTY. Everything is input, because that is what a terminal is: there
	// is no "click on a window" message, only the escape sequence a click produces.
	for {
		var m termMsg
		if err := conn.ReadJSON(&m); err != nil {
			return
		}
		switch m.Type {
		case "input":
			if len(m.Data) > 0 {
				if _, err := ptmx.Write(m.Data); err != nil {
					return
				}
			}
		case "resize":
			// A terminal resize is a kernel-level event (TIOCSWINSZ), which is how
			// bubbletea learns its size: WindowSizeMsg arrives because the PTY told
			// it, not because a JSON field said so.
			if m.Cols >= 20 && m.Rows >= 5 {
				_ = pty.Setsize(ptmx, &pty.Winsize{Cols: uint16(m.Cols), Rows: uint16(m.Rows)})
			}
		case "ping":
			_ = write(termMsg{Type: "pong"})
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

// termMsg is the whole protocol: bytes in, bytes out, a resize, and a ping. There is
// no field describing what the screen contains, because the screen is bytes.
type termMsg struct {
	Type string `json:"type"`
	Data []byte `json:"data,omitempty"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
	Text string `json:"text,omitempty"`
}

// sameHost compares an Origin header's host with the request's.
func sameHost(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == host
}

// parsePositive parses a decimal integer, for the query-string size hints.
func parsePositive(v string) (int, bool) {
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// SessionEnv is the environment a session runs with. TERM is what makes the child emit
// the escape sequences a terminal emulator expects; without it every program falls
// back to dumb mode and the browser renders a wall of plain text.
func SessionEnv(extra ...string) []string {
	env := make([]string, 0, len(os.Environ())+4)
	for _, kv := range os.Environ() {
		// A TERM inherited from the container is almost always unset or dumb, and
		// passing xterm-256color explicitly is what the browser is emulating.
		if len(kv) > 5 && kv[:5] == "TERM=" {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "TERM=xterm-256color", "COLORTERM=truecolor")
	env = append(env, extra...)
	return env
}
