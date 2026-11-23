// Command salotto-web serves the salotto TUI to a browser.
//
// It is a PTY bridge, the way ttyd and GoTTY are: every websocket gets its own
// salotto-tui process on its own pseudo-terminal, and the browser runs a terminal
// emulator against it. The alternative - serialising the model's screen into JSON and
// rebuilding a UI from those fields in JavaScript - is a second renderer, and this
// front end has already paid for that twice: a nicklist drawn both by the model and by
// a sidebar, an input line printed in two places, and a framed render wrapped in an
// HTML frame.
//
// A PTY also settles, for free, the things that were hand-made before and each had its
// own bug:
//
//	window size   TIOCSWINSZ -> bubbletea's WindowSizeMsg. No resize protocol.
//	mouse         xterm emits the SGR mouse sequences; bubbletea decodes them.
//	keys          xterm emits what a terminal emits, for every key and modifier.
//	paste         bracketed paste, handled by bubbletea.
//	colour        256-colour SGR from lipgloss, rendered by the emulator.
//
// And because each session is its own process with its own bouncer client, two
// browsers are two clients of the same user rather than two views of one shared cursor.
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
	"os/exec"
	"os/signal"
	"path/filepath"
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
	bouncer := flag.String("bouncer", "", "bouncer address to attach to (host:port); empty serves a local-only UI")
	nick := flag.String("nick", "salotto", "nick to register with the bouncer")
	window := flag.String("window", "", "initial window (default: the nick's own window)")
	tuiPath := flag.String("tui", "", "path to the salotto-tui binary (default: beside this binary)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("salotto-web %s\n", version)
		return 0
	}

	bin, err := resolveTUI(*tuiPath)
	if err != nil {
		slog.Error("cannot find the terminal client", "err", err, "hint", "pass -tui")
		return 1
	}

	// A configured bouncer that cannot be reached is a front end that would accept a
	// websocket, start a child, and have that child die before drawing anything: the
	// browser shows an empty page and nobody learns why. Refusing to start hands it to
	// the supervisor instead, which is where a retry belongs. An empty -bouncer stays a
	// deliberate local-only UI.
	if *bouncer != "" {
		probe, err := net.DialTimeout("tcp", *bouncer, 5*time.Second)
		if err != nil {
			slog.Error("bouncer unreachable; exiting so the front end is retried",
				"bouncer", *bouncer, "err", err)
			return 1
		}
		_ = probe.Close()
	}

	// Each session builds its own child. The child owns the bouncer connection, so it
	// dies with the session and a closed tab closes its client - which is what any IRC
	// client does, and what a shared in-process Program could not do.
	cmdFor := func(cols, rows int) *exec.Cmd {
		// No size arguments: the PTY was created at the size the browser asked for, and
		// the child learns it from the terminal (TIOCGWINSZ) the same way it learns every
		// resize. Passing it on the command line as well would be a second source of truth
		// for the one thing the kernel already knows - and, as it happened, a flag the
		// child did not define, so every session died on the first frame.
		var args []string
		if *bouncer != "" {
			args = append(args, "-bouncer", *bouncer, "-nick", *nick)
		}
		if *window != "" {
			args = append(args, "-window", *window)
		}
		cmd := exec.Command(bin, args...)
		cmd.Env = ui.SessionEnv()
		// The child's stderr goes to the pod log: when a session dies there is nothing
		// in the browser to explain why, and a silent 1006 close is not a diagnosis.
		cmd.Stderr = os.Stderr
		return cmd
	}

	srv := &http.Server{
		Handler:           ui.TermHandler(ui.NewTermServer(cmdFor)),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "salotto-web: %v\n", err)
		return 1
	}

	slog.Info("web listening", "addr", ln.Addr().String(), "tui", bin,
		"bouncer", *bouncer, "nick", *nick)

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

// resolveTUI finds the terminal client. Shipping both binaries beside each other is the
// deployment's job; looking beside ourselves first means the web front end does not need
// to be told where its sibling is.
func resolveTUI(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", err
		}
		return explicit, nil
	}
	if self, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(self), "salotto-tui")
		if _, err := os.Stat(cand); err == nil {
			return cand, nil
		}
	}
	if p, err := exec.LookPath("salotto-tui"); err == nil {
		return p, nil
	}
	return "", errors.New("salotto-tui not found beside the web binary or in PATH")
}
