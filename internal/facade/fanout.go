package facade

import (
	"io"
	"sync"
	"sync/atomic"
)

// Subscriber is one attached client attachment. It owns two preallocated
// outbound buffers: one being filled by Broadcast and one being written to the
// socket by the attachment's writer goroutine.
type Subscriber struct {
	w io.Writer

	mu       sync.Mutex
	buf      []byte
	out      []byte
	scratch  []byte
	n        int
	outN     int
	nick     string
	extended bool
	closed   bool

	wake    chan struct{}
	done    chan struct{}
	closeMu sync.Once

	// closer, when set, is closed together with the subscriber. The facade uses
	// it to tear down the TCP connection of a client that fell behind.
	closer io.Closer
}

// NewSubscriber allocates all buffers for one client. bufSize is the outbound
// buffer size in bytes (clamped to at least 4096).
func NewSubscriber(w io.Writer, bufSize int) *Subscriber {
	if bufSize < 4096 {
		bufSize = 4096
	}
	return &Subscriber{
		w:       w,
		buf:     make([]byte, bufSize),
		out:     make([]byte, bufSize),
		scratch: make([]byte, 0, bufSize),
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
}

// SetIdentity records the nick and extended-join state of the attached client.
// It is called before replay and whenever the client changes either.
func (s *Subscriber) SetIdentity(nick string, extendedJoin bool) {
	s.mu.Lock()
	s.nick = nick
	s.extended = extendedJoin
	s.mu.Unlock()
}

// Write copies one framed IRC line (already CRLF-terminated by the caller) into
// the buffer without rewriting it. It returns false if the line does not fit;
// the caller drops the client. Write does not allocate.
func (s *Subscriber) Write(line []byte) bool {
	return s.WriteFrom(line, "")
}

// WriteFrom copies one framed IRC line into the buffer, rewriting the upstream
// identity to this client's own nick when upstreamNick is non-empty. Lines that
// need no rewrite are copied byte-for-byte. A rewrite is spliced through the
// subscriber's scratch buffer, so the steady state does not allocate.
func (s *Subscriber) WriteFrom(line []byte, upstreamNick string) bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false
	}

	out, changed, drop := rewriteLine(s.scratch[:0], line, s.nick, upstreamNick, s.extended)
	if drop {
		s.scratch = out[:0]
		s.mu.Unlock()
		return true
	}
	if changed {
		s.scratch = out
	} else {
		out = line
	}

	if s.n+len(out) > len(s.buf) {
		s.mu.Unlock()
		return false
	}
	s.n += copy(s.buf[s.n:], out)
	s.mu.Unlock()

	select {
	case s.wake <- struct{}{}:
	default:
	}
	return true
}

// Flush writes the accumulated buffer to the writer. It is called only by the
// subscriber's own writer goroutine. The buffer is swapped before the write, so
// a slow socket never blocks Broadcast on the subscriber's mutex.
func (s *Subscriber) Flush() error {
	s.mu.Lock()
	s.buf, s.out = s.out, s.buf
	s.outN = s.n
	s.n = 0
	s.mu.Unlock()

	p := s.out[:s.outN]
	for len(p) > 0 {
		n, err := s.w.Write(p)
		if n > 0 {
			p = p[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// Close marks the subscriber closed and unblocks its goroutine.
func (s *Subscriber) Close() {
	s.closeMu.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()

		close(s.done)
		if s.closer != nil {
			_ = s.closer.Close()
		}
	})
}

// Done returns a channel closed when Close is called.
func (s *Subscriber) Done() <-chan struct{} { return s.done }

// Hub is the set of attached subscribers.
type Hub struct {
	mu      sync.Mutex
	subs    []*Subscriber
	n       int
	max     int
	dropped atomic.Uint64
}

// NewHub returns a hub holding at most maxSubs subscribers.
func NewHub(maxSubs int) *Hub {
	if maxSubs < 0 {
		maxSubs = 0
	}
	return &Hub{
		subs: make([]*Subscriber, maxSubs),
		max:  maxSubs,
	}
}

// Add registers a subscriber. It returns false if maxSubs is reached.
func (h *Hub) Add(s *Subscriber) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.n >= h.max {
		return false
	}
	h.subs[h.n] = s
	h.n++
	return true
}

// Remove unregisters a subscriber.
func (h *Hub) Remove(s *Subscriber) {
	h.mu.Lock()
	for i := 0; i < h.n; i++ {
		if h.subs[i] == s {
			h.n--
			h.subs[i] = h.subs[h.n]
			h.subs[h.n] = nil
			break
		}
	}
	h.mu.Unlock()
}

// Broadcast copies line into every subscriber's buffer without identity
// rewriting. See BroadcastFrom for the normal upstream path.
func (h *Hub) Broadcast(line []byte) {
	h.BroadcastFrom(line, "")
}

// BroadcastFrom copies line into every subscriber's buffer, rewriting the
// upstream nick to each subscriber's own nick. A subscriber whose buffer is
// full is removed and closed, and the dropped counter increments. No socket is
// written here and no subscriber can block the broadcast.
func (h *Hub) BroadcastFrom(line []byte, upstreamNick string) {
	h.mu.Lock()
	i := 0
	for i < h.n {
		s := h.subs[i]
		if s.WriteFrom(line, upstreamNick) {
			i++
			continue
		}

		s.Close()
		h.dropped.Add(1)
		h.n--
		h.subs[i] = h.subs[h.n]
		h.subs[h.n] = nil
	}
	h.mu.Unlock()
}

// Len returns the number of attached subscribers.
func (h *Hub) Len() int {
	h.mu.Lock()
	n := h.n
	h.mu.Unlock()
	return n
}

// Dropped returns the cumulative number of slow clients dropped.
func (h *Hub) Dropped() uint64 { return h.dropped.Load() }
