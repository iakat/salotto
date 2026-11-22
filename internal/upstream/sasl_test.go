package upstream

import (
	"bufio"
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/iakat/salotto/internal/ring"
)

// TestSASLPlain asserts the client does SASL PLAIN with the user as both
// authzid and authcid, and continues registration only after a 903.
func TestSASLPlain(t *testing.T) {
	ln := listen(t)
	defer ln.Close()

	c, err := New(Config{
		Name:       "test",
		Addr:       ln.Addr().String(),
		Nick:       "nick",
		SASLUser:   "user",
		SASLPass:   "pass",
		Ring:       ring.New(4, 4096),
		BackoffMin: 10 * time.Millisecond,
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

	conn := accept(t, ln)
	defer conn.Close()
	br := bufio.NewReader(conn)

	expectLine(t, conn, br, "CAP LS 302")
	writeServerLines(t, conn, ":srv CAP * LS :sasl=PLAIN server-time")
	expectLine(t, conn, br, "CAP REQ :server-time sasl")
	writeServerLines(t, conn, ":srv CAP * ACK :server-time sasl")
	expectLine(t, conn, br, "AUTHENTICATE PLAIN")
	writeServerLines(t, conn, "AUTHENTICATE +")

	line := readClientLine(t, conn, br)
	const prefix = "AUTHENTICATE "
	if !strings.HasPrefix(line, prefix) {
		t.Fatalf("client sent %q, want an AUTHENTICATE payload", line)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, prefix))
	if err != nil {
		t.Fatalf("decode SASL payload: %v", err)
	}
	if got, want := string(raw), "user\x00user\x00pass"; got != want {
		t.Fatalf("SASL payload = %q, want %q", got, want)
	}

	writeServerLines(t, conn, ":srv 903 nick :SASL authentication successful")
	expectLine(t, conn, br, "CAP END")
	expectLine(t, conn, br, "NICK nick")
	expectLine(t, conn, br, "USER nick 0 * :")
	writeServerLines(t, conn, ":srv 001 nick :Welcome")

	waitFor(t, 2*time.Second, func() bool { return c.State() == StateRegistered }, "registered after SASL")
}

// TestSASLNotRequestedWithoutCredentials asserts sasl is never requested when
// no SASL user is configured, even when the server offers it.
func TestSASLNotRequestedWithoutCredentials(t *testing.T) {
	ln := listen(t)
	defer ln.Close()

	c, err := New(Config{
		Name:       "test",
		Addr:       ln.Addr().String(),
		Nick:       "nick",
		Ring:       ring.New(4, 4096),
		BackoffMin: 10 * time.Millisecond,
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

	conn := accept(t, ln)
	defer conn.Close()
	br := bufio.NewReader(conn)

	expectLine(t, conn, br, "CAP LS 302")
	writeServerLines(t, conn, ":srv CAP * LS :sasl=PLAIN server-time")
	expectLine(t, conn, br, "CAP REQ :server-time")
	writeServerLines(t, conn, ":srv CAP * ACK :server-time")
	expectLine(t, conn, br, "CAP END")
}
