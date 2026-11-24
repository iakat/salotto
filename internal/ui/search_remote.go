package ui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// SearchAvailable tells the model that the server on the other end can answer a search from its
// own log index.
//
// It is a message rather than a constructor argument because the answer is not known when the
// client starts: the registration burst is what says "salotto". A model built by hand, a headless
// renderer and a client of a plain IRCd never receive this, and they must keep the old in-memory
// behaviour exactly.
type SearchAvailable struct{}

// SearchWanted is the model asking the transport to run a search on the bouncer.
//
// The model does not own a socket, so it cannot call logfile.Query itself, and it must not block
// the render loop while the bouncer walks a year of files. The id travels all the way to the wire
// and back on every reply line; it is how a late answer is recognised as late.
type SearchWanted struct {
	ID      int
	Query   string
	Window  string
	Network string
	Since   string
	Until   string
}

// SearchResults is one complete answer from the bouncer.
//
// It arrives once, on SEARCH-END, rather than one message per hit: the picker is a list, not a
// stream, and filling it incrementally would redraw and re-index it once per row. Err carries a
// SEARCH-ERR reason, which is deliberately not the same thing as an empty Results slice.
type SearchResults struct {
	ID      int
	Results []SearchResult
	Err     string
}

// searchArgs is a parsed /search line: the free-text query plus the optional bounds the client
// understands. It mirrors the facade's parser so a typo is refused before a round trip, but the
// facade parses again: the wire is the contract, and a client is not a trusted parser.
type searchArgs struct {
	query   string
	window  string
	network string
	since   string
	until   string
}

// parseSearchArgs reads "/search [option:value ...] <query>".
//
// Options are only recognised while they lead. Everything from the first token that is not one of
// them is the query, spaces and colons included: a search for "http://example.com" must not be
// read as an option called "http".
func parseSearchArgs(input string) (searchArgs, error) {
	var a searchArgs
	fields := strings.Fields(input)
	i := 0
	for ; i < len(fields); i++ {
		name, value, ok := strings.Cut(fields[i], ":")
		if !ok {
			break
		}
		switch strings.ToLower(name) {
		case "window":
			a.window = value
		case "network":
			a.network = value
		case "since":
			a.since = value
		case "until":
			a.until = value
		default:
			goto query
		}
	}
query:
	a.query = strings.Join(fields[i:], " ")
	if a.query == "" {
		return a, fmt.Errorf("usage: /search [window:<name>] [network:<name>] [since:<YYYY-MM-DD>] [until:<YYYY-MM-DD>] <text>")
	}
	for _, bound := range []struct{ name, value string }{
		{"since", a.since}, {"until", a.until},
	} {
		if bound.value == "" {
			continue
		}
		if _, err := time.Parse("2006-01-02", bound.value); err != nil {
			return a, fmt.Errorf("usage: /search %s must be YYYY-MM-DD", bound.name)
		}
	}
	if a.since != "" && a.until != "" && a.since > a.until {
		return a, fmt.Errorf("usage: /search since is after until")
	}
	return a, nil
}

// startSearch is /search. It is the single place that decides between the bouncer's index and the
// client's own scrollback.
//
// One predicate, not two paths. With a bouncer the request becomes a command and the model returns
// before any disk is touched; without one the scan is local and synchronous, exactly as before.
// Having the remote path fall back to the local scan on an error would be a second decision made
// in a second place, and the two would eventually disagree about which history was searched.
func (m Model) startSearch(input string) (Model, tea.Cmd) {
	args, err := parseSearchArgs(input)
	if err != nil {
		return m.appendSystem(err.Error() + "\n"), nil
	}
	if !m.searchRemote {
		return m.openSearch(args.query), nil
	}

	m.searchSeq++
	id := m.searchSeq
	m.searchPending = true
	m.searchQuery = args.query
	// A new question replaces the old list. Leaving the old picker up would let the arrows walk a
	// result set that is no longer the answer on screen.
	m.searchHits = nil
	m.searchAt = 0
	m.searchJumpedAt = -1

	want := SearchWanted{
		ID:      id,
		Query:   args.query,
		Window:  args.window,
		Network: args.network,
		Since:   args.since,
		Until:   args.until,
	}
	return m, func() tea.Msg { return want }
}

// applySearchResults files one answer from the bouncer.
//
// An answer for a superseded request is dropped, not shown late. A late picker is worse than no
// picker: it looks like the answer to the question the user is looking at, so they walk a list
// from an older query believing it is current, and the only way to notice is that the hits are
// for words they no longer remember typing. No reply is honest - the "searching…" row already
// says the current question has not been answered.
func (m Model) applySearchResults(msg SearchResults) Model {
	if msg.ID != m.searchSeq || !m.searchPending {
		return m
	}
	m.searchPending = false
	if msg.Err != "" {
		return m.appendSystem("search failed: " + msg.Err + "\n")
	}
	if len(msg.Results) == 0 {
		// Naming the query matters: "/search sat" returning nothing and "/search satloto"
		// returning nothing are different problems and the message should say which.
		return m.appendSystem("no matches for " + m.searchQuery + "\n")
	}
	m.searchHits = m.fileSearchResults(msg.Results)
	m.searchAt = 0
	m.searchJumpedAt = -1
	return m
}

// fileSearchResults prepares wire hits for the picker without changing the active window.
//
// Arriving while the user is reading something else must not steal focus: the list is drawn in
// the conversation area, but the window underneath stays the one the user chose, and only walking
// the list commits a jump.
func (m Model) fileSearchResults(results []SearchResult) []SearchResult {
	// Whether a name needs its network on screen is a property of the whole answer, not of one
	// row: "status" alone is fine until the answer also holds another network's "status".
	seen := make(map[string]map[string]bool, len(results))
	for _, hit := range results {
		name := winName(hit.Window)
		if seen[name] == nil {
			seen[name] = make(map[string]bool, 2)
		}
		if hit.Network != "" {
			seen[name][hit.Network] = true
		}
	}

	out := make([]SearchResult, 0, len(results))
	for _, hit := range results {
		name := winName(hit.Window)
		hit.Window = name
		if hit.Network != "" && (len(seen[name]) > 1 || m.nameOnOtherNetwork(name, hit.Network)) {
			hit.Window = name + " (" + hit.Network + ")"
		}
		out = append(out, hit)
	}
	return out
}

// nameOnOtherNetwork reports whether a live window with this display name belongs to another
// network. It is the open-window half of the check above, so a list that adds a disk hit beside a
// window already on screen still says which server each one is.
func (m Model) nameOnOtherNetwork(name, network string) bool {
	for key := range m.windows {
		net, base := keyWindow(key)
		if base == name && net != "" && net != network {
			return true
		}
	}
	return false
}

// windowForHit resolves a hit to the window key it belongs to.
//
// A wire hit names its network, and that is a better answer than the display name can give: a log
// window may not be open, and netOfName refuses to guess when two networks hold the same name. So
// the network is used directly - the key is created on that network rather than on whichever one
// happens to be open. A local hit has no network because it came from a key that already exists,
// and its display name (possibly qualified) goes through SetActive's usual resolution.
func (m Model) windowForHit(hit SearchResult) string {
	if hit.Network != "" {
		name := winName(hit.Window)
		// fileSearchResults may have printed "name (network)" on this row; that suffix is
		// decoration and must not become part of the window name the key is built from.
		if base, _, ok := splitQualifiedName(name); ok {
			name = base
		}
		return windowKey(hit.Network, name)
	}
	return hit.Window
}

// cancelSearch reports an in-flight request that can no longer be answered, so "searching…" does
// not stay on screen forever after the connection drops.
func (m Model) cancelSearch(reason string) Model {
	if !m.searchPending {
		return m
	}
	m.searchPending = false
	return m.appendSystem("search cancelled: " + reason + "\n")
}

// searchWireLine renders a SearchWanted as the verb the bouncer's facade parses.
//
// Options precede the query, space separated, and the query is last because it is free text. The
// id is first so the bouncer's answer can be tagged with it; the client never sends a search
// without one.
func searchWireLine(want SearchWanted) string {
	var b strings.Builder
	b.WriteString("SEARCH id:")
	b.WriteString(strconv.Itoa(want.ID))
	if want.Window != "" {
		b.WriteString(" window:")
		b.WriteString(want.Window)
	}
	if want.Network != "" {
		b.WriteString(" network:")
		b.WriteString(want.Network)
	}
	if want.Since != "" {
		b.WriteString(" since:")
		b.WriteString(want.Since)
	}
	if want.Until != "" {
		b.WriteString(" until:")
		b.WriteString(want.Until)
	}
	b.WriteString(" ")
	b.WriteString(want.Query)
	return b.String()
}
