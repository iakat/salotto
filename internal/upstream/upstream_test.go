package upstream

import (
	"bufio"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/iakat/salotto/internal/ring"
)

func TestRegistration(t *testing.T) {
	ln := listen(t)
	defer ln.Close()

	r := ring.New(4, 4096)
	cfg := Config{
		Name:       "test",
		Addr:       ln.Addr().String(),
		Nick:       "nick",
		User:       "user",
		RealName:   "Real Name",
		Channels:   []string{"#chan"},
		Ring:       r,
		BackoffMin: 10 * time.Millisecond,
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go c.Run(ctx)

	conn := accept(t, ln)
	defer conn.Close()
	br := bufio.NewReader(conn)

	serverRegister(t, conn, br,
		"server-time message-tags echo-message",
		"server-time message-tags echo-message",
		"nick", "USER user 0 * :Real Name")
	writeServerLines(t, conn, ":srv 005 nick CHANTYPES=# :supported")
	expectLine(t, conn, br, "JOIN #chan")
}

func TestPing(t *testing.T) {
	ln := listen(t)
	defer ln.Close()

	r := ring.New(4, 4096)
	cfg := Config{
		Name:       "test",
		Addr:       ln.Addr().String(),
		Nick:       "nick",
		Ring:       r,
		BackoffMin: 10 * time.Millisecond,
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go c.Run(ctx)

	conn := accept(t, ln)
	defer conn.Close()
	br := bufio.NewReader(conn)

	serverRegister(t, conn, br, "multi-prefix", "", "nick", "USER nick 0 * :")
	writeServerLines(t, conn, "PING :abc")

	got := readClientLine(t, conn, br)
	if got != "PONG :abc" && got != "PONG abc" {
		t.Fatalf("client sent %q, want a PONG for :abc", got)
	}

	if c.LinesIn() != 0 {
		t.Fatalf("LinesIn = %d, want 0 (PING must not be stored)", c.LinesIn())
	}
	if r.Newest() != 0 {
		t.Fatalf("ring newest = %d, want 0; lines: %v", r.Newest(), ringLines(r))
	}
	for _, line := range ringLines(r) {
		if strings.Contains(line, "PING") {
			t.Fatalf("ring contains PING: %q", line)
		}
	}
}

func TestLineStorage(t *testing.T) {
	ln := listen(t)
	defer ln.Close()

	r := ring.New(8, 4096)
	stored := make(chan uint64, 8)
	cfg := Config{
		Name:       "test",
		Addr:       ln.Addr().String(),
		Nick:       "nick",
		Ring:       r,
		BackoffMin: 10 * time.Millisecond,
		OnLine: func(seq uint64, line []byte) {
			stored <- seq
		},
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go c.Run(ctx)

	conn := accept(t, ln)
	defer conn.Close()
	br := bufio.NewReader(conn)

	serverRegister(t, conn, br, "multi-prefix", "", "nick", "USER nick 0 * :")

	msgs := []string{
		":alice!a@h PRIVMSG #chan :one",
		":bob!b@h PRIVMSG #chan :two",
		":carol!c@h PRIVMSG #chan :three",
	}
	writeServerLines(t, conn, msgs...)

	for i := 0; i < len(msgs); i++ {
		select {
		case <-stored:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for stored line %d", i)
		}
	}

	if r.Newest() != uint64(len(msgs)) {
		t.Fatalf("ring newest = %d, want %d", r.Newest(), len(msgs))
	}
	got := ringLines(r)
	if len(got) != len(msgs) {
		t.Fatalf("replayed %d lines, want %d", len(got), len(msgs))
	}
	for i := range msgs {
		if got[i] != msgs[i] {
			t.Fatalf("line %d = %q, want %q", i, got[i], msgs[i])
		}
	}
}
