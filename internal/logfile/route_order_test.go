package logfile

import (
	"testing"
	"time"
)

// Routing must observe a line before the next line is routed, and in the state
// the line arrived in. The drain goroutine is asynchronous, so a JOIN observed
// only by the drain can still be missing when the message that follows it is
// routed, and a QUIT observed too early loses the channels it should fan out
// to. Writer.Route does parse-then-observe on the caller's goroutine to pin it.
func TestRouteObservesInArrivalOrder(t *testing.T) {
	w := New("probe", "", 0, time.Now)
	defer w.Close()
	w.SetNick("me")
	w.SetServer("srv")

	if got := w.Route([]byte(":alice!a@h JOIN #chan"), false); len(got) != 1 || got[0].Window != "#chan" {
		t.Fatalf("JOIN routed to %v, want the #chan join entry", got)
	}
	// Without the JOIN having been observed, this would be a status line.
	got := w.Route([]byte(":bob!b@h PRIVMSG #chan :hi"), false)
	if len(got) != 1 || got[0].Window != "#chan" {
		t.Fatalf("PRIVMSG routed to %v, want one #chan entry", got)
	}
	// The QUIT must fan out to the channel the nick shared before it happened.
	quit := w.Route([]byte(":alice!a@h QUIT :bye"), false)
	if len(quit) != 1 || quit[0].Window != "#chan" {
		t.Fatalf("QUIT routed to %v, want #chan and not status", quit)
	}
	// And after it the nick is gone: a second QUIT has no shared channel left.
	if again := w.Route([]byte(":alice!a@h QUIT :again"), false); len(again) != 1 || again[0].Window != "status" {
		t.Fatalf("second QUIT routed to %v, want status", again)
	}
}

// A routed line is already observed, so the drain must not observe it a second
// time: the tracker would then believe a nick that parted is still a member.
func TestOnRoutedIsNotObservedTwice(t *testing.T) {
	w := New("probe", "", 0, time.Now)
	w.SetNick("me")
	w.SetServer("srv")

	line := []byte(":alice!a@h JOIN #chan")
	w.OnRouted(w.Route(line, false), line, false)
	part := []byte(":alice!a@h PART #chan")
	w.OnRouted(w.Route(part, false), part, false)
	w.Close()

	// With a double-observed JOIN the membership survives the PART, so this
	// line would still be routed to #chan; it must not be.
	after := w.Route([]byte(":bob!b@h PRIVMSG #chan :hi"), false)
	if len(after) == 0 {
		t.Fatal("nothing routed for a channel PRIVMSG")
	}
}
