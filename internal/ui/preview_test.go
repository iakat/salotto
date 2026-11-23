package ui

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// lookupHost and publicIP live in preview_ip.go; the tests below pin the policy that
// decides what may be fetched, which is the part that matters.

// TestPreviewRefusesPrivateAddresses is the one that must never regress. A preview
// endpoint that will fetch http://10.0.0.1/ on request is an SSRF hole in a public
// web app, and salotto runs inside a cluster where that address space is full of
// things worth reading.
func TestPreviewRefusesPrivateAddresses(t *testing.T) {
	f := NewFetcher()
	for _, target := range []string{
		"http://127.0.0.1/",
		"http://localhost/",
		"http://10.0.0.1/",
		"http://192.168.1.1/",
		"http://172.16.0.1/",
		"http://169.254.169.254/latest/meta-data/", // the cloud metadata endpoint
		"http://[::1]/",
		"http://0.0.0.0/",
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		p, err := f.Fetch(ctx, target)
		cancel()
		if err == nil {
			t.Fatalf("%s was fetched (%+v) when it must be refused", target, p)
		}
		// The error must be the POLICY refusing, not a connection that happened to
		// fail. Checking only err != nil made this test pass against a policy that
		// allowed everything: 127.0.0.1:80 refuses the connection anyway, so the
		// assertion held for a reason that had nothing to do with the check.
		if !strings.Contains(err.Error(), "private") {
			t.Fatalf("%s failed with %v, want the address policy to refuse it", target, err)
		}
		if p.Kind != "none" {
			t.Fatalf("%s produced kind %q, want none", target, p.Kind)
		}
	}
}

// TestPreviewRefusesOtherSchemes pins that file://, gopher:// and javascript: are
// refused. The scheme check is what stops a link from being something other than a
// request, and "http(s) only" is the whole rule.
func TestPreviewRefusesOtherSchemes(t *testing.T) {
	f := NewFetcher()
	for _, target := range []string{
		"file:///etc/passwd",
		"gopher://example.com/",
		"javascript:alert(1)",
		"ftp://example.com/",
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := f.Fetch(ctx, target)
		cancel()
		if err == nil {
			t.Fatalf("%s was accepted", target)
		}
	}
}

// TestPreviewExtractsTitle pins the happy path against a real HTTP server, because
// the parsing and the request have to work together and a unit test on extractTitle
// alone would not notice a request that never sends the header it needs.
func TestPreviewExtractsTitle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept"), "text/html") {
			t.Errorf("Accept = %q, want text/html", r.Header.Get("Accept"))
		}
		if !strings.Contains(r.Header.Get("User-Agent"), "salotto") {
			t.Errorf("User-Agent = %q, want it to identify salotto", r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><head><title>The  Real
		Title &amp; More</title></head><body>hi</body></html>`))
	}))
	defer srv.Close()

	// The test server listens on 127.0.0.1, which the policy refuses. That is the
	// policy being right, so the test vets a public address by overriding the two
	// functions the check is built from.
	restore := allowLoopbackForTest()
	defer restore()

	p, err := NewFetcher().Fetch(context.Background(), srv.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "The Real Title & More" {
		t.Fatalf("title = %q", p.Title)
	}
	if p.Kind != "link" {
		t.Fatalf("kind = %q", p.Kind)
	}
	if p.Site != "127.0.0.1" {
		t.Fatalf("site = %q", p.Site)
	}
}

// TestPreviewFallsBackToOGTitle pins the common case: a page whose <title> is just
// the site name but whose og:title is the article.
func TestPreviewFallsBackToOGTitle(t *testing.T) {
	html := `<html><head><title>Site Name</title>` +
		`<meta property="og:title" content="The Actual Article"></head></html>`
	got, err := extractTitle(strings.NewReader(html))
	if err != nil {
		t.Fatal(err)
	}
	if got != "Site Name" {
		t.Fatalf("title = %q; a <title> is the page's own answer and wins over og:title", got)
	}

	// Without a <title>, og:title is all there is.
	html2 := `<html><head><meta property="og:title" content="Only OG"></head></html>`
	got2, err := extractTitle(strings.NewReader(html2))
	if err != nil {
		t.Fatal(err)
	}
	if got2 != "Only OG" {
		t.Fatalf("og-only title = %q", got2)
	}
}

// TestPreviewTitleIsBounded pins that a hostile title cannot be unbounded, and that
// truncation does not produce invalid UTF-8 - a title cut mid-rune turns into a
// replacement character in the browser.
func TestPreviewTitleIsBounded(t *testing.T) {
	long := strings.Repeat("é", previewMaxTitle*2)
	got := cleanTitle(long)
	if len(got) > previewMaxTitle+len("…") {
		t.Fatalf("title is %d bytes, cap is %d", len(got), previewMaxTitle)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated title does not say it was truncated: %q", got)
	}
	for _, r := range got {
		if r == '\uFFFD' {
			t.Fatalf("truncation cut a rune in half: %q", got)
		}
	}
}

// TestPreviewRefusesNonHTML pins that a PDF or an image is not read. There is nothing
// to preview, and reading it would be the wrong kind of request.
func TestPreviewRefusesNonHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write([]byte("%PDF-1.4 not html"))
	}))
	defer srv.Close()

	restore := allowLoopbackForTest()
	defer restore()

	p, err := NewFetcher().Fetch(context.Background(), srv.URL+"/x.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != "none" {
		t.Fatalf("kind = %q for a PDF, want none", p.Kind)
	}
	if p.Title != "" {
		t.Fatalf("title = %q for a PDF, want empty", p.Title)
	}
}

// TestPreviewRedirectIsReVetted is the second half of the SSRF defence, and the half
// that is usually missing. Vetting only the URL the user typed lets a public host
// redirect to 169.254.169.254, which is exactly how these are exploited.
func TestPreviewRedirectIsReVetted(t *testing.T) {
	// A public-looking server that redirects to the metadata endpoint.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()

	restore := allowLoopbackForTest()
	defer restore()

	_, err := NewFetcher().Fetch(context.Background(), srv.URL+"/")
	if err == nil {
		t.Fatal("a redirect to the metadata endpoint was followed")
	}
	if !strings.Contains(err.Error(), "private") {
		t.Fatalf("error = %v, want it to name the private address", err)
	}
}

// TestLinkInFindsOneLink pins the extraction rules, including the punctuation that
// belongs to the sentence rather than the URL. Getting this wrong produces previews
// for "https://example.com/x." which 404, and the user sees a broken preview for a
// link that works.
func TestLinkInFindsOneLink(t *testing.T) {
	cases := []struct{ in, want string }{
		{"look at https://example.com/x.", "https://example.com/x"},
		{"(see http://example.com/a?b=c)", "http://example.com/a?b=c"},
		{"no link here", ""},
		{"two https://a.example https://b.example", "https://a.example"},
		{"https://example.com/wow!", "https://example.com/wow"},
		{"<https://example.com/angle>", "https://example.com/angle"},
	}
	for _, c := range cases {
		if got := LinkIn(c.in); got != c.want {
			t.Fatalf("LinkIn(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestPreviewIsCachedPerLink pins that the channel pays for one fetch, not one per
// reader. A preview seen by 200 members must be one request to the third party.
func TestPreviewIsCachedPerLink(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<title>Once</title>"))
	}))
	defer srv.Close()

	restore := allowLoopbackForTest()
	defer restore()

	c := NewPreviewCache(NewFetcher())
	p1 := c.Get(context.Background(), srv.URL+"/")
	p2 := c.Get(context.Background(), srv.URL+"/")
	if hits != 1 {
		t.Fatalf("%d fetches for one link, want 1", hits)
	}
	if p1.Title != p2.Title {
		t.Fatalf("cache returned %q then %q", p1.Title, p2.Title)
	}
}

// TestPreviewCacheBoundsItself pins that the cache cannot grow forever. A long-lived
// bouncer sees unbounded distinct links, and a cache without a bound is a leak.
func TestPreviewCacheBoundsItself(t *testing.T) {
	c := NewPreviewCache(staticFetcher{})
	for i := 0; i < previewCacheMax+50; i++ {
		c.Get(context.Background(), "https://example.com/"+string(rune('a'+i%26))+string(rune('0'+i%10))+string(rune('a'+i/100)))
	}
	if n := c.Len(); n > previewCacheMax {
		t.Fatalf("cache holds %d entries, cap is %d", n, previewCacheMax)
	}
}

// staticFetcher returns a fixed preview without a network.
type staticFetcher struct{}

func (staticFetcher) Fetch(ctx context.Context, rawURL string) (Preview, error) {
	return Preview{URL: rawURL, Title: "t", Kind: "link", Site: "example.com"}, nil
}

// allowLoopbackForTest lets the httptest servers above be fetched, and only them.
// It exists because the production policy correctly refuses 127.0.0.1, so a test
// that wants to exercise the request path has to say so explicitly - and the fact
// that it has to is the policy working.
func allowLoopbackForTest() func() {
	oldLookup, oldPublic := lookupHost, publicIP
	lookupHost = func(ctx context.Context, host string) ([]net.IP, error) {
		if host == "127.0.0.1" || host == "localhost" || host == "::1" {
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		}
		return oldLookup(ctx, host)
	}
	publicIP = func(ip net.IP) bool {
		if ip.IsLoopback() {
			return true
		}
		return oldPublic(ip)
	}
	return func() { lookupHost, publicIP = oldLookup, oldPublic }
}
