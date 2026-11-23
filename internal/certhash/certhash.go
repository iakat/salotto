// Package certhash renders certificate fingerprints in the one form ZNC uses,
// so a value copied from ZNC's ListTrustedServerFingerprints is the same string
// here (Socket.cpp: GetSSLPeerFingerprint digests with SHA-256 and escapes with
// EHEXCOLON: uppercase hex pairs separated by colons).
package certhash

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"strings"
)

// Fingerprint renders a certificate as uppercase colon-separated hex pairs.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return format(sum[:])
}

func format(sum []byte) string {
	enc := strings.ToUpper(hex.EncodeToString(sum))
	var out strings.Builder
	for i := 0; i < len(enc); i += 2 {
		if i > 0 {
			out.WriteByte(':')
		}
		out.WriteString(enc[i : i+2])
	}
	return out.String()
}

// Normalize puts a user-supplied fingerprint in canonical form: hex digits are
// extracted (so any separator style is accepted) and re-emitted as uppercase
// colon-separated pairs. It returns "" when the input is not a SHA-256 digest,
// which is what lets the caller reject garbage instead of storing it.
func Normalize(s string) string {
	var hexOnly strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			hexOnly.WriteRune(r)
		case r >= 'a' && r <= 'f':
			hexOnly.WriteRune(r - 'a' + 'A')
		case r >= 'A' && r <= 'F':
			hexOnly.WriteRune(r)
		}
	}
	raw := hexOnly.String()
	if len(raw) != 64 {
		return ""
	}
	out, err := hex.DecodeString(raw)
	if err != nil {
		return ""
	}
	return format(out)
}

// Valid reports whether s is a normalizable fingerprint.
func Valid(s string) bool { return Normalize(s) != "" }
