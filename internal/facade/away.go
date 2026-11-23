package facade

import (
	"strings"
	"sync"
	"time"

	"github.com/iakat/salotto/internal/ircline"
)

// defaultAwayReason is simple_away's SIMPLE_AWAY_DEFAULT_REASON
// (simple_away.cpp:21).
const defaultAwayReason = "Auto away at %awaytime%"

// away is ZNC's simple_away module: when the last client detaches, wait a while
// and then mark the user away on IRC; when a client attaches again, come back.
//
// The two booleans are the whole subtlety of the module. ZNC tracks
// m_bClientSetAway and m_bWeSetAway (simple_away.cpp:44-45) so that it never
// fights the user: if the client itself sent an AWAY we do not overwrite it, and
// coming back only clears an away *we* set. Without that, a bouncer that
// reconnects would silently un-away someone who meant to be away.
type away struct {
	mu sync.Mutex

	enabled   bool
	reason    string
	wait      time.Duration
	timer     *time.Timer
	weSet     bool
	clientSet bool

	// messages counts private messages that arrived while we were away, which is
	// awaystore's "You have N messages!" (awaystore.cpp:388-389).
	messages awayMessages

	// send is what talks to the networks. It is a function so the away logic
	// stays testable without a socket and so it can fan out over every network
	// the facade knows.
	send func(line string)
}

// newAway returns away handling. A zero wait disables the automatic behaviour
// but leaves manual state tracking on, which is what ZNC does with SetTimer 0:
// the module still knows whether it set you away.
func newAway(enabled bool, reason string, wait time.Duration, send func(string)) *away {
	return &away{enabled: enabled, reason: reason, wait: wait, send: send}
}

// notePrivate records a private message that arrived while we were away.
func (a *away) notePrivate() {
	a.mu.Lock()
	if a.weSet {
		a.messages.add()
	}
	a.mu.Unlock()
}

// clientAttached cancels a pending auto-away and comes back if we were the ones
// who set it (simple_away.cpp:122 OnClientLogin → SetBack). announce, if not nil,
// tells *this* client how much arrived while we were away; it is a parameter
// rather than a field because two clients can register at the same time and a
// shared field would both race and deliver one client's backlog to the other.
func (a *away) clientAttached(announce func(n int, capped bool)) {
	a.mu.Lock()
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	clear := a.weSet
	if clear {
		a.weSet = false
	}
	n, capped := a.messages.take()
	a.mu.Unlock()
	if clear {
		a.send("AWAY")
	}
	// awaystore's OnClientLogin: the count goes to the client that came back.
	if n > 0 && announce != nil {
		announce(n, capped)
	}
}

// clientDetached starts the away timer when the last client leaves
// (simple_away.cpp:126 OnClientDisconnect → SetAway). A second detach while the
// timer is already running does not restart it, so a flapping client does not
// postpone the away forever.
func (a *away) clientDetached() {
	a.mu.Lock()
	if !a.enabled || a.wait <= 0 || a.timer != nil || a.weSet || a.clientSet {
		a.mu.Unlock()
		return
	}
	a.timer = time.AfterFunc(a.wait, a.fire)
	a.mu.Unlock()
}

func (a *away) fire() {
	a.mu.Lock()
	a.timer = nil
	// Expand at fire time, not at detach: %awaytime% means the moment you were
	// actually marked away, which is also when the timer ran (simple_away.cpp:213
	// expands inside SetAway).
	reason := expandAwayTime(a.reason, time.Now())
	a.weSet = true
	// Counting starts now, not at detach: the count answers "what arrived while
	// the server said I was gone", which is the only interval the module owns.
	a.messages.reset()
	a.mu.Unlock()

	if reason == "" {
		// ZNC never sends an empty reason: ExpandReason falls back to the default
		// (simple_away.cpp:211). An unconfigured reason therefore still says
		// "Auto away at <time>".
		reason = expandAwayTime(defaultAwayReason, time.Now())
	}
	a.send("AWAY :" + reason)
}

// noteClientAway records an AWAY the user sent, so the auto-away does not
// overwrite a deliberate state. ZNC sets m_bClientSetAway from the module hook;
// here the facade calls it when it sees AWAY on the client's wire.
func (a *away) noteClientAway(text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if text == "" {
		a.clientSet = false
		a.weSet = false
		return
	}
	a.clientSet = true
	a.weSet = false
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
}

// reasonText is what a client sees when it asks; %awaytime% is ZNC's
// substitution for the time the away was set (simple_away.cpp:13).
func (a *away) reasonText(now time.Time) string {
	a.mu.Lock()
	r := a.reason
	a.mu.Unlock()
	return expandAwayTime(r, now)
}

// expandAwayTime replaces ZNC's %awaytime% token with a timestamp.
//
// The format is not a choice: simple_away expands the token with
// `CUtils::CTime(iTime, "Etc/UTC") + " UTC"` (simple_away.cpp:213-215), and
// CUtils::CTime is `FormatTime(t, "%c", tz)` (Utils.cpp:510-512). In the C locale
// that is "%a %b %e %H:%M:%S %Y", which is Go's "Mon Jan _2 15:04:05 2006" — note
// the space-padded day, so a single-digit date is "Nov  3", not "Nov 03".
//
// ZNC also runs ExpandString over the result, which is a wider substitution
// language. Only the one token the default reason uses is implemented: a token
// nobody sends is a feature nobody has asked for.
func expandAwayTime(reason string, now time.Time) string {
	if !strings.Contains(reason, "%awaytime%") {
		return reason
	}
	return strings.ReplaceAll(reason, "%awaytime%", now.UTC().Format("Mon Jan _2 15:04:05 2006")+" UTC")
}

// handleAway intercepts a client's AWAY: it records whether the user set the
// away themselves (so simple_away will not fight it) and then lets the line go
// to the network like any other. ZNC's module does the same from its
// OnUserRaw/OnUserMsg hook — the command is not answered by the bouncer, it is
// forwarded.
func (c *client) handleAway(rest []byte) bool {
	text := string(ircline.Params(rest))
	text = strings.TrimPrefix(text, ":")
	c.srv.away.noteClientAway(text)
	return c.forward(append(c.scratch[:0], rest...))
}

// stop cancels a pending timer, for shutdown.
func (a *away) stop() {
	a.mu.Lock()
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	a.mu.Unlock()
}
