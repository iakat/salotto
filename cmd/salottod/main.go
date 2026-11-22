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
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/iakat/salotto/internal/bindhost"
	"github.com/iakat/salotto/internal/config"
	"github.com/iakat/salotto/internal/facade"
	"github.com/iakat/salotto/internal/ident"
	"github.com/iakat/salotto/internal/logfile"
	"github.com/iakat/salotto/internal/ring"
	"github.com/iakat/salotto/internal/upstream"
)

var version = "dev"

type networkAdapter struct {
	name string
	conn *upstream.Conn
	ring *ring.Ring
}

func (n *networkAdapter) Name() string { return n.name }

func (n *networkAdapter) Send(line []byte) error { return n.conn.Send(line) }

func (n *networkAdapter) State() string { return n.conn.State().String() }

func (n *networkAdapter) Replay(from uint64, emit func(seq uint64, line []byte) bool) uint64 {
	return n.ring.Replay(from, emit)
}

func (n *networkAdapter) Newest() uint64 { return n.ring.Newest() }
func (n *networkAdapter) Oldest() uint64 { return n.ring.Oldest() }
func (n *networkAdapter) Bytes() int     { return n.ring.Bytes() }

func (n *networkAdapter) Stamp(seq uint64) (int64, bool) { return n.ring.Stamp(seq) }

type networkRuntime struct {
	name   string
	addr   string
	conn   *upstream.Conn
	ring   *ring.Ring
	writer *logfile.Writer
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

	runtimes := make([]*networkRuntime, 0, len(cfg.Networks))
	networks := make(map[string]*facade.Network, len(cfg.Networks))
	var srv *facade.Server

	reg := ident.NewRegistry()

	bindState := bindhost.New(cfg.User.BindHost, cfg.User.DenySetBindHost)
	for i := range cfg.Networks {
		bindState.Seed(cfg.Networks[i].Name, cfg.Networks[i].BindHost)
	}
	conns := make(map[string]*upstream.Conn, len(cfg.Networks))
	bindState.SetOnChange(func(network, host string) {
		if conn := conns[network]; conn != nil {
			if err := conn.SetBindHost(host); err != nil {
				logger.Error("invalid bind host from wire command", "network", network, "host", host, "err", err)
			}
		}
	})

	for i := range cfg.Networks {
		nc := cfg.Networks[i]
		netName := nc.Name
		bindHost, _ := bindState.Resolve(netName)
		// Per-network resolved ident, read at dial time. When it is empty the
		// pair is not registered at all: NO-USER is more honest than inventing
		// a value.
		identStr := cfg.Ident
		if identStr == "" {
			identStr = nc.Nick
		}
		r := ring.New(cfg.RingSegments, cfg.RingSegmentSize)

		var serverStr string
		w := logfile.New(netName, cfg.LogDir, cfg.LogQueue, time.Now)
		w.SetNick(nc.Nick)
		w.SetServer(nc.Addr)

		ucfg := upstream.Config{
			Name:               netName,
			Addr:               nc.Addr,
			TLS:                nc.TLS,
			InsecureSkipVerify: nc.InsecureSkipVerify,
			Nick:               nc.Nick,
			RealName:           nc.RealName,
			Pass:               nc.Pass,
			SASLUser:           nc.SASLUser,
			SASLPass:           nc.SASLPass,
			Channels:           append([]string(nil), nc.Channels...),
			BindHost:           bindHost,
			Ring:               r,
			Log:                logger.With("network", netName),
			OnLine: func(seq uint64, line []byte) {
				if srv != nil {
					srv.Broadcast(netName, seq, line, time.Now().UnixNano())
				}
				w.OnLine(seq, line)
			},
			OnSend: func(line []byte) {
				w.OnSend(line)
			},
			OnConnect: func() {
				w.Connect(serverStr)
			},
			OnDisconnect: func() {
				w.Disconnect(serverStr)
			},
			OnOpen: func(conn net.Conn) func() {
				if identStr == "" {
					return nil
				}
				return reg.Add(conn.LocalAddr(), conn.RemoteAddr(), identStr)
			},
		}
		if ucfg.RealName == "" {
			ucfg.RealName = nc.Nick
		}
		conn, err := upstream.New(ucfg)
		if err != nil {
			logger.Error("upstream config invalid", "network", netName, "err", err)
			return 1
		}
		serverStr = conn.ServerString()
		w.SetServer(serverStr)
		w.SetHasCap(conn.HasCap)
		conns[netName] = conn

		runtimes = append(runtimes, &networkRuntime{
			name:   netName,
			addr:   nc.Addr,
			conn:   conn,
			ring:   r,
			writer: w,
		})
		var n facade.Network = &networkAdapter{name: netName, conn: conn, ring: r}
		networks[netName] = &n
	}

	maxClients := cfg.MaxClients
	if maxClients <= 0 {
		maxClients = 64
	}
	hub := facade.NewHub(maxClients)
	srv, err = facade.New(facade.Config{
		ServerName: cfg.ServerName,
		Hub:        hub,
		Networks:   networks,
		BindHosts:  bindState,
		Log:        logger,
		MaxClients: maxClients,
	})
	if err != nil {
		logger.Error("facade config invalid", "err", err)
		return 1
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
			Handler:           statsHandler(start, runtimes, hub, srv, reg, cfg.IdentListen != ""),
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup

	for _, rt := range runtimes {
		wg.Add(1)
		go func(rt *networkRuntime) {
			defer wg.Done()
			logger.Info("upstream starting", "network", rt.name, "addr", rt.addr)
			rt.conn.Run(ctx)
		}(rt)
	}

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

	cancel()
	logger.Info("shutting down")
	for _, rt := range runtimes {
		if rt.writer != nil {
			rt.writer.Close()
		}
	}

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

func statsHandler(start time.Time, runtimes []*networkRuntime, hub *facade.Hub, srv *facade.Server, reg *ident.Registry, identEnabled bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")

		sorted := append([]*networkRuntime(nil), runtimes...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].name < sorted[j].name })

		var b strings.Builder
		fmt.Fprintf(&b, "uptime_seconds=%d\n", int64(time.Since(start).Seconds()))
		for _, rt := range sorted {
			fmt.Fprintf(&b, "net.%s.state=%s\n", rt.name, rt.conn.State())
			fmt.Fprintf(&b, "net.%s.attempts=%d\n", rt.name, rt.conn.Attempts())
			fmt.Fprintf(&b, "net.%s.lines_in=%d\n", rt.name, rt.conn.LinesIn())
			fmt.Fprintf(&b, "net.%s.lines_out=%d\n", rt.name, rt.conn.LinesOut())
			fmt.Fprintf(&b, "net.%s.ring_oldest=%d\n", rt.name, rt.ring.Oldest())
			fmt.Fprintf(&b, "net.%s.ring_newest=%d\n", rt.name, rt.ring.Newest())
			fmt.Fprintf(&b, "net.%s.ring_bytes=%d\n", rt.name, rt.ring.Bytes())
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
