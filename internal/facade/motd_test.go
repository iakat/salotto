package facade

import (
	"strings"
	"testing"
)

// newClientForMOTD registers one client over a real socket and returns every
// line the server sent, from the handshake through the MOTD block. It goes over
// the wire rather than calling sendMOTD directly because the ordering of 001..005
// against 375/372/376 is exactly what a client depends on.
func newClientForMOTD(t *testing.T, blocked, missing bool) []string {
	t.Helper()
	cfg := testConfig()
	cfg.MOTDBlocked = blocked
	cfg.MOTDMissing = missing
	_, addr := startServer(t, cfg)

	conn, br := dialClient(t, addr)
	sendLines(t, conn, "CAP LS 302")
	sendLines(t, conn, "CAP END")
	sendLines(t, conn, "NICK tester", "USER tester 0 * :Tester")

	var lines []string
	for {
		line := readClientLine(t, conn, br)
		lines = append(lines, line)
		// 376, 422 end the MOTD; anything else could follow, so stop there.
		if strings.Contains(line, " 376 ") || strings.Contains(line, " 422 ") {
			break
		}
	}
	return lines
}

// TestMOTDPolicy pins B15 against the two ZNC modules it ports.
//
// block_motd HALTs 375 and 372 and rewrites 376 to "MOTD blocked by ZNC";
// missingmotd answers login with 422. The difference is invisible until a client
// tries to parse the MOTD block: a client that sees 372 without a 375 renders
// garbage outside the block, which is why both are asserted.
func TestMOTDPolicy(t *testing.T) {
	tests := []struct {
		name       string
		blocked    bool
		missing    bool
		wantCodes  []string
		wantSubstr string
	}{
		{
			name:       "default serves the bouncer MOTD",
			wantCodes:  []string{"375", "372", "376"},
			wantSubstr: "replayed history follows",
		},
		{
			name:       "blocked suppresses the block and relabels 376",
			blocked:    true,
			wantCodes:  []string{"376"},
			wantSubstr: "MOTD blocked by ZNC",
		},
		{
			name:       "missing answers 422",
			missing:    true,
			wantCodes:  []string{"422"},
			wantSubstr: "MOTD File is missing",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lines := newClientForMOTD(t, tc.blocked, tc.missing)
			codes := make([]string, 0, len(lines))
			for _, line := range lines {
				fields := strings.Fields(line)
				for _, f := range fields {
					switch f {
					case "375", "372", "376", "422":
						codes = append(codes, f)
					}
				}
			}
			if strings.Join(codes, ",") != strings.Join(tc.wantCodes, ",") {
				t.Fatalf("MOTD numerics = %v, want %v (lines: %q)", codes, tc.wantCodes, lines)
			}
			if !strings.Contains(strings.Join(lines, "\n"), tc.wantSubstr) {
				t.Fatalf("output %q does not contain %q", lines, tc.wantSubstr)
			}
		})
	}
}

// TestMOTDBlockedNeverEmits372Alone is the regression guard for a rewrite that
// only relabels 376: ZNC HALTs both 375 and 372, so 372 must never reach a
// client whose operator blocked the MOTD.
func TestMOTDBlockedNeverEmits372Alone(t *testing.T) {
	lines := newClientForMOTD(t, true, false)
	for _, line := range lines {
		if strings.Contains(line, " 372 ") {
			t.Fatalf("blocked MOTD still emitted 372: %q", line)
		}
	}
}
