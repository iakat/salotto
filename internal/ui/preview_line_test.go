package ui

import (
	"context"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// plainText drops SGR sequences so an assertion reads the screen, not the styling. It is
// deliberately local: the external test package has its own for measuring width, and one
// helper used for two purposes would hide which one broke.
var sgrRe = regexp.MustCompile("\x1b\\[[0-9;]*m")

func plainText(s string) string { return sgrRe.ReplaceAllString(s, "") }

// Previews are lines now, not frame fields, because a terminal has no panels: the only
// thing it can show beside a message is another line. That makes the SSH front end and a
// real terminal get previews for free, which is the whole reason for the change - but it
// also means the card has to obey the rules a line obeys, and those are what these tests
// pin.

func TestAPreviewBecomesALine(t *testing.T) {
	Configure()
	m := New("#test")
	m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "alice", Text: "look https://example.com/x"}))

	m, _ = m.Step(PreviewReady{
		Window: "#test",
		URL:    "https://example.com/x",
		Title:  "An Example Page",
		Site:   "example.com",
	})

	view := plainText(m.View())
	if !strings.Contains(view, "An Example Page") {
		t.Fatalf("the card is not on screen:\n%s", view)
	}
	if !strings.Contains(view, "example.com") {
		t.Fatalf("the card does not name the site:\n%s", view)
	}
}

func TestAPreviewDoesNotCountAsUnread(t *testing.T) {
	// Nobody wants a channel to light up because a stranger's title tag was slow. A card
	// is decoration attached to a message that already counted.
	Configure()
	m := New("#here")
	m, _ = m.Step(Incoming(Line{Window: "#elsewhere", Nick: "alice", Text: "hi"}))
	before := m.Unread()["#elsewhere"]

	m, _ = m.Step(PreviewReady{Window: "#elsewhere", URL: "https://example.com", Title: "T"})

	if got := m.Unread()["#elsewhere"]; got != before {
		t.Fatalf("a preview changed the unread count from %d to %d", before, got)
	}
}

func TestAPreviewNeverBecomesTheActiveWindow(t *testing.T) {
	// A card can arrive after the user has moved on; opening a window because a fetch
	// finished would yank the view out from under them.
	Configure()
	m := New("#a")
	m, _ = m.Step(Incoming(Line{Window: "#b", Nick: "alice", Text: "hi"}))

	m, _ = m.Step(PreviewReady{Window: "#b", URL: "https://example.com", Title: "T"})

	if m.Active() != "#a" {
		t.Fatalf("a preview switched the active window to %q", m.Active())
	}
}

func TestAPreviewForAnUnknownWindowIsSafe(t *testing.T) {
	// The window can be gone: a card is fetched in the background and the user can close
	// the window before it lands.
	Configure()
	m := New("#a")
	m, _ = m.Step(PreviewReady{Window: "#gone", URL: "https://example.com", Title: "T"})
	if !strings.Contains(m.View(), "#a") {
		t.Fatal("the model broke on a preview for a window it no longer has")
	}
}

func TestPreviewTextIsOneLine(t *testing.T) {
	// A terminal's unit of display is a line. Three lines of card for one link is most of
	// a phone screen.
	cases := []struct{ title, site, url, want string }{
		{"Title", "example.com", "https://example.com/x", "Title — example.com"},
		{"Title", "", "https://example.com/x", "Title"},
		{"", "example.com", "https://example.com/x", "example.com"},
		{"", "", "https://example.com/x", "https://example.com/x"},
	}
	for _, c := range cases {
		if got := previewText(c.title, c.site, c.url); got != c.want {
			t.Errorf("previewText(%q,%q,%q) = %q, want %q", c.title, c.site, c.url, got, c.want)
		}
	}
}

// A preview arriving through the program has to reach the model. This drives the whole
// path - fetcher, cache, goroutine, Step - with a fake fetcher, because the failure worth
// catching is a card that is fetched and never shown.
func TestPreviewFromTheFetcherReachesTheModel(t *testing.T) {
	Configure()
	p := NewProgram("#test", nil)
	p.previews = NewPreviewCache(fakeFetcher{preview: Preview{
		URL: "https://example.com/x", Title: "Fetched Title", Site: "example.com", Kind: "link",
	}})
	p.Step(Incoming(Line{Window: "#test", Nick: "alice", Text: "see https://example.com/x"}))

	// Through View(), not Model(): Model() hands out the live maps, and reading them
	// from here while the fetch goroutine appends is a data race with a real crash at the
	// end of it. View() takes the lock, which is what a front end does too.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(p.View(), "Fetched Title") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the fetched card never reached the model:\n%s", p.View())
}

// TestPreviewsAreNotFetchedForOtherWindows: reading #a must not fetch every link in #b.
func TestPreviewsAreNotFetchedForOtherWindows(t *testing.T) {
	Configure()
	f := &recordingFetcher{}
	p := NewProgram("#a", nil)
	p.previews = NewPreviewCache(f)
	p.Step(Incoming(Line{Window: "#b", Nick: "alice", Text: "https://example.com/other"}))
	// Give the background fetch a chance to be wrong.
	time.Sleep(300 * time.Millisecond)
	if n := f.count(context.Background()); n != 0 {
		t.Fatalf("fetched %d links from a window the user is not looking at", n)
	}
}

type fakeFetcher struct{ preview Preview }

func (f fakeFetcher) Fetch(_ context.Context, rawURL string) (Preview, error) {
	p := f.preview
	p.URL = rawURL
	return p, nil
}

// recordingFetcher counts fetches from the goroutine that does them, so the counter is
// atomic: the test reads it while a fetch may be in flight, and an unsynchronised int
// here is a race report that has nothing to do with the model - the kind that teaches
// people to run -race less often.
type recordingFetcher struct{ n atomic.Int64 }

func (f *recordingFetcher) Fetch(_ context.Context, rawURL string) (Preview, error) {
	f.n.Add(1)
	return Preview{URL: rawURL, Title: "T", Kind: "link"}, nil
}

func (f *recordingFetcher) count(context.Context) int { return int(f.n.Load()) }
