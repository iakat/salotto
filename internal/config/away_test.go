package config

import "testing"

// TestAwayConfig pins the three simple_away keys and that one network asking for
// it is enough: the away state is the user's, not the network's, so the facade
// is process-wide and the getter folds the per-network settings into one answer.
func TestAwayConfig(t *testing.T) {
	cfg, err := loadString(t, `
[[network]]
name = "libera"
addr = "irc.libera.chat:6697"
nick = "nick"
away = true
away_wait = 30
away_reason = "brb, %awaytime%"

[[network]]
name = "oftc"
addr = "irc.oftc.net:6697"
nick = "nick"
away = false
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	n := cfg.Networks[0]
	if !n.Away {
		t.Fatal("away not parsed")
	}
	if n.AwayWait != 30 {
		t.Fatalf("away_wait = %d, want 30", n.AwayWait)
	}
	if n.AwayReason != "brb, %awaytime%" {
		t.Fatalf("away_reason = %q", n.AwayReason)
	}

	enabled, reason, wait := cfg.AwayEnabled()
	if !enabled {
		t.Fatal("AwayEnabled said no when one network asked for it")
	}
	if reason != "brb, %awaytime%" || wait != 30 {
		t.Fatalf("AwayEnabled = %q/%d", reason, wait)
	}
}

// TestAwayDisabledByDefault pins that the feature is opt-in: no network with
// away = true means no away, so an upgrade does not start marking people away.
func TestAwayDisabledByDefault(t *testing.T) {
	cfg, err := loadString(t, `
[[network]]
name = "libera"
addr = "irc.libera.chat:6697"
nick = "nick"
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if enabled, _, _ := cfg.AwayEnabled(); enabled {
		t.Fatal("away is on by default; it must be opt-in")
	}
}
