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

// clientJS is the browser half of the live transport: it converts the server's
// ANSI render to HTML and adds the structure a rendered string cannot carry.
// Embedded for the same reason as the CSS - the page must work with no CDN.
//
//go:embed assets/client.js
var clientJS string

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
  body { display: flex; align-items: stretch; color: #e6e6f0; }
  /* The sidebar is the one thing a terminal rendering cannot express, so it is
     real DOM and not part of the <pre>. It mirrors the model's window list. */
  aside.windows { width: 180px; flex: 0 0 180px; background: #16161f;
    border-right: 1px solid #2a2a3a; overflow-y: auto; padding: 8px 0; }
  aside.windows h2 { margin: 4px 12px 8px; font: 600 11px ui-monospace, monospace;
    letter-spacing: .08em; text-transform: uppercase; color: #7a7a95; }
  aside.windows .win { padding: 5px 12px; cursor: pointer;
    font: 13px ui-monospace, SFMono-Regular, Menlo, monospace;
    white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  aside.windows .win:hover { background: #1e1e2b; }
  aside.windows .win.active { background: #2d2d44; color: #fff; font-weight: 600; }
  aside.windows .win { display: flex; justify-content: space-between; gap: 6px; }
  aside.windows .win .name { overflow: hidden; text-overflow: ellipsis; }
  /* A mention is the strongest signal in the sidebar: something was said to you.
     It outranks "there is something new", so the mention class wins over unread
     and the count is not drawn at all. */
  aside.windows .win.mention { color: #ffd76b; font-weight: 700; }
  aside.windows .badge { flex: 0 0 auto; min-width: 16px; text-align: center;
    border-radius: 8px; padding: 0 4px; font-size: 11px; line-height: 15px;
    background: #3a3a55; color: #cfcfe8; }
  aside.windows .win.mention .badge { background: #ffd76b; color: #22190a; }
  /* Search sits above the terminal: a query is about the scrollback, and the
     results overlay it rather than replacing it, so context is not lost. */
  div.searchbar { display: flex; gap: 8px; padding: 6px 12px;
    border-bottom: 1px solid #2a2a3a; background: #16161f; }
  div.searchbar input { flex: 1; background: #0e0e15; color: #e6e6f0;
    border: 1px solid #2a2a3a; border-radius: 4px; padding: 4px 8px;
    font: 13px ui-monospace, SFMono-Regular, Menlo, monospace; }
  div.results { max-height: 40%; overflow-y: auto; background: #13131c;
    border-bottom: 1px solid #2a2a3a;
    font: 13px ui-monospace, SFMono-Regular, Menlo, monospace; }
  div.results .head { padding: 6px 12px; color: #7a7a95; font-size: 11px;
    text-transform: uppercase; letter-spacing: .06em; }
  div.results .hit { padding: 4px 12px; cursor: pointer; color: #c6c6dd; }
  div.results .hit:hover { background: #1e1e2b; }
  div.results .hl { color: #ffd76b; font-weight: 600; }
  div.results .hn { color: #8fb7ff; }
  div.wrap { flex: 1; display: flex; flex-direction: column; min-height: 0; }
  div.banner { padding: 6px 12px; background: #5a2020; color: #ffdede;
    font: 13px ui-monospace, monospace; }
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
  footer.status { padding: 4px 12px; background: #0e0e15; color: #7a7a95;
    border-top: 1px solid #2a2a3a;
    font: 12px ui-monospace, SFMono-Regular, Menlo, monospace;
    white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  aside.nicks { width: 150px; flex: 0 0 150px; background: #14141d;
    border-left: 1px solid #2a2a3a; overflow-y: auto; padding: 8px 0; }
  aside.nicks h2 { margin: 4px 12px 8px; font: 600 11px ui-monospace, monospace;
    letter-spacing: .08em; text-transform: uppercase; color: #7a7a95; }
  aside.nicks .nick { padding: 2px 12px; font: 12px ui-monospace, monospace;
    color: #b8b8d0; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  aside.nicks .nick.op { color: #7ddc8a; font-weight: 600; }
  aside.windows .count-nicks { color: #5c5c78; font-size: 11px; }
  @media (max-width: 640px) { aside.windows, aside.nicks { display: none; } }
</style>
</head>
<body>
<aside class="windows" id="windows"><h2>windows</h2></aside>
<div class="wrap">
<div class="banner" id="banner" hidden></div>
<div class="searchbar">
  <input id="search" placeholder="search scrollback (enter to run, esc to clear)">
</div>
<div class="results" id="results" hidden></div>
<pre class="term-container" id="term">{{.Terminal}}</pre>
<form class="input" id="input" method="post" action="/" autocomplete="off">
  <input name="line" id="line" placeholder="message or /command" autofocus>
  <button type="submit">send</button>
</form>
<footer class="status" id="status">{{.Status}}</footer>
</div>
<aside class="nicks" id="nicks" hidden></aside>
<script src="/client.js"></script>
</body>
</html>
`))

// Handler returns the web front end: the same Program the TUI runs, driven two
// ways at once. /ws is the live transport and is what a browser uses; the form
// POST remains because it is the degraded path when a socket cannot be opened,
// and because it is how the tests drive the page without a browser.
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
		// The page is rendered with the model's own state so a browser that
		// cannot open a socket still shows the window list and the connection
		// status; the socket only adds updates to what is already correct.
		m := p.Model()
		if err := pageTemplate.Execute(w, map[string]interface{}{
			"Terminal": template.HTML(HTML(p.View())),
			"Status":   m.Status(),
		}); err != nil {
			return
		}
	})

	// W1: the live transport. The page falls back to the form POST path if the
	// socket cannot be established, so a browser without WebSocket support (or
	// behind something that strips the upgrade) is degraded, not broken.
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		p.ServeWS(w, r)
	})

	mux.HandleFunc("/client.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		_, _ = w.Write([]byte(clientJS))
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
