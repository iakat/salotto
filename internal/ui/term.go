package ui

import (
	_ "embed"
	"net/http"
)

// This page is a terminal, and nothing else. There is no window list, no nick panel,
// no header and no footer in the HTML, because the model draws all of those: the page
// supplies a viewport and a way to type, and xterm.js draws the bytes the program
// writes. Every panel that used to live here was a second renderer, and each one
// drifted - a duplicated nicklist, a twice-printed input line, a box inside a box.
//
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
	mux.Handle("/ws", srv)
	return mux
}
