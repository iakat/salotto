package push

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Handler serves the self-service push endpoints a PWA needs.
//
// Four endpoints, and each exists because the browser cannot do it alone:
//
//	GET  /push/vapid        the applicationServerKey, so subscribe() can be called
//	POST /push/subscribe    a new subscription
//	POST /push/unsubscribe  revocation, so a device can leave
//	GET  /push/status       how many devices are registered, for the page to say
//
// The subscription endpoints are POST with a JSON body and no CSRF token, which is deliberate
// and is the one thing about this surface that needs saying out loud: a cross-origin page that
// can POST here can register *its own* endpoint, and the only thing that gets is a notification
// delivered to that page's push service - it cannot read anything, and it cannot see another
// device's subscription. What it must never do is leak the list, so GET returns a count and
// never the endpoints: an endpoint URL is a capability to push to that device.
func (s *Server) register(mux *http.ServeMux) {
	mux.HandleFunc("/push/vapid", s.handleVAPID)
	mux.HandleFunc("/push/subscribe", s.handleSubscribe)
	mux.HandleFunc("/push/unsubscribe", s.handleUnsubscribe)
	mux.HandleFunc("/push/status", s.handleStatus)
	mux.HandleFunc("/push/notify", s.handleNotify)
}

// Server is the public surface: a Sender plus what the handlers need.
type Server struct {
	Sender *Sender
	// Token, when set, is required as "Authorization: Bearer <token>" on /push/notify. That
	// endpoint is the only one that sends anything, and it exists so the *bouncer* can trigger
	// a notification: the front end knows who is attached, and the bouncer knows what arrived.
	// An unauthenticated send endpoint is a way for anyone who can reach this port to buzz the
	// user's phone, which is a denial of service with a personal shape.
	Token string
	// Enabled is false when no key was configured: the endpoints still answer, because a page
	// that 404s a subscribe looks broken, and "push is not configured" is a better message.
	Enabled bool
	// MaxSubscriptions bounds the store. A public subscribe endpoint with no bound is a way
	// to fill the disk from the internet, and the honest limit for a single-user bouncer is
	// "the user's devices", not a million.
	MaxSubscriptions int
}

// NewServer builds the HTTP surface.
func NewServer(v *VAPID, store *Store) *Server {
	if v == nil || store == nil {
		return &Server{Enabled: false}
	}
	return &Server{
		Sender:           NewSender(v, store),
		Enabled:          true,
		MaxSubscriptions: 32,
	}
}

// Handler returns a mux serving the push endpoints.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.register(mux)
	return mux
}

// handleVAPID returns the applicationServerKey.
func (s *Server) handleVAPID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if !s.Enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "push is not configured on this server",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": s.Sender.VAPID.PublicKey()})
}

// handleSubscribe stores a subscription.
func (s *Server) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !s.Enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "push is not configured"})
		return
	}

	var sub Subscription
	// A body limit: an unbounded decode is a way to allocate the server's memory from the
	// network, and a subscription is a few hundred bytes.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&sub); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "malformed subscription"})
		return
	}
	if !strings.HasPrefix(sub.Endpoint, "https://") {
		// Plain http endpoints do not exist, and accepting one would mean a stored URL that
		// can never work - which is worse than refusing it, because the operator would see a
		// device registered and receive nothing.
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "endpoint must be https"})
		return
	}
	if s.MaxSubscriptions > 0 && s.Sender.Store.Len() >= s.MaxSubscriptions {
		// Re-subscribing an existing endpoint is not growth, so check before refusing.
		known := false
		for _, existing := range s.Sender.Store.List() {
			if existing.Endpoint == sub.Endpoint {
				known = true
				break
			}
		}
		if !known {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"error": "subscription limit reached",
			})
			return
		}
	}

	if err := s.Sender.Store.Add(sub); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true})
}

// handleUnsubscribe forgets an endpoint.
func (s *Server) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil || body.Endpoint == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "endpoint required"})
		return
	}
	if s.Enabled {
		if err := s.Sender.Store.Remove(body.Endpoint); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not remove"})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleStatus reports how many devices are registered - a count, never the list. An endpoint
// URL is a capability to push to that device, so it is not something a GET hands out.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	n := 0
	if s.Enabled {
		n = s.Sender.Store.Len()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": s.Enabled,
		"devices": n,
	})
}

// writeJSON writes a JSON response with the right content type.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Notifier is what the bouncer side needs: build a Message and hand it over.
//
// It is an interface in the consumer's style so the daemon does not import this package's
// crypto just to say "there is a message".
type Notifier interface {
	Notify(msg Message) (sent, failed int)
}

// Notify implements Notifier.
func (s *Server) Notify(msg Message) (int, int) {
	if s == nil || !s.Enabled {
		return 0, 0
	}
	sent, failed, err := s.Sender.Send(msg)
	if err != nil {
		return sent, failed
	}
	return sent, failed
}

// Timeout is how long a single push send may take. Exported so the daemon can use the same
// number in a context deadline rather than guessing.
const Timeout = 10 * time.Second

// handleNotify sends a notification. This is the bouncer's entry point.
func (s *Server) handleNotify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !s.Enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "push is not configured"})
		return
	}
	// A missing token is a refusal, not a bypass: an operator who forgot to set one must not
	// get an open relay as a reward.
	if s.Token == "" || r.Header.Get("Authorization") != "Bearer "+s.Token {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
		return
	}
	var msg Message
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&msg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "malformed message"})
		return
	}
	// A notification with no title and no body is a buzz with nothing in it.
	if msg.Title == "" && msg.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "empty message"})
		return
	}
	sent, failed := s.Notify(msg)
	writeJSON(w, http.StatusOK, map[string]any{"sent": sent, "failed": failed})
}
