package main

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/iakat/salotto/internal/facade"
	"github.com/iakat/salotto/internal/logfile"
	"github.com/iakat/salotto/internal/ring"
)

type fakeNet struct {
	name string
	ring *ring.Ring
}

func newFakeNet(name string) *fakeNet {
	return &fakeNet{name: name, ring: ring.New(4, 4096)}
}

func (f *fakeNet) Name() string                 { return f.name }
func (f *fakeNet) Send([]byte) error            { return nil }
func (f *fakeNet) State() string                { return "registered" }
func (f *fakeNet) Newest() uint64               { return f.ring.Newest() }
func (f *fakeNet) Oldest() uint64               { return f.ring.Oldest() }
func (f *fakeNet) Bytes() int                   { return f.ring.Bytes() }
func (f *fakeNet) Stamp(s uint64) (int64, bool) { return f.ring.Stamp(s) }
func (f *fakeNet) Replay(from uint64, emit func(uint64, []byte) bool) uint64 {
	return f.ring.Replay(from, emit)
}

func newRouteFixture(t *testing.T) (*facade.Server, *facade.Hub, *logfile.Writer, *bytes.Buffer, *facade.Subscriber) {
	t.Helper()
	fn := newFakeNet("probe")
	n := facade.Network(fn)
	hub := facade.NewHub(8)
	s, err := facade.New(facade.Config{
		ServerName: "salotto",
		Network:    "ProbeNet",
		Networks:   map[string]*facade.Network{"probe": &n},
		Hub:        hub,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxClients: 8,
	})
	if err != nil {
		t.Fatalf("facade.New: %v", err)
	}
	w := logfile.New("probe", "", 0, time.Now)
	w.SetNick("probebot")
	t.Cleanup(w.Close)

	var out bytes.Buffer
	sub := facade.NewSubscriber(&out, 64*1024)
	t.Cleanup(sub.Close)
	return s, hub, w, &out, sub
}

func flushLines(t *testing.T, sub *facade.Subscriber, out *bytes.Buffer) []string {
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

func TestDispatchLineUsesWindowPath(t *testing.T) {
	s, _, w, out, sub := newRouteFixture(t)

	// The attached client is deliberately not in the hub: only the window path
	// (BroadcastWindow/BroadcastWindows) can reach it. If dispatchLine were the
	// old Broadcast-only code, this client would receive nothing.
	s.AttachWindow(sub, "probe", "#probe")

	w.Tracker().Observe([]byte(":probebot!p@h JOIN #probe"))
	line := []byte(":alice!a@h PRIVMSG #probe :hello one")
	dispatchLine(s, w.Tracker(), false, "probe", 5, line, false)

	got := flushLines(t, sub, out)
	if len(got) != 1 {
		t.Fatalf("attached client got %d lines, want 1: %q", len(got), got)
	}
	if !strings.Contains(got[0], "PRIVMSG #probe :hello one") {
		t.Fatalf("attached client got %q, want the channel line", got[0])
	}
	if !s.Known("probe", "#probe") {
		t.Fatal("windowState does not know #probe: BroadcastWindow was not called")
	}
}

func TestDispatchLineNoDuplication(t *testing.T) {
	s, hub, w, out, attached := newRouteFixture(t)
	s.AttachWindow(attached, "probe", "#probe")

	var statusOut bytes.Buffer
	unattached := facade.NewSubscriber(&statusOut, 64*1024)
	t.Cleanup(unattached.Close)

	// A real client is both in the hub and window-attached. The hub is the
	// catch-all the status fallback walks; the window path must not reach it
	// twice through Broadcast.
	if !hub.Add(attached) || !hub.Add(unattached) {
		t.Fatal("hub.Add failed")
	}

	w.Tracker().Observe([]byte(":probebot!p@h JOIN #probe"))
	dispatchLine(s, w.Tracker(), false, "probe", 6, []byte(":alice!a@h PRIVMSG #probe :hello"), false)

	gotAttached := flushLines(t, attached, out)
	if len(gotAttached) != 1 || !strings.Contains(gotAttached[0], "PRIVMSG #probe :hello") {
		t.Fatalf("attached client got %d lines, want exactly one: %q", len(gotAttached), gotAttached)
	}
	gotUnattached := flushLines(t, unattached, &statusOut)
	if len(gotUnattached) != 1 || !strings.Contains(gotUnattached[0], "PRIVMSG #probe :hello") {
		t.Fatalf("unattached client got %d lines, want exactly one via status: %q", len(gotUnattached), gotUnattached)
	}
}

func TestDispatchLineStatusGoesToEveryoneOnce(t *testing.T) {
	s, hub, w, out, attached := newRouteFixture(t)
	s.AttachWindow(attached, "probe", "#probe")

	var plainOut bytes.Buffer
	plain := facade.NewSubscriber(&plainOut, 64*1024)
	t.Cleanup(plain.Close)

	if !hub.Add(attached) || !hub.Add(plain) {
		t.Fatal("hub.Add failed")
	}

	dispatchLine(s, w.Tracker(), false, "probe", 7, []byte(":srv 254 probebot 0 :channels formed"), false)

	for name, sub := range map[string]*facade.Subscriber{"attached": attached, "plain": plain} {
		buf := out
		if name == "plain" {
			buf = &plainOut
		}
		got := flushLines(t, sub, buf)
		if len(got) != 1 || !strings.Contains(got[0], " 254 ") {
			t.Fatalf("%s client got %d status lines, want exactly one: %q", name, len(got), got)
		}
	}
}
