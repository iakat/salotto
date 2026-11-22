package facade

import (
	"bufio"
	"net"
	"strings"
	"testing"

	"github.com/iakat/salotto/internal/bindhost"
)

func testBindConfig(t *testing.T, user string, deny bool) (*fakeNet, Config, *bindhost.State) {
	t.Helper()
	fn := newFakeNet("libera")
	st := bindhost.New(user, deny)
	cfg := testConfig(fn)
	cfg.BindHosts = st
	return fn, cfg, st
}

// runCommand sends one client command after registration and returns the single
// status NOTICE that answers it.
func runCommand(t *testing.T, conn net.Conn, br *bufio.Reader, line string) string {
	t.Helper()
	sendLines(t, conn, line)
	return readClientLine(t, conn, br)
}

func newBindClient(t *testing.T, cfg Config) (net.Conn, *bufio.Reader) {
	t.Helper()
	_, addr := startServer(t, cfg)
	conn, br := dialClient(t, addr)
	register(t, conn, br, "test")
	collectReplay(t, conn, br)
	return conn, br
}

func TestShowBindHost(t *testing.T) {
	tests := []struct {
		name    string
		user    string
		network string
		want    string
	}{
		{name: "kernel", user: "", network: "", want: "bind_host= (kernel)"},
		{name: "user", user: "198.51.100.7", network: "", want: "bind_host=198.51.100.7 (user)"},
		{name: "network", user: "198.51.100.7", network: "203.0.113.9", want: "bind_host=203.0.113.9 (network)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, cfg, st := testBindConfig(t, tt.user, false)
			if tt.network != "" {
				if err := st.SetNetwork("libera", tt.network); err != nil {
					t.Fatalf("SetNetwork: %v", err)
				}
			}
			conn, br := newBindClient(t, cfg)
			got := runCommand(t, conn, br, "ShowBindHost")
			if !strings.Contains(got, tt.want) {
				t.Fatalf("ShowBindHost = %q, want it to contain %q", got, tt.want)
			}
		})
	}
}

func TestSetAndClearBindHost(t *testing.T) {
	_, cfg, st := testBindConfig(t, "198.51.100.7", false)
	conn, br := newBindClient(t, cfg)

	if got := runCommand(t, conn, br, "SetBindHost 203.0.113.9"); !strings.Contains(got, "203.0.113.9") {
		t.Fatalf("SetBindHost reply = %q", got)
	}
	host, origin := st.Resolve("libera")
	if host != "203.0.113.9" || origin != "network" {
		t.Fatalf("after SetBindHost = (%q, %q)", host, origin)
	}

	if got := runCommand(t, conn, br, "ClearBindHost"); !strings.Contains(got, "cleared") {
		t.Fatalf("ClearBindHost reply = %q", got)
	}
	host, origin = st.Resolve("libera")
	if host != "198.51.100.7" || origin != "user" {
		t.Fatalf("after ClearBindHost = (%q, %q), want the user fallback", host, origin)
	}
}

func TestSetUserBindHost(t *testing.T) {
	_, cfg, st := testBindConfig(t, "", false)
	conn, br := newBindClient(t, cfg)

	if got := runCommand(t, conn, br, "SetUserBindHost 198.51.100.7"); !strings.Contains(got, "198.51.100.7") {
		t.Fatalf("SetUserBindHost reply = %q", got)
	}
	if st.User() != "198.51.100.7" {
		t.Fatalf("user value = %q", st.User())
	}

	if got := runCommand(t, conn, br, "ClearUserBindHost"); !strings.Contains(got, "cleared") {
		t.Fatalf("ClearUserBindHost reply = %q", got)
	}
	if st.User() != "" {
		t.Fatalf("user value after clear = %q", st.User())
	}
}

func TestSetBindHostEqualToUserIsNoop(t *testing.T) {
	_, cfg, st := testBindConfig(t, "198.51.100.7", false)
	conn, br := newBindClient(t, cfg)

	got := runCommand(t, conn, br, "SetBindHost 198.51.100.7")
	if !strings.Contains(got, "already have this bind host") {
		t.Fatalf("SetBindHost equal to user = %q, want the no-op warning", got)
	}
	if st.Network("libera") != "" {
		t.Fatalf("network override = %q, want it left empty", st.Network("libera"))
	}
}

func TestBindHostDenied(t *testing.T) {
	_, cfg, st := testBindConfig(t, "198.51.100.7", true)
	conn, br := newBindClient(t, cfg)

	for _, cmd := range []string{"SetBindHost 203.0.113.9", "SetUserBindHost 203.0.113.9"} {
		got := runCommand(t, conn, br, cmd)
		if !strings.Contains(got, "deny_set_bind_host") {
			t.Fatalf("%s reply = %q, want it to name the policy", cmd, got)
		}
	}
	if st.User() != "198.51.100.7" || st.Network("libera") != "" {
		t.Fatalf("state changed despite denial: user=%q net=%q", st.User(), st.Network("libera"))
	}
}

func TestSetBindHostValidation(t *testing.T) {
	_, cfg, st := testBindConfig(t, "198.51.100.7", false)
	conn, br := newBindClient(t, cfg)

	got := runCommand(t, conn, br, "SetBindHost 198.51.100.7:6667")
	if !strings.Contains(got, "bare IP") {
		t.Fatalf("SetBindHost with port = %q, want a validation error", got)
	}
	if st.Network("libera") != "" {
		t.Fatalf("invalid value changed state: %q", st.Network("libera"))
	}

	got = runCommand(t, conn, br, "SetBindHost example.com")
	if !strings.Contains(got, "bare IP") {
		t.Fatalf("SetBindHost with hostname = %q, want a validation error", got)
	}
}
