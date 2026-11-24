package ui

import (
	"bytes"
	_ "embed"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"sync"
)

// This page is a terminal, and nothing else. There is no window list, no nick panel,
// no header and no footer in the HTML, because the model draws all of those: the page
// supplies a viewport and a way to type, and xterm.js draws the bytes the program
// writes. Every panel that used to live here was a second renderer, and each one
// drifted - a duplicated nicklist, a twice-printed input line, a box inside a box.
//
//go:embed assets/manifest.webmanifest
var manifestJSON string

//go:embed assets/sw.js
var serviceWorkerJS string

//go:embed assets/term.html
var termHTML string

//go:embed assets/term.js
var termJS string

//go:embed assets/xterm/xterm.js
var xtermJS string

//go:embed assets/xterm/xterm.css
var xtermCSS string

//go:embed assets/xterm/LICENSE
var xtermLicense string

// TermHandler serves the terminal page and the vendored emulator.
func TermHandler(srv *TermServer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// The page is a shell around a live terminal; it must never be cached, or a
		// redeploy leaves browsers on an old client talking to a new server.
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(termHTML))
	})
	mux.HandleFunc("/term.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		_, _ = w.Write([]byte(termJS))
	})
	mux.HandleFunc("/xterm.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		// Immutable and versioned in the path would be better; embedded means it can
		// only change when the binary does.
		w.Header().Set("Cache-Control", "max-age=86400")
		_, _ = w.Write([]byte(xtermJS))
	})
	mux.HandleFunc("/xterm.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "max-age=86400")
		_, _ = w.Write([]byte(xtermCSS))
	})
	mux.HandleFunc("/xterm-LICENSE", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(xtermLicense))
	})
	// The PWA surface. A service worker is only allowed to control the scope it is served
	// from, so it is served at the root - a worker under /assets/ would control /assets/
	// and nothing else, and the notification it exists to show would never fire.
	mux.HandleFunc("/manifest.webmanifest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/manifest+json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(manifestJSON))
	})
	mux.HandleFunc("/sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		// A cached service worker is an old worker serving a new page, and the classic
		// symptom is "I deployed the fix and the phone still shows the old behaviour".
		w.Header().Set("Cache-Control", "no-store")
		// A worker served with a Service-Worker-Allowed header can be scoped wider than its
		// path; served at the root it needs nothing, but being explicit stops a future move
		// from silently narrowing the scope.
		w.Header().Set("Service-Worker-Allowed", "/")
		_, _ = w.Write([]byte(serviceWorkerJS))
	})
	// Icons: generated, not shipped as files. A manifest without icons is not installable, and
	// two PNGs in the binary for a terminal app is 20 KiB of the same colour.
	mux.HandleFunc("/icon-192.png", iconHandler(192))
	mux.HandleFunc("/icon-512.png", iconHandler(512))
	mux.Handle("/ws", srv)
	return mux
}

// iconHandler serves a solid-colour PNG at the requested size, generated on first request.
//
// The manifest needs icons or the browser will not offer to install the app; a terminal client
// has no logo worth embedding, and shipping two identical PNGs of one colour is two files that
// can go stale. Generating them is a few lines and they always match the theme colour.
func iconHandler(size int) http.HandlerFunc {
	var once sync.Once
	var png []byte
	return func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { png = solidPNG(size) })
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "max-age=86400")
		_, _ = w.Write(png)
	}
}

// solidPNG encodes a size x size image of the terminal's background colour with a lighter
// bar across the middle, so the icon reads as a terminal rather than a blank square.
func solidPNG(size int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	bg := color.RGBA{R: 0x10, G: 0x10, B: 0x14, A: 0xff}
	fg := color.RGBA{R: 0x3a, G: 0xd0, B: 0x8a, A: 0xff}
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			img.Set(x, y, bg)
		}
	}
	// Two bars: the first full width at 40% height, the second at 60% and half width. Enough
	// to look intentional at 192px without being a glyph that renders badly at 48.
	bar := size / 16
	if bar < 2 {
		bar = 2
	}
	for y := size * 40 / 100; y < size*40/100+bar; y++ {
		for x := size / 8; x < size-size/8; x++ {
			img.Set(x, y, fg)
		}
	}
	for y := size * 58 / 100; y < size*58/100+bar; y++ {
		for x := size / 8; x < size/2; x++ {
			img.Set(x, y, fg)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil
	}
	return buf.Bytes()
}
