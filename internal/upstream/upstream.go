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

	"github.com/iakat/salotto/internal/ring"
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
	Channels           []string
	Ring               *ring.Ring
	// OnLine is called for every line stored in the ring. The line slice is
	// only valid for the duration of the call: it aliases an internal read
	// buffer, so copy it if it is retained.
	OnLine func(seq uint64, line []byte)
	// OnSend is called for every line sent to the network via Send.
	OnSend func(line []byte)
	// OnConnect is called after successful registration.
	OnConnect func()
	// OnDisconnect is called when the connection drops.
	OnDisconnect func()
	Log          *slog.Logger
	Dialer       *net.Dialer
	// BindHost, when non-empty, is the local source address the outbound socket
	// binds to. The dialer enables IP_FREEBIND for it, so the address does not
	// have to be configured on an interface.
	BindHost   string
	BackoffMin time.Duration
	BackoffMax time.Duration
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

	mu   sync.Mutex
	regC chan struct{}

	capsMu sync.RWMutex
	caps   map[string]bool

	freeSlabs  chan *slab
	writeSlabs chan *slab
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
	if cfg.Ring == nil {
		return nil, errors.New("upstream: Ring is required")
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
	}
	c.state.Store(int32(StateConnecting))
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

func (c *Conn) ServerString() string {
	host, port, err := net.SplitHostPort(c.cfg.Addr)
	if err != nil {
		return c.cfg.Addr
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

		if !sleepContext(ctx, backoff) {
			return
		}
		backoff *= 2
		if backoff > c.cfg.BackoffMax {
			backoff = c.cfg.BackoffMax
		}
	}
}
