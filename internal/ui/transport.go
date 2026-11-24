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
	// topics is the current topic per channel. It is state, not scrollback: a
	// topic is replaced, not appended to, and the whole point of showing it is
	// that it is the current answer to "what is this channel for".
	topics map[string]string
	// modes is the current channel mode string per channel ("+nt"), and
	// prefixOverrides records mode changes received before the member list that
	// they apply to: ergo can send MODE before our 353 for a channel we just
	// joined, and a prefix change for a nick we have not listed yet would be lost.
	modes  map[string]string
	active string

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
		topics:  make(map[string]string),
		modes:   make(map[string]string),
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
	// Our own nick is known here, before the server confirms it, and it has to be
	// recorded now: the bouncer echoes our own PRIVMSG back, and until 001 arrives every
	// such echo looks like a message from a stranger. The 001 handler adds whatever nick
	// the server actually gave us (it may be the altnick), so this is the first entry in a
	// set, not the answer.
	t.noteSelf(nick)
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
			// A bare line in the status window is a bouncer command - that is ZNC's
			// behaviour and ours: someone types "listchans" and expects the channel list.
			// It goes out as typed, NOT uppercased: upper-casing was a nicety for /commands
			// and it turns a stray sentence into a command the server answers with "421
			// Unknown command", which is a confusing way to say "you typed this in the wrong
			// window".
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
			// "/msg *status help" is the spelling every IRC client's /msg box produces, and
			// it must reach the bouncer's own command table. The facade accepts the
			// PRIVMSG form too, but sending it as a bare command here is what makes the
			// *status window show the answer in the window the user is looking at rather
			// than in a private buffer with the bouncer in it.
			target, msg, _ := strings.Cut(rest, " ")
			if target != "" && msg != "" {
				// The admin namespace is a bare command too: the bouncer recognises
				// "PRIVMSG *controlplane <cmd>", but sending the command directly is what
				// makes the answer land in the window the operator is looking at.
				if isStatusTarget(target) || isControlplaneTarget(target) {
					return msg
				}
				return "PRIVMSG " + target + " :" + msg
			}
			return "PRIVMSG " + rest + " :"
		}
		if upper == "TOPIC" {
			// "/topic" alone is the question "what is this channel's topic", and in the
			// window of a channel it means that channel. Sent bare it reaches the bouncer
			// as "TOPIC" with no target, which is a command for the client, not for the
			// server, and the user gets nothing back. Filling in the window is what every
			// other client does.
			t.mu.Lock()
			window := t.active
			t.mu.Unlock()
			if strings.HasPrefix(window, "#") {
				// "/topic" asks, "/topic <anything>" sets. The test is on whether there is
				// an argument at all, not on whether it contains a space: a topic is free
				// text and "timestamps and day rules" is one argument, not two.
				if rest == "" {
					return "TOPIC " + window
				}
				return "TOPIC " + window + " :" + rest
			}
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

// isStatusTarget reports whether a /msg target means the bouncer itself. "*status" is ZNC's
// spelling and "status" is the name of the window salotto opens; a client whose window is
// literally called "status" typing /msg status help means the same thing.
func isStatusTarget(target string) bool {
	return target == "*status" || target == "status"
}

// isControlplaneTarget is the admin namespace, which is a separate command surface on the
// bouncer: *status is the client talking about itself, *controlplane is the operator talking
// about the process.
func isControlplaneTarget(target string) bool {
	return target == "*controlplane" || target == "controlplane"
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

// lineTime reads the server-time tag, the IRCv3 tag a server or bouncer uses to say when
// something happened. It returns the zero time when the tag is absent or unparseable, and
// the caller substitutes the clock: the tag is the only honest source for a replayed line,
// and its absence means "now" rather than "unknown".
func lineTime(tags []byte) time.Time {
	if len(tags) == 0 {
		return time.Time{}
	}
	v := ircline.TagValue(tags, []byte("time"))
	if len(v) == 0 {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, string(v))
	if err != nil {
		return time.Time{}
	}
	return t
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
	tags, rest := ircline.Tags([]byte(line))

	// The timestamp comes from the server-time tag when there is one. A bouncer replay is
	// the case that matters: it delivers what happened hours or days ago, and a line shown
	// with the time it was replayed would put a whole night of conversation at the minute
	// the client reconnected. Servers that do not send the tag fall back to the clock,
	// which is right for a live line and wrong only for a server that is lying.
	stamp := lineTime(tags)
	push := func(l Line) {
		if l.Time.IsZero() {
			l.Time = stamp
		}
		t.push(Incoming(l))
	}

	prefix, rest := ircline.Prefix(rest)
	from := ircline.Nick(prefix)
	verb := strings.ToUpper(string(ircline.Verb(rest)))
	params := string(ircline.Params(rest))

	// A bouncer NOTICE about its own state is a status line, not a chat line:
	// ZNC's *status answers everything this way.
	if verb == "NOTICE" && (from == "status" || from == "") {
		_, text, _ := strings.Cut(params, " :")
		push(Line{Window: "status", Text: text, Kind: LineNotice})
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
		line := Line{Window: window, Nick: from, Text: text, Kind: kind, Own: t.isSelf(from)}
		if kind != LineNotice && t.mentionsUs(from, text) {
			line.Mention = true
		}
		push(line)
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
			// Somebody else joined. The line goes in the channel only if WE are in
			// it: a join is about that channel, and filing it in status leaves a
			// status window full of other people's arrivals and a channel that never
			// says who came. But a stranger joining a channel we are not in is not
			// our conversation, and opening nothing for it is the right answer.
			if !t.knowsChannel(channel) {
				push(Line{
					Window: "status",
					Text:   strings.TrimSpace(from + " " + verb + " " + params),
					Kind:   LineSystem,
				})
				return
			}
			t.addMember(channel, from)
			t.publishMembers(channel)
			push(Line{
				Window: channel,
				Nick:   from,
				Text:   "joined",
				Kind:   LineJoin,
			})
			return
		}
		if channel != "" && t.isSelf(from) {
			t.note(channel)
			push(Line{
				Window: channel,
				Nick:   from,
				Text:   "joined " + channel,
				Kind:   LineSelfJoin,
			})
			// 324 only arrives when asked, unlike 332 which the server volunteers
			// on JOIN. Without this the header shows a topic and no modes, which
			// looks like a channel with no modes rather than one we never asked
			// about.
			t.Send("MODE " + channel)
			return
		}
		push(Line{
			Window: "status",
			Text:   strings.TrimSpace(from + " " + verb + " " + params),
			Kind:   LineSystem,
		})
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
		push(Line{
			Window: "status",
			Text:   strings.TrimSpace(from + " " + verb + " " + params),
			Kind:   LineSystem,
		})
		return

	case "KICK":
		// A kick removes one nick from one channel. The channel is the first
		// parameter and the victim the second.
		fields := strings.Fields(params)
		if len(fields) >= 2 {
			channel := strings.TrimSuffix(fields[0], ",")
			t.removeMember(channel, fields[1])
			t.publishMembers(channel)
			reason := ""
			if len(fields) >= 3 {
				reason = " (" + strings.TrimPrefix(strings.Join(fields[2:], " "), ":") + ")"
			}
			push(Line{
				Window: channel,
				Nick:   from,
				Text:   "kicked " + fields[1] + reason,
				Kind:   LineKick,
			})
			return
		}
		push(Line{
			Window: "status",
			Text:   strings.TrimSpace(from + " " + verb + " " + params),
			Kind:   LineSystem,
		})
		return

	case "PART":
		// A part removes the parting nick from the channel it names.
		fields := strings.Fields(params)
		if len(fields) >= 1 && from != "" {
			reason := ""
			if len(fields) >= 2 {
				reason = " (" + strings.TrimPrefix(strings.Join(fields[1:], " "), ":") + ")"
			}
			for _, channel := range strings.Split(fields[0], ",") {
				t.removeMember(channel, from)
				t.publishMembers(channel)
				push(Line{
					Window: channel,
					Nick:   from,
					Text:   "left" + reason,
					Kind:   LinePart,
				})
			}
			return
		}
		push(Line{
			Window: "status",
			Text:   strings.TrimSpace(from + " " + verb + " " + params),
			Kind:   LineSystem,
		})
		return

	case "QUIT":
		// A quit removes the nick from every channel: one departure, several
		// lists. It is also news in every channel the nick was in, because that is
		// where the user noticed them - a quit filed only in status means a nick
		// silently disappears from the nicklist with no explanation.
		reason := strings.TrimPrefix(strings.TrimSpace(params), ":")
		for _, channel := range t.channelsOf(from) {
			text := "quit"
			if reason != "" {
				text = "quit (" + reason + ")"
			}
			push(Line{
				Window: channel,
				Nick:   from,
				Text:   text,
				Kind:   LineQuit,
			})
		}
		t.removeEverywhere(from)
		push(Line{
			Window: "status",
			Text:   strings.TrimSpace(from + " " + verb + " " + params),
			Kind:   LineSystem,
		})
		return

	case "TOPIC":
		// A topic change is state, so it updates the header; it is also news, so
		// it goes in the window as a line. Both, because "the topic changed" is
		// exactly the moment a user wants to know what it changed to.
		fields := strings.SplitN(params, " ", 2)
		// The channel has to be a channel. Our own "/topic timestamps and day rules" sent
		// from a channel window is "TOPIC #chan :timestamps and day rules" and parses; a
		// stray line where the first word is not a channel must not invent a window named
		// after that word, which is how a window called "timestamps" appeared in the list.
		if len(fields) >= 2 && strings.HasPrefix(fields[0], "#") {
			channel := fields[0]
			topic := strings.TrimPrefix(fields[1], ":")
			t.setTopic(channel, topic)
			push(Line{
				Window: channel,
				Nick:   from,
				Text:   "set the topic: " + topic,
				Kind:   LineSystem,
			})
			return
		}
		push(Line{
			Window: "status",
			Text:   strings.TrimSpace(from + " " + verb + " " + params),
			Kind:   LineSystem,
		})
		return

	case "MODE":
		// A MODE line is three different things depending on what it targets:
		//
		//   MODE #chan +nt            channel modes, which replace the mode string
		//   MODE #chan +o nick        a member gaining a prefix
		//   MODE mynick +i            our own user modes, which we do not draw
		//
		// All three are state, so none belongs in the scrollback. The first two
		// also change something visible, so the nicklist and mode string are
		// republished.
		fields := strings.Fields(params)
		if len(fields) >= 2 && strings.HasPrefix(fields[0], "#") {
			channel, modeStr := fields[0], fields[1]
			t.applyModes(channel, modeStr, fields[2:])
			t.publishMembers(channel)
			return
		}
		// Our own user modes are the only thing left that matters, and the status
		// bar does not show them, so they are recorded and not drawn.
		return

	case "PING":
		// The bouncer normally answers its own PINGs, but a direct connection to
		// an IRCd does not; answering here keeps both cases working.
		t.Send("PONG " + params)
		return
	}

	// 324 is the channel mode reply: "324 me #chan +nt". It is state, for the same
	// reason a topic is: the current answer, replaced when it changes.
	if verb == "324" {
		fields := strings.SplitN(params, " ", 3)
		if len(fields) >= 3 {
			t.setModes(fields[1], fields[2])
		}
		return
	}

	// 332 is the topic reply: "332 me #chan :the topic". 331 says there is none.
	// Both belong in the channel's header, not its scrollback: a topic is current
	// state that gets replaced, and leaving a copy in the conversation would show
	// every old topic forever.
	if verb == "332" {
		fields := strings.SplitN(params, " ", 3)
		if len(fields) >= 3 {
			t.setTopic(fields[1], strings.TrimPrefix(fields[2], ":"))
		}
		return
	}
	if verb == "331" {
		if fields := strings.Fields(params); len(fields) >= 2 {
			t.setTopic(fields[1], "")
		}
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
	//
	// The error range gets its own kind so it can be drawn as one. 401, 403 and
	// 473 arrive here when a command was refused, and a refusal rendered in the
	// same grey as the MOTD is a refusal the user never reads: the question they
	// ask afterwards is "why did my message not send", and the answer was on
	// screen. Numeric replies are 3 digits; anything else is a verb.
	kind := LineSystem
	if len(verb) == 3 && verb[0] >= '4' && verb[0] <= '5' {
		kind = LineError
	}
	push(Line{
		Window: "status",
		Text:   strings.TrimSpace(verb + " " + params),
		Kind:   kind,
	})
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
	// A server NOTICE never reaches here: handle() routes it to the status window before
	// this point, which is what keeps "/msg *status help" answering where the user asked.
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

// setModes records a channel's mode string and tells the UI.
//
// The reply is normalised through the same fold a live MODE uses, because the server
// writes the letters in whatever order it likes: without this, "+tn" from 324 and
// "+nt" after a change are the same channel shown differently, which reads as a
// change that did not happen.
func (t *Transport) setModes(channel, modes string) {
	normalised := "+"
	for _, r := range strings.TrimPrefix(modes, "+") {
		normalised = foldMode(normalised, r, true)
	}
	t.mu.Lock()
	t.modes[channel] = normalised
	t.mu.Unlock()
	t.push(ChannelModes{Window: channel, Modes: normalised})
}

// applyModes folds one MODE change into a channel's state: the mode letters go into
// the channel's mode string, and any mode in PREFIXES moves a nick's prefix.
//
// It handles the multi-change form ("+o-v nick1 nick2") because that is one line
// carrying two independent facts, and handling only the first would silently miss
// the rest - which is the kind of bug that shows as "the ops list is wrong
// sometimes".
func (t *Transport) applyModes(channel, modeStr string, args []string) {
	t.mu.Lock()
	current := t.modes[channel]
	if current == "" {
		current = "+"
	}
	set, ok := t.members[channel]
	if !ok {
		set = make(map[string]string)
		t.members[channel] = set
	}
	argi := 0
	adding := true
	for _, r := range modeStr {
		switch r {
		case '+':
			adding = true
		case '-':
			adding = false
		default:
			if prefix, ok := prefixModes[r]; ok {
				if argi < len(args) {
					nick := args[argi]
					argi++
					if adding {
						if rank(string(prefix)) > rank(set[nick]) {
							set[nick] = string(prefix)
						}
					} else {
						// Removing a prefix means the next-highest one applies,
						// which we do not know: the honest answer is "no prefix"
						// until the next NAMES, and guessing would invent rank.
						set[nick] = ""
					}
				}
				continue
			}
			// A list mode (+b, +e, +I) takes an argument we do not track, but the
			// argument still has to be consumed or the next mode in the line gets
			// the wrong one.
			if strings.ContainsRune("beIk", r) && argi < len(args) {
				argi++
				continue
			}
			current = foldMode(current, r, adding)
		}
	}
	t.modes[channel] = current
	modes := current
	t.mu.Unlock()
	t.push(ChannelModes{Window: channel, Modes: modes})
}

// prefixModes maps the mode letters that grant a membership prefix to that prefix.
var prefixModes = map[rune]string{'q': "~", 'a': "&", 'o': "@", 'h': "%", 'v': "+"}

// foldMode adds or removes one letter from a mode string, keeping it sorted so
// "+nt" and "+tn" are the same state. A mode string that changes order on every
// update would look like it is changing when it is not.
func foldMode(modes string, letter rune, adding bool) string {
	has := strings.ContainsRune(modes, letter)
	switch {
	case adding && !has:
		modes += string(letter)
	case !adding && has:
		modes = strings.ReplaceAll(modes, string(letter), "")
	}
	// Rebuild as "+" plus sorted letters: no "-" section is tracked, because a
	// channel's modes are a set and the reply (324) is the truth.
	letters := []rune(strings.TrimPrefix(modes, "+"))
	sort.Slice(letters, func(i, j int) bool { return letters[i] < letters[j] })
	return "+" + string(letters)
}

// setTopic records a channel's current topic and tells the UI. An empty topic is a
// real state - "there is no topic" is different from "we have not been told" - so it
// is recorded rather than deleted.
func (t *Transport) setTopic(channel, topic string) {
	t.mu.Lock()
	t.topics[channel] = topic
	t.mu.Unlock()
	t.push(Topic{Window: channel, Text: topic})
}

// knowsChannel reports whether we are in a channel, which decides whether somebody
// else's arrival belongs in that window or in status.
func (t *Transport) knowsChannel(channel string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.members[channel]
	return ok
}

// channelsOf lists the channels a nick is a member of, so a quit can be reported
// where the user saw the nick rather than only in status.
func (t *Transport) channelsOf(nick string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for channel, set := range t.members {
		if _, ok := set[nick]; ok {
			out = append(out, channel)
		}
	}
	sort.Strings(out)
	return out
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
