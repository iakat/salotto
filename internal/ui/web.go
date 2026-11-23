package ui

import (
	_ "embed"
	"html/template"
	"net/http"
	"strconv"

	tea "github.com/charmbracelet/bubbletea"
)

// termCSS is buildkite's terminal.css, which is what gives term-fgx* and
// term-bgx* their colours. It is embedded rather than fetched: the page must
// render with no network access and no CDN.
//
//go:embed assets/terminal.css
var termCSS string

// pageTemplate is the smallest possible shell: one <pre> for the terminal
// rendering and nothing else. There is no second layout to keep in sync with
// the TUI, which is the point of the terminal-to-HTML approach.
var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>salotto</title>
<link rel="stylesheet" href="/terminal.css">
<style>
  html, body { margin: 0; height: 100%; background: #12121a; }
  body { display: flex; align-items: stretch; }
  div.wrap { flex: 1; display: flex; flex-direction: column; min-height: 0; }
  pre.term-container { flex: 1; margin: 0; padding: 12px; overflow: auto;
    font: 14px/1.35 ui-monospace, SFMono-Regular, Menlo, monospace; }
  form.input { display: flex; gap: 8px; padding: 8px 12px;
    border-top: 1px solid #2a2a3a; background: #16161f; }
  form.input input { flex: 1; background: #0e0e15; color: #e6e6f0;
    border: 1px solid #2a2a3a; border-radius: 4px; padding: 6px 8px;
    font: 14px ui-monospace, SFMono-Regular, Menlo, monospace; }
  form.input button { background: #2d2d44; color: #e6e6f0; border: 0;
    border-radius: 4px; padding: 6px 14px; font: 14px ui-monospace, monospace;
    cursor: pointer; }
</style>
</head>
<body>
<div class="wrap">
<pre class="term-container">{{.Terminal}}</pre>
<form class="input" method="post" action="/" autocomplete="off">
  <input name="line" placeholder="message or /command" autofocus>
  <button type="submit">send</button>
</form>
</div>
</body>
</html>
`))

// Handler returns the web front end: the same Program the TUI runs, driven by
// HTTP for now. A WebSocket transport (W1) replaces the request-per-frame part;
// the render half does not change, because it is this file.
//
// It takes a *Program rather than a Model because a browser that can only render
// is a browser that cannot talk: without the Program there is no way for a POST
// to reach the transport, and the page would be a read-only log in a chat client.
func Handler(p *Program) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			// A submitted line goes through the Program, so it takes the same
			// path as a keystroke in the TUI: the local echo, the window
			// routing and the command mapping all happen in one place.
			if err := r.ParseForm(); err != nil {
				http.Error(w, "bad form", http.StatusBadRequest)
				return
			}
			// The window is applied first: a browser that submits a message
			// along with a window switch means "say this there", and doing it
			// the other way sends the line to whichever window was active when
			// the form was rendered.
			if win := r.PostFormValue("window"); win != "" {
				p.SetActiveWindow(win)
			}
			if line := r.PostFormValue("line"); line != "" {
				for _, ru := range line {
					p.Step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{ru}})
				}
				p.Step(tea.KeyMsg{Type: tea.KeyEnter})
			}
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Query parameters are the preview's input: ?width=&height= size the
		// model the same way the terminal driver's WindowSizeMsg does, so a
		// single page can be asserted against in a test without a browser.
		if v := r.URL.Query().Get("width"); v != "" {
			if n, ok := parsePositive(v); ok {
				p.Step(windowSize(n, p.Model().height))
			}
		}
		if v := r.URL.Query().Get("height"); v != "" {
			if n, ok := parsePositive(v); ok {
				p.Step(windowSize(p.Model().width, n))
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// template.HTML, not a string: the converter's output is markup, and
		// html/template would escape every span it produced.
		if err := pageTemplate.Execute(w, map[string]template.HTML{
			"Terminal": template.HTML(HTML(p.View())),
		}); err != nil {
			return
		}
	})

	// A form on the same page, so the browser front end can send a line without
	// any JavaScript. The WebSocket transport (W1) makes this real-time; the
	// submit path it will use is this one.
	mux.HandleFunc("/terminal.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = w.Write([]byte(termCSS))
	})
	return mux
}

func parsePositive(v string) (int, bool) {
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func windowSize(w, h int) tea.Msg { return tea.WindowSizeMsg{Width: w, Height: h} }
