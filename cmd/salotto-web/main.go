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

	"github.com/iakat/salotto/internal/push"
	"github.com/iakat/salotto/internal/ui"
	"github.com/iakat/salotto/internal/upload"
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
	vapidPath := flag.String("vapid", "", "path to the VAPID key file; enables push notifications")
	pushSubject := flag.String("vapid-subject", "mailto:admin@localhost", "contact for the push service")
	subsPath := flag.String("push-subscriptions", "", "path to the subscription store")
	pushToken := flag.String("push-token", "", "bearer token /push/notify requires (default: $SALOTTO_PUSH_TOKEN)")
	uploadDir := flag.String("upload-dir", "", "directory for uploaded files; empty disables uploads")
	uploadMaxFile := flag.String("upload-max-file", "25MiB", "largest single upload")
	uploadMaxUser := flag.String("upload-max-user", "500MiB", "total bytes one user may store; 0 for unbounded")
	uploadTTL := flag.String("upload-ttl", "720h", "how long an upload lives; 0 for never")
	settingsPath := flag.String("settings", "", "path to the client settings file shared by every browser session (default: $SALOTTO_SETTINGS)")
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

	// Client settings live in one file per deployment, on this pod's own volume. Every
	// browser session reads and writes the same file because they are the same person's
	// front end: "/fset highlight salotto" typed in one tab has to be there in the next
	// one, and a per-session temporary file would forget it on every reconnect - which
	// is exactly the bug this fixes. It is not in the ConfigMap for the same reason the
	// push key is not: the file is state, not configuration.
	settings := *settingsPath
	if settings == "" {
		settings = os.Getenv("SALOTTO_SETTINGS")
	}
	if settings != "" {
		if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
			slog.Warn("cannot create the settings directory", "path", settings, "err", err)
		}
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
		if settings != "" {
			args = append(args, "-settings", settings)
		}
		cmd := exec.Command(bin, args...)
		cmd.Env = ui.SessionEnv()
		// The child's stderr goes to the pod log: when a session dies there is nothing
		// in the browser to explain why, and a silent 1006 close is not a diagnosis.
		cmd.Stderr = os.Stderr
		return cmd
	}

	// Push is opt-in: no key path means no key is generated, no subscription store is opened,
	// and the /push endpoints answer "not configured". Generating a key by default would put
	// a private key on disk for a deployment that never asked for notifications.
	var pushSrv *push.Server
	if *vapidPath != "" {
		vapid, err := push.LoadVAPID(*vapidPath, *pushSubject)
		if err != nil {
			slog.Error("cannot load the push key", "err", err)
			return 1
		}
		storePath := *subsPath
		if storePath == "" {
			storePath = filepath.Join(filepath.Dir(*vapidPath), "push-subscriptions.json")
		}
		store, err := push.OpenStore(storePath)
		if err != nil {
			slog.Error("cannot open the subscription store", "err", err)
			return 1
		}
		pushSrv = push.NewServer(vapid, store)
		// A token from the environment as well as the flag, because an environment variable in a
		// PodSpec can come from a Secret while a flag has to be written into the manifest. The
		// token is the only thing standing between a stranger and "push to this device", so the
		// path that keeps it out of git is the path worth having.
		if *pushToken == "" {
			*pushToken = os.Getenv("SALOTTO_PUSH_TOKEN")
		}
		pushSrv.Token = *pushToken
		if pushSrv.Token == "" {
			// Refusing to start beats starting a notify endpoint that accepts anyone. The
			// operator asked for push, so they have to say who may trigger it.
			slog.Error("push is configured but no /push/notify token: set -push-token or SALOTTO_PUSH_TOKEN")
			return 1
		}
		slog.Info("push enabled", "key", vapid.PublicKey(), "subscriptions", store.Len())
	}

	term := ui.NewTermServer(cmdFor)
	handler := ui.TermHandler(term)

	if *uploadDir != "" {
		limits, err := ui.ParseUploadLimits(*uploadMaxFile, *uploadMaxUser, *uploadTTL)
		if err != nil {
			slog.Error("upload limits", "err", err)
			return 1
		}
		uploads, err := upload.New(*uploadDir, limits)
		if err != nil {
			slog.Error("upload store", "err", err)
			return 1
		}
		ui.SweepUploads(uploads, time.Hour)
		// The identity is the nick this front end registers with the bouncer: that is the one
		// identity the deployment has, and putting it in one place is what lets OIDC replace
		// it here later without touching the handler.
		uploadHandler := ui.UploadHandler(uploads, func(*http.Request) string { return *nick })
		mux := http.NewServeMux()
		mux.Handle("/uploads/", http.StripPrefix("/uploads", uploadHandler))
		mux.Handle("/", handler)
		handler = mux
		slog.Info("uploads enabled", "dir", *uploadDir,
			"max_file", limits.MaxFile, "max_user", limits.MaxUser, "ttl", limits.TTL.String())
	}
	if pushSrv != nil {
		// One mux, so /push and the terminal are one origin. A service worker registered from
		// this page can only control this origin, which is exactly the scope it needs.
		mux := http.NewServeMux()
		// One prefix, because the push package owns every /push path and splitting them
		// across four registrations only adds places for a path to be forgotten.
		mux.Handle("/push/", pushSrv.Handler())
		mux.Handle("/", handler)
		handler = mux
	}

	srv := &http.Server{
		Handler:           handler,
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
