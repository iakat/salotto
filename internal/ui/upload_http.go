package ui

// The upload HTTP surface.
//
// It is deliberately small: POST a file, GET a file, a list, and a delete. Everything that makes
// a shared file safe - the streamed quota, the content-addressed id, the sniffed type, the
// download-not-render default - lives in internal/upload and is not re-implemented here. This file
// is the mapping from HTTP to that store, and its only jobs are authentication, status codes and
// headers.
//
// There is **no authentication here yet**. The deployment has none (OIDC is the open blocker), so
// an uploader's identity arrives as a header the front end sets. That is not a security boundary
// and is not claimed to be one: it is the same identity the websocket already trusts, in one
// place, so that when OIDC lands the check goes here and nowhere else.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/iakat/salotto/internal/upload"
)

// UploadHandler serves the upload endpoints under prefix.
//
// Routes, all under prefix:
//
//	POST   prefix            multipart form, field "file"; answers JSON metadata
//	GET    prefix/<id>       the bytes, inline only for a safe type
//	GET    prefix?list=1     the caller's files, newest first
//	DELETE prefix/<id>       removes one of the caller's files
//
// The caller's identity comes from identify, which the deployment supplies. A nil identify means
// single-user mode: everything belongs to one name.
func UploadHandler(store *upload.Store, identify func(*http.Request) string) http.Handler {
	if identify == nil {
		identify = func(*http.Request) string { return "local" }
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who := identify(r)
		if who == "" {
			// No identity is a refusal, never a default user: a fallback name here would
			// silently pool every anonymous request into one quota.
			http.Error(w, "no identity", http.StatusUnauthorized)
			return
		}

		switch r.Method {
		case http.MethodPost:
			handleUpload(w, r, store, who)
		case http.MethodGet:
			if r.URL.Query().Get("list") != "" {
				handleList(w, store, who)
				return
			}
			handleDownload(w, r, store, who)
		case http.MethodDelete:
			handleDelete(w, r, store, who)
		default:
			w.Header().Set("Allow", "GET, POST, DELETE")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

// handleUpload reads a multipart body and streams the one file through the store.
func handleUpload(w http.ResponseWriter, r *http.Request, store *upload.Store, who string) {
	limits := store.Limits()
	// Refuse an oversized body before reading any of it, when the client said how big it is.
	//
	// This is not only an optimisation. Stopping a stream mid-body is the right thing for the
	// disk, but it is the wrong thing for the client: the server answers and closes while the
	// client is still writing, and the client sees a connection reset rather than the 413 that
	// explains what happened. Checking the declared length means the refusal arrives before the
	// body does, so the browser shows "file too large" instead of a network error.
	//
	// A chunked upload has no declared length, and that is what the streaming limit below is
	// for: the two guards cover different clients rather than the same one twice.
	if limits.MaxFile > 0 && r.ContentLength > 0 && r.ContentLength > limits.MaxFile+1<<20 {
		// The slack over the file limit is the multipart framing and the field headers
		// around it, which are not part of the file.
		writeErr(w, upload.ErrTooLarge)
		return
	}
	// A body limit on the *request* as well as on the store: the store stops a large file
	// mid-stream, but the multipart reader would still copy it into a temporary file first,
	// which is the same disk problem one layer up.
	if limits.MaxFile > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, limits.MaxFile+1<<20)
	}
	// The whole body is a form, so let the reader spill to disk rather than hold it in memory.
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, upload.ErrTooLarge)
			return
		}
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "no file field", http.StatusBadRequest)
		return
	}
	defer file.Close()

	meta := upload.File{
		Name:     header.Filename,
		Uploader: who,
	}
	// The declared type is recorded but never trusted; the store sniff the bytes and flags a
	// disagreement so the download is forced.
	if ct := header.Header.Get("Content-Type"); ct != "" {
		meta.Declared = ct
	}
	saved, err := store.Save(file, meta)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, uploadView(r, saved))
}

// uploadView is the JSON a client sees. It is separate from upload.File so the stored shape can
// change without changing the wire, and so the URL is computed where the request is known.
func uploadView(r *http.Request, f upload.File) map[string]any {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	host := r.Host
	out := map[string]any{
		"id":   f.ID,
		"name": upload.FileName(f.Name),
		"size": f.Size,
		"mime": f.MIME,
		"url":  fmt.Sprintf("%s://%s/uploads/%s", scheme, host, f.ID),
		// The URL is what gets pasted into a channel, so its presence is the contract.
	}
	if !f.Expires.IsZero() {
		out["expires"] = f.Expires.UTC().Format(time.RFC3339)
	}
	// A mismatch is worth telling the uploader about: their file will download instead of
	// showing a preview, and they should know why.
	out["inline"] = upload.InlineType(f.MIME) && !f.SniffDiff
	return out
}

// handleDownload serves the bytes. The type served is the sniffed one, never the client's claim.
func handleDownload(w http.ResponseWriter, r *http.Request, store *upload.Store, who string) {
	id := strings.TrimPrefix(r.URL.Path, "/")
	id = strings.Trim(id, "/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	f, fh, err := store.Open(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer fh.Close()

	// Content addressing makes the body immutable, so it can be cached forever. This is what
	// makes an uploaded file cheap to re-view without re-downloading it.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("ETag", `"`+f.ID+`"`)
	// A download of someone else's file is allowed - that is what a shared link is - but the
	// name and type still come from the store, not from the URL.
	w.Header().Set("Content-Type", f.MIME)
	disposition := "inline"
	if !upload.InlineType(f.MIME) || f.SniffDiff {
		// Anything not known-safe is an attachment. Serving an unknown type inline is how a
		// shared file becomes script on the bouncer's origin.
		disposition = "attachment"
	}
	name := upload.FileName(f.Name)
	// filename* is the RFC 5987 form: without it a non-ASCII name arrives mangled, and the
	// plain filename= is kept for clients that read only that one.
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`, disposition, name, urlEscape(name)))
	// nosniff stops a browser from second-guessing the type above, which is the other half of
	// the inline decision.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.FormatInt(f.Size, 10))
	// http.ServeContent gets range requests, HEAD and conditional requests for free, which is
	// what makes an image or a video seekable in the browser.
	http.ServeContent(w, r, name, f.Created, fh)
}

func handleList(w http.ResponseWriter, store *upload.Store, who string) {
	files, err := store.List(who)
	if err != nil {
		writeErr(w, err)
		return
	}
	views := make([]map[string]any, 0, len(files))
	for _, f := range files {
		views = append(views, map[string]any{
			"id":      f.ID,
			"name":    upload.FileName(f.Name),
			"size":    f.Size,
			"mime":    f.MIME,
			"created": f.Created.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"files": views,
		"used":  store.Used(who),
	})
}

func handleDelete(w http.ResponseWriter, r *http.Request, store *upload.Store, who string) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	if err := store.Delete(who, id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeErr maps the store's errors to statuses. The mapping matters: a client that cannot tell
// "too big" from "quota full" from "internal" cannot tell its user anything useful, and 500 for a
// refusal reads as a bug in the server.
func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, upload.ErrTooLarge):
		http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
	case errors.Is(err, upload.ErrQuotaExceeded):
		http.Error(w, "storage quota exceeded", http.StatusInsufficientStorage)
	case errors.Is(err, upload.ErrEmpty):
		http.Error(w, "empty upload", http.StatusBadRequest)
	case errors.Is(err, upload.ErrUploadNotFound):
		http.NotFound(w, nil)
	default:
		http.Error(w, "upload failed", http.StatusInternalServerError)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// urlEscape percent-encodes a name for the RFC 5987 filename* parameter.
func urlEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// sweepUploads runs the expiry sweep until the process ends. Expiry is only a promise if
// something enforces it: a file that Get refuses but that is still on disk is still on disk.
func SweepUploads(store *upload.Store, every time.Duration) {
	if every <= 0 {
		every = time.Hour
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for range t.C {
			if n, err := store.Sweep(); err == nil && n > 0 {
				slog.Info("upload sweep removed expired files", "count", n)
			}
		}
	}()
}

// parseLimits reads the operator's upload policy from flags, keeping the defaults in one place so
// the deployment and the documentation cannot disagree.
func ParseUploadLimits(maxFile, maxUser, ttl string) (upload.Limits, error) {
	var l upload.Limits
	var err error
	if l.MaxFile, err = parseCount(maxFile); err != nil {
		return l, fmt.Errorf("upload max file: %w", err)
	}
	if l.MaxUser, err = parseCount(maxUser); err != nil {
		return l, fmt.Errorf("upload max user: %w", err)
	}
	if ttl != "" {
		d, err := time.ParseDuration(ttl)
		if err != nil {
			return l, fmt.Errorf("upload ttl: %w", err)
		}
		l.TTL = d
	}
	return l, nil
}

// parseCount accepts a plain byte count or a KiB/MiB/GiB suffix, because "10MiB" in a manifest is
// readable and "10485760" is a number nobody can check at a glance.
func parseCount(s string) (int64, error) {
	if s == "" || s == "0" {
		return 0, nil
	}
	mult := int64(1)
	upper := strings.ToUpper(strings.TrimSpace(s))
	for _, suf := range []struct {
		s string
		m int64
	}{
		{"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10},
	} {
		if strings.HasSuffix(upper, suf.s) {
			mult = suf.m
			upper = strings.TrimSuffix(upper, suf.s)
			break
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(upper), 10, 64)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, errors.New("negative limit")
	}
	return n * mult, nil
}
