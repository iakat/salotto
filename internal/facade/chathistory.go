package facade

import (
	"strconv"
	"time"

	"github.com/iakat/salotto/internal/ircline"
)

// chathistory implements the part of IRCv3 CHATHISTORY a bouncer can honestly promise.
//
// The capability is advertised, so a client that sees it will send CHATHISTORY and will wait
// for an answer. ZNC answers it out of its own buffer because the buffer *is* the history: the
// bouncer was there, it recorded what happened, and the client is asking it to say so. This is
// the same shape, deliberately smaller:
//
//	CHATHISTORY LATEST <target> * <limit>
//	CHATHISTORY BEFORE <target> <timestamp=...> <limit>
//
// answered as a BATCH of the buffered lines for that window, sent with the same replay form as
// the automatic replay so a client sees one kind of history on its wire and not two.
//
// What it does not do: request history from upstream, or implement TARGETS/BETWEEN/AROUND.
// Those need a chathistory-capable server and a per-window message store with timestamps the
// server agrees with, and a half-answer would be worse than a clear refusal. An unimplemented
// subcommand gets a 421 naming it, which is exactly what the spec says a server should do when
// it does not know a command - so the client falls back instead of waiting.
func (c *client) handleChatHistory(rest []byte) bool {
	// ircline.Params returns the parameters as one slice, not a field list - the first
	// version of this treated it as a list and read the letter 'L' as the subcommand.
	args := splitParams(ircline.Params(rest))
	if len(args) < 2 {
		return c.numeric421("CHATHISTORY")
	}

	sub := string(args[0])
	// The target is the window name. A bouncer is one network per connection, so the target
	// names the window directly rather than a network-window pair.
	window := string(args[1])

	limit := 50
	var before int64
	switch sub {
	case "LATEST", "BEFORE", "AFTER":
	default:
		// TARGETS, BETWEEN, AROUND: not implemented, and said so.
		return c.numeric421("CHATHISTORY " + sub)
	}

	if sub == "BEFORE" || sub == "AFTER" {
		// The timestamp is required by the spec and is the cursor; without it there is
		// nothing to be before or after.
		if len(args) < 3 {
			return c.numeric421("CHATHISTORY " + sub)
		}
		ts, ok := parseHistoryTimestamp(string(args[2]))
		if !ok {
			return c.numeric421("CHATHISTORY " + sub)
		}
		before = ts
	}

	// A limit of "*" means "the server's default", which is what a client sends when it does
	// not care. Anything else is parsed, and a bad one falls back to the default rather than
	// failing the whole command: a limit is a hint, and refusing to serve history over a
	// mistyped number is a worse answer than serving fifty lines.
	if n := len(args); n >= 3 {
		last := string(args[n-1])
		if v, err := strconv.Atoi(last); err == nil && v > 0 && v <= 1000 {
			limit = v
		}
	}

	// Which network: the first in sorted order, the same rule forward() uses. A plain IRC
	// client has no in-band way to name a network on this connection, and inventing a second
	// rule for history would mean a client asking for #chan got a different #chan than the
	// one it sends to.
	var ne *networkEntry
	for _, cand := range c.srv.netSnapshot() {
		ne = cand
		break
	}
	if ne == nil {
		return c.numeric421("CHATHISTORY")
	}
	r, ok := ne.net.Window(window)
	if !ok {
		return c.numeric421("CHATHISTORY")
	}

	// Collect the newest `limit` lines, honouring the cursor. Walking the ring backwards is
	// not an interface it offers, so this walks forward into a bounded slice and keeps the
	// tail - the ring is capped, and a window that holds more than the ring's worth of
	// history is a window whose whole point is the last few hours.
	type stampLine struct {
		ts   int64
		line []byte
	}
	all := make([]stampLine, 0, limit)
	r.Replay(r.Oldest(), func(seq uint64, line []byte) bool {
		ts := time.Now().UnixNano()
		if v, has := r.Stamp(seq); has && v != 0 {
			ts = v
		}
		all = append(all, stampLine{ts: ts, line: append([]byte(nil), line...)})
		if len(all) > limit*4 {
			// Keep memory bounded even for a huge window: the tail is all we can serve.
			all = all[limit:]
		}
		return true
	})

	// Apply the cursor, then keep the last `limit`.
	selected := all[:0]
	for _, sl := range all {
		switch sub {
		case "BEFORE":
			if sl.ts < before {
				selected = append(selected, sl)
			}
		case "AFTER":
			if sl.ts > before {
				selected = append(selected, sl)
			}
		default:
			selected = append(selected, sl)
		}
	}
	if len(selected) > limit {
		selected = selected[len(selected)-limit:]
	}

	id := "chathistory-" + window
	// The batch is declared even when it is empty: a client that asked for history and gets
	// nothing at all cannot tell "no history" from "no answer", and the empty batch is the
	// one cheap way to say which it is.
	if !c.send(":", c.srv.cfg.ServerName, " BATCH +", id, " chathistory ", window) {
		return false
	}
	un := ne.knownNick()
	for _, sl := range selected {
		forLine, next := ne.advanceNick(un, sl.line)
		un = next
		buf := appendReplayLine(nil, sl.line, id, sl.ts)
		if !c.sub.WriteFrom(buf, forLine) {
			return false
		}
	}
	return c.send(":", c.srv.cfg.ServerName, " BATCH -", id)
}

// splitParams cuts a parameter string into whitespace-separated fields, keeping a trailing
// ":rest" as one field as IRC requires.
func splitParams(params []byte) [][]byte {
	var out [][]byte
	i := 0
	for i < len(params) {
		for i < len(params) && (params[i] == ' ' || params[i] == '\t') {
			i++
		}
		if i >= len(params) {
			break
		}
		if params[i] == ':' {
			out = append(out, params[i+1:])
			break
		}
		start := i
		for i < len(params) && params[i] != ' ' && params[i] != '\t' {
			i++
		}
		out = append(out, params[start:i])
	}
	return out
}

// numeric421 is the standard "unknown command" answer. Using it for a CHATHISTORY subcommand we
// do not implement is what makes the capability honest: the client is told, in the form it
// already understands, that this particular shape of the request is not available.
func (c *client) numeric421(command string) bool {
	nick := c.nick
	if nick == "" {
		nick = "*"
	}
	return c.send(":", c.srv.cfg.ServerName, " 421 ", nick, " ", command, " :Unknown command")
}

// parseHistoryTimestamp reads the ISO8601 form CHATHISTORY uses, with the optional
// "timestamp=" prefix a client may or may not include.
func parseHistoryTimestamp(s string) (int64, bool) {
	s = trimPrefixFold(s, "timestamp=")
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999Z07:00",
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05.999999999Z",
		"2006-01-02T15:04:05Z",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixNano(), true
		}
	}
	return 0, false
}

// trimPrefixFold drops a case-insensitive prefix.
func trimPrefixFold(s, prefix string) string {
	if len(s) >= len(prefix) && equalFoldString(s[:len(prefix)], prefix) {
		return s[len(prefix):]
	}
	return s
}

func equalFoldString(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
