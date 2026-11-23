// Package winring keeps one replay ring per window.
//
// It replaces the POC's single per-network ring. A Set owns a bounded number of
// lazily created windows; each window has its own ring.Ring and its own
// sequence counter. A line shared by two windows is appended to both rings with
// independent sequences, so a busy window cannot evict a quiet window's lines.
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
	maxSeq  uint64
	windows map[string]*entry
}

type entry struct {
	ring *ring.Ring
	// base is the last sequence emitted before the current ring was created.
	// It keeps sequences monotonic across Clear and eviction: a stale cursor is
	// a well-formed gap, not a silent rewind to an old sequence.
	base uint64
	used uint64
}

// New returns a Set holding at most maxWindows rings, each built with the same
// clamps as ring.New. maxWindows <= 0 means 1.
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
// sequence is monotonic for the lifetime of the Set, even across Clear and
// eviction.
func (s *Set) Append(window string, line []byte, ts int64) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	e := s.windows[window]
	if e == nil {
		e = s.create(window)
	}
	seq := e.base + e.ring.AppendStamp(line, ts)
	if seq > s.maxSeq {
		s.maxSeq = seq
	}
	s.clock++
	e.used = s.clock
	return seq
}

// create returns a new entry for window, evicting the least recently used
// window when the set is full. s.mu is held.
func (s *Set) create(window string) *entry {
	evicted := false
	if len(s.windows) >= s.max {
		var oldestName string
		var oldestUsed uint64
		first := true
		for name, e := range s.windows {
			if first || e.used < oldestUsed {
				first = false
				oldestName = name
				oldestUsed = e.used
			}
		}
		if oldestName != "" {
			delete(s.windows, oldestName)
			evicted = true
		}
	}
	e := &entry{ring: ring.New(s.segs, s.segSize)}
	if evicted {
		// Start after every sequence this Set has ever handed out, so a stale
		// cursor cannot replay a new line under an old number.
		e.base = s.maxSeq
	}
	s.windows[window] = e
	return e
}

// Ring returns a handle to the window's current ring. The handle keeps the
// sequence translation, so callers see the Set's per-window sequences.
func (s *Set) Ring(window string) (*Ring, bool) {
	s.mu.Lock()
	e := s.windows[window]
	if e == nil {
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
	e.base += e.ring.Newest()
	e.ring = ring.New(s.segs, s.segSize)
	s.clock++
	e.used = s.clock
}

// Windows returns the window names that currently have a ring, sorted.
func (s *Set) Windows() []string {
	s.mu.Lock()
	names := make([]string, 0, len(s.windows))
	for name := range s.windows {
		names = append(names, name)
	}
	s.mu.Unlock()
	sort.Strings(names)
	return names
}

// Bytes returns the sum of the buffered bytes over every window.
func (s *Set) Bytes() int {
	s.mu.Lock()
	total := 0
	for _, e := range s.windows {
		total += e.ring.Bytes()
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
