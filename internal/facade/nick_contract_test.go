package facade

import "testing"

// A network that reports its nick must be visible through Server.KnownNick, which
// is how every "is this line ours?" consumer works (chansaver's JOIN/PART
// tracking, the self-kick that disables a channel). This test is here because the
// production adapter in cmd/salottod did NOT implement Nick() while every mock in
// this package did: the mock was more capable than the real thing, so nothing in
// the facade suite could notice, and the chansaver silently recorded nothing.
func TestKnownNickFollowsTheNetwork(t *testing.T) {
	f := newFakeNet("TestNet")
	f.nick = "me"
	srv, _ := startServer(t, testConfig(f))
	if got := srv.KnownNick("TestNet"); got != "me" {
		t.Fatalf("KnownNick = %q, want %q; chansaver uses this to decide whether "+
			"a JOIN line is ours", got, "me")
	}
}
