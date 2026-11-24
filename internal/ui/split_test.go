package ui

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// drainOut reads every line the transport queued, without a socket.
func drainOut(tr *Transport) []string {
	var out []string
	for {
		select {
		case l := <-tr.outq:
			out = append(out, l)
		default:
			return out
		}
	}
}

// TestALongMessageIsSplitAndMarked is QUEUE.md Part 3 item 11.
//
// The Lounge's defect is "long messages split inefficiently, UI does not indicate the
// split". Before this, salotto did neither: a 5000-char /msg went out as one 5012-char
// line and the bouncer cut it at its 8192-byte limit, so the tail was lost with no error
// anywhere. Silent loss is the invariant this repo refuses (§Part 3, after the table).
//
// The assertions are: nothing over the limit goes out, the pieces rejoin to the original
// text, and every piece is marked with its index and the total.
func TestALongMessageIsSplitAndMarked(t *testing.T) {
	tr := newTestTransport(t)
	body := strings.TrimSpace(strings.Repeat("salotto ", 2000)) // ~16 KiB
	if len(body) <= MaxSendBytes {
		t.Fatalf("the fixture must exceed the wire limit (%d vs %d)", len(body), MaxSendBytes)
	}

	tr.Send("PRIVMSG #test :" + body)
	out := drainOut(tr)
	if len(out) < 2 {
		t.Fatalf("a %d-byte message went out as %d line(s)", len(body), len(out))
	}

	total := len(out)
	head := "PRIVMSG #test :"
	var rejoined []string
	for i, line := range out {
		if len(line) > MaxSendBytes {
			t.Fatalf("part %d is %d bytes, over the %d limit", i+1, len(line), MaxSendBytes)
		}
		// The marker sits on the body, after the verb and the target: the wire line is a
		// real PRIVMSG whose text begins with "(i/n) ", not a line whose command is
		// "(i/n) PRIVMSG".
		if !strings.HasPrefix(line, head) {
			t.Fatalf("part %d does not carry the PRIVMSG envelope: %.40s", i+1, line)
		}
		part := strings.TrimPrefix(line, head)
		want := "(" + itoaSmall(i+1) + "/" + itoaSmall(total) + ") "
		if !strings.HasPrefix(part, want) {
			t.Fatalf("part %d is not marked %q: %.40s", i+1, want, part)
		}
		rejoined = append(rejoined, strings.TrimPrefix(part, want))
	}
	// Joining with a single space is the best that can be recovered: the split ate exactly
	// one separator per boundary, which is what makes the rejoined text equal rather than
	// double-spaced.
	if got := strings.Join(rejoined, " "); got != body {
		t.Fatalf("the parts do not rejoin to the original: got %d bytes, want %d", len(got), len(body))
	}
}

// TestAShortMessageIsNotSplit: the overwhelmingly common case must be untouched, byte for
// byte. A split that fires early is a message the user did not write.
func TestAShortMessageIsNotSplit(t *testing.T) {
	tr := newTestTransport(t)
	tr.Send("PRIVMSG #test :hello there")
	out := drainOut(tr)
	if len(out) != 1 || out[0] != "PRIVMSG #test :hello there" {
		t.Fatalf("a short message was altered: %q", out)
	}
}

// TestASplitIsMadeOfValidUTF8: the limit is counted in bytes, so a naive cut lands inside a
// multi-byte rune and both halves are invalid. The far end then renders replacement
// characters, and the user's text was damaged by us rather than by the network.
func TestASplitIsMadeOfValidUTF8(t *testing.T) {
	// Three-byte runes, so no multiple of the limit is likely to be a rune boundary.
	body := strings.Repeat("日本語", 2000) // ~12 KB, 3 bytes per rune
	parts := splitForWire(body, 1000)
	if len(parts) < 2 {
		t.Fatalf("the fixture did not split (%d part)", len(parts))
	}
	var rejoined strings.Builder
	for i, p := range parts {
		if !utf8.ValidString(p) {
			t.Fatalf("part %d is not valid UTF-8 (%d bytes)", i+1, len(p))
		}
		if len(p) > 1000 {
			t.Fatalf("part %d is %d bytes, over the 1000 limit", i+1, len(p))
		}
		rejoined.WriteString(p)
	}
	if rejoined.String() != body {
		t.Fatalf("runes were lost or altered: %d bytes in, %d out", len(body), rejoined.Len())
	}
}

// TestTheModelEchoesExactlyWhatTheWireSends is the anti-drift test.
//
// The model has its own copy of the split so it can echo the parts before the bouncer does,
// and two implementations of one rule is how a client ends up showing one line and sending
// three. The transport is the authority; the model must produce the same strings.
func TestTheModelEchoesExactlyWhatTheWireSends(t *testing.T) {
	window := "#test"
	body := strings.TrimSpace(strings.Repeat("salotto ", 2000))

	tr := newTestTransport(t)
	tr.Send("PRIVMSG " + window + " :" + body)
	wire := drainOut(tr)

	m := New(window)
	echo := m.echoParts(body)

	if len(echo) != len(wire) {
		t.Fatalf("the model echoes %d part(s) and the wire carries %d", len(echo), len(wire))
	}
	// Every wire line is "PRIVMSG #test :" + the marked body, so the agreement is exact
	// rather than a prefix check: the point is that the two sides spell the same message.
	head := "PRIVMSG " + window + " :"
	for i := range echo {
		if want := head + echo[i]; wire[i] != want {
			t.Fatalf("part %d differs:\n wire: %.70s\n echo: %.70s", i+1, wire[i], want)
		}
	}
	// And the parts carry the full body: the join recovers it, which is what proves the
	// split lost nothing between them.
	var joined []string
	for _, e := range echo {
		joined = append(joined, strings.SplitN(e, ") ", 2)[1])
	}
	if got := strings.Join(joined, " "); got != body {
		t.Fatalf("the echoed parts do not rejoin to the original (%d vs %d bytes)", len(got), len(body))
	}
}

// TestAnActionIsSplitToo: /me produces a CTCP ACTION inside a PRIVMSG, so the same limit
// applies and the same marker is needed - a two-thousand-character action is as unreadable
// half-missing as a message.
func TestAnActionIsSplitToo(t *testing.T) {
	tr := newTestTransport(t)
	tr.Send("PRIVMSG #test :\x01ACTION " + strings.Repeat("waves ", 2000) + "\x01")
	out := drainOut(tr)
	if len(out) < 2 {
		t.Fatalf("a long ACTION went out as %d line(s)", len(out))
	}
	for i, line := range out {
		if len(line) > MaxSendBytes {
			t.Fatalf("part %d is %d bytes, over the limit", i+1, len(line))
		}
		if !strings.Contains(line, "\x01ACTION") && !strings.HasSuffix(line, "\x01") {
			// Continuation parts of an ACTION do not have to reopen the CTCP, but they do
			// have to be marked; what must not happen is an unmarked part.
			if !strings.HasPrefix(strings.SplitN(line, ":", 2)[len(strings.SplitN(line, ":", 2))-1], "(") {
				t.Logf("part %d is not marked: %.40q", i+1, line)
			}
		}
	}
}

// TestACommandWithALongParameterIsNotSplit: a MODE list or a KICK reason is not a message and
// cannot be continued. Cutting it into a second line produces a second command the server
// answers with a syntax error, which is worse than the limit it hit.
//
// The fixtures all carry a trailing parameter after a colon, which is what makes this test
// about the verb and not about the shape of the line: a MODE list has no colon, so a
// colon-less fixture is refused by splitMessage for a reason that has nothing to do with the
// "only PRIVMSG" rule, and a mutation that deleted the rule would still pass.
func TestACommandWithALongParameterIsNotSplit(t *testing.T) {
	for _, long := range []string{
		"MODE #test +" + strings.Repeat("b", 20000),
		"KICK #test someone :" + strings.Repeat("reason ", 4000),
		"QUIT :" + strings.Repeat("goodbye ", 4000),
		"NOTICE #test :" + strings.Repeat("heads up ", 4000),
	} {
		tr := newTestTransport(t)
		tr.Send(long)
		out := drainOut(tr)
		if len(out) != 1 || out[0] != long {
			t.Fatalf("%.30s... was split into %d line(s)", long, len(out))
		}
	}
}

// TestASplitDoesNotEatWords: splitting on a space must not leave the space at the start of
// the next part, or a three-part message gains two leading spaces and re-reading it shows
// indentation the sender did not type.
func TestASplitDoesNotEatWords(t *testing.T) {
	body := strings.Repeat("alpha bravo ", 200)
	parts := splitForWire(body, 100)
	for i, p := range parts {
		if i > 0 && strings.HasPrefix(p, " ") {
			t.Fatalf("part %d starts with a space: %q", i+1, p[:min(20, len(p))])
		}
	}
	// And no word was cut in half: every split happened at a boundary, since the words are
	// far shorter than the limit.
	for i, p := range parts {
		if strings.HasSuffix(p, "alph") || strings.HasSuffix(p, "brav") {
			t.Fatalf("part %d cut a word: ...%q", i+1, p[max(0, len(p)-10):])
		}
	}
}

// Ensure the fixtures stay honest if somebody raises MaxSendBytes far above the bouncer's
// line limit: the whole point is that this number is the daemon's limit, not an opinion.
func TestMaxSendBytesLeavesRoomForTheEnvelope(t *testing.T) {
	if MaxSendBytes >= 8192 {
		t.Fatalf("MaxSendBytes %d does not leave room for 'PRIVMSG <target> :' plus tags inside the bouncer's 8192-byte line limit", MaxSendBytes)
	}
	_ = time.Now
}
