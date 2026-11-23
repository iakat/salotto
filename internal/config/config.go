package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

const (
	// These match the clamps applied by internal/ring.New.
	minRingSegments    = 2
	minRingSegmentSize = 512
)

// User describes the single local user of the proof-of-concept process.
type User struct {
	BindHost        string
	DenySetBindHost bool
}

// Network describes one upstream IRC network.
type Network struct {
	Name     string
	Addr     string
	TLS      bool
	Nick     string
	RealName string
	Pass     string
	SASLUser string
	SASLPass string
	// SASLExternal selects SASL EXTERNAL: the identity is the client
	// certificate, not a password pair.
	SASLExternal bool
	// CertFile/KeyFile are the client certificate presented to the network.
	CertFile string
	KeyFile  string
	// AltNick is tried when Nick is in use (ZNC's Nick/AltNick pair).
	AltNick string
	// KeepNick retries Nick every 30 s while the server has us under another
	// one (ZNC's keepnick module).
	KeepNick bool
	// Perform are IRC lines sent on every connect, before the joins (ZNC's
	// perform module).
	Perform []string
	// ClearBufferOnMsg clears every channel and query buffer as soon as the user
	// speaks (ZNC's clearbufferonmsg).
	ClearBufferOnMsg bool
	// Away enables ZNC's simple_away: away on detach, back on attach.
	Away bool
	// AwayReason is the message set; %awaytime% expands to when it was set.
	AwayReason string
	// AwayWait is how long to wait after the last client detaches. Zero keeps
	// simple_away's 60 s default.
	AwayWait           int
	Channels           []string
	BindHost           string
	InsecureSkipVerify bool
}

// Config is the complete salottod configuration.
type Config struct {
	Listen          string
	TLSListen       string
	TLSCert         string
	TLSKey          string
	ServerName      string
	StatsListen     string
	LogDir          string
	LogQueue        int
	LogListen       string
	LogToken        string
	IdentListen     string
	Ident           string
	RingSegments    int
	RingSegmentSize int
	MaxWindows      int
	MaxClients      int
	// MOTDBlocked and MOTDMissing are the B15 knobs: ZNC's block_motd and
	// missingmotd modules, which are per-user settings there but process-wide
	// here because this daemon is single-user.
	MOTDBlocked bool
	MOTDMissing bool
	User        User
	Networks    []Network
	// MaxBufferLines caps SETBUFFER, ZNC's GetMaxBufferSize.
	MaxBufferLines int

	// StripControls mirrors ZNC's stripcontrols module: message text is written
	// to the logs without IRC colours and control bytes. Off by default, as the
	// module is in ZNC — the log module writes what was said.
	StripControls bool
}

// Default returns the built-in configuration. Networks are intentionally empty:
// a valid configuration must add at least one [[network]] section.
func Default() *Config {
	return &Config{
		Listen:          "127.0.0.1:6667",
		ServerName:      "salotto",
		StatsListen:     "127.0.0.1:6668",
		LogQueue:        4096,
		RingSegments:    8,
		RingSegmentSize: 262144,
		MaxWindows:      64,
		MaxClients:      32,
	}
}

// Load reads path, parses it over Default and validates the result.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := Default()
	if err := cfg.parse(string(data)); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// ResolveBindHost returns the address the dial must bind to and where it came
// from: the network value if set, else the user value, else "" (kernel picks).
// Same precedence as ZNC's CIRCNetwork::GetBindHost().
func ResolveBindHost(user, network string) (host, origin string) {
	switch {
	case network != "":
		return network, "network"
	case user != "":
		return user, "user"
	default:
		return "", "kernel"
	}
}

// ValidateIdentListen accepts empty (ident off) or a host:port literal.
func ValidateIdentListen(value string) error {
	if value == "" {
		return nil
	}
	if _, _, err := net.SplitHostPort(value); err != nil {
		return fmt.Errorf("ident_listen must be host:port, got %q", value)
	}
	return nil
}

// ValidateIdent accepts empty (the network's nick is used) or a single token
// made of [A-Za-z0-9._-], at most 32 bytes. A space or colon in an ident would
// corrupt the single-line RFC 1413 reply.
func ValidateIdent(value string) error {
	if value == "" {
		return nil
	}
	if len(value) > 32 {
		return fmt.Errorf("ident must be at most 32 characters, got %d", len(value))
	}
	for i := 0; i < len(value); i++ {
		ch := value[i]
		ok := ch >= 'A' && ch <= 'Z' ||
			ch >= 'a' && ch <= 'z' ||
			ch >= '0' && ch <= '9' ||
			ch == '.' || ch == '_' || ch == '-'
		if !ok {
			return fmt.Errorf("ident may only contain [A-Za-z0-9._-], got %q", value)
		}
	}
	return nil
}

// ValidateBindHost rejects anything but a bare IPv4 or IPv6 literal: a bind
// host with a port or a hostname would silently bind to something else than the
// operator wrote.
func ValidateBindHost(field, value string) error {
	if value == "" {
		return nil
	}
	if net.ParseIP(value) == nil {
		return fmt.Errorf("%s must be a bare IP address, got %q", field, value)
	}
	return nil
}

// Validate checks the parts of the configuration that cannot be left empty or
// below the ring's own hard limits.
func (c *Config) Validate() error {
	if len(c.Networks) == 0 {
		return errors.New("at least one [[network]] is required")
	}
	if strings.TrimSpace(c.Listen) == "" {
		return errors.New("listen must not be empty")
	}
	if err := ValidateBindHost("user.bind_host", c.User.BindHost); err != nil {
		return err
	}
	if err := ValidateIdentListen(c.IdentListen); err != nil {
		return err
	}
	if err := ValidateIdent(c.Ident); err != nil {
		return err
	}
	if c.RingSegments < minRingSegments {
		return fmt.Errorf("ring_segments must be at least %d", minRingSegments)
	}
	if c.RingSegmentSize < minRingSegmentSize {
		return fmt.Errorf("ring_segment_size must be at least %d", minRingSegmentSize)
	}
	if c.MaxWindows <= 0 {
		c.MaxWindows = 64
	}
	if c.LogQueue <= 0 {
		c.LogQueue = 4096
	}
	if c.TLSListen != "" && c.TLSCert == "" {
		return errors.New("tls_cert is required when tls_listen is set")
	}
	for i := range c.Networks {
		n := &c.Networks[i]
		if strings.TrimSpace(n.Name) == "" {
			return fmt.Errorf("network %d: name must not be empty", i+1)
		}
		if strings.TrimSpace(n.Addr) == "" {
			return fmt.Errorf("network %q: addr must not be empty", n.Name)
		}
		if strings.TrimSpace(n.Nick) == "" {
			return fmt.Errorf("network %q: nick must not be empty", n.Name)
		}
		if err := ValidateBindHost(fmt.Sprintf("networks[%d].bind_host", i), n.BindHost); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) parse(text string) error {
	const (
		secGlobal  = ""
		secUser    = "user"
		secNetwork = "network"
	)
	section := secGlobal
	for lineNo, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if strings.HasPrefix(line, "[[") {
				if !strings.HasSuffix(line, "]]") {
					return fmt.Errorf("line %d: malformed section header %q", lineNo+1, line)
				}
				name := strings.TrimSpace(line[2 : len(line)-2])
				if name != secNetwork {
					return fmt.Errorf("line %d: unknown section [[%s]]", lineNo+1, name)
				}
				c.Networks = append(c.Networks, Network{})
				section = secNetwork
				continue
			}
			if !strings.HasSuffix(line, "]") {
				return fmt.Errorf("line %d: malformed section header %q", lineNo+1, line)
			}
			name := strings.TrimSpace(line[1 : len(line)-1])
			if name != secUser {
				return fmt.Errorf("line %d: unknown section [%s]", lineNo+1, name)
			}
			section = secUser
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return fmt.Errorf("line %d: expected key = value", lineNo+1)
		}
		key := strings.TrimSpace(line[:eq])
		value := strings.TrimSpace(line[eq+1:])
		if key == "" {
			return fmt.Errorf("line %d: empty key", lineNo+1)
		}

		var err error
		switch section {
		case secNetwork:
			err = c.parseNetworkKey(&c.Networks[len(c.Networks)-1], key, value)
		case secUser:
			err = c.parseUserKey(key, value)
		default:
			err = c.parseGlobalKey(key, value)
		}
		if err != nil {
			return fmt.Errorf("line %d: %w", lineNo+1, err)
		}
	}
	return nil
}

func (c *Config) parseGlobalKey(key, value string) error {
	var err error
	switch key {
	case "listen":
		c.Listen, err = parseString(value)
	case "tls_listen":
		c.TLSListen, err = parseString(value)
	case "tls_cert":
		c.TLSCert, err = parseString(value)
	case "tls_key":
		c.TLSKey, err = parseString(value)
	case "server_name":
		c.ServerName, err = parseString(value)
	case "stats_listen":
		c.StatsListen, err = parseString(value)
	case "log_dir":
		c.LogDir, err = parseString(value)
	case "log_queue":
		c.LogQueue, err = parseInt(value)
	case "log_listen":
		c.LogListen, err = parseString(value)
	case "log_token":
		c.LogToken, err = parseString(value)
	case "ident_listen":
		c.IdentListen, err = parseString(value)
	case "ident":
		c.Ident, err = parseString(value)
	case "ring_segments":
		c.RingSegments, err = parseInt(value)
	case "ring_segment_size":
		c.RingSegmentSize, err = parseInt(value)
	case "max_windows":
		c.MaxWindows, err = parseInt(value)
	case "max_clients":
		c.MaxClients, err = parseInt(value)
	case "motd_blocked":
		c.MOTDBlocked, err = parseBool(value)
	case "motd_missing":
		c.MOTDMissing, err = parseBool(value)
	default:
		return fmt.Errorf("unknown key %q", key)
	}
	return err
}

func (c *Config) parseUserKey(key, value string) error {
	var err error
	switch key {
	case "bind_host":
		c.User.BindHost, err = parseString(value)
	case "deny_set_bind_host":
		c.User.DenySetBindHost, err = parseBool(value)
	default:
		return fmt.Errorf("unknown key %q in [user]", key)
	}
	return err
}

func (c *Config) parseNetworkKey(n *Network, key, value string) error {
	var err error
	switch key {
	case "name":
		n.Name, err = parseString(value)
	case "addr":
		n.Addr, err = parseString(value)
	case "tls":
		n.TLS, err = parseBool(value)
	case "nick":
		n.Nick, err = parseString(value)
	case "realname":
		n.RealName, err = parseString(value)
	case "pass":
		n.Pass, err = parseString(value)
	case "sasl_user":
		n.SASLUser, err = parseString(value)
	case "altnick":
		n.AltNick, err = parseString(value)
	case "keepnick":
		n.KeepNick, err = parseBool(value)
	case "perform":
		// perform = ["PRIVMSG NickServ :IDENTIFY hunter2", ...] — sent on every
		// connect, before the joins.
		n.Perform, err = parseStringList(value)
	case "clearbufferonmsg":
		n.ClearBufferOnMsg, err = parseBool(value)
	case "max_buffer_lines":
		c.MaxBufferLines, err = parseInt(value)
	case "strip_controls":
		c.StripControls, err = parseBool(value)
	case "away":
		n.Away, err = parseBool(value)
	case "away_reason":
		n.AwayReason, err = parseString(value)
	case "away_wait":
		n.AwayWait, err = parseInt(value)
	case "sasl_external":
		n.SASLExternal, err = parseBool(value)
	case "cert_file":
		n.CertFile, err = parseString(value)
	case "key_file":
		n.KeyFile, err = parseString(value)
	case "sasl_pass":
		n.SASLPass, err = parseString(value)
	case "channels":
		n.Channels, err = parseStringList(value)
	case "bind_host":
		n.BindHost, err = parseString(value)
	case "insecure_skip_verify":
		n.InsecureSkipVerify, err = parseBool(value)
	default:
		return fmt.Errorf("unknown key %q in [[network]]", key)
	}
	return err
}

func stripComment(line string) string {
	inQuote := false
	escaped := false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if escaped {
			escaped = false
			continue
		}
		if inQuote {
			switch ch {
			case '\\':
				escaped = true
			case '"':
				inQuote = false
			}
			continue
		}
		switch ch {
		case '"':
			inQuote = true
		case '#':
			return line[:i]
		}
	}
	return line
}

func parseString(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if value[0] == '"' {
		s, rest, err := readQuoted(value)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(rest) != "" {
			return "", errors.New("unexpected trailing characters after quoted string")
		}
		return s, nil
	}
	if value[0] == '[' {
		return "", errors.New("expected a string, got a list")
	}
	if strings.ContainsAny(value, " \t") {
		return "", errors.New("unquoted value contains whitespace")
	}
	return value, nil
}

func parseStringList(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value[0] != '[' {
		return nil, errors.New("expected a list of quoted strings")
	}

	var out []string
	i := 1
	for {
		for i < len(value) && (value[i] == ' ' || value[i] == '\t' || value[i] == ',') {
			i++
		}
		if i >= len(value) {
			return nil, errors.New("unterminated list")
		}
		if value[i] == ']' {
			i++
			break
		}
		if value[i] != '"' {
			return nil, fmt.Errorf("list element %d is not a quoted string", len(out)+1)
		}
		s, rest, err := readQuoted(value[i:])
		if err != nil {
			return nil, err
		}
		out = append(out, s)
		i += len(value[i:]) - len(rest)
	}
	if strings.TrimSpace(value[i:]) != "" {
		return nil, errors.New("unexpected trailing characters after list")
	}
	return out, nil
}

func readQuoted(s string) (value, rest string, err error) {
	if len(s) == 0 || s[0] != '"' {
		return "", "", errors.New("expected a quoted string")
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		ch := s[i]
		switch ch {
		case '\\':
			if i+1 >= len(s) {
				return "", "", errors.New("dangling escape in quoted string")
			}
			i++
			switch s[i] {
			case '"', '\\':
				b.WriteByte(s[i])
			default:
				b.WriteByte('\\')
				b.WriteByte(s[i])
			}
		case '"':
			return b.String(), s[i+1:], nil
		default:
			b.WriteByte(ch)
		}
	}
	return "", "", errors.New("unterminated quoted string")
}

func parseBool(value string) (bool, error) {
	switch strings.TrimSpace(value) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("expected true or false, got %q", strings.TrimSpace(value))
	}
}

func parseInt(value string) (int, error) {
	v, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("expected an integer, got %q", strings.TrimSpace(value))
	}
	return v, nil
}

// AwayEnabled reports whether any network asked for simple_away, and returns the
// reason and wait to use. The facade is process-wide and single-user, so the
// first non-empty settings win: a per-network away reason would be an odd thing
// to have, since the away state is the *user's*, not the network's.
func (c *Config) AwayEnabled() (enabled bool, reason string, waitSeconds int) {
	for i := range c.Networks {
		n := &c.Networks[i]
		if !n.Away {
			continue
		}
		if !enabled {
			enabled = true
			reason = n.AwayReason
			waitSeconds = n.AwayWait
			continue
		}
		if reason == "" {
			reason = n.AwayReason
		}
		if waitSeconds == 0 {
			waitSeconds = n.AwayWait
		}
	}
	return enabled, reason, waitSeconds
}

// ClearBufferOnMsg reports whether any network asked for ZNC's
// clearbufferonmsg behaviour. The facade is process-wide and single-user, so one
// network asking for it is enough.
func (c *Config) ClearBufferOnMsg() bool {
	for _, n := range c.Networks {
		if n.ClearBufferOnMsg {
			return true
		}
	}
	return false
}
