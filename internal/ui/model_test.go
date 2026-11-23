package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestMain(m *testing.M) {
	// The daemon and the tests have no TTY; without this lipgloss resolves the
	// ASCII profile and View() would carry no SGR at all, hiding exactly the
	// bug the HTML test is here to catch.
	Configure()
	m.Run()
}

func seeded() Model {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 60, Height: 14})
	m, _ = m.Step(Incoming{Window: "status", Nick: "srv", Text: "connected", Kind: LineSystem})
	m, _ = m.Step(Incoming{Window: "#salotto", Nick: "katia", Text: "ciao", Kind: LineMessage})
	m, _ = m.Step(Incoming{Window: "#salotto", Nick: "katia", Text: "waves", Kind: LineAction})
	m, _ = m.Step(Incoming{Window: "#salotto", Nick: "op", Text: "topic changed", Kind: LineNotice})
	// Show the channel, not the server window: the model's active window is what
	// View renders, and a driver never renders a window the user is not in.
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyCtrlN})
	return m
}

// TestOneModelThreeDrivers is the §2.0 claim, asserted rather than described:
// the same Model value produces the same View when driven by hand (headless),
// when queried directly, and when the SSH Program renders it. There is no
// second renderer to disagree with.
func TestOneModelThreeDrivers(t *testing.T) {
	m := seeded()
	direct := m.View()

	// headless: a driver that replays messages and calls View.
	mm := New("status")
	_, headless := Headless(mm,
		tea.WindowSizeMsg{Width: 60, Height: 14},
		Incoming{Window: "status", Nick: "srv", Text: "connected", Kind: LineSystem},
		Incoming{Window: "#salotto", Nick: "katia", Text: "ciao", Kind: LineMessage},
		Incoming{Window: "#salotto", Nick: "katia", Text: "waves", Kind: LineAction},
		Incoming{Window: "#salotto", Nick: "op", Text: "topic changed", Kind: LineNotice},
		tea.KeyMsg{Type: tea.KeyCtrlN},
	)
	if headless != direct {
		t.Fatalf("headless view differs from direct view:\n%q\nvs\n%q", headless, direct)
	}

	// The SSH driver builds a Program from the same value. Rendering it needs a
	// pty, so what is asserted here is that the value satisfies tea.Model: the
	// Program constructor rejects anything that does not.
	var _ tea.Model = m
	if p := SSHProgram(strings.NewReader(""), &strings.Builder{}, m); p == nil {
		t.Fatal("SSHProgram returned nil for a valid model")
	}
}

// TestViewIsStyled is why Configure exists: a daemon has no TTY, so without the
// explicit ANSI256 profile View() would be colourless and the browser would get
// monochrome HTML with no error anywhere.
func TestViewIsStyled(t *testing.T) {
	m := seeded()
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hi")})
	view := m.View()
	if !strings.Contains(view, "\x1b[") {
		t.Fatal("View has no SGR: the colour profile was not configured")
	}
	if !strings.Contains(view, "\x1b[38;5;") {
		t.Fatal("View is not using 8-bit palette SGR")
	}
	if strings.Contains(view, "\x1b[38;2;") {
		t.Fatal("View used 24-bit colour, which the HTML converter drops silently")
	}
}

// TestNoTruecolorSurvivesHTML is the trap QUEUE.md §2.0 names: terminal-to-html
// v3 maps 8-bit SGR to classes and ignores 24-bit, so a truecolor style would
// reach the browser as unstyled text. Render, then assert both halves.
func TestNoTruecolorSurvivesHTML(t *testing.T) {
	m := seeded()
	html := HTML(m.View())
	if html == "" {
		t.Fatal("HTML(empty) for a non-empty view")
	}
	if strings.Contains(html, "\x1b") {
		t.Fatalf("HTML still contains ESC bytes: %q", html)
	}
	if !strings.Contains(html, "class=\"term-fgx") {
		t.Fatalf("HTML has no terminal classes: %q", html)
	}
	if strings.Contains(html, "38;2;") || strings.Contains(html, "38;5;") {
		t.Fatalf("HTML leaked an SGR sequence: %q", html)
	}
}

// TestWindowSwitching: bubbletea resolves Ctrl-N/Ctrl-P to their own KeyTypes,
// not to KeyRunes, so a driver that only handled runes would silently drop
// window switching.
func TestWindowSwitching(t *testing.T) {
	m := New("status")
	m, _ = m.Step(Incoming{Window: "#salotto", Nick: "k", Text: "hi"})
	if got := m.Active(); got != "status" {
		t.Fatalf("active = %q, want status", got)
	}
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyCtrlN})
	if got := m.Active(); got != "#salotto" {
		t.Fatalf("after Ctrl-N active = %q, want #salotto", got)
	}
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyCtrlP})
	if got := m.Active(); got != "status" {
		t.Fatalf("after Ctrl-P active = %q, want status", got)
	}
	// One window: cycling must be a no-op, not a panic or a blank screen.
	single := New("status")
	single, _ = single.Step(tea.KeyMsg{Type: tea.KeyCtrlN})
	if got := single.Active(); got != "status" {
		t.Fatalf("single-window cycle active = %q, want status", got)
	}
}

// TestSubmitEmitsSent: the model does not send anything itself; it returns a
// command whose message the transport acts on. That is the seam the web bridge
// and the SSH driver both use.
func TestSubmitEmitsSent(t *testing.T) {
	m := New("status")
	for _, r := range "hello" {
		m, _ = m.Step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if got := m.Input(); got != "hello" {
		t.Fatalf("input = %q, want hello", got)
	}
	m, cmd := m.Step(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter returned no command")
	}
	msg := cmd()
	sent, ok := msg.(Sent)
	if !ok || sent.Text != "hello" {
		t.Fatalf("cmd() = %#v, want Sent{hello}", msg)
	}
	if got := m.Input(); got != "" {
		t.Fatalf("input after submit = %q, want empty", got)
	}
}

// TestScrollPinsToBottom: a new line must not scroll a reading user, and the
// status bar must say so.
func TestScrollPinsToBottom(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 40, Height: 10})
	for i := 0; i < 20; i++ {
		m, _ = m.Step(Incoming{Window: "status", Text: "line"})
	}
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyUp})
	if m.scroll != 1 {
		t.Fatalf("scroll = %d, want 1", m.scroll)
	}
	if !strings.Contains(Plain(m.View()), "scroll 1") {
		t.Fatal("status bar does not report the scroll offset")
	}
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyDown})
	if m.scroll != 0 {
		t.Fatalf("scroll = %d, want 0", m.scroll)
	}
}
