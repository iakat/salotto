package upstream

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iakat/salotto/internal/logfile"
	"github.com/iakat/salotto/internal/winring"
)

// A CONNECT must reconnect at once, not after the backoff ladder.
//
// Measured on the deployed bouncer: Disconnect/Connect pairs took ~81 s to bring the
// upstream back, which is longer than BackoffMax and therefore cannot be the ladder
// alone — the poke was being lost. The chansaver parity case then failed on a budget
// it should never have needed, and the feature looked broken while it worked.
//
// This test drives the real Run loop against a real listener and asserts the second
// connection arrives promptly after SetConnectEnabled(true).
func TestConnectPokesTheRunLoopAwake(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Count accepted connections so the test can wait for a real dial, not a timer.
	var accepted atomic.Int64
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			go func() {
				// Hold the connection open: a bouncer's upstream does not close on
				// its own, and an immediate EOF would reconnect for the wrong reason.
				buf := make([]byte, 4096)
				for {
					if _, err := conn.Read(buf); err != nil {
						conn.Close()
						return
					}
				}
			}()
		}
	}()

	cfg := Config{
		Name:    "test",
		Windows: winring.New(8, 4, 4096),
		Route: func(line []byte, outbound bool) []logfile.Entry {
			return nil
		},
		Addr: ln.Addr().String(),
		Nick: "tester",
		// A long ceiling on purpose: if the poke is lost, the reconnect waits this
		// long and the test fails on its deadline rather than a timing coincidence.
		BackoffMin: 20 * time.Millisecond,
		BackoffMax: 30 * time.Second,
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	// Wait for the first connection.
	deadline := time.Now().Add(10 * time.Second)
	for accepted.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if accepted.Load() == 0 {
		t.Fatal("never connected the first time")
	}

	// Disconnect, let the loop settle into its backoff wait, then connect again.
	c.SetConnectEnabled(false)
	time.Sleep(150 * time.Millisecond)
	before := accepted.Load()

	start := time.Now()
	c.SetConnectEnabled(true)
	deadline = time.Now().Add(10 * time.Second)
	for accepted.Load() == before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	elapsed := time.Since(start)

	if accepted.Load() == before {
		t.Fatal("CONNECT never produced a new connection within 10s")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("CONNECT took %v to reconnect; it must poke the loop awake rather "+
			"than wait out the backoff", elapsed)
	}
}
