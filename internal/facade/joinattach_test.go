package facade

import (
	"testing"

	"github.com/iakat/salotto/internal/ircline"
)

// TestJoinAttachesTheClientToTheChannel pins the rule that a client which joins a
// channel must be attached to it.
//
// Without this the client is attached only to "status", so the JOIN answering its
// own request lands in a window buffer nobody is reading: the channel exists, the
// bouncer is in it, and the user's window list never grows. That is exactly what
// the browser front end did - /join appeared to do nothing, because the only thing
// that would have told the UI about the new window was a line it had not been
// attached to receive.
func TestJoinAttachesTheClientToTheChannel(t *testing.T) {
	s, _, sub, _ := newWindowFixture(t)
	const network = "libera"
	c := &client{srv: s, sub: sub, nick: "test"}
	// Through handle(), which is the read loop's own door: a test that calls
	// attachJoined directly passes even when forward never calls it, which is a
	// test of a function nobody runs.
	raw := []byte("JOIN #attached")
	_, rest := ircline.Tags(raw)
	_, rest = ircline.Prefix(rest)
	if !c.handle(raw, rest) {
		t.Fatal("handle closed the client")
	}

	if !s.WindowAttached(sub, network, "#attached") {
		t.Fatal("the client that joined is not attached to the channel it joined")
	}
}

// TestJoinAttachesEveryChannelInOneLine pins the multi-channel form. "JOIN #a,#b"
// is one line that creates two windows, and attaching to only the first leaves the
// second invisible in the same way.
func TestJoinAttachesEveryChannelInOneLine(t *testing.T) {
	s, _, sub, _ := newWindowFixture(t)
	const network = "libera"
	c := &client{srv: s, sub: sub, nick: "test"}

	raw := []byte("JOIN #one,#two,#three")
	_, rest := ircline.Tags(raw)
	_, rest = ircline.Prefix(rest)
	if !c.handle(raw, rest) {
		t.Fatal("handle closed the client")
	}

	for _, channel := range []string{"#one", "#two", "#three"} {
		if !s.WindowAttached(sub, network, channel) {
			t.Fatalf("not attached to %s", channel)
		}
	}
}

// TestNonJoinLinesDoNotAttach pins that this is about JOIN specifically. A PRIVMSG
// to a channel we are not in must not silently subscribe the client to it, because
// that would turn every stray line into an attachment.
func TestNonJoinLinesDoNotAttach(t *testing.T) {
	s, _, sub, _ := newWindowFixture(t)
	c := &client{srv: s, sub: sub, nick: "test"}

	raw := []byte("PRIVMSG #notjoined :hello")
	_, rest := ircline.Tags(raw)
	_, rest = ircline.Prefix(rest)
	if !c.handle(raw, rest) {
		t.Fatal("handle closed the client")
	}

	if s.WindowAttached(sub, "libera", "#notjoined") {
		t.Fatal("a PRIVMSG attached the client to a channel it did not join")
	}
}
