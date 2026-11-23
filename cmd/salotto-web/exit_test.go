package main

import (
	"errors"
	"net"
	"net/http"
	"os/exec"
	"testing"
	"time"
)

// TestWebExitsWhenTheBouncerGoesAway pins the choice that a lost bouncer makes
// the process exit rather than keep serving.
//
// There is no in-process reconnect, so a front end whose connection died is
// wrong in the worst way: the page still renders, the input box still accepts
// text, POST still answers 303, and none of it reaches anything. A UI that lies
// quietly is worse than one that is visibly down, so the process exits and the
// supervisor brings back a fresh dial with a replayed buffer. That is only true
// if the exit actually happens, which is what this test runs the built binary to
// check rather than asserting the intent in a comment.
func TestWebExitsWhenTheBouncerGoesAway(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go binary not available to build the front end")
	}

	// A listener that accepts and then hangs up: the transport dials, then reads
	// EOF. No bouncer is needed to prove the front end notices.
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

	bin := t.TempDir() + "/salotto-web"
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	addr := freeAddr(t)
	cmd := exec.Command(bin, "-addr", addr, "-bouncer", ln.Addr().String(), "-nick", "e2e-ui")
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// The process must go away on its own, and must do it promptly: a front end
	// that notices a dead bouncer ten minutes later is still lying for ten
	// minutes.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exitErr *exec.ExitError
		if err == nil {
			t.Fatal("the front end stayed up with a dead bouncer")
		}
		if !errors.As(err, &exitErr) {
			t.Fatalf("unexpected exit: %v", err)
		}
		if code := exitErr.ExitCode(); code != 1 {
			t.Fatalf("exit code = %d, want 1 (the bouncer connection is gone)", code)
		}
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the front end kept serving with a dead bouncer")
	}
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

	bin := t.TempDir() + "/salotto-web"
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

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

	bin := t.TempDir() + "/salotto-web"
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

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
