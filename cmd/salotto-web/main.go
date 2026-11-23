// Command salotto-web serves the salotto UI as HTML.
//
// This is the second front end in QUEUE.md §2.0, and it is deliberately thin:
// it converts a ui.Program's View() to HTML with buildkite's terminal-to-html, so
// the browser sees the terminal rendering instead of a second UI. The WebSocket
// transport (W1) will replace the request-per-page part; the rendering half is
// this file and does not change.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/iakat/salotto/internal/ui"
)

var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	window := flag.String("window", "status", "initial window")
	bouncer := flag.String("bouncer", "", "bouncer address to attach to (host:port); empty serves a local-only UI")
	nick := flag.String("nick", "salotto", "nick to register with the bouncer")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("salotto-web %s\n", version)
		return 0
	}

	// Same reason as salotto-ssh: no TTY means no colour unless the profile is
	// pinned, and the HTML converter only understands 8-bit SGR.
	ui.Configure()

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "salotto-web: %v\n", err)
		return 1
	}
	// One transport for the process, not one per request: the browser polls, and
	// a bouncer connection per page load would open and close a client on every
	// refresh, which the bouncer sees as a flapping user. It is attached to the
	// shared Program the handler serves.
	tr, err := dialBouncer(*bouncer, *nick)
	if err != nil {
		slog.Warn("bouncer dial failed; serving a local-only UI", "err", err)
	}
	if tr != nil {
		defer tr.Close()
	}
	prog := ui.NewProgram(*window, tr)

	srv := &http.Server{
		Handler:           ui.Handler(prog),
		ReadHeaderTimeout: 5 * time.Second,
	}
	slog.Info("web listening", "addr", ln.Addr().String(), "window", *window)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	if tr != nil {
		go pump(prog, tr)
	}

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		slog.Error("web serve failed", "err", err)
		return 1
	}
	return 0
}

// pump feeds transport messages into the Program. The web handler renders on
// demand, so the pump exists to keep the model current between requests: without
// it the page would show whatever arrived before the first render and never
// change.
func pump(prog *ui.Program, tr *ui.Transport) {
	for {
		msg, ok := prog.Next()
		if !ok {
			return
		}
		prog.Step(msg)
	}
}

// dialBouncer connects to the bouncer if one was configured. An empty address is
// not an error: the UI works without a bouncer, which is what a quick look at the
// renderer wants.
func dialBouncer(addr, nick string) (*ui.Transport, error) {
	if addr == "" {
		return nil, nil
	}
	tr, err := ui.Dial(addr)
	if err != nil {
		return nil, err
	}
	tr.Register(nick, "")
	return tr, nil
}
