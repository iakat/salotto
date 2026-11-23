package upstream

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/iakat/salotto/internal/certhash"
	"github.com/iakat/salotto/internal/ircline"
)

const (
	dialTimeout = 30 * time.Second
	maxLine     = 8192
	// ipv6Freebind is IPV6_FREEBIND. syscall does not define it, and the value
	// is stable on Linux (include/uapi/linux/in6.h).
	ipv6Freebind = 0x4e
)

// dialerWithBindHost returns a dialer that binds the outbound socket to host and
// enables IP_FREEBIND before bind, so the address does not have to be local.
// base is never mutated: the result is a copy with its own Control.
func dialerWithBindHost(base *net.Dialer, host string) *net.Dialer {
	if host == "" {
		return base
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return base
	}

	d := *base
	d.LocalAddr = &net.TCPAddr{IP: ip}

	prev := d.Control
	d.Control = func(network, address string, c syscall.RawConn) error {
		if prev != nil {
			if err := prev(network, address, c); err != nil {
				return err
			}
		}
		var serr error
		if err := c.Control(func(fd uintptr) {
			if ip.To4() != nil {
				serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_FREEBIND, 1)
			} else {
				serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, ipv6Freebind, 1)
			}
		}); err != nil {
			return err
		}
		return serr
	}
	return &d
}

func (c *Conn) runOnce(ctx context.Context) (registered bool, err error) {
	attempt := c.attempts.Add(1)

	// Walk the server list rather than always dialling cfg.Addr: with more than
	// one server configured, a dead entry must not be a dead network
	// (IRCNetwork's server rotation, ClientCommand.cpp:820-885).
	addr := c.servers.Next()
	if addr == "" {
		return false, errors.New("upstream: no servers configured")
	}
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	raw, err := c.dialerForDial().DialContext(dialCtx, "tcp", addr)
	if err != nil {
		cancel()
		if bindHost := c.BindHost(); bindHost != "" {
			timeout := errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded)
			if timeout || errors.Is(err, syscall.ENETUNREACH) {
				c.log.Warn("dial failed with a bind host set; the address may be bound but unroutable",
					"name", c.cfg.Name,
					"bind_host", bindHost,
					"err", err,
				)
			}
		}
		return false, err
	}

	onClose := func() {}
	if c.cfg.OnOpen != nil {
		onClose = c.cfg.OnOpen(raw)
		if onClose == nil {
			onClose = func() {}
		}
	}
	var closeOnce sync.Once
	defer func() { closeOnce.Do(onClose) }()

	c.setLive(raw)
	c.setLiveServer(addr)
	defer func() {
		c.setLiveServer("")
		c.setLive(nil)
	}()

	conn := raw
	if c.cfg.TLS {
		host, _, _ := net.SplitHostPort(addr)
		tlsCfg := &tls.Config{
			ServerName:         host,
			InsecureSkipVerify: c.cfg.InsecureSkipVerify,
		}
		if c.cfg.CertFile != "" && c.cfg.KeyFile != "" {
			cert, err := tls.LoadX509KeyPair(c.cfg.CertFile, c.cfg.KeyFile)
			if err != nil {
				cancel()
				raw.Close()
				return false, fmt.Errorf("upstream: %s client certificate: %w", c.cfg.Name, err)
			}
			tlsCfg.Certificates = []tls.Certificate{cert}
		}
		if c.cfg.TrustFingerprint != nil && !c.cfg.InsecureSkipVerify {
			// Verify the chain ourselves so a pinned fingerprint can rescue a
			// certificate the PKI rejects, without ever trusting an unverified
			// chain: ZNC's CheckSSLCert accepts on PKI or on an exact pin, and
			// refuses otherwise.
			tlsCfg.InsecureSkipVerify = true
			tlsCfg.VerifyConnection = func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return fmt.Errorf("upstream: %s presented no certificate", c.cfg.Name)
				}
				cert := cs.PeerCertificates[0]
				pool := x509.NewCertPool()
				for _, c := range cs.PeerCertificates[1:] {
					pool.AddCert(c)
				}
				opts := x509.VerifyOptions{DNSName: host, Roots: nil, Intermediates: pool}
				_, verifyErr := cert.Verify(opts)
				if verifyErr == nil {
					return nil
				}
				fp := certhash.Fingerprint(cert)
				if c.cfg.TrustFingerprint(fp) {
					if c.cfg.Log != nil {
						c.cfg.Log.Info("tls: accepting pinned certificate",
							"name", c.cfg.Name, "fingerprint", fp, "pki_error", verifyErr.Error())
					}
					return nil
				}
				return fmt.Errorf("upstream: %s certificate not trusted: %w", c.cfg.Name, verifyErr)
			}
		}
		tconn := tls.Client(raw, tlsCfg)
		if err := tconn.HandshakeContext(dialCtx); err != nil {
			cancel()
			raw.Close()
			return false, err
		}
		conn = tconn
	}
	cancel()

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	defer close(done)
	defer conn.Close()

	if attempt > 1 {
		c.appendSynthetic(attempt)
	}

	if c.cfg.Pass != "" {
		if err := c.writeLine(conn, []byte("PASS "+c.cfg.Pass)); err != nil {
			return false, err
		}
	}

	br := bufio.NewReaderSize(conn, 64*1024)
	scratch := make([]byte, maxLine)
	if err := c.register(conn, br, scratch); err != nil {
		return false, err
	}

	stop := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		c.writeLoop(conn, stop)
	}()

	c.setState(StateRegistered)

	if c.cfg.OnConnect != nil {
		c.cfg.OnConnect()
	}

	// Perform lines go first, before the joins: they are the commands that make
	// the session what the user expects (identify, set modes), and a JOIN that
	// races an IDENTIFY is the classic ordering bug. ZNC runs perform on
	// OnIRCConnected, which fires on 001 just like this block.
	if c.cfg.Perform != nil {
		for _, line := range c.cfg.Perform() {
			if err := c.enqueue([]byte(line)); err != nil {
				close(stop)
				conn.Close()
				<-writerDone
				c.drainWrites()
				return true, err
			}
		}
	}

	join := c.cfg.Channels
	if c.cfg.JoinChannels != nil {
		join = c.cfg.JoinChannels()
	}
	for _, ch := range join {
		if err := c.enqueue([]byte("JOIN " + ch)); err != nil {
			close(stop)
			conn.Close()
			<-writerDone
			c.drainWrites()
			return true, err
		}
	}
	c.markRegistered()

	keepStop := make(chan struct{})
	keepDone := make(chan struct{})
	go func() {
		defer close(keepDone)
		c.keepNickLoop(keepStop)
	}()

	err = c.readLoop(br, scratch)
	close(keepStop)
	<-keepDone

	if c.cfg.OnDisconnect != nil {
		c.cfg.OnDisconnect()
	}

	close(stop)
	conn.Close()
	<-writerDone
	c.drainWrites()
	return true, err
}

func (c *Conn) appendSynthetic(attempt uint64) {
	line := []byte(fmt.Sprintf(
		"NOTICE * :*** salotto reconnecting to %s (attempt %d)",
		c.cfg.Name, attempt,
	))
	c.store(line, false)
}

func (c *Conn) answerPing(conn net.Conn, rest []byte) error {
	params := ircline.Params(rest)
	line := make([]byte, 0, 5+len(params))
	line = append(line, "PONG "...)
	line = append(line, params...)
	return c.writeLine(conn, line)
}

func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

// sleepBackoff waits out the backoff but returns early when the run loop is
// poked: a CONNECT while we are backing off must dial at once, not up to a
// minute later.
func sleepBackoff(ctx context.Context, poke <-chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-poke:
		return true
	case <-timer.C:
		return true
	}
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
