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

	case Incoming:
		m.append(Line(msg))
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
	if m.scroll > 0 {
		m.scroll++
	}
}

// Styles are built once: lipgloss resolves colours lazily, so the profile set
// by the caller before the first View applies. Every colour is a 256-palette
// index for the reason documented in docs/UI.md: the HTML converter maps
// 8-bit SGR to classes and drops 24-bit colour silently.
var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
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

	inner := width - 4 // border plus padding on each side
	body := m.bodyLines(inner, height-6)

	var b strings.Builder
	b.WriteString(borderStyle.Render("╭─ "))
	b.WriteString(titleStyle.Render(m.active))
	if m.scroll > 0 {
		b.WriteString(systemStyle.Render(fmt.Sprintf(" (scroll %d)", m.scroll)))
	}
	head := lipgloss.Width(b.String()) + lipgloss.Width(borderStyle.Render(" ╮"))
	if pad := width - head; pad > 0 {
		b.WriteString(borderStyle.Render(strings.Repeat("─", pad) + "╮"))
	}
	b.WriteString("\n")

	for _, line := range body {
		b.WriteString(borderStyle.Render("│ "))
		b.WriteString(line)
		if pad := inner - lipgloss.Width(line); pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
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
