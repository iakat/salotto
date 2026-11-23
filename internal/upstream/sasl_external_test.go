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
	"encoding/base64"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iakat/salotto/internal/winring"
)

// certPair makes a self-signed certificate and returns its PEM files, for the
// server and (with a different subject) for the client.
func certPair(t *testing.T, cn string) (certPEM, keyPEM []byte, cert *x509.Certificate, key *ecdsa.PrivateKey) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	certPEM = pemEncode("CERTIFICATE", der)
	keyDER, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM = pemEncode("EC PRIVATE KEY", keyDER)
	return certPEM, keyPEM, parsed, k
}

func pemEncode(kind string, der []byte) []byte {
	var b strings.Builder
	b.WriteString("-----BEGIN " + kind + "-----\n")
	// base64 with 64-char lines, as tls.LoadX509KeyPair expects.
	enc := base64.StdEncoding.EncodeToString(der)
	for i := 0; i < len(enc); i += 64 {
		end := i + 64
		if end > len(enc) {
			end = len(enc)
		}
		b.WriteString(enc[i:end] + "\n")
	}
	b.WriteString("-----END " + kind + "-----\n")
	return []byte(b.String())
}

// TestSASLExternalSendsEmptyResponse is B6-EXTERNAL: the mechanism is EXTERNAL,
// the response is a single "+" and no username or password is ever sent. ZNC
// does exactly this (sasl.cpp:198-213). A server that requires a certificate
// but is offered credentials would fail, so the empty response is the assertion
// that matters.
func TestSASLExternalSendsEmptyResponse(t *testing.T) {
	srvCertPEM, srvKeyPEM, _, _ := certPair(t, "server")
	clientCertPEM, clientKeyPEM, _, _ := certPair(t, "client")

	dir := t.TempDir()
	certFile := filepath.Join(dir, "client.pem")
	keyFile := filepath.Join(dir, "client.key")
	if err := os.WriteFile(certFile, clientCertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, clientKeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	srvCert, err := tls.X509KeyPair(srvCertPEM, srvKeyPEM)
	if err != nil {
		t.Fatalf("server keypair: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	type exchange struct {
		mech     string
		response string
	}
	got := make(chan exchange, 1)

	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer raw.Close()
				// RequireClientCert: SASL EXTERNAL without a certificate is
				// meaningless, so the test server demands one.
				tlsConn := tls.Server(raw, &tls.Config{
					Certificates: []tls.Certificate{srvCert},
					ClientAuth:   tls.RequireAnyClientCert,
				})
				if err := tlsConn.HandshakeContext(context.Background()); err != nil {
					return
				}
				br := bufio.NewReader(tlsConn)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					line = strings.TrimRight(line, "\r\n")
					switch {
					case strings.HasPrefix(line, "CAP LS"):
						_, _ = tlsConn.Write([]byte(":srv CAP * LS :sasl=PLAIN,EXTERNAL\r\n"))
					case strings.HasPrefix(line, "CAP REQ"):
						_, _ = tlsConn.Write([]byte(":srv CAP * ACK :sasl\r\n"))
					case strings.HasPrefix(line, "AUTHENTICATE "):
						arg := strings.TrimPrefix(line, "AUTHENTICATE ")
						if arg == "PLAIN" || arg == "EXTERNAL" {
							// The mechanism: prompt with "+".
							_, _ = tlsConn.Write([]byte("AUTHENTICATE +\r\n"))
							continue
						}
						// The response. Record it, then succeed.
						got <- exchange{response: arg}
						_, _ = tlsConn.Write([]byte(":srv 903 nick :SASL success\r\n"))
					case strings.HasPrefix(line, "CAP END"):
					case strings.HasPrefix(line, "USER "):
						_, _ = tlsConn.Write([]byte(":srv 001 nick :welcome\r\n"))
					}
				}
			}()
		}
	}()

	w := winring.New(4, 4, 4096)
	c, err := New(Config{
		Name:             "test",
		Addr:             ln.Addr().String(),
		TLS:              true,
		Nick:             "nick",
		SASLExternal:     true,
		CertFile:         certFile,
		KeyFile:          keyFile,
		Windows:          w,
		Route:            routeStatus,
		BackoffMin:       10 * time.Millisecond,
		BackoffMax:       20 * time.Millisecond,
		TrustFingerprint: func(string) bool { return true },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx)
	}()

	select {
	case ex := <-got:
		if ex.response != "+" {
			t.Fatalf("AUTHENTICATE response = %q, want the empty EXTERNAL payload \"+\"", ex.response)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("the server never received the AUTHENTICATE response")
	}

	waitFor(t, 4*time.Second, func() bool {
		return c.State().String() == "registered"
	}, "registration after SASL EXTERNAL")
	cancel()
	<-done
}

// TestSASLExternalRequiresCertificate: with no certificate configured the dial
// must fail at the TLS layer, not fall back to an unauthenticated session.
func TestSASLExternalRequiresCertificate(t *testing.T) {
	srvCertPEM, srvKeyPEM, _, _ := certPair(t, "server")
	srvCert, err := tls.X509KeyPair(srvCertPEM, srvKeyPEM)
	if err != nil {
		t.Fatalf("server keypair: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer raw.Close()
				tlsConn := tls.Server(raw, &tls.Config{
					Certificates: []tls.Certificate{srvCert},
					ClientAuth:   tls.RequireAnyClientCert,
				})
				_ = tlsConn.HandshakeContext(context.Background())
			}()
		}
	}()

	c, err := New(Config{
		Name:             "test",
		Addr:             ln.Addr().String(),
		TLS:              true,
		Nick:             "nick",
		SASLExternal:     true,
		Windows:          winring.New(4, 4, 4096),
		Route:            routeStatus,
		BackoffMin:       10 * time.Millisecond,
		BackoffMax:       20 * time.Millisecond,
		TrustFingerprint: func(string) bool { return true },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx)
	}()
	<-done
	if got := c.State().String(); got == "registered" {
		t.Fatal("SASL EXTERNAL without a client certificate registered anyway")
	}
}
