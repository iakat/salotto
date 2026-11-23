package facade

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iakat/salotto/internal/winring"
)

type fakeNet struct {
	name string

	mu        sync.Mutex
	sends     []string
	state     string
	set       *winring.Set
	server    string
	mask      string
	connected bool
	resized   map[string]bool
	nick      string
	servers   []string
	chans     []string
	in        uint64
	out       uint64
}

func newFakeNet(name string) *fakeNet {
	return &fakeNet{
		name:    name,
		state:   "registered",
		set:     winring.New(8, 4, 4096),
		resized: make(map[string]bool),
	}
}

// Nick reports the nick we hold upstream, which is what KnownNick falls back to
// before any 001 has been seen through the facade.
func (f *fakeNet) Nick() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nick
}

// Servers/AddServer/DelServer/MoveChan are the ADDSERVER/DELSERVER/MOVECHAN
// surface. The fake keeps a real ordered list so the command tests exercise the
// same ordering logic the daemon does.
func (f *fakeNet) Servers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.servers...)
}

func (f *fakeNet) AddServer(addr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.servers {
		if strings.EqualFold(existing, addr) {
			return false
		}
	}
	f.servers = append(f.servers, addr)
	return true
}

func (f *fakeNet) DelServer(addr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, existing := range f.servers {
		if strings.EqualFold(existing, addr) {
			f.servers = append(f.servers[:i], f.servers[i+1:]...)
			return true
		}
	}
	return false
}

// MoveChan mirrors the daemon's channelSet: index range-checked first, then the
// channel looked up, matching IRCNetwork::MoveChan's order of validation.
func (f *fakeNet) MoveChan(channel string, index int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if index < 0 || index >= len(f.chans) {
		return errors.New("Invalid index")
	}
	at := -1
	for i, name := range f.chans {
		if strings.EqualFold(name, channel) {
			at = i
			break
		}
	}
	if at < 0 {
		return errors.New("You are not on " + channel)
	}
	name := f.chans[at]
	f.chans = append(f.chans[:at], f.chans[at+1:]...)
	f.chans = append(f.chans, "")
	copy(f.chans[index+1:], f.chans[index:])
	f.chans[index] = name
	return nil
}

func (f *fakeNet) ResizeWindow(window string, lines int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resized[window] = true
	return true
}

func (f *fakeNet) Name() string { return f.name }

func (f *fakeNet) State() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeNet) Send(line []byte) error {
	f.mu.Lock()
	f.sends = append(f.sends, string(line))
	f.mu.Unlock()
	return nil
}

func (f *fakeNet) Window(name string) (WindowRing, bool) {
	r, ok := f.set.Ring(name)
	if !ok {
		return nil, false
	}
	return r, true
}

func (f *fakeNet) Windows() []string { return f.set.Windows() }
func (f *fakeNet) Bytes() int        { return f.set.Bytes() }

func (f *fakeNet) Server() string { return f.server }

func (f *fakeNet) NickMask() string { return f.mask }

func (f *fakeNet) HasPassword() bool { return false }

func (f *fakeNet) Traffic() (in, out uint64) { return f.in, f.out }

func (f *fakeNet) connectEnabled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

func (f *fakeNet) SetConnectEnabled(enabled bool) {
	f.mu.Lock()
	f.connected = enabled
	f.mu.Unlock()
}

func (f *fakeNet) add(window, line string, ts int64) uint64 {
	return f.set.Append(window, []byte(line), ts)
}

func (f *fakeNet) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.sends))
	copy(out, f.sends)
	return out
}

func testConfig(nets ...Network) Config {
	m := make(map[string]*Network, len(nets))
	for _, n := range nets {
		nn := n
		m[n.Name()] = &nn
	}
	return Config{
		ServerName: "salotto",
		Network:    "TestNet",
		Hub:        NewHub(8),
		Networks:   m,
		MaxClients: 8,
		OutBuf:     64 * 1024,
		MaxLineLen: 8192,
	}
}

func startServer(t *testing.T, cfg Config) (*Server, string) {
	t.Helper()
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = s.Serve(ln) }()
	return s, ln.Addr().String()
}

func dialClient(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, bufio.NewReader(conn)
}

func sendLines(t *testing.T, conn net.Conn, lines ...string) {
	t.Helper()
	for _, line := range lines {
		_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.WriteString(conn, line+"\r\n"); err != nil {
			t.Fatalf("write %q: %v", line, err)
		}
	}
}

func readClientLine(t *testing.T, conn net.Conn, br *bufio.Reader) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read client line: %v", err)
	}
	return strings.TrimRight(line, "\r\n")
}

func collectUntil(t *testing.T, conn net.Conn, br *bufio.Reader, pred func(string) bool) []string {
	t.Helper()
	var out []string
	for i := 0; i < 2000; i++ {
		line := readClientLine(t, conn, br)
		out = append(out, line)
		if pred(line) {
			return out
		}
	}
	t.Fatalf("did not see expected line; got %d lines, last %q", len(out), out[len(out)-1])
	return nil
}

func sendRegistration(t *testing.T, conn net.Conn, nick string) {
	t.Helper()
	sendLines(t, conn,
		"CAP LS 302",
		"CAP REQ :server-time batch",
		"CAP END",
		"NICK "+nick,
		"USER "+nick+" 0 * :Test",
	)
}

func register(t *testing.T, conn net.Conn, br *bufio.Reader, nick string) []string {
	t.Helper()
	sendRegistration(t, conn, nick)
	return collectUntil(t, conn, br, func(line string) bool {
		return strings.Contains(line, " 376 ")
	})
}

// collectReplay drains the registration replay. With per-window rings a
// network with no buffered windows emits no batch at all, so an idle deadline,
// not a final marker, is what ends the replay.
func collectReplay(t *testing.T, conn net.Conn, br *bufio.Reader) []string {
	t.Helper()
	var out []string
	for {
		_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		line, err := br.ReadString('\n')
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				_ = conn.SetReadDeadline(time.Time{})
				return out
			}
			t.Fatalf("read client line: %v", err)
		}
		out = append(out, strings.TrimRight(line, "\r\n"))
	}
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

func TestRegistration(t *testing.T) {
	fn := newFakeNet("libera")
	_, addr := startServer(t, testConfig(fn))
	conn, br := dialClient(t, addr)

	lines := register(t, conn, br, "test")
	joined := strings.Join(lines, "\n")

	for _, want := range []string{" 001 test ", " 004 test ", " 005 test ", "NETWORK=TestNet"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("registration output missing %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(joined, " CAP * ACK :server-time batch") {
		t.Fatalf("CAP REQ was not acknowledged:\n%s", joined)
	}
}

func TestReplay(t *testing.T) {
	fn := newFakeNet("libera")
	ts := time.Date(2024, 1, 2, 3, 4, 5, 678000000, time.UTC).UnixNano()
	fn.add("#c", "@aaa=bbb PRIVMSG #c :one", ts)
	fn.add("#c", ":n!u@h PRIVMSG #c :two", ts+int64(time.Second))
	fn.add("#c", "PRIVMSG #c :three", ts+2*int64(time.Second))

	_, addr := startServer(t, testConfig(fn))
	conn, br := dialClient(t, addr)
	register(t, conn, br, "test")
	lines := collectReplay(t, conn, br)

	var msgs []string
	for _, line := range lines {
		if strings.Contains(line, "PRIVMSG #c :") {
			msgs = append(msgs, line)
		}
	}
	if len(msgs) != 3 {
		t.Fatalf("replayed %d PRIVMSG lines, want 3:\n%s", len(msgs), strings.Join(lines, "\n"))
	}

	wantTime := time.Unix(0, ts).UTC().Format("2006-01-02T15:04:05.000Z")
	if !strings.HasPrefix(msgs[0], "@batch=libera-#c-replay;aaa=bbb;time="+wantTime+" ") {
		t.Fatalf("first replayed line = %q, want batch first, existing tag preserved and time tag", msgs[0])
	}
	for i, msg := range msgs {
		if !strings.Contains(msg, "batch=libera-#c-replay") {
			t.Fatalf("line %d lacks batch tag: %q", i, msg)
		}
		if !strings.Contains(msg, "time=") {
			t.Fatalf("line %d lacks time tag: %q", i, msg)
		}
	}
	if !strings.HasSuffix(msgs[0], " :one") || !strings.HasSuffix(msgs[1], " :two") || !strings.HasSuffix(msgs[2], " :three") {
		t.Fatalf("replay order wrong: %q", msgs)
	}

	foundOpen, foundClose := false, false
	for _, line := range lines {
		if strings.Contains(line, " BATCH +replay libera-#c-replay") {
			foundOpen = true
		}
		if strings.Contains(line, " BATCH -replay libera-#c-replay") {
			foundClose = true
		}
	}
	if !foundOpen || !foundClose {
		t.Fatalf("replay batch markers missing (open=%v close=%v)", foundOpen, foundClose)
	}
}

func TestLiveBroadcast(t *testing.T) {
	fn := newFakeNet("libera")
	s, addr := startServer(t, testConfig(fn))
	conn, br := dialClient(t, addr)
	register(t, conn, br, "test")
	collectReplay(t, conn, br)
	waitFor(t, 3*time.Second, func() bool { return s.Attached() == 1 }, "client attached")

	ts := time.Date(2025, 6, 7, 8, 9, 10, 110000000, time.UTC).UnixNano()
	s.Broadcast("libera", 4, []byte(":n!u@h PRIVMSG #c :live"), ts)

	line := readClientLine(t, conn, br)
	if !strings.Contains(line, "PRIVMSG #c :live") {
		t.Fatalf("live line = %q, want PRIVMSG", line)
	}
	wantTime := time.Unix(0, ts).UTC().Format("2006-01-02T15:04:05.000Z")
	if !strings.Contains(line, "time="+wantTime) {
		t.Fatalf("live line missing time tag %s: %q", wantTime, line)
	}
}

func TestPingIsLocal(t *testing.T) {
	fn := newFakeNet("libera")
	_, addr := startServer(t, testConfig(fn))
	conn, br := dialClient(t, addr)
	register(t, conn, br, "test")
	collectReplay(t, conn, br)

	sendLines(t, conn, "PING :xyz")
	line := readClientLine(t, conn, br)
	if !strings.Contains(line, "PONG") || !strings.Contains(line, ":xyz") {
		t.Fatalf("PING reply = %q, want a PONG carrying :xyz", line)
	}
	if got := fn.sent(); len(got) != 0 {
		t.Fatalf("PING was forwarded upstream: %v", got)
	}
}

func TestForward(t *testing.T) {
	fn := newFakeNet("libera")
	_, addr := startServer(t, testConfig(fn))
	conn, br := dialClient(t, addr)
	register(t, conn, br, "test")
	collectReplay(t, conn, br)

	sendLines(t, conn, "PRIVMSG #chan :hello")
	waitFor(t, 3*time.Second, func() bool { return len(fn.sent()) == 1 }, "line forwarded upstream")

	got := fn.sent()
	if len(got) != 1 || got[0] != "PRIVMSG #chan :hello" {
		t.Fatalf("upstream received %q, want exactly PRIVMSG #chan :hello", got)
	}
}

func TestSlowClientDropped(t *testing.T) {
	fn := newFakeNet("libera")
	hub := NewHub(8)
	cfg := testConfig(fn)
	cfg.Hub = hub
	cfg.OutBuf = 32 * 1024
	s, addr := startServer(t, cfg)

	healthy, healthyBR := dialClient(t, addr)
	register(t, healthy, healthyBR, "healthy")
	collectReplay(t, healthy, healthyBR)
	waitFor(t, 3*time.Second, func() bool { return s.Attached() == 1 }, "healthy client attached")

	after := make(chan string, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		for {
			_ = healthy.SetReadDeadline(time.Now().Add(10 * time.Second))
			line, err := healthyBR.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if strings.Contains(line, "AFTER-SLOW") {
				select {
				case after <- line:
				default:
				}
				return
			}
		}
	}()
	<-started

	slow, _ := dialClient(t, addr)
	sendRegistration(t, slow, "slow")
	waitFor(t, 3*time.Second, func() bool { return s.Attached() == 2 }, "slow client attached")

	fill := append([]byte(":n!u@h PRIVMSG #c :"), bytes.Repeat([]byte("x"), 4000)...)
	deadline := time.Now().Add(5 * time.Second)
	broadcasts := 0
	for hub.Dropped() == 0 && time.Now().Before(deadline) {
		broadcasts++
		s.Broadcast("libera", uint64(broadcasts), fill, time.Now().UnixNano())
		time.Sleep(300 * time.Microsecond)
	}
	if hub.Dropped() == 0 {
		t.Fatalf("slow client was not dropped after %d broadcasts", broadcasts)
	}

	expectClosed(t, slow)

	s.Broadcast("libera", uint64(broadcasts+1), []byte(":n!u@h PRIVMSG #c :AFTER-SLOW"), time.Now().UnixNano())
	select {
	case line := <-after:
		if !strings.Contains(line, "AFTER-SLOW") {
			t.Fatalf("healthy client got %q", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("healthy client did not keep receiving after the slow client was dropped")
	}
}

func expectClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4096)
	for {
		_, err := conn.Read(buf)
		if err == nil {
			continue
		}
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatal("slow client connection was not closed")
		}
		return
	}
}

func TestMaxClients(t *testing.T) {
	fn := newFakeNet("libera")
	cfg := testConfig(fn)
	cfg.MaxClients = 2
	cfg.Hub = NewHub(4)
	s, addr := startServer(t, cfg)

	c1, b1 := dialClient(t, addr)
	register(t, c1, b1, "one")
	collectReplay(t, c1, b1)

	c2, b2 := dialClient(t, addr)
	register(t, c2, b2, "two")
	collectReplay(t, c2, b2)

	waitFor(t, 3*time.Second, func() bool { return s.Attached() == 2 }, "two clients attached")

	c3, b3 := dialClient(t, addr)
	line := readClientLine(t, c3, b3)
	if !strings.HasPrefix(line, "ERROR :too many clients") {
		t.Fatalf("third client got %q, want ERROR :too many clients", line)
	}
}

func registerWithCaps(t *testing.T, conn net.Conn, br *bufio.Reader, nick, caps string) []string {
	t.Helper()
	sendLines(t, conn,
		"CAP LS 302",
		"CAP REQ :"+caps,
		"CAP END",
		"NICK "+nick,
		"USER "+nick+" 0 * :Test",
	)
	return collectUntil(t, conn, br, func(line string) bool {
		return strings.Contains(line, " 376 ")
	})
}

func withoutTags(line string) string {
	if strings.HasPrefix(line, "@") {
		if i := strings.IndexByte(line, ' '); i >= 0 {
			return line[i+1:]
		}
	}
	return line
}

func replayRewriteFixture(t *testing.T) (*fakeNet, int64) {
	t.Helper()
	fn := newFakeNet("libera")
	ts := time.Date(2024, 2, 3, 4, 5, 6, 789000000, time.UTC).UnixNano()
	for i, tc := range []struct{ window, line string }{
		{"status", ":srv 001 up :Welcome"},
		{"#c", ":up!u@h JOIN #c * :real"},
		{"#c", ":srv 353 up @ #c :up alice +bob"},
		{"#c", ":srv 366 up #c :End"},
		{"#c", ":up!u@h PRIVMSG #c :hi"},
		{"#c", ":alice!a@h PRIVMSG #c :hello"},
	} {
		fn.add(tc.window, tc.line, ts+int64(i)*int64(time.Second))
	}
	return fn, ts
}

func TestReplayAddressesClientAsItself(t *testing.T) {
	fn, _ := replayRewriteFixture(t)
	_, addr := startServer(t, testConfig(fn))
	conn, br := dialClient(t, addr)
	registerWithCaps(t, conn, br, "me", "server-time batch")
	lines := collectReplay(t, conn, br)

	var got001, gotJoin, got353, gotSelf, gotAlice string
	for _, line := range lines {
		rest := withoutTags(line)
		switch {
		case strings.Contains(rest, " 001 "):
			got001 = rest
		case strings.Contains(rest, " JOIN #c"):
			gotJoin = rest
		case strings.Contains(rest, " 353 "):
			got353 = rest
		case strings.Contains(rest, "PRIVMSG #c :hi"):
			gotSelf = rest
		case strings.Contains(rest, "PRIVMSG #c :hello"):
			gotAlice = rest
		}
	}

	if !strings.Contains(got001, " 001 me ") || strings.Contains(got001, " 001 up ") {
		t.Fatalf("001 = %q, want target me", got001)
	}
	if gotJoin != ":me!u@h JOIN #c" {
		t.Fatalf("JOIN = %q, want extended-join stripped and prefix rewritten", gotJoin)
	}
	if got353 != ":srv 353 me @ #c :me alice +bob" {
		t.Fatalf("353 = %q, want target and names rewritten", got353)
	}
	if strings.Contains(got353, " up") || strings.Contains(got353, "@up") {
		t.Fatalf("353 still contains upstream nick: %q", got353)
	}
	if gotSelf != ":me!u@h PRIVMSG #c :hi" {
		t.Fatalf("self PRIVMSG = %q, want prefix rewritten", gotSelf)
	}
	if gotAlice != ":alice!a@h PRIVMSG #c :hello" {
		t.Fatalf("alice PRIVMSG = %q, want unchanged", gotAlice)
	}
}

func TestReplayKeepsExtendedJoinWhenNegotiated(t *testing.T) {
	fn, _ := replayRewriteFixture(t)
	_, addr := startServer(t, testConfig(fn))
	conn, br := dialClient(t, addr)
	registerWithCaps(t, conn, br, "me", "server-time batch extended-join")
	lines := collectReplay(t, conn, br)

	for _, line := range lines {
		rest := withoutTags(line)
		if strings.Contains(rest, " JOIN #c") {
			if rest != ":me!u@h JOIN #c * :real" {
				t.Fatalf("extended JOIN = %q, want account and realname kept", rest)
			}
			return
		}
	}
	t.Fatal("no JOIN line in replay")
}

func TestLiveBroadcastRewritesUpstreamIdentity(t *testing.T) {
	fn := newFakeNet("libera")
	ts := time.Date(2024, 2, 3, 4, 5, 6, 789000000, time.UTC).UnixNano()
	fn.add("status", ":srv 001 up :Welcome", ts)

	s, addr := startServer(t, testConfig(fn))
	conn, br := dialClient(t, addr)
	registerWithCaps(t, conn, br, "me", "server-time batch")
	collectReplay(t, conn, br)
	waitFor(t, 3*time.Second, func() bool { return s.Attached() == 1 }, "client attached")

	s.Broadcast("libera", 2, []byte(":up!u@h PRIVMSG #c :live"), ts+int64(time.Second))
	line := readClientLine(t, conn, br)
	if rest := withoutTags(line); rest != ":me!u@h PRIVMSG #c :live" {
		t.Fatalf("live line = %q, want upstream prefix rewritten to me", rest)
	}
}

func TestOtherUserLineIsByteIdentical(t *testing.T) {
	var buf bytes.Buffer
	sub := NewSubscriber(&buf, 4096)
	sub.SetIdentity("me", false)

	line := []byte(":alice!a@h PRIVMSG #c :hello\r\n")
	if !sub.WriteFrom(line, "up") {
		t.Fatal("WriteFrom rejected the line")
	}
	if err := sub.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), line) {
		t.Fatalf("third-party line changed: got %q, want %q", buf.Bytes(), line)
	}
}
