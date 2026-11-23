package facade

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// windowKey identifies a replay window the way the log does: network + window
// name. The server window is "status".
func windowKey(network, window string) string {
	if window == "" {
		window = "status"
	}
	return network + "/" + window
}

type clientWindows struct {
	attached map[string]bool
	muted    map[string]bool
	cursors  map[string]uint64
}

func newClientWindows() *clientWindows {
	return &clientWindows{
		attached: make(map[string]bool),
		muted:    make(map[string]bool),
		cursors:  make(map[string]uint64),
	}
}

// windowState is the in-memory B2 state: per-client cursors, attach/detach and
// mute flags. Nothing is persisted; a client that is not attached simply has no
// state and starts from Oldest().
//
// A window is in exactly one of three states for a given client:
//
//	state                                  live delivery              PlayBuffer
//	attached (default when no opinion)     yes, once                  replays from cursor
//	detached (explicit DETACH)             no                         replays from cursor
//	status (no opinion, no attaches)       yes, once, via catch-all   n/a
//
// An absent key in clientWindows.attached means "no opinion"; the status
// catch-all applies. An explicit false means "detached": the client stated a
// preference, so neither the window path nor the catch-all may deliver to it.
// Muting has the same live effect as detaching here; the flags stay separate
// because the wire commands differ and LISTCHANS reports them differently.
//
// The rings are one per window (internal/winring). A per-window cursor indexes
// that window's own ring, so a busy window cannot evict a quiet window's lines
// and a GAP means this window really overflowed.
type windowState struct {
	mu       sync.Mutex
	clients  map[*Subscriber]*clientWindows
	known    map[string]bool
	disabled map[string]bool
}

func newWindowState() *windowState {
	return &windowState{
		clients:  make(map[*Subscriber]*clientWindows),
		known:    make(map[string]bool),
		disabled: make(map[string]bool),
	}
}

func (ws *windowState) client(sub *Subscriber) *clientWindows {
	cw := ws.clients[sub]
	if cw == nil {
		cw = newClientWindows()
		ws.clients[sub] = cw
	}
	return cw
}

func (ws *windowState) attach(sub *Subscriber, key string) bool {
	ws.mu.Lock()
	cw := ws.client(sub)
	was := cw.attached[key]
	cw.attached[key] = true
	ws.known[key] = true
	ws.mu.Unlock()
	return !was
}

func (ws *windowState) detach(sub *Subscriber, key string) bool {
	ws.mu.Lock()
	cw := ws.client(sub)
	was := cw.attached[key]
	cw.attached[key] = false
	ws.known[key] = true
	ws.mu.Unlock()
	return was
}

func (ws *windowState) mute(sub *Subscriber, key string, muted bool) {
	ws.mu.Lock()
	cw := ws.client(sub)
	ws.known[key] = true
	if muted {
		cw.muted[key] = true
	} else {
		delete(cw.muted, key)
	}
	ws.mu.Unlock()
}

func (ws *windowState) ack(sub *Subscriber, key string, seq uint64) {
	ws.mu.Lock()
	cw := ws.client(sub)
	ws.known[key] = true
	if seq > cw.cursors[key] {
		cw.cursors[key] = seq
	}
	ws.mu.Unlock()
}

func (ws *windowState) cursor(sub *Subscriber, key string) (uint64, bool) {
	ws.mu.Lock()
	cw := ws.clients[sub]
	var cursor uint64
	var ok bool
	if cw != nil {
		cursor, ok = cw.cursors[key]
	}
	ws.mu.Unlock()
	return cursor, ok
}

// clear marks the window known and advances every client's cursor past the
// lines being cleared. Clearing is deliberate, not loss: without this a client
// would see a GAP for lines the operator chose to drop. The ring itself is
// cleared separately because it lives in the network handle.
func (ws *windowState) clear(key string, newest uint64) {
	ws.mu.Lock()
	ws.known[key] = true
	for _, cw := range ws.clients {
		if cw.cursors[key] < newest {
			cw.cursors[key] = newest
		}
	}
	ws.mu.Unlock()
}

func (ws *windowState) delivering(key string) []*Subscriber {
	ws.mu.Lock()
	subs := make([]*Subscriber, 0, len(ws.clients))
	for sub, cw := range ws.clients {
		if cw.attached[key] && !cw.muted[key] {
			subs = append(subs, sub)
		}
	}
	ws.mu.Unlock()
	return subs
}

func (ws *windowState) forget(sub *Subscriber) {
	ws.mu.Lock()
	delete(ws.clients, sub)
	ws.mu.Unlock()
}

// liveState is a client's explicit relationship to one window.
type liveState int

const (
	// liveViaOther: the client has no opinion about this window but follows
	// some other window, so the catch-all must not reach it.
	liveViaOther liveState = iota
	// liveAttached: explicitly attached, or no opinion and the client has no
	// attaches at all. The latter is the status catch-all case.
	liveAttached
	// liveSuppressed: explicitly detached, or muted. No live delivery at all.
	liveSuppressed
)

// liveState reports a client's explicit relationship to a window:
// liveAttached (explicitly attached, or no opinion and the client has no
// attaches at all), liveSuppressed (explicitly detached), or liveViaOther.
//
// The distinction matters because an absent key is not the same as an explicit
// false: absent means "no opinion" and lets the catch-all apply, while false
// means "detached" and suppresses both the window path and the catch-all.
func (ws *windowState) liveState(sub *Subscriber, key string) liveState {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return liveStateLocked(ws.clients[sub], key)
}

// liveStateLocked is liveState with ws.mu held.
func liveStateLocked(cw *clientWindows, key string) liveState {
	if cw == nil {
		return liveAttached
	}
	if cw.muted[key] {
		return liveSuppressed
	}
	if attached, present := cw.attached[key]; present {
		if attached {
			return liveAttached
		}
		return liveSuppressed
	}
	if hasAnyAttachLocked(cw) {
		return liveViaOther
	}
	return liveAttached
}

// hasAnyAttachLocked reports whether cw is attached to at least one window,
// with ws.mu held.
func hasAnyAttachLocked(cw *clientWindows) bool {
	if cw == nil {
		return false
	}
	for _, attached := range cw.attached {
		if attached {
			return true
		}
	}
	return false
}

// deliveringAny returns each subscriber attached to at least one of keys and
// not muted on it, once per subscriber, in map order.
func (ws *windowState) deliveringAny(keys []string) []*Subscriber {
	ws.mu.Lock()
	seen := make(map[*Subscriber]bool, len(ws.clients))
	var subs []*Subscriber
	for sub, cw := range ws.clients {
		for _, key := range keys {
			if cw.attached[key] && !cw.muted[key] {
				seen[sub] = true
				break
			}
		}
		if seen[sub] {
			subs = append(subs, sub)
		}
	}
	ws.mu.Unlock()
	return subs
}

// skipCatchAll reports whether the status catch-all must leave sub alone for a
// line belonging to keys: sub already got the line directly, explicitly
// detached or muted one of the line's windows, or follows another window.
func (ws *windowState) skipCatchAll(sub *Subscriber, keys []string, direct bool) bool {
	if direct {
		return true
	}
	for _, key := range keys {
		switch ws.liveState(sub, key) {
		case liveSuppressed:
			return true
		case liveViaOther:
			return true
		}
	}
	return false
}

func (ws *windowState) setDisabled(key string, disabled bool) bool {
	ws.mu.Lock()
	ws.known[key] = true
	was := ws.disabled[key]
	if disabled {
		ws.disabled[key] = true
	} else {
		delete(ws.disabled, key)
	}
	ws.mu.Unlock()
	return was != disabled
}

func (ws *windowState) isDisabled(key string) bool {
	ws.mu.Lock()
	disabled := ws.disabled[key]
	ws.mu.Unlock()
	return disabled
}

func (ws *windowState) isAttached(sub *Subscriber, key string) bool {
	ws.mu.Lock()
	var attached bool
	if cw := ws.clients[sub]; cw != nil {
		attached = cw.attached[key]
	}
	ws.mu.Unlock()
	return attached
}

func (ws *windowState) knownWindows(network string) []string {
	prefix := network + "/"
	ws.mu.Lock()
	names := make([]string, 0, len(ws.known))
	for key := range ws.known {
		if strings.HasPrefix(key, prefix) {
			names = append(names, key[len(prefix):])
		}
	}
	ws.mu.Unlock()
	sort.Strings(names)
	return names
}

// AttachWindow records that sub wants delivery for a window. The cursor is not
// reset: detaching and re-attaching resumes where the client stopped. It
// reports whether the attachment was new.
func (s *Server) AttachWindow(sub *Subscriber, network, window string) bool {
	return s.windows.attach(sub, windowKey(network, window))
}

// DetachWindow stops live delivery for a window and freezes its cursor, so
// PlayBuffer resumes from where the client stopped. Detaching is an explicit
// preference: an absent key means "no opinion" (status catch-all applies),
// while the explicit false written here suppresses both the window path and the
// catch-all. It reports whether the window was attached.
func (s *Server) DetachWindow(sub *Subscriber, network, window string) bool {
	return s.windows.detach(sub, windowKey(network, window))
}

// SetDisabled sets the not-buffered flag for a window. Matching ZNC's
// CChan::Disable, the flag only suppresses the *rejoin* after a reconnect
// (IRCNetwork.cpp:998); it does not stop buffering nor replay, and it does not
// part a channel we are already joined to. The daemon reads the flag when it
// dials (upstream.Config.JoinChannels), which is where the rejoin is decided.
// It reports whether the flag changed.
func (s *Server) SetDisabled(network, window string, disabled bool) bool {
	return s.windows.setDisabled(windowKey(network, window), disabled)
}

// Disabled reports whether a window is not buffered.
func (s *Server) Disabled(network, window string) bool {
	return s.windows.isDisabled(windowKey(network, window))
}

// WindowAttached reports whether sub is attached to a window.
func (s *Server) WindowAttached(sub *Subscriber, network, window string) bool {
	return s.windows.isAttached(sub, windowKey(network, window))
}

// WindowStats returns the number of known windows for a network and how many of
// them are disabled.
func (s *Server) WindowStats(network string) (known, disabled int) {
	names := s.windows.knownWindows(network)
	for _, name := range names {
		if s.Disabled(network, name) {
			disabled++
		}
	}
	return len(names), disabled
}

// Known reports whether a window has ever been seen for this network.
func (s *Server) Known(network, window string) bool {
	key := windowKey(network, window)
	s.windows.mu.Lock()
	known := s.windows.known[key]
	s.windows.mu.Unlock()
	return known
}

// BufferEmpty reports whether a window has nothing to replay: its own ring has
// no lines. A disabled window is *not* empty: ZNC's DisableChan only suppresses
// the rejoin after a reconnect (CChan::Disable, IRCNetwork.cpp:998), it does not
// stop buffering or replay, so neither do we.
func (s *Server) BufferEmpty(network, window string) bool {
	ne := s.network(network)
	if ne == nil {
		return true
	}
	r, ok := ne.net.Window(window)
	if !ok {
		return true
	}
	return r.Newest() == 0
}

// matchWindows returns the known windows of network whose name matches any
// token in arg, using ZNC's rule (CClient::MatchChans, src/Client.cpp:1147):
// split on commas, then on spaces, union the per-token matches and de-duplicate.
// Matching is case-insensitive with '*' and '?' wildcards; a token with no
// wildcard matches the window name exactly.
//
// There is no FindChans to port: our window space is "status" plus channel
// names, so exact/glob matching is the whole rule. ZNC's nick-aware lookup and
// query namespace do not exist here.
func (s *Server) matchWindows(network, arg string) []string {
	seen := make(map[string]bool)
	for _, tok := range strings.FieldsFunc(arg, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	}) {
		for _, name := range s.windows.knownWindows(network) {
			if wildMatch(tok, name) {
				seen[name] = true
			}
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// wildMatch is ZNC's CString::WildCmp (src/ZNCString.cpp:175): '*' matches any
// run, '?' matches one byte, everything else compares case-insensitively.
func wildMatch(pattern, name string) bool {
	wild := strings.ToLower(pattern)
	str := strings.ToLower(name)
	p, s := 0, 0
	star, mark := -1, 0
	for s < len(str) {
		if p < len(wild) && (wild[p] == str[s] || wild[p] == '?') {
			p++
			s++
		} else if p < len(wild) && wild[p] == '*' {
			star = p
			mark = s
			p++
		} else if star >= 0 {
			p = star + 1
			mark++
			s = mark
		} else {
			return false
		}
	}
	for p < len(wild) && wild[p] == '*' {
		p++
	}
	return p == len(wild)
}

// MuteWindow suppresses live delivery for a window without forgetting its
// cursor. A muted window can still be replayed explicitly.
//
// In this in-memory model mute and detach are behaviourally identical: both
// mean "do not deliver live, do buffer, let PlayBuffer catch up". They stay
// separate flags because the wire commands differ and LISTCHANS must still
// report detach state (a muted window is not shown as detached there).
func (s *Server) MuteWindow(sub *Subscriber, network, window string, muted bool) {
	s.windows.mute(sub, windowKey(network, window), muted)
}

// AckWindow advances one client's cursor for a window. Cursors only move
// forward.
func (s *Server) AckWindow(sub *Subscriber, network, window string, seq uint64) {
	s.windows.ack(sub, windowKey(network, window), seq)
}

// WindowCursor returns the last acknowledged sequence for one client/window, if
// it has one. A client with no cursor starts from Oldest().
func (s *Server) WindowCursor(sub *Subscriber, network, window string) (uint64, bool) {
	return s.windows.cursor(sub, windowKey(network, window))
}

// BroadcastWindow delivers one live line to the clients attached to a window.
// It is additive: Broadcast keeps its broadcast-to-everyone behaviour for the
// POC client path, which does not negotiate windows.
//
// Routing rule: a line reaches a client at most once. Clients explicitly
// attached to one of the line's windows get it through BroadcastWindows (the
// window path); every client with no explicit attach gets it through the
// Broadcast catch-all inside BroadcastWindows. Clients attached to any window
// never receive window lines from that catch-all, so a client attached to
// "status" cannot see a channel line twice. Status lines themselves are not
// windowed: Broadcast is the status stream and delivers them to everyone,
// attached or not.
func (s *Server) BroadcastWindow(netName, window string, seq uint64, line []byte, ts int64) {
	_ = seq
	key := windowKey(netName, window)
	s.windows.mu.Lock()
	s.windows.known[key] = true
	s.windows.mu.Unlock()

	subs := s.windows.delivering(key)
	if len(subs) == 0 {
		return
	}

	var un string
	if ne := s.network(netName); ne != nil {
		forLine, _ := ne.advanceNick(ne.knownNick(), line)
		un = forLine
	}

	p := s.linePool.Get().(*[]byte)
	b := appendLiveLine((*p)[:0], line, ts)
	for _, sub := range subs {
		sub.WriteFrom(b, un)
	}
	*p = b
	s.linePool.Put(p)
}

// BroadcastWindows delivers one live line to the clients attached to any of
// windows (once, even when a client is attached to several) and to every client
// with no opinion through the status catch-all. A client that explicitly
// detached (or muted) one of windows gets the line live through neither path:
// it is buffered and PLAYBUFFER is how that client catches up. No client
// receives it twice. It is the routing entry point for a non-status line.
func (s *Server) BroadcastWindows(netName string, windows []string, seq uint64, line []byte, ts int64) {
	if len(windows) == 0 {
		return
	}
	keys := make([]string, 0, len(windows))
	s.windows.mu.Lock()
	for _, window := range windows {
		key := windowKey(netName, window)
		s.windows.known[key] = true
		keys = append(keys, key)
	}
	s.windows.mu.Unlock()

	attached := s.windows.deliveringAny(keys)
	attachedSet := make(map[*Subscriber]bool, len(attached))
	for _, sub := range attached {
		attachedSet[sub] = true
	}

	var un string
	if ne := s.network(netName); ne != nil {
		forLine, _ := ne.advanceNick(ne.knownNick(), line)
		un = forLine
	}

	p := s.linePool.Get().(*[]byte)
	b := appendLiveLine((*p)[:0], line, ts)
	for _, sub := range attached {
		sub.WriteFrom(b, un)
	}
	s.hub.BroadcastFromSkip(b, un, func(sub *Subscriber) bool {
		return s.windows.skipCatchAll(sub, keys, attachedSet[sub])
	})
	*p = b
	s.linePool.Put(p)
}

// PlayBuffer replays each requested window from that window's own ring, from
// max(oldest, cursor+1). If the window has already dropped the unread part of
// a client's cursor, a GAP marker is written first, matching docs/SPEC.md §6.
// With per-window rings a GAP means this window really overflowed.
func (s *Server) PlayBuffer(sub *Subscriber, network string, windows ...string) bool {
	ne := s.network(network)
	if ne == nil {
		return false
	}
	if len(windows) == 0 {
		windows = []string{"status"}
	}
	un := ne.knownNick()

	for _, window := range windows {
		key := windowKey(network, window)

		r, ok := ne.net.Window(window)
		if !ok {
			continue
		}
		oldest := r.Oldest()
		newest := r.Newest()
		if newest == 0 {
			continue
		}
		from := oldest

		if cursor, has := s.windows.cursor(sub, key); has {
			if cursor+1 > from {
				from = cursor + 1
			}
			if cursor+1 < oldest {
				// At least one line after the cursor is gone from this
				// window's ring. Report the lost range before replaying the
				// survivors.
				gap := appendGapLine(nil, s.cfg.ServerName, network, window, cursor+1, oldest-1)
				if !sub.Write(gap) {
					return false
				}
			}
		}
		if from > newest {
			continue
		}

		batch := network + "-" + window + "-replay"
		buf := make([]byte, 0, maxRingLine+512)
		ok = true
		r.Replay(from, func(seq uint64, line []byte) bool {
			ts := time.Now().UnixNano()
			if v, has := r.Stamp(seq); has && v != 0 {
				ts = v
			}
			forLine, next := ne.advanceNick(un, line)
			un = next
			buf = appendReplayLine(buf[:0], line, batch, ts)
			if !sub.WriteFrom(buf, forLine) {
				ok = false
				return false
			}
			return true
		})
		if !ok {
			return false
		}
		// A replay is a played line: advance the cursor so a second PlayBuffer
		// cannot replay it again. PlayBuffer is the deliberate catch-up path, so
		// replaying is the same as acknowledging for the client's cursor.
		s.windows.ack(sub, key, newest)
	}
	return true
}

// ClearBuffer physically drops one window's already-broadcast lines, so a
// cleared buffer is never replayed. The window's sequences stay monotonic: a
// stale cursor from a client is a well-formed GAP, not a silent rewind. It is a
// no-op with an explanatory message when the window is unknown.
func (s *Server) ClearBuffer(network, window string) (bool, string) {
	ne := s.network(network)
	if ne == nil {
		return false, fmt.Sprintf("unknown network %q", network)
	}
	key := windowKey(network, window)
	s.windows.mu.Lock()
	known := s.windows.known[key]
	s.windows.mu.Unlock()
	if !known {
		return false, fmt.Sprintf("unknown window %q", key)
	}
	var newest uint64
	if r, ok := ne.net.Window(window); ok {
		newest = r.Newest()
		r.Clear()
	}
	s.windows.clear(key, newest)
	return true, ""
}

func appendGapLine(dst []byte, server, network, window string, from, to uint64) []byte {
	dst = append(dst, ':')
	dst = append(dst, server...)
	dst = append(dst, " GAP "...)
	dst = append(dst, network...)
	dst = append(dst, ' ')
	dst = append(dst, window...)
	dst = append(dst, ' ')
	dst = appendUint(dst, from)
	dst = append(dst, ' ')
	dst = appendUint(dst, to)
	return append(dst, '\r', '\n')
}

func appendUint(dst []byte, v uint64) []byte {
	var b [20]byte
	i := len(b)
	if v == 0 {
		i--
		b[i] = '0'
	}
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return append(dst, b[i:]...)
}
