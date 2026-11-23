package upstream

import (
	"bufio"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/iakat/salotto/internal/winring"
)

// TestSendBeforeRun asserts Send refuses while the connection has never
// registered.
func TestSendBeforeRun(t *testing.T) {
	c, err := New(Config{
		Name:    "test",
		Addr:    "127.0.0.1:1",
		Nick:    "nick",
		Windows: winring.New(8, 4, 4096),
		Route:   routeStatus,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := c.Send([]byte("PRIVMSG #chan :hi")); !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("Send = %v, want ErrNotRegistered", err)
	}
}

// TestSendWhileBackoff asserts Send refuses while the client is between
// connections.
func TestSendWhileBackoff(t *testing.T) {
	ln := listen(t)
	defer ln.Close()

	c, err := New(Config{
		Name:       "test",
		Addr:       ln.Addr().String(),
		Nick:       "nick",
		Windows:    winring.New(8, 4, 4096),
		Route:      routeStatus,
		BackoffMin: 50 * time.Millisecond,
		BackoffMax: 50 * time.Millisecond,
	})
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
	waitFor(t, 2*time.Second, func() bool { return c.State() == StateRegistered }, "registered")

	first.Close()
	waitFor(t, 2*time.Second, func() bool { return c.State() != StateRegistered }, "left registered state")

	if err := c.Send([]byte("PRIVMSG #chan :hi")); !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("Send = %v, want ErrNotRegistered while down", err)
	}

	cancel()
	<-done
}
