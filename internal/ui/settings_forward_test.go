package ui

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The settings document is shared state, not one client's private file.
//
// One bouncer blob is fanned out to every client attached to the account, and those clients are
// not necessarily the same build: a laptop on the release before last, a browser tab on trunk, a
// phone that has not been updated. Each of them decodes the blob into the fields it knows.
//
// The failure this pins: a client that wrote back only the fields it could see deleted every key
// it did not recognise. Changing one boolean in an older client therefore wiped a newer client's
// settings - and the older client looked innocent, because it had never seen the key and had no
// way to notice it was gone. The next fan-out carried the loss to everyone.

// decodeSlice is Settings.token, reversed, for assertions.
func decodeSlice(t *testing.T, tok string) map[string]json.RawMessage {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(tok)
	if err != nil {
		t.Fatalf("token is not base64: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("token is not a JSON object: %v (%s)", err, raw)
	}
	return fields
}

// TestAKeyThisBuildDoesNotKnowSurvivesASave is the core case.
func TestAKeyThisBuildDoesNotKnowSurvivesASave(t *testing.T) {
	blob := []byte(`{"timestamps":true,"font_ligatures":false,"highlights":["salotto"]}`)

	m := New("#test")
	m.settings = decodeSettings(blob)
	m.settings.Timestamps = false // the user changes one thing this build knows

	tok, err := m.settings.token()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	fields := decodeSlice(t, tok)

	if _, ok := fields["font_ligatures"]; !ok {
		t.Fatalf("the key this build has no field for was dropped: %s", tok)
	}
	if string(fields["font_ligatures"]) != "false" {
		t.Fatalf("the preserved value changed: %s", fields["font_ligatures"])
	}
	// And our own change did land, or the preservation came at the cost of the edit.
	var ts bool
	if err := json.Unmarshal(fields["timestamps"], &ts); err != nil || ts {
		t.Fatalf("the change this client made did not survive: %s", fields["timestamps"])
	}
	// Exactly one copy of each key. Counting on the base64 text would count nothing, so count
	// on the decoded document that decodeSlice already produced: the first version of this
	// spliced foreign keys into a document that already contained them, producing a duplicate
	// that a JSON decoder silently resolves by keeping the last.
	for _, k := range []string{"timestamps", "highlights", "font_ligatures"} {
		raw, err := base64.StdEncoding.DecodeString(tok)
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(string(raw), `"`+k+`":`); n != 1 {
			t.Fatalf("%q appears %d times in the decoded token: %s", k, n, raw)
		}
	}
}

// TestAForeignKeyOfAnyShapeRoundTripsByteForByte: an object, a list and a number are all values a
// future client might store. Reformatting one is changing it.
func TestAForeignKeyOfAnyShapeRoundTripsByteForByte(t *testing.T) {
	blob := []byte(`{"timestamps":true,` +
		`"future_object":{"a":1,"b":[2,3],"c":{"d":"e"}},` +
		`"future_list":[1,2,3],` +
		`"future_number":42,` +
		`"future_string":"hello world"}`)

	s := decodeSettings(blob)
	tok, err := s.token()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	fields := decodeSlice(t, tok)
	for k, want := range map[string]string{
		"future_object": `{"a":1,"b":[2,3],"c":{"d":"e"}}`,
		"future_list":   `[1,2,3]`,
		"future_number": `42`,
		"future_string": `"hello world"`,
	} {
		if got := string(fields[k]); got != want {
			t.Fatalf("%s round-tripped as %s, want %s", k, got, want)
		}
	}
}

// TestAnUnknownKeySurvivesAFileRoundTripToo: the file is the offline copy, and a client with no
// bouncer is exactly where someone upgrades builds. The same preservation has to hold there, or
// running without a bouncer is the one configuration that eats a newer client's settings.
func TestAnUnknownKeySurvivesAFileRoundTripToo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	blob := `{"timestamps":true,"font_ligatures":false}`
	if err := os.WriteFile(path, []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}

	s := LoadSettings(path)
	s.Timestamps = false
	if err := SaveSettings(path, s); err != nil {
		t.Fatalf("save: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("saved file is not JSON: %v\n%s", err, data)
	}
	if _, ok := fields["font_ligatures"]; !ok {
		t.Fatalf("the file round-trip dropped the unknown key: %s", data)
	}
	// Saved with indentation and a trailing newline, as the file always was.
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("the file lost its trailing newline: %q", data)
	}
}

// TestTwoClientsWithTheSameSettingsWriteTheSameBytes.
//
// The document is shared, so writes to it race in a way a single client's file never did: two
// clients that each preserve the same foreign keys must produce the same bytes, or every save
// looks like a change to whatever compares them (a sync tool, a diff, a test). Map iteration
// order is random in Go, so without an explicit sort the order of the preserved keys varies run
// to run even for one client.
func TestTwoClientsWithTheSameSettingsWriteTheSameBytes(t *testing.T) {
	blob := []byte(`{"timestamps":true,` +
		`"zzz_last":{"k":1},` +
		`"aaa_first":[1,2],` +
		`"mmm_middle":7,` +
		`"beta_thing":"x",` +
		`"alpha_thing":"y"}`)

	first, err := decodeSettings(blob).token()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		next, err := decodeSettings(blob).token()
		if err != nil {
			t.Fatal(err)
		}
		if next != first {
			t.Fatalf("run %d produced different bytes; the preserved keys are in map order, not sorted", i+1)
		}
	}
	// And the order is ascending, so it is not merely stable but predictable.
	raw, _ := base64.StdEncoding.DecodeString(first)
	s := string(raw)
	idx := func(k string) int { return strings.Index(s, `"`+k+`":`) }
	if !(idx("aaa_first") < idx("alpha_thing") && idx("alpha_thing") < idx("beta_thing") &&
		idx("beta_thing") < idx("mmm_middle") && idx("mmm_middle") < idx("zzz_last")) {
		t.Fatalf("the preserved keys are not in sorted order: %s", s)
	}
}

// TestAnUnexportedFieldIsNotOnTheWire.
//
// knownKeys derives the wire keys from the struct tags, and the settings struct has one field
// whose job is to hold keys this build does not own. If that field were published it would be
// nested inside the document it is a remainder of, growing the blob a little on every save -
// a leak that only shows up as a settings blob slowly approaching its size limit.
func TestAnUnexportedFieldIsNotOnTheWire(t *testing.T) {
	blob := []byte(`{"timestamps":true,"future_key":1}`)
	s := decodeSettings(blob)
	if len(s.unknown) == 0 {
		t.Fatalf("the fixture did not produce a foreign key")
	}
	tok, err := s.token()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(tok)
	if strings.Contains(string(raw), `"unknown"`) {
		t.Fatalf("the preserved-keys field was published into the document: %s", raw)
	}
	// And it is not counted as a known key either, or it would be dropped from preservation.
	if knownKeys()["unknown"] {
		t.Fatalf("knownKeys lists the unexported field")
	}
}

// TestAKnownKeyIsNotTreatedAsForeign: the negative control. If knownKeys were wrong in the other
// direction - treating our own keys as foreign - every save would duplicate them, and the test
// above would still pass because the key it checks is genuinely foreign.
func TestAKnownKeyIsNotTreatedAsForeign(t *testing.T) {
	known := knownKeys()
	for _, k := range []string{"timestamps", "seconds", "date_rule", "nick_colors", "hotlist",
		"scroll_step", "aliases", "mutes", "highlights", "group_by_server", "buffer_numbers"} {
		if !known[k] {
			t.Fatalf("%q is a field of Settings but knownKeys does not list it", k)
		}
	}
	// And the unexported field is not on the wire: it has no tag, and publishing it would put
	// the preserved blob inside the blob.
	if known["unknown"] {
		t.Fatalf("the unexported unknown field is listed as a wire key")
	}
}

// TestADefaultSettingsHasNoForeignKeys: a fresh client must not invent keys, or its first save
// would fill the shared document with entries no client owns.
func TestADefaultSettingsHasNoForeignKeys(t *testing.T) {
	s := DefaultSettings()
	tok, err := s.token()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	fields := decodeSlice(t, tok)
	if len(fields) != len(knownKeys()) {
		t.Fatalf("a default document has %d keys and the struct has %d", len(fields), len(knownKeys()))
	}
	for k := range fields {
		if !knownKeys()[k] {
			t.Fatalf("a default document contains a key no field owns: %q", k)
		}
	}
}

// TestAnOlderDocumentStillGetsDefaults: decoding over defaults is what makes a document from a
// previous build usable. Preserving foreign keys must not have replaced that with "keep whatever
// was in the file", which would leave a setting added last week unset for everyone who upgraded.
func TestAnOlderDocumentStillGetsDefaults(t *testing.T) {
	s := decodeSettings([]byte(`{"timestamps":false}`))
	if s.Timestamps {
		t.Fatalf("the document's own value was overridden by the default")
	}
	if !s.NickColors || !s.Hotlist || s.ScrollStep != DefaultSettings().ScrollStep {
		t.Fatalf("keys missing from an older document did not get their defaults: %+v", s)
	}
}
