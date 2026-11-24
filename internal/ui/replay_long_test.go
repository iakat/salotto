package ui

import (
	"fmt"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// A reconnect must not re-draw a window, however long the absence was.
//
// The daemon replays a window's whole ring, so the number of lines that come back is the number the
// client may already hold - there is no bound the client can assume. The reconciliation therefore
// cannot be "look in the last N lines": that was a real defect, and it hid because every test of it
// held fewer than N lines. A client away for a day came back to a window that grew, which is the
// one thing a bouncer replay exists to prevent.

// burst fills a window with n message lines, each said once, spread over time.
func burst(m Model, window string, n int) Model {
	base := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		m, _ = m.Step(Incoming(Line{
			Window: window,
			Nick:   "alice",
			Text:   fmt.Sprintf("line %d", i),
			Kind:   LineMessage,
			Time:   base.Add(time.Duration(i) * time.Second),
		}))
	}
	return m
}

// replayAll feeds the same lines back as one replay batch, the way the daemon does after a
// reconnect: same nick, same text, same order, tagged with a batch id and marked as a replay.
func replayAll(m Model, window, batch string, n int) Model {
	base := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		m, _ = m.Step(Incoming(Line{
			Window:   window,
			Nick:     "alice",
			Text:     fmt.Sprintf("line %d", i),
			Kind:     LineMessage,
			Time:     base.Add(time.Duration(i) * time.Second),
			Replayed: true,
			Batch:    batch,
		}))
	}
	return m
}

// linesIn counts the lines in a window, which is the number the user sees.
func linesIn(m Model, window string) int { return len(m.windows[window]) }

// TestALongReplayIsNotDrawnTwice is the defect: 2000 lines is four times the old 512-line window,
// so with a capped scan the first 1488 copies matched nothing and were drawn again.
func TestALongReplayIsNotDrawnTwice(t *testing.T) {
	const n = 2000
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = burst(m, "#salotto", n)
	if got := linesIn(m, "#salotto"); got != n {
		t.Fatalf("the window holds %d lines after %d arrived", got, n)
	}

	m = replayAll(m, "#salotto", "libera-#salotto-replay", n)
	if got := linesIn(m, "#salotto"); got != n {
		t.Fatalf("after replaying %d lines the window holds %d; %d were drawn twice",
			n, got, got-n)
	}
}

// TestALongReplayStillDeliversWhatWasMissed is the other half, and the reason a plain "drop
// anything that looks familiar" would be wrong: if the client was away for 2000 lines but only
// holds 1500, the 500 it never got must be drawn, even though the window is far longer than any
// window a scan would look at.
func TestALongReplayStillDeliversWhatWasMissed(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = burst(m, "#salotto", 1500)

	// The replay delivers 2000: the client holds the first 1500 and is missing the last 500.
	m = replayAll(m, "#salotto", "libera-#salotto-replay", 2000)
	if got := linesIn(m, "#salotto"); got != 2000 {
		t.Fatalf("after a 2000-line replay over a 1500-line window the client holds %d, want 2000",
			got)
	}
	// And the last line is there: the missed ones are the end of the replay, which is exactly
	// where a capped scan would have given up.
	last := m.windows["#salotto"][1999]
	if last.Text != "line 1999" {
		t.Fatalf("the last line is %q, want line 1999", last.Text)
	}
}

// TestARepeatedSentenceIsReconciledAsAMultiset, at a length the old cap could not reach: alice says
// "ok" 700 times while the client is away and holds none of them, so all 700 must be drawn.
func TestARepeatedSentenceIsReconciledAsAMultiset(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = burst(m, "#salotto", 800) // unrelated traffic, so the window is long

	// 700 identical lines arrive in one batch.
	for i := 0; i < 700; i++ {
		m, _ = m.Step(Incoming(Line{
			Window:   "#salotto",
			Nick:     "bob",
			Text:     "ok",
			Kind:     LineMessage,
			Time:     time.Now(),
			Replayed: true,
			Batch:    "b",
		}))
	}
	got := 0
	for _, l := range m.windows["#salotto"] {
		if l.Nick == "bob" && l.Text == "ok" {
			got++
		}
	}
	if got != 700 {
		t.Fatalf("the client holds %d of 700 repeated lines; a set would hold 1 and a capped scan fewer", got)
	}
}

// TestTheTallyIsPerBatchNotPerWindow: a batch that arrives twice is two events. The second copy
// over a window that already has the first is suppressed, but a *third* delivery in the same batch
// must be drawn - the window held two, so it gets two.
func TestTheTallyIsPerBatchNotPerWindow(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})

	line := func(batch string) Line {
		return Line{Window: "#salotto", Nick: "alice", Text: "hi", Kind: LineMessage,
			Time: time.Now(), Replayed: true, Batch: batch}
	}
	// The window holds two copies before any replay.
	m, _ = m.Step(Incoming(Line{Window: "#salotto", Nick: "alice", Text: "hi", Kind: LineMessage, Time: time.Now()}))
	m, _ = m.Step(Incoming(Line{Window: "#salotto", Nick: "alice", Text: "hi", Kind: LineMessage, Time: time.Now()}))

	// A batch delivering the same line twice adds nothing...
	m, _ = m.Step(Incoming(line("a")))
	m, _ = m.Step(Incoming(line("a")))
	if got := linesIn(m, "#salotto"); got != 2 {
		t.Fatalf("a batch of two over a window of two left %d lines, want 2", got)
	}

	// ...and a third delivery in the same batch is one the client never had.
	m, _ = m.Step(Incoming(line("a")))
	if got := linesIn(m, "#salotto"); got != 3 {
		t.Fatalf("the third copy was suppressed: window holds %d, want 3", got)
	}
}

// TestEachBatchStartsFromTheWindowItSees.
//
// A tally that outlived its batch would keep counting deliveries across two of them, so the second
// replay would look as though more copies had already been drawn than were ever on screen - and the
// line would be repeated. A reconnect produces one batch per window, but a client reconnecting
// twice, or a bouncer replaying after a restart, produces more.
func TestEachBatchStartsFromTheWindowItSees(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})

	// The window holds one copy of a line from before the outage.
	m, _ = m.Step(Incoming(Line{Window: "#salotto", Nick: "alice", Text: "hi", Kind: LineMessage, Time: time.Now()}))

	replayed := func(batch string) Line {
		return Line{Window: "#salotto", Nick: "alice", Text: "hi", Kind: LineMessage,
			Time: time.Now(), Replayed: true, Batch: batch}
	}

	// Two batches, each delivering that one line. Each must recognise it as already held.
	m, _ = m.Step(Incoming(replayed("first")))
	m, _ = m.Step(Incoming(replayed("second")))

	if got := linesIn(m, "#salotto"); got != 1 {
		t.Fatalf("the window holds %d copies after two batches replayed one line, want 1", got)
	}
}
