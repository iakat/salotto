package facade

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/iakat/salotto/internal/ircline"
)

// prefsBlobOfSize builds a settings-shaped document whose base64 token is longer than any
// command line the daemon accepts, without touching the blob cap itself.
func prefsBlobOfSize(n int) string {
	return `{"highlights":["` + strings.Repeat("z", n) + `"]}`
}

// TestALargeLegalBlobIsAcceptedWhole is the positive control for the truncation fix, and it
// has to come first: a test suite that only proves refusals would pass on a daemon that refuses
// every blob.
//
// The size is chosen to be much larger than MaxLineLen and well under the cap, which is exactly
// the range the old reader made unreachable. Round-tripping it byte for byte is the assertion
// that matters - "no error" is satisfied by a store that kept a prefix.
func TestALargeLegalBlobIsAcceptedWhole(t *testing.T) {
	cfg := testConfig(newFakeNet("libera"))
	_, addr := startServer(t, cfg)
	conn, br := dialClient(t, addr)
	register(t, conn, br, "alice")
	collectReplay(t, conn, br)

	raw := prefsBlobOfSize(20000)
	if len(raw) <= cfg.MaxLineLen {
		t.Fatalf("the fixture must be larger than the command limit (%d vs %d), or it proves nothing",
			len(raw), cfg.MaxLineLen)
	}
	token := base64.StdEncoding.EncodeToString([]byte(raw))
	sendLines(t, conn, "PREF SET "+token)

	got, ok := getPrefs(t, conn, br)
	if !ok {
		t.Fatalf("a %d-byte blob (legal, under the %d cap) was not stored", len(raw), ircline.MaxPrefsBytes)
	}
	if got != token {
		t.Fatalf("the store holds %d of %d token bytes: it kept a fragment, not the settings",
			len(got), len(token))
	}
	decoded, err := base64.StdEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("the stored token is not base64: %v", err)
	}
	if string(decoded) != raw {
		t.Fatalf("round trip changed the bytes: %d in, %d out", len(raw), len(decoded))
	}
}

// TestATruncatedPrefsLineIsRefusedNotStored covers both alignments, because they took
// different paths before this fix and only one of them was loud.
//
// MaxLineLen is set so that the token the reader can assemble is cut at a byte count that
// is, and is not, a multiple of four. A base64 string cut on a four-character boundary still
// decodes; the test that only covers the unaligned case passes on a daemon that silently
// stores a fragment in the aligned one.
func TestATruncatedPrefsLineIsRefusedNotStored(t *testing.T) {
	cases := []struct {
		name     string
		lineMax  int
		wantSame bool
	}{
		// 8193 is the alignment that used to poison the store: the assembled line is cut to
		// 8184 token bytes, a whole number of base64 quanta.
		{"truncation lands on a base64 quantum", 8193, false},
		// 8192 is the alignment that used to be refused with the wrong message.
		{"truncation lands between quanta", 8192, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(newFakeNet("libera"))
			cfg.MaxLineLen = tc.lineMax
			_, addr := startServer(t, cfg)
			conn, br := dialClient(t, addr)
			register(t, conn, br, "alice")
			collectReplay(t, conn, br)

			// A previous good blob: the refusal must leave this in place rather than half
			// replacing it, which is the failure a "refuse but store the prefix" fix leaves.
			good := base64.StdEncoding.EncodeToString([]byte(`{"timestamps":false}`))
			sendLines(t, conn, "PREF SET "+good)
			if got, ok := getPrefs(t, conn, br); !ok || got != good {
				t.Fatalf("the good blob was not stored before the oversized attempt: %q", got)
			}

			// Far over the read limit, so the line is definitely cut.
			huge := base64.StdEncoding.EncodeToString([]byte(prefsBlobOfSize(ircline.MaxPrefsBytes * 2)))
			sendLines(t, conn, "PREF SET "+huge)

			line := readClientLine(t, conn, br)
			if !strings.Contains(line, " NOTICE ") {
				t.Fatalf("an oversized blob did not produce a status NOTICE: %q", line)
			}
			if strings.Contains(strings.ToLower(line), "base64") {
				t.Fatalf("the refusal blames the encoding: %q (base64 is exactly what it was)", line)
			}

			got, ok := getPrefs(t, conn, br)
			if !ok || got != good {
				t.Fatalf("the store holds %q after a refused blob, want the previous settings %q", got, good)
			}
		})
	}
}

// TestABlobJustOverTheReadLimitIsRefused is the boundary case, and it is the one the flag
// exists for.
//
// A line far over the limit is cut in an early chunk, so a reader that forgets to set the flag
// on the *last* chunk still reports overflow. A blob a few bytes over the limit is cut only at
// the very end - which is exactly where a blob just above the cap lands, and exactly the case a
// user produces by growing their settings a little at a time. Without the flag being set there,
// the store keeps a fragment and every client reverts to the defaults.
func TestABlobJustOverTheReadLimitIsRefused(t *testing.T) {
	cfg := testConfig(newFakeNet("libera"))
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, addr := startServer(t, cfg)
	conn, br := dialClient(t, addr)
	register(t, conn, br, "alice")
	collectReplay(t, conn, br)

	// A token a few bytes longer than the reader will assemble: legal base64, refused only
	// because it does not fit.
	token := base64.StdEncoding.EncodeToString([]byte(prefsBlobOfSize(s.maxReadLine())))
	token = token[:min(len(token), s.maxReadLine()+8)]
	for len(token)%4 != 0 {
		token = token[:len(token)-1]
	}
	sendLines(t, conn, "PREF SET "+token)

	line := readClientLine(t, conn, br)
	if !strings.Contains(line, " NOTICE ") || !strings.Contains(strings.ToLower(line), "larger than this daemon accepts") {
		t.Fatalf("a blob just over the read limit was not refused by size: %q", line)
	}
	if got, ok := getPrefs(t, conn, br); ok {
		t.Fatalf("a truncated blob was stored anyway: %d bytes", len(got))
	}
}

// TestPrefsCapIsReachableThroughTheReader pins the derived relationship. The blob cap and the
// reader's limit are two numbers that must agree, and the failure when they do not is invisible:
// a cap the reader cannot reach is a limit that only exists in a comment.
func TestPrefsCapIsReachableThroughTheReader(t *testing.T) {
	cfg := testConfig(newFakeNet("libera"))
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The largest legal blob, encoded, plus the verb and the CRLF, has to fit the reader.
	token := (ircline.MaxPrefsBytes+2)/3*4 + len("PREF SET ") + 2
	if got := s.maxReadLine(); got < token {
		t.Fatalf("the reader accepts %d bytes but a legal blob needs %d: the cap is unreachable", got, token)
	}
	// And a configured command limit must not shrink it.
	cfg.MaxLineLen = 4096
	small, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := small.maxReadLine(); got < token {
		t.Fatalf("with MaxLineLen=4096 the reader accepts %d bytes, still short of the %d a legal blob needs", got, token)
	}
}
