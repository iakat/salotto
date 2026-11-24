package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Every row of the frame is exactly as wide as the terminal.
//
// This is not cosmetic. The client draws its own box: the pipe that closes the conversation
// column is the only thing that shows where a line ends and the member list begins, so a row one
// cell short puts the right-hand pipes in two different columns and the whole frame looks broken
// at a glance - which is exactly how it was reported ("the top border is 1char too short, same
// with the line the input field is on").
func TestEveryFrameRowIsExactlyAsWideAsTheTerminal(t *testing.T) {
	for _, size := range [][2]int{{120, 30}, {100, 24}, {80, 20}, {72, 18}, {60, 12}} {
		m := New("status")
		m, _ = m.Step(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m.append(Line{Window: "status", Nick: "alice", Text: "hello there"})
		m, _ = m.Step(Nicklist{Window: "status", Nicks: []Nick{{Name: "alice"}, {Name: "bob"}}})
		m.topics["status"] = "a topic that is long enough to need truncating in a narrow frame"

		rows := strings.Split(m.View(), "\n")
		for i, row := range rows {
			if row == "" {
				continue
			}
			if w := lipgloss.Width(row); w != size[0] {
				t.Errorf("size %dx%d: row %d is %d cells, want %d: %q", size[0], size[1], i, w, size[0], row)
			}
		}
	}
}
