package upload

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T, limits Limits) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := New(dir, limits)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

// TestTheQuotaIsEnforcedDuringTheWrite is the property the whole package exists for.
//
// A limit checked after the body has been read is not a limit: the bytes are already on disk, so
// an oversized upload fills the disk and *then* gets refused. This measures the disk during the
// write, from inside the reader, which is the only place that can tell "refused before the bytes
// landed" from "refused afterwards".
func TestTheQuotaIsEnforcedDuringTheWrite(t *testing.T) {
	const limit = 1024
	s, dir := newStore(t, Limits{MaxFile: limit})

	// A source far larger than the limit. The reader reports how much of it was pulled. It is
	// not a byte-exact witness of what was written - io.Copy hands the writer a whole buffer -
	// so the assertion is that the store stopped pulling early, and the disk below is exact.
	const source = 10 << 20
	var produced int64
	r := io.LimitReader(&countingReader{n: &produced}, source)
	_, err := s.Save(r, File{Uploader: "alice", Name: "big.bin"})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("a %d byte upload against a %d byte limit returned %v", source, limit, err)
	}
	if produced > 1<<20 {
		t.Fatalf("the store pulled %d of %d bytes before stopping: it read the whole upload", produced, source)
	}
	// And the disk agrees.
	if got := storeBytes(t, dir); got > limit {
		t.Fatalf("%d bytes are on disk after a refused upload", got)
	}
}

// TestARefusedUploadLeavesNothingBehind pins the cleanup: a refused upload must not leave a
// temporary file, because "refused" and "still occupying space" is the same as accepted.
func TestARefusedUploadLeavesNothingBehind(t *testing.T) {
	s, dir := newStore(t, Limits{MaxFile: 64})
	if _, err := s.Save(strings.NewReader(strings.Repeat("x", 100)), File{Uploader: "alice"}); err == nil {
		t.Fatal("a 100 byte upload against a 64 byte limit was accepted")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("a refused upload left %q behind", e.Name())
	}
}

// TestTheUserTotalCountsAcrossFiles pins the quota as a sum, not a per-file rule: each upload is
// under the file limit and the third is refused anyway.
func TestTheUserTotalCountsAcrossFiles(t *testing.T) {
	s, _ := newStore(t, Limits{MaxFile: 100, MaxUser: 250})
	// Distinct bodies, because every upload is its own file and costs its own space.
	for i := 0; i < 2; i++ {
		body := strings.Repeat(string(rune('a'+i)), 100)
		if _, err := s.Save(strings.NewReader(body), File{Uploader: "alice"}); err != nil {
			t.Fatalf("upload %d refused: %v", i+1, err)
		}
	}
	// Each file is within MaxFile, but only 50 of the 250 total is left, so the third is
	// refused for the total and not for its own size. That is the distinction this pins.
	third := strings.Repeat("z", 100)
	if _, err := s.Save(strings.NewReader(third), File{Uploader: "alice"}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("an upload past the total quota returned %v", err)
	}
	// A file that fits in what remains is still allowed: the quota is a budget, not a cliff.
	fits := strings.Repeat("y", 40)
	if _, err := s.Save(strings.NewReader(fits), File{Uploader: "alice"}); err != nil {
		t.Fatalf("a 40 byte upload with 50 bytes left was refused: %v", err)
	}
}

// TestEveryUploadIsItsOwnFile pins that posting the same bytes twice gives two files, not one.
//
// Content addressing made the id a hash of the contents, which is tidy and wrong: the second
// upload's name silently replaced the first's, and deleting either deleted both - breaking a link
// already pasted into a channel. The live probe found it by deleting one upload and looking for
// the other. Users think in uploads, not in blobs.
func TestEveryUploadIsItsOwnFile(t *testing.T) {
	s, dir := newStore(t, Limits{MaxUser: 10 << 20})
	body := strings.Repeat("hello", 100)
	first, err := s.Save(strings.NewReader(body), File{Uploader: "alice", Name: "first.txt"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Save(strings.NewReader(body), File{Uploader: "alice", Name: "second.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatalf("two uploads of the same bytes share the id %q", first.ID)
	}
	// Each keeps its own name, which is what a user sees.
	if f, err := s.Get(second.ID); err != nil || f.Name != "second.txt" {
		t.Fatalf("the second upload's name is %q (%v)", f.Name, err)
	}
	// Deleting one leaves the other intact - the link in the channel still works.
	if err := s.Delete("alice", first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(second.ID); err != nil {
		t.Fatalf("deleting the first upload removed the second: %v", err)
	}
	// Both are charged, because both occupy space.
	if got, want := s.Used("alice"), int64(len(body)); got != want {
		t.Fatalf("used = %d after deleting one of two copies, want %d", got, want)
	}
	if got := storeBytes(t, dir); got != int64(len(body)) {
		t.Fatalf("%d bytes on disk, want one file's worth", got)
	}
}

// TestTwoUsersWithTheSameFileAreSeparate pins the content-addressing bug that only shows when
// two people post the same screenshot: one shared path means one deleting it destroys the
// other's file.
func TestTwoUsersWithTheSameFileAreSeparate(t *testing.T) {
	s, _ := newStore(t, Limits{})
	body := strings.Repeat("identical", 50)
	a, err := s.Save(strings.NewReader(body), File{Uploader: "alice", Name: "shot.png"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Save(strings.NewReader(body), File{Uploader: "bob", Name: "shot.png"})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatal("two users' identical files share one id")
	}
	if err := s.Delete("alice", a.ID); err != nil {
		t.Fatal(err)
	}
	// Bob's must still be there, byte for byte.
	f, fh, err := s.Open(b.ID)
	if err != nil {
		t.Fatalf("bob's file disappeared when alice deleted hers: %v", err)
	}
	defer fh.Close()
	if f.Size != int64(len(body)) {
		t.Fatalf("bob's file is %d bytes, want %d", f.Size, len(body))
	}
}

// TestAClientFilenameNeverReachesTheFilesystem walks the names that break a naive store.
func TestAClientFilenameNeverReachesTheFilesystem(t *testing.T) {
	s, dir := newStore(t, Limits{})
	for _, name := range []string{
		"../../etc/passwd",
		"..",
		".",
		"",
		"a\x00b",
		strings.Repeat("z", 5000),
		"con",
	} {
		f, err := s.Save(strings.NewReader("body for "+name), File{Uploader: "alice", Name: name})
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		if strings.ContainsAny(f.ID, "/\\\x00") {
			t.Fatalf("%q produced an id with a path separator: %q", name, f.ID)
		}
		// The file is inside the store, whatever it is called.
		if _, err := os.Stat(s.dataPath(f.ID)); err != nil {
			t.Fatalf("%q: stored file is not where the store says: %v", name, err)
		}
		// And the sanitised name is safe for a header.
		if got := FileName(name); strings.ContainsAny(got, "\r\n\"") {
			t.Fatalf("FileName(%q) = %q carries a header-breaking character", name, got)
		}
	}
	// Nothing escaped the directory.
	parent := filepath.Dir(dir)
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "passwd") {
			t.Fatalf("an upload escaped the store dir: %q", e.Name())
		}
	}
}

// TestAnHTMLFileClaimingToBeAnImageIsAMismatch pins the sniffing rule: what the bytes are wins,
// and a disagreement forces a download.
func TestAnHTMLFileClaimingToBeAnImageIsAMismatch(t *testing.T) {
	s, _ := newStore(t, Limits{})
	body := "<html><script>alert(1)</script></html>"
	f, err := s.Save(strings.NewReader(body), File{Uploader: "alice", Declared: "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	if !f.SniffDiff {
		t.Fatal("HTML claiming to be a PNG was not flagged")
	}
	if InlineType(f.MIME) {
		t.Fatalf("HTML was considered safe to serve inline as %q", f.MIME)
	}
	// The honest case: a real PNG declared as one.
	png := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 100)
	f2, err := s.Save(strings.NewReader(png), File{Uploader: "alice", Declared: "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	if f2.SniffDiff {
		t.Fatalf("a real PNG was flagged as a mismatch (%q vs %q)", f2.MIME, "image/png")
	}
}

// TestExpiryIsTheDefault pins that a file has a lifetime unless the operator says otherwise, and
// that an expired file is indistinguishable from a missing one.
func TestExpiryIsTheDefault(t *testing.T) {
	s, _ := newStore(t, Limits{TTL: time.Hour})
	f, err := s.Save(strings.NewReader("body"), File{Uploader: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if f.Expires.IsZero() {
		t.Fatal("a file was stored with no expiry")
	}
	// Age it past the deadline rather than sleeping.
	s.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, err := s.Get(f.ID); !errors.Is(err, ErrUploadNotFound) {
		t.Fatalf("an expired file returned %v", err)
	}
	// A sweep removes the bytes too: refusing to serve a file that is still on disk is only
	// half of expiry.
	n, err := s.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("the sweep removed %d files, want 1", n)
	}
	if _, err := os.Stat(s.dataPath(f.ID)); !os.IsNotExist(err) {
		t.Fatal("the expired file's bytes are still on disk after a sweep")
	}
}

// TestZeroTTLMeansNever pins the operator's opt-out.
func TestZeroTTLMeansNever(t *testing.T) {
	s, _ := newStore(t, Limits{})
	f, err := s.Save(strings.NewReader("body"), File{Uploader: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if !f.Expires.IsZero() {
		t.Fatalf("a zero TTL produced an expiry: %v", f.Expires)
	}
	s.now = func() time.Time { return time.Now().Add(100 * 365 * 24 * time.Hour) }
	if _, err := s.Get(f.ID); err != nil {
		t.Fatalf("a file with no expiry expired: %v", err)
	}
}

// TestDeletingSomeoneElsesFileIsNotFound pins that the error does not leak existence.
func TestDeletingSomeoneElsesFileIsNotFound(t *testing.T) {
	s, _ := newStore(t, Limits{})
	f, err := s.Save(strings.NewReader("alice's file"), File{Uploader: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("bob", f.ID); !errors.Is(err, ErrUploadNotFound) {
		t.Fatalf("deleting another user's file returned %v", err)
	}
	if _, err := s.Get(f.ID); err != nil {
		t.Fatal("the file was deleted anyway")
	}
}

// TestListIsPerUser pins that the file list is how a user finds what to delete, so it must not
// show anyone else's files.
func TestListIsPerUser(t *testing.T) {
	s, _ := newStore(t, Limits{})
	for i, who := range []string{"alice", "bob", "alice"} {
		body := strings.Repeat(string(rune('a'+i)), 10)
		if _, err := s.Save(strings.NewReader(body), File{Uploader: who, Name: who + ".txt"}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.List("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("alice sees %d files, want 2", len(got))
	}
	for _, f := range got {
		if f.Uploader != "alice" {
			t.Fatalf("the list contains %q's file", f.Uploader)
		}
	}
}

// TestAnEmptyUploadIsRefused pins that a zero-byte body is an error rather than a stored file:
// an empty file is almost always a broken form, and serving it silently hides the bug.
func TestAnEmptyUploadIsRefused(t *testing.T) {
	s, dir := newStore(t, Limits{})
	if _, err := s.Save(bytes.NewReader(nil), File{Uploader: "alice"}); !errors.Is(err, ErrEmpty) {
		t.Fatalf("an empty upload returned %v", err)
	}
	if got := storeBytes(t, dir); got != 0 {
		t.Fatalf("an empty upload left %d bytes", got)
	}
}

// TestTheQuotaCountIsRebuiltFromDisk pins that a restart cannot desynchronise the counter from
// what is actually stored.
func TestTheQuotaCountIsRebuiltFromDisk(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(strings.NewReader(strings.Repeat("x", 300)), File{Uploader: "alice"}); err != nil {
		t.Fatal(err)
	}
	before := s.Used("alice")

	// A new store over the same directory, as a restart would be.
	again, err := New(dir, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Used("alice"); got != before {
		t.Fatalf("after a restart the count is %d, want %d", got, before)
	}
}

// countingReader reports how many bytes were actually pulled from it.
type countingReader struct{ n *int64 }

func (c *countingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	*c.n += int64(len(p))
	return len(p), nil
}

// storeBytes is the total size of the data files in dir, excluding sidecars and temporaries.
func storeBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		name := info.Name()
		if strings.HasSuffix(name, ".json") || strings.HasPrefix(name, "incoming-") {
			return nil
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return total
}
