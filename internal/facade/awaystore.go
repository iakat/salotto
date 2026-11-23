package facade

import (
	"strings"

	"github.com/iakat/salotto/internal/ircline"
)

// awayMessages is the part of ZNC's awaystore module that our rings do not
// already do.
//
// Most of awaystore is a private message store, and that half is redundant here:
// an incoming private message already lands in the sender's query window, and a
// reattaching client replays it through PlayBuffer. ZNC needs a separate store
// because its buffer is a per-window line deque that a module cannot count;
// salotto has per-window rings with cursors, so "what arrived while I was away"
// is already answerable.
//
// What is *not* redundant is the accounting: ZNC tells the returning user how
// much is waiting ("Welcome back!" / "You have N messages!", awaystore.cpp:388-389),
// and that count is what makes the module useful in practice — it is the
// difference between replaying a buffer and knowing you should.
//
// The window is exactly the bouncer-set away, not "no client attached". A user
// who detached with no away configured has an unread buffer like any other; the
// count is about messages that arrived while the network was told you were gone.
type awayMessages struct {
	count int
	// capped records that we stopped counting, so the notice can say "N+" rather
	// than lie about a number we did not finish counting.
	capped bool
}

// maxAwayMessages bounds the count. ZNC's store is unbounded and grows a file;
// a count needs no such growth, but it still must not be able to claim a number
// that overflows the format.
const maxAwayMessages = 9999

func (m *awayMessages) reset() {
	m.count = 0
	m.capped = false
}

func (m *awayMessages) add() {
	if m.count >= maxAwayMessages {
		m.capped = true
		return
	}
	m.count++
}

// take returns the count and clears it, which is what a returning client
// consumes. A second client attaching afterwards gets nothing to report, so two
// clients on one account do not both announce the same backlog.
func (m *awayMessages) take() (int, bool) {
	n, capped := m.count, m.capped
	m.reset()
	return n, capped
}

// NoteInbound feeds one line from a network to the away accounting. The daemon
// calls it for every line before the fan-out, so the count is exact regardless
// of which windows a line ends up in.
//
// Which lines count is ZNC's set: a private PRIVMSG and a private CTCP ACTION
// (awaystore.cpp OnPrivTextMessage, OnPrivActionMessage). A NOTICE does not
// count, and neither does channel traffic unless the module was loaded with
// -chans, which is off by default.
func (s *Server) NoteInbound(netName string, line []byte) {
	if s.away == nil {
		return
	}
	own := s.KnownNick(netName)
	if own == "" {
		return
	}

	_, rest := ircline.Tags(line)
	prefix, rest := ircline.Prefix(rest)
	verb := ircline.Verb(rest)
	if !strings.EqualFold(string(verb), "PRIVMSG") {
		return
	}
	from := ircline.Nick(prefix)
	if from == "" || strings.EqualFold(from, own) {
		return
	}

	params := string(ircline.Params(rest))
	target, text := splitFirst(params)
	if !strings.EqualFold(target, own) {
		// A channel message. ZNC's awaystore ignores these unless asked with
		// -chans, so we ignore them too rather than inventing a highlight scan.
		return
	}
	// A CTCP that is not ACTION (VERSION, PING, ...) is not someone talking to
	// you. ZNC's CTextMessage/CActionMessage split does the same filtering.
	if strings.HasPrefix(text, "\x01") && !strings.HasPrefix(strings.ToUpper(text), "\x01ACTION ") {
		return
	}
	s.away.notePrivate()
}

// splitFirst splits "target :rest" into its two halves, trimming the trailing
// colon IRC puts before a free-text parameter.
func splitFirst(params string) (string, string) {
	target, text, _ := strings.Cut(params, " ")
	return target, strings.TrimPrefix(text, ":")
}
