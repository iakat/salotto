package upstream

import (
	"testing"
	"time"

	"github.com/iakat/salotto/internal/logfile"
	"github.com/iakat/salotto/internal/winring"
)

// TestBuffExtrasReachesTheReplayRing is ZNC parity for its buffextras module:
// joins, parts, quits, nick changes, kicks, topic changes and mode changes must
// be in the replay buffer, not only in the log file. ZNC needs a module because
// its buffer is a message list that only CClient::AddBuffer writes; our rings are
// appended by the same routing step that writes the log, so B2's split makes it
// automatic. This test is the guard that it stays automatic.
//
// The bodies are ZNC's own buffextras strings ("{1} joined", "{1} parted: {2}",
// "{1} quit: {2}", "{1} is now known as {2}", "{1} kicked {2} with reason: {3}",
// "{1} changed the topic to: {2}", "{1} set mode: {2} {3}"), because a client
// rendering ZNC playback expects them verbatim.
func TestBuffExtrasReachesTheReplayRing(t *testing.T) {
	// A real Writer, not logfile.Route: the tracker learns channel membership
	// from the lines it observes, and a QUIT or NICK only fans out to the
	// channels the tracker already knows the nick is in.
	w := logfile.New("libera", t.TempDir(), 8, time.Now)
	t.Cleanup(w.Close)
	set := winring.New(16, 4, 4096)
	c := &Conn{cfg: Config{Windows: set, Route: w.Route}}

	// Everyone must be a member of #c before their NICK or QUIT can fan out:
	// the tracker fans those out to the channels it knows the nick is in, so an
	// unknown nick would land in status instead.
	for _, nick := range []string{"alice", "bob", "carol", "dave", "erin", "gina", "hank", "ivan"} {
		c.store([]byte(":"+nick+"!x@h JOIN #c"), false)
	}

	feed := []struct{ line, window, want string }{
		{":alice!a@h JOIN #c", "#c", "*** Joins: alice (a@h)"},
		{":bob!b@h PART #c :bye", "#c", "*** Parts: bob (b@h) (bye)"},
		{":carol!c@h NICK carol_", "#c", "*** carol is now known as carol_"},
		{":dave!d@h TOPIC #c :new topic", "#c", "*** dave changes topic to 'new topic'"},
		{":erin!e@h MODE #c +o frank", "#c", "*** erin sets mode: +o frank"},
		{":gina!g@h KICK #c hank :rude", "#c", "*** hank was kicked by gina (rude)"},
		{":ivan!i@h QUIT :gone", "#c", "*** Quits: ivan (i@h) (gone)"},
	}
	for _, f := range feed {
		if !c.store([]byte(f.line), false) {
			t.Fatalf("%q was not buffered at all", f.line)
		}
	}

	r, ok := set.Ring("#c")
	if !ok {
		t.Fatal("#c has no ring")
	}
	got := make(map[string]bool, 8)
	r.Replay(r.Oldest(), func(seq uint64, line []byte) bool {
		got[string(line)] = true
		return true
	})

	// Every feed line must be in the ring. The ring stores the wire line, not the
	// rendered log body, so the assertion is on the lines that produced an entry.
	for _, f := range feed {
		if !got[f.line] {
			t.Errorf("ring for %s is missing %q; it holds %v", f.window, f.line, keys(got))
		}
	}
	_ = time.Now
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
