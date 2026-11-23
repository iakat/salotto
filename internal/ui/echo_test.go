package ui

import (
	"bufio"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// The two bugs a person saw first, and neither had a test.

// TestOwnEchoIsNotDrawnTwice is the duplicate-message bug.
//
// A bouncer echoes the sender's own line - that is the whole point of a bouncer, a second
// client has to see what the first one said - so an optimistic local echo plus the real
// echo shows every message twice. The user typed "hello" and saw "hello hello".
func TestOwnEchoIsNotDrawnTwice(t *testing.T) {
	Configure()
	m := New("#test")

	// The optimistic line, drawn the instant the user hits enter.
	m, _ = m.Step(Sent{Text: "hello there"})
	// And the bouncer's authoritative echo of the same line.
	m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "alice", Text: "hello there", Kind: LineMessage, Own: true}))

	if got := strings.Count(plainText(m.View()), "hello there"); got != 1 {
		t.Fatalf("the message is on screen %d times, want 1:\n%s", got, plainText(m.View()))
	}
}

func TestTheEchoGivesTheLineItsRealNick(t *testing.T) {
	// Replacing rather than appending is what lets the line carry the nick the bouncer
	// reports. Otherwise our own messages stay attributed to "me" forever, which is wrong
	// in every log and every scrollback search.
	Configure()
	m := New("#test")
	m, _ = m.Step(Sent{Text: "hi"})
	m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "alice", Text: "hi", Kind: LineMessage, Own: true}))

	lines := m.windows["#test"]
	if len(lines) != 1 {
		t.Fatalf("%d lines, want 1 (the echo replaces the pending one)", len(lines))
	}
	if lines[0].Nick != "alice" {
		t.Fatalf("nick = %q, want alice", lines[0].Nick)
	}
	if lines[0].Pending {
		t.Fatal("the confirmed line is still marked pending")
	}
}

func TestSendingTheSameThingTwiceStillShowsTwice(t *testing.T) {
	// The wrong way to fix the duplicate bug is to drop any line whose text matches
	// something already in the window. Saying "yes" twice is one of the most common things
	// people do in a chat client.
	Configure()
	m := New("#test")
	m, _ = m.Step(Sent{Text: "yes"})
	m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "alice", Text: "yes", Kind: LineMessage, Own: true}))
	m, _ = m.Step(Sent{Text: "yes"})
	m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "alice", Text: "yes", Kind: LineMessage, Own: true}))

	if got := strings.Count(plainText(m.View()), "yes"); got != 2 {
		t.Fatalf("a repeated message is shown %d times, want 2:\n%s", got, plainText(m.View()))
	}
}

func TestSomeoneElsesLineIsNotSwallowed(t *testing.T) {
	// The confirmation must only match a line we sent. If it matched on text alone, the
	// first person to repeat what we said would replace our message.
	Configure()
	m := New("#test")
	m, _ = m.Step(Sent{Text: "hello"})
	m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "bob", Text: "hello", Kind: LineMessage}))

	lines := m.windows["#test"]
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2: bob's line is not our echo", len(lines))
	}
	if lines[0].Nick != "me" || lines[1].Nick != "bob" {
		t.Fatalf("nicked wrong: %q then %q", lines[0].Nick, lines[1].Nick)
	}
}

func TestEchoInAnotherWindowDoesNotConfirmHere(t *testing.T) {
	// The bouncer can route an echo to a different window than the one that was active
	// when the user typed (a /msg, a window switch mid-flight). Confirming across windows
	// would delete a message from a window the user is reading.
	Configure()
	m := New("#a")
	m, _ = m.Step(Sent{Text: "hello"})
	m, _ = m.Step(Incoming(Line{Window: "#b", Nick: "alice", Text: "hello", Kind: LineMessage, Own: true}))

	if got := len(m.windows["#a"]); got != 1 {
		t.Fatalf("the echo in another window removed the pending line: %d lines in #a", got)
	}
	if got := len(m.windows["#b"]); got != 1 {
		t.Fatalf("the echo was not added to its own window: %d lines in #b", got)
	}
}

// TestTheTransportMarksOurOwnLines is where Own comes from. The model cannot decide it:
// only the transport knows which nick the bouncer gave us, and a bouncer can hold a nick
// and an altnick at once. This exercises the real parse path with a fake bouncer.
func TestTheTransportMarksOurOwnLines(t *testing.T) {
	Configure()
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	tr.Register("alice", "")
	fb.inject(t, ":alice!a@host PRIVMSG #test :from me")
	fb.inject(t, ":bob!b@host PRIVMSG #test :from bob")

	own := 0
	theirs := 0
	deadline := time.After(5 * time.Second)
	for own+theirs < 2 {
		select {
		case msg, ok := <-tr.Msgs():
			if !ok {
				t.Fatal("transport closed")
			}
			inc, isIncoming := msg.(Incoming)
			if !isIncoming {
				continue
			}
			if inc.Own {
				own++
			} else {
				theirs++
			}
		case <-deadline:
			t.Fatalf("own=%d theirs=%d after 5s", own, theirs)
		}
	}
	if own != 1 || theirs != 1 {
		t.Fatalf("own=%d theirs=%d, want 1 and 1", own, theirs)
	}
}

// TestTheChildRegistersWithTheBouncer is the bug with no bug report: the terminal client
// dialled the bouncer and never registered, so the bouncer held a connection that was not
// a user. No channel list, no nick list, an empty status window - and nothing anywhere
// saying why, because a registered and an unregistered client look the same from the UI.
//
// The web front end did this in its own dial before the PTY bridge moved the dial into the
// child. Moving one without the other is exactly the kind of omission a test has to close.
func TestTheChildRegistersWithTheBouncer(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go binary not available")
	}
	dir := t.TempDir()
	tui := filepath.Join(dir, "salotto-tui")
	build := exec.Command("go", "build", "-o", tui, "github.com/iakat/salotto/cmd/salotto-tui")
	build.Env = append(build.Environ(), "CGO_ENABLED=0")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, b)
	}

	// A real listener that records the first lines the client sends. No pty: this is about
	// what the client says on the wire, and a terminal would only add noise.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	lines := make(chan string, 32)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		sc := bufio.NewScanner(c)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			default:
			}
		}
	}()

	cmd := exec.Command(tui, "-bouncer", ln.Addr().String(), "-nick", "alice")
	// The pty package gives it a terminal, so bubbletea starts; the assertions below only
	// read what it wrote to the socket.
	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		_ = ptmx.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	// Drain the terminal so the child never blocks on a full pty buffer.
	go func() {
		buf := make([]byte, 32<<10)
		for {
			if _, err := ptmx.Read(buf); err != nil {
				return
			}
		}
	}()

	// NICK and USER are the registration. Without them the bouncer has a socket and
	// nothing else.
	seenNick, seenUser := false, false
	deadline := time.After(20 * time.Second)
	for !seenNick || !seenUser {
		select {
		case l := <-lines:
			if strings.HasPrefix(l, "NICK alice") {
				seenNick = true
			}
			if strings.HasPrefix(l, "USER ") {
				seenUser = true
			}
		case <-deadline:
			t.Fatalf("the client never registered (NICK=%v USER=%v): a bouncer holds a user only once it gets them, and until then there is no window list and no nick list", seenNick, seenUser)
		}
	}
}
