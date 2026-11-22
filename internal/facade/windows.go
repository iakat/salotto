package facade

import (
	"fmt"
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
	mu      sync.Mutex
	clients map[*Subscriber]*clientWindows
	known   map[string]bool
	floor   map[string]uint64
}

func newWindowState() *windowState {
	return &windowState{
		clients: make(map[*Subscriber]*clientWindows),
		known:   make(map[string]bool),
		floor:   make(map[string]uint64),
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

func (ws *windowState) attach(sub *Subscriber, key string) {
	ws.mu.Lock()
	ws.client(sub).attached[key] = true
	ws.known[key] = true
	ws.mu.Unlock()
}

func (ws *windowState) detach(sub *Subscriber, key string) {
	ws.mu.Lock()
	if cw := ws.clients[sub]; cw != nil {
		delete(cw.attached, key)
	}
	ws.mu.Unlock()
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

// AttachWindow records that sub wants delivery for a window. The cursor is not
// reset: detaching and re-attaching resumes where the client stopped.
func (s *Server) AttachWindow(sub *Subscriber, network, window string) {
	s.windows.attach(sub, windowKey(network, window))
}

// DetachWindow stops delivery for a window and freezes its cursor.
func (s *Server) DetachWindow(sub *Subscriber, network, window string) {
	s.windows.detach(sub, windowKey(network, window))
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
