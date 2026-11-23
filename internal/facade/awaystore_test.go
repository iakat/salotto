package facade

import (
	"strconv"
	"testing"
	"time"
)

// awayStub returns a Server with away handling on, plus the lines the away
// policy sends to the networks.
func awayStub(t *testing.T) (*Server, *[]string) {
	t.Helper()
	f := newFakeNet("TestNet")
	f.nick = "me"
	s, _ := startServer(t, testConfig(f))
	sent := &[]string{}
	s.away = newAway(true, "gone", time.Hour, func(line string) {
		*sent = append(*sent, line)
	})
	return s, sent
}

// announceCollector is the callback a registering client passes in: it records
// what the returning client is told.
func announceCollector() (func(n int, capped bool), *[]string) {
	notices := &[]string{}
	return func(n int, capped bool) {
		mark := ""
		if capped {
			mark = "+"
		}
		*notices = append(*notices, "You have "+strconv.Itoa(n)+mark+" messages!")
	}, notices
}

// awayNow forces the state fire() produces, so a test need not sleep through
// away_wait.
func awayNow(s *Server) {
	s.away.mu.Lock()
	s.away.weSet = true
	s.away.messages.reset()
	s.away.mu.Unlock()
}

// TestAwayStoreCountsPrivateMessages is awaystore's core promise: while the
// server thinks you are away, private messages are counted so the returning
// client learns how much arrived (awaystore.cpp:388-389).
func TestAwayStoreCountsPrivateMessages(t *testing.T) {
	s, _ := awayStub(t)
	awayNow(s)

	s.NoteInbound("TestNet", []byte(":bob!b@h PRIVMSG me :hello"))
	s.NoteInbound("TestNet", []byte(":carol!c@h PRIVMSG me :\x01ACTION waves\x01"))

	announce, notices := announceCollector()
	s.away.clientAttached(announce)
	if len(*notices) != 1 {
		t.Fatalf("announcements = %v, want one", *notices)
	}
	if got := (*notices)[0]; got != "You have 2 messages!" {
		t.Fatalf("announcement = %q, want 'You have 2 messages!'", got)
	}
}

// TestAwayStoreIgnoresNoise pins the filtering: channel traffic, NOTICEs, CTCPs
// that are not ACTIONs, and our own messages do not count, matching awaystore's
// OnPrivTextMessage / OnPrivActionMessage hooks and their absence for notices
// and channels (-chans is off by default).
func TestAwayStoreIgnoresNoise(t *testing.T) {
	s, _ := awayStub(t)
	awayNow(s)

	for _, line := range [][]byte{
		[]byte(":bob!b@h PRIVMSG #chan :channel chatter"),
		[]byte(":bob!b@h NOTICE me :notice"),
		[]byte(":bob!b@h PRIVMSG me :\x01VERSION\x01"),
		[]byte(":me!m@h PRIVMSG me :my own line"),
		[]byte(":bob!b@h JOIN #chan"),
		[]byte(":bob!b@h PRIVMSG other :someone else's query"),
	} {
		s.NoteInbound("TestNet", line)
	}

	announce, notices := announceCollector()
	s.away.clientAttached(announce)
	if len(*notices) != 0 {
		t.Fatalf("announced %v, want nothing for noise", *notices)
	}
}

// TestAwayStoreCountsOnlyWhileAway: a message arriving with no away set is an
// ordinary unread buffer line, not an away message. The count belongs to the
// interval the server said you were gone.
func TestAwayStoreCountsOnlyWhileAway(t *testing.T) {
	s, _ := awayStub(t)
	s.NoteInbound("TestNet", []byte(":bob!b@h PRIVMSG me :hello"))

	announce, notices := announceCollector()
	s.away.clientAttached(announce)
	if len(*notices) != 0 {
		t.Fatalf("announced %v while not away", *notices)
	}
}

// TestAwayStoreCountIsConsumedOnce: two clients on one account must not both
// announce the same backlog, so the count is taken, not read.
func TestAwayStoreCountIsConsumedOnce(t *testing.T) {
	s, _ := awayStub(t)
	awayNow(s)
	s.NoteInbound("TestNet", []byte(":bob!b@h PRIVMSG me :hello"))

	first, firstNotices := announceCollector()
	s.away.clientAttached(first)
	second, secondNotices := announceCollector()
	s.away.clientAttached(second)

	if len(*firstNotices) != 1 {
		t.Fatalf("first client announced %v, want one", *firstNotices)
	}
	if len(*secondNotices) != 0 {
		t.Fatalf("second client announced %v; the backlog was already delivered", *secondNotices)
	}
}

// TestAwayStoreCountCaps: an unbounded count would be a lie once it overflows
// the format, so past the cap the notice says "9999+" instead.
func TestAwayStoreCountCaps(t *testing.T) {
	var m awayMessages
	for i := 0; i < maxAwayMessages+5; i++ {
		m.add()
	}
	n, capped := m.take()
	if n != maxAwayMessages || !capped {
		t.Fatalf("take() = %d/%v, want %d/true", n, capped, maxAwayMessages)
	}
	if n, capped := m.take(); n != 0 || capped {
		t.Fatalf("second take() = %d/%v, want 0/false", n, capped)
	}
}
