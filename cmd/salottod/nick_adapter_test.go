package main

import "testing"

// The chansaver chain is four links long and every one of them looked correct on
// its own:
//
//  1. upstream sends our own JOIN to OnLine          (measured: it does)
//  2. the channelSet records it                     (TestProbeJoinParse: it does)
//  3. JoinChannels returns the set at reconnect     (reads the same set)
//  4. the facade can tell which lines are ours      (KnownNick, via Nick())
//
// Link 4 was missing: networkAdapter did not implement Nick(), so KnownNick
// returned "" and noteSelfJoinPart bailed out on its first guard. The result was
// a chansaver that listed channels it would never re-join — visible only by
// reconnecting a deployed bouncer and watching whether the channel came back,
// which is exactly what the differential harness does and what a unit test of
// links 1-3 could not.
//
// This test covers link 4 where it can be covered cheaply: the adapter must
// implement the interface the facade uses to discover our nick.
func TestNetworkAdapterReportsItsNick(t *testing.T) {
	var a any = &networkAdapter{}
	src, ok := a.(interface{ Nick() string })
	if !ok {
		t.Fatal("networkAdapter does not implement Nick(); " +
			"facade.KnownNick will always be empty, so chansaver and the " +
			"self-kick disable can never fire")
	}
	// Only the interface membership is asserted here: calling it needs a live
	// upstream.Conn, and the point of the test is that the method exists at all.
	_ = src
}
