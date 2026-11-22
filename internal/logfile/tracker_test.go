package logfile

import (
	"testing"
)

func TestNewTrackerVariadic(t *testing.T) {
	tr := NewTracker("me")
	if tr.OwnNick() != "me" {
		t.Fatal("nick")
	}
	tr2 := NewTracker()
	if tr2.OwnNick() != "" {
		t.Fatal("empty")
	}
}

func TestTrackerNAMES(t *testing.T) {
	tr := NewTracker("me")
	tr.Observe([]byte(":srv 353 me = #c :@a b"))
	tr.Observe([]byte(":srv 353 me = #c :c"))
	tr.Observe([]byte(":srv 366 me #c :End"))
	m := tr.Members("#c")
	if !m["a"] || !m["b"] || !m["c"] {
		t.Fatalf("members: %v", m)
	}
}

func TestTrackerPartKickDisconnect(t *testing.T) {
	tr := NewTracker("me")
	tr.Observe([]byte(":me JOIN #c"))
	found := false
	for _, c := range tr.Channels() {
		if c == "#c" {
			found = true
		}
	}
	if !found {
		t.Fatal("channel not added")
	}
	tr.Observe([]byte(":x KICK #c me :bye"))
	for _, c := range tr.Channels() {
		if c == "#c" {
			t.Fatal("channel not removed")
		}
	}
	tr.Clear()
	if tr.OwnNick() != "" {
		t.Fatal("not cleared")
	}
}
