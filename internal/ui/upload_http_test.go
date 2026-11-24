package ui

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/iakat/salotto/internal/upload"
)

func uploadServer(t *testing.T, limits upload.Limits) (*httptest.Server, *upload.Store) {
	t.Helper()
	store, err := upload.New(t.TempDir(), limits)
	if err != nil {
		t.Fatal(err)
	}
	who := "alice"
	srv := httptest.NewServer(http.StripPrefix("/uploads", UploadHandler(store, func(*http.Request) string { return who })))
	t.Cleanup(srv.Close)
	return srv, store
}

// postFile posts one multipart file and returns the response.
func postFile(t *testing.T, srv *httptest.Server, name, contentType string, body []byte) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{`form-data; name="file"; filename="` + name + `"`}
	if contentType != "" {
		h["Content-Type"] = []string{contentType}
	}
	part, err := mw.CreatePart(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatal(err)
	}
	mw.Close()
	resp, err := http.Post(srv.URL+"/uploads/", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestAnUploadComesBackByteForByte is the round trip. A store that saves and a handler that serves
// can each be right while the pair is wrong, so the bytes go in over HTTP and come back over HTTP.
func TestAnUploadComesBackByteForByte(t *testing.T) {
	srv, _ := uploadServer(t, upload.Limits{MaxFile: 1 << 20})
	body := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x01\x02\x03", 500))
	resp := postFile(t, srv, "shot.png", "image/png", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload returned %d: %s", resp.StatusCode, readAll(t, resp.Body))
	}
	var out struct {
		ID     string `json:"id"`
		URL    string `json:"url"`
		MIME   string `json:"mime"`
		Size   int64  `json:"size"`
		Inline bool   `json:"inline"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Size != int64(len(body)) {
		t.Fatalf("reported size %d, want %d", out.Size, len(body))
	}
	if out.MIME != "image/png" {
		t.Fatalf("sniffed %q", out.MIME)
	}
	if !out.Inline {
		t.Fatal("a real PNG was not marked inline")
	}

	got, err := http.Get(srv.URL + "/uploads/" + out.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()
	if got.StatusCode != http.StatusOK {
		t.Fatalf("download returned %d", got.StatusCode)
	}
	back := readAll(t, got.Body)
	if !bytes.Equal(back, body) {
		t.Fatalf("the bytes came back different: %d in, %d out", len(body), len(back))
	}
	// The served type is the sniffed one, not the multipart claim.
	if ct := got.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("served Content-Type %q", ct)
	}
	if cd := got.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "inline") {
		t.Fatalf("a PNG was served as %q", cd)
	}
	if got.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("nosniff is missing: the inline decision can be second-guessed by the browser")
	}
}

// TestAnOversizedUploadIsRefusedOverHTTP pins the status a client keys its message on, and that the
// refusal is the store's rather than a truncated body the client thinks succeeded.
func TestAnOversizedUploadIsRefusedOverHTTP(t *testing.T) {
	srv, store := uploadServer(t, upload.Limits{MaxFile: 1024})
	body := bytes.Repeat([]byte("x"), 8192)
	resp := postFile(t, srv, "big.bin", "application/octet-stream", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized upload returned %d, want 413", resp.StatusCode)
	}
	if used := store.Used("alice"); used != 0 {
		t.Fatalf("a refused upload left %d bytes charged", used)
	}
}

// TestADeclaredOversizeIsRefusedBeforeTheBodyArrives pins the clean refusal.
//
// Cutting a stream off mid-body protects the disk but punishes the client: the answer arrives while
// the client is still writing, the connection closes under it, and what the user sees is a network
// error rather than "file too large". When the client declares its length - which every browser does
// for a form upload - the refusal can be sent before the body is read, so this asserts that the
// handler answers 413 *without consuming the body*.
//
// The handler is called directly rather than through a real socket, and that is deliberate. The
// first version of this test wrapped the body in a counting reader and let http.Client drive it; the
// counter then measured the *client transport* pumping bytes into a socket the server had already
// answered on, so it reported between 1 and 2.8 MB read before a refusal that had in fact happened
// immediately, and failed intermittently against correct code. A witness that is not the thing you
// claim to be measuring is not a witness. Here the counting reader is wrapped around a body the
// handler is handed, so what it records is what the handler asked for and nothing else.
func TestADeclaredOversizeIsRefusedBeforeTheBodyArrives(t *testing.T) {
	store, err := upload.New(t.TempDir(), upload.Limits{MaxFile: 1 << 10})
	if err != nil {
		t.Fatal(err)
	}
	h := UploadHandler(store, func(*http.Request) string { return "alice" })

	const total = 8 << 20
	counter := &countingBody{r: zeroReader{}}
	req := httptest.NewRequest("POST", "/uploads/", counter)
	req.ContentLength = total
	// A form content type, so the handler gets past the method check and reaches the length guard.
	// The boundary is irrelevant because the body must never be parsed.
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a declared %d byte upload against a 1 KiB limit returned %d, want 413", total, rec.Code)
	}
	if got := counter.n.Load(); got != 0 {
		t.Fatalf("%d bytes of the body were read before the refusal; the point is to refuse first", got)
	}
}

// zeroReader is an infinite supply of zeros, so the body is never materialised in the test.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// TestAnOversizedChunkedBodyIsNeverStored pins the case the declared-length guard cannot cover: a
// client that sends no Content-Length.
//
// It deliberately does not assert how many bytes were read before the refusal. I tried that, and it
// is not observable from here: the client keeps writing into kernel socket buffers while the server
// has already stopped reading, so the count depends on buffer sizes rather than on the server. The
// test passed and then failed on identical code. What *is* durable is the outcome - an oversized
// chunked upload is refused and nothing is stored - so that is what it asserts.
//
// The streamed limit in the store is still the guard here, and the declared-length check in front of
// it is what a browser gets (TestADeclaredOversizeIsRefusedBeforeTheBodyArrives). MaxBytesReader
// sits between them as defence in depth and is not separately provable at this layer for the reason
// above.
func TestAnOversizedChunkedBodyIsNeverStored(t *testing.T) {
	srv, store := uploadServer(t, upload.Limits{MaxFile: 1 << 10})

	const total = 8 << 20
	counter := &countingBody{r: io.LimitReader(zeroReader{}, total)}
	req, err := http.NewRequest("POST", srv.URL+"/uploads/", counter)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	// No ContentLength: this is the chunked client, and it is the point of the test.
	req.ContentLength = -1
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// A connection closed under the client is also a refusal, and it is the likelier one:
		// the server answers and closes while the client is still writing.
		if store.Used("alice") == 0 {
			return
		}
		t.Fatalf("the upload errored but %d bytes were stored", store.Used("alice"))
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusCreated {
		t.Fatal("an 8 MiB chunked upload against a 1 KiB limit was accepted")
	}
	if used := store.Used("alice"); used != 0 {
		t.Fatalf("a refused upload left %d bytes charged", used)
	}
}

// countingBody reports how much of a request body the transport actually consumed.
//
// The counter is atomic because it is written by the transport's goroutine and read by the test:
// a plain int64 here is a data race, and one that -race reports rather than one that quietly
// returns a stale number. A counter a test polls must be atomic.
type countingBody struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// TestAnHTMLEmailClaimingToBeAnImageIsForcedToDownload pins the inline decision end to end: the
// bytes win, and the response says attachment.
func TestAnHTMLClaimingToBeAnImageIsForcedToDownload(t *testing.T) {
	srv, _ := uploadServer(t, upload.Limits{})
	body := []byte("<html><script>alert(document.domain)</script></html>")
	resp := postFile(t, srv, "innocent.png", "image/png", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload returned %d", resp.StatusCode)
	}
	var out struct {
		ID     string `json:"id"`
		MIME   string `json:"mime"`
		Inline bool   `json:"inline"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.MIME == "image/png" {
		t.Fatal("the multipart Content-Type was trusted")
	}
	if out.Inline {
		t.Fatal("HTML was marked safe to render inline")
	}
	got, err := http.Get(srv.URL + "/uploads/" + out.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()
	if cd := got.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("HTML claiming to be an image was served as %q", cd)
	}
	// The served Content-Type must be the sniffed one. Asserting only the disposition would let
	// a handler that trusted the claim pass, because the disposition here is decided by the
	// mismatch flag - so this reads the header the browser actually acts on.
	if ct := got.Header.Get("Content-Type"); ct == "image/png" {
		t.Fatal("the served Content-Type was the client's claim, not the bytes")
	}
}

// TestASafeFileIsServedWithItsOwnType pins that the served type comes from the bytes. The claim is
// text/plain and the bytes are a PNG, so a handler that believed the claim would answer text/plain
// and this would fail. The download-vs-render decision is separate and is asserted to be
// "attachment" here precisely because the claim lied.
func TestASafeFileIsServedWithItsOwnType(t *testing.T) {
	srv, _ := uploadServer(t, upload.Limits{})
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x0}, 256)...)
	// Claimed as plain text: a client that trusted the claim would serve text/plain, and a
	// browser would then show the bytes as text rather than as the image it is.
	resp := postFile(t, srv, "photo.png", "text/plain", png)
	defer resp.Body.Close()
	var out struct {
		ID     string `json:"id"`
		MIME   string `json:"mime"`
		Inline bool   `json:"inline"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.MIME != "image/png" {
		t.Fatalf("a PNG claiming to be text was stored as %q", out.MIME)
	}
	got, err := http.Get(srv.URL + "/uploads/" + out.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()
	// The served type is the sniffed one. This is the assertion that separates "read the bytes"
	// from "believe the client": with the claim trusted, this header would read text/plain.
	if ct := got.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("served Content-Type %q, want the sniffed image/png", ct)
	}
	// And because the claim disagreed, it downloads rather than renders. That is deliberate:
	// a file whose own header contradicts its bytes is not a file to put in a page, even when
	// the bytes happen to be harmless. The mismatch is the signal, not the type.
	if cd := got.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("a PNG whose claim contradicted its bytes was served as %q", cd)
	}
}

// TestAnUnknownTypeDownloadsInsteadOfRendering pins the other direction with a type that is not on
// the safe list at all, so "serve everything inline" changes the answer.
func TestAnUnknownTypeDownloadsInsteadOfRendering(t *testing.T) {
	srv, _ := uploadServer(t, upload.Limits{})
	// A zip is not on the safe list and sniffs as application/zip; nothing about the claim
	// matters here, which is what makes this catch an unconditional inline.
	body := append([]byte("PK\x03\x04"), bytes.Repeat([]byte{0x00}, 512)...)
	resp := postFile(t, srv, "archive.zip", "application/zip", body)
	defer resp.Body.Close()
	var out struct {
		ID     string `json:"id"`
		MIME   string `json:"mime"`
		Inline bool   `json:"inline"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	got, err := http.Get(srv.URL + "/uploads/" + out.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()
	if cd := got.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("%q (%s) was served as %q; an unknown type must download", out.MIME, out.MIME, cd)
	}
}

// TestAGuessableIdIsRefused pins that the URL cannot address anything the store did not mint. A
// path traversal through the id is the obvious attack on a content-addressed store.
func TestAGuessableIdIsRefused(t *testing.T) {
	srv, store := uploadServer(t, upload.Limits{})
	// A real file exists, so a traversal has something to reach.
	if _, err := store.Save(strings.NewReader("secret"), upload.File{Uploader: "alice", Name: "s.txt"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/uploads/../../etc/passwd",
		"/uploads/" + strings.Repeat("a", 64),
		"/uploads/abc",
		"/uploads/",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			continue // a client-side refusal of the path is also a refusal
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("%q was served", path)
		}
	}
}

// TestTheListIsTheCallersOwn pins the list endpoint against leaking another user's files.
func TestTheListIsTheCallersOwn(t *testing.T) {
	dir := t.TempDir()
	store, err := upload.New(dir, upload.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(strings.NewReader("alice's"), upload.File{Uploader: "alice", Name: "a.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(strings.NewReader("bob's"), upload.File{Uploader: "bob", Name: "b.txt"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(UploadHandler(store, func(r *http.Request) string {
		return r.Header.Get("X-User")
	}))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/?list=1", nil)
	req.Header.Set("X-User", "alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Files []struct {
			Name string `json:"name"`
		} `json:"files"`
		Used int64 `json:"used"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Files) != 1 || out.Files[0].Name != "a.txt" {
		t.Fatalf("alice's list is %+v", out.Files)
	}
	if out.Used != int64(len("alice's")) {
		t.Fatalf("used = %d", out.Used)
	}
}

// TestWithNoIdentityNothingIsUploaded pins the refusal: a missing identity must not silently pool
// anonymous uploads into one quota.
func TestWithNoIdentityNothingIsUploaded(t *testing.T) {
	store, err := upload.New(t.TempDir(), upload.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(UploadHandler(store, func(*http.Request) string { return "" }))
	defer srv.Close()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("file", "x.txt")
	part.Write([]byte("body"))
	mw.Close()
	resp, err := http.Post(srv.URL+"/", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an anonymous upload returned %d, want 401", resp.StatusCode)
	}
}

// TestADeleteFreesTheQuota pins that deleting is how a user gets their space back - the only
// remedy a quota leaves them.
func TestADeleteFreesTheQuota(t *testing.T) {
	srv, store := uploadServer(t, upload.Limits{MaxFile: 100, MaxUser: 100})
	body := bytes.Repeat([]byte("q"), 90)
	resp := postFile(t, srv, "a.bin", "", body)
	var out struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if used := store.Used("alice"); used != 90 {
		t.Fatalf("used = %d after a 90 byte upload", used)
	}
	req, _ := http.NewRequest("DELETE", srv.URL+"/uploads/"+out.ID, nil)
	del, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("delete returned %d", del.StatusCode)
	}
	if used := store.Used("alice"); used != 0 {
		t.Fatalf("a delete left the quota at %d", used)
	}
	// And the space is usable again, which is the point of freeing it.
	again := postFile(t, srv, "b.bin", "", body)
	again.Body.Close()
	if again.StatusCode != http.StatusCreated {
		t.Fatalf("re-upload after a delete returned %d", again.StatusCode)
	}
}

// TestTheQuotaRefusalIsNotReportedAsAnInternalError pins the status mapping: 507 is "you are out
// of space", 500 is "the server is broken". A client shows these differently and one of them sends
// the user to complain.
func TestTheQuotaRefusalIsNotReportedAsAnInternalError(t *testing.T) {
	srv, _ := uploadServer(t, upload.Limits{MaxUser: 50})
	first := postFile(t, srv, "a.bin", "", bytes.Repeat([]byte("a"), 40))
	first.Body.Close()
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("the first upload returned %d", first.StatusCode)
	}
	second := postFile(t, srv, "b.bin", "", bytes.Repeat([]byte("b"), 40))
	defer second.Body.Close()
	if second.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("an upload past the quota returned %d, want 507", second.StatusCode)
	}
}

func readAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
