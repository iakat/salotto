package ui

import (
	"strings"
	"testing"
)

// These three shapes were all drawn as raw wire text, because the switch had no case for them and
// everything else falls through to "verb + params" in status. Live, that is what a user saw:
//
//	status   alice NICK alice2
//	status   alice AWAY :gone to lunch
//
// - the protocol talking to itself. Both are news about a person, and news about a person belongs
// where the person was being read.

// joined puts our own nick in a channel, which is what makes the channel known to the transport:
// a message about a channel we are not in is not our conversation and draws nothing.
func joined(t *testing.T, tr *Transport, channels ...string) {
	t.Helper()
	for _, ch := range channels {
		tr.handle([]byte(":alice!a@h JOIN " + ch + "\r\n"))
	}
	for {
		if _, ok := nextIncoming(t, tr); !ok {
			break
		}
	}
}

// feedAll pushes every wire line through a transport and collects what comes out.
func feedAll(t *testing.T, tr *Transport, lines ...string) []Line {
	t.Helper()
	for _, l := range lines {
		tr.handle([]byte(l))
	}
	var out []Line
	for {
		line, ok := nextIncoming(t, tr)
		if !ok {
			return out
		}
		out = append(out, Line(line))
	}
}

// TestANickChangeIsSaidInWords is the defect for NICK.
func TestANickChangeIsSaidInWords(t *testing.T) {
	tr := newTestTransport(t)
	joined(t, tr, "#salotto")
	tr.handle([]byte(":bob!b@h JOIN #salotto\r\n"))
	for {
		if _, ok := nextIncoming(t, tr); !ok {
			break
		}
	}

	lines := feedAll(t, tr, ":bob!b@h NICK boberta\r\n")
	if len(lines) == 0 {
		t.Fatal("a nick change drew nothing")
	}
	var found *Line
	for i := range lines {
		if lines[i].Kind == LineNick {
			found = &lines[i]
		}
	}
	if found == nil {
		t.Fatalf("no rename line; got %s", dumpLines(lines))
	}
	if found.Text != "is now known as boberta" {
		t.Fatalf("the rename says %q; it must say what happened, not repeat the wire", found.Text)
	}
	if found.Nick != "bob" {
		t.Fatalf("the rename is attributed to %q, want the old nick", found.Nick)
	}
}

// TestANickChangeIsNewsInEveryChannelTheyWereIn: one departure, several windows - the same rule
// QUIT follows, and for the same reason: that is where the user was reading them.
func TestANickChangeIsNewsInEveryChannelTheyWereIn(t *testing.T) {
	tr := newTestTransport(t)
	joined(t, tr, "#salotto", "#dev")
	for _, ch := range []string{"#salotto", "#dev"} {
		tr.handle([]byte(":bob!b@h JOIN " + ch + "\r\n"))
	}
	for {
		if _, ok := nextIncoming(t, tr); !ok {
			break
		}
	}

	lines := feedAll(t, tr, ":bob!b@h NICK boberta\r\n")
	windows := map[string]bool{}
	for _, l := range lines {
		if l.Kind == LineNick {
			windows[winName(l.Window)] = true
		}
	}
	if !windows["#salotto"] || !windows["#dev"] {
		t.Fatalf("the rename reached %v, want both channels", windows)
	}
}

// TestOurOwnNickChangeSaysSo: "you are now known as" is the one place the second person is right.
func TestOurOwnNickChangeSaysSo(t *testing.T) {
	tr := newTestTransport(t)
	lines := feedAll(t, tr, ":alice!a@h NICK alice2\r\n")
	var found *Line
	for i := range lines {
		if lines[i].Kind == LineNick {
			found = &lines[i]
		}
	}
	if found == nil {
		t.Fatalf("our own rename drew no LineNick; got %s", dumpLines(lines))
	}
	if !strings.HasPrefix(found.Text, "you are now known as ") {
		t.Fatalf("our own rename says %q", found.Text)
	}
}

// TestAWayIsDrawnAsPresence is the defect for AWAY. away-notify is in the CAP request, so the
// server volunteers these and an unhandled verb means the raw line lands in status.
func TestAWayIsDrawnAsPresence(t *testing.T) {
	tr := newTestTransport(t)
	joined(t, tr, "#salotto")
	tr.handle([]byte(":bob!b@h JOIN #salotto\r\n"))
	for {
		if _, ok := nextIncoming(t, tr); !ok {
			break
		}
	}

	lines := feedAll(t, tr, ":bob!b@h AWAY :gone to lunch\r\n")
	if len(lines) == 0 {
		t.Fatal("AWAY drew nothing")
	}
	if lines[0].Kind != LineAway {
		t.Fatalf("AWAY was drawn as kind %v", lines[0].Kind)
	}
	if lines[0].Text != "is away (gone to lunch)" {
		t.Fatalf("AWAY says %q", lines[0].Text)
	}
	if winName(lines[0].Window) != "#salotto" {
		t.Fatalf("AWAY landed in %q, want the channel bob is in", lines[0].Window)
	}
}

// TestComingBackSaysSo is the negative control for AWAY: an empty reason is "back", not "away ()".
func TestComingBackSaysSo(t *testing.T) {
	tr := newTestTransport(t)
	joined(t, tr, "#salotto")
	tr.handle([]byte(":bob!b@h JOIN #salotto\r\n"))
	for {
		if _, ok := nextIncoming(t, tr); !ok {
			break
		}
	}
	lines := feedAll(t, tr, ":bob!b@h AWAY\r\n", ":bob!b@h AWAY :\r\n")
	if len(lines) < 2 {
		t.Fatalf("coming back drew %d lines", len(lines))
	}
	for _, l := range lines {
		if l.Text != "is back" {
			t.Fatalf("coming back says %q", l.Text)
		}
	}
}

// TestAnInviteIsAMention: an invite's whole value is that it is noticed, so it is highlighted and
// it goes to status rather than to a channel the user may not be watching.
func TestAnInviteIsAMention(t *testing.T) {
	tr := newTestTransport(t)
	lines := feedAll(t, tr, ":bob!b@h INVITE alice :#secret\r\n")
	if len(lines) != 1 {
		t.Fatalf("the invite drew %d lines", len(lines))
	}
	if lines[0].Window != "status" {
		t.Fatalf("the invite landed in %q, want status", lines[0].Window)
	}
	if !lines[0].Mention {
		t.Fatal("the invite is not a mention, so it will not be noticed")
	}
	// The whole sentence, not a substring: "invites you to :#secret" contains "#secret" too, and
	// that is exactly the form a missing colon-strip produces. A Contains check would have
	// passed on the broken line.
	if lines[0].Text != "invites you to #secret" {
		t.Fatalf("the invite says %q, want the channel without its wire colon", lines[0].Text)
	}
}

// TestOurOwnJoinCountsAsMembership is the defect the rename tests uncovered: our own JOIN made a
// window but not a member, so knowsChannel answered false and every later join in that channel was
// dropped as "somebody else's channel". The symptom was a nicklist that never grew and a client
// that knew nothing about anyone: bob could join, rename and go away, and all of it was invisible.
func TestOurOwnJoinCountsAsMembership(t *testing.T) {
	tr := newTestTransport(t)
	joined(t, tr, "#salotto")

	if !tr.knowsChannel("#salotto") {
		t.Fatal("our own join did not make the channel known, so later joins in it are dropped")
	}
	lines := feedAll(t, tr, ":bob!b@h JOIN #salotto\r\n")
	if len(lines) == 0 {
		t.Fatal("bob's join drew nothing; the channel we are in ignored its own traffic")
	}
	if lines[0].Kind != LineJoin || winName(lines[0].Window) != "#salotto" {
		t.Fatalf("bob's join was drawn as %+v", lines[0])
	}
}
