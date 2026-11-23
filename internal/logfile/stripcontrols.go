package logfile

import "strings"

// stripControls removes IRC colour and control codes, reproducing ZNC's
// CString::StripControls_n (src/ZNCString.cpp:1452-1498) rather than an
// approximated regex, because the two disagree on the awkward cases.
//
// The colour code is \x03([0-9]{1,2}(,[0-9]{1,2})?)? — at most two digits for
// the foreground, an optional comma, at most two for the background. Two
// details only the original gets right:
//
//   - a comma not followed by digits is *kept* ("\x03,5" -> ",5"), which is what
//     the digits == 0 && comma branch exists for;
//   - a colour code at the very end of the line still emits a trailing comma if
//     one was pending.
//
// Everything below 0x20 and 0x7F is dropped outright.
//
// Why it matters beyond cosmetics: control bytes in a message body travel from a
// remote user into our log files and into a terminal. ESC sequences that survive
// into a TUI can move the cursor, rewrite the screen, or reach a terminal's
// clipboard handling, so stripping is a safety property and not a preference —
// which is why it lives here rather than in a renderer.
func stripControls(s string) string {
	if !hasControls(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	colorCode := false
	digits := 0
	comma := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == 0x03 {
			colorCode = true
			digits = 0
			comma = false
			continue
		}
		if colorCode {
			if ch >= '0' && ch <= '9' && digits < 2 {
				digits++
				continue
			}
			if ch == ',' && !comma {
				comma = true
				digits = 0
				continue
			}
			colorCode = false
			if digits == 0 && comma {
				// A comma that was not followed by digits is text, not syntax.
				b.WriteByte(',')
			}
		}
		if ch < 0x20 || ch == 0x7F {
			continue
		}
		b.WriteByte(ch)
	}
	if colorCode && digits == 0 && comma {
		b.WriteByte(',')
	}
	return b.String()
}

// hasControls reports whether the string contains anything stripControls would
// remove, so the common case — the overwhelming majority of messages — costs one
// pass and no allocation.
func hasControls(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c == 0x03 || c < 0x20 || c == 0x7F {
			return true
		}
	}
	return false
}
