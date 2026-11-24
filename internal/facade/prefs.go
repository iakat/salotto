package facade

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/iakat/salotto/internal/ircline"
)

// prefsFile is the one file the bouncer keeps here. Its contents are
// {"prefs":"<base64>"} and nothing in this package parses the string: it is
// wrapped so the file is valid JSON, and it is opaque so a human poking at it
// cannot mistake it for a settings document the daemon understands.
const prefsFile = "prefs.json"

// handlePref implements the two verbs a client uses to share its settings.
//
// PREF GET with no stored blob answers an explicit empty PREF, not silence.
// Silence would be smaller, but it would force the client to use a timer to
// tell "the bouncer holds nothing" from "the answer is still on its way", and
// a timer in the registration path races the first settings change. The empty
// token is the same fact without the race. The blob itself is never inspected
// beyond its base64 envelope and its size, which is what lets a client add a
// setting without this daemon changing.
//
// A SET whose line was cut to fit is refused instead of decoded. That check is
// first and it is not defensive: base64 cut on a four-character boundary still
// decodes, so without it the store would keep a fragment of somebody's settings,
// PREF GET would hand that fragment to every client, and each one would fail to
// parse it and quietly revert to the defaults. The symptom would be "my settings
// keep resetting", investigated in the client, while the damage happened here.
func (c *client) handlePref(rest []byte) bool {
	sub, args := nextToken(ircline.Params(rest))
	switch {
	case equalFold(sub, "GET"):
		nick := c.nick
		if nick == "" {
			nick = "*"
		}
		token, ok := c.srv.prefsToken()
		if !ok {
			return c.send(":", c.srv.cfg.ServerName, " PREF ", nick, " :")
		}
		return c.send(":", c.srv.cfg.ServerName, " PREF ", nick, " ", token)

	case equalFold(sub, "SET"):
		if c.lineOverflow {
			return c.status("PREF blob is larger than this daemon accepts (", itoa(uint64(ircline.MaxPrefsBytes)), " bytes)")
		}
		token := strings.TrimSpace(strings.TrimPrefix(string(args), ":"))
		if token == "" {
			return c.status("Usage: PREF SET <base64>")
		}
		raw, err := base64.StdEncoding.DecodeString(token)
		if err != nil {
			// The wire is base64 by definition, so this is not a settings blob
			// at all. Refusing it keeps the stored value a thing every client
			// can decode.
			return c.status("PREF SET expects base64")
		}
		if len(raw) > ircline.MaxPrefsBytes {
			return c.status("PREF blob is larger than this daemon accepts (", itoa(uint64(ircline.MaxPrefsBytes)), " bytes)")
		}
		if err := c.srv.storePrefs(token); err != nil && !c.status("PREF stored in memory but not persisted: "+err.Error()) {
			return false
		}
		nick := c.nick
		if nick == "" {
			nick = "*"
		}
		// Fan out to every other attached client and not to the sender, who
		// already has the blob. Reusing BroadcastFromSkip means there is one
		// broadcast path and one slow-client policy, not a second one that can
		// disagree about when to drop a client.
		line := []byte(":" + c.srv.cfg.ServerName + " PREF " + nick + " " + token + "\r\n")
		c.srv.hub.BroadcastFromSkip(line, "", func(s *Subscriber) bool {
			return s == c.sub
		})
		return true

	default:
		return c.status("Usage: PREF GET|SET <base64>")
	}
}

// prefsToken returns the stored token and whether one exists. The token is the
// client's bytes, not a re-encoding of them: a GET after a SET must answer the
// same bytes the sender sent, or an opaque store would not be opaque.
func (s *Server) prefsToken() (string, bool) {
	s.prefsMu.Lock()
	defer s.prefsMu.Unlock()
	return s.prefs, s.prefs != ""
}

// storePrefs remembers one blob and persists it when a state directory is
// configured. The in-memory value is updated even if the file cannot be
// written: the clients are already sharing it, and turning a disk error into a
// refused SET would make the feature fail on a read-only pod rather than just
// lose it on the next restart.
func (s *Server) storePrefs(token string) error {
	s.prefsMu.Lock()
	s.prefs = token
	err := s.writePrefsLocked(token)
	s.prefsMu.Unlock()
	if err != nil {
		s.log.Warn("prefs not persisted", "err", err)
	}
	return err
}

// writePrefsLocked writes <state_dir>/prefs.json with the same temporary file
// and rename that the client's own SaveSettings uses. A crash mid-write then
// leaves the previous settings rather than a truncated JSON file, which is the
// difference between losing one change and losing every change.
func (s *Server) writePrefsLocked(token string) error {
	dir := s.cfg.StateDir
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(struct {
		Prefs string `json:"prefs"`
	}{Prefs: token})
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := filepath.Join(dir, prefsFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadPrefs reads the stored blob at New so the first client after a restart
// gets the shared settings without having to seed them again. A missing file is
// the normal first run. A malformed one is ignored rather than fatal: the
// daemon's availability must not depend on a file it never parses, and an empty
// answer simply makes the next client the seed.
func (s *Server) loadPrefs() {
	dir := s.cfg.StateDir
	if dir == "" {
		return
	}
	path := filepath.Join(dir, prefsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("prefs not loaded", "path", path, "err", err)
		}
		return
	}
	var file struct {
		Prefs string `json:"prefs"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		s.log.Warn("prefs file is not valid JSON", "path", path, "err", err)
		return
	}
	if file.Prefs == "" {
		return
	}
	// The size cap applies to what a client could put on the wire, and the file
	// is just another way to put a token in front of this process. Checking the
	// decoded length also means a corrupt file cannot make GET return something
	// every client will fail to decode.
	raw, err := base64.StdEncoding.DecodeString(file.Prefs)
	if err != nil {
		s.log.Warn("prefs file is not base64", "path", path, "err", err)
		return
	}
	if len(raw) > ircline.MaxPrefsBytes {
		s.log.Warn("prefs file is too large", "path", path, "bytes", len(raw))
		return
	}
	s.prefsMu.Lock()
	s.prefs = file.Prefs
	s.prefsMu.Unlock()
}
