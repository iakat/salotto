package upstream

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

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

	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	raw, err := c.dialerForDial().DialContext(dialCtx, "tcp", c.cfg.Addr)
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

	conn := raw
	if c.cfg.TLS {
		host, _, _ := net.SplitHostPort(c.cfg.Addr)
		tconn := tls.Client(raw, &tls.Config{
			ServerName:         host,
			InsecureSkipVerify: c.cfg.InsecureSkipVerify,
		})
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

	for _, ch := range c.cfg.Channels {
		if err := c.enqueue([]byte("JOIN " + ch)); err != nil {
			close(stop)
			conn.Close()
			<-writerDone
			c.drainWrites()
			return true, err
		}
	}
	c.markRegistered()

	err = c.readLoop(br, scratch)

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
	seq := c.cfg.Ring.AppendStamp(line, time.Now().UnixNano())
	if c.cfg.OnLine != nil {
		c.cfg.OnLine(seq, line)
	}
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
