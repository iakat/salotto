package ui

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// Link previews, done in the one way that is not a privacy incident.
//
// The Lounge fetches previews in the browser, which means every link a user sees is
// a request from THEIR address to someone else's server: the sender learns the
// reader's IP, and a channel with 200 members makes 200 requests to a third party.
// Salotto fetches server-side, once per link, and only for links a rendered line
// actually contains - so the sender learns that a preview happened, not who read it.
//
// The rules, in order of how much they cost when broken:
//
//  1. No request to a private, loopback, link-local or otherwise unroutable address.
//     A preview endpoint that will fetch http://10.0.0.1/ on request is an SSRF hole
//     in a public web app, and the daemon runs inside a cluster where that address
//     space is full of things worth reading.
//  2. No redirects followed into an address the first check refused. Following a
//     redirect re-checks the target, because the first check only vetted the URL the
//     user typed.
//  3. A short timeout and a small body cap. A preview is decoration; it must never
//     be able to hold a handler open or fill memory.
//  4. Nothing is fetched until a line containing the link has been shown. Previews
//     are keyed off rendered lines, not off the buffer, so scrollback does not
//     produce traffic to old links.
//  5. Failures are silent. A dead link must not take the message down with it.

const (
	previewTimeout  = 4 * time.Second
	previewMaxBytes = 96 << 10
	previewMaxTitle = 140
)

// Preview is what a link looks like when it is worth showing.
type Preview struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	Site  string `json:"site"`
	// Kind is what to draw: "link" for anything with a title, "none" when the
	// fetch was refused or failed. It is explicit so the client never has to guess
	// from empty fields.
	Kind string `json:"kind"`
}

// Fetcher fetches link previews. It is an interface so tests can hand in a
// transport that records what was requested without a network.
type Fetcher interface {
	Fetch(ctx context.Context, rawURL string) (Preview, error)
}

// NewFetcher returns the production fetcher.
func NewFetcher() Fetcher {
	return &httpFetcher{client: &http.Client{
		Timeout: previewTimeout,
		// Redirects are followed by hand so each hop is re-vetted.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

type httpFetcher struct{ client *http.Client }

// Fetch gets one link's title, refusing anything it should not be asking for.
func (f *httpFetcher) Fetch(ctx context.Context, rawURL string) (Preview, error) {
	refuse := Preview{URL: rawURL, Kind: "none"}

	u, err := url.Parse(rawURL)
	if err != nil {
		return refuse, err
	}
	if !previewScheme(u) {
		return refuse, errors.New("not an http(s) link")
	}
	if err := vetHost(ctx, u.Hostname()); err != nil {
		return refuse, err
	}

	// Follow a small number of redirects, vetting each target. Three hops covers
	// http→https and shorteners; beyond that a link is being cute.
	current := u
	for hop := 0; hop < 3; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current.String(), nil)
		if err != nil {
			return refuse, err
		}
		// A preview must not be a convincing browser: no cookies, no referer, and
		// an honest UA. If a site wants to refuse previews, it should be able to.
		req.Header.Set("User-Agent", "salotto/1 (+link preview)")
		req.Header.Set("Accept", "text/html;q=1,*/*;q=0.1")

		resp, err := f.client.Do(req)
		if err != nil {
			return refuse, err
		}
		loc := resp.Header.Get("Location")
		if resp.StatusCode >= 300 && resp.StatusCode < 400 && loc != "" {
			resp.Body.Close()
			next, err := current.Parse(loc)
			if err != nil {
				return refuse, err
			}
			if !previewScheme(next) {
				return refuse, errors.New("redirect to a non-http scheme")
			}
			if err := vetHost(ctx, next.Hostname()); err != nil {
				return refuse, err
			}
			current = next
			continue
		}
		defer resp.Body.Close()

		ct := resp.Header.Get("Content-Type")
		if !strings.Contains(strings.ToLower(ct), "text/html") {
			// A PDF, an image, a video: nothing to preview, and reading it would be
			// the wrong kind of request.
			return Preview{URL: rawURL, Kind: "none", Site: current.Hostname()}, nil
		}
		if resp.StatusCode != http.StatusOK {
			return refuse, errors.New("status " + resp.Status)
		}

		title, err := extractTitle(io.LimitReader(resp.Body, previewMaxBytes))
		if err != nil || title == "" {
			return Preview{URL: rawURL, Kind: "none", Site: current.Hostname()}, nil
		}
		return Preview{URL: rawURL, Title: title, Site: current.Hostname(), Kind: "link"}, nil
	}
	return refuse, errors.New("too many redirects")
}

// previewScheme is http or https and nothing else. file://, gopher:// and
// javascript: are the usual ways an SSRF check is bypassed.
func previewScheme(u *url.URL) bool {
	return u.Scheme == "http" || u.Scheme == "https"
}

// vetHost refuses addresses that are not on the public internet. The check is on the
// address the connection would go to, not the name typed: a public name that
// resolves to 127.0.0.1 is exactly the attack this is for.
func vetHost(ctx context.Context, host string) error {
	if host == "" {
		return errors.New("no host")
	}
	addrs, err := lookupHost(ctx, host)
	if err != nil {
		return err
	}
	for _, ip := range addrs {
		if !publicIP(ip) {
			return errors.New("refusing to fetch a private address")
		}
	}
	return nil
}

// extractTitle pulls the <title> out of an HTML head. A real parser would be better
// and is deliberately not used: a preview needs one string, and a full HTML parser
// on 96 KiB of hostile input is a bigger surface than the string search it replaces.
func extractTitle(r io.Reader) (string, error) {
	buf, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	lower := strings.ToLower(string(buf))
	start := strings.Index(lower, "<title")
	if start < 0 {
		// og:title is what most sites use for link cards, and it is often better
		// than <title> because <title> carries the site name too.
		if v := metaContent(string(buf), "og:title"); v != "" {
			return cleanTitle(v), nil
		}
		return "", nil
	}
	open := strings.Index(lower[start:], ">")
	if open < 0 {
		return "", nil
	}
	body := string(buf[start+open+1:])
	end := strings.Index(strings.ToLower(body), "</title")
	if end < 0 {
		return "", nil
	}
	title := cleanTitle(body[:end])
	if title == "" {
		return cleanTitle(metaContent(string(buf), "og:title")), nil
	}
	return title, nil
}

// metaContent finds <meta property="name" content="..."> without a parser.
func metaContent(html, name string) string {
	lower := strings.ToLower(html)
	for i := 0; ; {
		j := strings.Index(lower[i:], "<meta")
		if j < 0 {
			return ""
		}
		j += i
		k := strings.Index(lower[j:], ">")
		if k < 0 {
			return ""
		}
		tag := lower[j : j+k]
		if strings.Contains(tag, name) {
			// Take the content attribute from the original-case text.
			orig := html[j : j+k]
			if v := attrValue(orig, "content"); v != "" {
				return v
			}
		}
		i = j + k
	}
}

// attrValue reads one attribute's value out of a tag, handling both quote styles.
func attrValue(tag, name string) string {
	lower := strings.ToLower(tag)
	idx := strings.Index(lower, name+"=")
	if idx < 0 {
		return ""
	}
	rest := tag[idx+len(name)+1:]
	if rest == "" {
		return ""
	}
	switch rest[0] {
	case '"', '\'':
		q := rest[0]
		if end := strings.IndexByte(rest[1:], q); end >= 0 {
			return rest[1 : 1+end]
		}
		return ""
	default:
		end := strings.IndexAny(rest, " \t>")
		if end < 0 {
			return rest
		}
		return rest[:end]
	}
}

// cleanTitle collapses whitespace, decodes the few entities that matter in a title,
// and caps the length. A title arrives from a stranger's server and is drawn in
// someone else's browser: it is text, and it is bounded.
func cleanTitle(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	replacer := strings.NewReplacer(
		"&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`,
		"&#39;", "'", "&apos;", "'", "&nbsp;", " ", "&#8217;", "'",
	)
	s = replacer.Replace(s)
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	if len(s) > previewMaxTitle {
		// Cut on a rune boundary: a title cut mid-rune is invalid UTF-8, and JSON
		// marshalling turns that into a replacement character the user sees.
		s = strings.ToValidUTF8(s[:previewMaxTitle], "") + "…"
	}
	return s
}

// LinkIn finds the first http(s) link in a line of chat text. Only the first: one
// preview per line is plenty, and a line full of links should not produce a burst of
// fetches.
func LinkIn(text string) string {
	for _, field := range strings.Fields(text) {
		// Surrounding punctuation is part of the sentence, not the URL. IRC users
		// write "look at https://example.com/x." with the period, and angle brackets
		// are how a client is told "this is a link, do not preview mine".
		field = strings.TrimRight(field, ".,;:!?)]}'\"")
		field = strings.TrimLeft(field, "<([{'\"")
		field = strings.TrimRight(field, ">")
		if strings.HasPrefix(field, "http://") || strings.HasPrefix(field, "https://") {
			return field
		}
	}
	return ""
}
