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
//
// The grouped sidebar is drawn here too, and for the same reason: a heading is a row with no
// marker column and no badge, so it is the one row where an off-by-one hides - it has nothing
// after it to push the width back out. The network names are deliberately too long for the
// sidebar, so the truncation path is the one measured.
func TestEveryFrameRowIsExactlyAsWideAsTheTerminal(t *testing.T) {
	for _, size := range [][2]int{{120, 30}, {100, 24}, {80, 20}, {72, 18}, {60, 12}} {
		for _, grouped := range []bool{false, true} {
			m := New("status")
			m, _ = m.Step(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			m.append(Line{Window: "status", Nick: "alice", Text: "hello there"})
			m, _ = m.Step(Nicklist{Window: "status", Nicks: []Nick{{Name: "alice"}, {Name: "bob"}}})
			m.topics["status"] = "a topic that is long enough to need truncating in a narrow frame"
			if grouped {
				m.settings.GroupByServer = true
				m, _ = m.Step(Incoming(Line{Window: "status", Network: "a-network-with-a-long-name", Text: "connected", Kind: LineSystem}))
				m, _ = m.Step(Incoming(Line{Window: "#salotto", Network: "a-network-with-a-long-name", Nick: "bob", Text: "hi", Kind: LineMessage}))
				m, _ = m.Step(Incoming(Line{Window: "#other", Network: "zz", Nick: "carol", Text: "hi", Kind: LineMessage}))
				// One name far too long for the column, because truncation is the path where
				// the label budget is actually spent: a list of short names never notices that
				// the indent and the marker were not subtracted, and the row comes out two
				// cells wide - the same defect as the border, just harder to see.
				m, _ = m.Step(Incoming(Line{Window: "#a-channel-with-a-name-far-too-long-to-fit", Network: "a-network-with-a-long-name", Nick: "dave", Text: "hi", Kind: LineMessage}))
				m.unread["#other"] = 12
				m.mention["#salotto"] = true
			}

			rows := strings.Split(m.View(), "\n")
			for i, row := range rows {
				if row == "" {
					continue
				}
				if w := lipgloss.Width(row); w != size[0] {
					t.Errorf("size %dx%d grouped=%v: row %d is %d cells, want %d: %q", size[0], size[1], grouped, i, w, size[0], row)
				}
			}
		}
	}
}
