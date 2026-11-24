package ui

import "testing"

// "/msg alice are you there" is a sentence with an addressee, not a command that acts elsewhere.
// Live, it went to the wire and drew nothing at all: no window opened, no line appeared, and the
// only evidence it had been sent was that the relayed message arrived later in a window nobody had
// opened. Pressing enter and watching nothing happen is the worst of the available failures - worse
// than a duplicate, because a duplicate at least shows the user what it thinks it did.

// TestMsgOpensTheWindowAndDrawsThere is the defect.
func TestMsgOpensTheWindowAndDrawsThere(t *testing.T) {
	m := connectedModel()
	m, _ = m.Step(Sent{Text: "/msg alice are you there"})

	lines, ok := m.windows[m.keyFor("alice")]
	if !ok {
		t.Fatalf("/msg alice opened no window; windows are %v", m.order)
	}
	if len(lines) != 1 {
		t.Fatalf("the query holds %d lines, want 1", len(lines))
	}
	if lines[0].Text != "are you there" {
		t.Fatalf("the query shows %q; it must show what was said, not the command", lines[0].Text)
	}
	if lines[0].Nick != "me" {
		t.Fatalf("the line is attributed to %q", lines[0].Nick)
	}
	if !lines[0].Pending {
		t.Fatal("the echo is not pending, so the relayed copy will be drawn beside it")
	}
}

// TestMsgSwitchesToTheWindow pins the second half: the next bare line belongs to that person.
func TestMsgSwitchesToTheWindow(t *testing.T) {
	m := connectedModel()
	m, _ = m.Step(Sent{Text: "/msg alice are you there"})

	if got := winName(m.active); got != "alice" {
		t.Fatalf("the active window is %q, want alice", got)
	}
}

// TestTheInputIsClearedAfterMsg: a switch that left the command in the box would send it again.
func TestTheInputIsClearedAfterMsg(t *testing.T) {
	m := connectedModel()
	m = typeLine(m, "/msg alice are you there")
	if len(m.input) != 0 {
		t.Fatalf("the input still holds %q", string(m.input))
	}
}

// TestMsgToAChannelStaysInTheChannel: a channel is already a window, and opening a query named
// "#salotto" beside it would split the conversation in two.
func TestMsgToAChannelStaysInTheChannel(t *testing.T) {
	m := connectedModel()
	m = m.SetActive("#salotto")
	m, _ = m.Step(Sent{Text: "/msg #salotto hello all"})

	if _, ok := m.windows[m.keyFor("#salotto")]; !ok {
		t.Fatalf("the channel window vanished; windows are %v", m.order)
	}
	lines := m.windows[m.keyFor("#salotto")]
	if len(lines) != 1 || lines[0].Text != "hello all" {
		t.Fatalf("the channel holds %+v", lines)
	}
	if got := winName(m.active); got != "#salotto" {
		t.Fatalf("the active window is %q; a channel message must not switch away", got)
	}
}

// TestMsgToStatusIsNotAQuery: "*status" is the bouncer's own window. Retargeting it would open a
// query window called "*status" and, worse, eat the answer.
func TestMsgToStatusIsNotAQuery(t *testing.T) {
	m := connectedModel()
	m, _ = m.Step(Sent{Text: "/msg *status help"})

	if _, ok := m.windows[m.keyFor("*status")]; ok {
		t.Fatal("/msg *status opened a query window")
	}
}

// TestAPlainMessageStillGoesToTheActiveWindow is the negative control for the retarget: only an
// /msg moves the line, and everything else is still a message to where the user is.
func TestAPlainMessageStillGoesToTheActiveWindow(t *testing.T) {
	m := connectedModel()
	m = m.SetActive("#salotto")
	m, _ = m.Step(Sent{Text: "hello there"})

	lines := m.windows[m.keyFor("#salotto")]
	if len(lines) != 1 || lines[0].Text != "hello there" {
		t.Fatalf("the channel holds %+v", lines)
	}
	if _, ok := m.windows[m.keyFor("alice")]; ok {
		t.Fatal("a plain message opened a query window")
	}
}
