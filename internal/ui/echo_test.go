package ui

import (
	"bufio"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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

// TestSpacesSurviveTyping is the bug that made every command arrive mangled.
//
// bubbletea reports a space as tea.KeySpace, a key type of its own, and not as a KeyRunes
// event carrying " ". A handler that only accepts KeyRunes silently drops every space:
// "/join #chan" becomes "/join#chan", "hello there" becomes "hellothere", and nothing
// errors - the wrong text just goes out. That is the worst shape an input bug can take.
func TestSpacesSurviveTyping(t *testing.T) {
	Configure()
	for _, c := range []struct {
		keys []tea.KeyMsg
		want string
	}{
		{[]tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune("join")}, {Type: tea.KeySpace}, {Type: tea.KeyRunes, Runes: []rune("#chan")}}, "join #chan"},
		{[]tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune("hello")}, {Type: tea.KeySpace}, {Type: tea.KeyRunes, Runes: []rune("there")}}, "hello there"},
		// Two spaces in a row, because a run of them is where a partial fix (only the
		// first space) shows up.
		{[]tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune("a")}, {Type: tea.KeySpace}, {Type: tea.KeySpace}, {Type: tea.KeyRunes, Runes: []rune("b")}}, "a  b"},
	} {
		m := New("#test")
		for _, k := range c.keys {
			m, _ = m.Step(k)
		}
		if got := string(m.input); got != c.want {
			t.Errorf("typed %q, want %q", got, c.want)
		}
	}
}

// TestASpaceReachesTheWire is the same bug at the other end: the input buffer being right
// is not the same as the line going out right.
func TestASpaceReachesTheWire(t *testing.T) {
	Configure()
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	tr.Register("alice", "")
	p := NewProgram("#test", tr)
	p.Step(tea.WindowSizeMsg{Width: 100, Height: 30})

	for _, ru := range "hello there" {
		if ru == ' ' {
			p.Step(tea.KeyMsg{Type: tea.KeySpace})
		} else {
			p.Step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{ru}})
		}
	}
	p.Step(tea.KeyMsg{Type: tea.KeyEnter})

	deadline := time.After(5 * time.Second)
	for {
		select {
		case line := <-fb.sent:
			if strings.Contains(line, "PRIVMSG") {
				if !strings.Contains(line, "hello there") {
					t.Fatalf("the line went out mangled: %q", line)
				}
				return
			}
		case <-deadline:
			t.Fatal("nothing was sent")
		}
	}
}

// TestTheRuntimeDoesNotEchoTwice is the duplicate bug where the unit tests were all green.
//
// Program.Update delegated to Step, and Step resolved the model's Sent command itself. Under
// a real bubbletea runtime the command is ALSO dispatched by the runtime and its message fed
// back through Update - so the local echo happened twice and every message was drawn twice.
// The headless tests never saw it because they called Step directly, where the runtime does
// not exist to double-dispatch.
func TestTheRuntimeDoesNotEchoTwice(t *testing.T) {
	Configure()
	p := NewProgram("#test", nil)
	p.Step(tea.WindowSizeMsg{Width: 100, Height: 30})

	// What a runtime does: send the key, then dispatch the returned command and feed its
	// message back in. Doing that by hand is exactly what bubbletea does internally.
	for _, ru := range "hi" {
		if _, cmd := p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{ru}}); cmd != nil {
			t.Fatalf("typing returned a command: %T", cmd)
		}
	}
	_, cmd := p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter returned no command")
	}
	// The runtime runs it and feeds the result back.
	if msg := cmd(); msg != nil {
		p.Update(msg)
	}

	if got := strings.Count(plainText(p.View()), "hi"); got != 1 {
		t.Fatalf("the local echo is on screen %d times, want 1:\n%s", got, plainText(p.View()))
	}
}

// TestABareStatusLineIsACommandAsTyped pins ZNC's behaviour and ours: in the status window
// a line with no slash is a bouncer command, so "listchans" works. It is sent as typed -
// upper-casing it turned a sentence typed in the wrong window into "421 Unknown command".
func TestABareStatusLineIsACommandAsTyped(t *testing.T) {
	Configure()
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	tr.Register("alice", "")

	tr.SetActive("status")
	// A bare line in the status window is a bouncer command, as typed. Uppercasing it was
	// what turned a stray sentence into "421 Unknown command".
	if got := tr.Command("listchans"); got != "listchans" {
		t.Fatalf("a bare status line became %q, want %q", got, "listchans")
	}

	// And in a channel it is still a message.
	tr.SetActive("#test")
	if got := tr.Command("hello there"); got != "PRIVMSG #test :hello there" {
		t.Fatalf("in a channel the line became %q", got)
	}
}

// TestJoiningSwitchesToTheChannel is the bug behind "my message disappeared".
//
// After /join the view stayed on the status window. A bare line there is a bouncer command,
// so the next thing typed went out as "HELLO" - answered with "421 Unknown command" - and
// the message the user thought they sent never existed. Every IRC client switches to the
// channel on join; not doing it made the input box lie about where it was sending.
func TestJoiningSwitchesToTheChannel(t *testing.T) {
	Configure()
	m := New("status")
	m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "alice", Text: "joined #test", Kind: LineSelfJoin}))

	if m.Active() != "#test" {
		t.Fatalf("after joining #test the active window is %q", m.Active())
	}
	if m.scroll != 0 {
		t.Fatalf("scroll = %d after joining, want the bottom", m.scroll)
	}
}

// TestSomeoneElsesJoinDoesNotSwitch is the other half: a channel filling up must not drag the
// view away from what the user is reading.
func TestSomeoneElsesJoinDoesNotSwitch(t *testing.T) {
	Configure()
	m := New("#reading")
	m, _ = m.Step(Incoming(Line{Window: "#other", Nick: "bob", Text: "joined", Kind: LineJoin}))

	if m.Active() != "#reading" {
		t.Fatalf("another person's join switched the view to %q", m.Active())
	}
}

// TestEnterPutsTheLineOnTheWire is the test the user's bug report deserved and did not get.
//
// Every earlier test checked the local echo, and the local echo is drawn by the model
// whether or not anything is sent. Under a real bubbletea runtime nothing was sent at all:
// Program.Update passed resolve=false, and resolving the model's command was the only thing
// that ever reached submit(). So the suite was green, the screen showed the message, and
// the message existed nowhere but the screen.
//
// The witness here is the socket. "hi" appearing on screen proves nothing; "PRIVMSG #test :hi"
// arriving at the other end of a TCP connection proves it left.
func TestEnterPutsTheLineOnTheWire(t *testing.T) {
	Configure()
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	p := NewProgram("#test", tr)
	p.Step(tea.WindowSizeMsg{Width: 100, Height: 30})

	// Type the way a runtime delivers it, then Enter, then run the command the way
	// bubbletea does and feed the result back. No Step() shortcut: this is Update only.
	for _, ru := range "hi" {
		if _, cmd := p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{ru}}); cmd != nil {
			t.Fatalf("typing returned a command: %T", cmd)
		}
	}
	_, cmd := p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter returned no command")
	}
	if msg := cmd(); msg != nil {
		p.Update(msg)
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case line := <-fb.sent:
			if strings.Contains(line, "NICK") || strings.Contains(line, "USER") {
				continue
			}
			if line != "PRIVMSG #test :hi" {
				t.Fatalf("sent %q, want PRIVMSG #test :hi", line)
			}
			return
		case <-deadline:
			t.Fatal("Enter drew the line on screen but sent nothing to the bouncer")
		}
	}
}

// TestABareStatusLineReachesTheWire is the same defect in the other window, and it is the
// one the user actually hit: "listchans" in the status window did nothing at all.
func TestABareStatusLineReachesTheWire(t *testing.T) {
	Configure()
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	p := NewProgram("status", tr)
	p.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	for _, ru := range "listchans" {
		p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{ru}})
	}
	_, cmd := p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter returned no command")
	}
	if msg := cmd(); msg != nil {
		p.Update(msg)
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case line := <-fb.sent:
			if strings.Contains(line, "NICK") || strings.Contains(line, "USER") {
				continue
			}
			if line != "listchans" {
				t.Fatalf("sent %q, want the verb as typed", line)
			}
			return
		case <-deadline:
			t.Fatal("the status-window command never left")
		}
	}
}

// TestMsgStatusReachesTheBouncerAsACommand is the spelling every client's /msg box produces.
//
// "/msg *status help" is how a person asks a bouncer what it can do, and if the client
// forwards it verbatim as "PRIVMSG *status :help" the user sees their own line echoed and no
// answer. The bouncer accepts the PRIVMSG form too, so both ends work - but the client test
// is on the WIRE, because the local echo looks identical either way.
func TestMsgStatusReachesTheBouncerAsACommand(t *testing.T) {
	Configure()
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	p := NewProgram("status", tr)
	p.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	line := "/msg *status help"
	for _, ru := range line {
		if ru == ' ' {
			p.Step(tea.KeyMsg{Type: tea.KeySpace})
			continue
		}
		p.Step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{ru}})
	}
	p.Step(tea.KeyMsg{Type: tea.KeyEnter})

	deadline := time.After(5 * time.Second)
	for {
		select {
		case got := <-fb.sent:
			if strings.Contains(got, "NICK") || strings.Contains(got, "USER") || strings.Contains(got, "CAP") {
				continue
			}
			if got != "help" {
				t.Fatalf("sent %q, want the bouncer command as typed", got)
			}
			return
		case <-deadline:
			t.Fatal("/msg *status help never left the client")
		}
	}
}

// TestSlashMsgInStatusGoesOutAsACommand pins a live bug: "/msg <nick> <text>" typed at the
// status window was sent as chat.
//
// The screen said so plainly - "<me> /msg salotto-probe-no-such-nick-zz9 hello" - so the slash
// had reached the model and the transport still turned the line into a PRIVMSG. The bouncer
// therefore had nothing to refuse, no 401 came back, and the error path looked broken. The
// transport has a branch for /msg; this asserts on the wire, because that is the only place
// the two spellings differ.
func TestSlashMsgInStatusGoesOutAsACommand(t *testing.T) {
	tr := newTestTransport(t)
	tr.SetActive("status")
	if got := tr.Command("/msg somebody hello there"); got != "PRIVMSG somebody :hello there" {
		t.Fatalf("status window /msg became %q", got)
	}
	// And in a channel it must be the same: /msg is addressed, not contextual.
	tr.SetActive("#chan")
	if got := tr.Command("/msg somebody hello there"); got != "PRIVMSG somebody :hello there" {
		t.Fatalf("channel window /msg became %q", got)
	}
}
