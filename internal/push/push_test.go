package push

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rfcB64 decodes the base64url values from RFC 8291, which contain line-wrapping whitespace.
func rfcB64(t *testing.T, s string) []byte {
	t.Helper()
	s = strings.Join(strings.Fields(s), "")
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

// TestTheRFCTestVectorEncryptsToThePublishedBytes is the only test in this package that proves
// the encryption is right rather than merely self-consistent.
//
// RFC 8291 section 5 publishes a complete example: keys, salt, payload, and the exact 145 bytes
// that go on the wire. An encryption scheme verified by round-tripping against itself can be
// wrong in a dozen ways - a swapped HKDF step, a missing NUL terminator, the wrong record
// delimiter - and every one of those failures looks the same in production: the push service
// answers 201 and the phone shows nothing.
func TestTheRFCTestVectorEncryptsToThePublishedBytes(t *testing.T) {
	uaPub := rfcB64(t, `BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcx
		aOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4`)
	asPrivRaw := rfcB64(t, `yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw`)
	salt := rfcB64(t, `DGv6ra1nlYgDCS1FRnbzlw`)
	auth := rfcB64(t, `BTBZMqHH6r4Tts7J_aSIgg`)

	asKey, err := ecdh.P256().NewPrivateKey(asPrivRaw)
	if err != nil {
		t.Fatal(err)
	}

	sub := Subscription{
		Endpoint: "https://push.example.net/push/JzLQ3raZJfFBR0aqvOMsLrt54w4rJUsV",
		P256dh:   base64.RawURLEncoding.EncodeToString(uaPub),
		Auth:     base64.RawURLEncoding.EncodeToString(auth),
	}

	// "When I grow up, I want to be a watermelon"
	got, err := encryptWith([]byte("When I grow up, I want to be a watermelon"), sub, asKey, salt)
	if err != nil {
		t.Fatal(err)
	}

	want := "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27ml" +
		"mlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPT" +
		"pK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"

	gotB64 := base64.RawURLEncoding.EncodeToString(got)
	if gotB64 != want {
		// Print the divergence at the first differing byte: the header is the salt, record
		// size and keyid, so a mismatch inside the first 86 bytes means the derivation, while
		// one after it means the AEAD.
		t.Fatalf("the encrypted body does not match RFC 8291 section 5\ngot  %s\nwant %s", gotB64, want)
	}
}

// TestTheIntermediateValuesMatchTheRFC checks the key derivation on its own, so a failure says
// which step is wrong rather than only that the bytes differ.
func TestTheIntermediateValuesMatchTheRFC(t *testing.T) {
	uaPub := rfcB64(t, `BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcx
		aOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4`)
	asPrivRaw := rfcB64(t, `yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw`)
	salt := rfcB64(t, `DGv6ra1nlYgDCS1FRnbzlw`)
	auth := rfcB64(t, `BTBZMqHH6r4Tts7J_aSIgg`)

	asKey, err := ecdh.P256().NewPrivateKey(asPrivRaw)
	if err != nil {
		t.Fatal(err)
	}
	uaKey, err := ecdh.P256().NewPublicKey(uaPub)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := asKey.ECDH(uaKey)
	if err != nil {
		t.Fatal(err)
	}

	check := func(name, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s\n got %s\nwant %s", name, got, want)
		}
	}

	check("ecdh_secret", base64.RawURLEncoding.EncodeToString(shared),
		"kyrL1jIIOHEzg3sM2ZWRHDRB62YACZhhSlknJ672kSs")

	prkKey := hkdfExtractV(auth, shared)
	check("PRK_key", base64.RawURLEncoding.EncodeToString(prkKey),
		"Snr3JMxaHVDXHWJn5wdC52WjpCtd2EIEGBykDcZW32k")

	asPub := asKey.PublicKey().Bytes()
	ikm := hkdfExpandOnly(prkKey, []byte("WebPush: info\x00"), uaPub, asPub, 32)
	check("IKM", base64.RawURLEncoding.EncodeToString(ikm),
		"S4lYMb_L0FxCeq0WhDx813KgSYqU26kOyzWUdsXYyrg")

	prk := hkdfExtractV(salt, ikm)
	check("PRK", base64.RawURLEncoding.EncodeToString(prk),
		"09_eUZGrsvxChDCGRCdkLiDXrReGOEVeSCdCcPBSJSc")

	cek := hkdfExpandOnly(prk, []byte("Content-Encoding: aes128gcm\x00"), nil, nil, 16)
	check("CEK", base64.RawURLEncoding.EncodeToString(cek), "oIhVW04MRdy2XN9CiKLxTg")

	nonce := hkdfExpandOnly(prk, []byte("Content-Encoding: nonce\x00"), nil, nil, 12)
	check("NONCE", base64.RawURLEncoding.EncodeToString(nonce), "4h_95klXJ5E_qnoN")
}

// TestTheHeaderIsTheShapeTheSpecRequires: salt, record size, key id length, key id. A browser
// reads these bytes positionally, so a wrong length here is an undecodable message rather than
// a wrong one.
func TestTheHeaderIsTheShapeTheSpecRequires(t *testing.T) {
	v, err := NewVAPID("mailto:test@example.org")
	if err != nil {
		t.Fatal(err)
	}
	_ = v
	ua, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	sub := Subscription{
		Endpoint: "https://push.example.net/x",
		P256dh:   base64.RawURLEncoding.EncodeToString(ua.PublicKey().Bytes()),
		Auth:     base64.RawURLEncoding.EncodeToString(auth),
	}
	body, err := encrypt([]byte("hi"), sub)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) < 16+4+1+65 {
		t.Fatalf("body is %d bytes, shorter than the 86-byte header", len(body))
	}
	if body[20] != 65 {
		t.Fatalf("key id length byte is %d, want 65 (an uncompressed P-256 point)", body[20])
	}
	if body[21] != 0x04 {
		t.Fatalf("key id does not start with 0x04, so it is not an uncompressed point")
	}
}

// TestTheLastRecordDelimiterIsTwo: 0x01 means "a record follows", and a browser that sees it
// waits forever. It is one byte and it is the difference between working and not.
func TestTheLastRecordDelimiterIsTwo(t *testing.T) {
	// Decrypt our own message the way a user agent would, and look at the delimiter.
	uaKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	sub := Subscription{
		Endpoint: "https://push.example.net/x",
		P256dh:   base64.RawURLEncoding.EncodeToString(uaKey.PublicKey().Bytes()),
		Auth:     base64.RawURLEncoding.EncodeToString(auth),
	}
	body, err := encrypt([]byte("payload"), sub)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := decryptForTest(body, uaKey, auth)
	if err != nil {
		t.Fatalf("a message this package produced could not be decrypted: %v", err)
	}
	if len(plain) == 0 || plain[len(plain)-1] != 0x02 {
		t.Fatalf("last record delimiter is %#x, want 0x02", plain[len(plain)-1])
	}
	if string(plain[:len(plain)-1]) != "payload" {
		t.Fatalf("plaintext is %q", plain)
	}
}

// TestTheSubjectFallsBackRatherThanBeingEmpty: a push service may reject a token whose sub is
// empty, and a rejected push the operator cannot see is worse than a default they can change.
func TestTheSubjectFallsBackRatherThanBeingEmpty(t *testing.T) {
	v := &VAPID{Subject: ""}
	if v.subjectOrFallback() == "" {
		t.Fatal("an empty subject would make every token rejectable")
	}
}

// TestTheAudienceIsTheOrigin: a token minted for the full endpoint URL is valid nowhere, and
// the failure looks like a permissions problem.
func TestTheAudienceIsTheOrigin(t *testing.T) {
	got, err := originOf("https://fcm.googleapis.com/fcm/send/abc123")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://fcm.googleapis.com" {
		t.Fatalf("audience = %q", got)
	}
	if _, err := originOf("nonsense"); err == nil {
		t.Fatal("a scheme-less endpoint was accepted")
	}
}

// TestTheVAPIDKeyRoundTripsThroughItsFile: the key is the whole security of the feature and it
// has to survive a restart, or every device has to re-subscribe.
func TestTheVAPIDKeyRoundTripsThroughItsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vapid.json")

	first, err := LoadVAPID(path, "mailto:test@example.org")
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadVAPID(path, "mailto:test@example.org")
	if err != nil {
		t.Fatal(err)
	}
	if first.Public != second.Public {
		t.Fatalf("the key changed across a load: %s then %s", first.Public, second.Public)
	}
	if first.Private.D.Cmp(second.Private.D) != 0 {
		t.Fatal("the private key changed across a load")
	}

	// 0600: the private key signs pushes; anyone who can read it can push to every device.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("the key file is %o, want 600", perm)
	}
}

// TestThePublicKeyIsAnUncompressedPoint: a browser rejects anything else, and the error it
// gives is unhelpful.
func TestThePublicKeyIsAnUncompressedPoint(t *testing.T) {
	v, err := NewVAPID("mailto:test@example.org")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(v.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 65 || raw[0] != 0x04 {
		t.Fatalf("public key is %d bytes starting %#x, want 65 starting 0x04", len(raw), raw[0])
	}
}

// TestTheJWTIsWellFormed: three base64url segments, ES256 header, and a signature that verifies.
func TestTheJWTIsWellFormed(t *testing.T) {
	v, err := NewVAPID("mailto:test@example.org")
	if err != nil {
		t.Fatal(err)
	}
	hdr, err := v.authHeader("https://push.example.net/send/abc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hdr, "vapid t=") || !strings.Contains(hdr, ",k=") {
		t.Fatalf("header = %q", hdr)
	}
	token := strings.TrimPrefix(strings.Split(hdr, ",")[0], "vapid t=")
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d segments", len(parts))
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var h struct{ Alg string }
	if err := json.Unmarshal(header, &h); err != nil || h.Alg != "ES256" {
		t.Fatalf("JWT header is %s", header)
	}

	// Verify the signature the way a push service does: raw R||S over SHA-256 of the input.
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	if len(sig) != 64 {
		t.Fatalf("signature is %d bytes, want 64 (raw R||S)", len(sig))
	}
	if !verifyES256(t, v, parts[0]+"."+parts[1], sig) {
		t.Fatal("the JWT signature does not verify")
	}
}

// TestTheSubscriptionStoreSurvivesARestart and replaces on re-subscribe.
func TestTheSubscriptionStoreSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subs.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	sub := Subscription{
		Endpoint: "https://push.example.net/a",
		P256dh:   "BOrU=",
		Auth:     "AAAA",
	}
	if err := store.Add(sub); err != nil {
		t.Fatal(err)
	}

	again, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.Len() != 1 {
		t.Fatalf("after a restart the store has %d subscriptions", again.Len())
	}

	// Re-subscribe with new keys on the same endpoint: the keys must be replaced, not kept
	// alongside, or every push to that device fails with the stale ones.
	sub.P256dh = "BNEW="
	sub.Auth = "BBBB"
	if err := again.Add(sub); err != nil {
		t.Fatal(err)
	}
	list := again.List()
	if len(list) != 1 {
		t.Fatalf("re-subscribing created %d entries", len(list))
	}
	if list[0].P256dh != "BNEW=" {
		t.Fatalf("the stale key survived: %q", list[0].P256dh)
	}
}

// TestTheSubscriptionStoreRefusesIncompleteEntries: a subscription without keys is a device
// that will never receive anything, and storing it hides that.
func TestTheSubscriptionStoreRefusesIncompleteEntries(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "s.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Add(Subscription{Endpoint: "https://x/y"}); err == nil {
		t.Fatal("a subscription without keys was accepted")
	}
}

// TestASubscriptionFileIsWrittenAtomically: a truncated file is every device lost, so the write
// goes through a temporary file.
func TestASubscriptionFileIsWrittenAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	store, _ := OpenStore(path)
	if err := store.Add(Subscription{Endpoint: "https://x/y", P256dh: "B=", Auth: "C="}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Fatal("the temporary file was left behind")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the store file was not written: %v", err)
	}
}

// TestAddingASubscriptionRecordsWhen: the timestamp is what lets an operator see a device they
// no longer recognise.
func TestAddingASubscriptionRecordsWhen(t *testing.T) {
	store, _ := OpenStore("")
	before := time.Now().Add(-time.Second)
	if err := store.Add(Subscription{Endpoint: "https://x/y", P256dh: "B=", Auth: "C="}); err != nil {
		t.Fatal(err)
	}
	list := store.List()
	if len(list) != 1 {
		t.Fatal("no subscription")
	}
	if list[0].Added.Before(before) {
		t.Fatalf("Added is %v, which is before the call", list[0].Added)
	}
}

// verifyES256 checks a raw R||S signature the way a push service does.
func verifyES256(t *testing.T, v *VAPID, signingInput string, sig []byte) bool {
	t.Helper()
	digest := sha256.Sum256([]byte(signingInput))
	r := new(big.Int).SetBytes(sig[:32])
	ss := new(big.Int).SetBytes(sig[32:])
	return ecdsa.Verify(&v.Private.PublicKey, digest[:], r, ss)
}

// decryptForTest is a user agent: it undoes encryptWith. It exists so the delimiter and the
// plaintext can be checked, which a test that only compares bytes cannot do.
func decryptForTest(body []byte, ua *ecdh.PrivateKey, auth []byte) ([]byte, error) {
	if len(body) < 21 {
		return nil, errShortBody
	}
	salt := body[:16]
	keyLen := int(body[20])
	if len(body) < 21+keyLen {
		return nil, errShortBody
	}
	asPub := body[21 : 21+keyLen]
	ciphertext := body[21+keyLen:]

	asKey, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		return nil, err
	}
	shared, err := ua.ECDH(asKey)
	if err != nil {
		return nil, err
	}
	ikm := hkdfExpandOnly(hkdfExtractV(auth, shared), []byte("WebPush: info\x00"), ua.PublicKey().Bytes(), asPub, 32)
	prk := hkdfExtractV(salt, ikm)
	cek := hkdfExpandOnly(prk, []byte("Content-Encoding: aes128gcm\x00"), nil, nil, 16)
	nonce := hkdfExpandOnly(prk, []byte("Content-Encoding: nonce\x00"), nil, nil, 12)

	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, nil)
}

var errShortBody = errors.New("push: body too short to parse")

// TestTheTokenClaimSaysTheOriginNotTheEndpoint reads the claim it actually signed, rather than
// trusting the helper that builds it. The endpoint URL in a token's aud makes the token valid
// at no push service at all, and the failure looks like a permissions problem.
func TestTheTokenClaimSaysTheOriginNotTheEndpoint(t *testing.T) {
	v, err := NewVAPID("mailto:test@example.org")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "https://fcm.googleapis.com/fcm/send/abc123"
	hdr, err := v.authHeader(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimPrefix(strings.Split(hdr, ",")[0], "vapid t=")
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d segments", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Aud != "https://fcm.googleapis.com" {
		t.Fatalf("aud = %q, want the origin without the path", claims.Aud)
	}
	if strings.Contains(claims.Aud, "abc123") {
		t.Fatalf("aud carries the endpoint path: %q", claims.Aud)
	}
	// Under 24h, because a push service rejects anything longer and the rejection is opaque.
	if life := time.Until(time.Unix(claims.Exp, 0)); life > 24*time.Hour || life < time.Minute {
		t.Fatalf("token lives %v", life)
	}
	if claims.Sub == "" {
		t.Fatal("sub is empty, which some push services reject")
	}
}

// TestTheSignatureIsAlways64Bytes: JWS wants R and S zero-padded to the curve size, and roughly
// one key in 256 produces a 31-byte scalar. A version that concatenates the raw big-endian
// integers works almost always and fails on those keys - which is the worst kind of bug,
// because it only appears in production and only for some users.
func TestTheSignatureIsAlways64Bytes(t *testing.T) {
	for i := 0; i < 400; i++ {
		v, err := NewVAPID("mailto:test@example.org")
		if err != nil {
			t.Fatal(err)
		}
		hdr, err := v.authHeader("https://push.example.net/x")
		if err != nil {
			t.Fatal(err)
		}
		token := strings.TrimPrefix(strings.Split(hdr, ",")[0], "vapid t=")
		parts := strings.Split(token, ".")
		sig, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			t.Fatal(err)
		}
		if len(sig) != 64 {
			t.Fatalf("attempt %d: signature is %d bytes, want 64 - a short R or S was not padded", i, len(sig))
		}
	}
}
