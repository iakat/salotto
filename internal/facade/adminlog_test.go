package facade

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestARefusalIsRecordedAsARefusal is the property the whole trail turns on.
//
// Every controlplane handler answers by writing a NOTICE, so the value they return means "the
// reply was sent", not "the command worked". An audit log built on that value would claim SHUTDOWN
// succeeded - the most alarming line it could possibly contain a false entry for.
func TestARefusalIsRecordedAsARefusal(t *testing.T) {
	s, _, sub, _ := smallRings(t)

	// SHUTDOWN is deliberately refused, and it answers with a NOTICE like everything else.
	lines := strings.Join(notices(runWindowCmd(t, s, sub, "PRIVMSG *controlplane SHUTDOWN now")), "\n")
	if !strings.Contains(lines, "remote off switch") {
		t.Fatalf("no refusal reached the client: %q", lines)
	}

	events := s.adminLog.Recent(10)
	if len(events) != 1 {
		t.Fatalf("%d events recorded, want 1: %+v", len(events), events)
	}
	e := events[0]
	if e.Action != "SHUTDOWN" {
		t.Fatalf("action recorded as %q", e.Action)
	}
	if e.OK {
		t.Fatal("a refused SHUTDOWN was recorded as accepted; this is the one entry that must never be wrong")
	}
	if e.Result == "" || e.Result == "-" {
		t.Fatal("the refusal was recorded without a reason")
	}
}

// TestAnAcceptedCommandIsRecordedWithItsAction pins the other side: a command that works is
// recorded, with the verb, the actor and an ok result.
func TestAnAcceptedCommandIsRecordedWithItsAction(t *testing.T) {
	s, _, sub, _ := smallRings(t)
	if got := strings.Join(notices(runWindowCmd(t, s, sub, "PRIVMSG *controlplane VERSION")), "\n"); !strings.Contains(got, "salotto") {
		t.Fatalf("VERSION produced no answer: %q", got)
	}
	events := s.adminLog.Recent(10)
	if len(events) != 1 {
		t.Fatalf("%d events, want 1: %+v", len(events), events)
	}
	e := events[0]
	if e.Action != "VERSION" || !e.OK {
		t.Fatalf("recorded %q ok=%v", e.Action, e.OK)
	}
	if e.Actor == "" {
		t.Fatal("no actor recorded: a trail without an actor answers nothing")
	}
	if e.At.IsZero() {
		t.Fatal("no timestamp")
	}
}

// TestAnUnknownCommandIsStillRecorded pins that a typo is not silently dropped. "Who tried what"
// is the question an audit trail is asked after an incident, and an unknown verb is an attempt.
func TestAnUnknownCommandIsStillRecorded(t *testing.T) {
	s, _, sub, _ := smallRings(t)
	got := strings.Join(notices(runWindowCmd(t, s, sub, "PRIVMSG *controlplane FROBNICATE the thing")), "\n")
	if !strings.Contains(got, "Unknown controlplane command") {
		t.Fatalf("no answer to an unknown command: %q", got)
	}
	events := s.adminLog.Recent(10)
	if len(events) != 1 {
		t.Fatalf("%d events, want 1: %+v", len(events), events)
	}
	if events[0].OK {
		t.Fatal("an unknown command was recorded as accepted")
	}
	if !strings.EqualFold(events[0].Action, "FROBNICATE") {
		t.Fatalf("action recorded as %q", events[0].Action)
	}
	if events[0].Target != "the thing" {
		t.Fatalf("the argument was not kept: %q", events[0].Target)
	}
}

// TestTheTrailSurvivesTheRingWrapping pins that the ring stays bounded and keeps the newest, so a
// long-running daemon does not grow an unbounded slice behind an admin command.
func TestTheTrailSurvivesTheRingWrapping(t *testing.T) {
	l := newAdminLog(3, "")
	for _, v := range []string{"one", "two", "three", "four", "five"} {
		l.Record("alice", v, "", "", true)
	}
	got := l.Recent(10)
	if len(got) != 3 {
		t.Fatalf("ring holds %d, want 3", len(got))
	}
	if got[0].Action != "three" || got[2].Action != "five" {
		t.Fatalf("the ring kept the wrong end: %v", []string{got[0].Action, got[1].Action, got[2].Action})
	}
}

// TestTheFileSinkActuallyWrites pins the half that outlives the process. A ring-only trail ends at
// the first restart, and the restart is often what an operator wants to know about.
func TestTheFileSinkActuallyWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin.log")
	l := newAdminLog(10, path)
	if err := l.Err(); err != nil {
		t.Fatalf("the sink failed to open: %v", err)
	}
	l.Record("alice", "TRAFFICCTL", "reset", "counters reset", true)
	l.Record("alice", "SHUTDOWN", "", "no admin authentication is configured", false)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines in the audit file, want 2:\n%s", len(lines), text)
	}
	for _, want := range []string{"actor=alice", "action=TRAFFICCTL", "result=ok", "action=SHUTDOWN", "result=refused"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the audit file is missing %q:\n%s", want, text)
		}
	}
}

// TestAnAuditLineCannotBeSplitInTwo pins the format property: a result that contains a newline must
// not become a second entry that a reader would believe.
func TestAnAuditLineCannotBeSplitInTwo(t *testing.T) {
	l := newAdminLog(10, "")
	e := l.Record("alice", "TRAFFICCTL", "a\nb\tc\rd", "line one\nline two", true)
	line := e.String()
	if strings.ContainsAny(line, "\n\r\t") {
		t.Fatalf("the rendered line carries a line break: %q", line)
	}
	// And the fields survive, flattened rather than dropped.
	if !strings.Contains(line, "line_one_line_two") {
		t.Fatalf("the result text was lost: %q", line)
	}
}

// TestABrokenSinkDoesNotStopTheBouncer pins that the audit log cannot take the service down. An
// unopenable path is reported, not fatal: a control that causes an outage is a liability, and this
// one is a bookkeeping measure with no auth behind it yet.
func TestABrokenSinkDoesNotStopTheBouncer(t *testing.T) {
	l := newAdminLog(10, "/nonexistent-dir-that-cannot-exist/admin.log")
	if l.Err() == nil {
		t.Fatal("a broken sink was not reported")
	}
	// Recording still works: the ring is the sink that cannot fail.
	l.Record("alice", "VERSION", "", "answered", true)
	if got := l.Recent(1); len(got) != 1 {
		t.Fatal("a broken file sink also stopped the ring")
	}
}

// TestAdminLogCommandShowsTheTrail pins that the trail is readable from the client, which is the
// whole point of building it into the bouncer rather than into an external log shipper.
func TestAdminLogCommandShowsTheTrail(t *testing.T) {
	s, _, sub, _ := smallRings(t)
	s.adminLog.Record("bob", "TRAFFICCTL", "", "throttled", true)

	got := strings.Join(notices(runWindowCmd(t, s, sub, "PRIVMSG *controlplane ADMINLOG")), "\n")
	if !strings.Contains(got, "TRAFFICCTL") {
		t.Fatalf("ADMINLOG showed nothing: %q", got)
	}
	if !strings.Contains(got, "bob") {
		t.Fatalf("ADMINLOG did not name the actor: %q", got)
	}
	// It also records itself, which is correct: "who read the audit log" is an auditable act.
	found := false
	for _, e := range s.adminLog.Recent(10) {
		if e.Action == "ADMINLOG" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reading the trail was not itself recorded: %+v", s.adminLog.Recent(10))
	}
}
