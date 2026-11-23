// Command salotto-ssh serves the salotto UI over SSH.
//
// It is one of the three front ends in QUEUE.md §2.0 and it contains no view
// code: it builds a ui.Model, hands it to wish's bubbletea middleware, and the
// middleware drives the same Program the tests drive by hand.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	wishtea "github.com/charmbracelet/wish/bubbletea"

	"github.com/iakat/salotto/internal/ui"
)

var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	addr := flag.String("addr", ":2222", "listen address")
	hostKey := flag.String("host-key", "", "path to the SSH host key (created if missing)")
	banner := flag.String("banner", "salotto", "banner name shown to clients")
	window := flag.String("window", "status", "initial window")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("salotto-ssh %s\n", version)
		return 0
	}

	// A daemon has no TTY. Without an explicit profile lipgloss resolves the
	// ASCII one and the SSH client receives a colourless screen; see
	// docs/UI.md.
	ui.Configure()

	opts := []ssh.Option{
		wish.WithAddress(*addr),
		wish.WithHostKeyPath(*hostKey),
		wish.WithBanner(ui.Banner(*banner)),
		wish.WithMiddleware(
			wishtea.Middleware(func(s ssh.Session) (tea.Model, []tea.ProgramOption) {
				m := ui.New(*window)
				// The middleware already wires the session's stdin/stdout and
				// its own renderer; the profile is forced in main so the
				// daemon's missing TTY cannot downgrade it.
				return m, []tea.ProgramOption{tea.WithAltScreen()}
			}),
		),
	}
	srv, err := wish.NewServer(opts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "salotto-ssh: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "salotto-ssh: %v\n", err)
		return 1
	}
	slog.Info("ssh listening", "addr", ln.Addr().String(), "window", *window)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, net.ErrClosed) {
			slog.Warn("ssh shutdown", "err", err)
		}
	case err := <-errCh:
		if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, ssh.ErrServerClosed) {
			slog.Error("ssh serve failed", "err", err)
			return 1
		}
	}
	return 0
}
