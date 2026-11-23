package ui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestWebRendersTheTerminalView is the web half of §2.0 end to end over HTTP:
// the same Model the SSH driver renders, served as HTML with no ESC bytes left
// and the converter's classes present.
func TestWebRendersTheTerminalView(t *testing.T) {
	m := seeded()
	srv := httptest.NewServer(Handler(m))
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

	srv := httptest.NewServer(Handler(m))
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

	srv := httptest.NewServer(Handler(New("status")))
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
