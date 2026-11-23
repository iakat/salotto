// Command salotto-ssh serves the salotto UI over SSH.
//
// It is one of the three front ends in QUEUE.md §2.0 and it contains no view
// code: it builds a ui.Program, runs it as a bubbletea Program on the SSH
// session's channel, and lets the transport pump lines into it.
//
// It deliberately does not use wish's bubbletea middleware. The middleware
// constructs the tea.Program itself, which means a goroutine holding a transport
// has no way to send into it — and a bouncer front end that cannot be fed lines
// is a screen with nothing on it. Creating the Program here costs four lines and
// makes the data flow obvious: wish handles SSH, bubbletea handles the terminal,
// and this file is the only place they meet.
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

	"github.com/iakat/salotto/internal/ui"
)

var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	addr := flag.String("addr", ":2222", "listen address")
	bouncer := flag.String("bouncer", "", "bouncer address to attach to (host:port); empty serves a local-only UI")
	nick := flag.String("nick", "salotto", "nick to register with the bouncer")
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
		wish.WithMiddleware(session(*window, *bouncer, *nick)),
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
	slog.Info("ssh listening", "addr", ln.Addr().String(), "window", *window, "bouncer", *bouncer)

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

// session is the SSH middleware: one TUI per session, each with its own bouncer
// connection, because two SSH sessions are two bouncer clients and the bouncer
// fans lines out to both. Sharing one transport would make the second session a
// spectator.
func session(window, bouncer, nick string) wish.Middleware {
	return func(next ssh.Handler) ssh.Handler {
		return func(s ssh.Session) {
			tr, err := dialBouncer(bouncer, nick)
			if err != nil {
				// A failed dial is not fatal: the UI is still usable, it just has
				// nothing to show, and saying so beats a silent blank screen.
				slog.Warn("bouncer dial failed; serving a local-only UI", "err", err)
				fmt.Fprintf(s, "salotto: %v (local-only UI)\r\n", err)
			}
			p := ui.NewProgram(window, tr)

			program := tea.NewProgram(p,
				tea.WithInput(s),
				tea.WithOutput(s),
				tea.WithAltScreen(),
			)

			// The transport pump sends into the Program this file owns, which is
			// exactly why the Program is built here rather than by the middleware.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tr != nil {
				go func() {
					defer program.Quit()
					for {
						select {
						case <-ctx.Done():
							return
						case msg, ok := <-tr.Msgs():
							if !ok {
								return
							}
							program.Send(msg)
						}
					}
				}()
			}

			if _, err := program.Run(); err != nil {
				slog.Warn("tui exited", "err", err)
			}
			if tr != nil {
				_ = tr.Close()
			}
			next(s)
		}
	}
}

// dialBouncer connects to the bouncer if one was configured. An empty address is
// not an error: the UI works without a bouncer, which is what a quick look at the
// renderer wants. A nil Transport is handled by ui.Program everywhere.
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
