package upstream

import (
	"bufio"
	"context"
	"testing"
	"time"

	"github.com/iakat/salotto/internal/ring"
)

// TestReconnect closes the server side of a registered connection and asserts
// that the client dials again, re-registers, and that the synthetic reconnect
// notice reaches the ring.
func TestReconnect(t *testing.T) {
	ln := listen(t)
	defer ln.Close()

	r := ring.New(8, 4096)
	cfg := Config{
		Name:       "test",
		Addr:       ln.Addr().String(),
		Nick:       "nick",
		Ring:       r,
		BackoffMin: 10 * time.Millisecond,
		BackoffMax: 20 * time.Millisecond,
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx)
	}()

	first := accept(t, ln)
	br := bufio.NewReader(first)
	serverRegister(t, first, br, "multi-prefix", "", "nick", "USER nick 0 * :")
	first.Close()

	second := accept(t, ln)
	defer second.Close()

	// The notice is appended at the start of the new connection, so it is in
	// the ring by the time the second dial is accepted.
	waitFor(t, 3*time.Second, func() bool {
		return ringContains(r, "reconnecting")
	}, "reconnecting notice in the ring")

	if got := c.Attempts(); got < 2 {
		t.Fatalf("Attempts = %d, want >= 2", got)
	}

	// Completing registration on the second connection proves the reconnect is
	// a full handshake, not just a dial.
	br2 := bufio.NewReader(second)
	serverRegister(t, second, br2, "multi-prefix", "", "nick", "USER nick 0 * :")
	waitFor(t, 2*time.Second, func() bool { return c.State() == StateRegistered }, "re-registered")

	cancel()
	<-done
	if got := c.State(); got != StateStopped {
		t.Fatalf("State = %s after cancel, want stopped", got)
	}
}
