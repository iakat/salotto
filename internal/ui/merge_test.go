package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Buffer merge is a window whose conversation is built from other windows at draw time. These
// tests are about what a person sees and what Enter would send, because both are invisible in
// the struct: a row with no source label still renders, and a target that is not on the wire
// still looks like a title. The two architectural tests are TestTheMergedViewHoldsNoSlice and
// TestMergingIsPerClientNotInTheBlob: each pins one of the tidy-looking implementations the
// design rejects.

// mergeNow is the fixed clock the fixtures use. Fixed times make "oldest first" exact instead
// of a race with the wall clock, and a merge is entirely an ordering question.
var mergeNow = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

// mergeModel is a connected client with two channels on one network. Same network on purpose:
// the members are keys, and a fixture with one network per channel would let a name-based
// implementation pass by accident.
func mergeModel(t *testing.T) Model {
	t.Helper()
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 120, Height: 30})
	m, _ = m.Step(Incoming(Line{Window: "status", Network: "libera", Nick: "srv", Text: "connected", Kind: LineSystem, Time: mergeNow}))
	m, _ = m.Step(Incoming(Line{Window: "#a", Network: "libera", Nick: "alice", Text: "alpha", Kind: LineMessage, Time: mergeNow.Add(time.Second)}))
	m, _ = m.Step(Incoming(Line{Window: "#b", Network: "libera", Nick: "bob", Text: "bravo", Kind: LineMessage, Time: mergeNow.Add(2 * time.Second)}))
	return m
}

// mergedTwo is the fixture after /merge #a #b, with the merged view active and its target
// chosen. It is built through Step and the typed command, not by setting m.merge by hand: a
// test that pokes the struct cannot catch a command that creates the wrong state.
func mergedTwo(t *testing.T) Model {
	t.Helper()
	m := typeLine(mergeModel(t), "/merge #a #b")
	if !m.merge || m.active != mergeKey {
		t.Fatalf("/merge did not open the merged view: merge=%v active=%q", m.merge, m.active)
	}
	return m
}

// salottoModel is a client sitting in #salotto with #other open beside it on one network. The
// one-argument merge rule is about the window the user is in, so the fixture has to put them
// in a real channel rather than in status.
func salottoModel(t *testing.T) Model {
	t.Helper()
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 120, Height: 30})
	m, _ = m.Step(Incoming(Line{Window: "status", Network: "libera", Nick: "srv", Text: "connected", Kind: LineSystem, Time: mergeNow}))
	m, _ = m.Step(Incoming(Line{Window: "#salotto", Network: "libera", Nick: "alice", Text: "ciao", Kind: LineMessage, Time: mergeNow.Add(time.Second)}))
	m, _ = m.Step(Incoming(Line{Window: "#other", Network: "libera", Nick: "bob", Text: "hello", Kind: LineMessage, Time: mergeNow.Add(2 * time.Second)}))
	return m.SetActive(m.keyFor("#salotto"))
}

// mergeWindowText is one window's own buffer, joined. Deliberately the member buffer rather
// than the drawn frame, so an echo test can say which window received the line.
func mergeWindowText(m Model, key string) string {
	var b strings.Builder
	for _, l := range m.windows[key] {
		b.WriteString(l.Text)
		b.WriteString("\n")
	}
	return b.String()
}

// TestTheMergedViewShowsBothWindowsInTimeOrder.
//
// The mutation is concatenation: walking #a then #b draws "alpha, charlie, bravo", which is a
// list of two buffers glued together, not a conversation. The timestamps are fixed and
// interleaved so the wrong order is deterministic.
func TestTheMergedViewShowsBothWindowsInTimeOrder(t *testing.T) {
	m := mergeModel(t)
	m, _ = m.Step(Incoming(Line{Window: "#a", Network: "libera", Nick: "alice", Text: "charlie", Kind: LineMessage, Time: mergeNow.Add(3 * time.Second)}))
	m = typeLine(m, "/merge #a #b")

	body := conversationText(m)
	alpha := strings.Index(body, "alpha")
	bravo := strings.Index(body, "bravo")
	charlie := strings.Index(body, "charlie")
	if alpha < 0 || bravo < 0 || charlie < 0 {
		t.Fatalf("a member's line is missing from the merged view:\n%s", body)
	}
	if !(alpha < bravo && bravo < charlie) {
		t.Fatalf("the merged rows are not oldest first: alpha=%d bravo=%d charlie=%d\n%s", alpha, bravo, charlie, body)
	}
}

// TestAMergedRowNamesItsWindow.
//
// A cross-window view whose rows have no source is a conversation with no attribution: the
// reader cannot tell which channel a question is for. The mutation is dropping the "[#a] "
// prefix, which leaves both rows readable and both rows useless.
func TestAMergedRowNamesItsWindow(t *testing.T) {
	m := mergedTwo(t)
	body := conversationText(m)
	if !strings.Contains(body, "[#a] alpha") {
		t.Fatalf("the row from #a does not name its window:\n%s", body)
	}
	if !strings.Contains(body, "[#b] bravo") {
		t.Fatalf("the row from #b does not name its window:\n%s", body)
	}
}

// TestTheMergedViewHoldsNoSlice.
//
// The merged view must be derived at View time. A line arriving after the merge has to show up
// without anything appending it to a merged buffer, and drawing the view must not rewrite a
// member's history. The mutation is one `m.windows[mergeKey] = append(...)` at merge time or at
// arrival: then the merged key holds a transcript, the new line is either missing or doubled,
// and this test goes red on the buffer length before it ever gets to the pixels.
func TestTheMergedViewHoldsNoSlice(t *testing.T) {
	m := mergedTwo(t)
	key := m.keyFor("#a")
	beforeMember := len(m.windows[key])
	if got := len(m.windows[mergeKey]); got != 0 {
		t.Fatalf("the merged window already holds %d copied line(s) after /merge", got)
	}

	m, _ = m.Step(Incoming(Line{Window: "#a", Network: "libera", Nick: "alice", Text: "arrived later", Kind: LineMessage, Time: mergeNow.Add(4 * time.Second)}))
	if got := len(m.windows[mergeKey]); got != 0 {
		t.Fatalf("an incoming line was copied into the merged window: %d line(s)", got)
	}
	if got := len(m.windows[key]); got != beforeMember+1 {
		t.Fatalf("the member holds %d lines, want %d", got, beforeMember+1)
	}

	body := conversationText(m)
	if !strings.Contains(body, "arrived later") {
		t.Fatalf("a line that arrived after /merge is missing from the view:\n%s", body)
	}
	if got := len(m.windows[mergeKey]); got != 0 {
		t.Fatalf("drawing the merged view wrote %d copied line(s) into it", got)
	}
	if got := len(m.windows[key]); got != beforeMember+1 {
		t.Fatalf("drawing the merged view changed the member's history: %d, want %d", got, beforeMember+1)
	}
}

// TestTypingGoesToTheTarget.
//
// The Sent is the only thing the transport reads, so the target has to be in it. The mutation
// is returning Sent{Text} with the merged key (or no window at all): the local echo still
// appears, which is why the test inspects the command rather than the screen.
func TestTypingGoesToTheTarget(t *testing.T) {
	m := mergedTwo(t)
	target := m.keyFor("#a")
	if m.mergeTarget != target {
		t.Fatalf("the first target is %q, want %q", m.mergeTarget, target)
	}

	m, cmd := typeLineStep(m, "hello target")
	if cmd == nil {
		t.Fatal("Enter produced no command, so nothing would reach the transport")
	}
	sent, ok := cmd().(Sent)
	if !ok {
		t.Fatalf("Enter produced %T, want Sent", cmd())
	}
	if sent.Window != target {
		t.Fatalf("Sent.Window = %q, want the target %q (the merged key is %q)", sent.Window, target, mergeKey)
	}
	if sent.Text != "hello target" {
		t.Fatalf("Sent.Text = %q, want the typed line", sent.Text)
	}

	echoed, _ := m.Update(sent)
	m2 := echoed.(Model)
	if !strings.Contains(mergeWindowText(m2, target), "hello target") {
		t.Fatalf("the local echo did not land in the target window %q", target)
	}
	if strings.Contains(mergeWindowText(m2, mergeKey), "hello target") {
		t.Fatal("the local echo landed in the merged window, which has no buffer to hold it")
	}
}

// TestCtrlTCyclesTheTarget.
//
// The title changing is not enough evidence on its own - a key that changes the target and
// says nothing leaves the user guessing - so this also asserts the one-line confirmation in
// the merged scrollback. The mutation is a no-op for ctrl-t: the target stays on #a and both
// assertions fail.
func TestCtrlTCyclesTheTarget(t *testing.T) {
	m := mergedTwo(t)
	a, b := m.keyFor("#a"), m.keyFor("#b")
	if m.mergeTarget != a {
		t.Fatalf("the first target is %q, want %q", m.mergeTarget, a)
	}

	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyCtrlT})
	if m.mergeTarget != b {
		t.Fatalf("after ctrl-t the target is %q, want %q", m.mergeTarget, b)
	}
	if body := conversationText(m); !strings.Contains(body, "target → #b") {
		t.Fatalf("ctrl-t did not name the new target in the merged scrollback:\n%s", body)
	}

	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyCtrlT})
	if m.mergeTarget != a {
		t.Fatalf("the target did not wrap back to %q, it is %q", a, m.mergeTarget)
	}
}

// TestTheTitleNamesTheTarget.
//
// The target is the answer to "where will this line go", and the title is the only place it
// is always visible. The mutation is rendering just winName(m.active), which gives "merge" -
// true, and useless.
func TestTheTitleNamesTheTarget(t *testing.T) {
	m := mergedTwo(t)
	title := strings.Split(plainText(m.View()), "\n")[0]
	if !strings.Contains(title, "merge → #a") {
		t.Fatalf("the title does not name the target: %q", title)
	}

	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyCtrlT})
	title = strings.Split(plainText(m.View()), "\n")[0]
	if !strings.Contains(title, "merge → #b") {
		t.Fatalf("the title still does not follow the target: %q", title)
	}
}

// TestClosingTheTargetMovesIt.
//
// /close in the merged view closes the target member. Leaving the target pointing at the
// closed window would print a recipient that the next line cannot reach. The mutation is
// deleting the window without moving mergeTarget.
func TestClosingTheTargetMovesIt(t *testing.T) {
	m := mergedTwo(t)
	a, b := m.keyFor("#a"), m.keyFor("#b")

	m = typeLine(m, "/close")
	if m.mergeTarget != b {
		t.Fatalf("after closing the target the target is %q, want the surviving %q", m.mergeTarget, b)
	}
	if !m.merge || m.active != mergeKey {
		t.Fatalf("/close closed the view instead of its target: merge=%v active=%q", m.merge, m.active)
	}
	if _, live := m.windows[a]; live {
		t.Fatalf("the closed target %q is still a window", a)
	}
}

// TestTheLastMemberClosingClosesTheMerge.
//
// A merged view with no members is a window the user cannot type in or leave except /close.
// The mutation is leaving the empty view open.
func TestTheLastMemberClosingClosesTheMerge(t *testing.T) {
	m := mergedTwo(t)
	m = typeLine(m, "/close")
	m = typeLine(m, "/close")

	if m.merge {
		t.Fatalf("the merged view survived the last member: members=%v target=%q", m.mergeMembers, m.mergeTarget)
	}
	if _, live := m.windows[mergeKey]; live {
		t.Fatal("the merged window is still in the window map after its last member closed")
	}
	if m.active == mergeKey {
		t.Fatal("the client is still looking at a merged view that no longer exists")
	}
}

// TestTheMergeIsNotABuffer.
//
// A merged view is a way of reading buffers, not one of them: a number would make /b offer a
// jump into a view, and a badge would be a second answer to "how much is new" that disagrees
// with every member's own count. The mutation is letting assignBuffer number salotto:merge,
// which also makes it appear in /b.
func TestTheMergeIsNotABuffer(t *testing.T) {
	m := mergedTwo(t)
	// A Step runs ensureBuffers, which is where an implementation that forgot to exempt the
	// merged key hands it a number. Without this the mutation would lie dormant until the
	// next redraw and the test would inspect a map nobody had filled yet.
	m, _ = m.Step(tea.WindowSizeMsg{Width: 120, Height: 30})
	if n := m.bufferNo[mergeKey]; n != 0 {
		t.Fatalf("the merged window was given buffer number %d", n)
	}
	if list := m.bufferList(); strings.Contains(list, "merge") {
		t.Fatalf("/b lists the merged view: %s", list)
	}
	row := m.windowRow(mergeKey)
	if row.text != "merge" {
		t.Fatalf("the sidebar row is %q, want just \"merge\" - no number", row.text)
	}
	if row.badge != "" {
		t.Fatalf("the merged row carries badge %q, want none", row.badge)
	}
	for _, key := range m.navigationOrder() {
		if key == mergeKey {
			t.Fatal("the merged view joined the /next walk")
		}
	}
}

// TestAOneArgumentMergeStillPullsInTheCurrentWindow.
//
// The convenience is the common case: the user is in a channel and names the other network's
// same-named channel, so one argument means "this one and that one". The mutation is dropping
// the rule entirely, which opens the merged view with only #other and leaves #salotto out -
// the fix for a bad name must not delete what a good name does.
func TestAOneArgumentMergeStillPullsInTheCurrentWindow(t *testing.T) {
	m := salottoModel(t)
	salotto, other := m.keyFor("#salotto"), m.keyFor("#other")

	m = typeLine(m, "/merge #other")

	if !m.merge || m.active != mergeKey {
		t.Fatalf("/merge #other did not open the merged view: merge=%v active=%q", m.merge, m.active)
	}
	if !m.isMergeMember(salotto) {
		t.Fatalf("the current window %q was not merged in: members=%v", salotto, m.mergeMembers)
	}
	if !m.isMergeMember(other) {
		t.Fatalf("the named window %q was not merged in: members=%v", other, m.mergeMembers)
	}
}

// TestMergingIsPerClientNotInTheBlob.
//
// Membership is this client's arrangement of its own windows, not shared state: two attached
// clients merging different channels is normal, and a merge that follows a user between a
// browser tab and a terminal is a surprise. The mutation is calling saveSettings() from
// /merge; the settings path here is a directory, so that call fails and draws "settings not
// saved" into the merged scrollback, which this test refuses.
func TestMergingIsPerClientNotInTheBlob(t *testing.T) {
	m := mergeModel(t)
	// A directory, not a file: SaveSettings cannot write it, so any attempt to persist the
	// merge leaves a visible error rather than a byte-identical rewrite this test would miss.
	m.settingsPath = t.TempDir()
	before, err := m.settings.token()
	if err != nil {
		t.Fatalf("encoding the settings before /merge: %v", err)
	}

	m = typeLine(m, "/merge #a #b")

	after, err := m.settings.token()
	if err != nil {
		t.Fatalf("encoding the settings after /merge: %v", err)
	}
	if after != before {
		t.Fatal("/merge changed the settings blob")
	}
	if body := conversationText(m); strings.Contains(body, "settings not saved") {
		t.Fatalf("/merge tried to persist the merge in the settings file:\n%s", body)
	}
	// A failed save is drawn in whichever window was active when it was attempted, which may
	// not be the merged one. Scan every buffer so the mutation is caught wherever it writes.
	for key, lines := range m.windows {
		for _, l := range lines {
			if strings.Contains(l.Text, "settings not saved") {
				t.Fatalf("/merge tried to persist the merge in the settings file (reported in %q): %s", key, l.Text)
			}
		}
	}
}

// TestAFailedMergeCreatesNothing.
//
// A command that reports failure and then changes the screen is two answers to one question.
// The mutation is creating the merged window before validating the names: the reply still says
// "no window matches", but the user is now standing in a merge they did not ask for.
func TestAFailedMergeCreatesNothing(t *testing.T) {
	m := mergeModel(t)
	before := m.active

	m = typeLine(m, "/merge nosuchwindow")

	if m.merge {
		t.Fatalf("a failed /merge opened the merged view: members=%v", m.mergeMembers)
	}
	if _, live := m.windows[mergeKey]; live {
		t.Fatal("a failed /merge created the merged window")
	}
	if m.active != before {
		t.Fatalf("a failed /merge switched the active window from %q to %q", before, m.active)
	}
}

// TestAFailedMergeNamesTheWindow.
//
// "no window matches" without the name leaves the user guessing which of several arguments was
// the bad one. The mutation is dropping the note, which makes the command silent.
func TestAFailedMergeNamesTheWindow(t *testing.T) {
	m := mergeModel(t)

	m = typeLine(m, "/merge nosuchwindow")

	if body := conversationText(m); !strings.Contains(body, "nosuchwindow") {
		t.Fatalf("the failure does not name the window that did not resolve:\n%s", body)
	}
}

// TestAPartlyResolvedMergeDoesNothing.
//
// If one name resolves and another does not, applying the half that resolved leaves a view that
// no command calls half-done. The mutation is applying the resolved subset: #salotto joins the
// membership while the command reports nosuchwindow.
func TestAPartlyResolvedMergeDoesNothing(t *testing.T) {
	m := mergedTwo(t)
	m, _ = m.Step(Incoming(Line{Window: "#salotto", Network: "libera", Nick: "alice", Text: "ciao", Kind: LineMessage, Time: mergeNow.Add(3 * time.Second)}))
	salotto := m.keyFor("#salotto")
	before := strings.Join(m.mergeMembers, ",")
	beforeActive := m.active

	m = typeLine(m, "/merge "+winName(salotto)+" nosuchwindow")

	if !m.merge {
		t.Fatal("a partly resolved /merge closed the merged view")
	}
	if after := strings.Join(m.mergeMembers, ","); after != before {
		t.Fatalf("a partly resolved /merge changed membership from %q to %q", before, after)
	}
	if m.isMergeMember(salotto) {
		t.Fatalf("a partly resolved /merge added the name that did resolve: members=%v", m.mergeMembers)
	}
	if m.active != beforeActive {
		t.Fatalf("a partly resolved /merge switched the active window from %q to %q", beforeActive, m.active)
	}
}
