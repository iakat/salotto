package logfile

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func TestStartupIndexesYesterday(t *testing.T) {
	dir := t.TempDir()
	today := time.Date(2026, 11, 22, 12, 0, 0, 0, time.Local)
	yesterday := today.AddDate(0, 0, -1).Format("2006-01-02")

	chanLog := filepath.Join(dir, "net", "#chan", yesterday+".log")
	statusLog := filepath.Join(dir, "net", "status", yesterday+".log")
	for _, p := range []string{chanLog, statusLog} {
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(chanLog, []byte("[10:00:00] <alice> findme in #chan\n[10:00:01] <bob> other\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statusLog, []byte("[10:00:02] Connected to IRC (findme)\n"), 0644); err != nil {
		t.Fatal(err)
	}

	w := New("net", dir, 16, fixedClock(today))
	w.SetNick("me")
	defer w.Close()

	chanIdx := chanLog + ".idx"
	statusIdx := statusLog + ".idx"
	waitForFile(t, chanIdx, 3*time.Second)
	waitForFile(t, statusIdx, 3*time.Second)

	for _, tc := range []struct {
		idx  string
		term string
	}{
		{chanIdx, "findme"},
		{statusIdx, "findme"},
	} {
		idx, err := LoadIndex(tc.idx)
		if err != nil {
			t.Fatalf("LoadIndex(%s): %v", tc.idx, err)
		}
		if _, ok, err := idx.Get([]byte(tc.term)); err != nil || !ok {
			t.Fatalf("%s does not index %q: err=%v ok=%v", tc.idx, tc.term, err, ok)
		}
	}
}

func TestSearchCreatesMissingIndex(t *testing.T) {
	dir := setupLogs(t)
	logPath := filepath.Join(dir, "net1", "#chan", "2026-11-20.log")
	idxPath := logPath + ".idx"
	_ = os.Remove(idxPath)

	if _, err := os.Stat(idxPath); err == nil {
		t.Fatal("index unexpectedly exists before search")
	}

	first, err := Query(dir, "net1", "#chan", "hello", "2026-11-20", "2026-11-20", 10, "asc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(idxPath); err != nil {
		t.Fatalf("search did not create %s: %v", idxPath, err)
	}

	second, err := Query(dir, "net1", "#chan", "hello", "2026-11-20", "2026-11-20", 10, "asc")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(second) {
		t.Fatalf("indexed search differs from first: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("hit %d differs: %+v vs %+v", i, first[i], second[i])
		}
	}
}

func TestStartupIndexIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	today := time.Date(2026, 11, 22, 12, 0, 0, 0, time.Local)
	yesterday := today.AddDate(0, 0, -1).Format("2006-01-02")

	logPath := filepath.Join(dir, "net", "#chan", yesterday+".log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("[10:00:00] <alice> findme\n"), 0644); err != nil {
		t.Fatal(err)
	}

	w := New("net", dir, 16, fixedClock(today))
	defer w.Close()

	idxPath := logPath + ".idx"
	waitForFile(t, idxPath, 3*time.Second)
	w.indexExisting()

	stamp := time.Now().Add(24 * time.Hour)
	if err := os.Chtimes(idxPath, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(idxPath)
	if err != nil {
		t.Fatal(err)
	}

	w.indexExisting()

	after, err := os.Stat(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("second pass rewrote up-to-date index: %v -> %v", before.ModTime(), after.ModTime())
	}
}
