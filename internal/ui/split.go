package ui

import (
	"strings"
)

// MaxSendBytes is the longest PRIVMSG this client will put on one line.
//
// The number is the protocol's, not a preference. RFC 1459 caps a message at 512 octets,
// but every server in practice accepts more and 512 is unusable for real conversation, so
// the figure used here is the one the bouncer itself enforces on a client line:
// internal/facade's MaxLineLen, default 8192, minus the room a PRIVMSG needs for
// "PRIVMSG <target> :" and the IRCv3 tags the bouncer adds. A message over the limit is
// otherwise cut by the bouncer and the tail is lost with no error - QUEUE.md Part 3 item
// 11, which is an explicit constraint on us rather than an inherited bug.
const MaxSendBytes = 8192 - 512

// markerRoom is the widest continuation marker "(999/999) ".
const markerRoom = 12

// splitForWire breaks one message into the lines that will actually carry it.
//
// Splitting happens at a word boundary when one is near the end of the chunk, because a
// message cut mid-word reads as corruption; when no boundary is close enough (a URL, a
// paste, a language without spaces) it hard-splits, because silently losing the rest is
// worse than an ugly break. The split is not an implementation detail the user can be
// spared: a message that arrived as three lines with no explanation looks like the client
// mangled it, so messageParts numbers the parts and the renderer shows the number.
//
// The encoding is UTF-8 and the limit is bytes, so a chunk is trimmed back to a rune
// boundary: splitting inside a multi-byte character produces two lines that are each
// invalid UTF-8, and the receiving client renders replacement characters the user cannot
// connect to anything they typed.
func splitForWire(text string, limit int) []string {
	if limit < 64 {
		// A limit this small cannot carry a message and is a caller bug; refusing to
		// loop forever matters more than being graceful.
		return []string{text}
	}
	if len(text) <= limit {
		return []string{text}
	}
	var out []string
	rest := text
	for len(rest) > limit {
		// Prefer the last space in the window, but only if it is not so far back that the
		// line is mostly empty: "aaaa bbbb" split at 5 on a 100-byte limit is worse than
		// carrying on to the next word.
		cut := -1
		if i := strings.LastIndexByte(rest[:limit], ' '); i > limit/2 {
			cut = i
		}
		if cut <= 0 {
			cut = limit
		}
		// Back off to a rune boundary. A cut inside a multi-byte rune makes both halves
		// invalid, which shows up as garbage at the far end and means the user's text was
		// damaged by us, not by the network.
		for cut > 0 && !utf8Start(rest[cut]) {
			cut--
		}
		if cut <= 0 {
			cut = limit
		}
		chunk := rest[:cut]
		rest = rest[cut:]
		// The space that ended the chunk is a separator, not content; leaving it makes the
		// next chunk start with a space and the whole message gain one per split.
		rest = strings.TrimPrefix(rest, " ")
		out = append(out, chunk)
	}
	if rest != "" {
		out = append(out, rest)
	}
	return out
}

// utf8Start reports whether b can begin a UTF-8 rune, i.e. it is not a continuation byte.
// A continuation byte is 10xxxxxx.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// ctcpAction is the \x01ACTION ...\x01 wrapper /me produces. Every part of a split action has
// to carry it: a part that opens with \x01ACTION and never closes makes a client render the
// rest of the message as raw control-tagged text, and a continuation with no wrapper is
// rendered as a plain message by the same reader that showed the first part as an action.
const (
	ctcpOpen  = "\x01ACTION "
	ctcpClose = "\x01"
)

// messageParts splits one message body into the parts it will travel as, each marked with its
// position. It returns a single unmarked part when the message fits.
//
// This is the only implementation of the rule, called by both the transport (which puts the
// envelope back in front of each part) and the model (which echoes the parts before the
// bouncer relays them). Two copies of "how a message is split" is how a client ends up
// showing one line and sending three, so there is one copy and a test that the two call sites
// still agree.
func messageParts(head, body string, limit int) []string {
	avail := limit - markerRoom - len(head)
	action := strings.HasPrefix(body, ctcpOpen) && strings.HasSuffix(body, ctcpClose)
	if action {
		body = strings.TrimSuffix(strings.TrimPrefix(body, ctcpOpen), ctcpClose)
		avail -= len(ctcpOpen) + len(ctcpClose)
	}
	if avail < 64 || len(body) <= avail {
		// Nothing to do. The action wrapper is left exactly as it arrived, not rebuilt, so
		// the untouched path is byte-identical to what the user's /me produced.
		return []string{withMarker(body, 0, 0)}
	}
	chunks := splitForWire(body, avail)
	out := make([]string, len(chunks))
	for i, c := range chunks {
		if action {
			c = ctcpOpen + c + ctcpClose
		}
		out[i] = withMarker(c, i+1, len(chunks))
	}
	return out
}

// withMarker prefixes a part with "(i/n) " when there is more than one part and returns it
// untouched when there is not, so the common case adds nothing to the text.
func withMarker(body string, i, n int) string {
	if n < 2 {
		return body
	}
	return "(" + itoaSmall(i) + "/" + itoaSmall(n) + ") " + body
}

// splitMessage divides a PRIVMSG into its envelope and its body: "PRIVMSG #c :hi" yields
// head "PRIVMSG #c :" and body "hi". ok is false for anything that is not a message with a
// trailing parameter, which is every command this client does not split.
func splitMessage(line string) (head, body string, ok bool) {
	if !splittable(line) {
		return "", "", false
	}
	i := strings.IndexByte(line, ':')
	if i < 0 {
		// "PRIVMSG #c" with no colon carries no text, so there is nothing to split.
		return "", "", false
	}
	return line[:i+1], line[i+1:], true
}

// splittable reports whether a line carries a message body that can be continued.
//
// A PRIVMSG (including the CTCP ACTION form /me produces) can: its body is free text and a
// reader treats two of them as two sentences. A command whose parameters are a list - a
// MODE, a JOIN, a KICK reason - cannot be continued in a way any server accepts, and
// pretending otherwise would produce a second line the server answers with a syntax error.
// Those keep their old behaviour, which is to be cut by the bouncer's limit.
func splittable(line string) bool {
	head := line
	if len(head) > 8 {
		head = head[:8]
	}
	return strings.HasPrefix(strings.ToUpper(head), "PRIVMSG ")
}

// itoaSmall renders a small positive int without pulling fmt onto this path.
func itoaSmall(n int) string {
	if n == 0 {
		return "0"
	}
	var b [4]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
