package logfile

import "testing"

// TestStripControlsMatchesZNC pins the awkward cases from ZNC's
// CString::StripControls_n, which a regex gets wrong.
func TestStripControlsMatchesZNC(t *testing.T) {
	cases := []struct{ in, want, why string }{
		{"plain text", "plain text", "nothing to do"},
		{"\x0304red", "red", "four = foreground colour"},
		{"\x0304,05red", "red", "colour pair"},
		{"\x03red", "red", "bare colour code"},
		{"\x0312,3text", "text", "one-digit background"},
		// The 5 here is consumed as a background colour digit, so the whole
		// thing disappears: only a comma with NO digits after it is text.
		{"\x03,5", "", "comma then a background digit"},
		{"\x03,", ",", "comma with nothing after it is text"},
		{"\x03", "", "trailing colour code"},
		{"\x03,", ",", "trailing code with a pending comma"},
		{"\x02bold\x02", "bold", "bold is a control byte"},
		{"\x1ditalic\x1f", "italic", "italic and underline"},
		{"a\x00b", "ab", "NUL dropped"},
		{"a\x7fb", "ab", "DEL dropped"},
		{"\x1b[31mansi\x1b[0m", "[31mansi[0m", "ESC is a control byte, the rest is text"},
		{"café", "café", "high bytes are not controls, so UTF-8 survives"},
		{"\x0304,05,06", ",06", "a second comma ends the code and is text"},
	}
	for _, tc := range cases {
		if got := stripControls(tc.in); got != tc.want {
			t.Errorf("stripControls(%q) = %q, want %q (%s)", tc.in, got, tc.want, tc.why)
		}
	}
}

// TestStripControlsLeavesOrdinaryTextAlone guards the fast path: a message with
// no control bytes must come back identical, not merely equal-looking.
func TestStripControlsLeavesOrdinaryTextAlone(t *testing.T) {
	const s = "just a normal sentence, with punctuation: and commas!"
	if got := stripControls(s); got != s {
		t.Fatalf("ordinary text was modified: %q", got)
	}
	if hasControls(s) {
		t.Fatal("hasControls reported controls in ordinary text")
	}
}

// TestStripControlsRemovesEscapes is the safety property: a remote user must not
// be able to put terminal escapes into what we render or log.
func TestStripControlsRemovesEscapes(t *testing.T) {
	const evil = "\x1b[2J\x1b[1;31mowned\x1b[0m"
	got := stripControls(evil)
	for _, b := range []byte(got) {
		if b == 0x1b {
			t.Fatalf("an ESC survived: %q", got)
		}
	}
}
