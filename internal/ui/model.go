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
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

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
	switch msg.Type {
	case tea.KeyEnter:
		text := strings.TrimSpace(string(m.input))
		m.input = m.input[:0]
		if text == "" {
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
		if m.scroll < len(m.windows[m.active]) {
			m.scroll++
		}
		return m, nil

	case tea.KeyDown:
		if m.scroll > 0 {
			m.scroll--
		}
		return m, nil

	case tea.KeyCtrlC:
		return m, tea.Quit

	case tea.KeyCtrlN:
		return m.cycle(1), nil

	case tea.KeyCtrlP:
		return m.cycle(-1), nil

	case tea.KeyRunes:
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
	if l.Window != m.active && l.Kind != LineGap && l.Kind != LinePreview {
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
	gapStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	inputStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("231"))
	barStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("250")).Background(lipgloss.Color("236"))
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

// View draws the whole screen: window list, conversation, member list, input line and
// status bar.
//
// The model draws all of it, and the web page is a thin shell around this string. That
// is the rule the three front ends live by: there is exactly one place that knows what
// a window list looks like, and it is here. A browser sidebar built in HTML was a
// second one, and it drifted immediately - the nicks were drawn twice, once by the
// renderer and once by the page.
func (m Model) View() string {
	l := m.layout()
	nicks := m.nicks[m.active]
	body := m.bodyLines(l.inner, l.contentRows)
	winCol := m.windowColumn(l.winWidth, l.contentRows)
	nickCol := nickColumn(nicks, l.nickWidth, l.contentRows)

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
func nickColumn(nicks []Nick, width, n int) []string {
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
		if nick.Prefix != "" {
			out = append(out, opStyle.Render(label))
		} else {
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
		out = append(out, lineLines(l, width)...)
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
func lineLines(l Line, width int) []string {
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
		out := lineLinesStyled(l, width)
		for i := range out {
			if i == 0 {
				out[i] = timeStyle.Render(stamp) + out[i]
				continue
			}
			out[i] = timeStyle.Render(strings.Repeat(" ", lipgloss.Width(stamp))) + out[i]
		}
		return out
	}
	return lineLinesStyled(l, width)
}

// lineLinesStyled is the per-kind renderer, without the timestamp. It is separate so the
// timestamp logic exists once: each kind already knows how to draw itself, and threading a
// prefix through all of them would be thirteen chances to forget one.
func lineLinesStyled(l Line, width int) []string {
	switch l.Kind {
	case LineGap:
		return wrapStyled(gapStyle, l.Text, width, "")
	case LineSystem:
		return wrapStyled(systemStyle, l.Text, width, "")
	case LineNotice:
		return wrapStyled(noticeStyle, "<"+l.Nick+"> "+l.Text, width, "")
	case LineAction:
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
		return wrapStyled(style, prefix+l.Text, width, strings.Repeat(" ", lipgloss.Width(prefix)))
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
