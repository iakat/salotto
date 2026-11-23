package winring

import (
	"strings"
	"sync"
	"testing"
)

func replay(r *Ring) []string {
	var out []string
	r.Replay(r.Oldest(), func(seq uint64, line []byte) bool {
		out = append(out, string(line))
		return true
	})
	return out
}

// A busy window must not evict a quiet window's lines.
func TestQuietWindowSurvivesBusyWindow(t *testing.T) {
	s := New(4, 8, 512)
	for i := 0; i < 3; i++ {
		s.Append("#quiet", []byte("quiet-"+string(rune('a'+i))), 0)
	}
	for i := 0; i < 1000; i++ {
		s.Append("#busy", []byte("busy"), 0)
	}

	r, ok := s.Ring("#quiet")
	if !ok {
		t.Fatal("quiet window missing")
	}
	got := replay(r)
	if len(got) != 3 {
		t.Fatalf("quiet replay = %v, want 3 lines", got)
	}
	if !strings.HasPrefix(got[0], "quiet-a") {
		t.Fatalf("quiet replay = %v, want the quiet lines", got)
	}
}

// A window that genuinely overflows must report the exact lost range.
func TestOverflowGapRange(t *testing.T) {
	s := New(2, 8, 512)
	for i := 0; i < 1000; i++ {
		s.Append("#c", []byte("line"), 0)
	}
	r, ok := s.Ring("#c")
	if !ok {
		t.Fatal("window missing")
	}
	oldest := r.Oldest()
	if oldest <= 1 {
		t.Fatalf("oldest = %d, want > 1", oldest)
	}
	missing := r.Replay(1, func(seq uint64, line []byte) bool { return true })
	if want := oldest - 1; missing != want {
		t.Fatalf("missing = %d, want %d", missing, want)
	}
}

// ClearBuffer physically drops the lines, and the sequence stays monotonic.
func TestClearDropsPhysically(t *testing.T) {
	s := New(4, 8, 512)
	seq := s.Append("#c", []byte("old"), 0)
	for i := 0; i < 5; i++ {
		seq = s.Append("#c", []byte("old"), 0)
	}
	s.Append("#other", []byte("keep"), 0)

	s.Clear("#c")
	r, ok := s.Ring("#c")
	if !ok {
		t.Fatal("window missing after clear")
	}
	if got := r.Bytes(); got != 0 {
		t.Fatalf("bytes after clear = %d, want 0", got)
	}
	if got := replay(r); len(got) != 0 {
		t.Fatalf("replay after clear = %v, want nothing", got)
	}
	if got := r.Newest(); got != 0 {
		t.Fatalf("newest after clear = %d, want 0 (empty)", got)
	}
	if got := r.Oldest(); got != 0 {
		t.Fatalf("oldest after clear = %d, want 0 (empty)", got)
	}

	next := s.Append("#c", []byte("new"), 0)
	if next <= seq {
		t.Fatalf("seq after clear = %d, want > %d", next, seq)
	}
	if got := replay(r); len(got) != 1 || string(got[0]) != "new" {
		t.Fatalf("replay after clear+append = %v, want [new]", got)
	}

	or, _ := s.Ring("#other")
	if got := replay(or); len(got) != 1 || string(got[0]) != "keep" {
		t.Fatalf("other window after clear = %v, want [keep]", got)
	}
}

// The same line in two windows has independent sequences and cursors.
func TestIndependentWindowSequences(t *testing.T) {
	s := New(4, 8, 512)
	s.Append("#a", []byte("only-a"), 0)
	line := []byte("shared")
	seqA := s.Append("#a", line, 0)
	seqB := s.Append("#b", line, 0)
	s.Append("#b", []byte("tail-b"), 0)

	if seqA == seqB {
		t.Fatalf("shared line has the same seq in both windows (%d)", seqA)
	}
	rA, _ := s.Ring("#a")
	rB, _ := s.Ring("#b")
	if got := rA.Oldest(); got != 1 {
		t.Fatalf("A oldest = %d, want 1 (independent)", got)
	}
	if got := rB.Oldest(); got != 1 {
		t.Fatalf("B oldest = %d, want 1 (independent)", got)
	}
	// A replay from A's cursor only walks A's ring.
	var seen []uint64
	rA.Replay(seqA, func(seq uint64, l []byte) bool {
		seen = append(seen, seq)
		return true
	})
	if len(seen) != 1 || seen[0] != seqA {
		t.Fatalf("A from its seq = %v, want [%d]", seen, seqA)
	}
	if got := rB.Newest(); got != seqB+1 {
		t.Fatalf("B newest = %d, want %d", got, seqB+1)
	}
}

// Eviction must not panic and must keep the most recently used windows.
func TestMaxWindowsEvictionKeepsRecent(t *testing.T) {
	s := New(2, 8, 512)
	s.Append("a", []byte("a"), 0)
	s.Append("b", []byte("b"), 0)
	s.Append("a", []byte("a2"), 0) // a is now the most recently used
	s.Append("c", []byte("c"), 0)  // evicts b

	if _, ok := s.Ring("b"); ok {
		t.Fatal("b should have been evicted")
	}
	if _, ok := s.Ring("a"); !ok {
		t.Fatal("a should have survived")
	}
	if _, ok := s.Ring("c"); !ok {
		t.Fatal("c should exist")
	}
	if got := s.Windows(); strings.Join(got, ",") != "a,c" {
		t.Fatalf("windows = %v, want [a c]", got)
	}
}

// A stale cursor from an evicted window must not replay a new line under an old
// number.
func TestEvictedWindowSequenceDoesNotRewind(t *testing.T) {
	s := New(1, 8, 512)
	for i := 0; i < 4; i++ {
		s.Append("a", []byte("a"), 0)
	}
	s.Append("b", []byte("b"), 0) // evicts a
	r, ok := s.Ring("b")
	if !ok {
		t.Fatal("b missing")
	}
	if r.Oldest() <= 4 {
		t.Fatalf("new window oldest = %d, want past the evicted sequences", r.Oldest())
	}
}

func TestConcurrentAppend(t *testing.T) {
	s := New(8, 8, 512)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			window := "#w" + string(rune('0'+i%4))
			for j := 0; j < 100; j++ {
				s.Append(window, []byte("x"), int64(j))
			}
			s.Bytes()
			if r, ok := s.Ring(window); ok {
				r.Newest()
				r.Oldest()
			}
		}(i)
	}
	wg.Wait()
}
