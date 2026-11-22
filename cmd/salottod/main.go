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

	"github.com/iakat/salotto/internal/config"
	"github.com/iakat/salotto/internal/facade"
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
	name string
	addr string
	conn *upstream.Conn
	ring *ring.Ring
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

	start := time.Now()

	runtimes := make([]*networkRuntime, 0, len(cfg.Networks))
	networks := make(map[string]*facade.Network, len(cfg.Networks))
	var srv *facade.Server

	for i := range cfg.Networks {
		nc := cfg.Networks[i]
		netName := nc.Name
		r := ring.New(cfg.RingSegments, cfg.RingSegmentSize)

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
			Ring:               r,
			Log:                logger.With("network", netName),
			OnLine: func(seq uint64, line []byte) {
				if srv != nil {
					srv.Broadcast(netName, seq, line, time.Now().UnixNano())
				}
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

		runtimes = append(runtimes, &networkRuntime{
			name: netName,
			addr: nc.Addr,
			conn: conn,
			ring: r,
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
			Handler:           statsHandler(start, runtimes, hub, srv),
			ReadHeaderTimeout: 5 * time.Second,
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

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	sig := <-sigCh
	logger.Info("signal received", "signal", sig.String())

	cancel()
	logger.Info("shutting down")

	_ = plainLn.Close()
	if tlsRaw != nil {
		_ = tlsRaw.Close()
	}
	if statsSrv != nil {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = statsSrv.Shutdown(shutdownCtx)
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

func statsHandler(start time.Time, runtimes []*networkRuntime, hub *facade.Hub, srv *facade.Server) http.Handler {
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
		}
		fmt.Fprintf(&b, "clients_attached=%d\n", srv.Attached())
		fmt.Fprintf(&b, "clients_dropped_slow=%d\n", hub.Dropped())
		fmt.Fprintf(&b, "goroutines=%d\n", runtime.NumGoroutine())
		_, _ = io.WriteString(w, b.String())
	})
	return mux
}
