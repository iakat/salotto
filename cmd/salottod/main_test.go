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
	"github.com/iakat/salotto/internal/winring"
)

type fakeNet struct {
	name   string
	set    *winring.Set
	server string
	mask   string
	in     uint64
	out    uint64
}

func newFakeNet(name string) *fakeNet {
	return &fakeNet{name: name, set: winring.New(8, 4, 4096)}
}

func (f *fakeNet) Name() string      { return f.name }
func (f *fakeNet) Send([]byte) error { return nil }
func (f *fakeNet) State() string     { return "registered" }
func (f *fakeNet) Bytes() int        { return f.set.Bytes() }

func (f *fakeNet) Server() string { return f.server }

func (f *fakeNet) NickMask() string { return f.mask }

func (f *fakeNet) HasPassword() bool { return false }

func (f *fakeNet) Traffic() (in, out uint64) { return f.in, f.out }

func (f *fakeNet) SetConnectEnabled(bool) {}

func (f *fakeNet) Window(name string) (facade.WindowRing, bool) {
	r, ok := f.set.Ring(name)
	if !ok {
		return nil, false
	}
	return r, true
}

func (f *fakeNet) Windows() []string { return f.set.Windows() }

func newRouteFixture(t *testing.T) (*facade.Server, *fakeNet, *facade.Hub, *logfile.Writer, *bytes.Buffer, *facade.Subscriber) {
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
	return s, fn, hub, w, &out, sub
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
	s, _, _, w, out, sub := newRouteFixture(t)

	// The attached client is deliberately not in the hub: only the window path
	// (BroadcastWindow/BroadcastWindows) can reach it. If dispatchLine were the
	// old Broadcast-only code, this client would receive nothing.
	s.AttachWindow(sub, "probe", "#probe")

	w.Tracker().Observe([]byte(":probebot!p@h JOIN #probe"))
	line := []byte(":alice!a@h PRIVMSG #probe :hello one")
	dispatchEntries(s, "probe", logfile.Route(w.Tracker(), line, false, false, ""), line)

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
	s, _, hub, w, out, attached := newRouteFixture(t)
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
	dispatchEntries(s, "probe", logfile.Route(w.Tracker(), []byte(":alice!a@h PRIVMSG #probe :hello"), false, false, ""), []byte(":alice!a@h PRIVMSG #probe :hello"))

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
	s, _, hub, w, out, attached := newRouteFixture(t)
	s.AttachWindow(attached, "probe", "#probe")

	var plainOut bytes.Buffer
	plain := facade.NewSubscriber(&plainOut, 64*1024)
	t.Cleanup(plain.Close)

	if !hub.Add(attached) || !hub.Add(plain) {
		t.Fatal("hub.Add failed")
	}

	dispatchEntries(s, "probe", logfile.Route(w.Tracker(), []byte(":srv 254 probebot 0 :channels formed"), false, false, ""), []byte(":srv 254 probebot 0 :channels formed"))

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

// routeCase is one routing scenario: a client in the hub (so the status
// catch-all can reach it) plus the ring and tracker the daemon drives.
type routeCase struct {
	s   *facade.Server
	fn  *fakeNet
	w   *logfile.Writer
	sub *facade.Subscriber
	out *bytes.Buffer
}

func newRouteCase(t *testing.T) *routeCase {
	s, fn, hub, w, out, sub := newRouteFixture(t)
	if !hub.Add(sub) {
		t.Fatal("hub.Add failed")
	}
	w.Tracker().Observe([]byte(":probebot!p@h JOIN #probe"))
	w.Tracker().Observe([]byte(":probebot!p@h JOIN #other"))
	return &routeCase{s: s, fn: fn, w: w, sub: sub, out: out}
}

func (fx *routeCase) send(t *testing.T, channel, body string) uint64 {
	line := []byte(":alice!a@h PRIVMSG " + channel + " :" + body)
	seq := fx.fn.set.Append(channel, line, time.Now().UnixNano())
	dispatchEntries(fx.s, "probe", logfile.Route(fx.w.Tracker(), line, false, false, ""), line)
	return seq
}

func (fx *routeCase) flush(t *testing.T) []string {
	return flushLines(t, fx.sub, fx.out)
}

// countBody counts delivered lines whose trailing body is exactly marker.
func countBody(lines []string, marker string) int {
	n := 0
	for _, line := range lines {
		if strings.Contains(line, "PRIVMSG ") && strings.HasSuffix(line, " :"+marker) {
			n++
		}
	}
	return n
}

// TestWindowRoutingStates pins the three-state rule from windows.go:
// attached = live once, detached = no live but PlayBuffer replays, status (no
// opinion) = live once through the catch-all. Mute behaves like detached.
func TestWindowRoutingStates(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, fx *routeCase)
		run   func(t *testing.T, fx *routeCase)
	}{
		{
			name: "attached: one live line",
			setup: func(t *testing.T, fx *routeCase) {
				fx.s.AttachWindow(fx.sub, "probe", "#probe")
			},
			run: func(t *testing.T, fx *routeCase) {
				fx.send(t, "#probe", "attached")
				if got := countBody(fx.flush(t), "attached"); got != 1 {
					t.Fatalf("attached live count = %d, want 1", got)
				}
			},
		},
		{
			name: "detached: no live, PlayBuffer replays once with batch",
			setup: func(t *testing.T, fx *routeCase) {
				fx.s.AttachWindow(fx.sub, "probe", "#probe")
				fx.s.DetachWindow(fx.sub, "probe", "#probe")
			},
			run: func(t *testing.T, fx *routeCase) {
				fx.send(t, "#probe", "missed")
				if got := countBody(fx.flush(t), "missed"); got != 0 {
					t.Fatalf("detached live count = %d, want 0", got)
				}
				if !fx.s.PlayBuffer(fx.sub, "probe", "#probe") {
					t.Fatal("PlayBuffer failed")
				}
				replay := fx.flush(t)
				if got := countBody(replay, "missed"); got != 1 {
					t.Fatalf("replay count = %d, want 1: %q", got, replay)
				}
				if !strings.Contains(strings.Join(replay, "\n"), "batch=probe-#probe-replay") {
					t.Fatalf("replay lacks the batch tag: %q", replay)
				}
			},
		},
		{
			name: "detached window stays silent while another is attached",
			setup: func(t *testing.T, fx *routeCase) {
				fx.s.AttachWindow(fx.sub, "probe", "#other")
				fx.s.AttachWindow(fx.sub, "probe", "#probe")
				fx.s.DetachWindow(fx.sub, "probe", "#probe")
			},
			run: func(t *testing.T, fx *routeCase) {
				fx.send(t, "#other", "other-live")
				fx.send(t, "#probe", "detached-live")
				got := fx.flush(t)
				if n := countBody(got, "other-live"); n != 1 {
					t.Fatalf("attached window live count = %d, want 1: %q", n, got)
				}
				if n := countBody(got, "detached-live"); n != 0 {
					t.Fatalf("detached window live count = %d, want 0: %q", n, got)
				}
			},
		},
		{
			name:  "status catch-all: one live line",
			setup: func(t *testing.T, fx *routeCase) {},
			run: func(t *testing.T, fx *routeCase) {
				fx.send(t, "#probe", "catchall")
				if got := countBody(fx.flush(t), "catchall"); got != 1 {
					t.Fatalf("catch-all live count = %d, want 1", got)
				}
			},
		},
		{
			name:  "re-attach resumes live and does not re-replay",
			setup: func(t *testing.T, fx *routeCase) {},
			run: func(t *testing.T, fx *routeCase) {
				fx.s.AttachWindow(fx.sub, "probe", "#probe")
				seq := fx.send(t, "#probe", "before-detach")
				if got := countBody(fx.flush(t), "before-detach"); got != 1 {
					t.Fatalf("pre-detach live count = %d, want 1", got)
				}
				fx.s.AckWindow(fx.sub, "probe", "#probe", seq)

				fx.s.DetachWindow(fx.sub, "probe", "#probe")
				fx.send(t, "#probe", "while-detached")
				if got := countBody(fx.flush(t), "while-detached"); got != 0 {
					t.Fatalf("detached live count = %d, want 0", got)
				}
				if !fx.s.PlayBuffer(fx.sub, "probe", "#probe") {
					t.Fatal("PlayBuffer failed")
				}
				replay := fx.flush(t)
				if got := countBody(replay, "while-detached"); got != 1 {
					t.Fatalf("first replay count = %d, want 1", got)
				}
				if got := countBody(replay, "before-detach"); got != 0 {
					t.Fatalf("acked live line replayed %d times, want 0", got)
				}

				fx.s.AttachWindow(fx.sub, "probe", "#probe")
				seq = fx.send(t, "#probe", "after-reattach")
				if got := countBody(fx.flush(t), "after-reattach"); got != 1 {
					t.Fatalf("post-reattach live count = %d, want 1", got)
				}
				fx.s.AckWindow(fx.sub, "probe", "#probe", seq)
				if !fx.s.PlayBuffer(fx.sub, "probe", "#probe") {
					t.Fatal("PlayBuffer failed")
				}
				if got := countBody(fx.flush(t), "while-detached"); got != 0 {
					t.Fatalf("PlayBuffer re-delivered an already replayed line %d times", got)
				}
			},
		},
		{
			name: "mute: no live, PlayBuffer still replays",
			setup: func(t *testing.T, fx *routeCase) {
				fx.s.AttachWindow(fx.sub, "probe", "#probe")
				fx.s.MuteWindow(fx.sub, "probe", "#probe", true)
			},
			run: func(t *testing.T, fx *routeCase) {
				fx.send(t, "#probe", "muted")
				if got := countBody(fx.flush(t), "muted"); got != 0 {
					t.Fatalf("muted live count = %d, want 0", got)
				}
				if !fx.s.PlayBuffer(fx.sub, "probe", "#probe") {
					t.Fatal("PlayBuffer failed")
				}
				if got := countBody(fx.flush(t), "muted"); got != 1 {
					t.Fatalf("muted replay count = %d, want 1", got)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newRouteCase(t)
			tt.setup(t, fx)
			tt.run(t, fx)
		})
	}
}
