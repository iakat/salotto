package upstream

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iakat/salotto/internal/logfile"
	"github.com/iakat/salotto/internal/winring"
)

// Config describes one upstream network.
type Config struct {
	Name               string
	Addr               string
	TLS                bool
	InsecureSkipVerify bool
	Nick               string
	User               string
	RealName           string
	Pass               string
	SASLUser           string
	SASLPass           string
	// SASLExternal selects SASL EXTERNAL (ZNC's module sasl.cpp, mechanism
	// EXTERNAL: "TLS certificate, for use with the *cert module") instead of
	// PLAIN. It sends no credentials: the identity is the client certificate, so
	// a server that does not see one refuses the mechanism and the connection
	// fails closed rather than silently falling back to PLAIN.
	SASLExternal bool
	// CertFile and KeyFile are the client certificate presented on the TLS
	// handshake, which is what SASL EXTERNAL authenticates. Both or neither.
	CertFile string
	KeyFile  string
	// Channels is the static channel list used when JoinChannels is nil. It is
	// kept for tests and for callers that have no runtime state to consult.
	Channels []string
	// JoinChannels, when non-nil, is called at every (re)connect instead of
	// reading Channels: it returns the channels to join right now. The daemon
	// supplies it so DisableChan can suppress the rejoin of a disabled channel,
	// which is the one thing ZNC's disabled flag actually does
	// (IRCNetwork.cpp:998 checks IsDisabled before JoinChan; CClient::AddBuffer
	// never does, so a disabled channel keeps buffering and replaying).
	JoinChannels func() []string
	// Perform, when non-nil, is asked for the lines to send immediately after
	// registration and before the joins, which is ZNC's perform module
	// (perform.cpp: OnIRCConnected → PutIRC for each stored line). It runs on
	// every connect, so a line that was wrong last time is retried.
	Perform func() []string
	// AltNick is the nick to try when the primary one is in use (433), which is
	// ZNC's IRCNetwork Nick/AltNick pair. Empty means accept whatever the server
	// assigned.
	AltNick string
	// KeepNick enables ZNC's keepnick module: retry the configured nick every
	// 30 s while the server has us under a different one.
	KeepNick bool
	// TrustFingerprint, when non-nil, is consulted only after the normal PKI
	// verification fails: ZNC's CheckSSLCert (Socket.cpp) accepts a certificate
	// whose SHA-256 fingerprint the operator pinned, so a self-signed server can
	// be used without InsecureSkipVerify and a wrong pin still fails closed.
	// It receives the colon-separated uppercase fingerprint.
	TrustFingerprint func(fp string) bool
	// Windows owns one replay ring per window for this network. The connection
	// routes every line to the windows the logger gives it and appends the line
	// to each of those rings.
	Windows *winring.Set
	// Route parses line once and returns the log entries it belongs to, in the
	// routing order the logger uses; outbound tells it how the line arrived. It
	// must not retain line. An empty result means the line is not buffered (a
	// CAP reply or a suppressed echo).
	Route func(line []byte, outbound bool) []logfile.Entry
	// OnLine is called once for every non-PING line the connection reads, with
	// the window list, the per-window sequence each append returned, and the
	// entries routing already parsed. The window list is empty for a line that
	// is not buffered (a CAP reply, a suppressed own echo); the callback still
	// sees it so the live status stream stays complete. The line slice is only
	// valid for the duration of the call: it aliases an internal read buffer,
	// so copy it if it is retained.
	OnLine func(windows []string, seqs []uint64, entries []logfile.Entry, line []byte)
	// OnSend is called for every line sent to the network via Send.
	OnSend func(line []byte)
	// OnConnect is called after successful registration.
	OnConnect func()
	// OnDisconnect is called when the connection drops.
	OnDisconnect func()
	// OnOpen, when non-nil, is called with the freshly dialled connection,
	// before registration is attempted. Its return value is called exactly once
	// when the connection closes or the dial attempt fails afterwards. Used by
	// the ident registry; it must be cheap and must not block.
	OnOpen func(conn net.Conn) (onClose func())
	Log    *slog.Logger
	Dialer *net.Dialer
	// BindHost, when non-empty, is the local source address the outbound socket
	// binds to. The dialer enables IP_FREEBIND for it, so the address does not
	// have to be configured on an interface.
	BindHost   string
	BackoffMin time.Duration
	BackoffMax time.Duration
}

// store routes line to its replay windows and appends it to each window's ring.
// One line shared by several windows is stored in each ring, with independent
// per-window sequences. It reports whether the line was buffered. OnLine fires
// even for an unbuffered line, with an empty window list.
func (c *Conn) store(line []byte, outbound bool) bool {
	if c.cfg.Windows == nil || c.cfg.Route == nil {
		return false
	}
	entries := c.cfg.Route(line, outbound)
	ts := time.Now().UnixNano()
	if cap(c.windows) < len(entries) {
		c.windows = make([]string, len(entries))
		c.seqs = make([]uint64, len(entries))
	}
	windows := c.windows[:len(entries)]
	seqs := c.seqs[:len(entries)]
	for i, e := range entries {
		window := e.Window
		if window == "" {
			window = "status"
		}
		windows[i] = window
		seqs[i] = c.cfg.Windows.Append(window, line, ts)
	}
	if c.cfg.OnLine != nil {
		c.cfg.OnLine(windows, seqs, entries, line)
	}
	return len(entries) > 0
}

// State is the current connection state.
type State int32

const (
	StateConnecting State = iota
	StateRegistered
	StateBackoff
	StateStopped
)

func (s State) String() string {
	switch s {
	case StateConnecting:
		return "connecting"
	case StateRegistered:
		return "registered"
	case StateBackoff:
		return "backoff"
	case StateStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

var (
	ErrClosed        = errors.New("upstream: closed")
	ErrNotRegistered = errors.New("upstream: not registered")
	ErrSendTimeout   = errors.New("upstream: send queue full")
	ErrLineTooLong   = errors.New("upstream: line too long")
)

// Conn is one managed upstream connection. It reconnects forever.
type Conn struct {
	cfg    Config
	log    *slog.Logger
	dialer *net.Dialer

	dialerMu   sync.Mutex
	baseDialer *net.Dialer

	state    atomic.Int32
	attempts atomic.Uint64
	linesIn  atomic.Uint64
	linesOut atomic.Uint64
	// bytesIn/bytesOut are the raw byte counters TRAFFIC reports.
	bytesIn  atomic.Uint64
	bytesOut atomic.Uint64

	mu   sync.Mutex
	regC chan struct{}

	// selfNick is the nick the server gave us, read by keepnick and by anything
	// that must tell our own messages apart.
	selfNick atomic.Value // string
	// selfMask is "nick!ident@host" for our own session, learned from the first
	// line that carries our own prefix. ZNC knows the same thing from the nick
	// it builds out of the messages it reads, and LISTNETWORKS prints it.
	selfMask atomic.Value // string

	capsMu sync.RWMutex
	caps   map[string]bool

	// activeServer is the address of the established connection, which is what
	// LISTSERVERS marks with an asterisk rather than guessing from the rotation.
	activeServer atomic.Value // string

	// servers is the ordered server list ADDSERVER/DELSERVER mutate, and the
	// rotation a failed connect walks. It is seeded from cfg.Addr so a
	// single-address config behaves as it always did.
	servers *serverList

	// enabled is ZNC's m_bIRCConnectEnabled: false means the operator asked the
	// bouncer to stay off IRC. The reconnect loop waits on poke rather than
	// spinning while it is false, and Disconnect closes the live socket so a
	// connected network goes down immediately.
	enabled atomic.Bool
	poke    chan struct{}

	// liveMu guards live, the socket of the current attempt, so Disconnect can
	// close exactly that socket and not a later one.
	liveMu sync.Mutex
	live   net.Conn

	freeSlabs  chan *slab
	writeSlabs chan *slab

	// windows and seqs are reused by store. Routing and appendSynthetic both
	// run on the connection's own goroutine, so no lock is needed.
	windows []string
	seqs    []uint64
}

// New validates cfg and returns a connection that has not dialed yet.
func New(cfg Config) (*Conn, error) {
	if cfg.Name == "" {
		return nil, errors.New("upstream: Name is required")
	}
	if cfg.Addr == "" {
		return nil, errors.New("upstream: Addr is required")
	}
	if _, _, err := net.SplitHostPort(cfg.Addr); err != nil {
		return nil, errors.New("upstream: invalid Addr: " + err.Error())
	}
	if cfg.Nick == "" {
		return nil, errors.New("upstream: Nick is required")
	}
	if cfg.BindHost != "" && net.ParseIP(cfg.BindHost) == nil {
		return nil, errors.New("upstream: invalid BindHost: not an IP address")
	}
	if cfg.Windows == nil {
		return nil, errors.New("upstream: Windows is required")
	}
	if cfg.User == "" {
		cfg.User = cfg.Nick
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Dialer == nil {
		cfg.Dialer = &net.Dialer{}
	}
	baseDialer := cfg.Dialer
	cfg.Dialer = dialerWithBindHost(baseDialer, cfg.BindHost)
	if cfg.BackoffMin <= 0 {
		cfg.BackoffMin = time.Second
	}
	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = 60 * time.Second
	}
	if cfg.BackoffMax < cfg.BackoffMin {
		cfg.BackoffMax = cfg.BackoffMin
	}

	c := &Conn{
		cfg:        cfg,
		log:        cfg.Log,
		dialer:     cfg.Dialer,
		baseDialer: baseDialer,
		regC:       make(chan struct{}),
		caps:       make(map[string]bool),
		freeSlabs:  make(chan *slab, slabCount),
		writeSlabs: make(chan *slab, slabCount),
		servers:    newServerList(cfg.Addr),
	}
	c.state.Store(int32(StateConnecting))
	c.enabled.Store(true)
	c.poke = make(chan struct{}, 1)
	for i := 0; i < slabCount; i++ {
		c.freeSlabs <- &slab{}
	}
	return c, nil
}

// SetBindHost repoints the next dial. The value is only used at dial time, so a
// running connection is not interrupted; ZNC does the same (CIRCNetwork.cpp
// 1545 accepts the value and only reads it in Connect).
func (c *Conn) SetBindHost(host string) error {
	if host != "" && net.ParseIP(host) == nil {
		return errors.New("upstream: invalid BindHost: not an IP address")
	}
	d := dialerWithBindHost(c.baseDialer, host)

	c.dialerMu.Lock()
	c.cfg.BindHost = host
	c.dialer = d
	c.dialerMu.Unlock()
	return nil
}

// BindHost returns the bind host the next dial will use.
func (c *Conn) BindHost() string {
	c.dialerMu.Lock()
	defer c.dialerMu.Unlock()
	return c.cfg.BindHost
}

func (c *Conn) dialerForDial() *net.Dialer {
	c.dialerMu.Lock()
	d := c.dialer
	c.dialerMu.Unlock()
	return d
}

func (c *Conn) State() State     { return State(c.state.Load()) }
func (c *Conn) Attempts() uint64 { return c.attempts.Load() }
func (c *Conn) LinesIn() uint64  { return c.linesIn.Load() }
func (c *Conn) LinesOut() uint64 { return c.linesOut.Load() }

func (c *Conn) HasCap(name string) bool {
	c.capsMu.RLock()
	defer c.capsMu.RUnlock()
	return c.caps[strings.ToLower(name)]
}

func (c *Conn) SetCaps(caps []string) {
	m := make(map[string]bool, len(caps))
	for _, cap := range caps {
		name := cap
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		m[strings.ToLower(name)] = true
	}
	c.capsMu.Lock()
	c.caps = m
	c.capsMu.Unlock()
}

// NickMask is "nick!ident@host" for the current session, or the bare nick when
// no line carrying our prefix has been seen yet.
func (c *Conn) NickMask() string {
	if v, ok := c.selfMask.Load().(string); ok && v != "" {
		return v
	}
	return c.CurrentNick()
}

// ConnectEnabled reports whether the bouncer may hold an upstream connection.
func (c *Conn) ConnectEnabled() bool { return c.enabled.Load() }

// SetConnectEnabled turns the upstream on or off. ZNC's Disconnect sets
// m_bIRCConnectEnabled to false and CONNECT sets it back; the reconnect loop
// consults it before every dial, which is why disabling has to interrupt the
// current attempt as well as the backoff (IRCNetwork.cpp SetIRCConnectEnabled).
func (c *Conn) SetConnectEnabled(enabled bool) {
	was := c.enabled.Swap(enabled)
	if enabled {
		if !was {
			c.pokeRun()
		}
		return
	}
	if was {
		c.closeLive()
		c.pokeRun()
	}
}

// pokeRun wakes the reconnect loop so it re-reads enabled without waiting out
// the backoff.
func (c *Conn) pokeRun() {
	select {
	case c.poke <- struct{}{}:
	default:
	}
}

// closeLive closes the socket of the attempt in flight, if any.
func (c *Conn) closeLive() {
	c.liveMu.Lock()
	conn := c.live
	c.liveMu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (c *Conn) setLive(conn net.Conn) {
	c.liveMu.Lock()
	c.live = conn
	c.liveMu.Unlock()
}

// Server is the address of the server we are connected to, else the configured
// one, in ZNC's display form.
func (c *Conn) Server() string { return c.ServerString() }

// HasPassword reports whether a PASS is configured. The value is never exposed.
func (c *Conn) HasPassword() bool { return c.cfg.Pass != "" }

// Traffic is bytes received and sent on the socket, summed over every attempt.
// ZNC counts the same thing per user (CUser::m_uBytesIn/m_uBytesOut in
// ClientCommand.cpp:1575's TRAFFIC handler).
func (c *Conn) Traffic() (in, out uint64) {
	return c.bytesIn.Load(), c.bytesOut.Load()
}

func (c *Conn) ServerString() string {
	// Report the server actually in use — the live one, else the next the
	// rotation would try — rather than always the configured address, or a
	// network that failed over would keep claiming the dead host.
	addr := c.liveServer()
	if addr == "" {
		addr = c.servers.Current()
	}
	if addr == "" {
		addr = c.cfg.Addr
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if c.cfg.TLS {
		return host + " +" + port
	}
	return host + " " + port
}

// Registered returns a channel that is closed when the current (or next)
// registration succeeds. A fresh channel is installed after each success.
func (c *Conn) Registered() <-chan struct{} {
	c.mu.Lock()
	ch := c.regC
	c.mu.Unlock()
	return ch
}

func (c *Conn) markRegistered() {
	c.mu.Lock()
	old := c.regC
	c.regC = make(chan struct{})
	close(old)
	c.mu.Unlock()
}

func (c *Conn) setState(s State) {
	c.state.Store(int32(s))
}

// Send queues one raw IRC line (without CRLF) for the network.
func (c *Conn) Send(line []byte) error {
	switch c.State() {
	case StateStopped:
		return ErrClosed
	case StateRegistered:
	default:
		return ErrNotRegistered
	}
	if c.cfg.OnSend != nil {
		c.cfg.OnSend(line)
	}
	return c.enqueue(line)
}

// Run blocks until ctx is done, reconnecting forever.
func (c *Conn) Run(ctx context.Context) {
	defer c.setState(StateStopped)

	backoff := c.cfg.BackoffMin
	for {
		if ctx.Err() != nil {
			return
		}
		if !c.enabled.Load() {
			c.setState(StateStopped)
			select {
			case <-ctx.Done():
				return
			case <-c.poke:
				continue
			}
		}
		c.setState(StateConnecting)
		registered, err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}

		c.setState(StateBackoff)
		if err != nil {
			c.log.Info("upstream disconnected",
				"name", c.cfg.Name,
				"attempt", c.Attempts(),
				"err", err,
			)
		}
		if registered {
			backoff = c.cfg.BackoffMin
		}

		poked, ok := sleepBackoff(ctx, c.poke, backoff)
		if !ok {
			return
		}
		backoff = nextBackoff(backoff, poked, c.cfg.BackoffMin, c.cfg.BackoffMax)
	}
}

// saslWanted reports whether the handshake should request the sasl capability.
// EXTERNAL needs a client certificate, not a username, which is why it is a
// separate condition rather than "SASLUser != \"\"".
func (c *Config) saslWanted() bool {
	if c.SASLExternal {
		return true
	}
	return c.SASLUser != ""
}

// AddServer appends a server to this network's list and reports whether it was
// added, which is ZNC's IRCNetwork::AddServer (ClientCommand.cpp:843). The
// address is a salotto "host:port"; ZNC's "+port means SSL" spelling is handled
// by the command layer, not here.
func (c *Conn) AddServer(addr string) bool { return c.servers.Add(addr) }

// DelServer removes a server. ZNC answers "No such server" when it was not
// there (ClientCommand.cpp:876).
func (c *Conn) DelServer(addr string) bool { return c.servers.Del(addr) }

// Servers returns the configured servers in order.
func (c *Conn) Servers() []string { return c.servers.Addrs() }

// liveServer is the address of the connection actually established, tracked in
// conn.go so LISTSERVERS can mark it rather than guessing from the rotation.
func (c *Conn) liveServer() string {
	if v := c.activeServer.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// setLiveServer records which server the current connection went to.
func (c *Conn) setLiveServer(addr string) { c.activeServer.Store(addr) }

// nextBackoff is the reconnect delay ladder: double on an ordinary failure, up to
// the ceiling, but drop back to the floor when the wait was cut short by a poke —
// somebody asked for this connection, and answering a request to connect now with
// a longer delay is the opposite of what was asked. ZNC restarts the delay on an
// explicit connect (CIRCNetwork::SetIRCConnectEnabled).
//
// It is a function rather than three inline lines so the ladder can be tested as
// the ladder; inline, a test of sleepBackoff alone would pass while Run still
// doubled after a poke, which is precisely the bug this guards.
func nextBackoff(cur time.Duration, poked bool, min, max time.Duration) time.Duration {
	if poked {
		return min
	}
	cur *= 2
	if max > 0 && cur > max {
		cur = max
	}
	return cur
}
