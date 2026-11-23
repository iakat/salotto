package ui

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// progFor wraps a pre-seeded Model in a Program, so these renderer tests keep
// asserting on the model they built while going through the same Handler the
// server uses.
func progFor(m Model) *Program { return &Program{model: m} }

// TestWebRendersTheTerminalView is the web half of §2.0 end to end over HTTP:
// the same Model the SSH driver renders, served as HTML with no ESC bytes left
// and the converter's classes present.
func TestWebRendersTheTerminalView(t *testing.T) {
	m := seeded()
	srv := httptest.NewServer(Handler(progFor(m)))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if strings.Contains(page, "\x1b") {
		t.Fatal("the page contains ESC bytes: the ANSI was not converted")
	}
	if !strings.Contains(page, `class="term-container"`) {
		t.Fatal("the page has no term-container element")
	}
	if !strings.Contains(page, "term-fgx") {
		t.Fatalf("the page has no terminal colour classes: %s", page[:min(400, len(page))])
	}
	// The text itself survives: the browser gets the terminal rendering, not a
	// summary of it.
	if !strings.Contains(page, "ciao") {
		t.Fatal("the message text is missing from the page")
	}
	if !strings.Contains(page, "/terminal.css") {
		t.Fatal("the page does not reference the stylesheet it needs")
	}

	cssResp, err := http.Get(srv.URL + "/terminal.css")
	if err != nil {
		t.Fatalf("GET /terminal.css: %v", err)
	}
	defer cssResp.Body.Close()
	css, _ := io.ReadAll(cssResp.Body)
	if !strings.Contains(string(css), "term-fgx") {
		t.Fatal("the served stylesheet is not terminal.css")
	}
}

// TestWebMatchesHeadlessView: the page body must be the conversion of the very
// string the headless driver produced. If a second renderer ever appears, this
// is what fails.
func TestWebMatchesHeadlessView(t *testing.T) {
	m := seeded()
	_, view := Headless(m)
	want := HTML(view)

	srv := httptest.NewServer(Handler(progFor(m)))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(body), want) {
		t.Fatalf("the page does not embed the headless rendering\nwant: %q", want)
	}
}

// TestWebResizeIsTheSameWindowSizeMsg: the preview's query parameters must go
// through the same message the terminal driver sends, not a parallel path. The
// page for ?width=60&height=14 must be byte-identical to the conversion of a
// model that received that WindowSizeMsg itself.
func TestWebResizeIsTheSameWindowSizeMsg(t *testing.T) {
	want := New("status")
	want, _ = want.Step(tea.WindowSizeMsg{Width: 60, Height: 14})
	wantHTML := HTML(want.View())

	srv := httptest.NewServer(Handler(NewProgram("status", nil)))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/?width=60&height=14")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), wantHTML) {
		t.Fatalf("the resized page is not the converted WindowSizeMsg rendering\nwant: %q", wantHTML)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestWebSubmitReachesTheBouncer is the behaviour the browser front end was
// missing: posting the form actually sends a line. Before this the web UI could
// render and nothing else.
func TestWebSubmitReachesTheBouncer(t *testing.T) {
	Configure()
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	prog := NewProgram("#salotto", tr)
	srv := httptest.NewServer(Handler(prog))
	defer srv.Close()

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.PostForm(srv.URL+"/", map[string][]string{"line": {"hello from a browser"}})
	if err != nil {
		t.Fatalf("POST /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}

	if got := fb.expect(t); got != "PRIVMSG #salotto :hello from a browser" {
		t.Fatalf("sent %q", got)
	}
}

// TestWebSubmitRejectsBareGet pins that the form is the only way to write: a GET
// with ?line= must not send anything, or a prefetcher would post messages.
func TestWebSubmitRejectsBareGet(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	srv := httptest.NewServer(Handler(NewProgram("#salotto", tr)))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/?line=should+not+send")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	select {
	case line := <-fb.sent:
		t.Fatalf("a GET sent %q", line)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestWebWindowSelection: a browser switches windows by name, and the next line
// must go there.
func TestWebWindowSelection(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	prog := NewProgram("#salotto", tr)
	srv := httptest.NewServer(Handler(prog))
	defer srv.Close()
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	resp, err := client.PostForm(srv.URL+"/", map[string][]string{
		"window": {"#other"},
		"line":   {"routed here"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := fb.expect(t); got != "PRIVMSG #other :routed here" {
		t.Fatalf("sent %q, want the selected window", got)
	}
}

// TestEveryColourIsAVariable pins the property that makes theming possible: the
// stylesheet has no literal colours outside the palette blocks. One hard-coded hex
// is a theme where that element stays dark in light mode, and it is invisible until
// somebody switches.
func TestEveryColourIsAVariable(t *testing.T) {
	css := pageHTML(t)
	// Strip the palette blocks first; they are where literals belong.
	body := css
	for _, marker := range []string{":root {", "body.light {"} {
		for {
			i := strings.Index(body, marker)
			if i < 0 {
				break
			}
			j := strings.Index(body[i:], "}")
			if j < 0 {
				break
			}
			body = body[:i] + body[i+j:]
		}
	}
	if strings.Contains(body, "background: #") || strings.Contains(body, "color: #") {
		idx := strings.Index(body, "color: #")
		if idx < 0 {
			idx = strings.Index(body, "background: #")
		}
		t.Fatalf("literal colour outside the palette at %q", body[max(0, idx-60):idx+40])
	}
}

// TestLightThemeRedefinesTheWholePalette pins that the two palettes have the same
// keys. A variable defined in one and missing from the other falls back to the
// inherited value, which shows as one element that did not change theme.
func TestLightThemeRedefinesTheWholePalette(t *testing.T) {
	css := pageHTML(t)
	dark := paletteKeys(t, css, ":root {")
	light := paletteKeys(t, css, "body.light {")
	if len(dark) == 0 {
		t.Fatal("no dark palette found")
	}
	// Only the colour half has to match: --font and friends are shared and live in
	// :root once. The rule that matters is that no COLOUR is defined in one theme
	// and missing from the other, because it would fall back to the inherited value
	// and show as one element that did not change theme.
	for k := range dark {
		if !strings.HasPrefix(k, "--") || strings.HasPrefix(k, "--font") {
			continue
		}
		if _, ok := light[k]; !ok {
			t.Fatalf("light theme does not define %s", k)
		}
	}
	for k := range light {
		if !strings.HasPrefix(k, "--") || strings.HasPrefix(k, "--font") {
			continue
		}
		if _, ok := dark[k]; !ok {
			t.Fatalf("light theme defines %s, which the dark one does not", k)
		}
	}
}

// paletteKeys lists the --variables defined in the block that starts at marker.
func paletteKeys(t *testing.T, css, marker string) map[string]bool {
	t.Helper()
	i := strings.Index(css, marker)
	if i < 0 {
		return nil
	}
	j := strings.Index(css[i:], "}")
	if j < 0 {
		return nil
	}
	out := map[string]bool{}
	for _, line := range strings.Split(css[i:i+j], ";") {
		line = strings.TrimSpace(line)
		if name, _, ok := strings.Cut(line, ":"); ok && strings.HasPrefix(name, "--") {
			out[strings.TrimSpace(name)] = true
		}
	}
	return out
}

// TestClientHasNoLiteralColoursInJS pins that the browser half does not hard-code a
// colour either: a colour set from JavaScript is a theme the stylesheet cannot reach.
func TestClientHasNoLiteralColoursInJS(t *testing.T) {
	js := clientJSSource(t)
	for _, bad := range []string{"#ffd76b", "#12121a", "#16161f", "style.color"} {
		if strings.Contains(js, bad) {
			t.Fatalf("client.js sets %q; colours belong in the stylesheet's variables", bad)
		}
	}
}

// TestThemeToggleIsWired pins that the control exists in the markup the server sends
// and that the client binds it. A button with no handler is a UI that lies.
func TestThemeToggleIsWired(t *testing.T) {
	html := pageHTML(t)
	if !strings.Contains(html, `id="theme"`) {
		t.Fatal("no theme control in the page")
	}
	js := clientJSSource(t)
	if !strings.Contains(js, `getElementById("theme")`) || !strings.Contains(js, "toggleTheme") {
		t.Fatal("the client never binds the theme control")
	}
}

// TestKeybindsAreDocumented pins that every bind is reachable and named the same in
// the handler and the hint text. A bind that exists but is undiscoverable is unused.
func TestKeybindsAreDocumented(t *testing.T) {
	js := clientJSSource(t)
	for _, key := range []string{`case "t"`, `case "k"`, `case "j"`, `case "i"`, `case "c"`} {
		if !strings.Contains(js, key) {
			t.Fatalf("keybind %s is missing", key)
		}
	}
	if !strings.Contains(js, "Alt-T") && !strings.Contains(pageHTML(t), "Alt-T") {
		t.Fatal("the theme keybind is not documented anywhere the user can see")
	}
}

// TestSettingsSurviveACorruptStore pins that a broken localStorage cannot break the
// client. Preferences are not worth a blank page, and localStorage is shared with
// anything else on the origin.
func TestSettingsSurviveACorruptStore(t *testing.T) {
	js := clientJSSource(t)
	if !strings.Contains(js, "JSON.parse") || !strings.Contains(js, "catch") {
		t.Fatal("setting parsing has no error path")
	}
	if !strings.Contains(js, "var defaults") {
		t.Fatal("there is no defaults object to fall back to")
	}
}

// pageHTML renders the page and returns it, so the tests read what the browser gets.
func pageHTML(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	if err := pageTemplate.Execute(&buf, nil); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// clientJSSource returns the embedded client.
func clientJSSource(t *testing.T) string {
	t.Helper()
	if clientJS == "" {
		t.Fatal("client.js is empty")
	}
	return clientJS
}
