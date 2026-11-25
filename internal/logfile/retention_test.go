package logfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// sweepTestDay is the reference "now" for every case: noon on 2026-11-25.
// Noon matters: with a 24-hour setting it is well past midnight, which is what
// exposes a missing start-of-yesterday floor.
func sweepTestDay() time.Time {
	return time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
}

// writeDayFile creates <dir>/<day>.log with body and returns its path.
func writeDayFile(t *testing.T, dir, day, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, day+".log")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeIndex writes a stand-in .idx next to a log; the sweep must not need to
// parse it to know it belongs to that day.
func writeIndex(t *testing.T, logPath, body string) string {
	t.Helper()
	path := logPath + ".idx"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s still exists (err=%v)", path, err)
	}
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s is gone: %v", path, err)
	}
}

func TestSweepDeletesADayOlderThanTheCutoff(t *testing.T) {
	dir := t.TempDir()
	old := writeDayFile(t, filepath.Join(dir, "libera", "#chan"), "2026-11-20", "five days old\n")

	res, err := Sweep(dir, 3*24*time.Hour, sweepTestDay())
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 1 {
		t.Fatalf("Files = %d, want 1", res.Files)
	}
	mustNotExist(t, old)
}

func TestSweepKeepsADayInsideTheCutoff(t *testing.T) {
	dir := t.TempDir()
	inside := writeDayFile(t, filepath.Join(dir, "libera", "#chan"), "2026-11-24", "yesterday\n")

	res, err := Sweep(dir, 3*24*time.Hour, sweepTestDay())
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 0 {
		t.Fatalf("Files = %d, want 0", res.Files)
	}
	mustExist(t, inside)
}

func TestZeroRetentionKeepsEverything(t *testing.T) {
	dir := t.TempDir()
	ancient := writeDayFile(t, filepath.Join(dir, "libera", "#chan"), "2020-01-01", "ancient\n")
	index := writeIndex(t, ancient, "idx\n")

	res, err := Sweep(dir, 0, sweepTestDay())
	if err != nil {
		t.Fatal(err)
	}
	if res != (SweepResult{}) {
		t.Fatalf("SweepResult = %+v, want zero", res)
	}
	mustExist(t, ancient)
	mustExist(t, index)
}

func TestSweepNeverDeletesTodayOrYesterday(t *testing.T) {
	dir := t.TempDir()
	window := filepath.Join(dir, "libera", "#chan")
	before := writeDayFile(t, window, "2026-11-23", "two days old\n")
	yesterday := writeDayFile(t, window, "2026-11-24", "yesterday\n")
	today := writeDayFile(t, window, "2026-11-25", "today\n")

	// Exactly 24 hours from noon is noon yesterday, which is after yesterday's
	// midnight: without the floor the yesterday file is older than the cutoff
	// and gets deleted while the day is complete. With the floor it survives.
	res, err := Sweep(dir, 24*time.Hour, sweepTestDay())
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 1 {
		t.Fatalf("Files = %d, want 1 (only the day before yesterday)", res.Files)
	}
	mustNotExist(t, before)
	mustExist(t, yesterday)
	mustExist(t, today)
}

func TestSweepDeletesTheIndexWithTheLog(t *testing.T) {
	dir := t.TempDir()
	logPath := writeDayFile(t, filepath.Join(dir, "libera", "#chan"), "2026-11-19", "old\n")
	idxPath := writeIndex(t, logPath, "old index\n")

	res, err := Sweep(dir, 24*time.Hour, sweepTestDay())
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 1 || res.Indexes != 1 {
		t.Fatalf("Files = %d, Indexes = %d, want 1 and 1", res.Files, res.Indexes)
	}
	mustNotExist(t, logPath)
	mustNotExist(t, idxPath)
}

func TestSweepLeavesAFileItCannotParse(t *testing.T) {
	dir := t.TempDir()
	window := filepath.Join(dir, "libera", "#chan")
	notes := filepath.Join(window, "notes.txt")
	archive := filepath.Join(window, "archive.log")
	shortDate := filepath.Join(window, "2026-1-1.log")
	backup := filepath.Join(window, "2026-11-20.log.bak")
	if err := os.MkdirAll(window, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{notes, archive, shortDate, backup} {
		if err := os.WriteFile(path, []byte("keep me\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := writeDayFile(t, window, "2026-11-20", "old\n")

	res, err := Sweep(dir, 24*time.Hour, sweepTestDay())
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 1 {
		t.Fatalf("Files = %d, want 1", res.Files)
	}
	mustNotExist(t, old)
	for _, path := range []string{notes, archive, shortDate, backup} {
		mustExist(t, path)
	}
}

func TestSweepRemovesEmptyWindowAndNetworkDirs(t *testing.T) {
	dir := t.TempDir()
	networkDir := filepath.Join(dir, "libera")
	windowDir := filepath.Join(networkDir, "#chan")
	writeDayFile(t, windowDir, "2026-11-20", "old\n")

	res, err := Sweep(dir, 24*time.Hour, sweepTestDay())
	if err != nil {
		t.Fatal(err)
	}
	if res.Windows != 1 {
		t.Fatalf("Windows = %d, want 1", res.Windows)
	}
	mustNotExist(t, windowDir)
	mustNotExist(t, networkDir)
	mustExist(t, dir)
}

func TestSweepRefusesAPathOutsideTheLogDir(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()

	// A link that looks like a network directory. If the sweep follows it, the
	// dated file below it is inside "outside" and would be deleted.
	outsideWindow := filepath.Join(outside, "win")
	outsideDay := writeDayFile(t, outsideWindow, "2020-01-01", "outside\n")
	if err := os.Symlink(outside, filepath.Join(dir, "evil")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// And a day file that is itself a link out of the tree.
	window := filepath.Join(dir, "libera", "#chan")
	writeDayFile(t, window, "2026-11-20", "old\n")
	victim := filepath.Join(outside, "victim.log")
	if err := os.WriteFile(victim, []byte("do not delete\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(window, "2020-01-01.log")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := Sweep(dir, 24*time.Hour, sweepTestDay()); err != nil {
		t.Fatal(err)
	}
	mustExist(t, outsideDay)
	mustExist(t, victim)
}

func TestSweepReportsBytesAndCounts(t *testing.T) {
	dir := t.TempDir()
	first := writeDayFile(t, filepath.Join(dir, "libera", "#one"), "2026-11-20", "0123456789")
	writeIndex(t, first, "i")
	second := writeDayFile(t, filepath.Join(dir, "libera", "#two"), "2026-11-19", "01234567890123456789")
	writeIndex(t, second, "i")

	res, err := Sweep(dir, 24*time.Hour, sweepTestDay())
	if err != nil {
		t.Fatal(err)
	}
	want := SweepResult{Files: 2, Bytes: 30, Indexes: 2, Windows: 2}
	if res != want {
		t.Fatalf("SweepResult = %+v, want %+v", res, want)
	}
}

func TestSweepIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	logPath := writeDayFile(t, filepath.Join(dir, "libera", "#chan"), "2026-11-20", "old\n")
	writeIndex(t, logPath, "idx\n")

	first, err := Sweep(dir, 24*time.Hour, sweepTestDay())
	if err != nil {
		t.Fatal(err)
	}
	if first.Files != 1 {
		t.Fatalf("first Files = %d, want 1", first.Files)
	}

	second, err := Sweep(dir, 24*time.Hour, sweepTestDay())
	if err != nil {
		t.Fatalf("second sweep returned %v, want nil", err)
	}
	if second != (SweepResult{}) {
		t.Fatalf("second SweepResult = %+v, want zero", second)
	}
}
