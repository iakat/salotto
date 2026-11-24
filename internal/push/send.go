package push

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Message is one notification.
type Message struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	// Tag makes a newer notification replace an older one with the same tag, which is the
	// difference between a phone that shows one "5 new messages" and a phone that shows five.
	Tag string `json:"tag,omitempty"`
	// URL is what the service worker opens on click.
	URL string `json:"url,omitempty"`
	// Badge is the unread count to show on the icon; zero leaves it alone.
	Badge int `json:"badge,omitempty"`
}

// encrypt builds an aes128gcm body for one subscription (RFC 8291 / RFC 8188).
//
// The shape is: a header with the sender's ephemeral public key, then a single record whose
// plaintext is the JSON payload followed by the padding delimiter. The push service sees only
// ciphertext and the ephemeral key, which is why a push can carry a channel name and a message
// preview through somebody else's infrastructure.
//
// Two details are easy to get wrong and both fail the same way - the browser silently drops the
// notification, and the push service still returns 201, so there is nothing to debug:
//
//   - the salt is 16 bytes and goes in the header, and the key derivation uses it before the
//     key material, not after;
//   - `auth` is the subscription's auth secret, and it is part of the HKDF *salt* for the
//     content encryption key, not an input key.
func encrypt(payload []byte, sub Subscription) ([]byte, error) {
	asKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}
	return encryptWith(payload, sub, asKey, salt)
}

// encryptWith is encrypt with the ephemeral key and salt supplied. It exists so the RFC 8291
// test vector can be reproduced exactly: a scheme verified only against itself is a scheme
// that is self-consistent and possibly wrong, and the failure mode here is silent - the push
// service returns 201 and the phone shows nothing.
func encryptWith(payload []byte, sub Subscription, asKey *ecdh.PrivateKey, salt []byte) (body []byte, err error) {
	uaPub, err := unb64(sub.P256dh)
	if err != nil {
		return nil, fmt.Errorf("push: bad p256dh: %w", err)
	}
	authSecret, err := unb64(sub.Auth)
	if err != nil {
		return nil, fmt.Errorf("push: bad auth: %w", err)
	}
	if len(authSecret) != 16 {
		return nil, fmt.Errorf("push: auth is %d bytes, want 16", len(authSecret))
	}

	curve := ecdh.P256()
	uaKey, err := curve.NewPublicKey(uaPub)
	if err != nil {
		return nil, fmt.Errorf("push: p256dh is not a P-256 point: %w", err)
	}
	shared, err := asKey.ECDH(uaKey)
	if err != nil {
		return nil, err
	}

	asPub := asKey.PublicKey().Bytes()

	// RFC 8291 section 3.4, in its two named steps. The first extract uses the subscription's
	// auth secret as the HKDF salt and the ECDH secret as the input key; that produces the IKM
	// for the second, whose info string binds both public keys - user agent first, then ours,
	// which is the order the RFC fixes and the order a browser checks.
	ikm := hkdfExpandOnly(hkdfExtractV(authSecret, shared), []byte("WebPush: info\x00"), uaPub, asPub, 32)

	// The content-encryption key and nonce come from a fresh extract keyed on the record salt.
	prk := hkdfExtractV(salt, ikm)
	cek := hkdfExpandOnly(prk, []byte("Content-Encoding: aes128gcm\x00"), nil, nil, 16)
	nonce := hkdfExpandOnly(prk, []byte("Content-Encoding: nonce\x00"), nil, nil, 12)

	// One record: payload || 0x02 (the last-record delimiter). 0x01 would mean "more
	// records follow" and a browser that sees it waits for a record that never comes.
	plain := append(append([]byte(nil), payload...), 0x02)
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ciphertext := gcm.Seal(nil, nonce, plain, nil)

	// Header: salt(16) || rs(4) || idlen(1) || keyid(asPub)
	var out bytes.Buffer
	out.Write(salt)
	var rs [4]byte
	binary.BigEndian.PutUint32(rs[:], 4096)
	out.Write(rs[:])
	out.WriteByte(byte(len(asPub)))
	out.Write(asPub)
	out.Write(ciphertext)
	return out.Bytes(), nil
}

// hkdfExtractV is HKDF-Extract: PRK = HMAC(salt, IKM).
func hkdfExtractV(salt, ikm []byte) []byte {
	mac := hmac.New(sha256.New, salt)
	mac.Write(ikm)
	return mac.Sum(nil)
}

// hkdfExpandOnly is HKDF-Expand: T(1) || T(2) ... truncated to n, where each T is
// HMAC(PRK, T(prev) || info || counter).
func hkdfExpandOnly(prk, info, a, b []byte, n int) []byte {
	full := make([]byte, 0, len(info)+len(a)+len(b))
	full = append(full, info...)
	full = append(full, a...)
	full = append(full, b...)

	var out, prev []byte
	for counter := byte(1); len(out) < n; counter++ {
		mac := hmac.New(sha256.New, prk)
		mac.Write(prev)
		mac.Write(full)
		mac.Write([]byte{counter})
		prev = mac.Sum(nil)
		out = append(out, prev...)
	}
	return out[:n]
}

// Sender delivers push messages.
type Sender struct {
	VAPID  *VAPID
	Store  *Store
	Client *http.Client
	// OnGone is called when a push service says the subscription no longer exists (404/410).
	// Those devices are removed from the store: a subscription that has been revoked is dead
	// weight, and re-sending to it on every message is a way to look like a spammer to a push
	// service.
	OnGone func(endpoint string)
}

// NewSender builds a sender with sane defaults.
func NewSender(v *VAPID, store *Store) *Sender {
	return &Sender{
		VAPID:  v,
		Store:  store,
		Client: &http.Client{Timeout: 10 * time.Second},
	}
}

// Send delivers msg to every subscription, returning how many succeeded.
//
// Per-device failures do not stop the others. A phone that has been wiped should not stop the
// tablet from getting the notification, and a push service having a bad day should not make
// every subsequent message give up.
func (s *Sender) Send(msg Message) (sent, failed int, err error) {
	if s == nil || s.VAPID == nil || s.Store == nil {
		return 0, 0, errors.New("push: not configured")
	}
	payload, err := jsonMarshal(msg)
	if err != nil {
		return 0, 0, err
	}
	subs := s.Store.List()
	sent, failed = 0, 0
	for _, sub := range subs {
		if serr := s.sendOne(sub, payload); serr != nil {
			failed++
			if errors.Is(serr, errGone) {
				_ = s.Store.Remove(sub.Endpoint)
				if s.OnGone != nil {
					s.OnGone(sub.Endpoint)
				}
			}
			continue
		}
		sent++
	}
	return sent, failed, nil
}

// errGone means the subscription no longer exists at the push service.
var errGone = errors.New("push: subscription is gone")

// sendOne delivers one encrypted message.
func (s *Sender) sendOne(sub Subscription, payload []byte) error {
	body, err := encrypt(payload, sub)
	if err != nil {
		return err
	}
	auth, err := s.VAPID.authHeader(sub.Endpoint)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", "86400")
	// Urgency decides whether the phone wakes for this. "normal" for a mention is right;
	// "high" is for something the user must see now, and using it for every message is how a
	// user turns notifications off.
	req.Header.Set("Urgency", "normal")

	resp, err := s.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return errGone
	default:
		return fmt.Errorf("push: service returned %d", resp.StatusCode)
	}
}

// jsonMarshal is encoding/json, named so the call sites read as "the payload".
func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}
