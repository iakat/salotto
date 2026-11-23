package main

import (
	"errors"
	"net"
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
