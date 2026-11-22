package ident

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testServer struct {
	ln  net.Listener
	srv *Server
	reg *Registry
}

func newTestServer(t *testing.T, timeout time.Duration) *testServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	reg := NewRegistry()
	srv, err := New(Config{
		Listener:     ln,
		Registry:     reg,
		QueryTimeout: timeout,
		Log:          discardLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(ctx); err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = ln.Close()
		<-done
	})
	return &testServer{ln: ln, srv: srv, reg: reg}
}

func (ts *testServer) dial(t *testing.T) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", ts.ln.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial ident: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// query writes req and returns the full reply line including CRLF, or "" on
// EOF.
func query(t *testing.T, conn net.Conn, req string) string {
	t.Helper()
	_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatalf("write request: %v", err)
	}
	return readReply(t, conn)
}

func readReply(t *testing.T, conn net.Conn) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		if err == io.EOF {
			return ""
		}
		t.Fatalf("read reply: %v", err)
	}
	return line
}

// tcpPair opens a real TCP connection and returns both ends, so the registry
// sees genuine *net.TCPAddr values.
func tcpPair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	client, err = net.DialTimeout("tcp", ln.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	server, err = ln.Accept()
	if err != nil {
		_ = client.Close()
		t.Fatalf("accept: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return client, server
}

func TestRoundTrip(t *testing.T) {
	ts := newTestServer(t, 0)
	client, server := tcpPair(t)
	remove := ts.reg.Add(client.LocalAddr(), client.RemoteAddr(), "testident")
	defer remove()
	_ = server

	la := client.LocalAddr().(*net.TCPAddr)
	ra := client.RemoteAddr().(*net.TCPAddr)

	conn := ts.dial(t)
	req := strings.Join([]string{itoa(ra.Port), itoa(la.Port)}, " , ") + "\r\n"
	got := query(t, conn, req)
	want := itoa(ra.Port) + " , " + itoa(la.Port) + " : USERID : UNIX : testident\r\n"
	if got != want {
		t.Fatalf("reply = %q, want %q", got, want)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestUnknownPairIsNoUser(t *testing.T) {
	ts := newTestServer(t, 0)
	client, _ := tcpPair(t)
	remove := ts.reg.Add(client.LocalAddr(), client.RemoteAddr(), "testident")
	defer remove()

	la := client.LocalAddr().(*net.TCPAddr)
	ra := client.RemoteAddr().(*net.TCPAddr)

	conn := ts.dial(t)

	nobody := itoa(ra.Port) + " , " + itoa(la.Port+1) + "\r\n"
	if got, want := query(t, conn, nobody), itoa(ra.Port)+" , "+itoa(la.Port+1)+" : ERROR : NO-USER\r\n"; got != want {
		t.Fatalf("near pair reply = %q, want %q", got, want)
	}

	if got, want := query(t, conn, "1 , 2\r\n"), "1 , 2 : ERROR : NO-USER\r\n"; got != want {
		t.Fatalf("unknown pair reply = %q, want %q", got, want)
	}
}

func TestMalformedIsInvalidPort(t *testing.T) {
	ts := newTestServer(t, 0)

	tests := []struct {
		name string
		req  string
	}{
		{"empty", "\r\n"},
		{"one port", "1\r\n"},
		{"three ports", "1,2,3\r\n"},
		{"port too large", "70000 , 1\r\n"},
		{"not numbers", "abc, def\r\n"},
		{"negative", "-1 , 2\r\n"},
		{"no comma", "1234 5678\r\n"},
		{"trailing junk", "1 , 2 x\r\n"},
		{"too long", strings.Repeat("1", 1200) + "\r\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := ts.dial(t)
			got := query(t, conn, tt.req)
			if got != "ERROR : INVALID-PORT\r\n" {
				t.Fatalf("reply = %q, want %q", got, "ERROR : INVALID-PORT\r\n")
			}
		})
	}
}

func TestNoNewlineHitsDeadline(t *testing.T) {
	ts := newTestServer(t, 100*time.Millisecond)
	conn := ts.dial(t)
	got := query(t, conn, "1234 , 5678")
	if got != "ERROR : INVALID-PORT\r\n" {
		t.Fatalf("reply = %q, want %q", got, "ERROR : INVALID-PORT\r\n")
	}
}

func TestRemoveIsEffective(t *testing.T) {
	ts := newTestServer(t, 0)
	client, _ := tcpPair(t)
	remove := ts.reg.Add(client.LocalAddr(), client.RemoteAddr(), "testident")
	remove()

	la := client.LocalAddr().(*net.TCPAddr)
	ra := client.RemoteAddr().(*net.TCPAddr)
	conn := ts.dial(t)
	req := itoa(ra.Port) + " , " + itoa(la.Port) + "\r\n"
	want := itoa(ra.Port) + " , " + itoa(la.Port) + " : ERROR : NO-USER\r\n"
	if got := query(t, conn, req); got != want {
		t.Fatalf("reply = %q, want %q", got, want)
	}

	remove()
}

func TestConcurrentAddRemoveLookup(t *testing.T) {
	reg := NewRegistry()
	const n = 50

	var added atomic.Int64
	var removed atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			local := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40000 + i}
			remote := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 6667}
			remove := reg.Add(local, remote, "ident")
			added.Add(1)
			if _, ok := reg.Lookup(Pair{ServerPort: 6667, ClientPort: 40000 + i}); !ok {
				t.Errorf("pair %d missing right after Add", i)
			}
			remove()
			removed.Add(1)
		}(i)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				reg.Lookup(Pair{ServerPort: 6667, ClientPort: 40000 + (i+j)%n})
			}
		}(i)
	}
	wg.Wait()

	if added.Load() != n || removed.Load() != n {
		t.Fatalf("added=%d removed=%d, want %d each", added.Load(), removed.Load(), n)
	}
	reg.mu.RLock()
	left := len(reg.m)
	reg.mu.RUnlock()
	if left != 0 {
		t.Fatalf("registry has %d entries after removes, want 0", left)
	}
}

func TestTwoQueriesThenClose(t *testing.T) {
	ts := newTestServer(t, 0)
	client, _ := tcpPair(t)
	remove := ts.reg.Add(client.LocalAddr(), client.RemoteAddr(), "testident")
	defer remove()

	la := client.LocalAddr().(*net.TCPAddr)
	ra := client.RemoteAddr().(*net.TCPAddr)
	req := itoa(ra.Port) + " , " + itoa(la.Port) + "\r\n"
	want := itoa(ra.Port) + " , " + itoa(la.Port) + " : USERID : UNIX : testident\r\n"

	conn := ts.dial(t)
	for i := 0; i < 2; i++ {
		if got := query(t, conn, req); got != want {
			t.Fatalf("reply %d = %q, want %q", i+1, got, want)
		}
	}

	_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_, _ = io.WriteString(conn, req)
	if got := readReply(t, conn); got != "" {
		t.Fatalf("third reply = %q, want connection closed", got)
	}
}
