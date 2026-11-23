package facade

import (
	"sort"
	"sync"

	"github.com/iakat/salotto/internal/certhash"
)

// Fingerprints is the per-network set of certificate fingerprints the operator
// trusts explicitly. ZNC's AddTrustedServerFingerprint does not replace CA
// validation, it adds a second way to accept a certificate: Socket.cpp's
// CheckSSLCert accepts when PKI validation passed, and otherwise accepts only
// if the peer's SHA-256 fingerprint is in this set. Pinning a self-signed
// server therefore works, and a wrong pin still fails closed.
type Fingerprints struct {
	mu   sync.RWMutex
	nets map[string]map[string]bool
}

// NewFingerprints returns an empty set.
func NewFingerprints() *Fingerprints {
	return &Fingerprints{nets: make(map[string]map[string]bool)}
}

// Add records a fingerprint for a network. It reports false when the input is
// not a usable SHA-256 fingerprint, which the caller turns into ZNC's Usage
// message instead of storing garbage.
func (f *Fingerprints) Add(network, fp string) bool {
	norm := certhash.Normalize(fp)
	if norm == "" {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	set := f.nets[network]
	if set == nil {
		set = make(map[string]bool)
		f.nets[network] = set
	}
	set[norm] = true
	return true
}

// Del forgets a fingerprint for a network.
func (f *Fingerprints) Del(network, fp string) bool {
	norm := certhash.Normalize(fp)
	if norm == "" {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	set := f.nets[network]
	if !set[norm] {
		return false
	}
	delete(set, norm)
	if len(set) == 0 {
		delete(f.nets, network)
	}
	return true
}

// List returns a network's fingerprints in sorted order.
func (f *Fingerprints) List(network string) []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	set := f.nets[network]
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for fp := range set {
		out = append(out, fp)
	}
	sort.Strings(out)
	return out
}

// Trusted reports whether a fingerprint is trusted for a network. It is the
// predicate the dialler's TLS check calls, so a pin added at runtime applies to
// the next reconnect without a restart.
func (f *Fingerprints) Trusted(network, fp string) bool {
	norm := certhash.Normalize(fp)
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.nets[network][norm]
}

// ForgetNetwork drops a network's set, so a removed network cannot leave pins
// behind for a later network reusing the name.
func (f *Fingerprints) ForgetNetwork(network string) {
	f.mu.Lock()
	delete(f.nets, network)
	f.mu.Unlock()
}

// TrustedServerFingerprints is the set the wire commands and the dialler share.
func (s *Server) TrustedServerFingerprints() *Fingerprints { return s.fingerprints }

// AddTrustedServerFingerprint records a pin for a network.
func (s *Server) AddTrustedServerFingerprint(network, fp string) bool {
	return s.fingerprints.Add(network, fp)
}

// DelTrustedServerFingerprint forgets a pin for a network.
func (s *Server) DelTrustedServerFingerprint(network, fp string) bool {
	return s.fingerprints.Del(network, fp)
}

// ListTrustedServerFingerprints returns a network's pins.
func (s *Server) ListTrustedServerFingerprints(network string) []string {
	return s.fingerprints.List(network)
}
