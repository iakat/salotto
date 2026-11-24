package facade

import (
	"strings"
	"testing"
	"time"
)

// topicReplies returns the 332 lines in a client's received stream.
func topicReplies(lines []string) []string {
	var out []string
	for _, line := range lines {
		if strings.Contains(line, " 332 ") {
			out = append(out, line)
		}
	}
	return out
}

// TestTheBouncerRemembersATopic: a 332 arrives while one client is attached, and
// a second client that attaches afterwards still gets the topic.
//
// This is the half the feature was missing. The client has always drawn the row;
// the bouncer relayed the 332 once into the status ring and forgot it, so a
// client attaching after that ring aged out showed no topic forever. Deleting the
// map write leaves nothing to send and this goes red.
func TestTheBouncerRemembersATopic(t *testing.T) {
	fn := newFakeNet("libera")
	s, addr := startServer(t, testConfig(fn))

	// The first client is attached while the topic arrives, so the second one is
	// genuinely a later client and not the one that could have kept the value.
	first, firstReader := dialClient(t, addr)
	register(t, first, firstReader, "alice")
	_ = collectReplay(t, first, firstReader)

	s.NoteTopic("libera", []byte(":up 332 me #chan :THE EARLY TOPIC"))

	conn, br := dialClient(t, addr)
	register(t, conn, br, "test")
	lines := collectReplay(t, conn, br)

	got := topicReplies(lines)
	if len(got) != 1 {
		t.Fatalf("got %d topic replies, want 1:\n%s", len(got), strings.Join(lines, "\n"))
	}
	if !strings.Contains(got[0], " 332 test #chan :THE EARLY TOPIC") {
		t.Fatalf("the remembered topic did not reach the client: %q", got[0])
	}
}

// TestATopicChangeReplacesTheRememberedOne: the daemon stores the latest topic,
// whether it arrived as the 332 reply or as the TOPIC verb that changes it.
//
// No client is attached while either line arrives, so there is no client struct a
// plausible implementation could have parked the value on: the only way to send
// the second topic is for the daemon to have replaced the first.
func TestATopicChangeReplacesTheRememberedOne(t *testing.T) {
	fn := newFakeNet("libera")
	s, addr := startServer(t, testConfig(fn))

	s.Broadcast("libera", 0, []byte(":up 332 me #chan :the first topic"), time.Now().UnixNano())
	s.Broadcast("libera", 0, []byte(":op!o@h TOPIC #chan :the second topic"), time.Now().UnixNano())

	conn, br := dialClient(t, addr)
	register(t, conn, br, "test")
	lines := collectReplay(t, conn, br)

	got := topicReplies(lines)
	if len(got) != 1 {
		t.Fatalf("got %d topic replies, want 1:\n%s", len(got), strings.Join(lines, "\n"))
	}
	if !strings.Contains(got[0], " 332 test #chan :the second topic") {
		t.Fatalf("the client saw the wrong topic: %q", got[0])
	}
	if strings.Contains(got[0], "the first topic") {
		t.Fatalf("the old topic was sent instead of replaced: %q", got[0])
	}
}

// TestTheTopicArrivesAfterTheReplay: the topic is current state, not history, so
// it is written after the last BATCH -replay and carries no batch tag.
//
// Sending it inside the replay loop is the tidier-looking implementation this
// pins against: a client may treat a batched line as old, and a client that
// builds windows from replayed JOINs does not yet have the channel window when
// the first lines of a batch arrive.
func TestTheTopicArrivesAfterTheReplay(t *testing.T) {
	fn := newFakeNet("libera")
	fn.add("#chan", ":n!u@h PRIVMSG #chan :history", time.Now().UnixNano())
	s, addr := startServer(t, testConfig(fn))

	s.NoteTopic("libera", []byte(":up 332 me #chan :THE EARLY TOPIC"))

	conn, br := dialClient(t, addr)
	register(t, conn, br, "test")
	lines := collectReplay(t, conn, br)

	batchEnd, topic := -1, -1
	for i, line := range lines {
		if strings.Contains(line, " BATCH -replay ") {
			batchEnd = i
		}
		if strings.Contains(line, " 332 ") {
			topic = i
			if strings.Contains(line, "batch=") {
				t.Fatalf("the topic was sent inside the replay batch: %q", line)
			}
		}
	}
	if batchEnd < 0 {
		t.Fatalf("no replay batch was closed:\n%s", strings.Join(lines, "\n"))
	}
	if topic < 0 {
		t.Fatalf("no topic was sent:\n%s", strings.Join(lines, "\n"))
	}
	if topic < batchEnd {
		t.Fatalf("the topic arrived before BATCH -replay (topic %d, batch end %d):\n%s",
			topic, batchEnd, strings.Join(lines, "\n"))
	}
}

// TestNoTopicIsSentForAChannelWithoutOne: no topic means no line. An empty 332
// would claim the channel has a topic and then show nothing, and a 331 for every
// channel on every attach is noise the client does not need.
func TestNoTopicIsSentForAChannelWithoutOne(t *testing.T) {
	fn := newFakeNet("libera")
	fn.add("#chan", ":n!u@h PRIVMSG #chan :history", time.Now().UnixNano())
	_, addr := startServer(t, testConfig(fn))

	conn, br := dialClient(t, addr)
	register(t, conn, br, "test")
	lines := collectReplay(t, conn, br)

	for _, line := range lines {
		if strings.Contains(line, " 332 ") || strings.Contains(line, " 331 ") {
			t.Fatalf("a channel with no topic got a topic reply: %q", line)
		}
	}
}

// TestATopicSurvivesAReconnect: client A is attached when the topic is learned,
// disconnects, and client B attaches later and gets it.
//
// The topic is recorded while A is the only client, so an implementation that
// keeps the value on the client struct hands it to A and to nobody else. B is a
// new client on the same network, and the only place the value can come from is
// the daemon.
func TestATopicSurvivesAReconnect(t *testing.T) {
	fn := newFakeNet("libera")
	s, addr := startServer(t, testConfig(fn))

	first, firstReader := dialClient(t, addr)
	register(t, first, firstReader, "alice")
	_ = collectReplay(t, first, firstReader)

	s.Broadcast("libera", 0, []byte(":up 332 me #chan :the topic that outlives a client"), time.Now().UnixNano())
	_ = first.Close()
	waitFor(t, time.Second, func() bool { return s.hub.Len() == 0 }, "the first client to detach")

	second, secondReader := dialClient(t, addr)
	register(t, second, secondReader, "bob")
	lines := collectReplay(t, second, secondReader)

	got := topicReplies(lines)
	if len(got) != 1 {
		t.Fatalf("the second client got %d topic replies, want 1:\n%s",
			len(got), strings.Join(lines, "\n"))
	}
	if !strings.Contains(got[0], " 332 bob #chan :the topic that outlives a client") {
		t.Fatalf("the topic did not survive the first client: %q", got[0])
	}
}
