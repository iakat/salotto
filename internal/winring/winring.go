// Package winring keeps one replay ring per window.
//
// It replaces the POC's single per-network ring. A Set owns a bounded number of
// live windows; each window has its own ring.Ring and its own sequence counter.
// A line shared by two windows is appended to both rings with independent
// sequences, so a busy window cannot evict a quiet window's lines.
package winring

import (
	"sort"
	"sync"

	"github.com/iakat/salotto/internal/ring"
)

// Set owns one ring per window name. It is safe for concurrent use.
type Set struct {
	mu      sync.Mutex
	segs    int
	segSize int
	max     int
	clock   uint64
	live    int
	windows map[string]*entry
}

type entry struct {
	// ring is nil for an evicted window. The entry is kept as a tombstone so a
	// window that reappears continues its sequence instead of restarting at 1.
	ring *ring.Ring
	// base is the last sequence emitted before the current ring was created.
	// It keeps sequences monotonic across Clear and eviction: a stale cursor is
	// a well-formed gap, not a silent rewind to an old sequence.
	base uint64
	used uint64
	// segs and segSize are the window's own ring geometry when SETBUFFER
	// resized it; zero means "use the Set's defaults".
	segs    int
	segSize int
}

// New returns a Set holding at most maxWindows live rings, each built with the
// same clamps as ring.New. maxWindows <= 0 means 1.
func New(maxWindows, segments, segSize int) *Set {
	if maxWindows < 1 {
		maxWindows = 1
	}
	return &Set{
		segs:    segments,
		segSize: segSize,
		max:     maxWindows,
		windows: make(map[string]*entry),
	}
}

// Append appends line to window and returns that window's sequence number. The
// sequence is monotonic for the lifetime of the Set's knowledge of the window,
// even across Clear and eviction.
func (s *Set) Append(window string, line []byte, ts int64) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	e := s.windows[window]
	if e == nil {
		e = s.create(window)
	} else if e.ring == nil {
		e.ring = ring.New(s.geo(e))
		s.live++
	}
	seq := e.base + e.ring.AppendStamp(line, ts)
	s.clock++
	e.used = s.clock
	return seq
}

// geo returns a window's ring geometry, falling back to the Set's defaults when
// SETBUFFER has not resized it.
func (s *Set) geo(e *entry) (int, int) {
	if e.segs > 0 && e.segSize > 0 {
		return e.segs, e.segSize
	}
	return s.segs, s.segSize
}

// create returns a new entry for window, evicting the least recently used live
// window when the set is full. s.mu is held and window has no entry.
func (s *Set) create(window string) *entry {
	if s.live >= s.max {
		s.evictLRU()
	}
	s.trimTombstones()

	e := &entry{ring: ring.New(s.segs, s.segSize)}
	s.windows[window] = e
	s.live++
	return e
}

// evictLRU turns the least recently used live window into a tombstone. s.mu is
// held.
func (s *Set) evictLRU() {
	var name string
	var used uint64
	first := true
	for n, e := range s.windows {
		if e.ring == nil {
			continue
		}
		if first || e.used < used {
			first = false
			name = n
			used = e.used
		}
	}
	if name == "" {
		return
	}
	e := s.windows[name]
	e.base += e.ring.Newest()
	e.ring = nil
	e.used = 0
	s.live--
}

// trimTombstones bounds the bookkeeping for evicted windows: at most max
// tombstones are kept, oldest first out. s.mu is held.
func (s *Set) trimTombstones() {
	tombstones := len(s.windows) - s.live
	if tombstones <= s.max {
		return
	}
	var name string
	var used uint64
	first := true
	for n, e := range s.windows {
		if e.ring != nil {
			continue
		}
		if first || e.used < used {
			first = false
			name = n
			used = e.used
		}
	}
	if name != "" {
		delete(s.windows, name)
	}
}

// Ring returns a handle to the window's current ring. The handle keeps the
// sequence translation, so callers see the Set's per-window sequences.
func (s *Set) Ring(window string) (*Ring, bool) {
	s.mu.Lock()
	e := s.windows[window]
	if e == nil || e.ring == nil {
		s.mu.Unlock()
		return nil, false
	}
	s.clock++
	e.used = s.clock
	r := &Ring{s: s, window: window, r: e.ring, base: e.base}
	s.mu.Unlock()
	return r, true
}

// Clear physically drops every line buffered for window. The window's sequence
// counter continues from the dropped newest, so a stale client cursor still
// maps to a well-formed gap rather than a rewind.
func (s *Set) Clear(window string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e := s.windows[window]
	if e == nil {
		return
	}
	if e.ring != nil {
		e.base += e.ring.Newest()
		e.ring = ring.New(s.segs, s.segSize)
	}
	s.clock++
	e.used = s.clock
}

// ResizeWindow is the interface method form of Resize.
func (s *Set) ResizeWindow(window string, lines int) bool { return s.Resize(window, lines) }

// Resize rebuilds one window's ring so it holds about lineCount lines. ZNC's
// SetBuffer sets a per-window line count (CChan::SetBufferCount); here the
// capacity is ring geometry, so the equivalent is a ring sized for that count.
// Buffered lines are carried over oldest-first, so a resize keeps what the user
// asked to keep and drops only what no longer fits.
//
// It reports false when the count cannot be expressed, which is what makes the
// command answer with ZNC's "Setting buffer size failed" instead of pretending.
func (s *Set) Resize(window string, lineCount int) bool {
	segs, segSize, ok := geometryFor(lineCount)
	if !ok {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	e := s.windows[window]
	if e == nil {
		// Nothing buffered yet: remember the geometry so the ring is built at
		// the requested size when the window first sees a line.
		e = &entry{}
		s.windows[window] = e
	}
	if e.ring != nil {
		fresh := ring.New(segs, segSize)
		e.ring.Replay(e.ring.Oldest(), func(_ uint64, line []byte) bool {
			fresh.Append(line)
			return true
		})
		// The kept lines were re-appended into a fresh ring, so their new
		// ordinal is 1..M while their old display sequence was the tail of the
		// old ring's. Re-point the base so the newest kept line still reports
		// the sequence the client last saw: display = base + ordinal, and the
		// last kept line must read old.Newest(), so base = old.Newest() - M.
		// Growth keeps every line, so M == old.Newest() and the base is 0 —
		// which is why a resize never disturbs a cursor that is still valid.
		if newest, kept := e.ring.Newest(), fresh.Newest(); newest >= kept {
			e.base = newest - kept
		} else {
			e.base = 0
		}
		e.ring = fresh
	}
	e.segs, e.segSize = segs, segSize
	s.clock++
	e.used = s.clock
	return true
}

// geometryFor converts a line count into ring geometry. A segment holds
// segSize/4 entries (ring.New's own rule), so segSize == lineCount*4 gives one
// segment's worth of lines. The clamps mirror ring.New.
func geometryFor(lineCount int) (segs, segSize int, ok bool) {
	if lineCount < 0 {
		return 0, 0, false
	}
	segSize = lineCount * 4
	if segSize < 512 {
		segSize = 512
	}
	return 2, segSize, true
}

// Windows returns the window names that currently have a live ring, sorted.
func (s *Set) Windows() []string {
	s.mu.Lock()
	names := make([]string, 0, s.live)
	for name, e := range s.windows {
		if e.ring != nil {
			names = append(names, name)
		}
	}
	s.mu.Unlock()
	sort.Strings(names)
	return names
}

// Bytes returns the sum of the buffered bytes over every live window.
func (s *Set) Bytes() int {
	s.mu.Lock()
	total := 0
	for _, e := range s.windows {
		if e.ring != nil {
			total += e.ring.Bytes()
		}
	}
	s.mu.Unlock()
	return total
}

// Ring is a window-scoped view of a ring. Sequences are the Set's per-window
// sequences; after Clear they continue from where the dropped ring stopped.
// A handle is a snapshot: after Clear it still points at the dropped ring, so
// callers that need the new ring ask Set.Ring again.
type Ring struct {
	s      *Set
	window string
	r      *ring.Ring
	base   uint64
}

// Clear physically drops the window's buffered lines. It is Set.Clear for the
// handle's window.
func (r *Ring) Clear() {
	if r.s != nil {
		r.s.Clear(r.window)
	}
}

func (r *Ring) Newest() uint64 {
	n := r.r.Newest()
	if n == 0 {
		return 0
	}
	return r.base + n
}

func (r *Ring) Oldest() uint64 {
	if r.r.Newest() == 0 {
		return 0
	}
	return r.base + r.r.Oldest()
}

func (r *Ring) Replay(from uint64, emit func(seq uint64, line []byte) bool) uint64 {
	newest := r.Newest()
	if newest == 0 {
		return 0
	}
	oldest := r.Oldest()
	var missing uint64
	if from < oldest {
		missing = oldest - from
		from = oldest
	}
	if from > newest {
		return missing
	}
	return missing + r.r.Replay(from-r.base, func(seq uint64, line []byte) bool {
		return emit(r.base+seq, line)
	})
}

func (r *Ring) Stamp(seq uint64) (int64, bool) {
	if seq <= r.base {
		return 0, false
	}
	return r.r.Stamp(seq - r.base)
}

func (r *Ring) Bytes() int { return r.r.Bytes() }
