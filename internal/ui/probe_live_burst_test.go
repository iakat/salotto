package ui

import (
	"os"
	"strings"
	"testing"
)

// TestZZLiveBurst is driven by verify_dedup.py. It reads a registration burst captured from a
// real salottod (SALOTTO_BURST) and feeds it to a model twice, which is what a reconnect looks
// like from the model's side - then checks the scrollback did not double.
//
// It exists because the unit tests in this package build their own lines and can therefore put
// ReplayFinished, 376 and the replay in any order they like. The real daemon's order is 376
// first and the replay after it, which broke the first version of this feature invisibly. A
// fixture captured from the binary is the only version of this test that could have caught that.
//
// The name starts with ZZ so it sorts last and is skipped when the variable is absent: it is a
// probe, not part of the suite.
func TestZZLiveBurst(t *testing.T) {
	path := os.Getenv("SALOTTO_BURST")
	if path == "" {
		t.Skip("no captured burst; run scratch/verify_dedup.py")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range strings.Split(string(raw), "\n") {
		if l = strings.TrimRight(l, "\r"); l != "" {
			lines = append(lines, l)
		}
	}

	// The model a client has after its first connection: every line of the burst already drawn.
	m := New("status")
	m.connected = true
	for _, l := range lines {
		m = m.applyWire(t, l)
	}
	first := totalLines(m)
	if first == 0 {
		t.Fatalf("the burst produced no lines at all, so this probe proves nothing")
	}
	// The lines that matter are the replayed ones. The registration burst (001, 002, ... 376 and
	// the MOTD) legitimately says the same thing again on every connection, so counting the total
	// scrollback would call a correct reconnect a failure - that is what the first version of this
	// probe did.
	replayedText := map[string]int{}
	for _, l := range lines {
		if !strings.HasPrefix(l, "@") || !strings.Contains(l, "batch=") {
			continue
		}
		if i := strings.Index(l, "PRIVMSG "); i >= 0 {
			replayedText[l[i:]]++
		}
	}
	if len(replayedText) == 0 {
		t.Fatalf("the burst has no replayed PRIVMSG lines, so this probe cannot test deduplication")
	}

	// The reconnect: the same burst arrives again on a fresh connection. Everything in it is
	// either already held (the replay) or genuinely new, and the model must not double.
	m2 := m
	m2, _ = m2.Step(Disconnected{})
	m2, _ = m2.Step(Reconnecting{})
	for _, l := range lines {
		m2 = m2.applyWire(t, l)
	}
	second := totalLines(m2)

	// Each replayed message must appear exactly as many times after the reconnect as before it.
	// That is the property: the reconnect re-delivers history the client already holds, and the
	// client must not draw it twice - while the registration lines around it may of course be
	// said again.
	for text, want := range replayedText {
		before := countIn(m, text)
		after := countIn(m2, text)
		if before != want {
			t.Fatalf("%q appears %d time(s) before the reconnect, want %d: the fixture is not what this probe assumes", text, before, want)
		}
		if after != want {
			t.Fatalf("%q appears %d time(s) after replaying it %d time(s): the replay was drawn again", text, after, want)
		}
	}
	t.Logf("scrollback %d -> %d across a %d-line burst (%d replayed messages, none duplicated)",
		first, second, len(lines), len(replayedText))

	// The negative control: two identical *live* lines are both kept.
	m3 := New("status")
	m3.connected = true
	live := "@time=2026-11-24T10:00:00.000Z :alice!a@h PRIVMSG #x :same words"
	m3 = m3.applyWire(t, live)
	m3 = m3.applyWire(t, live)
	if got := totalLines(m3); got != 2 {
		t.Fatalf("two identical live lines drew %d line(s), want 2: live traffic must never be dropped", got)
	}
}

// applyWire runs one wire line through the transport's parser and the model's Update, which is
// the whole path a line takes inside a real client.
func (m Model) applyWire(t *testing.T, wire string) Model {
	t.Helper()
	tr := newTestTransport(t)
	tr.handle([]byte(wire + "\r\n"))
	var out Model = m
	for {
		select {
		case msg, ok := <-tr.Msgs():
			if !ok {
				return out
			}
			out, _ = out.Step(msg)
		default:
			return out
		}
	}
}

// countIn counts the window lines whose text contains s.
func countIn(m Model, s string) int {
	n := 0
	for _, ls := range m.windows {
		for _, l := range ls {
			if l.Text != "" && strings.Contains(s, l.Text) {
				n++
			}
		}
	}
	return n
}

// totalLines is every line in every window, which is what "the scrollback" means to a user.
func totalLines(m Model) int {
	n := 0
	for _, ls := range m.windows {
		n += len(ls)
	}
	return n
}
