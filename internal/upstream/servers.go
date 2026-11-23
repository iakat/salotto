package upstream

import (
	"sort"
	"strings"
	"sync"
)

// serverList is the set of servers a network tries, in order.
//
// ZNC keeps a CServer list per network and cycles through it: ADDSERVER appends,
// DELSERVER removes, and a failed connect moves to the next entry
// (IRCNetwork::AddServer / DelServer, ClientCommand.cpp:820-885). salotto used to
// have exactly one address from the config, which made ADDSERVER unimplementable
// and meant a dead server was a dead network.
//
// The first entry is the primary, so a config with one address behaves exactly as
// before: Next always returns it and the failover loop is a no-op.
type serverList struct {
	mu    sync.Mutex
	addrs []string
	next  int
}

// newServerList returns a list seeded with the configured address. An empty seed
// is allowed so a network added at runtime can start with nothing and have a
// server added later.
func newServerList(seed string) *serverList {
	l := &serverList{}
	if seed = strings.TrimSpace(seed); seed != "" {
		l.addrs = []string{seed}
	}
	return l
}

// Add appends a server. It reports false when the address is already there,
// which is what ZNC answers with "Perhaps the server is already added".
func (l *serverList) Add(addr string) bool {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, existing := range l.addrs {
		if strings.EqualFold(existing, addr) {
			return false
		}
	}
	l.addrs = append(l.addrs, addr)
	return true
}

// Del removes a server and reports whether it was there. Removing the entry the
// rotation was pointing at keeps the rotation consistent by clamping the index.
func (l *serverList) Del(addr string) bool {
	addr = strings.TrimSpace(addr)
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, existing := range l.addrs {
		if !strings.EqualFold(existing, addr) {
			continue
		}
		l.addrs = append(l.addrs[:i], l.addrs[i+1:]...)
		if l.next >= len(l.addrs) {
			l.next = 0
		}
		return true
	}
	return false
}

// Addrs is a copy of the list in order, for LISTSERVERS.
func (l *serverList) Addrs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.addrs...)
}

// Next returns the address to dial and advances the rotation, so a failed server
// is not retried forever while a working one sits second in the list.
//
// It returns "" when the list is empty, which the dial path treats as "stay
// disconnected" rather than dialling the empty string.
func (l *serverList) Next() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.addrs) == 0 {
		return ""
	}
	addr := l.addrs[l.next%len(l.addrs)]
	l.next = (l.next + 1) % len(l.addrs)
	return addr
}

// Current is the server the rotation would try next, i.e. the one a LISTSERVERS
// call marks with ZNC's trailing asterisk.
func (l *serverList) Current() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.addrs) == 0 {
		return ""
	}
	return l.addrs[l.next%len(l.addrs)]
}

// Sorted returns the addresses sorted, which is what a stable rendering needs.
func (l *serverList) Sorted() []string {
	out := l.Addrs()
	sort.Strings(out)
	return out
}
