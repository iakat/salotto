package main

import "testing"

// chansaver's job is to follow our own JOIN and PART, and "our own" is harder than it
// looks once a nick can move. The deployed fixture has two bouncers on one IRCd, so
// they trade the nick: salotto registers as "e2e", loses it, falls back to "e2e_", and
// the JOIN answering its own rejoin can arrive as ":e2e" — a nick it held a moment ago.
//
// Comparing against one current nick recorded nothing in that case, so the module was
// silently dead: the set stayed as the config list, and a channel joined at runtime
// was never re-joined. Every unit test passed because its fake network reported one
// stable nick.
func TestChansaverAcceptsAnyNickWeMayHold(t *testing.T) {
	c := newChannelSet([]string{"#static"})

	// The JOIN arrives under the configured nick while the live one is the alt — the
	// exact shape measured on the deployed pair, with the extended-join suffix the
	// bouncer adds.
	line := []byte("@msgid=x;time=2026-11-23T18:25:25.755Z " +
		":e2e!~u@2wtvy4qsijvvq.irc JOIN #e2e-keep58322 * :e2e salotto")
	own := []string{"e2e_", "e2e"} // live first, then what the config asks for
	if !c.noteSelfJoinPartAny(line, own) {
		t.Fatalf("the JOIN under %q was not recorded; list=%v", "e2e", c.List())
	}

	// A gave-up nick must not be accepted as ours: another client's JOIN is not ours
	// and recording it would re-join a channel we were never in.
	other := []byte(":stranger!u@h JOIN #not-mine")
	if c.noteSelfJoinPartAny(other, own) {
		t.Fatalf("a stranger's JOIN was recorded as ours; list=%v", c.List())
	}

	// And the plain single-nick form still works, for callers with a stable nick.
	if !c.noteSelfJoinPart([]byte(":e2e_!u@h JOIN #plain"), "e2e_") {
		t.Fatal("the single-nick form stopped working")
	}
}

// A PART under any of our nicks must evict the channel: the reconnect must not put
// back a channel we deliberately left.
func TestChansaverPartRemovesUnderAnyNick(t *testing.T) {
	c := newChannelSet([]string{"#a"})
	c.Add("#b")
	if !c.noteSelfJoinPartAny([]byte(":e2e!u@h PART #b"), []string{"e2e_", "e2e"}) {
		t.Fatal("PART under a nick we hold was ignored")
	}
	for _, ch := range c.List() {
		if ch == "#b" {
			t.Fatalf("#b came back after our own PART; list=%v", c.List())
		}
	}
}
