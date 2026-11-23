package facade

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// awayRecorder collects what the away policy sent, so the tests can assert on
// the wire rather than on internal flags.
type awayRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *awayRecorder) send(line string) {
	r.mu.Lock()
	r.lines = append(r.lines, line)
	r.mu.Unlock()
}

func (r *awayRecorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

// TestAwayOnlyWhenTheLastClientLeaves is simple_away's OnClientDisconnect rule
// (simple_away.cpp:126): one detach among several must not set away.
func TestAwayOnlyWhenTheLastClientLeaves(t *testing.T) {
	rec := &awayRecorder{}
	a := newAway(true, "gone", 20*time.Millisecond, rec.send)

	a.clientAttached(nil)
	a.clientDetached()
	time.Sleep(60 * time.Millisecond)
	if got := rec.got(); len(got) != 0 && got[0] != "AWAY :gone" {
		t.Fatalf("unexpected lines: %v", got)
	}

	// With the timer path exercised, the positive case: detach fires.
	rec2 := &awayRecorder{}
	b := newAway(true, "gone", 20*time.Millisecond, rec2.send)
	b.clientDetached()
	time.Sleep(80 * time.Millisecond)
	if got := rec2.got(); len(got) != 1 || got[0] != "AWAY :gone" {
		t.Fatalf("auto-away did not fire: %v", got)
	}
}

// TestAwayComesBackOnAttach pins SetBack (simple_away.cpp:196): attaching
// clears the away, but only one we set.
func TestAwayComesBackOnAttach(t *testing.T) {
	rec := &awayRecorder{}
	a := newAway(true, "gone", 10*time.Millisecond, rec.send)
	a.clientDetached()
	time.Sleep(60 * time.Millisecond)
	if len(rec.got()) != 1 {
		t.Fatalf("setup: away did not fire: %v", rec.got())
	}

	a.clientAttached(nil)
	got := rec.got()
	if len(got) != 2 || got[1] != "AWAY" {
		t.Fatalf("attach did not clear our away: %v", got)
	}

	// A second attach must not send another AWAY: we already came back.
	a.clientAttached(nil)
	if len(rec.got()) != 2 {
		t.Fatalf("duplicate AWAY on a redundant attach: %v", rec.got())
	}
}

// TestAwayDoesNotFightTheUser is the m_bClientSetAway rule
// (simple_away.cpp:189): if the user is already away by their own choice, the
// module must neither overwrite the reason nor clear it on attach.
func TestAwayDoesNotFightTheUser(t *testing.T) {
	rec := &awayRecorder{}
	a := newAway(true, "gone", 10*time.Millisecond, rec.send)

	a.noteClientAway("lunch")
	a.clientDetached()
	time.Sleep(60 * time.Millisecond)
	if got := rec.got(); len(got) != 0 {
		t.Fatalf("auto-away overwrote a user-set away: %v", got)
	}

	a.clientAttached(nil)
	if got := rec.got(); len(got) != 0 {
		t.Fatalf("attach cleared a user-set away: %v", got)
	}

	// The user coming back themselves re-arms the module.
	a.noteClientAway("")
	a.clientDetached()
	time.Sleep(60 * time.Millisecond)
	if got := rec.got(); len(got) != 1 {
		t.Fatalf("auto-away did not re-arm after the user came back: %v", got)
	}
}

// TestAwayDetachDoesNotRestartTheTimer: a flapping client must not postpone the
// away forever.
func TestAwayDetachDoesNotRestartTheTimer(t *testing.T) {
	rec := &awayRecorder{}
	a := newAway(true, "gone", 40*time.Millisecond, rec.send)
	a.clientDetached()
	time.Sleep(10 * time.Millisecond)
	a.clientDetached() // would restart a naive timer
	a.clientDetached()
	time.Sleep(90 * time.Millisecond)
	if got := rec.got(); len(got) != 1 {
		t.Fatalf("detaches restarted the timer: %v", got)
	}
}

// TestAwayDisabledNeverFires pins that the feature is off by default and stays
// off when the operator has not asked for it.
func TestAwayDisabledNeverFires(t *testing.T) {
	rec := &awayRecorder{}
	a := newAway(false, "gone", 10*time.Millisecond, rec.send)
	a.clientDetached()
	time.Sleep(60 * time.Millisecond)
	if got := rec.got(); len(got) != 0 {
		t.Fatalf("disabled away fired: %v", got)
	}
}

// TestAwayReasonExpandsTime checks the %awaytime% substitution the default
// reason uses (simple_away.cpp:13).
func TestAwayReasonExpandsTime(t *testing.T) {
	now := time.Date(2026, 11, 23, 15, 4, 5, 0, time.UTC)
	if got := expandAwayTime("Auto away at %awaytime%", now); got != "Auto away at 2026-11-23 15:04:05" {
		t.Fatalf("expansion: %q", got)
	}
	if got := expandAwayTime("plain", now); got != "plain" {
		t.Fatalf("a reason without the token was rewritten: %q", got)
	}
}

// TestAwayFansOutOverEveryNetwork: the state is the user's, so one AWAY must
// reach every network rather than only the first.
func TestAwayFansOutOverEveryNetwork(t *testing.T) {
	fn := newFakeNet("libera")
	other := newFakeNet("oftc")
	s, err := New(testConfig(fn, other))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.netSnapshot()) != 2 {
		t.Fatalf("expected two networks, got %d", len(s.netSnapshot()))
	}
	s.broadcastAway("AWAY :gone")

	for _, n := range []*fakeNet{fn, other} {
		sent := n.sent()
		if len(sent) != 1 || !strings.Contains(sent[0], "AWAY :gone") {
			t.Fatalf("%s did not receive the away: %v", n.name, sent)
		}
	}
}
