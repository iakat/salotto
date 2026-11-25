package ui

import "github.com/charmbracelet/lipgloss"

// NickAt reports the nick under a click at character cell (x, y), "" if the click did not
// land on one. It is the sibling of WindowAt: the two read the rows the renderer drew rather
// than rebuilding the layout from the model, so a wrapped message or a timestamp cannot make
// the click land on the line above.
//
// It is deliberately not a hover test. Changing the cursor or highlighting the nick under the
// pointer would cost a redraw for every mouse-move event, on a daemon that may be serving many
// clients, and terminals disagree about whether they deliver motion at all. A click is one
// event and this is what it needs.
//
// The conversation and the member list are covered. The member list is drawn as one column,
// so there is one column of nicks here; if a second column is ever drawn beside it, this
// function needs that column's offset too, otherwise it would confidently name the wrong
// person. A dead zone is better than a wrong nick.
func (m Model) NickAt(x, y int) string {
	l := m.layout()
	bodyStart := 1
	if l.winWidth > 0 {
		bodyStart += l.winWidth + 1
	}
	row := y - 1 // row 0 is the top border
	if l.topicRow {
		row-- // the topic row sits between the border and the content
	}
	if row < 0 || row >= l.contentRows {
		return ""
	}
	if col := x - bodyStart; col >= 0 && col < l.inner {
		if nick := m.bodyNickAt(col, row, l); nick != "" {
			return nick
		}
	}
	if l.nickWidth > 0 {
		// The member list begins after the conversation column and its closing border.
		nickStart := bodyStart + l.inner + 1
		if col := x - nickStart; col >= 0 && col < l.nickWidth {
			return m.memberNickAt(col, row, l)
		}
	}
	return ""
}

// bodyNickAt looks up one conversation row in the index the renderer produced. The index
// carries the line number and the nick's columns, so this function never wraps a line or
// measures a prefix: doing either here would be the second layout that bodyIndex exists to
// avoid.
func (m Model) bodyNickAt(col, row int, l layout) string {
	index := m.bodyIndex(l.inner, l.contentRows)
	if row >= len(index) {
		return ""
	}
	r := index[row]
	// A row with no line is a collapsed run or the padding under a short buffer. Neither has
	// anybody behind it, so a click there is a miss rather than a query to a neighbour.
	if r.line < 0 || col < r.nickStart || col >= r.nickEnd {
		return ""
	}
	lines := m.windows[m.active]
	if r.line >= len(lines) {
		return ""
	}
	line := lines[r.line]
	// The index only sets a span for conversation kinds, but the check is here too: a future
	// kind that happened to carry a nick must not become clickable without deciding to.
	switch line.Kind {
	case LineMessage, LineAction, LineNotice:
	default:
		return ""
	}
	if line.Nick == "" || m.isSelfNick(line.Nick) {
		return ""
	}
	return line.Nick
}

// memberNickAt reads the member list's one drawn column. The nick list itself is the index:
// one row, one nick, in the order nickColumn used, which is why a click on the fifth name
// cannot select the fourth.
//
// The prefix is part of the drawn label but not part of the person's name. Clicking "@" in
// "@alice" is a miss; clicking "alice" is the query. A label clipped by the column width
// makes only the visible part clickable, so the hit test stops where the drawn name does.
func (m Model) memberNickAt(col, row int, l layout) string {
	if l.nickWidth <= 2 {
		return ""
	}
	nicks := m.nicks[m.active]
	if row < 0 || row >= len(nicks) {
		return ""
	}
	nick := nicks[row]
	if nick.Name == "" {
		return ""
	}
	label := nick.Name
	if nick.Prefix != "" {
		label = nick.Prefix + nick.Name
	}
	labelWidth := lipgloss.Width(truncateTo(label, l.nickWidth-2))
	start := lipgloss.Width(nick.Prefix)
	end := start + lipgloss.Width(nick.Name)
	if end > labelWidth {
		end = labelWidth
	}
	if col < start || col >= end {
		return ""
	}
	if m.isSelfNick(nick.Name) {
		return ""
	}
	return nick.Name
}
