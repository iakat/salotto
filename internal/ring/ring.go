package ring

import "sync"

type segment struct {
	data    []byte
	offsets []int
	stamps  []int64
	first   uint64
	count   int
	used    int
}

type Ring struct {
	mu         sync.RWMutex
	segs       []segment
	n          int
	segSize    int
	maxEntries int
	w          int
	usedSegs   int
	newest     uint64
	oldest     uint64
	bytes      int
}

func New(segments, segSize int) *Ring {
	if segments < 2 {
		segments = 2
	}
	if segSize < 512 {
		segSize = 512
	}
	maxEntries := segSize / 4
	if maxEntries < 1 {
		maxEntries = 1
	}
	r := &Ring{
		segs:       make([]segment, segments),
		n:          segments,
		segSize:    segSize,
		maxEntries: maxEntries,
	}
	for i := range r.segs {
		r.segs[i].data = make([]byte, segSize)
		r.segs[i].offsets = make([]int, maxEntries)
		r.segs[i].stamps = make([]int64, maxEntries)
	}
	return r
}

func (r *Ring) Append(line []byte) uint64 {
	return r.AppendStamp(line, 0)
}

func (r *Ring) AppendStamp(line []byte, ts int64) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(line)
	truncated := false
	if n > 65535 {
		n = 65535
		truncated = true
	}
	rec := 2 + n
	if rec > r.segSize {
		n = r.segSize - 2
		rec = r.segSize
		truncated = true
	}

	s := &r.segs[r.w]
	if s.count >= r.maxEntries || s.used+rec > r.segSize {
		r.w = (r.w + 1) % r.n
		t := &r.segs[r.w]
		r.bytes -= t.used
		t.count = 0
		t.used = 0
		t.first = 0
		s = t
	}
	if s.count == 0 {
		s.first = r.newest + 1
		if r.usedSegs < r.n {
			r.usedSegs++
		}
	}

	off := s.used
	s.data[off] = byte(n)
	s.data[off+1] = byte(n >> 8)
	copy(s.data[off+2:], line[:n])

	idx := s.count
	if truncated {
		s.offsets[idx] = -off - 1
	} else {
		s.offsets[idx] = off
	}
	s.stamps[idx] = ts
	s.count++
	s.used += rec
	r.bytes += rec
	r.newest++
	r.oldest = r.calcOldest()
	return r.newest
}

func (r *Ring) calcOldest() uint64 {
	if r.newest == 0 {
		return 0
	}
	if r.usedSegs < r.n {
		return r.segs[0].first
	}
	return r.segs[(r.w+1)%r.n].first
}

func (r *Ring) Stamp(seq uint64) (int64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, idx, ok := r.locate(seq)
	if !ok {
		return 0, false
	}
	return s.stamps[idx], true
}

func (r *Ring) Truncated(seq uint64) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, idx, ok := r.locate(seq)
	if !ok {
		return false
	}
	return s.offsets[idx] < 0
}

func (r *Ring) locate(seq uint64) (*segment, int, bool) {
	if seq == 0 || seq > r.newest || seq < r.oldest {
		return nil, 0, false
	}
	for i := range r.segs {
		s := &r.segs[i]
		if s.count == 0 {
			continue
		}
		if seq >= s.first && seq < s.first+uint64(s.count) {
			return s, int(seq - s.first), true
		}
	}
	return nil, 0, false
}

func (r *Ring) Replay(from uint64, emit func(seq uint64, line []byte) bool) (missing uint64) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.newest == 0 {
		return 0
	}
	if from < r.oldest {
		missing = r.oldest - from
		from = r.oldest
	}
	if from > r.newest {
		return missing
	}

	si := -1
	startIdx := 0
	for i := range r.segs {
		s := &r.segs[i]
		if s.count == 0 {
			continue
		}
		if from >= s.first && from < s.first+uint64(s.count) {
			si = i
			startIdx = int(from - s.first)
			break
		}
	}
	if si < 0 {
		return missing
	}

	i := si
	for {
		s := &r.segs[i]
		if s.count > 0 {
			idx := 0
			if i == si {
				idx = startIdx
			}
			off := s.offsets[idx]
			if off < 0 {
				off = -off - 1
			}
			seq := s.first + uint64(idx)
			for ; idx < s.count; idx++ {
				ln := int(s.data[off]) | int(s.data[off+1])<<8
				if !emit(seq, s.data[off+2:off+2+ln]) {
					return missing
				}
				off += 2 + ln
				seq++
			}
		}
		if i == r.w {
			break
		}
		i = (i + 1) % r.n
	}
	return missing
}

func (r *Ring) Oldest() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.oldest
}

func (r *Ring) Newest() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.newest
}

func (r *Ring) Len() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.newest == 0 {
		return 0
	}
	return r.newest - r.oldest + 1
}

func (r *Ring) Bytes() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.bytes
}
