package upstream

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iakat/salotto/internal/winring"
)

// TestOnOpenClosedOnTLSFailure proves the ident hook sees the TCP connection
// before the handshake and is unwound when the handshake fails: the pair must
// not stay registered.
func TestOnOpenClosedOnTLSFailure(t *testing.T) {
	ln := listen(t)
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, "this is not a TLS handshake\r\n")
	}()

	var opened atomic.Int64
	var closed atomic.Int64
	c, err := New(Config{
		Name:       "test",
		Addr:       ln.Addr().String(),
		TLS:        true,
		Nick:       "nick",
		Windows:    winring.New(8, 2, 512),
		Route:      routeStatus,
		BackoffMin: time.Millisecond,
		OnOpen: func(conn net.Conn) func() {
			opened.Add(1)
			var once atomic.Bool
			return func() {
				if once.CompareAndSwap(false, true) {
					closed.Add(1)
				}
			}
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := c.runOnce(ctx); err == nil {
		t.Fatal("runOnce succeeded against a non-TLS listener")
	}

	if opened.Load() != 1 {
		t.Fatalf("OnOpen called %d times, want 1", opened.Load())
	}
	if closed.Load() != 1 {
		t.Fatalf("onClose called %d times after TLS failure, want 1", closed.Load())
	}
}
