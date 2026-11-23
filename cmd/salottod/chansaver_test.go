package main

import (
	"reflect"
	"testing"
)

// TestChannelSetFollowsOurOwnJoinPart is chansaver's rule
// (modules/chansaver.cpp:69-92): our own JOIN adds, our own PART removes, and
// nothing else touches the list.
func TestChannelSetFollowsOurOwnJoinPart(t *testing.T) {
	c := newChannelSet([]string{"#seed"})

	if !c.noteSelfJoinPart([]byte(":me!u@h JOIN #new"), "me") {
		t.Fatal("own JOIN did not change the set")
	}
	if !c.Has("#new") {
		t.Fatal("JOIN not recorded")
	}

	// Someone else joining must not add anything.
	if c.noteSelfJoinPart([]byte(":other!u@h JOIN #theirs"), "me") {
		t.Fatal("another user's JOIN changed the set")
	}
	if c.Has("#theirs") {
		t.Fatal("another user's channel was recorded")
	}

	if !c.noteSelfJoinPart([]byte(":me!u@h PART #new :bye"), "me") {
		t.Fatal("own PART did not change the set")
	}
	if c.Has("#new") {
		t.Fatal("PART not recorded")
	}
	if !c.Has("#seed") {
		t.Fatal("PART removed the wrong channel")
	}
}

// TestChannelSetCaseInsensitive pins RFC1459 casemapping, which is what the
// facade advertises: part of #Chan is the same as part of #chan.
func TestChannelSetCaseInsensitive(t *testing.T) {
	c := newChannelSet([]string{"#Chan"})
	if !c.Has("#chan") {
		t.Fatal("case-insensitive match failed")
	}
	c.noteSelfJoinPart([]byte(":me!u@h PART #CHAN"), "me")
	if c.Has("#Chan") {
		t.Fatal("PART with different case left the channel in the set")
	}
}

// TestChannelSetIgnoresKicks pins ZNC's omission: chansaver has no kick
// handler, so being kicked leaves the channel in the list and the disable flag
// (B12) is what stops the rejoin. If this test ever starts failing because
// someone "fixed" the missing case, that is a behaviour change away from ZNC and
// should be a deliberate decision, not a bug fix.
func TestChannelSetIgnoresKicks(t *testing.T) {
	c := newChannelSet([]string{"#a"})
	if c.noteSelfJoinPart([]byte(":op!u@h KICK #a me :out"), "me") {
		t.Fatal("a kick changed the saved channel list; ZNC's chansaver does not do that")
	}
	if !c.Has("#a") {
		t.Fatal("a kick removed the channel from the list")
	}
}

// TestChannelSetListIsSorted pins that reconnect order is stable, so two dials
// of the same config send the same JOIN sequence.
func TestChannelSetListIsSorted(t *testing.T) {
	c := newChannelSet([]string{"#zeta", "#alpha", "#mike"})
	if got, want := c.List(), []string{"#alpha", "#mike", "#zeta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("List() = %v, want %v", got, want)
	}
}

// TestChannelSetIgnoresLinesWithoutOurNick: a JOIN that is not ours must be a
// no-op even when the nick is empty (before 001), which is the state the very
// first lines arrive in.
func TestChannelSetIgnoresLinesWithoutOurNick(t *testing.T) {
	c := newChannelSet(nil)
	if c.noteSelfJoinPart([]byte(":me!u@h JOIN #x"), "") {
		t.Fatal("a line was recorded before we knew our own nick")
	}
	if c.Has("#x") {
		t.Fatal("channel recorded without knowing our nick")
	}
}
