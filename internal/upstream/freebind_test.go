package upstream

import (
	"context"
	"errors"
	"net"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/iakat/salotto/internal/ring"
)

// freebindAddr is in TEST-NET-1 (RFC 5737) and is not configured on any
// interface of the test host. Binding to it is exactly the property under test.
var freebindAddr = [4]byte{192, 0, 2, 77}

func bindIPv4(t *testing.T, port int, freebind bool) (fd int, bound syscall.SockaddrInet4, err error) {
	t.Helper()
	fd, err = syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })

	if freebind {
		if err := syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_FREEBIND, 1); err != nil {
			t.Fatalf("setsockopt IP_FREEBIND: %v", err)
		}
	}

	sa := &syscall.SockaddrInet4{Port: port, Addr: freebindAddr}
	if err := syscall.Bind(fd, sa); err != nil {
		return fd, syscall.SockaddrInet4{}, err
	}

	bound, ok := sockaddrInet4(t, fd)
	if !ok {
		t.Fatalf("getsockname returned a non-IPv4 address")
	}
	return fd, bound, nil
}

func sockaddrInet4(t *testing.T, fd int) (syscall.SockaddrInet4, bool) {
	t.Helper()
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatalf("getsockname: %v", err)
	}
	v4, ok := sa.(*syscall.SockaddrInet4)
	if !ok {
		return syscall.SockaddrInet4{}, false
	}
	return *v4, true
}

// TestFreebindBindSucceeds proves the property at the bind() syscall: with
// IP_FREEBIND the kernel accepts a source address that is not local.
func TestFreebindBindSucceeds(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("IP_FREEBIND is Linux-specific")
	}

	_, bound, err := bindIPv4(t, 0, true)
	if err != nil {
		t.Fatalf("bind with IP_FREEBIND failed: %v", err)
	}
	if bound.Addr != freebindAddr {
		t.Fatalf("bound address = %v, want %v", net.IP(bound.Addr[:]), net.IP(freebindAddr[:]))
	}
}

// TestFreebindNegativeControl runs the same bind() without the setsockopt. It
// must fail with EADDRNOTAVAIL: that is what proves the option, not the host,
// is doing the work.
func TestFreebindNegativeControl(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("IP_FREEBIND is Linux-specific")
	}

	_, _, err := bindIPv4(t, 0, false)
	if !errors.Is(err, syscall.EADDRNOTAVAIL) {
		t.Fatalf("bind without IP_FREEBIND = %v, want EADDRNOTAVAIL", err)
	}
}

// TestFreebindDialerControl exercises the production dialer path: the dial must
// bind to the non-local address and then attempt the TCP handshake. On this host
// the address has no route, so the handshake ends in ENETUNREACH; on a host with
// a route to 192.0.2.0/24 it ends in ECONNREFUSED. Either is a connect-phase
// error, which can only happen after bind() succeeded. EADDRNOTAVAIL would mean
// bind() failed and is a hard failure here.
func TestFreebindDialerControl(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("IP_FREEBIND is Linux-specific")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	c, err := New(Config{
		Name:       "test",
		Addr:       addr,
		Nick:       "nick",
		Ring:       ring.New(2, 512),
		BindHost:   net.IP(freebindAddr[:]).String(),
		BackoffMin: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := c.dialer.DialContext(ctx, "tcp", addr)
	if conn != nil {
		_ = conn.Close()
	}
	if errors.Is(err, syscall.EADDRNOTAVAIL) {
		t.Fatalf("dial failed at bind with EADDRNOTAVAIL; FREEBIND was not applied: %v", err)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, syscall.ENETUNREACH) {
		t.Fatalf("dial = %v, want ECONNREFUSED or ENETUNREACH (bind must succeed before connect)", err)
	}
}

// TestDialerWithoutFreebindFailsAtBind is the negative control for the production
// path: the same non-local LocalAddr without IP_FREEBIND must fail with
// EADDRNOTAVAIL at bind().
func TestDialerWithoutFreebindFailsAtBind(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("IP_FREEBIND is Linux-specific")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	d := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IP(freebindAddr[:])}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", addr)
	if conn != nil {
		_ = conn.Close()
	}
	if !errors.Is(err, syscall.EADDRNOTAVAIL) {
		t.Fatalf("dial without FREEBIND = %v, want EADDRNOTAVAIL", err)
	}
}

// TestSetBindHostRepointsNextDial checks that SetBindHost is applied to the next
// dial without touching a running connection (ZNC's SetBindHost is the same: it
// stores the value and Connect reads it).
func TestSetBindHostRepointsNextDial(t *testing.T) {
	c, err := New(Config{
		Name: "test",
		Addr: "127.0.0.1:1",
		Nick: "nick",
		Ring: ring.New(2, 512),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.BindHost() != "" {
		t.Fatalf("initial BindHost = %q", c.BindHost())
	}
	if err := c.SetBindHost("192.0.2.77"); err != nil {
		t.Fatalf("SetBindHost: %v", err)
	}
	if c.BindHost() != "192.0.2.77" {
		t.Fatalf("BindHost = %q", c.BindHost())
	}
	d := c.dialerForDial()
	if d == nil || d.LocalAddr == nil || d.LocalAddr.String() != "192.0.2.77:0" {
		t.Fatalf("next dial LocalAddr = %v", d.LocalAddr)
	}
	if err := c.SetBindHost("not-an-ip"); err == nil {
		t.Fatal("SetBindHost accepted a non-IP")
	}
	if err := c.SetBindHost(""); err != nil {
		t.Fatalf("SetBindHost(\"\"): %v", err)
	}
	if d := c.dialerForDial(); d.LocalAddr != nil || d.Control != nil {
		t.Fatalf("cleared dialer still bound: LocalAddr=%v Control set=%v", d.LocalAddr, d.Control != nil)
	}
}

func TestDialerWithBindHostDoesNotMutate(t *testing.T) {
	base := &net.Dialer{}
	_ = dialerWithBindHost(base, "192.0.2.77")
	if base.LocalAddr != nil || base.Control != nil {
		t.Fatalf("base dialer was mutated: LocalAddr=%v Control set=%v", base.LocalAddr, base.Control != nil)
	}

	d := dialerWithBindHost(base, "192.0.2.77")
	if d == base {
		t.Fatal("dialerWithBindHost returned the caller's dialer")
	}
	if d.LocalAddr == nil || d.LocalAddr.String() != "192.0.2.77:0" {
		t.Fatalf("LocalAddr = %v", d.LocalAddr)
	}
}
