package ui

import "testing"

// A private conversation is two windows' worth of messages and one window's worth of sense: both
// halves belong under the nick of the person being talked to.
//
// Our own half only reaches the client because the server relays it back, and the relayed line
// carries *us* as the sender - so a router that files a private message by its sender puts our own
// words in a window named after ourselves. Live, "/msg alice are you there" produced no alice
// window at all and a window called nicholas containing our own message.

// TestOurOwnPrivateMessageGoesToTheOtherParty is the defect.
func TestOurOwnPrivateMessageGoesToTheOtherParty(t *testing.T) {
	tr := newTestTransport(t)
	tr.noteSelf("nicholas") // the helper already knows "alice"; add ours
	tr.handle([]byte(":nicholas!u@h PRIVMSG alice :are you there"))

	line, ok := nextIncoming(t, tr)
	if !ok {
		t.Fatal("nothing came out of the transport")
	}
	if line.Window != "alice" {
		t.Fatalf("the window is %q; our own message must land in the query with alice", line.Window)
	}
	if line.Nick != "nicholas" {
		t.Fatalf("the nick is %q, want our own", line.Nick)
	}
	if !line.Own {
		t.Fatal("the line is not marked as ours, so it would be drawn as if alice had said it")
	}
}

// TestAPrivateMessageToUsStillGoesToTheOtherParty is the negative control: the sender is the window
// when the sender is not us, which is the ordinary case and must not regress.
func TestAPrivateMessageToUsStillGoesToTheOtherParty(t *testing.T) {
	// newTestTransport notes "alice" as our own nick, so a message from alice is a message from
	// us. Use a third party and keep the ordinary case honest.
	tr := newTestTransport(t)
	tr.handle([]byte(":bob!b@h PRIVMSG alice :hello there"))

	line, ok := nextIncoming(t, tr)
	if !ok {
		t.Fatal("nothing came out of the transport")
	}
	if line.Window != "bob" {
		t.Fatalf("the window is %q, want bob", line.Window)
	}
	if line.Own {
		t.Fatal("bob's message is marked as ours")
	}
}

// TestOurOwnChannelMessageStillGoesToTheChannel pins that the self check did not swallow the
// channel branch above it: in a channel we are usually the sender and the window is the channel.
func TestOurOwnChannelMessageStillGoesToTheChannel(t *testing.T) {
	tr := newTestTransport(t)
	tr.noteSelf("nicholas")
	tr.handle([]byte(":nicholas!u@h PRIVMSG #salotto :hello channel"))

	line, ok := nextIncoming(t, tr)
	if !ok {
		t.Fatal("nothing came out of the transport")
	}
	if line.Window != "#salotto" {
		t.Fatalf("the window is %q, want #salotto", line.Window)
	}
	if !line.Own {
		t.Fatal("our own channel message is not marked as ours")
	}
}

// TestSomeoneElsesChannelMessageGoesToTheChannel is what makes the channel branch load-bearing: a
// message from another person lands in the channel, not in a query named after them. Without it
// the two branches look interchangeable, because our own messages happen to be classified
// correctly by either one.
func TestSomeoneElsesChannelMessageGoesToTheChannel(t *testing.T) {
	tr := newTestTransport(t)
	tr.handle([]byte(":bob!b@h PRIVMSG #salotto :hello channel"))

	line, ok := nextIncoming(t, tr)
	if !ok {
		t.Fatal("nothing came out of the transport")
	}
	if line.Window != "#salotto" {
		t.Fatalf("the window is %q, want #salotto", line.Window)
	}
	if line.Own {
		t.Fatal("bob's channel message is marked as ours")
	}
}

// TestAnActionWeSendGoesToTheOtherParty covers the CTCP path through the same router: /me to a
// nick is the same envelope and used to have the same defect.
func TestAnActionWeSendGoesToTheOtherParty(t *testing.T) {
	tr := newTestTransport(t)
	tr.noteSelf("nicholas")
	tr.handle([]byte(":nicholas!u@h PRIVMSG alice :\x01ACTION waves\x01"))

	line, ok := nextIncoming(t, tr)
	if !ok {
		t.Fatal("nothing came out of the transport")
	}
	if line.Window != "alice" {
		t.Fatalf("the window is %q, want alice", line.Window)
	}
	if line.Kind != LineAction {
		t.Fatalf("the kind is %v, want LineAction", line.Kind)
	}
	if line.Text != "waves" {
		t.Fatalf("the text is %q, want the body", line.Text)
	}
}
