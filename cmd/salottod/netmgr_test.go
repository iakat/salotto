package main

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/iakat/salotto/internal/bindhost"
	"github.com/iakat/salotto/internal/config"
	"github.com/iakat/salotto/internal/facade"
	"github.com/iakat/salotto/internal/ident"
)

// fakeUpstream is a minimal IRC server: it accepts, answers the handshake and
// then idles, which is all netmgr.Start needs to reach "registered".
func fakeUpstream(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				br := bufio.NewReader(conn)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					switch {
					case strings.HasPrefix(line, "NICK "):
					case strings.HasPrefix(line, "USER "):
					case strings.HasPrefix(line, "CAP LS"):
						_, _ = io.WriteString(conn, ":srv CAP * LS :multi-prefix\r\n")
					default:
						// Anything else (CAP END, JOIN, PING) is answered with
						// the 001 that completes registration, once.
						_, _ = io.WriteString(conn, ":srv 001 nick :welcome\r\n")
					}
				}
			}()
		}
	}()
	return ln
}

func newTestNetmgr(t *testing.T) (*netmgr, *facade.Server) {
	t.Helper()
	cfg := &config.Config{
		ServerName:      "salotto",
		RingSegments:    4,
		RingSegmentSize: 4096,
		MaxWindows:      8,
	}
	srv, err := facade.New(facade.Config{ServerName: "salotto", Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("facade.New: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reg := ident.NewRegistry()
	m := newNetmgr(cfg, srv, log, reg, bindhost.New("", false), ctx)
	return m, srv
}

// TestStartStopLeavesNoGoroutine is the B5 lifecycle guarantee: a network that
// is started and then stopped must leave nothing dialling behind, and the
// facade must forget it.
func TestStartStopLeavesNoGoroutine(t *testing.T) {
	ln := fakeUpstream(t)
	m, srv := newTestNetmgr(t)

	// One sample after the fake server's own goroutines exist, so the baseline
	// does not count them as leaked.
	time.Sleep(20 * time.Millisecond)
	base := runtime.NumGoroutine()

	for i := 0; i < 3; i++ {
		spec := config.Network{
			Name: "rt", Addr: ln.Addr().String(), Nick: "nick", RealName: "nick",
		}
		if err := m.Start(spec); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if got := srv.Networks(); len(got) != 1 || got[0] != "rt" {
			t.Fatalf("after Start Networks = %v, want [rt]", got)
		}
		if err := m.Start(spec); err == nil {
			t.Fatal("starting the same network twice succeeded")
		}
		// Give the dial loop a moment to actually connect, so Stop has something
		// to cancel rather than a no-op.
		time.Sleep(30 * time.Millisecond)

		if err := m.Stop("rt"); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if got := srv.Networks(); len(got) != 0 {
			t.Fatalf("after Stop Networks = %v, want empty", got)
		}
		if err := m.Stop("rt"); err == nil {
			t.Fatal("stopping an unknown network succeeded")
		}
	}

	// The dial loop exits on ctx; give it the grace period the shutdown path
	// gives it, then assert the goroutine count came back.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= base {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("goroutines after Stop = %d, baseline %d", runtime.NumGoroutine(), base)
}

// TestStatsReflectsRuntimeNetworks is the /stats half of B5: the handler reads
// the manager, so a network added after boot shows up and a stopped one does
// not.
func TestStatsReflectsRuntimeNetworks(t *testing.T) {
	ln := fakeUpstream(t)
	m, srv := newTestNetmgr(t)

	if err := m.Start(config.Network{Name: "rt", Addr: ln.Addr().String(), Nick: "nick"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = m.Stop("rt") }()

	names := m.Names()
	if len(names) != 1 || names[0] != "rt" {
		t.Fatalf("Names = %v, want [rt]", names)
	}
	snap := m.Snapshot()
	if len(snap) != 1 || snap[0].name != "rt" {
		t.Fatalf("Snapshot = %v, want one runtime named rt", snap)
	}
	if got := srv.Networks(); len(got) != 1 {
		t.Fatalf("facade Networks = %v, want the runtime network", got)
	}
}
