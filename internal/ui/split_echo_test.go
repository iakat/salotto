package ui

import (
	"strings"
	"testing"
)

// sendAndRelay drives one submission through the model and then relays back the lines the
// server would echo, which is how a real bouncer answers: every part comes back as its own
// PRIVMSG. It returns the model and the number of lines still pending.
//
// This is the shape of the bug the split could introduce and unit-testing the split alone
// cannot see: the model echoes three marked lines, the bouncer relays three marked
// PRIVMSGs, and if the comparison is off by one character every echo leaves a pending ghost
// on screen - the client showing six lines where the user typed one.
func sendAndRelay(t *testing.T, m Model, text string) (Model, int) {
	t.Helper()
	parts := m.echoParts(text)

	out := newTestTransport(t)
	out.Send("PRIVMSG " + m.active + " :" + text)
	wire := drainOut(out)

	next, _ := m.Update(Sent{Text: text})
	m2 := next.(Model)
	for _, w := range wire {
		_, body, _ := splitMessage(w)
		n2, _ := m2.Update(Incoming(Line{
			Window: m.active,
			Nick:   "me",
			Text:   body,
			Own:    true,
			Kind:   LineMessage,
		}))
		m2 = n2.(Model)
	}
	pending := 0
	for _, l := range m2.windows[m.active] {
		if l.Pending {
			pending++
		}
	}
	if len(parts) != len(wire) {
		t.Fatalf("echo has %d part(s) and the wire has %d", len(parts), len(wire))
	}
	return m2, pending
}

// TestEveryPartOfASplitMessageIsConfirmed: a split message must not leave optimistic ghosts
// behind. Each part is relayed back individually, so the confirmation has to match per part.
func TestEveryPartOfASplitMessageIsConfirmed(t *testing.T) {
	body := strings.TrimSpace(strings.Repeat("salotto ", 2000))
	m := New("#test")
	m2, pending := sendAndRelay(t, m, body)
	if pending != 0 {
		t.Fatalf("%d echoed part(s) were never confirmed", pending)
	}
	// And the window holds exactly the parts, not the parts plus ghosts.
	var mine []Line
	for _, l := range m2.windows["#test"] {
		if l.Own {
			mine = append(mine, l)
		}
	}
	want := len(m.echoParts(body))
	if len(mine) != want {
		t.Fatalf("the window holds %d of our lines, want %d", len(mine), want)
	}
}

// TestAShortMessageStillConfirms: the negative control. If the split logic accidentally made
// ordinary messages unconfirmable, the test above would still pass for the long case while
// every normal message grew a ghost.
func TestAShortMessageStillConfirms(t *testing.T) {
	m := New("#test")
	_, pending := sendAndRelay(t, m, "hello there")
	if pending != 0 {
		t.Fatalf("a short message was not confirmed (%d pending)", pending)
	}
}

// TestASplitActionIsWrappedInEveryPart: an ACTION is a CTCP verb inside PRIVMSG, and every
// part of it needs the \x01ACTION wrapper. A part that opens the wrapper without closing it
// makes the receiving client render the rest of the line as raw control text, and a
// continuation with no wrapper is drawn as an ordinary message by the same reader that drew
// the first part as an action.
func TestASplitActionIsWrappedInEveryPart(t *testing.T) {
	tr := newTestTransport(t)
	tr.Send("PRIVMSG #test :\x01ACTION " + strings.TrimSpace(strings.Repeat("waves ", 2000)) + "\x01")
	out := drainOut(tr)
	if len(out) < 2 {
		t.Fatalf("a long ACTION went out as %d line(s)", len(out))
	}
	for i, line := range out {
		_, body, _ := splitMessage(line)
		// Strip the marker, then the ACTION wrapper must be intact and balanced.
		if strings.HasPrefix(body, "(") {
			if j := strings.Index(body, ") "); j > 0 {
				body = body[j+2:]
			}
		}
		if !strings.HasPrefix(body, "\x01ACTION ") {
			t.Fatalf("part %d does not reopen the ACTION: %.40q", i+1, body)
		}
		if !strings.HasSuffix(body, "\x01") {
			t.Fatalf("part %d does not close the ACTION: %.40q", i+1, body)
		}
		if n := strings.Count(body, "\x01"); n != 2 {
			t.Fatalf("part %d has %d \\x01 bytes, want 2: %.40q", i+1, n, body)
		}
	}
}
