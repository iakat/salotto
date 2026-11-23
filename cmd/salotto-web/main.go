// Command salotto-web serves the salotto UI as HTML.
//
// This is the second front end in QUEUE.md §2.0, and it is deliberately thin:
// it converts a ui.Model's View() to HTML with buildkite's terminal-to-html, so
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
	srv := &http.Server{
		Handler:           ui.Handler(ui.New(*window)),
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

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		slog.Error("web serve failed", "err", err)
		return 1
	}
	return 0
}
