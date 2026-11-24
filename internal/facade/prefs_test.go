package facade

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iakat/salotto/internal/ircline"
)

// prefsResponse reads one PREF GET answer and returns its token. The empty token
// is real state ("the bouncer holds nothing"), so ok is false only when the line
// is not a PREF at all.
func prefsResponse(t *testing.T, conn interface {
	SetReadDeadline(time.Time) error
}, br interface {
	ReadString(byte) (string, error)
}) (string, bool) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read PREF answer: %v", err)
	}
	line = strings.TrimRight(line, "\r\n")
	_, rest, ok := strings.Cut(line, " PREF ")
	if !ok {
		t.Fatalf("not a PREF line: %q", line)
	}
	_, token, _ := strings.Cut(rest, " ")
	token = strings.TrimSpace(strings.TrimPrefix(token, ":"))
	return token, token != ""
}

func getPrefs(t *testing.T, conn interface {
	SetReadDeadline(time.Time) error
	Write([]byte) (int, error)
}, br interface {
	ReadString(byte) (string, error)
}) (string, bool) {
	t.Helper()
	if _, err := conn.Write([]byte("PREF GET\r\n")); err != nil {
		t.Fatalf("write PREF GET: %v", err)
	}
	return prefsResponse(t, conn, br)
}

// TestPrefsAreStoredOpaqueAndSurviveARestart is the whole reason the blob is
// bytes and not a settings struct. The token deliberately is not JSON: a daemon
// that parsed it would either reject it or rewrite it, and both mean a client
// cannot add a setting without a daemon release.
func TestPrefsAreStoredOpaqueAndSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(newFakeNet("libera"))
	cfg.StateDir = dir
	_, addr := startServer(t, cfg)
	conn, br := dialClient(t, addr)
	register(t, conn, br, "alice")
	collectReplay(t, conn, br)

	token := base64.StdEncoding.EncodeToString([]byte("not json\x00opaque bytes"))
	sendLines(t, conn, "PREF SET "+token)

	got, ok := getPrefs(t, conn, br)
	if !ok || got != token {
		t.Fatalf("stored token = %q (present %v), want the bytes that were sent %q", got, ok, token)
	}

	// The file is a wrapper, not a settings document: it must hold exactly the
	// token the client sent.
	data, err := os.ReadFile(filepath.Join(dir, "prefs.json"))
	if err != nil {
		t.Fatalf("no prefs file was written: %v", err)
	}
	var file struct {
		Prefs string `json:"prefs"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("prefs file is not JSON: %v\n%s", err, data)
	}
	if file.Prefs != token {
		t.Fatalf("file holds %q, want %q", file.Prefs, token)
	}

	cfg2 := testConfig(newFakeNet("libera"))
	cfg2.StateDir = dir
	_, addr2 := startServer(t, cfg2)
	conn2, br2 := dialClient(t, addr2)
	register(t, conn2, br2, "bob")
	collectReplay(t, conn2, br2)

	got, ok = getPrefs(t, conn2, br2)
	if !ok || got != token {
		t.Fatalf("after restart the bouncer answered %q (present %v), want %q", got, ok, token)
	}
}

// TestPrefsFanOutToTheOtherClientsAndNotTheSource pins both halves of the
// fan-out rule. Sending it back to the sender would make two clients echo the
// same blob at each other, and sending it to nobody leaves the second client
// looking at settings the bouncer has already replaced.
func TestPrefsFanOutToTheOtherClientsAndNotTheSource(t *testing.T) {
	cfg := testConfig(newFakeNet("libera"))
	s, addr := startServer(t, cfg)

	sender, senderBr := dialClient(t, addr)
	register(t, sender, senderBr, "alice")
	collectReplay(t, sender, senderBr)

	other, otherBr := dialClient(t, addr)
	register(t, other, otherBr, "bob")
	collectReplay(t, other, otherBr)

	waitFor(t, 3*time.Second, func() bool { return s.Attached() == 2 }, "both clients attached")

	token := base64.StdEncoding.EncodeToString([]byte(`{"opaque":true}`))
	sendLines(t, sender, "PREF SET "+token)

	otherLine := readClientLine(t, other, otherBr)
	if !strings.Contains(otherLine, " PREF ") || !strings.HasSuffix(otherLine, " "+token) {
		t.Fatalf("the other client received %q, want the settings token", otherLine)
	}

	// The sender already has the blob; a PREF line would be the echo that makes
	// two clients bounce it forever.
	_ = sender.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if line, err := senderBr.ReadString('\n'); err == nil {
		t.Fatalf("the sender received its own PREF back: %q", line)
	}
	_ = sender.SetReadDeadline(time.Time{})
}

// TestPrefsRefuseAnOversizedBlob: an unbounded parameter is a way to allocate
// the operator's memory from a client. The refusal has to leave the old value
// alone, so "too large" does not mean "the last settings are gone".
func TestPrefsRefuseAnOversizedBlob(t *testing.T) {
	cfg := testConfig(newFakeNet("libera"))
	cfg.MaxLineLen = 200000
	cfg.OutBuf = 200000
	_, addr := startServer(t, cfg)
	conn, br := dialClient(t, addr)
	register(t, conn, br, "alice")
	collectReplay(t, conn, br)

	big := strings.Repeat("x", ircline.MaxPrefsBytes+1)
	token := base64.StdEncoding.EncodeToString([]byte(big))
	sendLines(t, conn, "PREF SET "+token)

	line := readClientLine(t, conn, br)
	if !strings.Contains(line, " NOTICE ") || !strings.Contains(strings.ToLower(line), "larger than this daemon accepts") {
		t.Fatalf("oversized blob was not refused with a status NOTICE: %q", line)
	}

	if got, ok := getPrefs(t, conn, br); ok {
		t.Fatalf("an oversized blob was stored anyway: %q", got)
	}
}

// TestPrefsAreInMemoryWithoutAStateDir: with no state directory the round trip
// still works, and a second process starts empty. That is the honest default
// while rings and cursors are also memory only.
func TestPrefsAreInMemoryWithoutAStateDir(t *testing.T) {
	cfg := testConfig(newFakeNet("libera"))
	_, addr := startServer(t, cfg)
	conn, br := dialClient(t, addr)
	register(t, conn, br, "alice")
	collectReplay(t, conn, br)

	token := base64.StdEncoding.EncodeToString([]byte(`{"opaque":true}`))
	sendLines(t, conn, "PREF SET "+token)
	if got, ok := getPrefs(t, conn, br); !ok || got != token {
		t.Fatalf("in-memory round trip = %q (present %v), want %q", got, ok, token)
	}

	cfg2 := testConfig(newFakeNet("libera"))
	_, addr2 := startServer(t, cfg2)
	conn2, br2 := dialClient(t, addr2)
	register(t, conn2, br2, "bob")
	collectReplay(t, conn2, br2)
	if got, ok := getPrefs(t, conn2, br2); ok {
		t.Fatalf("with no state_dir a second process still answered %q, so the blob was persisted", got)
	}
}
