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

	// Mention marks a line that addressed us by name. It lives on the line rather
	// than in a separate index so a renderer draws a highlight from the data it is
	// already drawing, and a search result carries its own answer to "was this for
	// me".
	Mention bool
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
)

// Incoming is the message a transport sends when a line arrives. It is a plain
// value so the web bridge can build one from a wire frame and a test can build
// one from a literal.
type Incoming Line

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
		m.append(Line{Window: m.active, Nick: "me", Text: text, Kind: LineMessage})
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

// append adds a line to its window and makes that window known. Auto-scroll
// stays pinned to the bottom unless the user scrolled up.
func (m *Model) append(l Line) {
	if l.Window == "" {
		l.Window = m.active
	}
	if _, seen := m.windows[l.Window]; !seen {
		m.order = append(m.order, l.Window)
	}
	m.windows[l.Window] = append(m.windows[l.Window], l)
	// Unread is a count of things you did not see arrive. A line in the window you
	// are looking at was seen, so it does not count; that is what makes the number
	// meaningful rather than a total.
	if l.Window != m.active && l.Kind != LineGap {
		if m.unread == nil {
			m.unread = make(map[string]int)
		}
		m.unread[l.Window]++
	}
	if m.scroll > 0 {
		m.scroll++
	}
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
	timeStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
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
func (m Model) View() string {
	width := m.width
	if width < 20 {
		width = 20
	}
	height := m.height
	if height < 6 {
		height = 6
	}

	// The nicklist takes a column on the right when the window has members. It is
	// drawn inside the same border as the conversation, so the TUI shows what the
	// browser shows in its sidebar rather than something a little different: one
	// model, three renderers, and this is the third one agreeing.
	nickWidth := 0
	nicks := m.nicks[m.active]
	if len(nicks) > 0 && width >= 60 {
		nickWidth = 18
		if width < 90 {
			nickWidth = 14
		}
	}
	inner := width - 4 - nickWidth // border, padding, and the nick column
	body := m.bodyLines(inner, height-6)
	nickCol := nickColumn(nicks, nickWidth, height-6)

	var b strings.Builder
	b.WriteString(borderStyle.Render("╭─ "))
	b.WriteString(titleStyle.Render(m.active))
	if m.scroll > 0 {
		b.WriteString(systemStyle.Render(fmt.Sprintf(" (scroll %d)", m.scroll)))
	}
	// The topic sits on the title row, which is where every IRC client puts it and
	// the only place it can be seen without stealing a line from the conversation.
	// Modes first: they are short and fixed-width-ish, so they survive truncation
	// where a long topic would not.
	if modes := m.modes[m.active]; modes != "" && modes != "+" {
		b.WriteString(topicStyle.Render("  " + modes))
	}
	if topic := m.topics[m.active]; topic != "" {
		room := width - lipgloss.Width(b.String()) - 6
		if room > 12 {
			b.WriteString(topicStyle.Render("  " + truncateTo(topic, room)))
		}
	}
	head := lipgloss.Width(b.String()) + lipgloss.Width(borderStyle.Render(" ╮"))
	if pad := width - head; pad > 0 {
		b.WriteString(borderStyle.Render(strings.Repeat("─", pad) + "╮"))
	}
	b.WriteString("\n")

	for i, line := range body {
		b.WriteString(borderStyle.Render("│ "))
		b.WriteString(line)
		if pad := inner - lipgloss.Width(line); pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
		}
		if nickWidth > 0 {
			b.WriteString(borderStyle.Render("│"))
			cell := ""
			if i < len(nickCol) {
				cell = nickCol[i]
			}
			b.WriteString(" ")
			b.WriteString(cell)
			// Pad on display width, not byte length: a nick with a wide glyph must
			// not push the border out.
			if pad := nickWidth - 2 - lipgloss.Width(cell); pad > 0 {
				b.WriteString(strings.Repeat(" ", pad))
			}
		}
		b.WriteString(borderStyle.Render(" │"))
		b.WriteString("\n")
	}

	b.WriteString(borderStyle.Render("├" + strings.Repeat("─", width-2) + "┤"))
	b.WriteString("\n")
	b.WriteString(borderStyle.Render("│ "))
	b.WriteString(inputStyle.Render("> " + string(m.input)))
	if pad := width - 4 - lipgloss.Width(string(m.input)) - 2; pad > 0 {
		b.WriteString(strings.Repeat(" ", pad))
	}
	b.WriteString(borderStyle.Render(" │"))
	b.WriteString("\n")
	b.WriteString(borderStyle.Render("╰" + strings.Repeat("─", width-2) + "╯"))
	b.WriteString("\n")
	b.WriteString(m.statusBar())
	return b.String()
}

// bodyLines renders exactly n lines of scrollback, styled, bottom-aligned: a
// short window shows the newest lines, which is what a chat window means.
func (m Model) bodyLines(width, n int) []string {
	if n < 1 {
		n = 1
	}
	lines := m.windows[m.active]
	end := len(lines) - m.scroll
	if end > len(lines) {
		end = len(lines)
	}
	if end < 0 {
		end = 0
	}
	start := end - n
	if start < 0 {
		start = 0
	}
	out := make([]string, 0, n)
	for _, l := range lines[start:end] {
		out = append(out, styleLine(l, width))
	}
	for len(out) < n {
		out = append(out, "")
	}
	return out
}

// nickColumn renders the member list as n rows of at most width-2 columns. Ops keep
// their prefix; ordinary members are plain. It is truncated rather than wrapped:
// a nicklist that runs off the bottom of the screen would push the conversation out
// of view, which is the wrong trade for a list you can scroll by asking NAMES again.
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
		if lipgloss.Width(label) > width-2 {
			label = lipgloss.NewStyle().MaxWidth(width - 2).Render(label)
		}
		if nick.Prefix != "" {
			out = append(out, opStyle.Render(label))
		} else {
			out = append(out, nickStyle.Render(label))
		}
	}
	return out
}

// truncateTo shortens a string to fit a display width, adding an ellipsis. Width
// and not length: a topic with a wide glyph must not overflow the border.
func truncateTo(text string, width int) string {
	if lipgloss.Width(text) <= width {
		return text
	}
	if width <= 1 {
		return ""
	}
	return lipgloss.NewStyle().MaxWidth(width-1).Render(text) + "…"
}

func styleLine(l Line, width int) string {
	text := l.Text
	if lipgloss.Width(text) > width-2 {
		// Truncate on width, not bytes: a wide glyph must not eat the border.
		text = lipgloss.NewStyle().MaxWidth(width - 2).Render(text)
	}
	switch l.Kind {
	case LineGap:
		return gapStyle.Render(text)
	case LineSystem:
		return systemStyle.Render(text)
	case LineNotice:
		return noticeStyle.Render("<" + l.Nick + "> " + text)
	case LineAction:
		return actionStyle.Render("* " + l.Nick + " " + text)
	default:
		// A mention is drawn differently in every front end. The model decides
		// that it IS a mention; each renderer decides what that looks like, which
		// is why this is a style here and a CSS class in the browser.
		if l.Mention {
			return nickStyle.Render("<"+l.Nick+">") + " " + mentionStyle.Render(text)
		}
		return nickStyle.Render("<"+l.Nick+">") + " " + bodyStyle.Render(text)
	}
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
