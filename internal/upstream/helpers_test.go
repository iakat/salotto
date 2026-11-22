package upstream

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/iakat/salotto/internal/ring"
)

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

func accept(t *testing.T, ln net.Listener) net.Conn {
	t.Helper()
	if tl, ok := ln.(*net.TCPListener); ok {
		tl.SetDeadline(time.Now().Add(3 * time.Second))
		defer tl.SetDeadline(time.Time{})
	}
	conn, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	return conn
}

func readClientLine(t *testing.T, conn net.Conn, br *bufio.Reader) string {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read client line: %v", err)
	}
	return strings.TrimRight(line, "\r\n")
}

func expectLine(t *testing.T, conn net.Conn, br *bufio.Reader, want string) {
	t.Helper()
	got := readClientLine(t, conn, br)
	if got != want {
		t.Fatalf("client sent %q, want %q", got, want)
	}
}

func writeServerLines(t *testing.T, conn net.Conn, lines ...string) {
	t.Helper()
	for _, line := range lines {
		if _, err := io.WriteString(conn, line+"\r\n"); err != nil {
			t.Fatalf("write server line: %v", err)
		}
	}
}

// serverRegister performs the non-SASL registration handshake. wantReq may be
// empty when the offered capabilities contain nothing the client wants.
func serverRegister(t *testing.T, conn net.Conn, br *bufio.Reader, offered, wantReq, nick, userLine string) {
	t.Helper()
	expectLine(t, conn, br, "CAP LS 302")
	writeServerLines(t, conn, ":srv CAP * LS :"+offered)
	if wantReq != "" {
		expectLine(t, conn, br, "CAP REQ :"+wantReq)
		writeServerLines(t, conn, ":srv CAP * ACK :"+wantReq)
	}
	expectLine(t, conn, br, "CAP END")
	expectLine(t, conn, br, "NICK "+nick)
	expectLine(t, conn, br, userLine)
	writeServerLines(t, conn, ":srv 001 "+nick+" :Welcome")
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

func ringLines(r *ring.Ring) []string {
	var out []string
	r.Replay(r.Oldest(), func(seq uint64, line []byte) bool {
		out = append(out, string(line))
		return true
	})
	return out
}

func ringContains(r *ring.Ring, substr string) bool {
	for _, line := range ringLines(r) {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}
