package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Key helpers, so the test reads as keystrokes rather than struct literals.
func keyRune(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }
func keyEnter() tea.KeyMsg      { return tea.KeyMsg{Type: tea.KeyEnter} }
func keyCtrlN() tea.KeyMsg      { return tea.KeyMsg{Type: tea.KeyCtrlN} }

// TestProgramIsLocalWithoutTransport pins that a Program with no transport still
// works: it is a UI, not a connection.
func TestProgramIsLocalWithoutTransport(t *testing.T) {
	Configure()
	p := NewProgram("#test", nil)
	p.Step(Incoming(Line{Window: "#test", Nick: "alice", Text: "hi"}))
	if !strings.Contains(Plain(p.View()), "hi") {
		t.Fatalf("view: %q", Plain(p.View()))
	}
}

// TestProgramSubmitsThroughTheTransport is the behaviour the front ends were
// missing: pressing Enter actually sends something.
func TestProgramSubmitsThroughTheTransport(t *testing.T) {
	Configure()
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	p := NewProgram("#salotto", tr)
	for _, r := range "hello wire" {
		p.Step(keyRune(r))
	}
	p.Step(keyEnter())

	if got := fb.expect(t); got != "PRIVMSG #salotto :hello wire" {
		t.Fatalf("sent %q", got)
	}
	// The model echoed it locally, so the user sees what they typed.
	if !strings.Contains(Plain(p.View()), "hello wire") {
		t.Fatalf("no local echo: %q", Plain(p.View()))
	}
}

// TestProgramSlashCommandsGoOutAsVerbs: a /command reaches the bouncer as the
// bare verb, which is salotto's dialect.
func TestProgramSlashCommandsGoOutAsVerbs(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	p := NewProgram("status", tr)
	for _, r := range "/listnetworks" {
		p.Step(keyRune(r))
	}
	p.Step(keyEnter())

	if got := fb.expect(t); got != "LISTNETWORKS" {
		t.Fatalf("sent %q", got)
	}
}

// TestProgramSyncsActiveWindow: after switching windows the next bare line must
// go to the new one, or a user answers in the wrong channel — the worst kind of
// quiet bug in a chat client.
func TestProgramSyncsActiveWindow(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	p := NewProgram("#a", tr)
	// A line arriving in #b publishes that window, and Ctrl-N cycles to it.
	p.Step(Incoming(Line{Window: "#b", Nick: "x", Text: "from b"}))
	p.Step(keyCtrlN())
	if got := p.WindowName(); got != "#b" {
		t.Fatalf("after Ctrl-N the active window is %q, want #b", got)
	}

	for _, r := range "answer" {
		p.Step(keyRune(r))
	}
	p.Step(keyEnter())

	got := fb.expect(t)
	if !strings.HasPrefix(got, "PRIVMSG ") {
		t.Fatalf("sent %q, want a PRIVMSG", got)
	}
	if got != "PRIVMSG #b :answer" {
		t.Fatalf("line went to %q, want the window the user switched to", got)
	}
}

// TestProgramDeliversTransportMessages: Next is how the loop gets line into the
// view, and it must be the model's message type.
func TestProgramDeliversTransportMessages(t *testing.T) {
	Configure()
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	p := NewProgram("#salotto", tr)
	fb.inject(t, ":alice!a@h PRIVMSG #salotto :over the bridge")

	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("view never updated: %q", Plain(p.View()))
		default:
		}
		msg, ok := p.Next()
		if !ok {
			t.Fatal("transport closed")
		}
		p.Step(msg)
		if strings.Contains(Plain(p.View()), "over the bridge") {
			return
		}
	}
}
