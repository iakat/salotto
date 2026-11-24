// Package ui is the single presentation model for every salotto front end.
//
// The rule that keeps the browser from growing a second UI implementation: a
// model here knows nothing about what it is drawing into. Input arrives as a
// tea.Msg, output is one string of text plus ANSI. An SSH session feeds it a
// real terminal, the web front end calls Update/View itself and pipes View()
// through an ANSI→HTML converter, and a test calls Update/View with synthetic
// messages. There is exactly one View for all three (QUEUE.md §2.0).
package ui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// completionState remembers which cycle Tab is on. The key is the word the cycle started from, so
// that growing the word ("/jo" then "/join") starts a fresh cycle instead of continuing the old one.
type completionState struct {
	// prefix is what the user had typed when the cycle started, and cands is the set that prefix
	// matched. Both are kept because the word on screen changes on every Tab: keying the cycle on
	// the word would restart it after each completion, and Tab would then appear stuck.
	prefix string
	cands  []string
	idx    int
}

// Line is one rendered piece of scrollback: a sender and its text, already
// plain (no ANSI) so the model can style it per front end.
type Line struct {
	Window string
	Nick   string
	Text   string
	Kind   LineKind

	// Time is when the line happened. It comes from the server-time tag when the server
	// or the bouncer sent one, and from the clock otherwise.
	//
	// A buffer that spans days cannot be read without it: a bouncer replays what happened
	// while you were away, and a replay with no timestamps is a wall of text in an order
	// that only looks chronological. The zero value means "no time known" and renders as
	// no timestamp rather than as an epoch, because a wrong timestamp is worse than none.
	Time time.Time

	// Mention marks a line that addressed us by name. It lives on the line rather
	// than in a separate index so a renderer draws a highlight from the data it is
	// already drawing, and a search result carries its own answer to "was this for
	// me".
	Mention bool

	// Own marks a line that came from us. The bouncer echoes our own PRIVMSG back, and
	// that echo is authoritative - it is what makes a second client see the same
	// conversation - so it must replace the optimistic line rather than join it. Own is
	// set by the transport, which is the only place that knows which nicks are ours.
	Own bool

	// Pending marks a local echo: the line the user just typed, shown before the
	// bouncer has confirmed it. The bouncer echoes our own PRIVMSG back (that is what
	// a bouncer is for - a second client must see it), so without this flag every
	// message we send appears twice: once optimistically, once for real.
	//
	// The echoed line replaces the pending one rather than being added: that keeps the
	// message in the place the user saw it and gives it its real nick and timestamp.
	Pending bool
}

// LineKind selects the style a line is drawn with. It exists so the model never
// carries markup: the renderer decides what an "action" looks like.
type LineKind int

const (
	LineMessage LineKind = iota
	LineAction
	LineNotice
	LineSystem
	LineGap
	// The membership and channel events. Each is its own kind rather than one
	// "system" bucket because every front end wants to draw them differently:
	// a join and a quit are the same event with opposite meaning, and a client that
	// renders them identically makes a busy channel unreadable.
	LineJoin
	LinePart
	LineQuit
	LineKick
	LineNick
	LineTopic
	LineMode
	// LineSelfJoin is our own JOIN. It is separate from LineJoin because it does something
	// no other line does: it switches the active window. Joining a channel and staying in
	// the status window means the next thing you type goes out as a bouncer command - the
	// user joins #chan, says "hello", and the server answers "421 Unknown command".
	LineSelfJoin
	// LineDay is the date rule inserted between two lines from different days. It is a
	// line rather than a per-render decoration so it scrolls with the conversation: a
	// separator that is computed at draw time moves as you scroll, and the whole point is
	// to tell you where one day ended.
	LineDay

	// LinePreview is a link card. It is a line and not a panel because a terminal
	// has no panels: the one thing a terminal can show beside a message is another
	// line, and putting it there means the SSH and PTY front ends get previews for
	// free instead of the browser getting them alone.
	LinePreview

	// LineError is a numeric reply in the error range: 401 no such nick, 403 cannot
	// send to channel, 404 too many channels, 473 invite only. It is separate from
	// LineSystem because both used to be drawn the same grey, and an error that
	// looks like a log line is an error people scroll past - "my message didn't
	// send" is exactly the case where the reason was on screen the whole time.
	LineError
)

// Incoming is the message a transport sends when a line arrives. It is a plain
// value so the web bridge can build one from a wire frame and a test can build
// one from a literal.
type Incoming Line

// PreviewReady is the message that a link's card has been fetched. It is a message and
// not a field on a frame because a card is a line: the model appends it, and every front
// end shows it because every front end renders the model. The URL is what identifies it
// - a fetch can finish after its window is gone.
type PreviewReady struct {
	Window string
	URL    string
	Title  string
	Site   string
}

// Nicklist is the message a transport sends when the membership of a window
// changes. Nicks are sorted and carry their prefix modes ("@", "+", "") so the
// renderer can show who is opped without a second lookup.
//
// It is a message of its own rather than a Line because membership is not
// conversation: it has no scrollback, it is replaced wholesale, and a window shows
// exactly one of them.
type Nicklist struct {
	Window string
	Nicks  []Nick
}

// Nick is one member of a channel: a name and its highest prefix mode, "" for an
// ordinary member.
//
// The JSON tags are lowercase because this type is also what travels to the
// browser. Without them Go marshals "Name"/"Prefix", the client reads
// name/prefix, and the nicklist renders as "undefined" while every test that only
// inspects the Go side passes.
type Nick struct {
	Name   string `json:"name"`
	Prefix string `json:"prefix,omitempty"`
}

// Topic is the message a transport sends when a channel's topic is known or
// changes. It is a message of its own because a topic is replaced rather than
// appended: putting it in the scrollback would leave every old topic on screen.
type Topic struct {
	Window string
	Text   string
}

// ChannelModes is the message a transport sends when a channel's mode string is
// known or changes. It is state: "+nt" is replaced, not appended.
type ChannelModes struct {
	Window string
	Modes  string
}

// Sent is the message a transport sends when the user submitted a line.
type Sent struct{ Text string }

// Model is the whole UI. It is a tea.Model and nothing more: three drivers
// instantiate it, none of them subclass it.
type Model struct {
	width  int
	height int
	// history is every line this client has sent, oldest first, and histPos is where the
	// up/down arrows are in it. histPos == len(history) means "not browsing": the input line is
	// whatever the user typed. A saved draft is kept for that state so pressing Up and then Down
	// again returns the half-typed message rather than erasing it - losing what you were writing
	// because you looked for an earlier line is the classic way a history feature earns hate.
	history []string
	histPos int
	draft   string

	// completion is where Tab is in its cycle.
	completion completionState

	windows map[string][]Line
	active  string
	order   []string

	// unread counts lines that arrived in a window the user was not looking at,
	// and mention marks the subset addressed to us by name. They are separate
	// because they answer different questions: "is there anything new" and "is
	// there anything for me" - and the second is the one worth a colour.
	unread  map[string]int
	mention map[string]bool

	// nicks is the membership of each channel, replaced wholesale by a Nicklist
	// message. It is separate from windows because membership is not scrollback:
	// it has no history, and a channel window shows exactly one of them.
	nicks map[string][]Nick

	// topics is the current topic per channel. Empty means either "no topic" or
	// "not told yet"; the two are not distinguished because both render as an
	// absent header, and inventing a third state would be a distinction with no
	// visible difference.
	topics map[string]string

	// modes is the channel mode string per window ("+nt"), shown in the header next
	// to the topic: the two together are "what is this channel", and a user who
	// cannot see that a channel is +m cannot tell that their message failed for a
	// reason.
	modes map[string]string

	input   []rune
	scroll  int
	focused bool

	// settings is the client's own configuration, editable with /fset and shown on the
	// settings screen. It is the client's, not the bouncer's: a bouncer setting changes
	// what happens on the wire for every attached client, and these change what this
	// terminal looks like. Mixing the two in one namespace is how a user ends up
	// surprised that unchecking something in a browser changed their phone.
	settings Settings

	// showSettings draws the settings screen instead of the conversation. It is a mode,
	// not a window: it has no buffer, nothing can arrive in it, and leaving it must
	// return to exactly the window and scroll position it covered.
	showSettings bool

	// settingsRow is the selected row on the settings screen.
	settingsRow int

	// settingsPath is where this client's settings are persisted. Empty means "do not
	// persist", which is what a test and the headless renderer want.
	settingsPath string

	// Connected is the transport's report, not a guess: the status bar must
	// never claim a link the transport has not got.
	connected bool
	status    string
}

// New returns a model showing one window. window is the initial window name
// ("status" for the server window).
func New(window string) Model {
	m := Model{
		windows:   make(map[string][]Line),
		active:    window,
		order:     []string{window},
		width:     80,
		height:    24,
		connected: true,
		focused:   true,
		// The defaults, not the zero value: a zero Settings is a client with timestamps,
		// day rules and nick colours all off, which is a client nobody would recognise as
		// the configurable one.
		settings: DefaultSettings(),
		unread:   make(map[string]int),
		mention:  make(map[string]bool),
		nicks:    make(map[string][]Nick),
		topics:   make(map[string]string),
		modes:    make(map[string]string),
	}
	m.windows[window] = nil
	return m
}

// Init satisfies tea.Model. There is no timer: the model is driven entirely by
// messages, which is what lets a test drive it without a clock.
func (m Model) Init() tea.Cmd { return nil }

// Active returns the window currently shown.
func (m Model) Active() string { return m.active }

// SetActive switches to a named window, remembering it in the order list so a
// window can be selected before any line has arrived in it. A web request knows
// a window name rather than a keystroke, and without this the browser could only
// ever switch between windows that already had traffic.
func (m Model) SetActive(name string) Model {
	if name == "" {
		return m
	}
	if _, seen := m.windows[name]; !seen {
		m.windows[name] = nil
		m.order = append(m.order, name)
	}
	m.active = name
	m.scroll = 0
	// Looking at a window is the only thing that clears it: not reading it, not
	// scrolling it, just having it open.
	delete(m.unread, name)
	delete(m.mention, name)
	return m
}

// Input returns the current input line, for a driver that echoes it elsewhere.
func (m Model) Input() string { return string(m.input) }

// Connected reports whether the model is being told the transport is up.
func (m Model) Connected() bool { return m.connected }

// Update satisfies tea.Model. A driver that owns the concrete value (the web
// bridge, a test) calls Step instead: tea.Model is the interface the SSH
// Program needs, not the one the other two drivers want.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.Step(msg)
	return next, cmd
}

// Step is Update returning the concrete type, so a driver can loop without a
// type assertion per message.
func (m Model) Step(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.MouseMsg:
		// The wheel scrolls the conversation. A terminal's scrollback is the emulator's,
		// but this program runs on the alt screen, where there is no emulator scrollback
		// to fall back on - so a wheel event has to move the model's own scroll or the
		// wheel does nothing at all. Three lines a notch is the usual feel.
		if msg.Action == tea.MouseActionPress {
			switch msg.Button {
			case tea.MouseButtonWheelUp:
				return m.scrollBy(3), nil
			case tea.MouseButtonWheelDown:
				return m.scrollBy(-3), nil
			}
		}
		// A click in the window list selects that window. The model drew the list, so
		// the model is what knows where it is; a click handler in the page would be a
		// second copy of the layout, and the two would disagree the first time a
		// breakpoint moved.
		//
		// Only a press, and only the left button: a wheel notch is also a MouseMsg with
		// coordinates, and treating it as a click would switch window every time the user
		// scrolled over the sidebar.
		if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
			if win := m.WindowAt(msg.X, msg.Y); win != "" {
				return m.SetActive(win), nil
			}
		}
		return m, nil

	case PreviewReady:
		// A card is decoration appended under the line that carried the link. It never
		// counts as unread and it never becomes active: nobody wants a channel to light
		// up because a stranger's title tag loaded slowly.
		m.append(Line{
			Window: msg.Window,
			Kind:   LinePreview,
			Text:   previewText(msg.Title, msg.Site, msg.URL),
		})
		return m, nil

	case ChannelModes:
		if m.modes == nil {
			m.modes = make(map[string]string, 4)
		}
		m.modes[msg.Window] = msg.Modes
		return m, nil

	case Topic:
		if m.topics == nil {
			m.topics = make(map[string]string, 4)
		}
		m.topics[msg.Window] = msg.Text
		return m, nil

	case Nicklist:
		if m.nicks == nil {
			m.nicks = make(map[string][]Nick, 4)
		}
		// Replace, never merge: the transport sends the complete list at 366, so a
		// merge would keep people who left.
		if len(msg.Nicks) == 0 {
			delete(m.nicks, msg.Window)
		} else {
			m.nicks[msg.Window] = msg.Nicks
		}
		return m, nil

	case Incoming:
		line := Line(msg)
		// Our own line coming back from the bouncer confirms the pending echo instead of
		// being drawn beside it. A bouncer echoes the sender - that is how a second
		// client sees the same conversation - so anything else shows every message twice.
		if line.Kind == LineSelfJoin {
			// Switch to the channel we just joined. This is what every IRC client does, and
			// not doing it is why a message sent after /join turned into "421 Unknown
			// command": the view stayed on status, and a bare line there is a bouncer
			// command.
			m.append(line)
			m.active = line.Window
			m.scroll = 0
			return m, nil
		}
		// A highlight word is a mention. It is decided here rather than in the transport
		// because the words are this client's settings: the transport is shared by every
		// front end of this account, and a highlight set in a browser must not silently
		// change what a phone considers interesting.
		if !line.Mention && !line.Own && m.highlighted(line.Text) {
			line.Mention = true
		}
		if !line.Pending {
			if m.confirmPending(line) {
				if line.Mention {
					m = m.NoteMention(line.Window)
				}
				return m, nil
			}
		}
		m.append(line)
		if line.Mention {
			m = m.NoteMention(line.Window)
		}
		return m, nil

	case Sent:
		text := strings.TrimSpace(msg.Text)
		if text == "" {
			return m, nil
		}
		// A submitted line is echoed as a line of our own: the model is not the
		// place that decides whether the transport accepted it, so this is a
		// local echo and nothing more.
		m.append(Line{Window: m.active, Nick: "me", Text: text, Kind: LineMessage, Pending: true, Time: time.Now()})
		m.input = m.input[:0]
		return m, nil

	case tea.KeyMsg:
		return m.updateKey(msg)
	}
	return m, nil
}

func (m Model) updateKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	// The settings screen owns the keyboard while it is up. It is a mode, not a window: a
	// keystroke meant for it must not reach the input box behind it, and returning to the
	// conversation has to restore exactly what it covered.
	if m.showSettings {
		return m.updateSettingsKey(msg), nil
	}
	switch msg.Type {
	case tea.KeyEnter:
		text := strings.TrimSpace(string(m.input))
		m.input = m.input[:0]
		if text == "" {
			return m, nil
		}
		// Remember it for the up arrow. Recorded at submit rather than at send, because the
		// model is where the user's intent exists: a line that then fails to reach the wire is
		// still a line they typed and will want back to fix.
		m = m.rememberInput(text)
		// An alias is expanded before anything is parsed, so "/j #x" and "/join #x" are the
		// same command with one implementation rather than two.
		text = m.settings.expand(text, 0)
		if name, rest, ok := slash(text, "fset", "set"); ok {
			return m.handleFset(name, rest), nil
		}
		if _, rest, ok := slash(text, "alias"); ok {
			return m.handleAlias(rest), nil
		}
		if _, _, ok := slash(text, "settings", "prefs"); ok {
			// The screen, not the command: /fset sets one value, /settings shows them all
			// and is the discoverable half.
			m.showSettings = true
			m.settingsRow = 0
			return m, nil
		}
		// "/go pattern" switches windows. It is resolved here rather than sent, because the
		// window list is the model's: a bouncer has no idea which windows this client is
		// showing, so a command sent upstream could only ever be wrong. Fuzzy rather than
		// exact because the point of the command is to avoid typing the whole name, and a
		// prefix test would make "/go #sal" and "/go salotto" behave differently for no
		// reason a user can see.
		if arg, ok := goCommand(text); ok {
			if name, found := m.matchWindow(arg); found {
				return m.SetActive(name), nil
			}
			if arg == "" {
				return m, nil
			}
			return m.appendSystem("no window matches " + arg + "\n"), nil
		}
		// "/search pattern" searches across every window in memory and shows the hits as a
		// list of jumps. It is resolved here for the same reason /go is: the scrollback lives
		// in the client, so a command sent upstream could only return what the bouncer's
		// ring holds rather than what this client has shown.
		//
		// The result is a list rather than a jump because search has several answers and
		// picking one for the user is a guess. Each row names its window, so the next /go
		// reaches it.
		if query, ok := searchCommand(text); ok {
			return m.searchIntoWindow(query), nil
		}
		if text == "/close" {
			// Closing is the mirror of /go and needs the same knowledge of the window list.
			return m.closeActive(), nil
		}
		return m, func() tea.Msg { return Sent{Text: text} }

	case tea.KeyBackspace:
		if n := len(m.input); n > 0 {
			m.input = m.input[:n-1]
		}
		return m, nil

	case tea.KeyEsc:
		m.input = m.input[:0]
		return m, nil

	case tea.KeyUp:
		// Up recalls the last thing you sent in this window, which is what every client does
		// and what a thumb expects. Scrolling moved to PgUp/PgDn for exactly that reason: the
		// two cannot share a key, and between "scroll the buffer" and "get my last message
		// back" only one of them is something a user does a hundred times a session.
		return m.historyPrev(), nil

	case tea.KeyDown:
		return m.historyNext(), nil

	case tea.KeyPgUp:
		if m.scroll < len(m.windows[m.active]) {
			m.scroll += m.settings.ScrollStep
		}
		return m, nil

	case tea.KeyPgDown:
		if m.scroll > 0 {
			m.scroll -= m.settings.ScrollStep
		}
		if m.scroll < 0 {
			m.scroll = 0
		}
		return m, nil

	case tea.KeyTab:
		return m.complete(), nil

	case tea.KeyCtrlC:
		return m, tea.Quit

	case tea.KeyCtrlN:
		return m.cycle(1), nil

	case tea.KeyCtrlP:
		return m.cycle(-1), nil

	case tea.KeyRunes:
		// Alt-A jumps to the next window with unread activity, which is what weechat calls
		// the hotlist. On Alt rather than a bare key because it is a jump, and a jump on a
		// bare letter would fight the input line.
		if msg.Alt && len(msg.Runes) == 1 && (msg.Runes[0] == 'a' || msg.Runes[0] == 'A') {
			return m.hotlistNext(), nil
		}
		m.input = append(m.input, msg.Runes...)
		return m, nil

	case tea.KeySpace:
		// bubbletea reports a space as KeySpace, not as a KeyRunes event carrying " ".
		// Accepting only KeyRunes drops every space, so typing "hello there" produced
		// "hellothere" and "/join #chan" produced "/join#chan" - and the message went out
		// mangled rather than failing, which is the worst way for an input bug to behave.
		m.input = append(m.input, ' ')
		return m, nil
	}
	return m, nil
}

func (m Model) cycle(delta int) Model {
	if len(m.order) < 2 {
		return m
	}
	idx := 0
	for i, name := range m.order {
		if name == m.active {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(m.order)) % len(m.order)
	m.active = m.order[idx]
	m.scroll = 0
	return m
}

// complete is Tab: it completes what the cursor is on, cycling through candidates on repeat.
//
// Three things can be completed, decided by where the word is rather than by asking the user:
//
//   - the first word, if it starts with "/", is a command name (and then a channel for /join);
//   - a word starting with "#" is a channel;
//   - anything else after a command's argument position is a nick, because that is the only thing
//     an argument is likely to be. For a PRIVMSG the first argument is the target (nick or
//     channel) and the rest is the message, which must never be completed - completing the body of
//     a sentence would rewrite what the user is saying.
//
// Cycles rather than guesses: a second Tab with the same prefix advances to the next candidate,
// and the cycle is keyed on the word it started from so it cannot drift as the word grows.
func (m Model) complete() Model {
	text := string(m.input)
	// The word under the cursor: from the last space before the cursor to the cursor.
	start := strings.LastIndexByte(text, ' ') + 1
	word := text[start:]
	atStart := start == 0

	if atStart && strings.HasPrefix(word, "/") {
		cands := m.commandCandidates(word)
		return m.applyCompletion(text, start, word, cands, word)
	}
	if strings.HasPrefix(word, "#") {
		cands := m.channelCandidates(word)
		return m.applyCompletion(text, start, word, cands, word)
	}
	if atStart {
		// A bare first word with no slash is a nick to talk to (/query is the explicit form),
		// but completing it would silently turn "hello" into a nick. Left alone.
		return m
	}
	if !m.completesNicks(text[:start]) {
		return m
	}
	cands := m.nickCandidates(word)
	return m.applyCompletion(text, start, word, cands, word)
}

// applyCompletion replaces the word with the next candidate. key identifies the cycle, so that
// repeated Tab with the same prefix advances while a changed prefix starts over.
func (m Model) applyCompletion(text string, start int, word string, cands []string, key string) Model {
	if len(cands) == 0 {
		m.completion = completionState{}
		return m
	}
	idx := 0
	// Continuing a cycle is decided by what is on screen, not by what the user has typed since:
	// after a completion the word *is* the candidate that was just chosen. So a word that is one
	// of the last cycle's candidates continues that cycle, wherever the cursor is now.
	if len(m.completion.cands) > 0 && contains(m.completion.cands, word) {
		cands = m.completion.cands
		idx = (m.completion.idx + 1) % len(cands)
	}
	choice := cands[idx]
	// Replace the whole token, and swallow anything after the cursor within the word, so a
	// completion never leaves a tail behind.
	rest := text[start+len(word):]
	m.input = []rune(text[:start] + choice + rest)
	m.completion = completionState{prefix: key, cands: cands, idx: idx}
	return m
}

// completesNicks reports whether the word at the cursor is in a nick position for the command
// already typed. The message body of a PRIVMSG is not, and neither is the text after /msg <target>.
func (m Model) completesNicks(prefix string) bool {
	fields := strings.Fields(prefix)
	if len(fields) == 0 {
		return false
	}
	// A bare word with no command is being typed as a message to the active channel.
	if !strings.HasPrefix(fields[0], "/") {
		return false
	}
	switch strings.ToLower(fields[0]) {
	case "/msg", "/query", "/notice", "/whois", "/kick", "/op", "/deop", "/voice", "/invite":
		// The first argument is a nick or channel; after that it is text.
		return len(fields) == 1
	case "/join", "/part", "/topic", "/mode":
		return false
	}
	return false
}

// modelCommands are the commands this client resolves itself. They are listed here rather than
// derived from the dispatch because the dispatch is a chain of ifs, not a table, and inventing a
// table for the benefit of Tab would mean two lists to keep in step - the failure mode the /fset
// screen already had once.
var modelCommands = []string{
	"/alias", "/close", "/fset", "/find", "/go", "/help", "/query", "/search", "/settings",
}

// wireCommands are passed through to the bouncer with the leading slash stripped. They are listed
// for completion only: the client does not implement them and must not pretend to.
var wireCommands = []string{
	"/adminlog", "/away", "/back", "/cap", "/chathistory", "/deop", "/devoice", "/invite",
	"/join", "/kick", "/list", "/me", "/mode", "/msg", "/nick", "/notice", "/op", "/part",
	"/quit", "/topic", "/voice", "/who", "/whois",
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// commandCandidates lists command names starting with the given prefix.
func (m Model) commandCandidates(prefix string) []string {
	prefix = strings.ToLower(prefix)
	var out []string
	for _, c := range append(append([]string{}, modelCommands...), wireCommands...) {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

// channelCandidates lists windows whose name starts with the prefix, plus "#" itself so a bare
// hash can grow into the list.
func (m Model) channelCandidates(prefix string) []string {
	var out []string
	for name := range m.windows {
		if strings.HasPrefix(name, prefix) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// nickCandidates lists the nicks known in the active window.
func (m Model) nickCandidates(prefix string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, n := range m.nicks[m.active] {
		if n.Name != "" && strings.HasPrefix(n.Name, prefix) && !seen[n.Name] {
			seen[n.Name] = true
			out = append(out, n.Name)
		}
	}
	sort.Strings(out)
	return out
}

// rememberInput appends a submitted line to the history and resets the browse position.
//
// A repeat of the line before it is not stored twice: the up arrow doing nothing is a better
// answer than it producing the same message twice, which reads as a broken key.
func (m Model) rememberInput(text string) Model {
	if len(m.history) > 0 && m.history[len(m.history)-1] == text {
		m.histPos = len(m.history)
		m.draft = ""
		return m
	}
	m.history = append(m.history, text)
	// The history is unbounded by design in the sense that it is the client's session, but an
	// unbounded slice is still a leak on a long-lived session, so it is capped the way the
	// scrollback is.
	const maxHistory = 500
	if len(m.history) > maxHistory {
		m.history = m.history[len(m.history)-maxHistory:]
	}
	m.histPos = len(m.history)
	m.draft = ""
	return m
}

// historyPrev walks back through what was sent. histPos == len(history) is the live input line; the
// text there is saved as a draft on the way in so historyNext can restore it.
//
// The draft is the reason this is not a one-line slice index: a user who types half a message, taps
// up to check something, and taps down again expects their half-message back, not an empty box.
func (m Model) historyPrev() Model {
	if len(m.history) == 0 {
		return m
	}
	if m.histPos == len(m.history) {
		m.draft = string(m.input)
	}
	if m.histPos > 0 {
		m.histPos--
	}
	m.input = []rune(m.history[m.histPos])
	m.scroll = 0
	return m
}

// historyNext walks forward, and past the newest line it restores the draft.
func (m Model) historyNext() Model {
	if len(m.history) == 0 {
		return m
	}
	if m.histPos >= len(m.history) {
		return m
	}
	m.histPos++
	if m.histPos >= len(m.history) {
		m.input = []rune(m.draft)
		m.draft = ""
		return m
	}
	m.input = []rune(m.history[m.histPos])
	m.scroll = 0
	return m
}

// scrollBy moves the conversation by n lines, clamped to the buffer. Positive scrolls up
// into history. The clamp is at both ends because an unclamped scroll is a blank window the
// user cannot get out of without scrolling back by exactly the same amount.
func (m Model) scrollBy(n int) Model {
	m.scroll += n
	if m.scroll < 0 {
		m.scroll = 0
	}
	if top := len(m.windows[m.active]); m.scroll > top {
		m.scroll = top
	}
	return m
}

// goCommand parses "/go <pattern>". The argument is everything after the verb, spaces and
// all: a window name can contain spaces in a query window, so cutting at the first space
// would silently search for half of what the user typed.
func goCommand(text string) (arg string, ok bool) {
	body := strings.TrimPrefix(text, "/")
	verb, rest, _ := strings.Cut(body, " ")
	if !strings.EqualFold(verb, "go") {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// searchCommand parses "/search <pattern>". Like /go, the pattern is everything after the verb:
// a phrase with spaces is the common case and cutting at the first space would search for a word
// the user did not ask for.
func searchCommand(text string) (query string, ok bool) {
	body := strings.TrimPrefix(text, "/")
	verb, rest, _ := strings.Cut(body, " ")
	if !strings.EqualFold(verb, "search") && !strings.EqualFold(verb, "find") {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// matchWindow fuzzy-matches a window name. It scores rather than filters, so "/go sat"
// finds "#salotto" and "/go st" finds "status" without either being a special case. An
// exact match always wins, and an empty pattern matches nothing: "/go" alone is a user who
// has not finished typing, and jumping to an arbitrary window would be worse than staying.
func (m Model) matchWindow(pattern string) (string, bool) {
	if pattern == "" {
		return "", false
	}
	lower := strings.ToLower(pattern)
	best, bestScore := "", -1
	for _, name := range m.order {
		nameLower := strings.ToLower(name)
		score := fuzzyScore(strings.TrimPrefix(nameLower, "#"), lower)
		// The name with its "#" included is also tried, so "/go #sal" and "/go sal" are the
		// same search; a user should not have to know which one this program stores.
		if s := fuzzyScore(nameLower, lower); s > score {
			score = s
		}
		// The comparison is on the name without its "#" as well, or no channel is ever an
		// exact match: the stored name is "#sal" and nobody types the sigil.
		if nameLower == lower || strings.TrimPrefix(nameLower, "#") == strings.TrimPrefix(lower, "#") {
			score = 1 << 20
		}
		if score > bestScore {
			best, bestScore = name, score
		}
	}
	return best, bestScore >= 0
}

// fuzzyScore reports how well pattern matches text as a subsequence, higher is better, and
// -1 when it does not match at all. Matching in order but not contiguously is what makes
// the command useful for long channel names; the bonuses prefer a prefix run (the common
// case: "/go sal" for "#salotto") over scattered letters.
func fuzzyScore(text, pattern string) int {
	if pattern == "" {
		return 0
	}
	ti, pi, score, run := 0, 0, 0, 0
	for ti < len(text) && pi < len(pattern) {
		if text[ti] == pattern[pi] {
			score += 2
			if run == pi {
				// Every character so far has been contiguous from the start.
				score += 3
			}
			if ti == 0 {
				score += 4
			}
			run++
			pi++
			ti++
			continue
		}
		run = 0
		ti++
		score -= 1
	}
	if pi < len(pattern) {
		return -1
	}
	return score
}

// closeActive removes the current window from the list without telling the server. It is the
// client-side "I am done looking at this", for a query window whose conversation is over or a
// channel someone else already parted; a PART is a different thing and still has to be typed.
func (m Model) closeActive() Model {
	name := m.active
	if len(m.order) <= 1 {
		return m
	}
	out := make([]string, 0, len(m.order))
	for _, w := range m.order {
		if w != name {
			out = append(out, w)
		}
	}
	delete(m.windows, name)
	delete(m.unread, name)
	delete(m.mention, name)
	delete(m.nicks, name)
	m.order = out
	idx := 0
	for i, w := range m.order {
		if w == m.active {
			idx = i
			break
		}
	}
	if idx >= len(m.order) {
		idx = len(m.order) - 1
	}
	m.active = m.order[idx]
	m.scroll = 0
	return m
}

// appendSystem puts a note in the active window. It is used for the client's own refusals -
// "no window matches" and friends - which are not chat and must not be sent anywhere.
func (m Model) appendSystem(text string) Model {
	m.append(Line{Window: m.active, Text: text, Kind: LineSystem})
	return m
}

// muted reports whether a nick is on this client's mute list.
func (m Model) muted(nick string) bool {
	if nick == "" {
		return false
	}
	for _, w := range m.settings.Mutes {
		if strings.EqualFold(w, nick) {
			return true
		}
	}
	return false
}

// highlighted reports whether text contains one of this client's highlight words.
func (m Model) highlighted(text string) bool {
	return matchedBy(text, m.settings.Highlights)
}

// slash reports whether text is "/<name...>" with nothing else, and returns the remainder.
// It is the one place a client-side command word is compared, so "/fset" and "/set" cannot
// drift into two behaviours.
func slash(text string, names ...string) (name, rest string, ok bool) {
	if !strings.HasPrefix(text, "/") {
		return "", "", false
	}
	body := strings.TrimPrefix(text, "/")
	word, tail, _ := strings.Cut(body, " ")
	for _, n := range names {
		if strings.EqualFold(word, n) {
			return word, strings.TrimSpace(tail), true
		}
	}
	return "", "", false
}

// handleFset sets one client setting: "/fset timestamps off", "/fset scroll_step 5", and a
// bare "/fset" to list them, which is what makes the command discoverable.
func (m Model) handleFset(name, rest string) Model {
	if rest == "" {
		for _, row := range m.settings.rows() {
			m.appendSystem(fmt.Sprintf("%-14s %s", row.label, row.value))
		}
		m.appendSystem("use /fset <name> <value>; /settings shows the full screen")
		return m
	}
	key, value, hasValue := strings.Cut(rest, " ")
	if !hasValue {
		value = ""
	}
	if err := m.settings.set(key, value); err != nil {
		return m.appendSystem(err.Error())
	}
	m.saveSettings()
	return m.appendSystem(fmt.Sprintf("%s is now %s", key, valueOf(m.settings, key)))
}

// handleAlias adds an alias, or lists them. Listing on a bare invocation matters: an alias
// nobody can remember is an alias nobody uses.
func (m Model) handleAlias(rest string) Model {
	if rest == "" {
		for _, line := range m.settings.aliasList() {
			m.appendSystem(line)
		}
		return m
	}
	if err := m.settings.applyAlias(rest); err != nil {
		return m.appendSystem(err.Error())
	}
	m.saveSettings()
	return m.appendSystem("alias set: " + rest)
}

// valueOf reads a setting back for the confirmation line. It is a second switch over the
// same keys, which is the price of not making the settings table reflective; the test that
// walks rows() is what keeps the two in step.
// valueOf reads a setting back for the confirmation line /fset prints.
//
// It reads the one table instead of switching on keys a second time. The copy this
// replaced had fallen behind the newer keys, so "/fset highlight salotto" answered
// "highlight is now ?" - and a confirmation that lies is worse than none, because it
// teaches the user to stop reading it. A test caught the drift; having two tables is what
// made the drift possible, so there is now one, and canonical() is the only place an
// alias is resolved.
func valueOf(s Settings, key string) string {
	want := canonical(key)
	for _, row := range s.rows() {
		if row.key == want {
			return row.value
		}
	}
	return "?"
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// hotlistNext jumps to the next window with unread activity, wrapping past the end. It is
// the hotlist: the point is not to show what is unread but to take you there, and a jump that
// stopped at the bottom would leave the user scrolling the sidebar by hand.
func (m Model) hotlistNext() Model {
	if !m.settings.Hotlist || len(m.order) < 2 {
		return m
	}
	cur := 0
	for i, name := range m.order {
		if name == m.active {
			cur = i
			break
		}
	}
	for step := 1; step <= len(m.order); step++ {
		idx := (cur + step) % len(m.order)
		name := m.order[idx]
		if idx == cur {
			continue
		}
		if m.unread[name] > 0 || m.mention[name] {
			return m.SetActive(name)
		}
	}
	return m
}

// updateSettingsKey drives the settings screen. Up and down move, space and enter toggle a
// boolean, left and right change a number, and escape or q leaves.
func (m Model) updateSettingsKey(msg tea.KeyMsg) Model {
	rows := m.settings.rows()
	if m.settingsRow >= len(rows) {
		m.settingsRow = len(rows) - 1
	}
	if m.settingsRow < 0 {
		m.settingsRow = 0
	}
	row := rows[m.settingsRow]
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		m.showSettings = false
		return m
	case tea.KeyUp:
		if m.settingsRow > 0 {
			m.settingsRow--
		}
		return m
	case tea.KeyDown:
		if m.settingsRow < len(rows)-1 {
			m.settingsRow++
		}
		return m
	case tea.KeyLeft:
		if row.kind == "int" {
			_ = m.settings.set(row.key, strconv.Itoa(max(1, m.settings.ScrollStep-1)))
			m.saveSettings()
		}
		return m
	case tea.KeyRight:
		if row.kind == "int" {
			_ = m.settings.set(row.key, strconv.Itoa(m.settings.ScrollStep+1))
			m.saveSettings()
		}
		return m
	case tea.KeyEnter, tea.KeySpace:
		_ = m.settings.set(row.key, "")
		m.saveSettings()
		return m
	case tea.KeyRunes:
		if len(msg.Runes) == 1 && (msg.Runes[0] == 'q' || msg.Runes[0] == 'Q') {
			m.showSettings = false
		}
		return m
	}
	return m
}

// saveSettings persists the settings next to the client's other state. A failure is reported
// in the window rather than ignored: a setting that looks saved and is not is worse than one
// that refused.
func (m Model) saveSettings() {
	if m.settingsPath == "" {
		return
	}
	if err := SaveSettings(m.settingsPath, m.settings); err != nil {
		m.appendSystem("settings not saved: " + err.Error())
	}
}

// WithSettings gives the model a settings file to load and save. It is a separate call rather
// than a constructor argument because every existing caller - tests, the SSH server, the PTY
// child - is entitled to the defaults without knowing where they live on disk.
func (m Model) WithSettings(path string) Model {
	m.settingsPath = path
	m.settings = LoadSettings(path)
	return m
}

// confirmPending finds the local echo this line confirms and rewrites it in place.
//
// It matches on window, text and the sending nick being ours ("me" is the local echo's
// placeholder). The match is the most recent pending line with the same text in the same
// window, because sending the same thing twice is a thing people do and the first echo
// belongs to the first send.
func (m Model) confirmPending(line Line) bool {
	// Only our own line can confirm a local echo. Matching on text alone would let the
	// first person to repeat what we said delete our message, which is a bug you cannot
	// see until someone says "yes" after you did.
	if !line.Own {
		return false
	}
	lines := m.windows[line.Window]
	for i := len(lines) - 1; i >= 0; i-- {
		l := lines[i]
		if !l.Pending || l.Text != line.Text || l.Kind != line.Kind {
			continue
		}
		lines[i] = line
		return true
	}
	return false
}

// append adds a line to its window and makes that window known. Auto-scroll
// stays pinned to the bottom unless the user scrolled up.
func (m *Model) append(l Line) {
	if l.Window == "" {
		l.Window = m.active
	}
	if _, seen := m.windows[l.Window]; !seen {
		m.order = append(m.order, l.Window)
	}
	// A new day gets a rule in the conversation, not a decoration in the renderer. It is
	// inserted before the first line of the day, which is the only place where "this is a
	// different day" is true no matter how far the user scrolls.
	if !l.Time.IsZero() && l.Kind != LineDay {
		if prev := m.lastTime(l.Window); !prev.IsZero() && !sameDay(prev, l.Time) {
			m.windows[l.Window] = append(m.windows[l.Window], Line{
				Window: l.Window,
				Kind:   LineDay,
				Text:   l.Time.Format("Monday 2 Jan 2006"),
				Time:   l.Time,
			})
		}
	}
	m.windows[l.Window] = append(m.windows[l.Window], l)
	// Unread is a count of things you did not see arrive. A line in the window you
	// are looking at was seen, so it does not count; that is what makes the number
	// meaningful rather than a total.
	//
	// A preview never counts. It is decoration attached to a message that already
	// counted, and a channel lighting up because a stranger's title tag loaded slowly
	// is the cheapest way to make the number untrustworthy.
	//
	// A muted nick does not count. The line is still drawn, because hiding it would leave
	// a channel that looks idle while somebody is addressing you - "why did nobody answer"
	// is a worse failure than "that person is noisy". Mute suppresses the signal, never
	// the text.
	if l.Window != m.active && l.Kind != LineGap && l.Kind != LinePreview && !m.muted(l.Nick) {
		if m.unread == nil {
			m.unread = make(map[string]int)
		}
		m.unread[l.Window]++
	}
	if m.scroll > 0 {
		m.scroll++
	}
}

// lastTime is the time of the newest line in a window that has one. Used to decide
// whether an arriving line starts a new day.
func (m *Model) lastTime(window string) time.Time {
	lines := m.windows[window]
	for i := len(lines) - 1; i >= 0; i-- {
		if !lines[i].Time.IsZero() {
			return lines[i].Time
		}
	}
	return time.Time{}
}

// sameDay compares two instants by calendar day in the same location. Comparing hours is
// wrong across a day boundary and comparing UTC days is wrong for anyone not on UTC, which
// would put the rule an evening early or late depending on the season.
func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// Styles are built once: lipgloss resolves colours lazily, so the profile set
// by the caller before the first View applies. Every colour is a 256-palette
// index for the reason documented in docs/UI.md: the HTML converter maps
// 8-bit SGR to classes and drops 24-bit colour silently.
var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	// mentionStyle is the same 256-palette idea: a warm colour for a line that
	// addresses us, so it stands out without a background fill that would fight
	// the terminal's own theme.
	mentionStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("222")).Bold(true)
	// opStyle marks channel operators in the nick column, in the same palette.
	opStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("114"))
	// topicStyle is deliberately dimmer than a nickname: a topic is context, not
	// conversation, and it is on screen constantly.
	topicStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	borderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("63"))
	nickStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("212"))
	// eventStyle is the dimmest thing on screen on purpose: a join is not news, and
	// a channel where every arrival is loud cannot be read. The arrows say what
	// happened; the colour says it does not matter.
	eventStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("242"))
	topicEventStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("180"))
	kickStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("174"))
	// A preview is decoration, so it is dimmer than a message and cannot be mistaken
	// for something a person said.
	previewStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Italic(true)
	timeStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	// dayStyle is the date rule: present enough to find while scrolling, quiet enough that
	// it does not compete with the conversation on either side of it.
	dayStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("109"))
	bodyStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("188"))
	actionStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("176")).Italic(true)
	noticeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("179"))
	systemStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	// errorStyle is red, and used for nothing else: a terminal has one channel for
	// "this went wrong" and spending it on anything else is how it stops meaning it.
	errorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	gapStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	inputStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("231"))
	barStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("250")).Background(lipgloss.Color("236"))
)

// View renders the whole screen: a titled box with the scrollback and an input
// line, then a status bar. It is the only rendering code in the program.
// layout is the geometry of one full-screen render. It exists as a value because
// the mouse handler has to agree with the renderer about where the window list is:
// computing the columns twice is how a click selects the wrong channel, and it is the
// kind of bug that only shows up on a screen size nobody tested.
type layout struct {
	width, height       int
	winWidth, nickWidth int
	inner               int
	contentRows         int
	// topicRow is true when the active window's topic is drawn on its own line. The
	// renderer and the mouse handler share this value, so a click lands on the row the
	// user sees rather than one row off.
	topicRow bool
}

func (m Model) layout() layout {
	width := m.width
	if width < 20 {
		width = 20
	}
	height := m.height
	if height < 6 {
		height = 6
	}
	l := layout{width: width, height: height}

	// The window list needs room to be readable. Under 72 columns it is dropped
	// rather than squeezed: a sidebar of four characters is a column of dots, and the
	// keyboard still switches windows.
	if len(m.order) > 0 && width >= 72 {
		l.winWidth = 20
		if width < 100 {
			l.winWidth = 16
		}
	}
	if len(m.nicks[m.active]) > 0 && width >= 72 {
		l.nickWidth = 18
		if width < 100 {
			l.nickWidth = 14
		}
	}
	l.inner = width - 2
	if l.winWidth > 0 {
		l.inner -= l.winWidth + 1
	}
	if l.nickWidth > 0 {
		l.inner -= l.nickWidth + 1
	}
	if l.inner < 8 {
		l.inner = 8
	}
	// A topic gets a row of its own directly under the title, spanning the full width:
	// a topic is about the channel, not about the conversation column, and squeezing it
	// into the title bar truncated the one piece of text a newcomer needs to read in full.
	l.topicRow = m.topics[m.active] != "" && height >= 10

	// Rows: top border, [topic], content, separator, input, bottom border, status.
	l.contentRows = height - 5
	if l.topicRow {
		l.contentRows--
	}
	if l.contentRows < 1 {
		l.contentRows = 1
	}
	return l
}

// settingsView draws the settings screen: one row per setting, the selected row highlighted.
//
// It is drawn here rather than in the page for the same reason as everything else: the screen
// and the command must agree about what a setting is, and two renderers drift. It is also the
// honest place for it - a browser-only settings panel could not be reached from SSH or a real
// terminal, and a setting you can only change on one of your three front ends is a bug.
func (m Model) settingsView() string {
	rows := m.settings.rows()
	width := m.width
	if width < 40 {
		width = 40
	}
	height := m.height
	if height < 10 {
		height = 10
	}

	inner := width - 4
	lines := make([]string, 0, len(rows))
	for i, row := range rows {
		marker := "  "
		labelStyle := systemStyle
		if i == m.settingsRow {
			marker = "▸ "
			labelStyle = titleStyle
		}
		value := row.value
		if row.kind == "bool" {
			if value == "on" {
				value = "[on]"
			} else {
				value = "[off]"
			}
		}
		label := labelStyle.Render(marker + row.label)
		pad := inner - lipgloss.Width(label) - lipgloss.Width(value)
		if pad < 1 {
			pad = 1
		}
		lines = append(lines, label+strings.Repeat(" ", pad)+bodyStyle.Render(value))
	}

	// The alias list belongs on this screen too: aliases are settings, and a separate screen
	// for five entries would be a screen nobody visits.
	if len(m.settings.Aliases) > 0 {
		lines = append(lines, "")
		lines = append(lines, topicStyle.Render("aliases"))
		for _, a := range m.settings.aliasList() {
			lines = append(lines, systemStyle.Render("  "+a))
		}
	}

	var b strings.Builder
	title := titleStyle.Render("settings")
	b.WriteString(borderStyle.Render("╭─ ") + title)
	m.closeTop(&b, width, "╮")
	b.WriteString("\n")

	// Top border, separator, input, bottom border, status bar: the conversation's own
	// arithmetic, so the settings screen is the same size as the thing it covers.
	room := height - 5
	for i := 0; i < room; i++ {
		b.WriteString(borderStyle.Render("│ "))
		line := ""
		if i < len(lines) {
			line = lines[i]
		}
		b.WriteString(line)
		if pad := inner - lipgloss.Width(line); pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
		}
		b.WriteString(" " + borderStyle.Render("│") + "\n")
	}

	b.WriteString(borderStyle.Render("├" + strings.Repeat("─", width-2) + "┤"))
	b.WriteString("\n")
	b.WriteString(borderStyle.Render("│ "))
	hint := systemStyle.Render("space toggles, ←/→ changes, esc leaves")
	b.WriteString(hint)
	if pad := inner - lipgloss.Width(hint); pad > 0 {
		b.WriteString(strings.Repeat(" ", pad))
	}
	b.WriteString(" " + borderStyle.Render("│") + "\n")
	b.WriteString(borderStyle.Render("╰"+strings.Repeat("─", width-2)+"╯") + "\n")
	b.WriteString(barStyle.Render(" settings · client, not bouncer "))
	if pad := width - lipgloss.Width(" settings · client, not bouncer ") - 2; pad > 0 {
		b.WriteString(barStyle.Render(strings.Repeat(" ", pad)))
	}
	return b.String()
}

// View draws the whole screen: window list, conversation, member list, input line and
// status bar.
//
// The model draws all of it, and the web page is a thin shell around this string. That
// is the rule the three front ends live by: there is exactly one place that knows what
// a window list looks like, and it is here. A browser sidebar built in HTML was a
// second one, and it drifted immediately - the nicks were drawn twice, once by the
// renderer and once by the page.
func (m Model) View() string {
	if m.showSettings {
		return m.settingsView()
	}
	l := m.layout()
	nicks := m.nicks[m.active]
	body := m.bodyLines(l.inner, l.contentRows)
	winCol := m.windowColumn(l.winWidth, l.contentRows)
	nickCol := nickColumn(nicks, l.nickWidth, l.contentRows, m.settings)

	var b strings.Builder

	// Top border, with the active window as the title and the topic and modes after
	// it. Modes first: they are short and survive truncation where a long topic does
	// not.
	b.WriteString(borderStyle.Render("╭─ "))
	b.WriteString(titleStyle.Render(m.active))
	if m.scroll > 0 {
		b.WriteString(systemStyle.Render(fmt.Sprintf(" (scroll %d)", m.scroll)))
	}
	if modes := m.modes[m.active]; modes != "" && modes != "+" {
		b.WriteString(topicStyle.Render("  " + modes))
	}
	m.closeTop(&b, l.width, "╮")
	b.WriteString("\n")

	// The topic row. It spans the whole frame rather than the conversation column: a
	// topic belongs to the channel, and the member list is not a margin for it.
	if l.topicRow {
		topic := m.topics[m.active]
		room := l.width - 4
		b.WriteString(borderStyle.Render("│ ") + topicStyle.Render(truncateTo(topic, room)))
		if pad := room - lipgloss.Width(topicStyle.Render(truncateTo(topic, room))); pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
		}
		b.WriteString(borderStyle.Render(" │"))
		b.WriteString("\n")
	}

	for i := 0; i < l.contentRows; i++ {
		b.WriteString(borderStyle.Render("│"))
		if l.winWidth > 0 {
			b.WriteString(cell(winCol, i, l.winWidth))
			b.WriteString(borderStyle.Render("│"))
		}
		b.WriteString(cell(body, i, l.inner))
		if l.nickWidth > 0 {
			b.WriteString(borderStyle.Render("│"))
			b.WriteString(cell(nickCol, i, l.nickWidth))
		}
		b.WriteString(borderStyle.Render("│"))
		b.WriteString("\n")
	}

	// The separator knows which columns exist, so the frame is one box and not three
	// boxes stacked.
	b.WriteString(borderStyle.Render("├"))
	if l.winWidth > 0 {
		b.WriteString(borderStyle.Render(strings.Repeat("─", l.winWidth) + "┼"))
	}
	b.WriteString(borderStyle.Render(strings.Repeat("─", l.inner)))
	if l.nickWidth > 0 {
		b.WriteString(borderStyle.Render("┼" + strings.Repeat("─", l.nickWidth)))
	}
	b.WriteString(borderStyle.Render("┤"))
	b.WriteString("\n")

	// Input row, spanning the conversation column: typing has nothing to do with the
	// member list.
	b.WriteString(borderStyle.Render("│"))
	if l.winWidth > 0 {
		b.WriteString(strings.Repeat(" ", l.winWidth))
		b.WriteString(borderStyle.Render("│"))
	}
	input := "> " + string(m.input)
	b.WriteString(inputStyle.Render(truncateTo(input, l.inner-1)))
	if pad := l.inner - 1 - lipgloss.Width(inputStyle.Render(input)); pad > 0 {
		b.WriteString(strings.Repeat(" ", pad))
	}
	if pad := l.inner - 1 - lipgloss.Width(input); pad < 0 {
		_ = pad
	}
	if l.nickWidth > 0 {
		b.WriteString(borderStyle.Render("│" + strings.Repeat(" ", l.nickWidth)))
	}
	b.WriteString(borderStyle.Render("│"))
	b.WriteString("\n")

	b.WriteString(borderStyle.Render("╰" + strings.Repeat("─", l.width-2) + "╯"))
	b.WriteString("\n")
	b.WriteString(m.statusBar())
	return b.String()
}

// closeTop fills the rest of the top border and closes it.
func (m Model) closeTop(b *strings.Builder, width int, corner string) {
	head := lipgloss.Width(b.String()) + lipgloss.Width(borderStyle.Render(" "+corner))
	if pad := width - head; pad > 0 {
		b.WriteString(borderStyle.Render(strings.Repeat("─", pad) + corner))
	} else {
		b.WriteString(borderStyle.Render(corner))
	}
}

// cell returns row i of a column, padded to width on display width. Display width and
// not byte length: a nick or a topic with a wide glyph must not push the border out.
func cell(col []string, i, width int) string {
	s := ""
	if i < len(col) {
		s = col[i]
	}
	out := s
	if pad := width - lipgloss.Width(s); pad > 0 {
		out += strings.Repeat(" ", pad)
	}
	return out
}

// windowColumn renders the window list: the active one marked, and a mention or an
// unread count where there is one. It is inside the frame because the whole screen
// belongs to the model - a window list drawn by the page was the second renderer that
// drifted.
func (m Model) windowColumn(width, n int) []string {
	if width <= 2 || n <= 0 {
		return nil
	}
	out := make([]string, 0, n)
	for _, name := range m.order {
		if len(out) >= n {
			break
		}
		marker := "  "
		if name == m.active {
			marker = "▸ "
		}
		badge := ""
		switch {
		case m.mention[name]:
			badge = "@"
		case m.unread[name] > 0:
			badge = fmt.Sprintf("%d", m.unread[name])
		}
		room := width - 2
		if badge != "" {
			room -= 2
		}
		label := truncateTo(name, room)
		line := marker + label
		if badge != "" {
			pad := width - lipgloss.Width(line) - lipgloss.Width(badge)
			if pad > 0 {
				line += strings.Repeat(" ", pad)
			}
			line += badge
		}
		switch {
		case name == m.active:
			out = append(out, titleStyle.Render(line))
		case m.mention[name]:
			out = append(out, mentionStyle.Render(line))
		default:
			out = append(out, bodyStyle.Render(line))
		}
	}
	return out
}

// WindowAt reports which window a click at character cell (x, y) landed on, "" if the
// click was not in the window list. It uses the same layout the renderer used, so the
// two cannot disagree.
func (m Model) WindowAt(x, y int) string {
	l := m.layout()
	if l.winWidth == 0 {
		return ""
	}
	// Column 0 is the left border, the list starts at 1 and is winWidth wide.
	if x < 1 || x > l.winWidth {
		return ""
	}
	row := y - 1 // row 0 is the top border
	if l.topicRow {
		row-- // the topic row sits between the border and the list
	}
	if row < 0 || row >= l.contentRows {
		return ""
	}
	if row < len(m.order) {
		return m.order[row]
	}
	return ""
}

// nickColumn renders the member list as n rows of at most width-2 columns. Ops keep
// their prefix; ordinary members are plain. It is truncated rather than wrapped: a
// nicklist that runs off the bottom pushes the conversation out of view, which is the
// wrong trade for a list you can refresh by asking NAMES again.
func nickColumn(nicks []Nick, width, n int, st Settings) []string {
	if width <= 2 || n <= 0 {
		return nil
	}
	out := make([]string, 0, n)
	for _, nick := range nicks {
		if len(out) >= n {
			break
		}
		label := nick.Name
		if nick.Prefix != "" {
			label = nick.Prefix + nick.Name
		}
		label = truncateTo(label, width-2)
		switch {
		case nick.Prefix != "":
			// An operator is green whether or not nick colours are on: the prefix is
			// channel state, not identity, and someone with ops should be findable.
			out = append(out, opStyle.Render(label))
		case st.NickColors:
			out = append(out, nickColor(nick.Name).Render(label))
		default:
			out = append(out, nickStyle.Render(label))
		}
	}
	return out
}

// truncateTo shortens a string to fit a display width, adding an ellipsis. Width and
// not length: a topic with a wide glyph must not overflow the border.
func truncateTo(text string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(text) <= width {
		return text
	}
	if width <= 1 {
		return ""
	}
	return lipgloss.NewStyle().MaxWidth(width-1).Render(text) + "…"
}

// Body is gone: the model draws the whole screen, and the browser is a thin shell
// around it. Keeping a body-only renderer was what let the page grow a second window
// list and a second nicklist beside this one.

// bodyLines renders the scrollback: wrapped to width, bottom-aligned, exactly n lines.
// A chat window means the newest lines, and a long line means a second line and not an
// ellipsis - truncating is what made the browser cut sentences off at the right edge.
func (m Model) bodyLines(width, n int) []string {
	if n < 1 {
		n = 1
	}
	if width < 1 {
		width = 1
	}
	lines := m.windows[m.active]
	end := len(lines) - m.scroll
	if end > len(lines) {
		end = len(lines)
	}
	if end < 0 {
		end = 0
	}
	// A lookback bound, not the whole buffer: wrapping expands lines, and rendering a
	// thousand of them to show twenty is work with no visible result.
	start := end - 200
	if start < 0 {
		start = 0
	}
	out := make([]string, 0, n)
	for _, l := range lines[start:end] {
		out = append(out, lineLines(l, width, m.settings)...)
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	for len(out) < n {
		out = append(out, "")
	}
	return out
}

// lineLines renders one line, wrapped to width. Each kind decides its own prefix and
// style; continuation lines are indented under the text so a wrapped message still
// reads as one message.
func lineLines(l Line, width int, st Settings) []string {
	// A timestamp is prefixed to every line that has one, and the wrap width shrinks by
	// exactly the prefix: a line that is wrapped before its timestamp is added is a line
	// that overflows the frame by six columns on every busy channel.
	if l.Kind == LineDay {
		return dayRule(l, width)
	}
	stamp := ""
	if !l.Time.IsZero() {
		stamp = l.Time.Format("15:04:05") + " "
	}
	if stamp != "" {
		width -= lipgloss.Width(stamp)
		if width < 8 {
			stamp = ""
			width += lipgloss.Width(l.Time.Format("15:04:05")) + 1
		}
	}
	if stamp != "" {
		out := lineLinesStyled(l, width, st)
		for i := range out {
			if i == 0 {
				out[i] = timeStyle.Render(stamp) + out[i]
				continue
			}
			out[i] = timeStyle.Render(strings.Repeat(" ", lipgloss.Width(stamp))) + out[i]
		}
		return out
	}
	return lineLinesStyled(l, width, st)
}

// lineLinesStyled is the per-kind renderer, without the timestamp. It is separate so the
// timestamp logic exists once: each kind already knows how to draw itself, and threading a
// prefix through all of them would be thirteen chances to forget one.
func lineLinesStyled(l Line, width int, st Settings) []string {
	switch l.Kind {
	case LineGap:
		return wrapStyled(gapStyle, l.Text, width, "")
	case LineSystem:
		return wrapStyled(systemStyle, l.Text, width, "")
	case LineError:
		return wrapStyled(errorStyle, l.Text, width, "")
	case LineNotice:
		if st.NickColors && l.Nick != "" {
			return nickStyled(noticeStyle, l.Nick, "<", "> ", l.Text, width, "")
		}
		return wrapStyled(noticeStyle, "<"+l.Nick+"> "+l.Text, width, "")
	case LineAction:
		if st.NickColors && l.Nick != "" {
			return nickStyled(actionStyle, l.Nick, "* ", " ", l.Text, width, "")
		}
		return wrapStyled(actionStyle, "* "+l.Nick+" "+l.Text, width, "")
	case LineJoin, LineSelfJoin:
		return wrapStyled(eventStyle, "→ "+l.Nick+" "+l.Text, width, "")
	case LinePart:
		return wrapStyled(eventStyle, "← "+l.Nick+" "+l.Text, width, "")
	case LineQuit:
		return wrapStyled(eventStyle, "⇐ "+l.Nick+" "+l.Text, width, "")
	case LineKick:
		return wrapStyled(kickStyle, "✖ "+l.Text, width, "")
	case LineNick:
		return wrapStyled(eventStyle, "↔ "+l.Text, width, "")
	case LineTopic:
		return wrapStyled(topicEventStyle, "≡ "+l.Nick+" "+l.Text, width, "")
	case LineMode:
		return wrapStyled(eventStyle, "± "+l.Text, width, "")
	case LinePreview:
		return wrapStyled(previewStyle, "┄ "+l.Text, width, "")
	default:
		prefix := "<" + l.Nick + "> "
		style := bodyStyle
		if l.Mention {
			style = mentionStyle
		}
		indent := strings.Repeat(" ", lipgloss.Width(prefix))
		// The nick is coloured, the text is not: a person is recognisable by their name, and
		// colouring the body makes a busy channel unreadable and the mention highlight
		// invisible.
		if l.Nick != "" && st.NickColors {
			return nickStyled(style, l.Nick, "<", "> ", l.Text, width, indent)
		}
		return wrapStyled(style, prefix+l.Text, width, indent)
	}
}

// dayRule draws the separator between two days: a horizontal rule with the date in it.
//
// A chat buffer that spans days needs the boundary to be visible, because "yesterday" is
// not a property of a line, it is a property of where the line sits in the flow. Deriving
// it at draw time would put the rule at a different place every scroll; storing it as a
// line means it scrolls with the conversation it belongs to and a replay from the bouncer
// carries the same boundaries the live view had.
func dayRule(l Line, width int) []string {
	label := l.Text
	if s := "─── " + label + " "; lipgloss.Width(s) < width {
		line := s + strings.Repeat("─", width-lipgloss.Width(s))
		return []string{dayStyle.Render(line)}
	}
	return []string{dayStyle.Render(truncateTo(label, width))}
}

// wrapStyled wraps text to width and styles each resulting line. The indent is applied
// to continuation lines so a wrapped message lines up under its own text rather than
// under the nickname.
// nickStyled renders lead+nick+trail+body in style, with the nick in its own colour.
//
// The pieces are rendered separately on purpose. A lipgloss render ends with an ANSI reset,
// so splicing a coloured nick into an already-styled string - which is what this used to do -
// ends the outer style at the nick: every character after it is drawn in the terminal's
// default colour. For a message that means an uncoloured body, and for the case that found
// it, a mention highlight set on the line and then wiped by the nick that precedes the text.
// Each piece therefore re-establishes the outer colour instead of inheriting the gap.
func nickStyled(style lipgloss.Style, nick, lead, trail, body string, width int, indent string) []string {
	plainPrefix := lead + nick + trail
	chunks := wrapIndented(plainPrefix+body, width, lipgloss.Width(indent))
	for i := 1; i < len(chunks); i++ {
		chunks[i] = indent + chunks[i]
	}
	out := make([]string, 0, len(chunks))
	for i, c := range chunks {
		if i == 0 && strings.HasPrefix(c, plainPrefix) {
			out = append(out,
				style.Render(lead)+
					nickColor(nick).Render(nick)+
					style.Render(trail)+
					style.Render(strings.TrimPrefix(c, plainPrefix)))
			continue
		}
		out = append(out, style.Render(c))
	}
	return out
}

func wrapStyled(style lipgloss.Style, text string, width int, indent string) []string {
	if width < 4 {
		return []string{style.Render(text)}
	}
	// Continuation lines carry the indent, so they have that many fewer columns to work
	// with. Adding the indent AFTER wrapping is what pushed every long message one line
	// past the right border: the chunk was measured at the full width and then made wider.
	chunks := wrapIndented(text, width, lipgloss.Width(indent))
	for i := 1; i < len(chunks); i++ {
		chunks[i] = indent + chunks[i]
	}
	out := make([]string, 0, len(chunks))
	for _, c := range chunks {
		out = append(out, style.Render(c))
	}
	return out
}

// wrapIndented wraps text where the first line may be `width` wide and every later line only
// `width-indent`. Indent is the column the continuation has to start at.
func wrapIndented(text string, width, indent int) []string {
	if indent <= 0 || width-indent < 4 {
		return wrapText(text, width)
	}
	var out []string
	cur, curW := "", 0
	limit := func() int {
		if len(out) == 0 {
			return width
		}
		return width - indent
	}
	flush := func() {
		if cur != "" {
			out = append(out, cur)
			cur, curW = "", 0
		}
	}
	for _, word := range strings.Split(text, " ") {
		w := lipgloss.Width(word)
		sep := 0
		if curW > 0 {
			sep = 1
		}
		if curW+sep+w <= limit() {
			if sep == 1 {
				cur += " "
				curW++
			}
			cur += word
			curW += w
			continue
		}
		flush()
		for lipgloss.Width(word) > limit() {
			head := lipgloss.NewStyle().MaxWidth(limit()).Render(word)
			out = append(out, head)
			word = strings.TrimPrefix(word, head)
			if word == "" {
				break
			}
		}
		cur, curW = word, lipgloss.Width(word)
	}
	flush()
	if len(out) == 0 {
		out = append(out, "")
	}
	return out
}

// wrapText breaks text into lines of at most width display cells, preferring spaces.
// A word longer than the line is split: refusing to split it would push the border out
// on exactly the input that is easiest to produce (a pasted URL).
func wrapText(text string, width int) []string {
	if width < 2 {
		return []string{text}
	}
	if lipgloss.Width(text) <= width {
		return []string{text}
	}
	var out []string
	cur := ""
	curW := 0
	flush := func() {
		if cur != "" {
			out = append(out, cur)
			cur, curW = "", 0
		}
	}
	for _, word := range strings.Split(text, " ") {
		w := lipgloss.Width(word)
		sep := 0
		if curW > 0 {
			sep = 1
		}
		if curW+sep+w <= width {
			if sep == 1 {
				cur += " "
				curW++
			}
			cur += word
			curW += w
			continue
		}
		flush()
		// The word alone does not fit: hard-split it.
		for lipgloss.Width(word) > width {
			head := lipgloss.NewStyle().MaxWidth(width).Render(word)
			out = append(out, head)
			word = strings.TrimPrefix(word, head)
		}
		cur, curW = word, lipgloss.Width(word)
	}
	flush()
	if len(out) == 0 {
		out = append(out, "")
	}
	return out
}

func (m Model) statusBar() string {
	state := "offline"
	if m.connected {
		state = "connected"
	}
	extra := ""
	if m.status != "" {
		extra = " · " + m.status
	}
	return barStyle.Width(max(m.width, 20)).Render(fmt.Sprintf(" %s · %s%s", m.active, state, extra))
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Windows returns the window list in cycle order. The web client renders it as a
// sidebar, so it needs the list the model actually has rather than a copy kept in
// the browser: a second list is a list that drifts.
func (m Model) Windows() []string {
	out := make([]string, len(m.order))
	copy(out, m.order)
	return out
}

// SetInput replaces the input buffer, for a client mirroring typing.
func (m Model) SetInput(text string) Model {
	m.input = m.input[:0]
	m.input = append(m.input, []rune(text)...)
	return m
}

// Status is the status-bar text.
func (m Model) Status() string { return m.status }

// Unread returns the per-window unread counts, for a sidebar that shows which
// windows have something new. A copy, so a renderer cannot corrupt the model.
func (m Model) Unread() map[string]int {
	out := make(map[string]int, len(m.unread))
	for k, v := range m.unread {
		out[k] = v
	}
	return out
}

// Mentions returns the windows where we were addressed by name. Separate from
// unread on purpose: everything new is not everything for you.
func (m Model) Mentions() map[string]bool {
	out := make(map[string]bool, len(m.mention))
	for k, v := range m.mention {
		out[k] = v
	}
	return out
}

// NoteMention marks a window as containing a line addressed to us. The transport
// decides whether a line mentions us, because it is the only place that knows our
// nick; the model just records the consequence.
func (m Model) NoteMention(window string) Model {
	if m.mention == nil {
		m.mention = make(map[string]bool)
	}
	if window != m.active {
		m.mention[window] = true
	}
	return m
}

// SearchResult is one hit: the window it is in and the line itself.
type SearchResult struct {
	Window string
	Line   Line
}

// Search finds every line containing the query, case-insensitively, across every
// window. It is the in-client half of W4: the server-side log index answers "find
// this from last month", and this answers "find this in what I have in front of
// me". Both are needed; they are different questions.
//
// It is a linear scan on purpose. The scrollback in memory is bounded by the
// window caps, so there is no index to keep in sync with appends, and an index that
// can disagree with the lines it indexes is worse than a scan.
func (m Model) Search(query string) []SearchResult {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return nil
	}
	var out []SearchResult
	for _, window := range m.order {
		for _, line := range m.windows[window] {
			if strings.Contains(strings.ToLower(line.Text), query) ||
				strings.Contains(strings.ToLower(line.Nick), query) {
				out = append(out, SearchResult{Window: window, Line: line})
			}
		}
	}
	return out
}

// searchIntoWindow renders the hits of a search into the active window as system lines.
//
// It reports the count first, because "3 hits" is the answer to "did I spell it right" and a list
// alone does not say whether the list is over. Hits are capped for the same reason the scrollback
// is: an unbounded write into the window from a one-word query would push everything else out.
func (m Model) searchIntoWindow(query string) Model {
	if query == "" {
		return m.appendSystem("usage: /search <text>\n")
	}
	hits := m.Search(query)
	if len(hits) == 0 {
		// Naming the query matters: "/search sat" returning nothing and "/search satloto"
		// returning nothing are different problems and the message should say which.
		return m.appendSystem("no matches for " + query + "\n")
	}
	// The cap is the terminal's height, not a fixed number. A fixed 50 in a 30-row terminal is a
	// list whose own count line has already scrolled off the top by the time it finishes - so the
	// user sees the tail of a result with no idea how many there were, and the answer to "did I
	// spell it right" is exactly the line that got pushed away. Found by the live probe, which
	// could not find the count on screen and turned out to be right.
	// The count line, the frame's two rows, and the "... and N more" line if it appears, plus the
	// input row. Getting this wrong by one is what makes the count scroll away, which is the whole
	// point of the cap.
	maxShown := m.height - 6
	if maxShown < 5 {
		maxShown = 5 // a tiny terminal still shows a result rather than nothing
	}
	if maxShown > 50 {
		maxShown = 50 // and never more than this, however tall the window
	}
	shown := hits
	extra := 0
	if len(shown) > maxShown {
		extra = len(shown) - maxShown
		shown = shown[:maxShown]
	}
	m = m.appendSystem(fmt.Sprintf("%d match(es) for %q, newest window first seen order:\n", len(hits), query))
	for _, hit := range shown {
		// The window, the nick and the text, because a hit with no window is a line the user
		// cannot go back to.
		nick := hit.Line.Nick
		if nick == "" {
			nick = "-"
		}
		m = m.appendSystem(fmt.Sprintf("  %s  <%s> %s\n", hit.Window, nick, hit.Line.Text))
	}
	if extra > 0 {
		m = m.appendSystem(fmt.Sprintf("  ... and %d more\n", extra))
	}
	return m
}

// Nicklist returns the members of the active window, or of a named one. An empty
// result means "not a channel" - a query window has no membership.
func (m Model) Nicklist(window string) []Nick {
	out := make([]Nick, len(m.nicks[window]))
	copy(out, m.nicks[window])
	return out
}

// NickCounts reports how many members each window has, for a sidebar that shows
// "how busy is this channel" without shipping every name to the browser.
func (m Model) NickCounts() map[string]int {
	out := make(map[string]int, len(m.nicks))
	for window, nicks := range m.nicks {
		out[window] = len(nicks)
	}
	return out
}

// Topic returns the current topic of a window, "" if it has none or we have not
// been told.
func (m Model) Topic(window string) string { return m.topics[window] }

// Modes returns a window's channel mode string ("+nt"), "" if not a channel or not
// known.
func (m Model) Modes(window string) string { return m.modes[window] }

// previewText renders a card as one line: the site, the title, and the URL only when
// the title does not already name it. One line because a terminal's unit of display is a
// line, and three lines of card for one link is most of a phone screen.
func previewText(title, site, rawURL string) string {
	title = strings.TrimSpace(title)
	site = strings.TrimSpace(site)
	switch {
	case title != "" && site != "":
		return title + " — " + site
	case title != "":
		return title
	case site != "":
		return site
	default:
		return rawURL
	}
}

// Links returns the distinct links in a window's recent lines, most recent first.
//
// Only recent lines, and only lines that were actually shown: the point of a preview
// is the message the user is looking at, and fetching links out of a month of
// scrollback would turn reading old channels into a burst of traffic to strangers.
func (m Model) Links(window string, max int) []string {
	lines := m.windows[window]
	const lookBack = 40
	seen := make(map[string]bool, max)
	out := make([]string, 0, max)
	for i := len(lines) - 1; i >= 0 && len(lines)-i <= lookBack; i-- {
		u := LinkIn(lines[i].Text)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
		if len(out) >= max {
			break
		}
	}
	return out
}

// Width and Height report the size the model was last told. A driver needs them to
// ask for a render that fits, and there is no other way to know: the model holds the
// size precisely because the driver told it.
func (m Model) Width() int { return m.width }

// Height is the model's last known height.
func (m Model) Height() int { return m.height }
