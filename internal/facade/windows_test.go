package facade

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func newWindowFixture(t *testing.T) (*Server, *fakeNet, *Subscriber, *bytes.Buffer) {
	t.Helper()
	fn := newFakeNet("libera")
	s, err := New(testConfig(fn))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	sub := NewSubscriber(&out, 64*1024)
	t.Cleanup(sub.Close)
	return s, fn, sub, &out
}

func (fx *windowFixture) addLive(s *Server, fn *fakeNet, body string) uint64 {
	line := ":n!u@h PRIVMSG #c :" + body
	seq := fn.add("#c", line, time.Now().UnixNano())
	s.BroadcastWindow("libera", "#c", seq, []byte(line), time.Now().UnixNano())
	return seq
}

type windowFixture struct{}

func bodies(lines []string) []string {
	var out []string
	for _, l := range lines {
		l = withoutTags(l)
		if i := strings.Index(l, "PRIVMSG #c :"); i >= 0 {
			out = append(out, l[i+len("PRIVMSG #c :"):])
		}
	}
	return out
}

func flushLines(t *testing.T, sub *Subscriber, out *bytes.Buffer) []string {
	t.Helper()
	if err := sub.Flush(); err != nil {
		t.Fatal(err)
	}
	data := strings.TrimRight(out.String(), "\r\n")
	out.Reset()
	if data == "" {
		return nil
	}
	return strings.Split(data, "\r\n")
}

func TestPlayBufferResumesFromCursor(t *testing.T) {
	s, fn, sub, out := newWindowFixture(t)
	fx := &windowFixture{}
	s.AttachWindow(sub, "libera", "#c")
	for _, body := range []string{"one", "two", "three", "four"} {
		fx.addLive(s, fn, body)
	}
	_ = flushLines(t, sub, out)

	s.AckWindow(sub, "libera", "#c", 2)

	if !s.PlayBuffer(sub, "libera", "#c") {
		t.Fatal("PlayBuffer failed")
	}
	got := bodies(flushLines(t, sub, out))
	want := []string{"three", "four"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("replay after ack = %v, want %v", got, want)
	}
}

func TestPlayBufferEmitsGap(t *testing.T) {
	s, fn, sub, out := newWindowFixture(t)
	fx := &windowFixture{}
	s.AttachWindow(sub, "libera", "#c")
	for _, body := range []string{"one", "two", "three"} {
		fx.addLive(s, fn, body)
	}
	_ = flushLines(t, sub, out)
	s.AckWindow(sub, "libera", "#c", 1)

	// A tiny ring: adding more lines evicts the client's cursor (seq 2..).
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

	if !s.PlayBuffer(sub, "libera", "#c") {
		t.Fatal("PlayBuffer failed")
	}
	lines := flushLines(t, sub, out)
	if len(lines) == 0 {
		t.Fatal("no output")
	}
	first := lines[0]
	if !strings.Contains(first, " GAP ") {
		t.Fatalf("first replay line = %q, want GAP marker", first)
	}
	oldest := cring.Oldest()
	if !strings.Contains(first, " 2 "+uitoa(oldest-1)) {
		t.Fatalf("GAP = %q, want lost range 2..%d", first, oldest-1)
	}
	if len(lines) < 2 {
		t.Fatal("GAP was not followed by surviving lines")
	}
}

func uitoa(v uint64) string {
	var b [20]byte
	i := len(b)
	if v == 0 {
		return "0"
	}
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

func TestDetachFreezesCursor(t *testing.T) {
	s, fn, sub, out := newWindowFixture(t)
	fx := &windowFixture{}
	s.AttachWindow(sub, "libera", "#c")
	fx.addLive(s, fn, "before")
	_ = flushLines(t, sub, out)
	s.AckWindow(sub, "libera", "#c", 1)

	s.DetachWindow(sub, "libera", "#c")
	fx.addLive(s, fn, "while-away-1")
	fx.addLive(s, fn, "while-away-2")
	if got := flushLines(t, sub, out); len(got) != 0 {
		t.Fatalf("detached window still delivered %v", got)
	}

	s.AttachWindow(sub, "libera", "#c")
	if !s.PlayBuffer(sub, "libera", "#c") {
		t.Fatal("PlayBuffer failed")
	}
	got := bodies(flushLines(t, sub, out))
	want := []string{"while-away-1", "while-away-2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("replay after re-attach = %v, want %v", got, want)
	}
}

func TestClearBufferHidesReplay(t *testing.T) {
	s, fn, sub, out := newWindowFixture(t)
	fx := &windowFixture{}
	s.AttachWindow(sub, "libera", "#c")
	for i := 0; i < 10; i++ {
		fx.addLive(s, fn, "old")
	}
	_ = flushLines(t, sub, out)

	ok, msg := s.ClearBuffer("libera", "#c")
	if !ok {
		t.Fatalf("ClearBuffer failed: %s", msg)
	}
	if !s.PlayBuffer(sub, "libera", "#c") {
		t.Fatal("PlayBuffer failed")
	}
	if got := flushLines(t, sub, out); len(got) != 0 {
		t.Fatalf("cleared buffer replayed %v", got)
	}

	fx.addLive(s, fn, "new")
	if got := bodies(flushLines(t, sub, out)); strings.Join(got, ",") != "new" {
		t.Fatalf("live after clear = %v, want only new", got)
	}

	if ok, msg := s.ClearBuffer("libera", "never-seen"); ok || msg == "" {
		t.Fatalf("ClearBuffer on unknown window = %v, %q; want no-op with message", ok, msg)
	}
}

func TestTwoClientsDifferentCursors(t *testing.T) {
	s, fn, subA, outA := newWindowFixture(t)
	fx := &windowFixture{}
	var outB bytes.Buffer
	subB := NewSubscriber(&outB, 64*1024)
	t.Cleanup(subB.Close)

	s.AttachWindow(subA, "libera", "#c")
	s.AttachWindow(subB, "libera", "#c")
	for _, body := range []string{"one", "two", "three", "four", "five", "six"} {
		fx.addLive(s, fn, body)
	}
	_ = flushLines(t, subA, outA)
	_ = flushLines(t, subB, &outB)

	s.AckWindow(subA, "libera", "#c", 2)
	s.AckWindow(subB, "libera", "#c", 4)

	if !s.PlayBuffer(subA, "libera", "#c") || !s.PlayBuffer(subB, "libera", "#c") {
		t.Fatal("PlayBuffer failed")
	}
	gotA := bodies(flushLines(t, subA, outA))
	gotB := bodies(flushLines(t, subB, &outB))
	if strings.Join(gotA, ",") != "three,four,five,six" {
		t.Fatalf("client A replay = %v", gotA)
	}
	if strings.Join(gotB, ",") != "five,six" {
		t.Fatalf("client B replay = %v", gotB)
	}
}
