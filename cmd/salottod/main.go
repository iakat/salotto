package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/iakat/salotto/internal/bindhost"
	"github.com/iakat/salotto/internal/config"
	"github.com/iakat/salotto/internal/facade"
	"github.com/iakat/salotto/internal/ident"
	"github.com/iakat/salotto/internal/ircline"
	"github.com/iakat/salotto/internal/logfile"
	"github.com/iakat/salotto/internal/upstream"
	"github.com/iakat/salotto/internal/winring"
)

var version = "dev"

type networkAdapter struct {
	channels *channelSet
	name     string
	conn     *upstream.Conn
	set      *winring.Set
}

func (n *networkAdapter) Name() string { return n.name }

func (n *networkAdapter) Send(line []byte) error { return n.conn.Send(line) }

func (n *networkAdapter) State() string { return n.conn.State().String() }

func (n *networkAdapter) Window(name string) (facade.WindowRing, bool) {
	r, ok := n.set.Ring(name)
	if !ok {
		return nil, false
	}
	return r, true
}

func (n *networkAdapter) Windows() []string { return n.set.Windows() }

func (n *networkAdapter) Bytes() int { return n.set.Bytes() }

func (n *networkAdapter) Server() string { return n.conn.Server() }

// Nick is what the facade uses to attribute a line to ourselves. Without it
// KnownNick is always empty, so every consumer of "is this our own line?" — the
// chansaver, which must follow our own JOIN and PART, and the self-kick that
// disables a channel — silently never fires. The adapter grew the method when
// upstream.Conn learned it; this is the wiring that makes it reachable.
func (n *networkAdapter) Nick() string { return n.conn.CurrentNick() }

func (n *networkAdapter) NickMask() string { return n.conn.NickMask() }

func (n *networkAdapter) HasPassword() bool { return n.conn.HasPassword() }

func (n *networkAdapter) Traffic() (in, out uint64) { return n.conn.Traffic() }

func (n *networkAdapter) SetConnectEnabled(enabled bool) { n.conn.SetConnectEnabled(enabled) }

func (n *networkAdapter) Servers() []string { return n.conn.Servers() }

func (n *networkAdapter) AddServer(addr string) bool { return n.conn.AddServer(addr) }

func (n *networkAdapter) DelServer(addr string) bool { return n.conn.DelServer(addr) }

// MoveChan moves a channel in the saved order this network re-joins from.
func (n *networkAdapter) MoveChan(channel string, index int) error {
	if n.channels == nil {
		return errors.New("Invalid index")
	}
	return n.channels.MoveChan(channel, index)
}

func (n *networkAdapter) ResizeWindow(window string, lines int) bool {
	return n.set.Resize(window, lines)
}

func main() {
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", "salotto.toml", "path to the configuration file")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn or error")
	showVersion := flag.Bool("version", false, "print the version and exit")
	check := flag.Bool("check", false, "validate the configuration and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("salottod %s\n", version)
		return 0
	}

	level, err := parseLogLevel(*logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("config load failed", "path", *configPath, "err", err)
		return 1
	}
	logger.Info("config loaded", "path", *configPath, "networks", len(cfg.Networks))

	if *check {
		logger.Info("config ok", "path", *configPath)
		return 0
	}

	if err := validateStatsAddr(cfg.StatsListen); err != nil {
		logger.Error("invalid stats_listen", "addr", cfg.StatsListen, "err", err)
		return 1
	}
	if err := validateStatsAddr(cfg.LogListen); err != nil {
		// log_listen may be non-loopback; only validate host:port syntax here
		host, _, err2 := net.SplitHostPort(cfg.LogListen)
		if cfg.LogListen != "" && (err2 != nil || host == "") {
			logger.Error("invalid log_listen", "addr", cfg.LogListen, "err", err2)
			return 1
		}
	}
	if cfg.LogListen != "" && !isLoopbackAddr(cfg.LogListen) && cfg.LogToken == "" {
		logger.Error("log_listen is non-loopback but log_token is empty")
		return 1
	}

	start := time.Now()

	reg := ident.NewRegistry()

	bindState := bindhost.New(cfg.User.BindHost, cfg.User.DenySetBindHost)

	maxClients := cfg.MaxClients
	if maxClients <= 0 {
		maxClients = 64
	}
	hub := facade.NewHub(maxClients)
	awayEnabled, awayReason, awayWaitSeconds := cfg.AwayEnabled()
	srv, err := facade.New(facade.Config{
		ServerName:  cfg.ServerName,
		Hub:         hub,
		BindHosts:   bindState,
		Log:         logger,
		MaxClients:  maxClients,
		MOTDBlocked: cfg.MOTDBlocked,
		MOTDMissing: cfg.MOTDMissing,
		// clearbufferonmsg is a per-network knob in ZNC; here it is process-wide
		// because the daemon is single-user. Any network asking for it turns it
		// on.
		ClearBuffersOnMsg: cfg.ClearBufferOnMsg(),
		// simple_away is also a per-network module; the away state is the
		// user's, so any network asking for it turns it on for the daemon.
		AwayEnabled:    awayEnabled,
		AwayReason:     awayReason,
		AwayWait:       time.Duration(awayWaitSeconds) * time.Second,
		MaxBufferLines: cfg.MaxBufferLines,
	})
	if err != nil {
		logger.Error("facade config invalid", "err", err)
		return 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := newNetmgr(cfg, srv, logger, reg, bindState, ctx)
	for i := range cfg.Networks {
		if err := mgr.Start(cfg.Networks[i]); err != nil {
			logger.Error("network start failed", "network", cfg.Networks[i].Name, "err", err)
			mgr.StopAll()
			return 1
		}
	}
	plainLn, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		logger.Error("listen failed", "addr", cfg.Listen, "err", err)
		return 1
	}

	var tlsLn net.Listener
	var tlsRaw net.Listener
	if cfg.TLSListen != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			_ = plainLn.Close()
			logger.Error("tls certificate load failed", "cert", cfg.TLSCert, "key", cfg.TLSKey, "err", err)
			return 1
		}
		tlsRaw, err = net.Listen("tcp", cfg.TLSListen)
		if err != nil {
			_ = plainLn.Close()
			logger.Error("listen failed", "addr", cfg.TLSListen, "tls", true, "err", err)
			return 1
		}
		tlsLn = tls.NewListener(tlsRaw, &tls.Config{Certificates: []tls.Certificate{cert}})
	}

	var statsLn net.Listener
	var statsSrv *http.Server
	if cfg.StatsListen != "" {
		statsLn, err = net.Listen("tcp", cfg.StatsListen)
		if err != nil {
			_ = plainLn.Close()
			if tlsRaw != nil {
				_ = tlsRaw.Close()
			}
			logger.Error("stats listen failed", "addr", cfg.StatsListen, "err", err)
			return 1
		}
		statsSrv = &http.Server{
			Handler:           statsHandler(start, mgr, hub, srv, reg, cfg.IdentListen != ""),
			ReadHeaderTimeout: 5 * time.Second,
		}
	}
	var logLn net.Listener
	var logSrv *http.Server
	if cfg.LogListen != "" {
		logLn, err = net.Listen("tcp", cfg.LogListen)
		if err != nil {
			_ = plainLn.Close()
			if tlsRaw != nil {
				_ = tlsRaw.Close()
			}
			logger.Error("log listen failed", "addr", cfg.LogListen, "err", err)
			return 1
		}
		logSrv = &http.Server{
			Handler:           logfile.SearchHandler(cfg.LogDir, cfg.LogToken),
			ReadHeaderTimeout: 5 * time.Second,
		}
	}

	var identLn net.Listener
	var identSrv *ident.Server
	if cfg.IdentListen != "" {
		identLn, err = net.Listen("tcp", cfg.IdentListen)
		if err != nil {
			logger.Warn("ident listen failed; continuing without ident", "addr", cfg.IdentListen, "err", err)
			identLn = nil
		} else {
			identSrv, err = ident.New(ident.Config{
				Listener: identLn,
				Registry: reg,
				Log:      logger,
			})
			if err != nil {
				logger.Warn("ident setup failed; continuing without ident", "err", err)
				_ = identLn.Close()
				identLn, identSrv = nil, nil
			}
		}
	}

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := srv.Serve(plainLn); err != nil && !errors.Is(err, net.ErrClosed) {
			logger.Error("facade serve failed", "addr", plainLn.Addr().String(), "err", err)
		}
	}()
	logger.Info("listening", "addr", plainLn.Addr().String(), "tls", false)

	if tlsLn != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.Serve(tlsLn); err != nil && !errors.Is(err, net.ErrClosed) {
				logger.Error("facade serve failed", "addr", tlsLn.Addr().String(), "tls", true, "err", err)
			}
		}()
		logger.Info("listening", "addr", tlsLn.Addr().String(), "tls", true)
	}

	if identSrv != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := identSrv.Serve(ctx); err != nil {
				logger.Error("ident serve failed", "addr", identLn.Addr().String(), "err", err)
			}
		}()
		logger.Info("ident listening", "addr", identLn.Addr().String())
	}

	if statsSrv != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := statsSrv.Serve(statsLn); err != nil &&
				!errors.Is(err, http.ErrServerClosed) &&
				!errors.Is(err, net.ErrClosed) {
				logger.Error("stats serve failed", "addr", statsLn.Addr().String(), "err", err)
			}
		}()
		logger.Info("stats listening", "addr", statsLn.Addr().String())
	}
	if logSrv != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := logSrv.Serve(logLn); err != nil &&
				!errors.Is(err, http.ErrServerClosed) &&
				!errors.Is(err, net.ErrClosed) {
				logger.Error("log serve failed", "addr", logLn.Addr().String(), "err", err)
			}
		}()
		logger.Info("log listening", "addr", logLn.Addr().String())
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	sig := <-sigCh
	logger.Info("signal received", "signal", sig.String())

	logger.Info("shutting down")
	// Stop cancels each dial loop, waits for it, closes that network's writer and
	// only then unregisters it, so no line can reach a closed writer.
	mgr.StopAll()
	cancel()

	_ = plainLn.Close()
	if tlsRaw != nil {
		_ = tlsRaw.Close()
	}
	if identLn != nil {
		_ = identLn.Close()
	}
	if statsSrv != nil {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = statsSrv.Shutdown(shutdownCtx)
		shutdownCancel()
	}
	if logSrv != nil {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = logSrv.Shutdown(shutdownCtx)
		shutdownCancel()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		logger.Info("shutdown complete")
	case <-time.After(5 * time.Second):
		logger.Warn("shutdown timed out", "wait", "5s")
	}
	return 0
}

// dispatchRouted delivers one line live to the windows routing already chose.
// A line whose only window is status is the status stream; a line with a
// channel or query window goes through BroadcastWindows. The caller owns
// windows and must not keep using it after the call.
func dispatchRouted(srv *facade.Server, netName string, windows []string, line []byte) {
	dispatchRoutedFrom(srv, netName, windows, line, nil)
}

// dispatchRoutedFrom is dispatchRouted for a line a client sent. source is that
// client, and it decides who gets the echo: ZNC sends the author's own line back
// only when the author negotiated echo-message (Client.cpp:1138), so a client
// that draws its own messages locally is not shown them twice.
func dispatchRoutedFrom(srv *facade.Server, netName string, windows []string, line []byte, source *facade.Subscriber) {
	if srv == nil {
		return
	}
	nonStatus := false
	for _, window := range windows {
		if window != "status" && window != "" {
			nonStatus = true
			break
		}
	}
	if !nonStatus {
		srv.Broadcast(netName, 0, line, time.Now().UnixNano())
		return
	}
	srv.BroadcastWindowsFrom(netName, windows, 0, line, time.Now().UnixNano(), source)
}

// dispatchEntries is dispatchRouted for a caller that has the parsed entries
// but not the window list. It runs on the outbound path, where the allocation
// does not matter.
func dispatchEntries(srv *facade.Server, netName string, entries []logfile.Entry, line []byte) {
	dispatchEntriesFrom(srv, netName, entries, line, nil)
}

// dispatchEntriesFrom is dispatchEntries for a line a client sent: source is
// that client, and only it is subject to the echo-message gate.
func dispatchEntriesFrom(srv *facade.Server, netName string, entries []logfile.Entry, line []byte, source *facade.Subscriber) {
	windows := make([]string, len(entries))
	for i, e := range entries {
		windows[i] = e.Window
	}
	dispatchRoutedFrom(srv, netName, windows, line, source)
}

// joinableChannels returns the channels a network should join on the next
// connect: the configured list minus the windows the operator disabled. ZNC
// suppresses the rejoin of a disabled channel and nothing else
// (IRCNetwork.cpp:998), so this is the whole of the flag's effect on dialling.
func joinableChannels(configured []string, srv *facade.Server, network string) []string {
	if srv == nil {
		return append([]string(nil), configured...)
	}
	out := make([]string, 0, len(configured))
	for _, ch := range configured {
		if srv.Disabled(network, ch) {
			continue
		}
		out = append(out, ch)
	}
	return out
}

// noteSelfKick mirrors ZNC's IRCSock.cpp:734: being kicked from a channel
// disables that channel, so the bouncer does not immediately re-join a channel
// the operator was removed from on purpose. The kick itself is still routed and
// buffered; only the rejoin is suppressed.
func noteSelfKick(srv *facade.Server, network string, line []byte) {
	if srv == nil {
		return
	}
	own := srv.KnownNick(network)
	if own == "" {
		return
	}
	_, rest := ircline.Tags(line)
	prefix, rest := ircline.Prefix(rest)
	if !ircline.IsVerb(rest, "KICK") {
		return
	}
	params := ircline.Params(rest)
	fields := strings.Fields(string(params))
	if len(fields) < 2 {
		return
	}
	channel, target := fields[0], fields[1]
	if !strings.EqualFold(target, own) {
		return
	}
	_ = prefix
	srv.SetDisabled(network, channel, true)
}

func parseLogLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid log level %q (want debug, info, warn or error)", value)
	}
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateStatsAddr(addr string) error {
	if addr == "" {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" {
		return errors.New("stats_listen must name a loopback host")
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("stats_listen host %q is not an IP address", host)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("stats_listen host %q is not loopback", host)
	}
	return nil
}

func statsHandler(start time.Time, mgr *netmgr, hub *facade.Hub, srv *facade.Server, reg *ident.Registry, identEnabled bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")

		// Read from the manager, not a startup slice: /stats must reflect a
		// network added or removed at runtime.
		sorted := mgr.Snapshot()

		var b strings.Builder
		fmt.Fprintf(&b, "uptime_seconds=%d\n", int64(time.Since(start).Seconds()))
		for _, rt := range sorted {
			fmt.Fprintf(&b, "net.%s.state=%s\n", rt.name, rt.conn.State())
			fmt.Fprintf(&b, "net.%s.attempts=%d\n", rt.name, rt.conn.Attempts())
			fmt.Fprintf(&b, "net.%s.lines_in=%d\n", rt.name, rt.conn.LinesIn())
			fmt.Fprintf(&b, "net.%s.lines_out=%d\n", rt.name, rt.conn.LinesOut())
			fmt.Fprintf(&b, "net.%s.ring_bytes=%d\n", rt.name, rt.set.Bytes())
			known, disabled := srv.WindowStats(rt.name)
			fmt.Fprintf(&b, "net.%s.windows_known=%d\n", rt.name, known)
			fmt.Fprintf(&b, "net.%s.windows_disabled=%d\n", rt.name, disabled)
		}
		if identEnabled {
			fmt.Fprintf(&b, "ident.enabled=1\n")
			fmt.Fprintf(&b, "ident.queries=%d\n", reg.Queries.Load())
			fmt.Fprintf(&b, "ident.refused=%d\n", reg.Refused.Load())
		}
		fmt.Fprintf(&b, "clients_attached=%d\n", srv.Attached())
		fmt.Fprintf(&b, "clients_dropped_slow=%d\n", hub.Dropped())
		fmt.Fprintf(&b, "goroutines=%d\n", runtime.NumGoroutine())
		_, _ = io.WriteString(w, b.String())
	})
	return mux
}
