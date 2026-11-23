package logfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStripControlsModuleIsOptIn pins the ZNC behaviour: `log` writes message
// text as it arrived and stripcontrols is a separate module you load when you
// want the colours gone.
//
// Two different things are true at once, and this test keeps them apart:
// sanitizeBody always removes CR, LF and C0 bytes except tab, because a log is
// line-oriented and an embedded newline would corrupt it; stripcontrols removes
// IRC colour codes and the rest of the C0 range from the text a human reads. The
// first is log integrity and is not optional, the second is presentation and is.
func TestStripControlsModuleIsOptIn(t *testing.T) {
	const line = ":x!u@h PRIVMSG #c :\x0304red\x03 and\ttab"

	read := func(strip bool) string {
		t.Helper()
		dir := t.TempDir()
		w := New("net", dir, 64, time.Now)
		w.SetStripControls(strip)
		w.OnLine(1, []byte(line))
		w.Close()
		files, err := os.ReadDir(filepath.Join(dir, "net", "#c"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no log file: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(dir, "net", "#c", files[0].Name()))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	off := read(false)
	// sanitizeBody removes the \x03 byte itself because it is a C0 control, so
	// what survives is the colour code's digits. That is the honest expectation:
	// the bytes never reach the file even with the module off.
	if !strings.Contains(off, "04red") {
		t.Fatalf("with the module off the body must not have been colour-stripped: %q", off)
	}

	// The colour code's digits survive the log-integrity pass and only the
	// colour module removes them, which is what makes the two layers distinct.

	on := read(true)
	if strings.Contains(on, "04red") {
		t.Fatalf("with the module on the colours must be gone: %q", on)
	}
	if !strings.Contains(on, "red and") {
		t.Fatalf("the text itself must survive: %q", on)
	}
}

// TestStripControlsKeepsTheReplayRingIntact: the stripping happens on the log
// entry, not on the wire line, so the window ring still carries what the user
// actually said. Otherwise toggling a logging module would change history.
func TestStripControlsKeepsTheReplayRingIntact(t *testing.T) {
	dir := t.TempDir()
	w := New("net", dir, 64, time.Now)
	w.SetStripControls(true)
	line := []byte(":x!u@h PRIVMSG #c :\x0304keep me\x03")
	entries := w.Route(line, false)
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	if !strings.Contains(entries[0].Body, "\x0304") {
		t.Fatalf("the routed entry was stripped, so the ring loses the original: %q", entries[0].Body)
	}
	w.Close()
}
