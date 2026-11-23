package facade

import "testing"

// The facade caches the upstream nick, and several consumers decide "is this line
// ours?" by comparing against it — chansaver's JOIN/PART tracking and the self-kick
// that disables a channel.
//
// The cache used to be seeded from the target of the first numeric on the wire,
// which is the upstream nick only while the client has not picked a different one.
// A bouncer client routinely picks a different one, so the cache held the client's
// nick: the deployed daemon believed it was "e2e" while upstream it was "e2e_", and
// every "is this line ours?" test answered no. chansaver recorded nothing and no
// channel was ever re-joined, while the unit tests passed because their fake
// network reported the right nick.
//
// A cached nick must therefore never be preferred over what the live network says.
func TestLiveNickBeatsAStaleGuess(t *testing.T) {
	f := newFakeNet("TestNet")
	f.nick = "e2e_"
	srv, _ := startServer(t, testConfig(f))

	ne := srv.network("TestNet")
	if ne == nil {
		t.Fatal("network not registered")
	}

	// Seed the cache the way the deployed daemon did: a numeric addressed to the
	// client's nick, which the facade read as "our" nick.
	ne.nick.set("e2e")
	if got := ne.knownNick(); got != "e2e_" {
		t.Fatalf("knownNick = %q with the cache claiming e2e; the live network "+
			"says e2e_ and must win, or chansaver never recognises our own JOIN", got)
	}

	// And the line-level path must use it too.
	line := []byte(":ergo.test 001 e2e :Welcome to the network")
	forLine, _ := ne.advanceNick(ne.knownNick(), line)
	if forLine != "e2e_" {
		t.Fatalf("advanceNick used %q for the line; want e2e_", forLine)
	}
}

// A network that cannot report its nick still has to work, so the fallback from the
// wire must survive: this is the POC adapter's path.
func TestNickFallbackWhenTheNetworkIsSilent(t *testing.T) {
	ne := &networkEntry{name: "Silent"}
	line := []byte(":ergo.test 001 me :Welcome to the network")
	forLine, _ := ne.advanceNick(ne.knownNick(), line)
	if forLine != "me" {
		t.Fatalf("fallback derived %q from the wire, want %q", forLine, "me")
	}
}
