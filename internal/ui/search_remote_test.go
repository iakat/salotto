package ui

import (
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// remoteSearchModel is a model that has been told a bouncer is on the other end. The message is the
// same one the transport pushes when 002 names salotto, so the test drives the production switch
// rather than setting a private field behind its back.
func remoteSearchModel(t *testing.T) Model {
	t.Helper()
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Step(Incoming{Window: "status", Nick: "srv", Text: "connected", Kind: LineSystem})
	m, _ = m.Step(Incoming{Window: "#c", Nick: "bob", Text: "a needle in memory", Kind: LineMessage})
	m, _ = m.Step(SearchAvailable{})
	return m
}

// TestTheClientFilesSearchRepliesIntoSrows: the wire form is parsed into the model's own type,
// with the window and the line a picker needs. An off-by-one in the field split puts the window
// name in the body or drops the first character of it, and the row still looks like a row.
func TestTheClientFilesSearchRepliesIntoSrows(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	fb.inject(t, "@salotto.search=9 :salotto NOTICE me :SEARCH 2026-11-20 10:00:00 libera #c <alice> hello there")
	fb.inject(t, "@salotto.search=9 :salotto NOTICE me :SEARCH-END 1")

	msg := nextSearchResults(t, tr)
	if msg.ID != 9 {
		t.Fatalf("the answer carried id %d, want 9", msg.ID)
	}
	if msg.Err != "" {
		t.Fatalf("the answer failed: %q", msg.Err)
	}
	if len(msg.Results) != 1 {
		t.Fatalf("%d results, want 1", len(msg.Results))
	}
	hit := msg.Results[0]
	if hit.Window != "#c" {
		t.Fatalf("window = %q, want #c", hit.Window)
	}
	if hit.Network != "libera" {
		t.Fatalf("network = %q, want libera", hit.Network)
	}
	if hit.Date != "2026-11-20" || hit.Time != "10:00:00" {
		t.Fatalf("date/time = %q %q", hit.Date, hit.Time)
	}
	if hit.Line.Nick != "alice" || hit.Line.Text != "hello there" {
		t.Fatalf("line = <%s> %q, want <alice> %q", hit.Line.Nick, hit.Line.Text, "hello there")
	}
	if hit.Line.Time.IsZero() {
		t.Fatal("the hit has no timestamp, so the list cannot say when it happened")
	}
}

// TestANoticeThatMerelyStartsWithSearchIsNotSwallowed: the reply tag, not the first word, is what
// marks an answer as ours. A person or a plain server can write a NOTICE whose text begins
// "SEARCH"; treating the text as the marker would silently delete a real message, which is worse
// than showing a protocol line that was not meant for the picker.
func TestANoticeThatMerelyStartsWithSearchIsNotSwallowed(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	fb.inject(t, ":alice!a@host NOTICE #c :SEARCH is a word we use")

	deadline := time.After(3 * time.Second)
	for {
		select {
		case msg, ok := <-tr.Msgs():
			if !ok {
				t.Fatal("the transport closed before the notice arrived")
			}
			in, ok := msg.(Incoming)
			if ok && strings.Contains(in.Text, "SEARCH is a word we use") {
				return
			}
		case <-deadline:
			t.Fatal("a plain NOTICE beginning with SEARCH was swallowed as a search reply")
		}
	}
}

// nextSearchResults waits for one SearchResults message, ignoring anything else the transport may
// have pushed first.
func nextSearchResults(t *testing.T, tr *Transport) SearchResults {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case msg, ok := <-tr.Msgs():
			if !ok {
				t.Fatal("transport closed before a search answer arrived")
			}
			if res, ok := msg.(SearchResults); ok {
				return res
			}
		case <-deadline:
			t.Fatal("no SearchResults arrived")
		}
	}
}

// TestALateSearchReplyIsDropped: a second search supersedes the first, and the first answer must
// not open a picker when it lands afterwards. Comparing always-true (or comparing the query text,
// which the wire does not echo) leaves the user looking at a list from a question they replaced -
// worse than waiting, because it looks current.
func TestALateSearchReplyIsDropped(t *testing.T) {
	m := remoteSearchModel(t)

	m, _ = typeLineStep(m, "/search first")
	first := m.searchSeq
	m, _ = typeLineStep(m, "/search second")
	second := m.searchSeq
	if first == 0 || second == first {
		t.Fatalf("the two requests did not get distinct ids: %d then %d", first, second)
	}

	m, _ = m.Step(SearchResults{ID: first, Results: []SearchResult{{
		Window: "#c",
		Line:   Line{Nick: "bob", Text: "old answer", Kind: LineMessage},
	}}})
	if len(m.searchHits) != 0 {
		t.Fatalf("a superseded answer opened a picker with %d hits", len(m.searchHits))
	}
	if !m.searchPending {
		t.Fatal("dropping a late answer also dropped the pending state of the current one")
	}

	m, _ = m.Step(SearchResults{ID: second, Results: []SearchResult{{
		Window: "#c",
		Line:   Line{Nick: "bob", Text: "current answer", Kind: LineMessage},
	}}})
	if len(m.searchHits) != 1 || m.searchHits[0].Line.Text != "current answer" {
		t.Fatalf("the current answer did not fill the picker: %+v", m.searchHits)
	}
}

// TestSearchWithoutABouncerStillScansMemory: with no transport there is no index to ask, and the
// model must behave exactly as it did before the wire was added - synchronous, local, and no
// "searching…" state that can never be resolved.
func TestSearchWithoutABouncerStillScansMemory(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Step(Incoming{Window: "#c", Nick: "bob", Text: "a needle in memory", Kind: LineMessage})

	next, cmd := typeLineStep(m, "/search needle")
	if cmd != nil {
		t.Fatal("the local fallback produced a command for a wire that is not there")
	}
	if len(next.searchHits) != 1 {
		t.Fatalf("the local scan found %d hits, want 1", len(next.searchHits))
	}
	if next.searchPending {
		t.Fatal("the local scan left the model waiting for a bouncer answer")
	}
	if next.searchHits[0].Window != "#c" || !strings.Contains(next.searchHits[0].Line.Text, "needle") {
		t.Fatalf("the local hit is wrong: %+v", next.searchHits[0])
	}
}

// TestSearchRowsNameTheNetworkWhenTwoWindowsShareAName: two networks can both hold "#c", and a
// list that prints "#c" twice is a list where the user cannot tell which row they are choosing.
// The jump must also use the row's network rather than whichever "#c" happens to be open, or the
// label would name one server and the jump land on another.
func TestSearchRowsNameTheNetworkWhenTwoWindowsShareAName(t *testing.T) {
	m := remoteSearchModel(t)
	m, _ = typeLineStep(m, "/search needle")
	id := m.searchSeq
	m, _ = m.Step(SearchResults{ID: id, Results: []SearchResult{
		{Window: "#c", Network: "libera", Line: Line{Nick: "a", Text: "one"}},
		{Window: "#c", Network: "oftc", Line: Line{Nick: "b", Text: "two"}},
	}})

	if len(m.searchHits) != 2 {
		t.Fatalf("got %d hits, want 2", len(m.searchHits))
	}
	if m.searchHits[0].Window != "#c (libera)" || m.searchHits[1].Window != "#c (oftc)" {
		t.Fatalf("duplicate window names are indistinguishable: %q and %q",
			m.searchHits[0].Window, m.searchHits[1].Window)
	}

	m = m.jumpToHit(m.searchHits[1])
	if m.active != "oftc/#c" {
		t.Fatalf("the oftc hit jumped to %q, want oftc/#c", m.active)
	}
}

// TestAfterADisconnectSearchFallsBackToMemory: the bouncer's index is on the other end of a socket
// that is gone, so the model must stop asking for it. Leaving searchRemote set after a drop is the
// failure mode where every /search says "searching…" forever while the render loop keeps drawing,
// which looks like a slow disk rather than a missing connection.
func TestAfterADisconnectSearchFallsBackToMemory(t *testing.T) {
	m := remoteSearchModel(t)
	m, _ = m.Step(Disconnected{})

	next, cmd := typeLineStep(m, "/search needle")
	if cmd != nil {
		t.Fatal("a disconnected client still asked the missing bouncer for its index")
	}
	if len(next.searchHits) != 1 {
		t.Fatalf("the fallback scan found %d hits, want the one in memory", len(next.searchHits))
	}
	if next.searchPending {
		t.Fatal("the fallback left a pending remote search behind")
	}
}

// TestSearchDoesNotBlockTheRenderLoop: /search returns before the bouncer has scanned anything. The
// model shows the pending row, holds no hits yet, and the work is a message for the transport -
// calling Query inline would freeze every keystroke behind a disk walk.
func TestSearchDoesNotBlockTheRenderLoop(t *testing.T) {
	m := remoteSearchModel(t)

	next, cmd := typeLineStep(m, "/search needle")
	if cmd == nil {
		t.Fatal("/search scanned synchronously instead of asking the bouncer")
	}
	if len(next.searchHits) != 0 {
		t.Fatalf("/search filled %d hits before any answer arrived", len(next.searchHits))
	}
	if !next.searchPending {
		t.Fatal("/search did not mark the request as waiting for an answer")
	}
	if body := strings.Join(next.bodyLines(60, 12), "\n"); !strings.Contains(body, "searching") {
		t.Fatalf("the pending row is not on screen: %q", body)
	}

	msg, ok := cmd().(SearchWanted)
	if !ok {
		t.Fatalf("the command produced %T, want SearchWanted", cmd())
	}
	if msg.ID != next.searchSeq || msg.Query != "needle" {
		t.Fatalf("SearchWanted = %+v, want id %d and query needle", msg, next.searchSeq)
	}
	// The id has to reach the wire, or the answer comes back without one and every reply is
	// indistinguishable from a stale one.
	wire := searchWireLine(msg)
	if !strings.Contains(wire, "id:"+strconv.Itoa(msg.ID)+" ") {
		t.Fatalf("the request line %q does not carry the request id", wire)
	}
}
