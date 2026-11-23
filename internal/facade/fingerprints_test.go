package facade

import (
	"strings"
	"testing"

	"github.com/iakat/salotto/internal/certhash"
)

const sampleFP = "E5:BE:6C:C0:E6:F1:06:3A:69:04:51:9F:E6:5C:D7:46:D8:AF:9D:E5:C8:D2:C6:20:17:E3:1F:A3:A7:3E:3C:EE"

// TestTrustedFingerprintCommands: B14's three wire commands, with ZNC's exact
// responses (ClientCommand.cpp:918-960) and its normalisation rule.
func TestTrustedFingerprintCommands(t *testing.T) {
	s, fn, sub, out := newWindowFixture(t)
	_ = fn

	got := notices(runWindowCmd(t, s, sub, "LISTTRUSTEDSERVERFINGERPRINTS"))
	if strings.Join(got, "\n") != "No fingerprints added." {
		t.Fatalf("empty list = %q, want the ZNC notice", got)
	}

	got = notices(runWindowCmd(t, s, sub, "ADDTRUSTEDSERVERFINGERPRINT"))
	if strings.Join(got, "\n") != "Usage: AddTrustedServerFingerprint <fi:ng:er>" {
		t.Fatalf("usage = %q", got)
	}

	got = notices(runWindowCmd(t, s, sub, "ADDTRUSTEDSERVERFINGERPRINT not-a-fingerprint"))
	if !strings.Contains(strings.Join(got, "\n"), "Not a SHA-256 fingerprint") {
		t.Fatalf("garbage pin = %q, want a refusal", got)
	}
	if len(s.ListTrustedServerFingerprints("libera")) != 0 {
		t.Fatal("garbage was stored as a pin")
	}

	// Lowercase, no separators: still the same fingerprint, as ZNC's parser
	// would treat it.
	bare := strings.ToLower(strings.ReplaceAll(sampleFP, ":", ""))
	got = notices(runWindowCmd(t, s, sub, "ADDTRUSTEDSERVERFINGERPRINT "+bare))
	if strings.Join(got, "\n") != "Done." {
		t.Fatalf("add = %q, want Done.", got)
	}
	pins := s.ListTrustedServerFingerprints("libera")
	if len(pins) != 1 || pins[0] != sampleFP {
		t.Fatalf("stored pins = %v, want the canonical %s", pins, sampleFP)
	}

	got = notices(runWindowCmd(t, s, sub, "LISTTRUSTEDSERVERFINGERPRINTS"))
	if strings.Join(got, "\n") != "1. "+sampleFP {
		t.Fatalf("list = %q, want the numbered fingerprint", got)
	}

	_ = out
	got = notices(runWindowCmd(t, s, sub, "DELTRUSTEDSERVERFINGERPRINT "+sampleFP))
	if strings.Join(got, "\n") != "Done." {
		t.Fatalf("del = %q, want Done.", got)
	}
	if len(s.ListTrustedServerFingerprints("libera")) != 0 {
		t.Fatal("pin survived Del")
	}

	got = notices(runWindowCmd(t, s, sub, "DELTRUSTEDSERVERFINGERPRINT "+sampleFP))
	if !strings.Contains(strings.Join(got, "\n"), "No such fingerprint") {
		t.Fatalf("del of an unknown pin = %q", got)
	}
}

// TestFingerprintNormalisation pins the canonical form: the same digest written
// three ways must compare equal, because a pin that only matches its own
// spelling is a pin that silently never applies.
func TestFingerprintNormalisation(t *testing.T) {
	upper := sampleFP
	lower := strings.ToLower(sampleFP)
	bare := strings.ReplaceAll(upper, ":", "")
	spaced := strings.ReplaceAll(bare, "", " ")

	for _, in := range []string{upper, lower, bare, spaced} {
		if got := certhash.Normalize(in); got != upper {
			t.Fatalf("Normalize(%q) = %q, want %q", in, got, upper)
		}
	}
	for _, bad := range []string{"", "abc", "zz:zz", sampleFP + ":00"} {
		if got := certhash.Normalize(bad); got != "" {
			t.Fatalf("Normalize(%q) = %q, want refusal", bad, got)
		}
	}
}

// TestFingerprintsForgetNetwork: a removed network must not leave pins behind,
// for the same reason it must not leave cursors behind (B5).
func TestFingerprintsForgetNetwork(t *testing.T) {
	f := NewFingerprints()
	if !f.Add("libera", sampleFP) {
		t.Fatal("Add refused a valid fingerprint")
	}
	if !f.Trusted("libera", sampleFP) {
		t.Fatal("a just-added pin is not trusted")
	}
	f.ForgetNetwork("libera")
	if f.Trusted("libera", sampleFP) {
		t.Fatal("pin survived ForgetNetwork")
	}
}
