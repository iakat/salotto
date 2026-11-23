package ui

import (
	"fmt"
	"io"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"github.com/muesli/termenv"

	terminal "github.com/buildkite/terminal-to-html/v3"
)

// Configure must be called once, before the first View, by any process that is
// not attached to a terminal: a daemon has no TTY, so lipgloss would otherwise
// resolve the ASCII profile and strip every colour before the HTML converter
// ever sees it. 256 colours specifically, because the converter maps 8-bit SGR
// to its term-fgx* classes and its 24-bit branch is an empty case: truecolor
// would render as unstyled text in the browser (QUEUE.md §2.0, trap 1).
func Configure() {
	lipgloss.SetColorProfile(termenv.ANSI256)
}

// HTML renders a View() string as the HTML the web front end embeds. It is the
// whole web renderer: the browser gets the terminal rendering, not a second UI.
func HTML(view string) string {
	return terminal.Render([]byte(view))
}

// Headless drives the model with no terminal at all: it applies messages in
// order and returns the final View. Tests and bots use it, and it is the proof
// that the model is a value and not a Program.
func Headless(m Model, msgs ...tea.Msg) (Model, string) {
	for _, msg := range msgs {
		m, _ = m.Step(msg)
	}
	return m, m.View()
}

// SSHProgram builds the bubbletea program an SSH session runs. in/out are the
// SSH channel: bubbletea sees a terminal because wish hands it one, but nothing
// in the model knows that.
func SSHProgram(in io.Reader, out io.Writer, m Model) *tea.Program {
	return tea.NewProgram(m, tea.WithInput(in), tea.WithOutput(out))
}

// Table is a small helper the settings screens share, so the boxed look is
// defined once. It returns plain text plus ANSI, like every other view.
func Table(headers []string, rows [][]string) string {
	t := table.New().Border(lipgloss.NormalBorder()).BorderStyle(borderStyle).StyleFunc(
		func(row, _ int) lipgloss.Style {
			if row == table.HeaderRow {
				return titleStyle
			}
			return bodyStyle
		})
	t.Headers(headers...)
	for _, row := range rows {
		t.Row(row...)
	}
	return t.Render()
}

// Banner is the one-line banner the SSH server prints before the TUI starts.
func Banner(name string) string {
	return fmt.Sprintf("salotto · %s · Ctrl-C to quit, Ctrl-N/Ctrl-P to switch windows\r\n", name)
}

// Plain strips ANSI from a rendered view, for a log line or a test that wants
// to assert on text. It is deliberately dumb: SGR sequences only.
func Plain(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
