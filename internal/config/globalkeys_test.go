package config

import (
	"os"
	"path/filepath"
	"testing"
)

// minNetwork is the smallest [[network]] that satisfies Validate. Every case in
// this file needs one, because the key under test has to sit at the top of the
// file where global keys live.
const minNetwork = "\n[[network]]\nname = \"n\"\naddr = \"h:1\"\nnick = \"x\"\n"

// parseSrc writes src to a temp file and loads it, which goes through the same
// path the daemon uses: file, then parse, then validate. Calling the key parser
// directly would miss the whole class of bug this file exists to catch.
func parseSrc(t *testing.T, src string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "salotto.toml")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

// TestGlobalKeysAreReachable is a regression test for a whole class of bug I made
// three times: a key is added to the wrong key switch, so the config file is
// rejected with "unknown key" even though the field and the parser case both
// exist. It is invisible to a unit test that calls the parser directly, and it
// only shows up when a real daemon reads a real file — which is exactly how
// `strip_controls` and `max_buffer_lines` were found to be dead on arrival.
//
// Every root-level key in the example config must parse from the top of the file,
// above any [[network]] section.
func TestGlobalKeysAreReachable(t *testing.T) {
	// One key at a time, at the top of the file, where global keys live.
	globals := map[string]string{
		"listen":             `"127.0.0.1:0"`,
		"server_name":        `"salotto"`,
		"stats_listen":       `""`,
		"log_dir":            `""`,
		"log_queue":          `16`,
		"log_retention_days": `90`,
		"max_windows":        `8`,
		"max_clients":        `4`,
		"max_buffer_lines":   `500`,
		"strip_controls":     `true`,
		"motd_blocked":       `false`,
		"motd_missing":       `false`,
		"ring_segments":      `4`,
		"ring_segment_size":  `1024`,
	}
	// A network is required by Validate, so every case carries a minimal one; the
	// key under test stays at the top of the file, where global keys live.
	for key, val := range globals {
		cfg, err := parseSrc(t, key+" = "+val+minNetwork)
		if err != nil {
			t.Errorf("%s is not reachable as a global key: %v", key, err)
			continue
		}
		if cfg == nil {
			t.Errorf("%s parsed to a nil config", key)
		}
	}

	// And they must not parse inside a [[network]] section: a global key accepted
	// there would silently do nothing useful, which is the mirror-image bug.
	for _, key := range []string{"max_buffer_lines", "strip_controls"} {
		src := minNetwork + key + " = 1\n"
		if _, err := parseSrc(t, src); err == nil {
			t.Errorf("%s was accepted inside [[network]]; it is a global key", key)
		}
	}
}

// TestStripControlsRoundTrip keeps the value honest: a bool that parses but is
// never stored would pass the reachability test above and still do nothing.
func TestStripControlsRoundTrip(t *testing.T) {
	cfg, err := parseSrc(t, "server_name = \"s\"\nlisten = \"127.0.0.1:0\"\nstrip_controls = true\nmax_buffer_lines = 750\n"+minNetwork)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.StripControls {
		t.Fatal("strip_controls parsed but was not stored")
	}
	if cfg.MaxBufferLines != 750 {
		t.Fatalf("max_buffer_lines = %d, want 750", cfg.MaxBufferLines)
	}
}
