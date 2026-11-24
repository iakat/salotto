package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestTheFrameIsNeverTallerThanTheTerminal is the bug nobody could see in a test that only
// looked at a string.
//
// A terminal does not clip: a program that writes more lines than the screen has scrolls
// the screen, and the lines that scroll off the top are gone. The frame plus its status bar
// was one line too tall, so the top border - and with a short window, the conversation -
// scrolled away on every redraw. Locally in a big terminal the missing border hides above
// the fold and looks fine; in a 30-row browser window the model appears to have no chrome.
func TestTheFrameIsNeverTallerThanTheTerminal(t *testing.T) {
	Configure()
	for _, size := range []struct{ w, h int }{
		{100, 30}, {80, 24}, {120, 40}, {60, 12}, {100, 6}, {40, 10},
	} {
		m := New("#test")
		m, _ = m.Step(tea.WindowSizeMsg{Width: size.w, Height: size.h})
		got := strings.Count(m.View(), "\n") + 1
		if got != size.h {
			t.Errorf("%dx%d: the frame is %d lines, the terminal has %d", size.w, size.h, got, size.h)
		}
	}
}
