package facade

import (
	"bytes"
	"strings"
	"testing"

	"github.com/iakat/salotto/internal/ircline"
)

// serverCmdFixture returns a Server with one fake network and a client that
// writes to a bytes.Buffer, so a command's output is exactly what it wrote.
// handleLine sends one line through the real dispatch, which takes the tagged
// and the untagged form.
func handleLine(t *testing.T, c *client, line string) {
	t.Helper()
	raw := []byte(line)
	_, rest := ircline.Tags(raw)
	_, rest = ircline.Prefix(rest)
	if !c.handle(raw, rest) {
		t.Fatalf("%s: handler asked to close the client", line)
	}
}

// drainNotices flushes the client's output and returns the NOTICE payloads.
func drainNotices(c *client, out *bytes.Buffer) []string {
	_ = c.sub.Flush()
	data := strings.TrimRight(out.String(), "\r\n")
	out.Reset()
	if data == "" {
		return nil
	}
	return notices(strings.Split(data, "\r\n"))
}

func serverCmdFixture(t *testing.T, net *fakeNet) (*client, *bytes.Buffer) {
	t.Helper()
	s, _ := startServer(t, testConfig(net))
	buf := &bytes.Buffer{}
	sub := NewSubscriber(buf, 64*1024)
	sub.SetIdentity("test", false)
	return &client{srv: s, sub: sub, nick: "test"}, buf
}

// runServerCmd drives one bouncer command through the real dispatch and returns
// the notices it produced.
func runServerCmd(t *testing.T, net *fakeNet, command string) []string {
	t.Helper()
	c, buf := serverCmdFixture(t, net)
	handleLine(t, c, command)
	_ = c.sub.Flush()
	data := strings.TrimRight(buf.String(), "\r\n")
	buf.Reset()
	if data == "" {
		return nil
	}
	return notices(strings.Split(data, "\r\n"))
}

// TestAddServerSpellings pins ZNC's `AddServer <host> [[+]port] [pass]` parsing
// (ClientCommand.cpp:820-850) down to the answers: added, and the already-added
// sentence when it is a duplicate.
func TestAddServerSpellings(t *testing.T) {
	net := newFakeNet("TestNet")
	c, out := serverCmdFixture(t, net)

	handleLine(t, c, "ADDSERVER irc.example.org 6697")
	got := drainNotices(c, out)
	if len(got) != 1 || !strings.Contains(got[0], "Server added") {
		t.Fatalf("add with port: %v", got)
	}
	if servers := net.Servers(); len(servers) != 1 || servers[0] != "irc.example.org:6697" {
		t.Fatalf("servers = %v", servers)
	}

	// A "+" prefix is ZNC's SSL marker; the port is still the port.
	drainNotices(c, out)
	handleLine(t, c, "ADDSERVER irc.other.org +6698")
	if servers := net.Servers(); len(servers) != 2 || servers[1] != "irc.other.org:6698" {
		t.Fatalf("servers after +port = %v", servers)
	}

	// No port at all defaults to 6667, which is CServer::Parse's own default.
	drainNotices(c, out)
	handleLine(t, c, "ADDSERVER irc.third.org")
	if servers := net.Servers(); len(servers) != 3 || servers[2] != "irc.third.org:6667" {
		t.Fatalf("servers after bare host = %v", servers)
	}

	// Adding a duplicate is refused with ZNC's sentence.
	drainNotices(c, out)
	handleLine(t, c, "ADDSERVER irc.example.org 6697")
	got = drainNotices(c, out)
	if len(got) != 1 || !strings.Contains(got[0], "Perhaps the server is already added") {
		t.Fatalf("duplicate add: %v", got)
	}

	// No argument prints the usage and the "+ means SSL" hint.
	drainNotices(c, out)
	handleLine(t, c, "ADDSERVER")
	got = drainNotices(c, out)
	if len(got) != 2 || !strings.Contains(got[0], "Usage: AddServer") || !strings.Contains(got[1], "+ means SSL") {
		t.Fatalf("usage: %v", got)
	}
}

// TestDelServerAnswers pins the four distinct sentences, in ZNC's order of
// checks: usage, empty list, removed, not found.
func TestDelServerAnswers(t *testing.T) {
	net := newFakeNet("TestNet")
	c, out := serverCmdFixture(t, net)

	// Empty list first: ZNC answers "You don't have any servers added." before
	// it ever parses the argument's absence, so a bare DelServer says usage.
	handleLine(t, c, "DELSERVER")
	got := drainNotices(c, out)
	if len(got) != 1 || !strings.Contains(got[0], "Usage: DelServer") {
		t.Fatalf("no arg: %v", got)
	}

	handleLine(t, c, "DELSERVER irc.example.org")
	got = drainNotices(c, out)
	if len(got) != 1 || !strings.Contains(got[0], "don't have any servers") {
		t.Fatalf("empty list: %v", got)
	}

	// Two servers, so removing one leaves one and the "not found" branch is
	// reachable: with an empty list ZNC answers "You don't have any servers
	// added." *before* parsing the argument, which the block above pins.
	net.AddServer("irc.example.org:6697")
	net.AddServer("irc.other.org:6697")
	handleLine(t, c, "DELSERVER irc.example.org 6697")
	got = drainNotices(c, out)
	if len(got) != 1 || !strings.Contains(got[0], "Server removed") {
		t.Fatalf("removed: %v", got)
	}
	if servers := net.Servers(); len(servers) != 1 || servers[0] != "irc.other.org:6697" {
		t.Fatalf("servers after del = %v", servers)
	}

	handleLine(t, c, "DELSERVER irc.example.org 6697")
	got = drainNotices(c, out)
	if len(got) != 1 || !strings.Contains(got[0], "No such server") {
		t.Fatalf("not found: %v", got)
	}
}

// TestMoveChanCommand pins the 1-based wire index and the two error sentences.
func TestMoveChanCommand(t *testing.T) {
	net := newFakeNet("TestNet")
	net.chans = []string{"#a", "#b", "#c"}
	c, out := serverCmdFixture(t, net)

	handleLine(t, c, "MOVECHAN #c 1")
	got := drainNotices(c, out)
	if len(got) != 1 || !strings.Contains(got[0], "Moved channel #c to index 1") {
		t.Fatalf("move: %v", got)
	}
	if net.chans[0] != "#c" {
		t.Fatalf("order after move: %v", net.chans)
	}

	handleLine(t, c, "MOVECHAN #a 99")
	got = drainNotices(c, out)
	if len(got) != 1 || !strings.Contains(got[0], "Invalid index") {
		t.Fatalf("bad index: %v", got)
	}

	handleLine(t, c, "MOVECHAN #zzz 2")
	got = drainNotices(c, out)
	if len(got) != 1 || !strings.Contains(got[0], "You are not on #zzz") {
		t.Fatalf("unknown chan: %v", got)
	}

	handleLine(t, c, "MOVECHAN #a")
	got = drainNotices(c, out)
	if len(got) != 1 || !strings.Contains(got[0], "Usage: MoveChan") {
		t.Fatalf("usage: %v", got)
	}
}

// TestMoveNetworkIsRefused: ZNC's MOVENETWORK moves a network between *users*,
// and salotto is single-user. A usage sentence would imply the command works if
// spelled right, so the answer says what is actually true.
func TestMoveNetworkIsRefused(t *testing.T) {
	net := newFakeNet("TestNet")
	got := runServerCmd(t, net, "MOVENETWORK old new")
	if len(got) != 1 || !strings.Contains(got[0], "single-user") {
		t.Fatalf("MoveNetwork: %v", got)
	}
}

// TestZNCServerParse pins the parser directly, including the case where the host
// already carries a port.
func TestZNCServerParse(t *testing.T) {
	cases := []struct{ in, want string }{
		{"irc.example.org", "irc.example.org:6667"},
		{"irc.example.org 6697", "irc.example.org:6697"},
		{"irc.example.org +6697", "irc.example.org:6697"},
		{"irc.example.org 6697 secret", "irc.example.org:6697"},
		{"irc.example.org:7000", "irc.example.org:7000"},
	}
	for _, tc := range cases {
		got, ok := zncParseServer(tc.in)
		if !ok || got != tc.want {
			t.Fatalf("zncParseServer(%q) = %q/%v, want %q", tc.in, got, ok, tc.want)
		}
	}
	if _, ok := zncParseServer("   "); ok {
		t.Fatal("blank argument parsed as a server")
	}
}
