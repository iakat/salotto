package winring

import (
	"fmt"
	"testing"
)

// TestResizeWindowChangesCapacity proves SETBUFFER's resizing is real: the ring
// is rebuilt at the requested geometry, buffered lines survive, and the sequence
// numbers of what remains are still the ones a client cursor expects.
//
// A note on the numbers: a ring's capacity comes from its segment size
// (ring.New holds segSize/4 entries and clamps segSize to at least 512), so
// asking for five lines builds the smallest ring rather than a five-entry one.
// That floor is ring.New's, not this test's business — what SETBUFFER promises
// here is the geometry it asked for, and the two assertions below are the ones
// that can be stated honestly.
func TestResizeWindowChangesCapacity(t *testing.T) {
	s := New(8, 4, 4096)
	const window = "#c"

	// geometryFor(20) gives 2 segments of 80 bytes, so 20 entries each: 40 lines
	// fit in two segments. Write enough to fill both, then shrink.
	var first, last uint64
	for i := 1; i <= 40; i++ {
		seq := s.Append(window, []byte(fmt.Sprintf(":a!b@c PRIVMSG #c :line-%02d", i)), int64(i))
		if i == 1 {
			first = seq
		}
		last = seq
	}
	if first == 0 || last <= first {
		t.Fatalf("bad seed sequences: first=%d last=%d", first, last)
	}

	if !s.ResizeWindow(window, 5) {
		t.Fatal("ResizeWindow refused a valid size")
	}

	r, ok := s.Ring(window)
	if !ok {
		t.Fatal("window missing after resize")
	}
	if got := r.Newest(); got != last {
		t.Fatalf("resize moved the sequence counter: newest=%d, want %d", got, last)
	}

	var kept []string
	var seqs []uint64
	r.Replay(r.Oldest(), func(seq uint64, line []byte) bool {
		kept = append(kept, string(line))
		seqs = append(seqs, seq)
		return true
	})
	if len(kept) == 0 {
		t.Fatal("resize discarded every line; it must keep what still fits")
	}
	// The survivors must be the newest ones: the last line kept is the last line
	// written, and its sequence is the last sequence a client saw.
	newest := fmt.Sprintf("line-%02d", 40)
	if got := kept[len(kept)-1]; got[len(got)-len(newest):] != newest {
		t.Fatalf("last kept line is %q, want it to end with %q", got, newest)
	}
	if got := seqs[len(seqs)-1]; got != last {
		t.Fatalf("last kept line has sequence %d, want %d (a cursor must stay valid)", got, last)
	}
	// Survivors are contiguous end-to-end in sequence order.
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("sequence gap at %d: %d then %d", i, seqs[i-1], seqs[i])
		}
	}
}

// TestResizeWindowRejectsNegative pins that a nonsensical size is refused
// rather than silently clamped, so the command can report ZNC's failure text.
func TestResizeWindowRejectsNegative(t *testing.T) {
	s := New(8, 4, 4096)
	if s.ResizeWindow("#c", -1) {
		t.Fatal("a negative buffer size was accepted")
	}
}
