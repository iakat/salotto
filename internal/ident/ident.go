// Package ident implements the server side of RFC 1413 (ident). It answers
// only for TCP pairs this process actually opened: a blanket USERID reply
// would leak every socket on the host.
package ident

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultQueryTimeout = 5 * time.Second
	defaultMaxQueries   = 2
	maxRequestBytes     = 1000
)

// Pair identifies one TCP connection by the two ports that matter to identd.
type Pair struct {
	ServerPort int // remote port: the IRC server's side
	ClientPort int // local port: our side
}

// Registry maps socket pairs we opened to the ident string to report. It is
// safe for concurrent use.
type Registry struct {
	mu sync.RWMutex
	m  map[Pair]string

	// Counters, exported read-only for /stats.
	Queries atomic.Uint64
	Refused atomic.Uint64
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{m: make(map[Pair]string)}
}

// Add registers an open connection and returns the function that removes it.
// The caller must call the returned func exactly once, when the socket closes.
// Addresses that are not *net.TCPAddr are a programming error: the registration
// is dropped and a no-op remove is returned, because this runs in the dial path
// of a daemon and must not panic.
func (r *Registry) Add(local, remote net.Addr, ident string) (remove func()) {
	noop := func() {}
	if r == nil {
		return noop
	}
	la, ok := local.(*net.TCPAddr)
	if !ok {
		return noop
	}
	ra, ok := remote.(*net.TCPAddr)
	if !ok {
		return noop
	}
	p := Pair{ServerPort: ra.Port, ClientPort: la.Port}

	r.mu.Lock()
	r.m[p] = ident
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.m, p)
			r.mu.Unlock()
		})
	}
}

// Lookup returns the ident for a pair we opened, and whether we opened it.
func (r *Registry) Lookup(p Pair) (string, bool) {
	if r == nil {
		return "", false
	}
	r.mu.RLock()
	ident, ok := r.m[p]
	r.mu.RUnlock()
	return ident, ok
}

// Config configures a Server.
type Config struct {
	// Listener must be an already-bound TCP listener. The caller owns it and
	// decides whether a bind failure is fatal; for ident it is not.
	Listener net.Listener
	Registry *Registry
	Log      *slog.Logger
	// QueryTimeout is the deadline for one request. Zero means 5 seconds.
	QueryTimeout time.Duration
	// MaxQueries is the number of requests served per connection. Zero means 2.
	MaxQueries int
}

// Server answers RFC 1413 queries for the pairs in its registry.
type Server struct {
	ln         net.Listener
	reg        *Registry
	log        *slog.Logger
	timeout    time.Duration
	maxQueries int
}

// New returns a Server for cfg. The listener is not closed by New.
func New(cfg Config) (*Server, error) {
	if cfg.Listener == nil {
		return nil, errors.New("ident: Listener is required")
	}
	if cfg.Registry == nil {
		return nil, errors.New("ident: Registry is required")
	}
	s := &Server{
		ln:         cfg.Listener,
		reg:        cfg.Registry,
		log:        cfg.Log,
		timeout:    cfg.QueryTimeout,
		maxQueries: cfg.MaxQueries,
	}
	if s.timeout <= 0 {
		s.timeout = defaultQueryTimeout
	}
	if s.maxQueries <= 0 {
		s.maxQueries = defaultMaxQueries
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s, nil
}

// Addr returns the address the server listens on.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Serve accepts ident queries until ctx is done. It returns nil when the
// listener closes because of ctx, and the accept error otherwise.
func (s *Server) Serve(ctx context.Context) error {
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = s.ln.Close()
		case <-stop:
		}
	}()
	defer close(stop)

	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()

	br := bufio.NewReaderSize(conn, 1024)
	for i := 0; i < s.maxQueries; i++ {
		_ = conn.SetReadDeadline(time.Now().Add(s.timeout))
		line, err := readRequest(br, maxRequestBytes)
		if err != nil {
			if errors.Is(err, errRequestTooLong) || isTimeout(err) {
				s.reg.Queries.Add(1)
				s.refuse(conn, "ERROR : INVALID-PORT\r\n")
			}
			return
		}
		s.reg.Queries.Add(1)

		p, ok := parsePair(line)
		if !ok {
			s.refuse(conn, "ERROR : INVALID-PORT\r\n")
			return
		}
		ident, found := s.reg.Lookup(p)
		if !found {
			s.reg.Refused.Add(1)
			s.log.Debug("ident query refused")
			msg := fmt.Sprintf("%d , %d : ERROR : NO-USER\r\n", p.ServerPort, p.ClientPort)
			_ = conn.SetWriteDeadline(time.Now().Add(s.timeout))
			if _, err := io.WriteString(conn, msg); err != nil {
				return
			}
			continue
		}
		msg := fmt.Sprintf("%d , %d : USERID : UNIX : %s\r\n", p.ServerPort, p.ClientPort, ident)
		_ = conn.SetWriteDeadline(time.Now().Add(s.timeout))
		if _, err := io.WriteString(conn, msg); err != nil {
			return
		}
	}
}

func (s *Server) refuse(conn net.Conn, msg string) {
	s.reg.Refused.Add(1)
	s.log.Debug("ident query refused")
	_ = conn.SetWriteDeadline(time.Now().Add(s.timeout))
	_, _ = io.WriteString(conn, msg)
}

var errRequestTooLong = errors.New("ident: request too long")

func readRequest(br *bufio.Reader, max int) ([]byte, error) {
	line := make([]byte, 0, 64)
	for {
		b, err := br.ReadByte()
		if err != nil {
			return nil, err
		}
		if b == '\n' {
			if n := len(line); n > 0 && line[n-1] == '\r' {
				line = line[:n-1]
			}
			return line, nil
		}
		if len(line) >= max {
			return nil, errRequestTooLong
		}
		line = append(line, b)
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// parsePair strictly parses "<port> , <port>": optional spaces around the
// comma, both sides decimal 0..65535, nothing else.
func parsePair(line []byte) (Pair, bool) {
	i := 0
	port := func() (int, bool) {
		start := i
		n := 0
		for i < len(line) && line[i] >= '0' && line[i] <= '9' {
			if i-start >= 5 {
				return 0, false
			}
			n = n*10 + int(line[i]-'0')
			i++
		}
		if i == start || n > 65535 {
			return 0, false
		}
		return n, true
	}

	sp, ok := port()
	if !ok {
		return Pair{}, false
	}
	for i < len(line) && line[i] == ' ' {
		i++
	}
	if i >= len(line) || line[i] != ',' {
		return Pair{}, false
	}
	i++
	for i < len(line) && line[i] == ' ' {
		i++
	}
	cp, ok := port()
	if !ok {
		return Pair{}, false
	}
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	if i != len(line) {
		return Pair{}, false
	}
	return Pair{ServerPort: sp, ClientPort: cp}, true
}
