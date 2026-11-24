package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// Three bugs found by looking at a live screen, none of which any local test would have
// produced because each needed a real bouncer to say something unexpected.

// TestAServerNoticeGoesToStatus is "/msg *status help" landing nowhere.
//
// The bouncer answers every command with a NOTICE whose prefix is its own name - a server,
// with no "!user@host". The router treated "not a channel" as "a query, so name the window
// after the sender", which put the answer in a window called
// "salotto-alice.ch-test.katia.sh" and left the status window empty. A user typing at the
// status window and getting silence has no way to tell a broken bouncer from a typo.
func TestAServerNoticeGoesToStatus(t *testing.T) {
	tr := newTestTransport(t)
	tr.handle([]byte(":salotto-alice.ch-test.katia.sh NOTICE alice :The following commands are available:"))

	inc, ok := nextIncoming(t, tr)
	if !ok {
		t.Fatal("nothing came out of the transport")
	}
	if inc.Window != "status" {
		t.Fatalf("the bouncer's answer landed in a window called %q", inc.Window)
	}
	if !strings.Contains(inc.Text, "following commands") {
		t.Fatalf("the answer was not carried: %q", inc.Text)
	}
}

// TestAPrivateMessageStillOpensAQuery is the other half: a person writing to us must still
// get a window named after them, or every private conversation shares the status window.
func TestAPrivateMessageStillOpensAQuery(t *testing.T) {
	tr := newTestTransport(t)
	tr.handle([]byte(":bob!b@host PRIVMSG alice :hi alice"))

	inc, ok := nextIncoming(t, tr)
	if !ok {
		t.Fatal("nothing came out of the transport")
	}
	if inc.Window != "bob" {
		t.Fatalf("a private message from bob opened %q, want bob", inc.Window)
	}
}

// TestATopicChangeDoesNotInventAWindow is the "timestamps" window.
//
// A TOPIC line names its channel first. Parsing it without checking that the first word IS a
// channel means any line that starts with an unexpected word creates a window named after
// that word - which is how a window appeared in the list called "timestamps" after a
// mistyped /topic, and it is a permanent window that nothing will ever remove.
func TestATopicChangeDoesNotInventAWindow(t *testing.T) {
	tr := newTestTransport(t)
	tr.handle([]byte(":alice!a@host TOPIC timestamps and day rules"))

	for {
		select {
		case msg := <-tr.Msgs():
			if inc, ok := msg.(Incoming); ok && inc.Window == "timestamps" {
				t.Fatalf("a window called %q was created from a malformed TOPIC", inc.Window)
			}
		default:
			return
		}
	}
}

// TestTopicWithoutAnArgumentMeansTheActiveChannel is "/topic" doing nothing.
//
// In a channel window "/topic" means "show me this channel's topic" and "/topic x" means
// "set it". Passed through bare it becomes "TOPIC" with no parameter, which is not a
// server command at all, and the user gets no answer from either end.
func TestTopicWithoutAnArgumentMeansTheActiveChannel(t *testing.T) {
	tr := newTestTransport(t)
	tr.SetActive("#chan")

	if got := tr.Command("/topic"); got != "TOPIC #chan" {
		t.Fatalf("/topic sent %q", got)
	}
	if got := tr.Command("/topic hello there"); got != "TOPIC #chan :hello there" {
		t.Fatalf("/topic hello there sent %q", got)
	}
}

// nextIncoming reads the next model message, if it is an Incoming.
func nextIncoming(t *testing.T, tr *Transport) (Incoming, bool) {
	t.Helper()
	select {
	case msg := <-tr.Msgs():
		inc, ok := msg.(Incoming)
		return inc, ok
	default:
		return Incoming{}, false
	}
}

// newTestTransport builds a Transport that is not dialled: handle() and Command() are pure
// functions of the line and the state, and a socket would only add timing.
func newTestTransport(t *testing.T) *Transport {
	t.Helper()
	Configure()
	tr := &Transport{
		windows: make(map[string]bool),
		own:     make(map[string]bool),
		members: make(map[string]map[string]string),
		topics:  make(map[string]string),
		modes:   make(map[string]string),
		msgs:    make(chan tea.Msg, 64),
	}
	tr.noteSelf("alice")
	return tr
}
