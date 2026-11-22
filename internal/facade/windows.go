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
// mute flags, plus a per-window clear floor. Nothing is persisted; a client
// that is not attached simply has no state and starts from Oldest().
//
// The rings are still one per network. Per-window cursors index that shared
// ring, so replay for a window is an approximation: a busy window can push a
// quiet window's lines out before the quiet window's client reads them.
type windowState struct {
	mu       sync.Mutex
	clients  map[*Subscriber]*clientWindows
	known    map[string]bool
	disabled map[string]bool
	floor    map[string]uint64
}

func newWindowState() *windowState {
	return &windowState{
		clients:  make(map[*Subscriber]*clientWindows),
		known:    make(map[string]bool),
		disabled: make(map[string]bool),
		floor:    make(map[string]uint64),
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

func (ws *windowState) cleared(key string) uint64 {
	ws.mu.Lock()
	floor := ws.floor[key]
	ws.mu.Unlock()
	return floor
}

func (ws *windowState) clear(key string, newest uint64) {
	ws.mu.Lock()
	ws.known[key] = true
	ws.floor[key] = newest
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

// hasAnyAttach reports whether sub is attached to at least one window. The
// status fallback uses it to keep clients that asked for window delivery off
// the catch-all stream.
func (ws *windowState) hasAnyAttach(sub *Subscriber) bool {
	ws.mu.Lock()
	cw := ws.clients[sub]
	any := false
	if cw != nil {
		for _, attached := range cw.attached {
			if attached {
				any = true
				break
			}
		}
	}
	ws.mu.Unlock()
	return any
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

// DetachWindow stops delivery for a window and freezes its cursor. It reports
// whether the window was attached.
func (s *Server) DetachWindow(sub *Subscriber, network, window string) bool {
	return s.windows.detach(sub, windowKey(network, window))
}

// SetDisabled sets the not-buffered flag for a window. ZNC's flag also stops
// the channel being re-joined after a reconnect; we do not manage joins at
// runtime, so the observable effect here is "not buffered". Re-enabling
// advances the clear floor to the newest line so only subsequent lines can be
// replayed: a disabled window must not replay a stale buffer. It reports
// whether the flag changed.
func (s *Server) SetDisabled(network, window string, disabled bool) bool {
	key := windowKey(network, window)
	changed := s.windows.setDisabled(key, disabled)
	if changed && !disabled {
		if ne := s.network(network); ne != nil {
			s.windows.clear(key, ne.net.Newest())
		}
	}
	return changed
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

// BufferEmpty reports whether a window has nothing to replay: it is disabled,
// the shared ring has no lines, or a clear floor hides everything.
func (s *Server) BufferEmpty(network, window string) bool {
	if s.Disabled(network, window) {
		return true
	}
	ne := s.network(network)
	if ne == nil {
		return true
	}
	newest := ne.net.Newest()
	if newest == 0 {
		return true
	}
	from := ne.net.Oldest()
	if floor := s.windows.cleared(windowKey(network, window)); floor >= from {
		from = floor + 1
	}
	return from > newest
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
// window path); every client with no attach gets it through the status fallback
// inside BroadcastWindows, which is BroadcastUnattached. Clients attached to
// any window never receive window lines from that fallback, so a client
// attached to "status" cannot see a channel line twice. Status lines themselves
// are not windowed: Broadcast is the status stream and delivers them to
// everyone, attached or not.
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
// with no explicit attach through the status fallback. No client receives it
// twice. It is the routing entry point for a non-status line.
func (s *Server) BroadcastWindows(netName string, windows []string, seq uint64, line []byte, ts int64) {
	_ = seq
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
		if attachedSet[sub] {
			return true
		}
		return s.windows.hasAnyAttach(sub)
	})
	*p = b
	s.linePool.Put(p)
}

// PlayBuffer replays each requested window from max(oldest, cursor+1). If the
// shared ring has already dropped the unread part of a client's cursor, a GAP
// marker is written first, matching docs/SPEC.md §6. Cursors index the shared
// per-network ring; per-window ring slices are a separate item.
func (s *Server) PlayBuffer(sub *Subscriber, network string, windows ...string) bool {
	ne := s.network(network)
	if ne == nil {
		return false
	}
	if len(windows) == 0 {
		windows = []string{"status"}
	}
	st, _ := ne.net.(stampSource)
	un := ne.knownNick()

	for _, window := range windows {
		key := windowKey(network, window)
		if s.windows.isDisabled(key) {
			continue
		}

		oldest := ne.net.Oldest()
		newest := ne.net.Newest()
		if newest == 0 {
			continue
		}
		from := oldest
		if floor := s.windows.cleared(key); floor >= from {
			from = floor + 1
		}

		if cursor, ok := s.windows.cursor(sub, key); ok {
			if cursor+1 > from {
				from = cursor + 1
			}
			if cursor+1 < oldest {
				// At least one line after the cursor is gone from the shared
				// ring. Report the lost range before replaying the survivors.
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
		ok := true
		ne.net.Replay(from, func(seq uint64, line []byte) bool {
			ts := time.Now().UnixNano()
			if st != nil {
				if v, has := st.Stamp(seq); has && v != 0 {
					ts = v
				}
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
	}
	return true
}

// ClearBuffer hides one window's already-broadcast lines and advances every
// client's cursor past them, so a cleared buffer is not replayed. The shared
// ring is not physically erased (that needs per-window rings); a clear floor
// makes the old lines invisible to this window from then on. It is a no-op with
// an explanatory message when the window is unknown.
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
	s.windows.clear(key, ne.net.Newest())
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
