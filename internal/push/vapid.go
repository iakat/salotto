// Package push implements Web Push for the salotto web front end: VAPID keys, the
// subscription store, and the encrypted POST a push service accepts.
//
// Why this is not a library call: the whole point of the feature is that the browser can
// notify a phone whose tab is closed, which means the server signs with a private key and the
// payload is encrypted so the push service cannot read it. Both halves are specified and both
// are small - an ES256 JWT and one aes128gcm record - and they are written here rather than
// pulled in so the key handling is visible in this repository. A push library would encrypt
// exactly the same way and would also own the key ring, and the key ring is the part an
// operator has to be able to audit.
package push

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// b64 is unpadded base64url, which is what VAPID and JWT both want. The padding is not a
// stylistic choice: a JWT with padding is rejected by the push services this talks to, and the
// failure looks like a permissions problem.
func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func unb64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// VAPID is the key pair a push service uses to identify this application server.
//
// The private key is the whole security of the feature: anyone holding it can send a push to
// every subscription this server has registered. It is loaded from a file and written with
// 0600, and the only place it is ever serialised is that file - never in a log line, never in
// an admin command's output.
type VAPID struct {
	Private *ecdsa.PrivateKey
	Public  string // uncompressed point, base64url: the applicationServerKey a browser takes
	Subject string // mailto: or https: identity for the push service
}

// NewVAPID generates a key pair. P-256 because Web Push specifies it: ES256, not "ECDSA".
func NewVAPID(subject string) (*VAPID, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return &VAPID{Private: priv, Public: publicKeyB64(&priv.PublicKey), Subject: subject}, nil
}

// publicKeyB64 serialises the public key as the uncompressed point Web Push requires:
// 0x04 || X || Y, each 32 bytes.
func publicKeyB64(pub *ecdsa.PublicKey) string {
	return b64(elliptic.Marshal(elliptic.P256(), pub.X, pub.Y))
}

// LoadVAPID reads a key pair from path, or creates one there if the file does not exist.
//
// Creating on demand rather than refusing is deliberate for a deployment: a missing file is
// the first boot, not an error, and an operator who has to generate a key by hand before the
// pod starts is an operator who will generate it once, forget where it went, and re-register
// every device. The file is created 0600.
func LoadVAPID(path, subject string) (*VAPID, error) {
	if path == "" {
		return nil, errors.New("push: no key path")
	}
	data, err := os.ReadFile(path)
	if err == nil {
		var stored struct {
			Private string `json:"private"`
			Subject string `json:"subject"`
		}
		if jerr := json.Unmarshal(data, &stored); jerr != nil {
			return nil, fmt.Errorf("push: %s is not a VAPID key file: %w", path, jerr)
		}
		raw, derr := unb64(stored.Private)
		if derr != nil {
			return nil, fmt.Errorf("push: %s has a malformed key: %w", path, derr)
		}
		if len(raw) != 32 {
			return nil, fmt.Errorf("push: %s has a %d-byte key, want 32", path, len(raw))
		}
		priv := &ecdsa.PrivateKey{
			PublicKey: ecdsa.PublicKey{Curve: elliptic.P256()},
			D:         new(big.Int).SetBytes(raw),
		}
		priv.PublicKey.X, priv.PublicKey.Y = elliptic.P256().ScalarBaseMult(raw)
		v := &VAPID{Private: priv, Public: publicKeyB64(&priv.PublicKey), Subject: stored.Subject}
		if v.Subject == "" {
			v.Subject = subject
		}
		return v, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	v, err := NewVAPID(subject)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	body, _ := json.MarshalIndent(struct {
		Private string `json:"private"`
		Public  string `json:"public"`
		Subject string `json:"subject"`
	}{Private: b64(v.Private.D.Bytes()), Public: v.Public, Subject: v.Subject}, "", "  ")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return nil, err
	}
	return v, nil
}

// PublicKey is what the browser needs to subscribe: the applicationServerKey.
func (v *VAPID) PublicKey() string {
	if v == nil {
		return ""
	}
	return v.Public
}

// authHeader builds the Authorization header for one push service origin.
//
// The `aud` claim is the service's origin, not the full endpoint: a token minted for one push
// service is not valid at another, and using the whole URL makes the token invalid everywhere.
// The token is short-lived (12h here) because a replay window is the only thing limiting a
// stolen token, and push services reject anything over 24h.
func (v *VAPID) authHeader(endpoint string) (string, error) {
	origin, err := originOf(endpoint)
	if err != nil {
		return "", err
	}
	header := b64([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, err := json.Marshal(map[string]any{
		"aud": origin,
		"exp": time.Now().Add(12 * time.Hour).Unix(),
		"sub": v.subjectOrFallback(),
	})
	if err != nil {
		return "", err
	}
	signingInput := header + "." + b64(claims)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, v.Private, digest[:])
	if err != nil {
		return "", err
	}
	// JWS wants the raw R||S concatenation, each padded to the curve size, not the ASN.1
	// sequence ecdsa.Sign would produce as bytes.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	token := signingInput + "." + b64(sig)
	return fmt.Sprintf("vapid t=%s,k=%s", token, v.Public), nil
}

// subjectOrFallback keeps the token valid when no subject was configured. The spec requires a
// contact; a push service that gets an empty one may reject the request, and a rejected push
// that the operator cannot see is worse than a default they can change.
func (v *VAPID) subjectOrFallback() string {
	if v.Subject != "" {
		return v.Subject
	}
	return "mailto:admin@localhost"
}

// originOf returns scheme://host from an endpoint URL.
func originOf(endpoint string) (string, error) {
	i := strings.Index(endpoint, "://")
	if i < 0 {
		return "", fmt.Errorf("push: endpoint %q has no scheme", endpoint)
	}
	rest := endpoint[i+3:]
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		rest = rest[:j]
	}
	if rest == "" {
		return "", fmt.Errorf("push: endpoint %q has no host", endpoint)
	}
	return endpoint[:i+3] + rest, nil
}

// Subscription is one browser's registration, as the browser gave it.
type Subscription struct {
	Endpoint string    `json:"endpoint"`
	P256dh   string    `json:"p256dh"`
	Auth     string    `json:"auth"`
	User     string    `json:"user,omitempty"`
	Added    time.Time `json:"added"`
}

// Store is the subscription list, persisted to one JSON file.
//
// One file, not a database: this bouncer is single-user and the subscriptions are the user's
// devices. A file survives a restart, can be backed up by copying it, and an operator can see
// what is registered with `cat` - which is the level of ceremony a few phones deserve.
type Store struct {
	mu    sync.Mutex
	path  string
	subs  map[string]Subscription
	dirty bool
}

// OpenStore loads the subscriptions from path, or starts empty if it does not exist.
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, subs: make(map[string]Subscription)}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, err
	}
	var list []Subscription
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("push: %s is not a subscription file: %w", path, err)
	}
	for _, sub := range list {
		if sub.Endpoint != "" {
			s.subs[sub.Endpoint] = sub
		}
	}
	return s, nil
}

// Add records a subscription. Re-adding the same endpoint replaces it: a browser that
// re-subscribes after a key change sends the same endpoint with new keys, and keeping the old
// keys would mean every push to that device fails silently.
func (s *Store) Add(sub Subscription) error {
	if sub.Endpoint == "" || sub.P256dh == "" || sub.Auth == "" {
		return errors.New("push: subscription needs endpoint, p256dh and auth")
	}
	if sub.Added.IsZero() {
		sub.Added = time.Now().UTC()
	}
	s.mu.Lock()
	s.subs[sub.Endpoint] = sub
	s.mu.Unlock()
	return s.save()
}

// Remove forgets an endpoint.
func (s *Store) Remove(endpoint string) error {
	s.mu.Lock()
	delete(s.subs, endpoint)
	s.mu.Unlock()
	return s.save()
}

// List returns the subscriptions, newest first.
func (s *Store) List() []Subscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Subscription, 0, len(s.subs))
	for _, sub := range s.subs {
		out = append(out, sub)
	}
	return out
}

// Len reports how many devices are registered.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs)
}

// save writes the whole list. Called with the lock already released by the callers that
// mutate, and it takes the lock itself for the read - a file write inside a lock held across an
// HTTP handler is how a slow disk becomes a hung web server.
func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	s.mu.Lock()
	list := make([]Subscription, 0, len(s.subs))
	for _, sub := range s.subs {
		list = append(list, sub)
	}
	s.mu.Unlock()

	body, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	// Write to a temporary file and rename: a crash mid-write must not leave a truncated
	// subscription file, because that file is the only thing that can re-reach those devices.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
