package facade

import (
	"strings"
	"testing"
	"time"
)

// The bug this pins happened live: a client looking at #salotto said something in it and
// the server answered "403 No such channel". The channel was on the second of two networks
// and forward() sent every line to the first, so the wrong IRCd rejected it.
//
// It is the same mistake CHATHISTORY had, from the other side: a read was told to look at
// the first network, and a write was told to send to the first network. A bouncer with two
// networks has one namespace for nicks and one per network for channels, and anything that
// names a channel has to resolve it.

// TestASendFindsTheChannelOnTheSecondNetwork drives the real path: two networks configured,
// the channel only on the second, a client that speaks and then a look at what each fake
// upstream was told.
func TestASendFindsTheChannelOnTheSecondNetwork(t *testing.T) {
	first := newFakeNet("first")
	first.add("status", ":srv 001 me :welcome", time.Now().UnixNano())
	second := newFakeNet("second")
	second.add("#elsewhere", ":alice!a@h PRIVMSG #elsewhere :hi", time.Now().UnixNano())

	s, addr := startServer(t, testConfig(first, second))
	conn, br := dialClient(t, addr)
	registerWithCaps(t, conn, br, "me", "server-time batch")
	collectReplay(t, conn, br)

	_, _ = conn.Write([]byte("PRIVMSG #elsewhere :hello there\r\n"))
	eventually(t, 3*time.Second, func() bool {
		return hasSent(second.sent(), "PRIVMSG #elsewhere :hello there")
	})

	if hasSent(first.sent(), "PRIVMSG #elsewhere :hello there") {
		t.Fatalf("the line went to the network that does not have the channel: %v", first.sent())
	}
	_ = s
}

// A line that does not name a window still goes to the first network: a PRIVMSG to a nick
// is one namespace per client, and there is no window to look up.
func TestASendWithoutAWindowUsesTheFirstNetwork(t *testing.T) {
	first := newFakeNet("first")
	first.add("status", ":srv 001 me :welcome", time.Now().UnixNano())
	second := newFakeNet("second")
	second.add("#elsewhere", ":alice!a@h PRIVMSG #elsewhere :hi", time.Now().UnixNano())

	_, addr := startServer(t, testConfig(first, second))
	conn, br := dialClient(t, addr)
	registerWithCaps(t, conn, br, "me", "server-time batch")
	collectReplay(t, conn, br)

	_, _ = conn.Write([]byte("PRIVMSG bob :a private word\r\n"))
	eventually(t, 3*time.Second, func() bool {
		return hasSent(first.sent(), "PRIVMSG bob :a private word")
	})
	if hasSent(second.sent(), "PRIVMSG bob :a private word") {
		t.Fatal("a private message went to the second network")
	}
}

// A channel nobody has must not be silently rewritten onto another network: the first
// network is the honest default, and its refusal tells the user the channel is not there.
func TestAChannelOnNoNetworkFallsBackToTheFirst(t *testing.T) {
	first := newFakeNet("first")
	first.add("status", ":srv 001 me :welcome", time.Now().UnixNano())
	second := newFakeNet("second")
	second.add("#elsewhere", ":alice!a@h PRIVMSG #elsewhere :hi", time.Now().UnixNano())

	_, addr := startServer(t, testConfig(first, second))
	conn, br := dialClient(t, addr)
	registerWithCaps(t, conn, br, "me", "server-time batch")
	collectReplay(t, conn, br)

	_, _ = conn.Write([]byte("PRIVMSG #nowhere :into the void\r\n"))
	eventually(t, 3*time.Second, func() bool {
		return hasSent(first.sent(), "PRIVMSG #nowhere :into the void")
	})
}

func hasSent(lines []string, want string) bool {
	for _, l := range lines {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}

func eventually(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}
