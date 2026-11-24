package logfile

import (
	"testing"
)

// TestTheTopicIsNotFiledUnderStatus pins that a 332 names a channel, so it is
// routed to that channel and not to the server's status window.
//
// The old rule was "a numeric is a status line", and it cost the topic itself:
// the 332 was relayed once into status and then existed only in that ring. A
// client attaching after the status ring had dropped the line saw no topic at
// all. Removing the 332 case makes WindowFor answer "not a windowed line", which
// is exactly what treating it like any other server numeric does.
func TestTheTopicIsNotFiledUnderStatus(t *testing.T) {
	tr := NewTracker("me")
	first, all, ok := WindowFor(tr, []byte(":up 332 me #c :the topic"), false, false)
	if !ok {
		t.Fatal("the 332 produced no windowed line, so it would be filed under status")
	}
	if first != "#c" {
		t.Fatalf("the 332 was filed under %q, want #c", first)
	}
	if len(all) != 1 || all[0] != "#c" {
		t.Fatalf("the 332's windows are %v, want [#c]", all)
	}

	entries := Format(tr, nil, []byte(":up 332 me #c :the topic"), false, false, "up")
	if len(entries) != 1 || entries[0].Window != "#c" {
		t.Fatalf("Format filed the 332 under %+v, want the channel window", entries)
	}
}
