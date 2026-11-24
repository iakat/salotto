// Package upload implements file uploads for the web front end.
//
// IRC has no file transfer that survives a bouncer, so this is ours: a file is streamed to the
// server, stored under a content-addressed name, and the URL is posted to a channel as an
// ordinary message. Three properties are load-bearing and each is here for a reason:
//
//   - **The quota is enforced during the write, not after.** Checking the size when the request
//     ends means the whole file is already on disk, so the limit stops nothing: the disk fills
//     and then the request is refused. The counter is incremented as bytes land and the write is
//     cut off mid-stream at the limit.
//
//   - **The stored name is a hash of the contents, never the client's filename.** A client name
//     is attacker-controlled - "..", a NUL, a 4 GiB filename, a name that collides with another
//     user's file. It is kept as metadata and never touched by the filesystem.
//
//   - **Expiry is the default.** A bouncer that silently becomes permanent public file hosting is
//     a liability, so a file has a lifetime unless an operator says otherwise.
package upload

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Errors a caller has to tell apart. They are distinct because the HTTP layer answers each with
// a different status, and a client that cannot tell "too big" from "disk full" cannot tell the
// user anything useful.
var (
	ErrTooLarge       = errors.New("upload: file exceeds the per-file limit")
	ErrQuotaExceeded  = errors.New("upload: user storage quota exceeded")
	ErrEmpty          = errors.New("upload: empty upload")
	ErrUploadNotFound = errors.New("upload: no such upload")
)

// Limits are the operator's policy.
type Limits struct {
	// MaxFile is the largest single upload accepted, in bytes. Zero means no per-file limit,
	// which is still bounded by MaxUser.
	MaxFile int64
	// MaxUser is the total bytes one user may store. Zero means unbounded.
	MaxUser int64
	// TTL is how long a file lives. Zero means it never expires.
	TTL time.Duration
}

// File is the metadata stored beside the bytes. It is a JSON sidecar rather than a database row
// because there is one file per upload and the two have to be written together: a row whose
// bytes are missing is a broken download, while a sidecar next to the bytes cannot disagree about
// whether the upload exists.
type File struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"` // the client's name, metadata only
	Size      int64     `json:"size"`
	MIME      string    `json:"mime"` // what we sniffed from the bytes
	Declared  string    `json:"declared,omitempty"`
	Uploader  string    `json:"uploader"`
	Created   time.Time `json:"created"`
	Expires   time.Time `json:"expires,omitempty"`
	Sum       string    `json:"sum"`
	SniffDiff bool      `json:"sniff_diff,omitempty"`
}

// Expired reports whether the file's lifetime has passed at now.
func (f File) Expired(now time.Time) bool {
	return !f.Expires.IsZero() && now.After(f.Expires)
}

// Store is the upload directory. Every method is safe for concurrent use: two uploads of the
// same bytes race to the same content-addressed path, and either order is correct.
type Store struct {
	dir    string
	limits Limits
	mu     sync.Mutex
	// used is the bytes stored per uploader, so the quota is a sum over what is on disk
	// rather than a value that can drift from it.
	used map[string]int64
	now  func() time.Time
}

// New opens a store, creating the directory, and rebuilds the per-user byte counts from what is
// already there. Rebuilding rather than trusting a counter file is the only way the count and
// the disk cannot disagree after a restart or a crash mid-write.
func New(dir string, limits Limits) (*Store, error) {
	if dir == "" {
		return nil, errors.New("upload: empty store directory")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("upload: %w", err)
	}
	s := &Store{dir: dir, limits: limits, used: map[string]int64{}, now: time.Now}
	if err := s.recount(); err != nil {
		return nil, err
	}
	return s, nil
}

// recount rebuilds the per-user totals from the sidecars on disk.
func (s *Store) recount() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		f, err := s.readMeta(filepath.Join(s.dir, e.Name()))
		if err != nil {
			// A sidecar we cannot read is not a reason to refuse to start: the bytes it
			// describes are unreachable without it, so the file is already lost.
			continue
		}
		s.used[f.Uploader] += f.Size
	}
	return nil
}

// metaPath is where an upload's sidecar lives. Every path in the store derives from the id,
// which already namespaces the content hash by uploader.
func (s *Store) metaPath(id string) string {
	return filepath.Join(s.dir, id+".json")
}

// dataPath is where an upload's bytes live, beside its sidecar.
func (s *Store) dataPath(id string) string {
	return filepath.Join(s.dir, id)
}

func (s *Store) readMeta(path string) (File, error) {
	var f File
	body, err := os.ReadFile(path)
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(body, &f); err != nil {
		return f, err
	}
	return f, nil
}

// Limits returns the policy this store enforces, for a front end that wants to state it.
func (s *Store) Limits() Limits { return s.limits }

// Save streams r into the store under id, enforcing the limits as the bytes arrive.
//
// The order of operations matters and is the point of the method:
//
//  1. The per-file limit and the remaining user quota are combined into one budget. A file may be
//     smaller than MaxFile and still refused because the user is nearly out of space, so the
//     limit that applies is whichever is smaller.
//  2. Bytes are written through an io.Writer that counts as they pass and stops at the budget, so
//     a 100 GiB upload costs at most budget+1 bytes of disk.
//  3. Only after the write succeeds does the user's total increase, and only by what was
//     actually written: a request that died halfway leaves bytes on disk, and those bytes are
//     real usage.
func (s *Store) Save(r io.Reader, meta File) (File, error) {
	if meta.Uploader == "" {
		return File{}, errors.New("upload: no uploader")
	}
	// The bytes are namespaced per uploader even though the name is a content hash. Content
	// addressing alone would put two users' identical files at one path, and then one of them
	// deleting "their" file would delete the other's - a sharing bug that only shows up when
	// two people post the same screenshot.
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return File{}, fmt.Errorf("upload: %w", err)
	}
	budget, binding, err := s.budgetFor(meta.Uploader)
	if err != nil {
		return File{}, err
	}

	// A partial file is written to a temporary name and renamed into place only once it is
	// complete and identified: a half-written file under its final name would be served.
	tmp, err := os.CreateTemp(s.dir, "incoming-*")
	if err != nil {
		return File{}, fmt.Errorf("upload: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}

	h := sha256.New()
	// The sniffer needs the first bytes and they are already passing through, so sniff on the
	// way past instead of re-reading the file afterwards.
	var n int64
	var sniffed []byte
	var over bool
	w := &cappedWriter{
		w: tmp,
		onWrite: func(p []byte) {
			h.Write(p)
			n += int64(len(p))
			if len(sniffed) < 512 {
				take := 512 - len(sniffed)
				if take > len(p) {
					take = len(p)
				}
				sniffed = append(sniffed, p[:take]...)
			}
		},
		limit: budget,
		onLimit: func() {
			over = true
		},
	}
	// io.Copy stops on the writer's error, so a limit is reported through it. The limit has to
	// be classified before the error is handled, otherwise every hit reads as "too large" and a
	// user with a full quota is told their file is too big - advice that leads them to try a
	// smaller file, which will also fail.
	copyErr := func() error { _, err := io.Copy(w, r); return err }()
	if over {
		cleanup()
		return File{}, binding
	}
	if copyErr != nil {
		cleanup()
		return File{}, fmt.Errorf("upload: %w", copyErr)
	}
	if n == 0 {
		cleanup()
		return File{}, ErrEmpty
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return File{}, fmt.Errorf("upload: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return File{}, fmt.Errorf("upload: %w", err)
	}

	contentHash := hex.EncodeToString(h.Sum(nil))
	// Every upload gets its own id, even when the bytes are identical to a file the same user
	// already posted.
	//
	// The first version used the content hash as the id, which is tidy and wrong: posting the
	// same screenshot twice produced one file, so the second upload's name silently replaced the
	// first's, and deleting either copy deleted both - breaking a link already pasted into a
	// channel. Found by the live probe, which deleted one upload and then looked for the other.
	// Users think in uploads, not in blobs, so the id counts uploads.
	sum := userTag(meta.Uploader) + "-" + contentHash + "-" + randomTag()
	mimeType := http.DetectContentType(sniffed)
	if i := strings.IndexByte(mimeType, ';'); i >= 0 {
		mimeType = strings.TrimSpace(mimeType[:i])
	}
	meta.ID = sum
	meta.Sum = sum
	meta.Size = n
	meta.MIME = mimeType
	meta.Created = s.now()
	if s.limits.TTL > 0 {
		meta.Expires = meta.Created.Add(s.limits.TTL)
	}
	// A declared type that disagrees with the bytes is not a reason to refuse the upload,
	// but it is a reason to force a download: an HTML file claiming to be a PNG is the
	// classic way to get script execution on the serving origin.
	if meta.Declared != "" {
		declared := meta.Declared
		if i := strings.IndexByte(declared, ';'); i >= 0 {
			declared = strings.TrimSpace(declared[:i])
		}
		meta.SniffDiff = declared != "" && !sameType(declared, mimeType)
	}

	dataPath := s.dataPath(sum)
	if err := os.Rename(tmpName, dataPath); err != nil {
		cleanup()
		return File{}, fmt.Errorf("upload: %w", err)
	}
	if err := s.writeMeta(meta); err != nil {
		return File{}, err
	}

	// Every upload occupies its own space, so every upload is charged. Deduplicating would mean
	// two uploads sharing one file and one delete breaking the other's link.
	s.mu.Lock()
	s.used[meta.Uploader] += n
	s.mu.Unlock()
	return meta, nil
}

// sameType compares two media types, treating "image/jpg" and "image/jpeg" as the same because
// they are.
func sameType(a, b string) bool {
	norm := func(t string) string {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "image/jpg" {
			return "image/jpeg"
		}
		return t
	}
	return norm(a) == norm(b)
}

// budgetFor returns how many bytes this uploader may still write, and which limit is binding.
//
// The second return value is the error to report when the budget is hit. It is decided here, at
// the point where the two limits are compared, rather than reconstructed from the budget
// afterwards: a user whose quota is nearly full must be told the quota is full, not that their
// file is too large, because "try a smaller file" is advice that will also fail.
func (s *Store) budgetFor(uploader string) (int64, error, error) {
	s.mu.Lock()
	used := s.used[uploader]
	s.mu.Unlock()

	budget := s.limits.MaxFile
	binding := error(ErrTooLarge)
	if s.limits.MaxUser > 0 {
		remaining := s.limits.MaxUser - used
		if remaining <= 0 {
			return 0, ErrQuotaExceeded, ErrQuotaExceeded
		}
		if budget <= 0 || remaining < budget {
			budget = remaining
			binding = ErrQuotaExceeded
		}
	}
	if budget <= 0 {
		// No limits at all: a budget still has to exist because cappedWriter needs one.
		// A very large number is "unlimited" without a special case in the writer.
		budget = 1 << 62
	}
	return budget, binding, nil
}

// cappedWriter counts bytes and refuses to pass more than limit to the underlying writer.
//
// It exists so the limit is applied to the stream rather than to the result. A writer that
// accepts everything and checks the total afterwards has already written the bytes, which is the
// failure the quota is supposed to prevent.
type cappedWriter struct {
	w       io.Writer
	limit   int64
	n       int64
	onWrite func([]byte)
	onLimit func()
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.n >= c.limit {
		c.onLimit()
		return 0, ErrTooLarge
	}
	if room := c.limit - c.n; int64(len(p)) > room {
		// Write the part that fits and stop: the pressure is applied at the boundary, so the
		// caller learns the limit was hit without the excess ever reaching the disk.
		if room > 0 {
			if _, err := c.w.Write(p[:room]); err != nil {
				return 0, err
			}
			c.onWrite(p[:room])
			c.n += room
		}
		c.onLimit()
		return int(room), ErrTooLarge
	}
	written, err := c.w.Write(p)
	if written > 0 {
		c.onWrite(p[:written])
		c.n += int64(written)
	}
	return written, err
}

// randomTag is the per-upload suffix that makes each upload distinct. It is random rather than a
// counter because a counter has to be persisted, and a store that rebuilds its state from the
// directory (as this one does after a restart) cannot rebuild a counter that is only in memory.
func randomTag() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A failure here would hand two uploads the same id; the timestamp is a worse but real
		// fallback, and it is better than a zero constant.
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
	}
	return hex.EncodeToString(b[:])
}

// userTag is the short per-uploader prefix of an upload id. Hashing rather than using the name
// keeps an uploader called ".." from becoming a path, and keeps the id from spelling out who
// uploaded what.
func userTag(uploader string) string {
	sum := sha256.Sum256([]byte(uploader))
	return hex.EncodeToString(sum[:4])
}

// ID for an upload is the content hash, which is stable and unguessable. Because the bytes live
// in a per-user directory, the same content uploaded by two users is two files with one id; the
// id alone therefore cannot locate a file, and every lookup goes through the metadata sidecar,
// which names the uploader.
func (s *Store) writeMeta(f File) error {
	body, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	path := s.metaPath(f.ID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o640); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("upload: %w", err)
	}
	return nil
}

// Get returns a file's metadata, refusing an expired one exactly as if it did not exist: an
// expired file is gone as far as a caller can tell, and a 404 is the honest answer for both.
func (s *Store) Get(id string) (File, error) {
	if !validID(id) {
		return File{}, ErrUploadNotFound
	}
	f, err := s.readMeta(s.metaPath(id))
	if err != nil {
		return File{}, ErrUploadNotFound
	}
	if f.Expired(s.now()) {
		return File{}, ErrUploadNotFound
	}
	return f, nil
}

// Open returns the bytes of a live upload.
func (s *Store) Open(id string) (File, *os.File, error) {
	f, err := s.Get(id)
	if err != nil {
		return File{}, nil, err
	}
	fh, err := os.Open(s.dataPath(f.ID))
	if err != nil {
		return File{}, nil, ErrUploadNotFound
	}
	return f, fh, nil
}

// Delete removes one file and its metadata, returning the bytes freed to the uploader's quota.
func (s *Store) Delete(uploader, id string) error {
	f, err := s.Get(id)
	if err != nil {
		return err
	}
	if f.Uploader != uploader {
		// Not found, not forbidden: a caller must not learn that another user's file exists.
		return ErrUploadNotFound
	}
	return s.remove(f)
}

// remove deletes the bytes and sidecar of a file and decrements the owner's usage.
//
// It deliberately does not go through Get. Delete must refuse an expired file (it is gone as far
// as a caller can tell), but the sweep's whole job is to delete exactly those, so routing it
// through the same check made it find nothing and delete nothing - a sweep that reports success
// forever while the disk fills. The test caught this; the two paths are separate for that reason.
func (s *Store) remove(f File) error {
	if err := os.Remove(s.dataPath(f.ID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("upload: %w", err)
	}
	if err := os.Remove(s.metaPath(f.ID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("upload: %w", err)
	}
	s.mu.Lock()
	s.used[f.Uploader] -= f.Size
	if s.used[f.Uploader] < 0 {
		s.used[f.Uploader] = 0
	}
	s.mu.Unlock()
	return nil
}

// List returns one uploader's files, newest first. It is per-user on purpose: the list is how a
// user finds what to delete, so it is also the thing that must not show anyone else's files.
func (s *Store) List(uploader string) ([]File, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("upload: %w", err)
	}
	var out []File
	now := s.now()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		f, err := s.readMeta(filepath.Join(s.dir, e.Name()))
		if err != nil || f.Expired(now) || f.Uploader != uploader {
			continue
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// Sweep deletes expired files and returns how many went. It is called on a timer by the front
// end: expiry is only a promise if something enforces it, and a file that is refused by Get but
// still on disk is still on disk.
func (s *Store) Sweep() (int, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, fmt.Errorf("upload: %w", err)
	}
	now := s.now()
	removed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		f, err := s.readMeta(filepath.Join(s.dir, e.Name()))
		if err != nil || !f.Expired(now) {
			continue
		}
		if err := s.remove(f); err == nil {
			removed++
		}
	}
	return removed, nil
}

// Used returns the bytes an uploader is storing.
func (s *Store) Used(uploader string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used[uploader]
}

// validID accepts only the shape a content address has. It is a whitelist and not a sanitiser:
// the id comes from the URL, so it is attacker-controlled, and the only safe ids are the ones
// this package minted.
func validID(id string) bool {
	if len(id) != 82 { // 8 hex tag, "-", 64 hex hash, "-", 8 hex upload tag
		return false
	}
	if id[8] != '-' || id[73] != '-' {
		return false
	}
	id = id[:8] + id[9:73] + id[74:]
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'f' {
			continue
		}
		return false
	}
	return true
}

// InlineType reports whether a type may be served inline. Anything not on this list becomes a
// download, because serving an unknown type inline is how a shared file becomes script running
// on the bouncer's own origin.
func InlineType(mimeType string) bool {
	switch strings.ToLower(mimeType) {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "text/plain", "application/pdf":
		return true
	}
	return false
}

// FileName returns a name safe to use in a Content-Disposition header.
//
// The client's name is untrusted: it can contain quotes, CRLF, or a path. Everything outside a
// conservative set is replaced rather than escaped, because a header is not a place to be clever
// about encoding.
func FileName(name string) string {
	if name == "" {
		return "upload"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_' || r == ' ':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" || out == "." || out == ".." {
		return "upload"
	}
	if len(out) > 128 {
		out = out[:128]
	}
	return out
}

// extensionFor returns the extension a MIME type is allowed, so a served filename matches the
// bytes rather than the client's claim.
func extensionFor(mimeType string) string {
	if exts, err := mime.ExtensionsByType(mimeType); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return ""
}
