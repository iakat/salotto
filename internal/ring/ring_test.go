package ring

import (
	"fmt"
	"testing"
)

func TestAppendReplay(t *testing.T) {
	r := New(4, 512)
	lines := [][]byte{[]byte("hello"), []byte("world"), []byte("foo bar")}
	for i, line := range lines {
		if seq := r.Append(line); seq != uint64(i+1) {
			t.Fatalf("Append returned %d, want %d", seq, i+1)
		}
	}
	if r.Oldest() != 1 || r.Newest() != 3 || r.Len() != 3 {
		t.Fatalf("bounds = %d..%d len %d, want 1..3 len 3", r.Oldest(), r.Newest(), r.Len())
	}

	var got [][]byte
	missing := r.Replay(1, func(seq uint64, line []byte) bool {
		if seq != uint64(len(got)+1) {
			t.Fatalf("seq = %d, want %d", seq, len(got)+1)
		}
		got = append(got, append([]byte(nil), line...))
		return true
	})
	if missing != 0 {
		t.Fatalf("missing = %d, want 0", missing)
	}
	if len(got) != len(lines) {
		t.Fatalf("replayed %d lines, want %d", len(got), len(lines))
	}
	for i := range lines {
		if string(got[i]) != string(lines[i]) {
			t.Fatalf("line %d = %q, want %q", i, got[i], lines[i])
		}
	}
}

func testLine(i int) string {
	return fmt.Sprintf("line-%02d-%s", i, "0123456789012345678901234567890123456789")
}

func TestWrapAround(t *testing.T) {
	r := New(2, 64)
	const count = 50
	lines := make([]string, count)
	for i := 0; i < count; i++ {
		lines[i] = testLine(i)
		r.Append([]byte(lines[i]))
	}

	if r.Newest() != count {
		t.Fatalf("Newest = %d, want %d", r.Newest(), count)
	}
	if r.Oldest() <= 1 {
		t.Fatalf("Oldest = %d, want it to advance past 1", r.Oldest())
	}
	if r.Len() != r.Newest()-r.Oldest()+1 {
		t.Fatalf("Len = %d, want %d", r.Len(), r.Newest()-r.Oldest()+1)
	}

	var seqs []uint64
	var got []string
	missing := r.Replay(r.Oldest(), func(seq uint64, line []byte) bool {
		seqs = append(seqs, seq)
		got = append(got, string(line))
		return true
	})
	if missing != 0 {
		t.Fatalf("missing = %d, want 0", missing)
	}
	if uint64(len(got)) != r.Len() {
		t.Fatalf("replayed %d lines, want %d", len(got), r.Len())
	}
	if len(seqs) == 0 || seqs[0] != r.Oldest() {
		t.Fatalf("first seq = %v, want %d", seqs, r.Oldest())
	}
	if seqs[len(seqs)-1] != r.Newest() {
		t.Fatalf("last seq = %d, want %d", seqs[len(seqs)-1], r.Newest())
	}
	for i, seq := range seqs {
		if i > 0 && seq != seqs[i-1]+1 {
			t.Fatalf("gap or duplicate at index %d: %d after %d", i, seq, seqs[i-1])
		}
		if got[i] != lines[seq-1] {
			t.Fatalf("line for seq %d = %q, want %q", seq, got[i], lines[seq-1])
		}
	}
}

func TestReplayOverwritten(t *testing.T) {
	r := New(2, 64)
	for i := 0; i < 50; i++ {
		r.Append([]byte(testLine(i)))
	}
	if r.Oldest() <= 1 {
		t.Fatalf("Oldest = %d, want it to advance", r.Oldest())
	}

	calls := 0
	missing := r.Replay(1, func(seq uint64, line []byte) bool {
		calls++
		if calls == 1 && seq != r.Oldest() {
			t.Fatalf("first replayed seq = %d, want %d", seq, r.Oldest())
		}
		return true
	})
	if missing == 0 {
		t.Fatal("missing = 0, want > 0")
	}
	if calls == 0 {
		t.Fatal("Replay emitted nothing")
	}
}

func TestTruncation(t *testing.T) {
	r := New(2, 512)
	big := make([]byte, 5000)
	for i := range big {
		big[i] = 'x'
	}
	seq := r.Append(big)
	if !r.Truncated(seq) {
		t.Fatal("Truncated = false, want true")
	}

	var stored []byte
	missing := r.Replay(seq, func(s uint64, line []byte) bool {
		if s != seq {
			t.Fatalf("seq = %d, want %d", s, seq)
		}
		stored = append(stored[:0], line...)
		return true
	})
	if missing != 0 {
		t.Fatalf("missing = %d, want 0", missing)
	}
	if len(stored) >= len(big) {
		t.Fatalf("stored length = %d, want < %d", len(stored), len(big))
	}
	if len(stored) != 510 {
		t.Fatalf("stored length = %d, want 510", len(stored))
	}

	short := r.Append([]byte("short"))
	if r.Truncated(short) {
		t.Fatal("Truncated = true for short line")
	}
}

func TestStamp(t *testing.T) {
	r := New(2, 64)
	seq := r.AppendStamp([]byte("first"), 12345)
	ts, ok := r.Stamp(seq)
	if !ok || ts != 12345 {
		t.Fatalf("Stamp = %d, %v; want 12345, true", ts, ok)
	}

	for i := 0; i < 100; i++ {
		r.Append([]byte(testLine(i)))
	}
	if _, ok := r.Stamp(seq); ok {
		t.Fatalf("Stamp(%d) = true, want false after eviction", seq)
	}
	if _, ok := r.Stamp(0); ok {
		t.Fatal("Stamp(0) = true, want false")
	}
	if _, ok := r.Stamp(r.Newest() + 1); ok {
		t.Fatal("Stamp(newest+1) = true, want false")
	}
}

func TestEmpty(t *testing.T) {
	r := New(2, 64)
	if r.Oldest() != 0 || r.Newest() != 0 || r.Len() != 0 {
		t.Fatalf("empty bounds = %d..%d len %d, want all 0", r.Oldest(), r.Newest(), r.Len())
	}
	if r.Bytes() != 0 {
		t.Fatalf("Bytes = %d, want 0", r.Bytes())
	}

	called := false
	missing := r.Replay(1, func(seq uint64, line []byte) bool {
		called = true
		return true
	})
	if called {
		t.Fatal("emit was called on an empty ring")
	}
	if missing != 0 {
		t.Fatalf("missing = %d, want 0", missing)
	}
}

func TestEmitStopsEarly(t *testing.T) {
	r := New(2, 512)
	for i := 0; i < 5; i++ {
		r.Append([]byte{byte('a' + i)})
	}

	calls := 0
	var last uint64
	r.Replay(1, func(seq uint64, line []byte) bool {
		calls++
		last = seq
		return calls < 2
	})
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if last != 2 {
		t.Fatalf("last seq = %d, want 2", last)
	}
}

var allocEmitSink uint64

func allocEmit(seq uint64, line []byte) bool {
	allocEmitSink += seq + uint64(len(line))
	return true
}

func TestNoAllocations(t *testing.T) {
	r := New(8, 4096)
	scratch := make([]byte, 200)
	for i := range scratch {
		scratch[i] = 'a'
	}

	f := func() {
		for i := 0; i < 1000; i++ {
			r.Append(scratch)
		}
		r.Replay(r.Oldest(), allocEmit)
	}

	if got := testing.AllocsPerRun(10, f); got != 0 {
		t.Fatalf("allocations per run = %v, want 0", got)
	}
}

func noopEmit(seq uint64, line []byte) bool {
	return true
}

func BenchmarkAppend(b *testing.B) {
	r := New(8, 256*1024)
	line := []byte(":irc.example.net 001 nick :Welcome to the network")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Append(line)
	}
}

func BenchmarkReplay1MiB(b *testing.B) {
	r := New(8, 128*1024)
	line := []byte(":irc.example.net 001 nick :Welcome to the network")
	total := 0
	for total < 1<<20 {
		r.Append(line)
		total += len(line) + 2
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Replay(r.Oldest(), noopEmit)
	}
}
