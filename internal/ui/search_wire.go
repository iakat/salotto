package ui

import (
	"strconv"
	"strings"
	"time"

	"github.com/iakat/salotto/internal/ircline"
)

// searchID reads the request id from a SEARCH reply's tag. -1 means the line carried no id, and
// an unknown id is not allowed to match the model's first request by accident: the model's ids
// start at 1, so -1 can never be current.
func searchID(tags []byte) int {
	v := ircline.TagValue(tags, []byte(ircline.TagSearch))
	if len(v) == 0 {
		return -1
	}
	n, err := strconv.Atoi(string(v))
	if err != nil {
		return -1
	}
	return n
}

// handleSearchReply folds one line of a SEARCH answer into the pending list.
//
// The three forms are deliberately distinguishable at the first byte after "SEARCH": a hit has a
// space and four fixed fields, the terminator has a hyphen and a count, and the error has a
// hyphen and a reason. A client that treated "no hits" as the same thing as "the request failed"
// would send the user hunting for a spelling mistake in a search that never ran.
func (t *Transport) handleSearchReply(id int, text string) {
	switch {
	case strings.HasPrefix(text, "SEARCH-END"):
		results := t.searches[id]
		delete(t.searches, id)
		t.push(SearchResults{ID: id, Results: results})
	case strings.HasPrefix(text, "SEARCH-ERR"):
		reason := strings.TrimSpace(strings.TrimPrefix(text, "SEARCH-ERR"))
		delete(t.searches, id)
		t.push(SearchResults{ID: id, Err: reason})
	case strings.HasPrefix(text, "SEARCH "):
		hit, ok := parseSearchHit(strings.TrimPrefix(text, "SEARCH "))
		if !ok {
			return
		}
		t.searches[id] = append(t.searches[id], hit)
	}
}

// parseSearchHit reads "<date> <time> <network> <window> <body>".
//
// SplitN with a limit of five is the whole contract: the first four fields cannot contain a
// space, and the fifth is the body with every space, colon and tab it arrived with. Splitting
// further and rejoining would normalise the body, which is the one thing a search result must
// never do - the user asked for that exact text.
func parseSearchHit(rest string) (SearchResult, bool) {
	fields := strings.SplitN(rest, " ", 5)
	if len(fields) != 5 {
		return SearchResult{}, false
	}
	date, clock, network, window, body := fields[0], fields[1], fields[2], fields[3], fields[4]
	nick, text, kind := splitLoggedBody(body)
	when, _ := time.ParseInLocation("2006-01-02 15:04:05", date+" "+clock, time.Local)
	return SearchResult{
		Window:  window,
		Network: network,
		Date:    date,
		Time:    clock,
		Line: Line{
			Window:  window,
			Network: network,
			Nick:    nick,
			Text:    text,
			Kind:    kind,
			Time:    when,
		},
	}, true
}

// splitLoggedBody turns a log body into the row the picker draws.
//
// The log stores a message as "<nick> text" and everything else as "*** event"; that is a format
// for a reader, not a parser, but it is the only place the nick survives. A body that does not
// have the shape is kept whole and drawn as a system line rather than guessed at, because
// inventing a sender is worse than showing a line without one.
func splitLoggedBody(body string) (nick, text string, kind LineKind) {
	if strings.HasPrefix(body, "<") {
		if i := strings.Index(body, "> "); i > 1 {
			return body[1:i], body[i+2:], LineMessage
		}
	}
	if strings.HasPrefix(body, "*** ") {
		return "", body[4:], LineSystem
	}
	return "", body, LineSystem
}
