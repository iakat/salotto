package ui

import (
	"strings"
	"testing"
)

// A /me is a CTCP ACTION, not a message whose text starts with "/me".
//
// The bug this pins: every /me was echoed into the window as the command the user typed -
// "/me waves" - and the relayed line comes back as an ACTION with the wrapper stripped. The
// two never matched, so the echo stayed pending forever and the window showed both a
// "/me waves" ghost and the real action. It had been there since the echo was written and no
// test looked at it, because the only /me tests were about what goes on the wire.

// TestAShortMeEchoesAsAnActionNotAsTheCommand.
func TestAShortMeEchoesAsAnActionNotAsTheCommand(t *testing.T) {
	m := New("#test")
	next, _ := m.Update(Sent{Text: "/me waves"})
	m2 := next.(Model)

	var own []Line
	for _, l := range m2.windows["#test"] {
		if l.Pending || l.Own {
			own = append(own, l)
		}
	}
	if len(own) != 1 {
		t.Fatalf("a /me produced %d echoed line(s)", len(own))
	}
	if own[0].Text != "waves" {
		t.Fatalf("the echo shows %q, which is the command as typed rather than what was said", own[0].Text)
	}
	if own[0].Kind != LineAction {
		t.Fatalf("the echo has kind %v, but a relayed ACTION arrives as LineAction and confirmation compares the kind", own[0].Kind)
	}
}

// TestAMeIsConfirmedByItsRelayedAction: the point of the kind is this. The bouncer relays the
// ACTION back with the wrapper stripped; the pending echo must resolve.
func TestAMeIsConfirmedByItsRelayedAction(t *testing.T) {
	m := New("#test")
	next, _ := m.Update(Sent{Text: "/me waves"})
	m2 := next.(Model)

	relayed, _ := m2.Update(Incoming(Line{
		Window: "#test",
		Nick:   "me",
		Text:   "waves",
		Own:    true,
		Kind:   LineAction,
	}))
	m3 := relayed.(Model)
	for _, l := range m3.windows["#test"] {
		if l.Pending {
			t.Fatalf("the relayed action did not confirm the echo: %q is still pending", l.Text)
		}
	}
	if n := len(m3.windows["#test"]); n != 1 {
		t.Fatalf("the window holds %d line(s) after one /me and its relay, want 1", n)
	}
}

// TestALongMeIsSplitWithoutTheCommandText: the split path must not reintroduce the same bug in
// a new place. A long /me used to echo "(1/2) /me waves waves ..." - the command, marked.
func TestALongMeIsSplitWithoutTheCommandText(t *testing.T) {
	body := strings.TrimSpace(strings.Repeat("waves ", 2000))
	m := New("#test")
	next, _ := m.Update(Sent{Text: "/me " + body})
	m2 := next.(Model)

	var own []Line
	for _, l := range m2.windows["#test"] {
		if l.Own || l.Pending {
			own = append(own, l)
		}
	}
	if len(own) < 2 {
		t.Fatalf("a %d-char /me echoed as %d line(s)", len(body), len(own))
	}
	for i, l := range own {
		if strings.Contains(l.Text, "/me") {
			t.Fatalf("part %d echoes the command: %.40q", i+1, l.Text)
		}
		// The \x01 wrapper is wire syntax. A window that shows it is showing the protocol
		// instead of the conversation - and a received ACTION has it stripped, so a wrapper
		// in the echo is also a second way for confirmation to miss.
		if strings.Contains(l.Text, "\x01") {
			t.Fatalf("part %d shows the CTCP wrapper: %.40q", i+1, l.Text)
		}
		if l.Kind != LineAction {
			t.Fatalf("part %d has kind %v, want LineAction", i+1, l.Kind)
		}
		if i == 0 && !strings.HasPrefix(l.Text, "(1/") {
			t.Fatalf("part 1 is not marked: %.30q", l.Text)
		}
	}
	// Every part starts with the action's first word, not with the wrapper.
	for i, l := range own {
		body := l.Text
		if j := strings.Index(body, ") "); strings.HasPrefix(body, "(") && j > 0 {
			body = body[j+2:]
		}
		if !strings.HasPrefix(body, "waves") {
			t.Fatalf("part %d does not begin with the action text: %.40q", i+1, body)
		}
	}
}

// TestAParenthesisInTheTextIsNotAContinuationMarker.
//
// splitMarker exists to peel "(i/n) " off a part so the CTCP wrapper underneath can be
// stripped. It has to be sure: a user who writes "(see the logs) that was odd" is not sending
// a second of three parts, and reading the parenthesis as a marker would eat it from the
// window. The guards are the shape of the thing - digits, a slash, nothing else.
func TestAParenthesisInTheTextIsNotAContinuationMarker(t *testing.T) {
	cases := []struct{ in, mark, rest string }{
		{"(1/3) hello", "(1/3) ", "hello"},
		{"(12/99) hello", "(12/99) ", "hello"},
		// Not starting with a parenthesis but containing ") " is the case the HasPrefix guard
		// is for: without it the scan finds the ") " and calls everything before it a marker,
		// eating the first words of the message.
		{"hello) there", "", "hello) there"},
		// The case the leading "(" guard alone catches: digits and a slash, but no opening
		// parenthesis. Without the guard the scan reads "1/3" as a part number and eats it.
		{"1/3) hello", "", "1/3) hello"},
		// A parenthesis longer than any marker could be is text, not a marker: "(a very long
		// aside/1) ..." must not be peeled.
		{"(1234567890123/1) hello", "", "(1234567890123/1) hello"},
		{"(see the logs) that was odd", "", "(see the logs) that was odd"},
		{"(abc/def) hello", "", "(abc/def) hello"},
		{"(a) hello", "", "(a) hello"},
		{"hello (world)", "", "hello (world)"},
		{"", "", ""},
	}
	for _, c := range cases {
		mark, rest := splitMarker(c.in)
		if mark != c.mark || rest != c.rest {
			t.Fatalf("splitMarker(%q) = (%q, %q), want (%q, %q)", c.in, mark, rest, c.mark, c.rest)
		}
	}
}

// TestAnActionStartingWithParenthesesKeepsThem: the same guard, reached through /me, where the
// text is peeled apart for the CTCP wrapper. An action whose first word is "(sigh)" must keep
// its parenthesis in the window.
func TestAnActionStartingWithParenthesesKeepsThem(t *testing.T) {
	m := New("#test")
	next, _ := m.Update(Sent{Text: "/me (sigh) waves"})
	m2 := next.(Model)
	var own []Line
	for _, l := range m2.windows["#test"] {
		if l.Pending || l.Own {
			own = append(own, l)
		}
	}
	if len(own) != 1 {
		t.Fatalf("%d echoed line(s)", len(own))
	}
	if own[0].Text != "(sigh) waves" {
		t.Fatalf("the echo is %q, want the action text with its parentheses intact", own[0].Text)
	}
}

// TestAnOrdinaryMessageIsStillAMessage is the negative control: routing /me through the action
// path must not turn everything into an action. The cases are the near-misses that a sloppy
// prefix test would catch: the word "me" with no slash, a different command, and "/metoo" -
// which is not a verb at all and must not be read as one.
func TestAnOrdinaryMessageIsStillAMessage(t *testing.T) {
	for _, text := range []string{"me waves", "/msg #test waves", "/metoo waves", "/ME waves"} {
		m := New("#test")
		next, _ := m.Update(Sent{Text: text})
		m2 := next.(Model)
		want := LineMessage
		if text == "/ME waves" {
			// The verb is case-insensitive, like every IRC command: /ME is the same action.
			want = LineAction
		}
		for _, l := range m2.windows["#test"] {
			if l.Kind != want {
				t.Fatalf("%q was echoed as kind %v, want %v", text, l.Kind, want)
			}
		}
	}
}
