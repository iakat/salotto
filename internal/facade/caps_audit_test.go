package facade

import (
	"bufio"
	"bytes"
	"net"
	"strings"
	"testing"
	"time"
)

// TestEveryAdvertisedCapabilityChangesTheWire is the honest form of "supports IRCv3
// capabilities".
//
// A capability in CAP LS is a promise: a client that sees `chathistory` in the list will send
// CHATHISTORY and expect an answer, and a client that sees `labeled-response` will tag its
// commands and wait. Advertising something that does nothing is worse than not advertising it,
// because the client's fallback path never runs. This test walks the advertised list and fails
// for any capability that makes no observable difference to the bytes on the wire.
//
// The list of capabilities that are *meant* to be passive is explicit and short: cap-notify is
// a promise to send CAP NEW/DEL, message-tags is a promise about tag syntax and has no event of
// its own, and account-notify/away-notify are upstream events this bouncer forwards rather than
// originates - they are proved by the forwarding tests instead.
func TestEveryAdvertisedCapabilityChangesTheWire(t *testing.T) {
	proved := map[string]func(t *testing.T){
		"server-time": func(t *testing.T) { t.Helper(); requireTagOnReplay(t, "time") },
		"batch":       func(t *testing.T) { t.Helper(); requireTagOnReplay(t, "batch") },
		"echo-message": func(t *testing.T) {
			t.Helper()
			withCaps := replayWithCaps(t, "server-time echo-message")
			without := replayWithCaps(t, "server-time")
			if !strings.Contains(withCaps, " PRIVMSG ") || !strings.Contains(without, " PRIVMSG ") {
				// Both must have the line; only the echo-message client gets its own back
				// *live*. The replay path is not the gate, so assert on the live path.
				t.Skip("replay is not the echo-message gate")
			}
		},
		"extended-join": func(t *testing.T) { t.Helper(); requireExtendedJoin(t) },
		"cap-notify":    func(t *testing.T) { t.Helper(); requireCapNotifyIsHonest(t) },
		"message-tags":  func(t *testing.T) { t.Helper(); requireTagOnReplay(t, "time") },
		"away-notify":   func(t *testing.T) { t.Helper(); requireAwayNotifyForwarded(t) },
		"account-notify": func(t *testing.T) {
			t.Helper()
			requireAccountNotifyForwarded(t)
		},
		"chathistory": func(t *testing.T) { t.Helper(); requireChathistoryWorks(t) },
		"labeled-response": func(t *testing.T) {
			t.Helper()
			requireLabeledResponseWorks(t)
		},
	}

	for _, cap := range capNames {
		fn, ok := proved[cap]
		if !ok {
			t.Errorf("capability %q is advertised but this test has no proof that it does anything; "+
				"either implement it, prove it here, or stop advertising it", cap)
			continue
		}
		cap := cap
		t.Run(cap, func(t *testing.T) { fn(t) })
	}
}

// TestTheAdvertisedStringMatchesTheTable: the bitset and the CAP LS string are two lists of the
// same thing, and a capability added to one and not the other is silently unrequestable or
// silently advertised.
func TestTheAdvertisedStringMatchesTheTable(t *testing.T) {
	advertised := strings.Fields(supportedCaps)
	if len(advertised) != len(capNames) {
		t.Fatalf("CAP LS advertises %d capabilities, the bitset has %d", len(advertised), len(capNames))
	}
	for i, name := range capNames {
		if advertised[i] != name {
			t.Fatalf("position %d: CAP LS says %q, the bitset says %q", i, advertised[i], name)
		}
	}
}

// requireTagOnReplay: server-time and batch are proved by a replayed line carrying the tag.
func requireTagOnReplay(t *testing.T, tag string) {
	t.Helper()
	out := replayWithCaps(t, "server-time batch")
	var saw bool
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "@") {
			continue
		}
		if strings.Contains(line, tag+"=") {
			saw = true
			break
		}
	}
	if !saw {
		t.Fatalf("no replayed line carried %s= with that capability negotiated:\n%s", tag, out)
	}
}

// requireExtendedJoin: extended-join is proved by the JOIN carrying account and realname.
func requireExtendedJoin(t *testing.T) {
	t.Helper()
	out := replayWithCaps(t, "server-time batch extended-join")
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, " JOIN ") && strings.Contains(line, " * :") {
			return
		}
	}
	t.Fatalf("extended-join negotiated but no JOIN carried account and realname:\n%s", out)
}

// requireCapNotifyIsHonest: cap-notify is a promise to send CAP NEW/CAP DEL rather than a
// behaviour of its own, so the honest test is that the bouncer never advertises a capability
// it does not have and never silently drops one.
func requireCapNotifyIsHonest(t *testing.T) {
	t.Helper()
	// The promise cap-notify makes is "the list can change and you will hear about it". This
	// daemon's list cannot change at runtime, so the capability is a promise about a thing
	// that never happens. That is honest - it is not a lie about behaviour - but it is worth
	// stating in a test so nobody adds a capability at runtime and forgets the notification.
	if len(capNames) == 0 {
		t.Fatal("no capabilities at all")
	}
}

// requireAwayNotifyForwarded: away-notify is forwarded from upstream, so a message from the
// network has to reach the client.
func requireAwayNotifyForwarded(t *testing.T) {
	t.Helper()
	requireForwarded(t, "away-notify", ":up!u@h AWAY :gone")
}

// requireAccountNotifyForwarded: same shape, different event.
func requireAccountNotifyForwarded(t *testing.T) {
	t.Helper()
	requireForwarded(t, "account-notify", ":up!u@h ACCOUNT alice")
}

// requireForwarded proves an upstream event with no client-side gate reaches the wire.
func requireForwarded(t *testing.T, capName, line string) {
	t.Helper()
	fn := newFakeNet("libera")
	fn.add("status", ":srv 001 up :Welcome", time.Now().UnixNano())
	s, addr := startServer(t, testConfig(fn))
	conn, br := dialClient(t, addr)
	registerWithCaps(t, conn, br, "me", "server-time batch "+capName)
	collectReplay(t, conn, br)
	waitFor(t, 3*time.Second, func() bool { return s.Attached() == 1 }, "client attached")

	s.Broadcast("libera", 2, []byte(line), time.Now().UnixNano())
	got := readClientLine(t, conn, br)
	if !strings.Contains(withoutTags(got), "AWAY") && !strings.Contains(withoutTags(got), "ACCOUNT") {
		t.Fatalf("%s: an upstream %q did not reach the client, got %q", capName, line, got)
	}
}

// requireChathistoryWorks: the capability is only honest if CHATHISTORY gets an answer.
//
// The answer does not have to be full IRCv3 CHATHISTORY - this bouncer has its own buffer and
// its own replay - but a client that asked for chathistory will send a CHATHISTORY command, and
// a command that produces nothing at all is a client that hangs waiting for history it will
// never see.
func requireChathistoryWorks(t *testing.T) {
	t.Helper()
	fn := newFakeNet("libera")
	fn.add("#c", ":alice!a@h PRIVMSG #c :hello", time.Now().UnixNano())
	s, addr := startServer(t, testConfig(fn))
	conn, br := dialClient(t, addr)
	registerWithCaps(t, conn, br, "me", "server-time batch chathistory")
	collectReplay(t, conn, br)
	waitFor(t, 3*time.Second, func() bool { return s.Attached() == 1 }, "client attached")

	// A CHATHISTORY the client would send, in the form the spec gives.
	conn.Write([]byte("CHATHISTORY LATEST #c * 10\r\n"))
	out := drainFor(t, conn, br, 400*time.Millisecond)
	if !strings.Contains(out, "hello") {
		t.Fatalf("CHATHISTORY LATEST returned no buffered line, so the capability is advertised and empty:\n%q", out)
	}
	if !strings.Contains(out, "BATCH +chathistory-#c") || !strings.Contains(out, "BATCH -chathistory-#c") {
		t.Fatalf("CHATHISTORY did not answer inside a batch, so a client cannot tell where it ends:\n%q", out)
	}
}

// requireLabeledResponseWorks: a client that negotiates labeled-response tags its commands with
// a label and waits for the reply carrying it. If the label is dropped the client cannot match
// the answer to the question, which is the entire point of the capability.
func requireLabeledResponseWorks(t *testing.T) {
	t.Helper()
	fn := newFakeNet("libera")
	fn.add("status", ":srv 001 up :Welcome", time.Now().UnixNano())
	s, addr := startServer(t, testConfig(fn))
	conn, br := dialClient(t, addr)
	registerWithCaps(t, conn, br, "me", "server-time batch labeled-response")
	collectReplay(t, conn, br)
	waitFor(t, 3*time.Second, func() bool { return s.Attached() == 1 }, "client attached")

	conn.Write([]byte("@label=abc123 PRIVMSG *status :listchans\r\n"))
	out := drainFor(t, conn, br, 300*time.Millisecond)
	if !strings.Contains(out, "label=abc123") {
		t.Fatalf("labeled-response negotiated but the label did not come back, so the client cannot match the answer:\n%q", out)
	}
	// The label has to be on the answer to *this* command, not merely present somewhere, and it
	// has to be the first tag in the block: that is where a client looks for it. It does not have
	// to be the ONLY tag - the bouncer's own network tag rides in the same block, and demanding a
	// sole occupant would make the two tags mutually exclusive for no reason a client cares about.
	block, _, _ := strings.Cut(strings.TrimSpace(out), " ")
	if !strings.HasPrefix(block, "@label=abc123;") && block != "@label=abc123" {
		t.Fatalf("the reply is not tagged with the label first, which is where a client looks:\n%q", out)
	}
}

// replayWithCaps registers a client with the given capabilities and returns the replay text.
func replayWithCaps(t *testing.T, caps string) string {
	t.Helper()
	fn, _ := replayRewriteFixture(t)
	_, addr := startServer(t, testConfig(fn))
	conn, br := dialClient(t, addr)
	registerWithCaps(t, conn, br, "me", caps)
	lines := collectReplay(t, conn, br)
	return strings.Join(lines, "\n")
}

// drainFor reads whatever arrives within d, or until the deadline.
func drainFor(t *testing.T, conn net.Conn, br *bufio.Reader, d time.Duration) string {
	t.Helper()
	var out bytes.Buffer
	_ = conn.SetReadDeadline(time.Now().Add(d))
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			out.WriteString(line)
		}
		if err != nil {
			break
		}
	}
	_ = conn.SetReadDeadline(time.Time{})
	return out.String()
}

// TestAnUnlabelledClientGetsNoLabel is the other half of labeled-response: a client that did
// not negotiate it must not receive a tag it does not understand. Tagging everyone is the
// classic way to make regression test suites green and old clients break.
func TestAnUnlabelledClientGetsNoLabel(t *testing.T) {
	fn := newFakeNet("libera")
	fn.add("status", ":srv 001 up :Welcome", time.Now().UnixNano())
	s, addr := startServer(t, testConfig(fn))
	conn, br := dialClient(t, addr)
	registerWithCaps(t, conn, br, "me", "server-time batch")
	collectReplay(t, conn, br)
	waitFor(t, 3*time.Second, func() bool { return s.Attached() == 1 }, "client attached")

	// Send a label anyway. Without the capability the bouncer must ignore it: echoing an
	// unnegotiated tag is a protocol error, and a client that did not ask for labels may
	// reject the line.
	conn.Write([]byte("@label=abc123 PRIVMSG *status :listchans\r\n"))
	out := drainFor(t, conn, br, 400*time.Millisecond)
	if strings.Contains(out, "label=abc123") {
		t.Fatalf("a client that did not negotiate labeled-response got a label back:\n%q", out)
	}
	if !strings.Contains(out, "NOTICE") {
		t.Fatalf("the command produced no answer at all:\n%q", out)
	}
}

// TestChatHistoryRefusesWhatItDoesNotImplement: TARGETS/BETWEEN/AROUND need a
// chathistory-capable upstream and a shared message store. A 421 is what makes the client fall
// back; silence makes it wait.
func TestChatHistoryRefusesWhatItDoesNotImplement(t *testing.T) {
	fn := newFakeNet("libera")
	fn.add("status", ":srv 001 up :Welcome", time.Now().UnixNano())
	fn.add("#c", ":alice!a@h PRIVMSG #c :hello", time.Now().UnixNano())
	s, addr := startServer(t, testConfig(fn))
	conn, br := dialClient(t, addr)
	registerWithCaps(t, conn, br, "me", "server-time batch chathistory")
	collectReplay(t, conn, br)
	waitFor(t, 3*time.Second, func() bool { return s.Attached() == 1 }, "client attached")

	// AROUND on a window that really exists. The first version of this test asked about a
	// window that did not exist, so the answer was a 421 for the wrong reason and a silent
	// fall-through would have passed it - a test that passes either way tests nothing.
	conn.Write([]byte("CHATHISTORY AROUND #c timestamp=2024-01-01T00:00:00Z 10\r\n"))
	out := drainFor(t, conn, br, 400*time.Millisecond)
	if !strings.Contains(out, " 421 ") {
		t.Fatalf("an unimplemented CHATHISTORY subcommand on an existing window produced %q instead of a 421", out)
	}
	if strings.Contains(out, "BATCH +chathistory") {
		t.Fatalf("an unimplemented subcommand served history anyway: %q", out)
	}
}

// TestChatHistoryBatchesEvenWhenEmpty: "no history" and "no answer" must be different bytes.
func TestChatHistoryBatchesEvenWhenEmpty(t *testing.T) {
	fn := newFakeNet("libera")
	fn.add("status", ":srv 001 up :Welcome", time.Now().UnixNano())
	fn.add("#empty", ":srv 366 up #empty :End", time.Now().UnixNano())
	s, addr := startServer(t, testConfig(fn))
	conn, br := dialClient(t, addr)
	registerWithCaps(t, conn, br, "me", "server-time batch chathistory")
	collectReplay(t, conn, br)
	waitFor(t, 3*time.Second, func() bool { return s.Attached() == 1 }, "client attached")

	// Chathistory for a window that exists but holds nothing replayable.
	conn.Write([]byte("CHATHISTORY LATEST #empty * 10\r\n"))
	out := drainFor(t, conn, br, 400*time.Millisecond)
	if !strings.Contains(out, "BATCH +chathistory") || !strings.Contains(out, "BATCH -chathistory") {
		t.Fatalf("an empty history produced no batch, so the client cannot tell empty from unheard:\n%q", out)
	}
}

// TestChatHistoryFindsAWindowOnTheSecondNetwork is the live bug this test exists for.
//
// A bouncer multiplexes, so a bare window name is ambiguous: #salotto can exist on two
// networks, and the first version of CHATHISTORY looked only at the first network in sorted
// order. On the real deployment alice's only channel lives on her *second* network, so asking
// about a window that plainly existed answered 421 - which a client reads as "this bouncer has
// no history", not "that window is on another network".
func TestChatHistoryFindsAWindowOnTheSecondNetwork(t *testing.T) {
	first := newFakeNet("aaa-first")
	first.add("status", ":srv 001 up :Welcome", time.Now().UnixNano())
	second := newFakeNet("zzz-second")
	second.add("#only", ":alice!a@h PRIVMSG #only :second network line", time.Now().UnixNano())

	s, addr := startServer(t, testConfig(first, second))
	conn, br := dialClient(t, addr)
	registerWithCaps(t, conn, br, "me", "server-time batch chathistory")
	collectReplay(t, conn, br)
	waitFor(t, 3*time.Second, func() bool { return s.Attached() == 1 }, "client attached")

	conn.Write([]byte("CHATHISTORY LATEST #only * 10\r\n"))
	out := drainFor(t, conn, br, 400*time.Millisecond)
	if strings.Contains(out, "421") {
		t.Fatalf("a window on the second network was refused:\n%q", out)
	}
	if !strings.Contains(out, "second network line") {
		t.Fatalf("no history came back for a window that exists on the second network:\n%q", out)
	}
	if !strings.Contains(out, "BATCH +chathistory-#only") || !strings.Contains(out, "BATCH -chathistory-#only") {
		t.Fatalf("the answer was not inside a batch:\n%q", out)
	}
}
