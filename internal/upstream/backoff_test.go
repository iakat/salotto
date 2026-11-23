package upstream

import (
	"context"
	"testing"
	"time"
)

// A poke means "connect now", and it must also reset the delay. The bug this
// guards: sleepBackoff returned early on a poke but the caller still doubled the
// backoff, so every manual DISCONNECT/CONNECT pair made the next reconnect slower
// than the last. The deployed daemon reached attempt 10 with gaps of minutes, and
// CONNECT looked broken when it was actually the backoff ladder.
//
// Measured on the deployed salotto: after a handful of parity runs, reconnects
// were minutes apart, while ZNC reconnected at once (CIRCNetwork restarts the
// delay on an explicit connect).
func TestPokeResetsTheBackoffLadder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poke := make(chan struct{}, 1)
	min := 20 * time.Millisecond

	// First: a poke must report itself, so the caller can tell it from the timer.
	poke <- struct{}{}
	start := time.Now()
	poked, ok := sleepBackoff(ctx, poke, time.Hour)
	if !poked || !ok {
		t.Fatalf("poked = %v, ok = %v; want true, true", poked, ok)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the poke waited %v; it must return at once", elapsed)
	}

	// Second: the timer expiring is not a poke, and must keep the ladder growing.
	poked, ok = sleepBackoff(ctx, poke, min)
	if poked || !ok {
		t.Fatalf("poked = %v, ok = %v; a timeout is not a poke", poked, ok)
	}

	// Third: cancellation is neither, and means stop.
	cancel()
	poked, ok = sleepBackoff(ctx, poke, time.Hour)
	if poked || ok {
		t.Fatalf("poked = %v, ok = %v; cancellation must return ok=false", poked, ok)
	}
}

// The ladder itself: an uninterrupted failure sequence doubles up to the cap, and
// a poke drops it back to the floor. This is the arithmetic that Run performs, kept
// next to the sleep so the two cannot drift.
func TestBackoffLadderDoublesButAPokeResetsIt(t *testing.T) {
	min, max := 10*time.Millisecond, 40*time.Millisecond

	cur := min
	for _, want := range []time.Duration{20 * time.Millisecond, 40 * time.Millisecond, 40 * time.Millisecond} {
		cur = nextBackoff(cur, false, min, max)
		if cur != want {
			t.Fatalf("after an ordinary retry the delay is %v, want %v", cur, want)
		}
	}
	if got := nextBackoff(cur, true, min, max); got != min {
		t.Fatalf("after a poke the delay is %v, want the floor %v", got, min)
	}
}
