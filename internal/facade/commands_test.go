package facade

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/iakat/salotto/internal/ircline"
)

// runWindowCmd drives one client command through the same dispatch as the read
// loop and returns the framed lines it wrote. The caller must flush the
// subscriber before calling it so only the command's own output is returned.
func runWindowCmd(t *testing.T, s *Server, sub *Subscriber, line string) []string {
	t.Helper()
	c := &client{srv: s, sub: sub, nick: "test"}
	raw := []byte(line)
	_, rest := ircline.Tags(raw)
	_, rest = ircline.Prefix(rest)
	if !c.handle(raw, rest) {
		t.Fatalf("%s: handler asked to close the client", line)
	}
	buf := sub.w.(*bytes.Buffer)
	if err := sub.Flush(); err != nil {
		t.Fatal(err)
	}
	data := strings.TrimRight(buf.String(), "\r\n")
	buf.Reset()
	if data == "" {
		return nil
	}
	return strings.Split(data, "\r\n")
}

func notices(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if i := strings.Index(line, " NOTICE test :"); i >= 0 {
			out = append(out, line[i+len(" NOTICE test :"):])
		}
	}
	return out
}

func markKnown(s *Server, network string, names ...string) {
	for i, name := range names {
		line := ":n!u@h PRIVMSG " + name + " :seed"
		s.BroadcastWindow(network, name, uint64(i+1), []byte(line), time.Now().UnixNano())
	}
}

func TestAttachDetachCommandCounts(t *testing.T) {
	s, _, sub, out := newWindowFixture(t)
	markKnown(s, "libera", "#a", "#b")
	_ = flushLines(t, sub, out)

	tests := []struct {
		cmd  string
		want []string
	}{
		{"ATTACH #a,#b", []string{
			"There were 2 channels matching [#a,#b]",
			"Attached 2 channels",
		}},
		{"ATTACH #a,#b", []string{
			"There were 2 channels matching [#a,#b]",
			"Attached 0 channels",
		}},
		{"ATTACH #a", []string{
			"There was 1 channel matching [#a]",
			"Attached 0 channels",
		}},
		{"DETACH #a", []string{
			"There was 1 channel matching [#a]",
			"Detached 1 channel",
		}},
		{"DETACH #a,#b", []string{
			"There were 2 channels matching [#a,#b]",
			"Detached 1 channel",
		}},
		{"DETACH #nope", []string{
			"There were 0 channels matching [#nope]",
			"Detached 0 channels",
		}},
	}
	for _, tt := range tests {
		got := notices(runWindowCmd(t, s, sub, tt.cmd))
		if strings.Join(got, "\n") != strings.Join(tt.want, "\n") {
			t.Fatalf("%s = %q, want %q", tt.cmd, got, tt.want)
		}
	}
}

func TestDetachCommandKeepsCursor(t *testing.T) {
	s, fn, sub, out := newWindowFixture(t)
	fx := &windowFixture{}
	_ = flushLines(t, sub, out)

	markKnown(s, "libera", "#c")
	notices(runWindowCmd(t, s, sub, "ATTACH #c"))
	for _, body := range []string{"one", "two", "three"} {
		fx.addLive(s, fn, body)
	}
	_ = flushLines(t, sub, out)
	s.AckWindow(sub, "libera", "#c", 3)

	notices(runWindowCmd(t, s, sub, "DETACH #c"))
	fx.addLive(s, fn, "while-away-1")
	fx.addLive(s, fn, "while-away-2")
	if got := flushLines(t, sub, out); len(got) != 0 {
		t.Fatalf("detached window still delivered %v", got)
	}

	notices(runWindowCmd(t, s, sub, "ATTACH #c"))
	got := bodies(runWindowCmd(t, s, sub, "PLAYBUFFER #c"))
	want := "while-away-1,while-away-2"
	if strings.Join(got, ",") != want {
		t.Fatalf("replay after re-attach = %v, want %s", got, want)
	}
}

func TestPlayBufferEmptyCommand(t *testing.T) {
	s, _, sub, out := newWindowFixture(t)
	_ = flushLines(t, sub, out)
	markKnown(s, "libera", "#empty")
	_ = flushLines(t, sub, out)
	notices(runWindowCmd(t, s, sub, "ATTACH #empty"))
	lines := runWindowCmd(t, s, sub, "PLAYBUFFER #empty")
	got := notices(lines)
	want := []string{"The buffer for channel #empty is empty"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("PlayBuffer empty = %q, want %q", got, want)
	}
	if len(lines) != 1 {
		t.Fatalf("empty PlayBuffer wrote %d lines, want only the notice: %q", len(lines), lines)
	}
}

func TestPlayBufferGapCommand(t *testing.T) {
	s, fn, sub, out := newWindowFixture(t)
	fx := &windowFixture{}
	_ = flushLines(t, sub, out)
	markKnown(s, "libera", "#c")
	notices(runWindowCmd(t, s, sub, "ATTACH #c"))
	for _, body := range []string{"one", "two", "three"} {
		fx.addLive(s, fn, body)
	}
	_ = flushLines(t, sub, out)
	s.AckWindow(sub, "libera", "#c", 1)

	for i := 0; i < 6000; i++ {
		fx.addLive(s, fn, "filler")
		if i%256 == 0 {
			_ = flushLines(t, sub, out)
		}
	}
	_ = flushLines(t, sub, out)
	cring, _ := fn.set.Ring("#c")
	if oldest := cring.Oldest(); oldest <= 2 {
		t.Fatalf("ring did not truncate seq 2 (oldest %d)", oldest)
	}

	lines := runWindowCmd(t, s, sub, "PLAYBUFFER #c")
	if len(lines) < 2 {
		t.Fatalf("GAP replay wrote %d lines, want a GAP plus survivors", len(lines))
	}
	if !strings.Contains(lines[0], " GAP ") {
		t.Fatalf("first replay line = %q, want GAP marker", lines[0])
	}
}

func TestClearBufferCommandAdvancesCursor(t *testing.T) {
	s, fn, sub, out := newWindowFixture(t)
	fx := &windowFixture{}
	_ = flushLines(t, sub, out)
	markKnown(s, "libera", "#c")
	_ = flushLines(t, sub, out)
	notices(runWindowCmd(t, s, sub, "ATTACH #c"))
	for i := 0; i < 10; i++ {
		fx.addLive(s, fn, "old")
	}
	_ = flushLines(t, sub, out)

	got := notices(runWindowCmd(t, s, sub, "CLEARBUFFER #c"))
	want := []string{"1 buffer matching #c has been cleared"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ClearBuffer = %q, want %q", got, want)
	}

	notices(runWindowCmd(t, s, sub, "DETACH #c"))
	notices(runWindowCmd(t, s, sub, "ATTACH #c"))
	if got := notices(runWindowCmd(t, s, sub, "PLAYBUFFER #c")); strings.Join(got, ",") != "The buffer for channel #c is empty" {
		t.Fatalf("cleared buffer PlayBuffer = %q, want the empty-buffer notice", got)
	}

	notices(runWindowCmd(t, s, sub, "DETACH #c"))
	fx.addLive(s, fn, "new-one")
	fx.addLive(s, fn, "new-two")
	notices(runWindowCmd(t, s, sub, "ATTACH #c"))
	gotBodies := bodies(runWindowCmd(t, s, sub, "PLAYBUFFER #c"))
	if strings.Join(gotBodies, ",") != "new-one,new-two" {
		t.Fatalf("replay after clear+new = %v, want new-one,new-two", gotBodies)
	}

	if cursor, ok := s.WindowCursor(sub, "libera", "#c"); !ok || cursor < 10 {
		t.Fatalf("cursor after clear = %d (ok=%v), want it past the cleared lines", cursor, ok)
	}
}

func TestClearBufferNoMatch(t *testing.T) {
	s, _, sub, out := newWindowFixture(t)
	_ = flushLines(t, sub, out)
	got := notices(runWindowCmd(t, s, sub, "CLEARBUFFER #nope"))
	want := []string{"0 buffers matching #nope have been cleared"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ClearBuffer no match = %q, want %q", got, want)
	}
}

func TestClearAllBuffers(t *testing.T) {
	s, fn, sub, out := newWindowFixture(t)
	fx := &windowFixture{}
	_ = flushLines(t, sub, out)
	markKnown(s, "libera", "#c")
	_ = flushLines(t, sub, out)
	notices(runWindowCmd(t, s, sub, "ATTACH #c"))
	fx.addLive(s, fn, "chan")
	s.BroadcastWindow("libera", "alice", 99, []byte(":n!u@h PRIVMSG alice :query"), time.Now().UnixNano())
	_ = flushLines(t, sub, out)

	if got := notices(runWindowCmd(t, s, sub, "CLEARALLCHANNELBUFFERS")); strings.Join(got, "\n") != "All channel buffers have been cleared" {
		t.Fatalf("ClearAllChannelBuffers = %q", got)
	}
	if r, ok := fn.set.Ring("#c"); ok {
		if got := r.Newest(); got != 0 {
			t.Fatalf("#c newest after clear = %d, want 0", got)
		}
	}
	if r, ok := fn.set.Ring("alice"); ok && r.Newest() == 0 {
		t.Fatal("query ring was emptied by a channel clear")
	}
	if got := notices(runWindowCmd(t, s, sub, "CLEARALLQUERYBUFFERS")); strings.Join(got, "\n") != "All query buffers have been cleared" {
		t.Fatalf("ClearAllQueryBuffers = %q", got)
	}
	if got := notices(runWindowCmd(t, s, sub, "CLEARALLBUFFERS")); strings.Join(got, "\n") != "All buffers have been cleared" {
		t.Fatalf("ClearAllBuffers = %q", got)
	}
	if got := notices(runWindowCmd(t, s, sub, "CLEARALLBUFFERS")); strings.Join(got, "\n") != "All buffers have been cleared" {
		t.Fatalf("repeated ClearAllBuffers = %q", got)
	}
}

func TestDisableChanCommand(t *testing.T) {
	s, fn, sub, out := newWindowFixture(t)
	fx := &windowFixture{}
	_ = flushLines(t, sub, out)
	markKnown(s, "libera", "#c")
	_ = flushLines(t, sub, out)
	notices(runWindowCmd(t, s, sub, "ATTACH #c"))
	fx.addLive(s, fn, "old")
	_ = flushLines(t, sub, out)

	got := notices(runWindowCmd(t, s, sub, "DISABLECHAN #c"))
	want := []string{"There was 1 channel matching [#c]", "Disabled 1 channel"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("DisableChan = %q, want %q", got, want)
	}

	fx.addLive(s, fn, "while-disabled")
	if got := bodies(flushLines(t, sub, out)); strings.Join(got, ",") != "while-disabled" {
		t.Fatalf("disabled window did not deliver live line: %v", got)
	}

	got = notices(runWindowCmd(t, s, sub, "PLAYBUFFER #c"))
	want = []string{"The buffer for channel #c is empty"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("PlayBuffer while disabled = %q, want %q", got, want)
	}

	list := notices(runWindowCmd(t, s, sub, "LISTCHANS"))
	found := false
	for _, line := range list {
		if strings.Contains(line, "#c") && strings.Contains(line, "Disabled") {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListChans did not show #c as Disabled: %q", list)
	}

	got = notices(runWindowCmd(t, s, sub, "ENABLECHAN #c"))
	want = []string{"There was 1 channel matching [#c]", "Enabled 1 channel"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("EnableChan = %q, want %q", got, want)
	}

	notices(runWindowCmd(t, s, sub, "DETACH #c"))
	fx.addLive(s, fn, "after-enable")
	fx.addLive(s, fn, "later")
	notices(runWindowCmd(t, s, sub, "ATTACH #c"))
	// Re-enabling starts buffering again; it does not drop the lines buffered
	// before the disable, so the replay is the whole window.
	if got := bodies(runWindowCmd(t, s, sub, "PLAYBUFFER #c")); strings.Join(got, ",") != "old,while-disabled,after-enable,later" {
		t.Fatalf("replay after enable = %v, want the whole buffered window", got)
	}
}

func TestMatchWindows(t *testing.T) {
	s, _, _, _ := newWindowFixture(t)
	markKnown(s, "libera", "#a", "#b", "#ab", "#abc")

	tests := []struct {
		arg  string
		want string
	}{
		{"#a,#b", "#a,#b"},
		{"#a #b", "#a,#b"},
		{"#*", "#a,#ab,#abc,#b"},
		{"#A", "#a"},
		{"#?", "#a,#b"},
		{"#zzz", ""},
		{"#a,#zzz", "#a"},
	}
	for _, tt := range tests {
		got := strings.Join(s.matchWindows("libera", tt.arg), ",")
		if got != tt.want {
			t.Fatalf("matchWindows(%q) = %q, want %q", tt.arg, got, tt.want)
		}
	}
}

func TestListChansTable(t *testing.T) {
	s, _, sub, out := newWindowFixture(t)
	_ = flushLines(t, sub, out)
	markKnown(s, "libera", "#joined", "#detached", "#disabled")
	notices(runWindowCmd(t, s, sub, "ATTACH #joined"))
	notices(runWindowCmd(t, s, sub, "DISABLECHAN #disabled"))

	lines := notices(runWindowCmd(t, s, sub, "LISTCHANS"))
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"| Name      | Status   |",
		"| #detached | Detached |",
		"| #disabled | Disabled |",
		"| #joined   | Joined   |",
		"Total: 3, Joined: 1, Detached: 1, Disabled: 1",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("ListChans missing %q:\n%s", want, joined)
		}
	}
}

func TestWindowCommandsWithoutNetwork(t *testing.T) {
	s, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	sub := NewSubscriber(&out, 64*1024)
	t.Cleanup(sub.Close)

	for _, cmd := range []string{
		"ATTACH #c",
		"DETACH #c",
		"PLAYBUFFER #c",
		"CLEARBUFFER #c",
		"CLEARALLCHANNELBUFFERS",
		"CLEARALLQUERYBUFFERS",
		"CLEARALLBUFFERS",
		"DISABLECHAN #c",
		"ENABLECHAN #c",
		"LISTCHANS",
	} {
		got := notices(runWindowCmd(t, s, sub, cmd))
		want := "You must be connected with a network to use this command"
		if len(got) != 1 || got[0] != want {
			t.Fatalf("%s = %q, want %q", cmd, got, want)
		}
	}
}

func TestWindowStats(t *testing.T) {
	s, _, _, _ := newWindowFixture(t)
	markKnown(s, "libera", "#a", "#b")
	notices(runWindowCmd(t, s, NewSubscriber(&bytes.Buffer{}, 4096), "DISABLECHAN #b"))
	known, disabled := s.WindowStats("libera")
	if known != 2 || disabled != 1 {
		t.Fatalf("WindowStats = (%d, %d), want (2, 1)", known, disabled)
	}
}
