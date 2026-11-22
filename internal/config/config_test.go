package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadString(t *testing.T, body string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "salotto.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return Load(path)
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
		check   func(t *testing.T, cfg *Config)
	}{
		{
			name: "full example",
			body: `
# global
listen = "127.0.0.1:6667"
tls_listen = ""
tls_cert = ""
tls_key = ""
server_name = "salotto"
stats_listen = "127.0.0.1:6668"
ring_segments = 8
ring_segment_size = 262144
max_clients = 32

[[network]]
name = "libera"
addr = "irc.libera.chat:6697"
tls = true
nick = "mynick"
realname = "mynick"
pass = ""
sasl_user = ""
sasl_pass = ""
channels = ["#chan1", "#chan2"]
insecure_skip_verify = false

[[network]]
name = "freebnc"
addr = "irc.freebnc.net:6697"
tls = true
nick = "other"
realname = "other"
channels = []
`,
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.Listen != "127.0.0.1:6667" || cfg.StatsListen != "127.0.0.1:6668" {
					t.Fatalf("addresses = %q / %q", cfg.Listen, cfg.StatsListen)
				}
				if cfg.ServerName != "salotto" || cfg.RingSegments != 8 || cfg.RingSegmentSize != 262144 || cfg.MaxClients != 32 {
					t.Fatalf("global defaults = %+v", cfg)
				}
				if len(cfg.Networks) != 2 {
					t.Fatalf("networks = %d, want 2", len(cfg.Networks))
				}
				n := cfg.Networks[0]
				if n.Name != "libera" || n.Addr != "irc.libera.chat:6697" || !n.TLS || n.Nick != "mynick" || n.RealName != "mynick" {
					t.Fatalf("first network = %+v", n)
				}
				if len(n.Channels) != 2 || n.Channels[0] != "#chan1" || n.Channels[1] != "#chan2" {
					t.Fatalf("channels = %v", n.Channels)
				}
				if cfg.Networks[1].Name != "freebnc" || len(cfg.Networks[1].Channels) != 0 {
					t.Fatalf("second network = %+v", cfg.Networks[1])
				}
			},
		},
		{
			name: "comments and blank lines",
			body: `
# a comment before anything

listen = "127.0.0.1:7000"   # inline comment
server_name = "test"        # another

[[network]]
# the network section
name = "libera"
addr = "irc.libera.chat:6697"
nick = "nick"
`,
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.Listen != "127.0.0.1:7000" || cfg.ServerName != "test" {
					t.Fatalf("values not parsed through comments: %+v", cfg)
				}
				if len(cfg.Networks) != 1 || cfg.Networks[0].Name != "libera" {
					t.Fatalf("networks = %+v", cfg.Networks)
				}
			},
		},
		{
			name: "unknown key",
			body: `
listen = "127.0.0.1:6667"
lisen = "typo"
[[network]]
name = "libera"
addr = "irc.libera.chat:6697"
nick = "nick"
`,
			wantErr: "unknown key",
		},
		{
			name: "missing nick",
			body: `
[[network]]
name = "libera"
addr = "irc.libera.chat:6697"
`,
			wantErr: "nick",
		},
		{
			name: "escaped quote",
			body: `
[[network]]
name = "libera"
addr = "irc.libera.chat:6697"
nick = "nick"
pass = "pa\"ss\\word"
`,
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if got := cfg.Networks[0].Pass; got != `pa"ss\word` {
					t.Fatalf("pass = %q, want %q", got, `pa"ss\word`)
				}
			},
		},
		{
			name: "string list",
			body: `
[[network]]
name = "libera"
addr = "irc.libera.chat:6697"
nick = "nick"
channels = ["#a", "#b c", "#d"]
`,
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				got := cfg.Networks[0].Channels
				want := []string{"#a", "#b c", "#d"}
				if len(got) != len(want) {
					t.Fatalf("channels = %v, want %v", got, want)
				}
				for i := range want {
					if got[i] != want[i] {
						t.Fatalf("channels = %v, want %v", got, want)
					}
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadString(t, tt.body)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Load succeeded, want error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %q, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if tt.check != nil {
				tt.check(t, cfg)
			}
		})
	}
}

func TestBindHostConfig(t *testing.T) {
	t.Run("user and network", func(t *testing.T) {
		cfg, err := loadString(t, `
[user]
bind_host = "198.51.100.7"
deny_set_bind_host = true

[[network]]
name = "libera"
addr = "irc.libera.chat:6697"
nick = "nick"
bind_host = "2001:db8::1"
`)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.User.BindHost != "198.51.100.7" {
			t.Fatalf("user bind_host = %q", cfg.User.BindHost)
		}
		if !cfg.User.DenySetBindHost {
			t.Fatal("deny_set_bind_host not parsed")
		}
		if cfg.Networks[0].BindHost != "2001:db8::1" {
			t.Fatalf("network bind_host = %q", cfg.Networks[0].BindHost)
		}
	})

	bad := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name: "user hostname",
			body: `
[user]
bind_host = "example.com"

[[network]]
name = "libera"
addr = "irc.libera.chat:6697"
nick = "nick"
`,
			wantErr: "user.bind_host",
		},
		{
			name: "user port",
			body: `
[user]
bind_host = "198.51.100.7:6667"

[[network]]
name = "libera"
addr = "irc.libera.chat:6697"
nick = "nick"
`,
			wantErr: "user.bind_host",
		},
		{
			name: "network hostname",
			body: `
[[network]]
name = "libera"
addr = "irc.libera.chat:6697"
nick = "nick"
bind_host = "example.com"
`,
			wantErr: "networks[0].bind_host",
		},
		{
			name: "network garbage",
			body: `
[[network]]
name = "libera"
addr = "irc.libera.chat:6697"
nick = "nick"
bind_host = "not-an-ip"
`,
			wantErr: "networks[0].bind_host",
		},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadString(t, tt.body)
			if err == nil {
				t.Fatalf("Load succeeded, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}

	good := []struct {
		name  string
		value string
	}{
		{name: "ipv6 loopback", value: "::1"},
		{name: "ipv6 global", value: "2001:db8::1"},
		{name: "ipv4", value: "198.51.100.7"},
	}
	for _, tt := range good {
		t.Run("accept "+tt.name, func(t *testing.T) {
			if _, err := loadString(t, `
[user]
bind_host = "`+tt.value+`"

[[network]]
name = "libera"
addr = "irc.libera.chat:6697"
nick = "nick"
`); err != nil {
				t.Fatalf("Load(%q): %v", tt.value, err)
			}
		})
	}
}

func TestResolveBindHost(t *testing.T) {
	tests := []struct {
		name, user, network, wantHost, wantOrigin string
	}{
		{name: "network wins", user: "198.51.100.7", network: "203.0.113.9", wantHost: "203.0.113.9", wantOrigin: "network"},
		{name: "user wins", user: "198.51.100.7", network: "", wantHost: "198.51.100.7", wantOrigin: "user"},
		{name: "kernel picks", user: "", network: "", wantHost: "", wantOrigin: "kernel"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, origin := ResolveBindHost(tt.user, tt.network)
			if host != tt.wantHost || origin != tt.wantOrigin {
				t.Fatalf("ResolveBindHost(%q, %q) = (%q, %q), want (%q, %q)",
					tt.user, tt.network, host, origin, tt.wantHost, tt.wantOrigin)
			}
		})
	}
}

func TestIdentConfig(t *testing.T) {
	t.Run("parsed", func(t *testing.T) {
		cfg, err := loadString(t, `
ident_listen = "127.0.0.1:10113"
ident = "salotto-1"

[[network]]
name = "libera"
addr = "irc.libera.chat:6697"
nick = "nick"
`)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.IdentListen != "127.0.0.1:10113" {
			t.Fatalf("ident_listen = %q", cfg.IdentListen)
		}
		if cfg.Ident != "salotto-1" {
			t.Fatalf("ident = %q", cfg.Ident)
		}
	})

	t.Run("empty means off", func(t *testing.T) {
		cfg, err := loadString(t, `
ident_listen = ""
ident = ""

[[network]]
name = "libera"
addr = "irc.libera.chat:6697"
nick = "nick"
`)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.IdentListen != "" || cfg.Ident != "" {
			t.Fatalf("ident fields = %q / %q, want empty", cfg.IdentListen, cfg.Ident)
		}
	})

	bad := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name: "listen without port",
			body: `
ident_listen = "127.0.0.1"
[[network]]
name = "libera"
addr = "irc.libera.chat:6697"
nick = "nick"
`,
			wantErr: "ident_listen",
		},
		{
			name: "ident with space",
			body: `
ident = "sal otto"
[[network]]
name = "libera"
addr = "irc.libera.chat:6697"
nick = "nick"
`,
			wantErr: "ident",
		},
		{
			name: "ident with colon",
			body: `
ident = "sal:otto"
[[network]]
name = "libera"
addr = "irc.libera.chat:6697"
nick = "nick"
`,
			wantErr: "ident",
		},
		{
			name: "ident too long",
			body: `
ident = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
[[network]]
name = "libera"
addr = "irc.libera.chat:6697"
nick = "nick"
`,
			wantErr: "ident",
		},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadString(t, tt.body)
			if err == nil {
				t.Fatalf("Load succeeded, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}
