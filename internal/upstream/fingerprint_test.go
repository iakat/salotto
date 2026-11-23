package upstream

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/iakat/salotto/internal/certhash"
	"github.com/iakat/salotto/internal/winring"
)

// selfSignedServer starts a TLS listener whose certificate no CA signs, which is
// the case AddTrustedServerFingerprint exists for. It returns the listener and
// the fingerprint an operator would pin.
func selfSignedServer(t *testing.T) (net.Listener, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer raw.Close()
				tlsConn := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
				if err := tlsConn.HandshakeContext(context.Background()); err != nil {
					return
				}
				br := bufio.NewReader(tlsConn)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					switch {
					case strings.HasPrefix(line, "CAP LS"):
						// Offer nothing, so the client skips CAP REQ and SASL and
						// goes straight to NICK/USER: this test is about the
						// handshake, not about capabilities.
						_, _ = tlsConn.Write([]byte(":srv CAP * LS :\r\n"))
					case strings.HasPrefix(line, "USER "):
						_, _ = tlsConn.Write([]byte(":srv 001 nick :welcome\r\n"))
					}
				}
			}()
		}
	}()
	return ln, certhash.Fingerprint(cert)
}

func tlsConfig(t *testing.T, ln net.Listener, pin string) Config {
	t.Helper()
	return Config{
		Name:       "test",
		Addr:       ln.Addr().String(),
		TLS:        true,
		Nick:       "nick",
		Windows:    winring.New(8, 8, 4096),
		Route:      routeStatus,
		BackoffMin: 10 * time.Millisecond,
		BackoffMax: 20 * time.Millisecond,
		TrustFingerprint: func(fp string) bool {
			return fp == pin
		},
	}
}

// TestPinnedFingerprintAcceptsSelfSigned is B14's reason to exist: the CA cannot
// vouch for this certificate, but the operator pinned it, so the dial succeeds.
// ZNC does exactly this in CheckSSLCert: PKI first, then an exact pin.
func TestPinnedFingerprintAcceptsSelfSigned(t *testing.T) {
	ln, pin := selfSignedServer(t)
	cfg := tlsConfig(t, ln, pin)

	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx)
	}()

	waitFor(t, 4*time.Second, func() bool {
		return c.State().String() == "registered"
	}, "registration over a pinned self-signed certificate")
	cancel()
	<-done
}

// TestWrongPinFailsClosed: a different pin must not turn into "trust anything".
// This is the failure a naive implementation gets wrong by setting
// InsecureSkipVerify and only logging the fingerprint.
func TestWrongPinFailsClosed(t *testing.T) {
	ln, pin := selfSignedServer(t)
	other := "00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF"
	if pin == other {
		t.Fatal("test fingerprint collision")
	}
	cfg := tlsConfig(t, ln, other)

	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	// It must never register: the bad pin must fail the handshake, and the
	// reconnect loop must keep failing rather than falling through.
	select {
	case <-time.After(1500 * time.Millisecond):
	}
	if got := c.State().String(); got == "registered" {
		t.Fatalf("state = %q with a wrong pin: the handshake should have failed", got)
	}
	if got := c.Attempts(); got < 2 {
		t.Fatalf("Attempts = %d: the dial loop did not retry after a bad pin", got)
	}
}

// TestNoPinFailsClosed: with no hook at all, a self-signed certificate must be
// rejected, because that is the default and the safe one.
func TestNoPinFailsClosed(t *testing.T) {
	ln, _ := selfSignedServer(t)
	cfg := tlsConfig(t, ln, "unused")
	cfg.TrustFingerprint = nil

	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx)
	}()
	<-done
	if got := c.State().String(); got == "registered" {
		t.Fatal("a self-signed certificate was accepted with no pin configured")
	}
}
