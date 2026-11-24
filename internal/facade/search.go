package facade

import (
	"fmt"
	"strings"
	"time"

	"github.com/iakat/salotto/internal/ircline"
	"github.com/iakat/salotto/internal/logfile"
)

// searchRequest is one parsed SEARCH line.
//
// The fields are the wire's, not logfile.Query's: parsing here is what lets the facade refuse a
// bad value with a reason before the filesystem walk starts, and the id is the client's request
// number, echoed as a tag so a late answer can be told from the current one.
type searchRequest struct {
	id      string
	window  string
	network string
	since   string
	until   string
	query   string
}

// parseSearchRequest reads "SEARCH [option:value ...] <query>".
//
// Options are recognised only while they are leading. A query is the rest of the line, joined
// with single spaces, because the log body is free text and the common search is a phrase. The
// loop stops at the first token that is not a known option - so "SEARCH since:2026-01-01
// http://x" treats the URL as the query rather than as an unknown option with a bad value.
func parseSearchRequest(params string) (searchRequest, error) {
	var req searchRequest
	fields := strings.Fields(params)
	i := 0
	for ; i < len(fields); i++ {
		name, value, ok := strings.Cut(fields[i], ":")
		if !ok {
			break
		}
		switch strings.ToLower(name) {
		case "id":
			req.id = value
		case "window":
			req.window = value
		case "network":
			req.network = value
		case "since":
			req.since = value
		case "until":
			req.until = value
		default:
			// Not one of ours: it is the beginning of the free-text query. "http://x" and a
			// sentence containing a colon both land here, which is why the query is allowed to
			// start with a colon-bearing word at all.
			goto query
		}
	}
query:
	req.query = strings.Join(fields[i:], " ")
	if req.query == "" {
		return req, fmt.Errorf("usage: SEARCH [window:<name>] [network:<name>] [since:<YYYY-MM-DD>] [until:<YYYY-MM-DD>] <query>")
	}
	return req, nil
}

// handleSearch answers SEARCH from the bouncer's own log index.
//
// It is deliberately a thin adapter: logfile.Query already walks every network, window and day,
// and the index beside each log file is the thing that makes "find it from last month" cheap.
// The facade does not keep the results, rank them or decide what a hit looks like on screen -
// it writes one NOTICE per row and one terminator, and the client draws the picker.
//
// The wire is:
//
//	NOTICE <nick> :SEARCH <date> <time> <network> <window> <body>
//	NOTICE <nick> :SEARCH-END <count>
//	NOTICE <nick> :SEARCH-ERR <reason>
//
// The body is the tail on purpose. It is the only free-text field: it can contain spaces, colons
// and tabs, and splitting it anywhere else would either lose the tail or force an escaping scheme
// no log line had to agree to. Date, time, network and window cannot contain unescaped spaces in
// this format, so a reader can split the first four fields and take the rest verbatim.
func (c *client) handleSearch(rest []byte) bool {
	req, err := parseSearchRequest(string(ircline.Params(rest)))
	if err != nil {
		return c.searchError("", err.Error())
	}
	// Logging off is not an empty index: answering SEARCH-END 0 would be indistinguishable from
	// "nothing matched", and the user would go looking for a spelling mistake in a search that
	// never ran. Say which it is.
	if c.srv.cfg.LogDir == "" {
		return c.searchError(req.id, "logging disabled")
	}
	// Refuse a path component before Query does. Query would also refuse it, but reaching that
	// refusal means building a path from client input first; refusing here keeps the rule at the
	// boundary and gives the client the field it got wrong.
	if req.window != "" && logfile.BadPathComponent(req.window) {
		return c.searchError(req.id, "bad window")
	}
	if req.network != "" && logfile.BadPathComponent(req.network) {
		return c.searchError(req.id, "bad network")
	}
	if err := validSearchDate("since", req.since); err != nil {
		return c.searchError(req.id, err.Error())
	}
	if err := validSearchDate("until", req.until); err != nil {
		return c.searchError(req.id, err.Error())
	}
	if req.since != "" && req.until != "" && req.since > req.until {
		return c.searchError(req.id, "since is after until")
	}

	// limit 0 is Query's own default of 200 (capped at 1000). The cap is not raised here: one
	// NOTICE per hit means an unbounded answer is an unbounded write down a client socket, and a
	// one-word query over a year of logs would be exactly that.
	hits, err := logfile.Query(c.srv.cfg.LogDir, req.network, req.window, req.query,
		req.since, req.until, 0, "")
	if err != nil {
		return c.searchError(req.id, err.Error())
	}

	nick := c.nick
	if nick == "" {
		nick = "*"
	}
	for _, h := range hits {
		// One space after the window and then the body exactly as the log holds it: the split
		// that reads this consumes the four separators and nothing after them.
		line := fmt.Sprintf(":%s NOTICE %s :SEARCH %s %s %s %s %s",
			c.srv.cfg.ServerName, nick, h.Date, h.Time, h.Network, h.Window, h.Body)
		if !c.sendTagged(ircline.TagSearch, req.id, line) {
			return false
		}
	}
	return c.sendTagged(ircline.TagSearch, req.id,
		fmt.Sprintf(":%s NOTICE %s :SEARCH-END %d", c.srv.cfg.ServerName, nick, len(hits)))
}

// searchError writes the failure terminator. It is its own line rather than an empty result
// because "no hits" and "the request failed" are different answers, and only one of them means
// the user should try a different spelling.
func (c *client) searchError(id, reason string) bool {
	nick := c.nick
	if nick == "" {
		nick = "*"
	}
	return c.sendTagged(ircline.TagSearch, id,
		fmt.Sprintf(":%s NOTICE %s :SEARCH-ERR %s", c.srv.cfg.ServerName, nick, reason))
}

// validSearchDate checks an optional YYYY-MM-DD bound. An empty bound is not an error: Query
// supplies its own default (today, and seven days back).
func validSearchDate(name, value string) error {
	if value == "" {
		return nil
	}
	if _, err := time.Parse("2006-01-02", value); err != nil {
		return fmt.Errorf("bad %s: %s", name, value)
	}
	return nil
}
