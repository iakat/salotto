package facade

import (
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/iakat/salotto/internal/ircline"
	"github.com/iakat/salotto/internal/logfile"
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

	// Which network: the first one that actually holds this window. A bouncer multiplexes,
	// so a window name alone is ambiguous - two networks can both have #salotto - and the
	// answer is to serve the one the client asked about rather than to refuse the question.
	// Refusing was the first version's answer and it was wrong in the common case: a user
	// whose only channel is on their second network asked about a window that existed and
	// got 421, which reads as "this bouncer has no history" rather than "that name is on
	// another network". forward() keeps its first-network rule because a sent line has to
	// pick one; a read is free to look.
	var ne *networkEntry
	var r WindowRing
	for _, cand := range c.srv.netSnapshot() {
		if w, ok := cand.net.Window(window); ok {
			ne, r = cand, w
			break
		}
	}
	if ne == nil {
		// The window is not in any ring. Before refusing, ask whether it exists on disk: after a
		// restart every window is in this state, because the ring is memory and the logs are the
		// only thing that survived. A 421 here reads as "this bouncer does not know that window",
		// which is a lie about what was recorded and is exactly why scrollback appeared to stop
		// at the restart.
		if c.srv.cfg.LogDir != "" {
			// An empty network means "whichever network has it": logfile.Query already reads
			// every network directory when it is not told one, which is the same "a read is
			// free to look" rule the ring lookup uses.
			if c.historyFromLogs("", window, sub, before, limit) {
				return true
			}
		}
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

	// If the ring had nothing older than the cursor, the answer lives in the logs.
	//
	// This is the difference between "this bouncer has no history" and "this bouncer restarted".
	// The ring is memory and it dies with the process; the logs are the only history that
	// survives, so a BEFORE query whose cursor is older than the ring's oldest line is a
	// question the disk can answer and the ring cannot. Serving the empty batch instead would
	// tell the client its scrollback ended at the restart, which is a lie about what was recorded.
	if c.srv.cfg.LogDir != "" {
		oldest := int64(0)
		if len(all) > 0 {
			oldest = all[0].ts
		}
		var fallback bool
		// BEFORE: the cursor is older than everything we hold. AFTER: nothing here is newer,
		// so the ring cannot answer that either.
		switch {
		case sub == "BEFORE":
			// Older than everything the ring holds: only the disk can answer.
			fallback = len(all) == 0 || before < oldest
		case sub == "AFTER":
			// Newer than the ring's newest: the ring cannot answer that either.
			fallback = true
		case sub == "LATEST":
			// "The newest N" on a ring that holds nothing is the same question as BEFORE now.
			fallback = len(all) == 0
		}
		slog.Warn("chathistory decision", "window", window, "sub", sub, "fallback", fallback, "cursor", before, "oldest", oldest, "n", len(all))
		if fallback {
			// Temporary instrumentation: the live probe cannot tell "the branch was not taken"
			// from "the branch was taken and read nothing".
			slog.Warn("chathistory fallback", "window", window, "sub", sub, "cursor", before, "oldest", oldest, "lines", len(all), "logdir", c.srv.cfg.LogDir)
			if c.historyFromLogs(ne.name, window, sub, before, limit) {
				return true
			}
		}
	}

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

// historyFromLogs answers a CHATHISTORY window from the on-disk conversation logs.
//
// The logs are line-oriented text written by internal/logfile, and logfile.Query already knows how
// to read them - including the date index - so this reuses it rather than teaching the bouncer a
// second format. What it adds is the translation: a hit is "date time network window body", which
// is a sentence for a human, and a CHATHISTORY batch needs the line as it would appear on the wire.
//
// The batch is the same shape as the ring's, with the same id, because a client must not be able
// to tell which half of the bouncer answered - the ring and the logs are both "history", and a
// second wire form would make the client care about a distinction that is ours.
func (c *client) historyFromLogs(network, window, sub string, cursor int64, limit int) bool {
	// The cursor is a nanosecond stamp; the log query wants dates. Ask for a window wide enough
	// to hold `limit` lines before it, then trim: a day either side is cheap and the trim below
	// is exact, whereas guessing the right number of days is not.
	until := time.Unix(0, cursor).UTC()
	if cursor == 0 {
		until = time.Now().UTC()
	}
	since := until.AddDate(0, 0, -30)
	hits, err := logfile.Query(c.srv.cfg.LogDir, network, window, "",
		since.Format("2006-01-02"), until.Format("2006-01-02"), limit, "desc")
	if err != nil || len(hits) == 0 {
		return false
	}

	// Query returns newest first; a batch reads oldest first, like the ring's.
	lines := make([][]byte, 0, len(hits))
	times := make([]int64, 0, len(hits))
	for i := len(hits) - 1; i >= 0; i-- {
		h := hits[i]
		// The log line carries no timezone, so it is read on the same clock that wrote it: the
		// process's local time. That is only sound because the writer and this reader are the
		// same daemon on the same host - and it is why the deployment runs in UTC, where "local"
		// and the server-time tags the client compares against are the same clock. A log file
		// copied between timezones would be an hour out, which is a real limitation of a format
		// that records a time without saying which time it is.
		ts, err := time.ParseInLocation("2006-01-02 15:04:05", h.Date+" "+h.Time, time.Local)
		if err != nil {
			continue
		}
		n := ts.UnixNano()
		if sub == "BEFORE" && n >= cursor {
			continue
		}
		if sub == "AFTER" && n <= cursor {
			continue
		}
		// The log body is "nick<TAB>text" or a system line; rebuild the wire form from it. The
		// body is written for a reader, not a parser, so it is re-emitted as a NOTICE-shaped
		// line from the log rather than pretending to be the original PRIVMSG: inventing a
		// sender that the log did not record would be worse than saying where it came from.
		// The log body is the rendered line, "[HH:MM:SS] <nick> text" or "[HH:MM:SS] *** action",
		// which is a format for a reader. Rebuild a wire line from it: a "<nick>" prefix becomes
		// a PRIVMSG from that nick, and everything else becomes a NOTICE from the bouncer. The
		// sender is never invented - a line whose nick the log did not record is attributed to
		// the bouncer, because guessing who said something is worse than saying where it came
		// from.
		body := h.Body
		if i := strings.Index(body, "] "); i >= 0 && strings.HasPrefix(body, "[") {
			body = body[i+2:]
		}
		var wire []byte
		if strings.HasPrefix(body, "<") {
			if end := strings.Index(body, ">"); end > 1 {
				nick := body[1:end]
				text := strings.TrimPrefix(body[end+1:], " ")
				wire = []byte(":" + nick + " PRIVMSG " + window + " :" + text)
			}
		}
		if wire == nil {
			wire = []byte(":salotto!salotto@salotto NOTICE " + window + " :" + body)
		}
		lines = append(lines, wire)
		times = append(times, n)
	}
	if len(lines) == 0 {
		return false
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
		times = times[len(times)-limit:]
	}

	id := "chathistory-" + window
	if !c.send(":", c.srv.cfg.ServerName, " BATCH +", id, " chathistory ", window) {
		return false
	}
	for i, line := range lines {
		buf := appendReplayLine(nil, line, id, times[i])
		if !c.sub.WriteFrom(buf, window) {
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
