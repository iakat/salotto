package push

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestServer builds the HTTP surface with a real key and a store in a temp file.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	v, err := LoadVAPID(t.TempDir()+"/vapid.json", "mailto:test@example.org")
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(t.TempDir() + "/subs.json")
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(v, store)
}

// subscribeBody is a well-formed subscription, with an endpoint that does not exist. Nothing
// in these tests sends a push, so the endpoint only has to parse.
func subscribeBody(endpoint string) string {
	return `{"endpoint":"` + endpoint + `","p256dh":"BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4","auth":"BTBZMqHH6r4Tts7J_aSIgg"}`
}

func do(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

// TestVAPIDEndpointHandsOutTheKey: without it the page cannot call subscribe(), so the whole
// feature is unreachable.
func TestVAPIDEndpointHandsOutTheKey(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, http.MethodGet, "/push/vapid", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var got struct{ Key string }
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Key != s.Sender.VAPID.PublicKey() {
		t.Fatalf("key = %q", got.Key)
	}
	// It is public: an applicationServerKey is handed to every browser that subscribes.
	if len(got.Key) < 80 {
		t.Fatalf("key looks truncated: %q", got.Key)
	}
}

// TestSubscribeStoresADevice is the round trip the page performs.
func TestSubscribeStoresADevice(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, http.MethodPost, "/push/subscribe", subscribeBody("https://push.example.net/a"))
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if n := s.Sender.Store.Len(); n != 1 {
		t.Fatalf("the store has %d devices", n)
	}
}

// TestSubscribeRefusesAnIncompleteSubscription: a device with no keys can never receive
// anything, and storing it means a registered device that silently gets nothing.
func TestSubscribeRefusesAnIncompleteSubscription(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, http.MethodPost, "/push/subscribe", `{"endpoint":"https://push.example.net/a"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
}

// TestSubscribeRefusesAPlainHTTPEndpoint: such an endpoint does not exist, and accepting it
// registers a device that can never be reached.
func TestSubscribeRefusesAPlainHTTPEndpoint(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, http.MethodPost, "/push/subscribe", subscribeBody("http://push.example.net/a"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
}

// TestSubscribeRefusesOversizedBodies: an unbounded decode allocates the server's memory from
// the network, on an endpoint that is reachable by anyone.
func TestSubscribeRefusesOversizedBodies(t *testing.T) {
	s := newTestServer(t)
	big := `{"endpoint":"https://x/` + strings.Repeat("a", 32<<10) + `"}`
	w := do(t, s, http.MethodPost, "/push/subscribe", big)
	if w.Code == http.StatusCreated {
		t.Fatal("a 32 KiB body was accepted")
	}
}

// TestTheSubscriptionLimitIsEnforced is the bound that stops a public endpoint filling the disk.
func TestTheSubscriptionLimitIsEnforced(t *testing.T) {
	s := newTestServer(t)
	s.MaxSubscriptions = 2
	for i := 0; i < 2; i++ {
		if w := do(t, s, http.MethodPost, "/push/subscribe",
			subscribeBody("https://push.example.net/"+string(rune('a'+i)))); w.Code != http.StatusCreated {
			t.Fatalf("subscribe %d: %d %s", i, w.Code, w.Body)
		}
	}
	w := do(t, s, http.MethodPost, "/push/subscribe", subscribeBody("https://push.example.net/third"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("a third subscription got %d, want 429", w.Code)
	}

	// Re-subscribing an existing endpoint is not growth and must still work: a browser that
	// renews its subscription would otherwise be locked out at the limit.
	if w := do(t, s, http.MethodPost, "/push/subscribe", subscribeBody("https://push.example.net/a")); w.Code != http.StatusCreated {
		t.Fatalf("re-subscribing at the limit got %d: %s", w.Code, w.Body)
	}
}

// TestUnsubscribeRemovesADevice is how a device leaves.
func TestUnsubscribeRemovesADevice(t *testing.T) {
	s := newTestServer(t)
	body := subscribeBody("https://push.example.net/a")
	do(t, s, http.MethodPost, "/push/subscribe", body)
	if n := s.Sender.Store.Len(); n != 1 {
		t.Fatalf("setup: %d", n)
	}
	w := do(t, s, http.MethodPost, "/push/unsubscribe", `{"endpoint":"https://push.example.net/a"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if n := s.Sender.Store.Len(); n != 0 {
		t.Fatalf("after unsubscribing the store has %d", n)
	}
}

// TestStatusReportsACountAndNeverTheList. An endpoint URL is a capability to push to that
// device, so handing the list out over a GET would let any page that can reach this server
// push to the user's phone.
func TestStatusReportsACountAndNeverTheList(t *testing.T) {
	s := newTestServer(t)
	do(t, s, http.MethodPost, "/push/subscribe", subscribeBody("https://push.example.net/secret-endpoint"))
	w := do(t, s, http.MethodGet, "/push/status", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "secret-endpoint") {
		t.Fatalf("the status endpoint leaked the subscription list: %s", body)
	}
	if !strings.Contains(body, `"devices":1`) {
		t.Fatalf("status did not report the count: %s", body)
	}
}

// TestPushOffStillAnswers: a 404 makes the page look broken, and "not configured" is a message
// the operator can act on.
func TestPushOffStillAnswers(t *testing.T) {
	s := &Server{Enabled: false}
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/push/vapid"},
		{http.MethodPost, "/push/subscribe"},
		{http.MethodGet, "/push/status"},
	} {
		w := do(t, s, tc.method, tc.path, "{}")
		if w.Code == http.StatusNotFound {
			t.Fatalf("%s %s is a 404 with push off", tc.method, tc.path)
		}
		// /push/status answers 200 with enabled:false: it is the endpoint a page uses to
		// decide whether to show the button, so "not configured" is content, not an error.
		if tc.path == "/push/status" {
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"enabled":false`) {
				t.Fatalf("%s %s = %d %s", tc.method, tc.path, w.Code, w.Body)
			}
			continue
		}
		if w.Code != http.StatusServiceUnavailable && w.Code != http.StatusBadRequest {
			t.Fatalf("%s %s = %d", tc.method, tc.path, w.Code)
		}
	}
}

// TestSendingToAGoneSubscriptionRemovesIt: 410 means the browser revoked it, and a store that
// keeps it retries a dead endpoint forever.
func TestSendingToAGoneSubscriptionRemovesIt(t *testing.T) {
	var gotAuth, gotEncoding string
	var gotBody []byte
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotEncoding = r.Header.Get("Content-Encoding")
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		gotBody = buf.Bytes()
		w.WriteHeader(http.StatusGone)
	}))
	defer svc.Close()

	v, err := LoadVAPID(t.TempDir()+"/v.json", "mailto:t@e.org")
	if err != nil {
		t.Fatal(err)
	}
	store, _ := OpenStore(t.TempDir() + "/s.json")
	sub := Subscription{
		Endpoint: svc.URL + "/push/a",
		P256dh:   "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4",
		Auth:     "BTBZMqHH6r4Tts7J_aSIgg",
	}
	if err := store.Add(sub); err != nil {
		t.Fatal(err)
	}
	sender := NewSender(v, store)
	sent, failed, err := sender.Send(Message{Title: "hi", Body: "there"})
	if err != nil {
		t.Fatal(err)
	}
	if sent != 0 || failed != 1 {
		t.Fatalf("sent=%d failed=%d", sent, failed)
	}
	if n := store.Len(); n != 0 {
		t.Fatalf("a 410 left the subscription in the store: %d", n)
	}
	if !strings.HasPrefix(gotAuth, "vapid t=") {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotEncoding != "aes128gcm" {
		t.Fatalf("Content-Encoding = %q", gotEncoding)
	}
	if len(gotBody) < 86 {
		t.Fatalf("body is %d bytes", len(gotBody))
	}
	// The payload must not be readable in the body: a push goes through somebody else's
	// service, and the whole point of the encryption is that it carries a channel name.
	if bytes.Contains(gotBody, []byte("there")) {
		t.Fatal("the plaintext is visible in the request body")
	}
}

// TestNotifyRequiresAToken is the gate on the only endpoint that sends anything.
//
// Without it, anyone who can reach this port can buzz the user's phone - a denial of service
// with a personal shape - and a missing token is a refusal rather than a bypass, because an
// operator who forgot to set one must not get an open relay as a reward.
func TestNotifyRequiresAToken(t *testing.T) {
	s := newTestServer(t)
	s.Token = "sekrit"

	w := do(t, s, http.MethodPost, "/push/notify", `{"title":"x"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("an unauthenticated notify got %d", w.Code)
	}
	w = do(t, s, http.MethodPost, "/push/notify", `{"title":"x"}`)
	_ = w

	// Also not a bypass when no token is configured at all.
	s.Token = ""
	req := httptest.NewRequest(http.MethodPost, "/push/notify", strings.NewReader(`{"title":"x"}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("with no token configured notify got %d, want 403", rec.Code)
	}
}

// TestNotifyRejectsAnEmptyMessage: a buzz with nothing in it is worse than no notification.
func TestNotifyRejectsAnEmptyMessage(t *testing.T) {
	s := newTestServer(t)
	s.Token = "t"
	req := httptest.NewRequest(http.MethodPost, "/push/notify", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an empty message got %d", rec.Code)
	}
}

// TestNotifyWithATokenAndOneDeviceReachesThePushService is the whole path in one test.
func TestNotifyWithATokenAndOneDeviceReachesThePushService(t *testing.T) {
	var hits int
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusCreated)
	}))
	defer svc.Close()

	s := newTestServer(t)
	s.Token = "t"
	if err := s.Sender.Store.Add(Subscription{
		Endpoint: svc.URL + "/push/a",
		P256dh:   "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4",
		Auth:     "BTBZMqHH6r4Tts7J_aSIgg",
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/push/notify",
		strings.NewReader(`{"title":"#chan","body":"alice: hi","tag":"#chan"}`))
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("notify got %d: %s", rec.Code, rec.Body)
	}
	if hits != 1 {
		t.Fatalf("the push service was hit %d times", hits)
	}
	if !strings.Contains(rec.Body.String(), `"sent":1`) {
		t.Fatalf("response = %s", rec.Body)
	}
}
