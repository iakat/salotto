package logfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRotation(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	now := time.Date(2026, 11, 22, 23, 59, 59, 0, time.Local)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	w := New("net", dir, 10, clock)
	w.SetServer("srv")
	// day1 line
	w.OnLine(1, []byte(":x!u@h PRIVMSG #c :hi"))
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	now = time.Date(2026, 11, 23, 0, 0, 1, 0, time.Local)
	mu.Unlock()
	w.OnLine(2, []byte(":x!u@h PRIVMSG #c :hi2"))
	time.Sleep(50 * time.Millisecond)
	w.Close()
	// check two day files
	p1 := filepath.Join(dir, "net", "#c", "2026-11-22.log")
	p2 := filepath.Join(dir, "net", "#c", "2026-11-23.log")
	if _, err := os.Stat(p1); err != nil {
		t.Fatalf("missing day1: %v", err)
	}
	if _, err := os.Stat(p2); err != nil {
		t.Fatalf("missing day2: %v", err)
	}
}

func TestQueueDrop(t *testing.T) {
	dir := t.TempDir()
	// block drain by not reading? Use very small queue and flood before drain can process.
	// We'll create writer with queue depth 1 and a clock; flood 1000 events synchronously.
	clock := time.Now
	w := New("net", dir, 1, clock)
	// temporarily make handle slow? Can't. Instead just ensure OnLine doesn't block.
	start := time.Now()
	for i := 0; i < 1000; i++ {
		w.OnLine(uint64(i), []byte(":x!u@h PRIVMSG #c :hi"))
	}
	if time.Since(start) > time.Second {
		t.Fatal("OnLine blocked")
	}
	w.Close()
	if w.Dropped() == 0 {
		t.Fatal("expected drops on overflow")
	}
}

func TestBufferReuse(t *testing.T) {
	dir := t.TempDir()
	clock := time.Now
	w := New("net", dir, 256, clock)
	buf := make([]byte, 128)
	for i := 0; i < 200; i++ {
		line := fmt.Sprintf(":nick!u@h PRIVMSG #c :message-%d", i)
		n := copy(buf, line)
		w.OnLine(uint64(i+1), buf[:n])
	}
	if d := w.Dropped(); d != 0 {
		t.Fatalf("dropped = %d, want 0", d)
	}
	w.Close()
	files, err := os.ReadDir(filepath.Join(dir, "net", "#c"))
	if err != nil || len(files) == 0 {
		t.Fatalf("read dir: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "net", "#c", files[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 200 {
		t.Fatalf("got %d lines, want 200", len(lines))
	}
	for i := 0; i < 200; i++ {
		want := fmt.Sprintf("message-%d", i)
		if !strings.Contains(lines[i], want) {
			t.Fatalf("line %d missing %q: %q", i, want, lines[i])
		}
		// no duplicate fragments: each line starts with [
		if !strings.HasPrefix(lines[i], "[") {
			t.Fatalf("line %d no prefix: %q", i, lines[i])
		}
	}
}

func TestCRControlStrip(t *testing.T) {
	dir := t.TempDir()
	clock := time.Now
	w := New("net", dir, 256, clock)
	w.OnLine(1, []byte(":x!u@h PRIVMSG #c :a\rb\nc\x01d\te"))
	w.Close()
	files, _ := os.ReadDir(filepath.Join(dir, "net", "#c"))
	data, _ := os.ReadFile(filepath.Join(dir, "net", "#c", files[0].Name()))
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %q", len(lines), string(data))
	}
	if strings.Contains(lines[0], "\r") || strings.Contains(lines[0], "\n") || strings.Contains(lines[0], "\x01") {
		t.Fatalf("control not stripped: %q", lines[0])
	}
	if !strings.Contains(lines[0], "\t") {
		t.Fatalf("tab not preserved: %q", lines[0])
	}
}

func TestDuplicateNumeric(t *testing.T) {
	dir := t.TempDir()
	clock := time.Now
	w := New("net", dir, 256, clock)
	w.OnLine(1, []byte("@time=2026-11-22T19:03:43.325Z :irc.freebnc.net 254 salottopoc 0 :channels formed"))
	w.Close()
	files, _ := os.ReadDir(filepath.Join(dir, "net", "status"))
	data, _ := os.ReadFile(filepath.Join(dir, "net", "status", files[0].Name()))
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %q", len(lines), string(data))
	}
	if strings.Contains(lines[0], "@time=") {
		t.Fatalf("@time not dropped: %q", lines[0])
	}
}

func TestPoolBalance(t *testing.T) {
	// Assert the enqueue path itself, not whole-process allocations: the drain
	// goroutine's formatting allocates, so AllocsPerRun on a draining Writer is
	// inherently noisy (it flaked ~20% on dev). Block the drain in clock(), fill
	// the one-slot queue, then measure the drop path. A zero result proves the
	// slab went back to the pool and no new slab was allocated; Dropped proves
	// the send was actually rejected. BenchmarkOnLine is the same evidence for
	// the accepted path (0 B/op, 0 allocs/op).
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	clock := func() time.Time {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-gate
		return time.Now()
	}
	// An empty log_dir with no network subdirectory keeps the startup index pass
	// from calling clock, so entered/gate unambiguously belong to the drain.
	w := New("net", t.TempDir(), 1, clock)
	line := []byte(":x!u@h PRIVMSG #c :hi")

	w.OnLine(1, line)
	<-entered
	w.OnLine(2, line)

	allocs := testing.AllocsPerRun(100, func() {
		w.OnLine(3, line)
	})
	close(gate)
	w.Close()

	if allocs != 0 {
		t.Fatalf("drop path allocs per run = %v, want 0", allocs)
	}
	if w.Dropped() == 0 {
		t.Fatal("expected drops once the queue was full")
	}
}

func BenchmarkOnLine(b *testing.B) {
	w := New("net", "", 1, time.Now)
	line := []byte(":x!u@h PRIVMSG #c :hi")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.OnLine(uint64(i), line)
	}
	b.StopTimer()
	w.Close()
}
