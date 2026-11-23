package ui

import (
	"bufio"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/iakat/salotto/internal/ircline"
)

// Transport is the thing the UI was missing: a connection to a bouncer, and the
// mapping from its wire lines to model messages.
//
// Before this file the three front ends were complete renderers of an empty
// model — an SSH session or a browser page showed a chat window with nothing in
// it and no way to put anything there. That is the gap §2.0's "one model, three
// renderers" rule hides if you only read the model: the model was fine, the
// transport did not exist.
//
// The split stays the same as everywhere else in this package: Transport knows
// about sockets and IRC, the model knows about windows and lines, and neither
// imports the other's business. A transport feeds a channel of tea.Msg that a
// Program (or the web bridge) forwards to Update.
type Transport struct {
	conn net.Conn
	br   *bufio.Reader

	// Windows is the set of window names the transport routes lines into. A
	// channel line goes to its channel, a private message to the sender's nick,
	// and everything the server says about itself to "status", which is ZNC's
	// own convention and what the model's initial window is called.
	mu      sync.Mutex
	windows map[string]bool
	// own is the set of nicks this connection holds. A bouncer can be holding
	// more than one during a nick collision, so this is a set and not a string.
	own map[string]bool
	// members is per-channel membership, as the wire tells it to us: 353 replies
	// name the channel and the nicks with their prefix modes, and JOIN/PART/QUIT/
	// KICK/NICK keep it current after that. It lives in the transport because it is
	// wire knowledge - the model does not know what a "353" is, and should not.
	members map[string]map[string]string
	active  string

	outq chan string
	msgs chan tea.Msg
	done chan struct{}
	err  error
}

// Dial connects to a bouncer and starts reading. It does not register: call
// Register next, because the caller decides the nick and whether a PASS goes
// first.
func Dial(addr string) (*Transport, error) {
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return nil, err
	}
	t := &Transport{
		conn:    conn,
		br:      bufio.NewReaderSize(conn, 64*1024),
		windows: make(map[string]bool),
		own:     make(map[string]bool),
		members: make(map[string]map[string]string),
		active:  "status",
		outq:    make(chan string, 64),
		msgs:    make(chan tea.Msg, 256),
		done:    make(chan struct{}),
	}
	go t.readLoop()
	go t.writeLoop()
	return t, nil
}

// Msgs is the channel a driver forwards to Update. It closes when the
// connection ends, so a Program can exit instead of hanging on a dead link.
func (t *Transport) Msgs() <-chan tea.Msg { return t.msgs }

// Err is why the transport stopped, valid after Msgs closes.
func (t *Transport) Err() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

// Register performs a minimal handshake: optional PASS, then NICK and USER.
// CAP is requested only for the capabilities the model actually uses —
// message-tags/time for line timestamps and away-notify so an away set by the
// bouncer shows up in the window — rather than asking for everything.
func (t *Transport) Register(nick, pass string) {
	if pass != "" {
		t.Send("PASS " + pass)
	}
	t.Send("CAP LS 302")
	t.Send("NICK " + nick)
	t.Send("USER " + nick + " 0 * :" + nick)
	t.Send("CAP END")
}

// Send queues a line for the bouncer. It never blocks the UI: a full queue drops
// the line and reports it, because a keystroke that freezes the screen is worse
// than a lost command, and the box is not a place to hide that.
func (t *Transport) Send(line string) {
	select {
	case t.outq <- line:
	default:
		t.push(Incoming(Line{
			Window: "status",
			Text:   "not sent (connection is not keeping up): " + line,
			Kind:   LineNotice,
		}))
	}
}

// SetActive tells the transport which window the user is looking at, so a
// submitted line knows where to go and so a "current" marker can be shown. It is
// the one piece of view state the transport needs.
func (t *Transport) SetActive(window string) {
	t.mu.Lock()
	t.active = window
	t.windows[window] = true
	t.mu.Unlock()
}

// Command turns what the user typed into a line for the bouncer. A leading "/"
// is a command, and the mapping is ZNC's own bouncer commands where they exist,
// because someone typing /playbuffer expects it to work. A bare line goes to the
// active window: a channel gets a PRIVMSG, the status window gets the bare verb
// (salotto's dialect) so /me-style one-word commands still function.
func (t *Transport) Command(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if !strings.HasPrefix(text, "/") {
		t.mu.Lock()
		window := t.active
		t.mu.Unlock()
		if window == "" || window == "status" {
			return text
		}
		return "PRIVMSG " + window + " :" + text
	}
	body := strings.TrimPrefix(text, "/")
	verb, rest, _ := strings.Cut(body, " ")
	upper := strings.ToUpper(verb)

	switch upper {
	case "ME":
		t.mu.Lock()
		window := t.active
		t.mu.Unlock()
		return "PRIVMSG " + window + " :\x01ACTION " + rest + "\x01"
	case "JOIN", "PART", "QUIT", "NICK", "WHOIS", "MSG", "NOTICE", "AWAY", "TOPIC":
		if upper == "MSG" {
			// "/msg nick text" is a PRIVMSG, not a bouncer command.
			target, msg, _ := strings.Cut(rest, " ")
			if target != "" && msg != "" {
				return "PRIVMSG " + target + " :" + msg
			}
			return "PRIVMSG " + rest + " :"
		}
		return strings.TrimRight(upper+" "+rest, " ")
	}
	// Everything else is a salotto bouncer command, sent as the bare verb. A
	// command with no argument must not go out with a trailing space: the
	// argument is what follows, and a space that is not followed by anything is
	// just noise on the wire (and makes the line compare unequal to what a test
	// or a log expects).
	return strings.TrimRight(strings.ToUpper(verb)+" "+rest, " ")
}

func (t *Transport) writeLoop() {
	for {
		select {
		case <-t.done:
			return
		case line, ok := <-t.outq:
			if !ok {
				return
			}
			if _, err := t.conn.Write([]byte(line + "\r\n")); err != nil {
				t.fail(err)
				return
			}
		}
	}
}

func (t *Transport) readLoop() {
	defer close(t.msgs)
	for {
		line, err := t.br.ReadBytes('\n')
		if len(line) > 0 {
			t.handle(line)
		}
		if err != nil {
			t.fail(err)
			return
		}
	}
}

// handle maps one wire line to a model message. The routing rule is the same one
// the daemon uses: a channel or query line belongs to the window it names, and
// anything else is status. Lines the bouncer generates about itself (NOTICEs,
// numerics) go to status, which is where a user looks for them in ZNC too.
func (t *Transport) handle(raw []byte) {
	line := strings.TrimRight(string(raw), "\r\n")
	if line == "" {
		return
	}
	_, rest := ircline.Tags([]byte(line))

	prefix, rest := ircline.Prefix(rest)
	from := ircline.Nick(prefix)
	verb := strings.ToUpper(string(ircline.Verb(rest)))
	params := string(ircline.Params(rest))

	// A bouncer NOTICE about its own state is a status line, not a chat line:
	// ZNC's *status answers everything this way.
	if verb == "NOTICE" && (from == "status" || from == "") {
		_, text, _ := strings.Cut(params, " :")
		t.push(Incoming(Line{Window: "status", Text: text, Kind: LineNotice}))
		return
	}

	switch verb {
	case "PRIVMSG", "NOTICE":
		target, text, _ := strings.Cut(params, " ")
		text = strings.TrimPrefix(text, ":")
		window := t.windowFor(target, from)
		if window == "" {
			return
		}
		kind := LineMessage
		switch {
		case verb == "NOTICE":
			kind = LineNotice
		case strings.HasPrefix(text, "\x01ACTION"):
			kind = LineAction
			text = strings.TrimSuffix(strings.TrimPrefix(text, "\x01ACTION "), "\x01")
		case strings.HasPrefix(text, "\x01"):
			// Any other CTCP is not chat and does not belong in the window.
			return
		}
		// A mention is our own nick as a word in the text. A substring test would
		// flag "alice" inside "malice", and a word-boundary test done properly
		// needs to know the nick's length anyway, so this walks the text for
		// standalone occurrences.
		line := Line{Window: window, Nick: from, Text: text, Kind: kind}
		if kind != LineNotice && t.mentionsUs(from, text) {
			line.Mention = true
		}
		t.push(Incoming(line))
		return

	case "JOIN":
		// Our own JOIN is what opens a window. Everything else about membership
		// stays in status, because the model has no per-window membership table
		// and inventing one to show "bob joined #x" in #x would be a second
		// source of truth for who is where.
		//
		// The channel is the first parameter, minus a leading ":" when the server
		// sends the extended-join form (":ournick JOIN #chan account :realname").
		channel, _, _ := strings.Cut(strings.TrimPrefix(params, ":"), " ")
		channel = strings.TrimSuffix(channel, ",")
		if channel != "" && from != "" && !t.isSelf(from) {
			// Somebody else joined a channel we know about: membership changed,
			// so the nicklist has to follow or it lists people who left and misses
			// people who arrived.
			t.addMember(channel, from)
			t.publishMembers(channel)
		}
		if channel != "" && t.isSelf(from) {
			t.note(channel)
			t.push(Incoming(Line{
				Window: channel,
				Text:   "joined " + channel,
				Kind:   LineSystem,
			}))
			return
		}
		t.push(Incoming(Line{
			Window: "status",
			Text:   strings.TrimSpace(from + " " + verb + " " + params),
			Kind:   LineSystem,
		}))
		return

	case "NICK":
		// Our own nick change: the new nick is ours too, or every later line
		// prefixed with it stops being recognised as self.
		if t.isSelf(from) {
			if fields := strings.Fields(strings.TrimPrefix(params, ":")); len(fields) > 0 {
				t.noteSelf(fields[0])
			}
		}
		// A rename is a departure from every list and an arrival under a new name.
		var to string
		if fields := strings.Fields(strings.TrimPrefix(params, ":")); len(fields) > 0 {
			to = fields[0]
		}
		if to != "" && from != "" {
			t.renameEverywhere(from, to)
		}
		t.push(Incoming(Line{
			Window: "status",
			Text:   strings.TrimSpace(from + " " + verb + " " + params),
			Kind:   LineSystem,
		}))
		return

	case "KICK":
		// A kick removes one nick from one channel. The channel is the first
		// parameter and the victim the second.
		fields := strings.Fields(params)
		if len(fields) >= 2 {
			channel := strings.TrimSuffix(fields[0], ",")
			t.removeMember(channel, fields[1])
			t.publishMembers(channel)
		}
		t.push(Incoming(Line{
			Window: "status",
			Text:   strings.TrimSpace(from + " " + verb + " " + params),
			Kind:   LineSystem,
		}))
		return

	case "PART":
		// A part removes the parting nick from the channel it names.
		fields := strings.Fields(params)
		if len(fields) >= 1 && from != "" {
			for _, channel := range strings.Split(fields[0], ",") {
				t.removeMember(channel, from)
				t.publishMembers(channel)
			}
		}
		t.push(Incoming(Line{
			Window: "status",
			Text:   strings.TrimSpace(from + " " + verb + " " + params),
			Kind:   LineSystem,
		}))
		return

	case "QUIT":
		// A quit removes the nick from every channel: one departure, several
		// lists.
		t.removeEverywhere(from)
		t.push(Incoming(Line{
			Window: "status",
			Text:   strings.TrimSpace(from + " " + verb + " " + params),
			Kind:   LineSystem,
		}))
		return

	case "MODE":
		// Membership changes are status events rather than conversation, unless
		// they happen in a window the user is in — the model has no per-window
		// membership table, so status is the honest place.
		t.push(Incoming(Line{
			Window: "status",
			Text:   strings.TrimSpace(from + " " + verb + " " + params),
			Kind:   LineSystem,
		}))
		return

	case "PING":
		// The bouncer normally answers its own PINGs, but a direct connection to
		// an IRCd does not; answering here keeps both cases working.
		t.Send("PONG " + params)
		return
	}

	// 353 is the NAMES reply: "353 us = #chan :@op +voice plain". It is the
	// membership of a channel, not conversation, so it becomes a Nicklist message
	// instead of a scrollback line. The channel is the third parameter and the
	// names are everything after the colon.
	if verb == "353" {
		t.noteMembers(params)
		return
	}

	// 366 is end-of-NAMES, which is the moment the list is complete and worth
	// sending. Sending on every 353 would redraw a large channel once per reply.
	if verb == "366" {
		if fields := strings.Fields(params); len(fields) > 1 {
			t.publishMembers(fields[1])
		}
		return
	}

	// 001 names the nick we are registered as; every later routing decision that
	// asks "is this line about us" starts here.
	if strings.HasPrefix(verb, "00") && len(verb) == 3 {
		if fields := strings.Fields(params); len(fields) > 0 {
			t.noteSelf(fields[0])
		}
	}

	// Numerics and anything else are status. 001 also tells us our own nick,
	// which the model shows in the status bar.
	t.push(Incoming(Line{
		Window: "status",
		Text:   strings.TrimSpace(verb + " " + params),
		Kind:   LineSystem,
	}))
}

// windowFor decides which window a message belongs to. A message addressed to us
// personally goes to the sender's window (a query); a channel message goes to the
// channel. ownNick is learned from the connection so a PM is recognisable.
func (t *Transport) windowFor(target, from string) string {
	if strings.HasPrefix(target, "#") || strings.HasPrefix(target, "&") {
		t.note(target)
		return target
	}
	// A message to us: the window is the other party. Without knowing our own nick
	// a NOTICE from the server to us and a PM look identical, so anything that is
	// not a channel is treated as a query window named after the sender.
	if from == "" {
		return "status"
	}
	t.note(from)
	return from
}

// noteMembers records the nicks from one 353 reply. Repeated 353s for the same
// channel accumulate, which is how a channel with more members than fit one line
// is delivered.
func (t *Transport) noteMembers(params string) {
	// params: "<us> <symbol> <#channel> :nick nick nick"
	fields := strings.SplitN(params, " ", 4)
	if len(fields) < 4 {
		return
	}
	channel := fields[2]
	names := strings.TrimPrefix(fields[3], ":")
	t.mu.Lock()
	set, ok := t.members[channel]
	if !ok {
		set = make(map[string]string)
		t.members[channel] = set
	}
	for _, raw := range strings.Fields(names) {
		name, prefix := splitPrefix(raw)
		if name == "" {
			continue
		}
		// Keep the highest prefix seen: a nick is either opped or not, and a
		// repeated reply must not demote someone because it listed them later
		// with a symbol from a different reply.
		if prev, seen := set[name]; !seen || rank(prefix) > rank(prev) {
			set[name] = prefix
		}
	}
	t.mu.Unlock()
}

// splitPrefix separates a NAMES entry's prefix modes from the nick. IRC prefixes
// are ~ & @ % + in descending order of power.
func splitPrefix(raw string) (name, prefix string) {
	i := 0
	for i < len(raw) && strings.ContainsRune("~&@%+", rune(raw[i])) {
		i++
	}
	return raw[i:], raw[:i]
}

// rank orders prefixes so "highest wins" is a comparison and not a lookup table.
func rank(prefix string) int {
	switch {
	case strings.Contains(prefix, "~"):
		return 5
	case strings.Contains(prefix, "&"):
		return 4
	case strings.Contains(prefix, "@"):
		return 3
	case strings.Contains(prefix, "%"):
		return 2
	case strings.Contains(prefix, "+"):
		return 1
	}
	return 0
}

// addMember records one nick in one channel.
func (t *Transport) addMember(channel, nick string) {
	t.mu.Lock()
	set, ok := t.members[channel]
	if !ok {
		set = make(map[string]string)
		t.members[channel] = set
	}
	if _, seen := set[nick]; !seen {
		set[nick] = ""
	}
	t.mu.Unlock()
}

// removeMember drops one nick from one channel.
func (t *Transport) removeMember(channel, nick string) {
	t.mu.Lock()
	if set, ok := t.members[channel]; ok {
		delete(set, nick)
	}
	t.mu.Unlock()
}

// removeEverywhere drops a nick from every channel it is in, which is what QUIT
// means. It also publishes each affected list, because a quiet quit that leaves a
// stale nicklist is worse than no nicklist.
func (t *Transport) removeEverywhere(nick string) {
	t.mu.Lock()
	affected := make([]string, 0, 4)
	for channel, set := range t.members {
		if _, ok := set[nick]; ok {
			delete(set, nick)
			affected = append(affected, channel)
		}
	}
	t.mu.Unlock()
	for _, channel := range affected {
		t.publishMembers(channel)
	}
}

// renameEverywhere applies a nick change to every list holding the old name.
func (t *Transport) renameEverywhere(from, to string) {
	t.mu.Lock()
	affected := make([]string, 0, 4)
	for channel, set := range t.members {
		if prefix, ok := set[from]; ok {
			delete(set, from)
			set[to] = prefix
			affected = append(affected, channel)
		}
	}
	t.mu.Unlock()
	for _, channel := range affected {
		t.publishMembers(channel)
	}
}

// publishMembers sends the membership of a channel to the UI, sorted so the render
// is stable: an unsorted list reshuffles on every redraw and looks like it is
// changing when it is not.
func (t *Transport) publishMembers(channel string) {
	t.mu.Lock()
	set := make(map[string]string, len(t.members[channel]))
	for name, prefix := range t.members[channel] {
		set[name] = prefix
	}
	t.mu.Unlock()
	if len(set) == 0 {
		return
	}
	nicks := make([]Nick, 0, len(set))
	for name, prefix := range set {
		nicks = append(nicks, Nick{Name: name, Prefix: prefix})
	}
	sort.Slice(nicks, func(i, j int) bool {
		// Ops first, then by name: a nicklist is read top-down for "who is in
		// charge", and an alphabetical list buries the ops.
		if ri, rj := rank(nicks[i].Prefix), rank(nicks[j].Prefix); ri != rj {
			return ri > rj
		}
		return strings.ToLower(nicks[i].Name) < strings.ToLower(nicks[j].Name)
	})
	t.push(Nicklist{Window: channel, Nicks: nicks})
}

// isSelf reports whether a prefix nick is one of ours. A bouncer can hold more
// than one nick at a time (nick plus altnick during a collision), so this is a set
// and not a string compare; it is learned from 001 plus our own NICK changes.
func (t *Transport) isSelf(nick string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.own[nick]
}

// noteSelf records a nick as ours.
func (t *Transport) noteSelf(nick string) {
	if nick == "" {
		return
	}
	t.mu.Lock()
	t.own[nick] = true
	t.mu.Unlock()
}

// mentionsUs reports whether text addresses us by nick, as a whole word. It is
// done here rather than in the model because the transport is the only place that
// knows which nicks we are holding.
//
// A bouncer NOTICE or a line from ourselves is never a mention: telling the user
// they mentioned themselves is noise.
func (t *Transport) mentionsUs(from, text string) bool {
	t.mu.Lock()
	own := make([]string, 0, len(t.own))
	for nick := range t.own {
		own = append(own, nick)
	}
	t.mu.Unlock()
	for _, nick := range own {
		if strings.EqualFold(nick, from) {
			continue
		}
		if wordIn(text, nick) {
			return true
		}
	}
	return false
}

// wordIn reports whether word appears in text delimited by non-word characters.
// ASCII letters, digits and underscore count as word characters, which is the same
// set IRC nicknames use.
func wordIn(text, word string) bool {
	if word == "" {
		return false
	}
	lower, target := strings.ToLower(text), strings.ToLower(word)
	for i := 0; i+len(target) <= len(lower); i++ {
		if lower[i:i+len(target)] != target {
			continue
		}
		before := i == 0 || !isNickChar(lower[i-1])
		after := i+len(target) == len(lower) || !isNickChar(lower[i+len(target)])
		if before && after {
			return true
		}
	}
	return false
}

func isNickChar(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' ||
		b == '_' || b == '-' || b == '[' || b == ']' || b == '\\' || b == '^' || b == '`' || b == '{' || b == '}'
}

func (t *Transport) note(window string) {
	t.mu.Lock()
	t.windows[window] = true
	t.mu.Unlock()
}

func (t *Transport) push(msg tea.Msg) {
	select {
	case t.msgs <- msg:
	default:
		// The UI is not draining: dropping is the only option that cannot
		// deadlock the read loop, and a frozen bouncer connection is worse than a
		// missing line.
	}
}

func (t *Transport) fail(err error) {
	t.mu.Lock()
	if t.err == nil {
		t.err = err
	}
	t.mu.Unlock()
	select {
	case <-t.done:
	default:
		close(t.done)
	}
}

// Close ends the transport.
func (t *Transport) Close() error {
	t.fail(net.ErrClosed)
	return t.conn.Close()
}
