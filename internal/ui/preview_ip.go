package ui

import (
	"context"
	"net"
	"sync"
	"time"
)

// The address policy, kept in its own file because it is the security boundary and
// deserves to be readable on its own. It is a var so tests can substitute it; nothing
// outside a test may, which is why the only assignment outside this file is in
// preview_test.go's allowLoopbackForTest.

// lookupHost resolves a hostname to the addresses a connection would try.
var lookupHost = func(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	resolver := &net.Resolver{}
	// Prefer the context's resolver, so a hanging DNS server cannot hold a preview
	// open past its timeout.
	return resolver.LookupIP(ctx, "ip", host)
}

// publicIP is a var for the same reason lookupHost is: tests substitute it. Nothing
// outside a test may assign it.
// publicIP is a var for the same reason lookupHost is: tests substitute it. Nothing
// outside a test may assign it.
//
// The list is deliberately conservative and not exhaustive-looking: it refuses every
// address that is not globally routable, which is the only version of this check that
// does not need updating every time someone thinks of another reserved range.
var publicIP = func(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	// Ranges Go does not classify but that are not the public internet.
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 0: // "This network"
			return false
		case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127: // CGNAT 100.64/10
			return false
		case v4[0] == 192 && v4[1] == 0 && v4[2] == 0: // IETF protocol assignments
			return false
		case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19): // benchmarking
			return false
		case v4[0] >= 240: // reserved, includes broadcast
			return false
		}
		return true
	}
	// IPv6: only global unicast (2000::/3) is the public internet, minus the
	// things inside it that are not.
	if len(ip) == net.IPv6len {
		if ip[0]&0xe0 != 0x20 {
			return false
		}
		// 2001:db8::/32 is documentation.
		if ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x0d && ip[3] == 0xb8 {
			return false
		}
		return true
	}
	return false
}

// PreviewCache bounds how many links are remembered.
//
// A cache is required, not an optimisation: without one, a channel with 200 members
// makes 200 requests to the same third party for the same link, which is exactly the
// behaviour previews are criticised for. It is bounded because a long-lived bouncer
// sees unbounded distinct links.
const previewCacheMax = 256

// PreviewCache remembers previews, including the refusals: a link that could not be
// fetched must not be retried on every frame, or a dead host becomes a slow
// denial-of-service against the bouncer itself.
type PreviewCache struct {
	fetcher Fetcher

	// fetchMu serialises fetches. Previews are decoration and rare, so serialising
	// them costs nothing a user can notice, and it removes the whole class of bug
	// where two connections fetch the same new link at once - which is precisely
	// the amplification a preview feature is criticised for.
	fetchMu sync.Mutex

	mu    sync.Mutex
	order []string
	byURL map[string]Preview
}

// NewPreviewCache returns a cache over a fetcher.
func NewPreviewCache(f Fetcher) *PreviewCache {
	return &PreviewCache{fetcher: f, byURL: make(map[string]Preview, previewCacheMax)}
}

// Get returns the preview for a link, fetching it if it is not known.
//
// A fetch failure is remembered as kind "none" so it is not retried; the price is
// that a link that was briefly down stays unpreviewed for the process's lifetime,
// which is the right trade for decoration.
func (c *PreviewCache) Get(ctx context.Context, rawURL string) Preview {
	if p, ok := c.Cached(rawURL); ok {
		return p
	}

	c.fetchMu.Lock()
	defer c.fetchMu.Unlock()
	// Re-check under the fetch lock: whoever held it before us may have just
	// fetched this exact link.
	if p, ok := c.Cached(rawURL); ok {
		return p
	}

	fctx, cancel := context.WithTimeout(ctx, previewTimeout)
	defer cancel()
	p, err := c.fetcher.Fetch(fctx, rawURL)
	if err != nil {
		p = Preview{URL: rawURL, Kind: "none"}
	}

	c.mu.Lock()
	if _, seen := c.byURL[rawURL]; !seen {
		c.order = append(c.order, rawURL)
		// Evict the oldest. A proper LRU would need a hit order, and for decoration
		// the insertion order is close enough that the complexity is not earned.
		for len(c.order) > previewCacheMax {
			oldest := c.order[0]
			c.order = c.order[1:]
			delete(c.byURL, oldest)
		}
	}
	c.byURL[rawURL] = p
	c.mu.Unlock()
	return p
}

// Cached returns a preview without fetching or blocking. The frame builder uses
// this: a frame must never wait on a third-party server, because a slow link would
// then be indistinguishable from a hung bouncer.
func (c *PreviewCache) Cached(rawURL string) (Preview, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.byURL[rawURL]
	return p, ok
}

// Len is how many links are remembered.
func (c *PreviewCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.byURL)
}

// PrimeTimeout is how long the cache will wait on the first fetch. Exported for the
// frame builder, which must not hold a lock while fetching.
func (c *PreviewCache) PrimeTimeout() time.Duration { return previewTimeout }
