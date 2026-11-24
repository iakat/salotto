package ui

import (
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPrefsFromTheBouncerAreAdopted: the whole feature is that a setting made
// in one client is already in the next one. The PREF line must be intercepted
// (never drawn), decoded, and applied to the model.
func TestPrefsFromTheBouncerAreAdopted(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	p := NewProgram("#test", tr)

	want := DefaultSettings()
	want.Timestamps = false
	want.Highlights = []string{"salotto"}
	token, err := want.token()
	if err != nil {
		t.Fatal(err)
	}
	fb.inject(t, ":salotto PREF me "+token)

	msg, ok := p.Next()
	if !ok {
		t.Fatal("the transport closed before the PREF line")
	}
	if _, ok := msg.(Prefs); !ok {
		t.Fatalf("PREF was routed as %T instead of being intercepted", msg)
	}
	p.Step(msg)

	got := p.Model().settings
	if got.Timestamps {
		t.Fatal("the adopted settings did not turn timestamps off")
	}
	if !contains(got.Highlights, "salotto") {
		t.Fatalf("the adopted highlight list is %v, want salotto", got.Highlights)
	}
}

// TestFsetSendsTheSettingsToTheBouncer is the writing half: /fset must still
// write the client's file, but it also has to put the blob on the wire or the
// next client never sees the change.
func TestFsetSendsTheSettingsToTheBouncer(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	p := NewProgram("#test", tr)
	p.Key("runes", "/fset timestamps off")
	p.Key("enter", "")

	line := fb.expect(t)
	if !strings.HasPrefix(line, "PREF SET ") {
		t.Fatalf("/fset sent %q, want PREF SET", line)
	}
	token := strings.TrimPrefix(line, "PREF SET ")
	data, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("the settings blob is not base64: %v", err)
	}
	var sent Settings
	if err := json.Unmarshal(data, &sent); err != nil {
		t.Fatalf("the settings blob is not JSON: %v", err)
	}
	if sent.Timestamps {
		t.Fatal("the blob on the wire still has timestamps on")
	}
}

// TestAdoptingPrefsDoesNotEchoThem is the one piece of state logic in the
// change. If adoption sent the blob back, two attached clients would bounce the
// same settings at each other forever.
func TestAdoptingPrefsDoesNotEchoThem(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	p := NewProgram("#test", tr)
	want := DefaultSettings()
	want.Timestamps = false
	token, err := want.token()
	if err != nil {
		t.Fatal(err)
	}
	fb.inject(t, ":salotto PREF me "+token)

	msg, ok := p.Next()
	if !ok {
		t.Fatal("the transport closed before the PREF line")
	}
	p.Step(msg)

	select {
	case line := <-fb.sent:
		t.Fatalf("adopting the settings echoed them back: %q", line)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestAClientWithNoBouncerStillKeepsItsSettings is the regression guard: a
// plain IRCd, a headless test and the renderer tests have no bouncer, so the
// file has to remain the whole story there.
func TestAClientWithNoBouncerStillKeepsItsSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	p := NewProgram("#test", nil).WithSettings(path)

	p.Key("runes", "/fset seconds off")
	p.Key("enter", "")

	if reloaded := LoadSettings(path); reloaded.Seconds {
		t.Fatal("with no bouncer, /fset did not write the client's own file")
	}
}

// TestPrefsGetIsSentOnRegistration: the client cannot know whether the bouncer
// holds settings until it asks, and 001 is the one moment in the connection
// where registration is known to be complete.
func TestPrefsGetIsSentOnRegistration(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	tr.Register("me", "")
	fb.inject(t, ":srv 001 me :Welcome")

	found := false
	for i := 0; i < 10; i++ {
		if fb.expect(t) == "PREF GET" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("001 did not ask the bouncer for its settings")
	}
}

// TestAnEmptyPrefsAnswerSeedsTheBouncer fixes the timeout-free rule: an explicit
// empty PREF means "the bouncer holds nothing", and a client with a file
// answers it by sending that file. Silence would have needed a timer.
func TestAnEmptyPrefsAnswerSeedsTheBouncer(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	path := filepath.Join(t.TempDir(), "settings.json")
	if err := SaveSettings(path, Settings{Timestamps: true, Seconds: false}); err != nil {
		t.Fatal(err)
	}
	p := NewProgram("#test", tr).WithSettings(path)

	fb.inject(t, ":salotto PREF me :")
	msg, ok := p.Next()
	if !ok {
		t.Fatal("the transport closed before the empty PREF line")
	}
	p.Step(msg)

	line := fb.expect(t)
	if !strings.HasPrefix(line, "PREF SET ") {
		t.Fatalf("an empty answer produced %q, want the client's settings as PREF SET", line)
	}
}
