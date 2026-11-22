package bindhost

import (
	"strings"
	"testing"
)

func TestResolutionOrder(t *testing.T) {
	s := New("198.51.100.7", false)

	host, origin := s.Resolve("libera")
	if host != "198.51.100.7" || origin != "user" {
		t.Fatalf("user fallback = (%q, %q), want user", host, origin)
	}

	if err := s.SetNetwork("libera", "203.0.113.9"); err != nil {
		t.Fatalf("SetNetwork: %v", err)
	}
	host, origin = s.Resolve("libera")
	if host != "203.0.113.9" || origin != "network" {
		t.Fatalf("network override = (%q, %q), want network wins", host, origin)
	}

	empty := New("", false)
	host, origin = empty.Resolve("libera")
	if host != "" || origin != "kernel" {
		t.Fatalf("kernel = (%q, %q), want empty/kernel", host, origin)
	}
}

func TestClearFallsBack(t *testing.T) {
	s := New("198.51.100.7", false)
	if err := s.SetUser("198.51.100.7"); err != nil {
		t.Fatalf("SetUser: %v", err)
	}
	if err := s.SetNetwork("libera", "203.0.113.9"); err != nil {
		t.Fatalf("SetNetwork: %v", err)
	}
	if err := s.ClearNetwork("libera"); err != nil {
		t.Fatalf("ClearNetwork: %v", err)
	}
	host, origin := s.Resolve("libera")
	if host != "198.51.100.7" || origin != "user" {
		t.Fatalf("after clear = (%q, %q), want user fallback", host, origin)
	}
}

func TestSetNetworkEqualToUserClears(t *testing.T) {
	s := New("198.51.100.7", false)
	if err := s.SetNetwork("libera", "198.51.100.7"); err != nil {
		t.Fatalf("SetNetwork: %v", err)
	}
	if got := s.Network("libera"); got != "" {
		t.Fatalf("network value = %q, want cleared because it equals the user value", got)
	}
}

func TestDeniedRefused(t *testing.T) {
	s := New("198.51.100.7", true)

	for name, err := range map[string]error{
		"SetUser":    s.SetUser("203.0.113.9"),
		"ClearUser":  s.ClearUser(),
		"SetNetwork": s.SetNetwork("libera", "203.0.113.9"),
		"ClearNet":   s.ClearNetwork("libera"),
	} {
		if !IsDenied(err) {
			t.Fatalf("%s = %v, want the deny policy error", name, err)
		}
		if !strings.Contains(err.Error(), "deny_set_bind_host") {
			t.Fatalf("%s error = %q, want it to name the policy", name, err)
		}
	}

	if s.User() != "198.51.100.7" || s.Network("libera") != "" {
		t.Fatalf("state changed despite the deny policy: user=%q net=%q", s.User(), s.Network("libera"))
	}
}

func TestValidation(t *testing.T) {
	s := New("", false)
	for _, bad := range []string{"example.com", "198.51.100.7:6667", "not-an-ip", "1.2.3"} {
		if err := s.SetNetwork("libera", bad); err == nil {
			t.Fatalf("SetNetwork(%q) succeeded, want validation error", bad)
		}
		if err := s.SetUser(bad); err == nil {
			t.Fatalf("SetUser(%q) succeeded, want validation error", bad)
		}
	}
	for _, good := range []string{"::1", "2001:db8::1", "198.51.100.7"} {
		if err := s.SetUser(good); err != nil {
			t.Fatalf("SetUser(%q): %v", good, err)
		}
	}
}

func TestOnChange(t *testing.T) {
	s := New("", false)
	var gotNetwork, gotHost string
	s.SetOnChange(func(network, host string) {
		gotNetwork, gotHost = network, host
	})
	if err := s.SetNetwork("libera", "203.0.113.9"); err != nil {
		t.Fatalf("SetNetwork: %v", err)
	}
	if gotNetwork != "libera" || gotHost != "203.0.113.9" {
		t.Fatalf("onChange = (%q, %q)", gotNetwork, gotHost)
	}
}
