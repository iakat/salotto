package facade

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/iakat/salotto/internal/winring"
)

// smallRings returns a fixture whose windows each hold 8 segments of 512 bytes,
// so a busy window can overflow its own ring without touching another.
func smallRings(t *testing.T) (*Server, *fakeNet, *Subscriber, *bytes.Buffer) {
	t.Helper()
	fn := newFakeNet("libera")
	fn.set = winring.New(8, 8, 512)
	s, err := New(testConfig(fn))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	sub := NewSubscriber(&out, 64*1024)
	t.Cleanup(sub.Close)
	return s, fn, sub, &out
}

func addToWindow(fn *fakeNet, window, body string) uint64 {
	return fn.set.Append(window, []byte(":n!u@h PRIVMSG "+window+" :"+body), time.Now().UnixNano())
}

// windowBodies extracts the trailing body of lines addressed to window.
func windowBodies(lines []string, window string) []string {
	marker := "PRIVMSG " + window + " :"
	var out []string
	for _, l := range lines {
		l = withoutTags(l)
		if i := strings.Index(l, marker); i >= 0 {
			out = append(out, l[i+len(marker):])
		}
	}
	return out
}

// A busy window must not evict a quiet window's lines: with per-window rings a
// quiet window replays everything it buffered, with no GAP.
func TestBusyWindowDoesNotEvictQuietWindow(t *testing.T) {
	s, fn, sub, out := smallRings(t)
	s.AttachWindow(sub, "libera", "#busy")
	s.AttachWindow(sub, "libera", "#quiet")

	for i := 0; i < 3; i++ {
		addToWindow(fn, "#quiet", "quiet-"+string(rune('a'+i)))
	}
	for i := 0; i < 1000; i++ {
		addToWindow(fn, "#busy", "busy")
	}
	_ = flushLines(t, sub, out)

	if !s.PlayBuffer(sub, "libera", "#quiet") {
		t.Fatal("PlayBuffer failed")
	}
	lines := flushLines(t, sub, out)
	for _, line := range lines {
		if strings.Contains(line, " GAP ") {
			t.Fatalf("quiet window saw a GAP: %q", line)
		}
	}
	got := windowBodies(lines, "#quiet")
	want := []string{"quiet-a", "quiet-b", "quiet-c"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("quiet replay = %v, want %v", got, want)
	}
}

// A window that genuinely overflows reports the exact lost range for its own
// ring only.
func TestGapIsPerWindow(t *testing.T) {
	s, fn, sub, out := smallRings(t)
	s.AttachWindow(sub, "libera", "#busy")
	s.AttachWindow(sub, "libera", "#quiet")
	for i := 0; i < 3; i++ {
		addToWindow(fn, "#quiet", "quiet")
	}
	_ = flushLines(t, sub, out)
	s.AckWindow(sub, "libera", "#busy", 1)
	for i := 0; i < 1000; i++ {
		addToWindow(fn, "#busy", "busy")
	}
	_ = flushLines(t, sub, out)
	r, _ := fn.set.Ring("#busy")
	oldest := r.Oldest()

	if !s.PlayBuffer(sub, "libera", "#busy") {
		t.Fatal("PlayBuffer failed")
	}
	lines := flushLines(t, sub, out)
	if len(lines) < 2 {
		t.Fatalf("busy replay wrote %d lines, want GAP plus survivors", len(lines))
	}
	if !strings.Contains(lines[0], " GAP libera #busy 2 "+uitoa(oldest-1)) {
		t.Fatalf("GAP = %q, want lost range 2..%d for #busy", lines[0], oldest-1)
	}
}

// ClearBuffer physically drops the window's lines and leaves other windows
// intact.
func TestClearBufferPhysicallyDrops(t *testing.T) {
	s, fn, sub, out := smallRings(t)
	s.AttachWindow(sub, "libera", "#c")
	s.AttachWindow(sub, "libera", "#other")
	for i := 0; i < 5; i++ {
		addToWindow(fn, "#c", "old")
	}
	addToWindow(fn, "#other", "keep")
	_ = flushLines(t, sub, out)

	ok, msg := s.ClearBuffer("libera", "#c")
	if !ok {
		t.Fatalf("ClearBuffer failed: %s", msg)
	}
	if r, ok := fn.set.Ring("#c"); ok {
		if got := r.Bytes(); got != 0 {
			t.Fatalf("bytes after clear = %d, want 0", got)
		}
	}
	if !s.PlayBuffer(sub, "libera", "#c") {
		t.Fatal("PlayBuffer failed")
	}
	if got := flushLines(t, sub, out); len(got) != 0 {
		t.Fatalf("cleared window replayed %v", got)
	}

	or, _ := fn.set.Ring("#other")
	if got := or.Bytes(); got == 0 {
		t.Fatal("clearing #c emptied #other")
	}
	if !s.PlayBuffer(sub, "libera", "#other") {
		t.Fatal("PlayBuffer failed")
	}
	if got := windowBodies(flushLines(t, sub, out), "#other"); strings.Join(got, ",") != "keep" {
		t.Fatalf("#other replay = %v, want [keep]", got)
	}
}

// The same line buffered in two windows has independent sequences and cursors.
func TestSharedLineHasIndependentCursors(t *testing.T) {
	s, fn, sub, out := smallRings(t)
	s.AttachWindow(sub, "libera", "#a")
	s.AttachWindow(sub, "libera", "#b")

	line := ":n!u@h PRIVMSG #a :shared"
	// Buffer extra lines in #a so its sequence advances independently.
	fn.set.Append("#a", []byte(":n!u@h PRIVMSG #a :before"), time.Now().UnixNano())
	seqA := fn.set.Append("#a", []byte(line), time.Now().UnixNano())
	seqB := fn.set.Append("#b", []byte(":n!u@h PRIVMSG #b :shared"), time.Now().UnixNano())
	if seqA == seqB || seqA != seqB+1 {
		t.Fatalf("shared line seqs = #a %d, #b %d; want independent counters", seqA, seqB)
	}

	if !s.PlayBuffer(sub, "libera", "#a") || !s.PlayBuffer(sub, "libera", "#b") {
		t.Fatal("PlayBuffer failed")
	}
	outLines := flushLines(t, sub, out)
	if got := windowBodies(outLines, "#a"); strings.Join(got, ",") != "before,shared" {
		t.Fatalf("#a replay = %v", got)
	}
	if got := windowBodies(outLines, "#b"); strings.Join(got, ",") != "shared" {
		t.Fatalf("#b replay = %v", got)
	}

	// Acking #a at a later seq than #b must not move #b's cursor.
	s.AckWindow(sub, "libera", "#a", seqA+5)
	if cursor, ok := s.WindowCursor(sub, "libera", "#b"); !ok || cursor != seqB {
		t.Fatalf("#b cursor = %d (ok=%v), want %d", cursor, ok, seqB)
	}
}
