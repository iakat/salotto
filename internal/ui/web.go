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
  pre.term-container { flex: 1; margin: 0; padding: 12px; overflow: auto;
    font: 14px/1.35 ui-monospace, SFMono-Regular, Menlo, monospace; }
</style>
</head>
<body>
<pre class="term-container">{{.Terminal}}</pre>
</body>
</html>
`))

// Handler returns the web front end: the same Model, driven by HTTP for now.
// A WebSocket transport (W1) replaces the request-per-frame part; the render
// half does not change, because it is this file.
func Handler(m Model) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Query parameters are the preview's input: ?width=&height= size the
		// model the same way the terminal driver's WindowSizeMsg does, so a
		// single page can be asserted against in a test without a browser.
		if v := r.URL.Query().Get("width"); v != "" {
			if n, ok := parsePositive(v); ok {
				m, _ = m.Step(windowSize(n, m.height))
			}
		}
		if v := r.URL.Query().Get("height"); v != "" {
			if n, ok := parsePositive(v); ok {
				m, _ = m.Step(windowSize(m.width, n))
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// template.HTML, not a string: the converter's output is markup, and
		// html/template would escape every span it produced.
		if err := pageTemplate.Execute(w, map[string]template.HTML{
			"Terminal": template.HTML(HTML(m.View())),
		}); err != nil {
			return
		}
	})

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
