package main

import (
	"errors"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestALostBouncerKillsTheSessionNotTheFrontEnd pins the boundary the PTY bridge moved.
//
// The old front end held one bouncer connection for the process, so losing it meant the
// whole web server was lying and had to exit. Now each session owns its own child, and
// the connection that dies belongs to the session: the browser's terminal shows the
// child exiting, and the front end keeps serving so the next tab gets a fresh dial. That
// is a different contract and the test has to say so, because "the process exits" is the
// kind of assertion that is either true for a reason or quietly wrong for a month.
func TestALostBouncerKillsTheSessionNotTheFrontEnd(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go binary not available to build the front end")
	}

	// A listener that accepts and then hangs up: the child dials, reads EOF, and exits.
	// No bouncer is needed to prove the session notices.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	bin := buildFrontEnd(t)
	addr := freeAddr(t)
	cmd := exec.Command(bin, "-addr", addr, "-bouncer", ln.Addr().String(), "-nick", "e2e-ui")
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	// The front end must stay up: it is a bridge, and a bridge with one broken session is
	// still a bridge.
	waitListening(t, "http://"+addr+"/")

	// And the session must end, visibly. Opening the socket starts a child; the child
	// finds the bouncer gone and exits, which closes the socket. A socket that stays open
	// with a dead child behind it is a terminal that accepts typing and delivers nothing.
	u := "ws://" + addr + "/ws?cols=80&rows=24"
	conn, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	for {
		var m struct {
			Type string `json:"type"`
		}
		if err := conn.ReadJSON(&m); err != nil {
			// Closed, which is the correct ending: the child is gone.
			return
		}
		if m.Type == "exit" || m.Type == "error" {
			return
		}
	}
}

// waitListening polls until the URL answers 200.
func waitListening(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s never answered", url)
}

// freeAddr returns a port that was free a moment ago, for the child to bind.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// TestWebExitsWhenTheBouncerCannotBeDialled covers the other half: a configured
// bouncer that refuses the connection at startup.
//
// The old behaviour was to log a warning and serve a local-only UI, which is the
// same lie in a different place - the page accepts lines and delivers nothing. An
// empty -bouncer still means a deliberate local-only UI; a dial that failed means
// the front end could not attach and should be retried.
func TestWebExitsWhenTheBouncerCannotBeDialled(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go binary not available to build the front end")
	}

	// A port with nothing listening: take one and close it again. The dial is
	// refused, which is the case that used to fall through to a local-only UI.
	dead := freeAddr(t)

	bin := buildFrontEnd(t)

	cmd := exec.Command(bin, "-addr", freeAddr(t), "-bouncer", dead, "-nick", "e2e-ui")
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exitErr *exec.ExitError
		if err == nil {
			t.Fatal("the front end served a local-only UI for a bouncer it could not dial")
		}
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			t.Fatalf("want exit code 1, got %v", err)
		}
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the front end hung dialling a dead bouncer")
	}
}

// TestWebServesLocalOnlyWithoutABouncer keeps the deliberate case deliberate: an
// empty address is a renderer preview and must not exit.
func TestWebServesLocalOnlyWithoutABouncer(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go binary not available to build the front end")
	}

	bin := buildFrontEnd(t)

	addr := freeAddr(t)
	cmd := exec.Command(bin, "-addr", addr)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	// It must be answering, not exiting: poll the page until it responds.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("a local-only UI without -bouncer never answered")
}

// buildFrontEnd builds the web front end AND the terminal client it spawns, into one
// directory, because that is how the image ships them: resolveTUI looks beside itself
// first, and a web binary alone in an empty directory has no sibling to find.
func buildFrontEnd(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, c := range []struct{ pkg, name string }{
		{".", "salotto-web"},
		{"../salotto-tui", "salotto-tui"},
	} {
		out := filepath.Join(dir, c.name)
		build := exec.Command("go", "build", "-o", out, c.pkg)
		if b, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", c.name, err, b)
		}
	}
	return filepath.Join(dir, "salotto-web")
}
