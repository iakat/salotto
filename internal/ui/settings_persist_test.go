package ui_test

import (
	"encoding/base64"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iakat/salotto/internal/ui"
)

// A setting typed in a browser has to survive the tab. The bridge starts a fresh child per
// websocket, so without a shared settings file every reconnect is a client with the
// defaults - and "/fset highlight salotto" would report success, draw the new value in the
// settings screen, and be gone the moment the page reloaded. That is exactly what happened
// live: the value was accepted, listed, and then not in effect on the next arriving line.
//
// This drives two real child processes through one file, which is the shape the web pod
// uses: -settings on the child, one path for every session.
func TestASettingSurvivesTheSession(t *testing.T) {
	bin := buildTUI(t)
	settings := filepath.Join(t.TempDir(), "settings.json")

	// typeIn starts a session against settings, types keys, and returns what was drawn
	// once want appears.
	typeIn := func(keys, want string) string {
		srv := httptest.NewServer(ui.TermHandler(ui.NewTermServer(func(cols, rows int) *exec.Cmd {
			cmd := exec.Command(bin, "-settings", settings)
			cmd.Env = ui.SessionEnv()
			return cmd
		})))
		defer srv.Close()

		tm := dialTerminal(t, srv.URL, 100, 30)
		tm.waitFor("\u2570", 20*time.Second)
		for _, r := range keys {
			tm.send(map[string]string{"type": "input", "data": base64.StdEncoding.EncodeToString([]byte(string(r)))})
			time.Sleep(15 * time.Millisecond)
		}
		return tm.waitFor(want, 10*time.Second)
	}

	// First session: set a highlight.
	_ = typeIn("/fset highlight salotto\r", "now salotto")
	if _, err := os.Stat(settings); err != nil {
		t.Fatalf("no settings file was written at %s, so nothing can survive the session: %v", settings, err)
	}

	// Second session: the value has to be there without being set again.
	second := typeIn("/fset\r", "highlights")
	if strings.Contains(second, "highlights     none") {
		t.Fatalf("a new session lost the highlight set in the previous one:\n%q", second)
	}
}
