package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/iakat/salotto/internal/bindhost"
	"github.com/iakat/salotto/internal/config"
	"github.com/iakat/salotto/internal/facade"
	"github.com/iakat/salotto/internal/ident"
	"github.com/iakat/salotto/internal/logfile"
	"github.com/iakat/salotto/internal/upstream"
	"github.com/iakat/salotto/internal/winring"
)

// netmgr owns the lifecycle of every network: the dialling goroutine, the log
// writer, the per-window rings and the facade registration. It exists so a
// network can be created and destroyed while the daemon runs (B5) with exactly
// the construction the boot path used, instead of a second, drifting copy.
//
// Nothing here is persisted: a network added at runtime lives until the process
// exits or an admin removes it, because there is no admin surface yet. When that
// surface arrives (§2.0.2) it is the one that writes the config.
type netmgr struct {
	cfg       *config.Config
	srv       *facade.Server
	log       *slog.Logger
	reg       *ident.Registry
	bindState *bindhost.State
	parent    context.Context

	mu       sync.Mutex
	runtimes map[string]*networkRuntime
	conns    map[string]*upstream.Conn
}

// networkRuntime is one live network: what /stats reports and what Stop tears
// down.
type networkRuntime struct {
	name     string
	addr     string
	conn     *upstream.Conn
	set      *winring.Set
	writer   *logfile.Writer
	channels *channelSet

	cancel context.CancelFunc
	done   chan struct{}
}

func newNetmgr(cfg *config.Config, srv *facade.Server, log *slog.Logger, reg *ident.Registry, bindState *bindhost.State, parent context.Context) *netmgr {
	m := &netmgr{
		cfg:       cfg,
		srv:       srv,
		log:       log,
		reg:       reg,
		bindState: bindState,
		parent:    parent,
		runtimes:  make(map[string]*networkRuntime, len(cfg.Networks)),
		conns:     make(map[string]*upstream.Conn, len(cfg.Networks)),
	}
	// One callback for the life of the process: it looks the connection up at
	// call time, so a network added later gets bind-host changes too.
	bindState.SetOnChange(func(network, host string) {
		m.mu.Lock()
		conn := m.conns[network]
		m.mu.Unlock()
		if conn == nil {
			return
		}
		if err := conn.SetBindHost(host); err != nil {
			log.Error("invalid bind host from wire command", "network", network, "host", host, "err", err)
		}
	})
	return m
}

// Start builds and dials one network, then registers it with the facade. It is
// the only construction path: boot and runtime both go through it.
func (m *netmgr) Start(nc config.Network) error {
	if nc.Name == "" {
		return errors.New("network: empty name")
	}
	if nc.Addr == "" {
		return fmt.Errorf("network %q: empty addr", nc.Name)
	}

	m.mu.Lock()
	if _, dup := m.runtimes[nc.Name]; dup {
		m.mu.Unlock()
		return fmt.Errorf("network %q already running", nc.Name)
	}
	m.mu.Unlock()

	// Seed before resolving: the config value is the network's override, and it
	// must not be filtered by the operator's deny policy, which governs only the
	// wire commands.
	m.bindState.Seed(nc.Name, nc.BindHost)
	bindHost, _ := m.bindState.Resolve(nc.Name)

	// Per-network resolved ident, read at dial time. When it is empty the pair
	// is not registered at all: NO-USER is more honest than inventing a value.
	identStr := m.cfg.Ident
	if identStr == "" {
		identStr = nc.Nick
	}

	maxWindows := m.cfg.MaxWindows
	if maxWindows <= 0 {
		maxWindows = 64
	}
	set := winring.New(maxWindows, m.cfg.RingSegments, m.cfg.RingSegmentSize)
	channels := newChannelSet(nc.Channels)

	netName := nc.Name
	serverStr := nc.Addr
	w := logfile.New(netName, m.cfg.LogDir, m.cfg.LogQueue, time.Now)
	w.SetStripControls(m.cfg.StripControls)
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
		SASLExternal:       nc.SASLExternal,
		AltNick:            nc.AltNick,
		KeepNick:           nc.KeepNick,
		CertFile:           nc.CertFile,
		KeyFile:            nc.KeyFile,
		Channels:           append([]string(nil), nc.Channels...),
		BindHost:           bindHost,
		Windows:            set,
		Route:              w.Route,
		Log:                m.log.With("network", netName),
		// Read at every (re)connect: a channel the operator disabled must not be
		// re-joined, which is the only effect ZNC's disabled flag has. Buffering
		// and replay are unaffected.
		JoinChannels: func() []string {
			list := channels.List()
			out := joinableChannels(list, m.srv, netName)
			m.log.Info("DBG rejoin", "saved", list, "out", out, "nicks", m.srv.OwnNicks(netName))
			return out
		},
		// ZNC's perform module: the lines run on every connect, before the
		// joins, because a JOIN that races an IDENTIFY is the classic bug.
		Perform: func() []string { return append([]string(nil), nc.Perform...) },
		// Read at every handshake, so a pin added with
		// AddTrustedServerFingerprint applies to the next reconnect.
		TrustFingerprint: func(fp string) bool {
			return m.srv.TrustedServerFingerprints().Trusted(netName, fp)
		},
		OnLine: func(windows []string, seqs []uint64, entries []logfile.Entry, line []byte) {
			dispatchRouted(m.srv, netName, windows, line)
			w.OnRouted(entries, line, false)
			noteSelfKick(m.srv, netName, line)
			// chansaver: our own JOIN/PART/KICK decides what gets re-joined on
			// the next connect, which is what makes the list follow reality
			// instead of the config file.
			own := m.srv.OwnNicks(netName)
			if got := channels.noteSelfJoinPartAny(line, own); got {
				m.log.Info("DBG rec", "nicks", own, "line", string(line), "list", channels.List())
			} else if bytes.Contains(line, []byte(" JOIN ")) {
				m.log.Info("DBG skip", "nicks", own, "line", string(line), "list", channels.List())
			}
			// awaystore: count private messages that arrive while the network
			// says we are away, so the returning client is told how much it
			// missed instead of silently replaying a buffer.
			m.srv.NoteInbound(netName, line)
		},
		OnSend: func(line []byte) {
			entries := w.Route(line, true)
			dispatchEntriesFrom(m.srv, netName, entries, line, m.srv.OutboundSource())
			w.OnRouted(entries, line, true)
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
			return m.reg.Add(conn.LocalAddr(), conn.RemoteAddr(), identStr)
		},
	}
	if ucfg.RealName == "" {
		ucfg.RealName = nc.Nick
	}
	conn, err := upstream.New(ucfg)
	if err != nil {
		w.Close()
		return fmt.Errorf("network %q: %w", netName, err)
	}
	serverStr = conn.ServerString()
	w.SetServer(serverStr)
	w.SetHasCap(conn.HasCap)

	rt := &networkRuntime{
		name:     netName,
		addr:     nc.Addr,
		conn:     conn,
		set:      set,
		writer:   w,
		channels: channels,
		done:     make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(m.parent)
	rt.cancel = cancel

	m.mu.Lock()
	m.runtimes[netName] = rt
	m.conns[netName] = conn
	m.mu.Unlock()

	// Register before dialling so a line arriving on the first connection has a
	// network to be routed through.
	if err := m.srv.AddNetwork(netName, &networkAdapter{name: netName, conn: conn, set: set, channels: channels}); err != nil {
		m.mu.Lock()
		delete(m.runtimes, netName)
		delete(m.conns, netName)
		m.mu.Unlock()
		cancel()
		w.Close()
		return err
	}

	m.log.Info("upstream starting", "network", netName, "addr", nc.Addr)
	go func() {
		defer close(rt.done)
		conn.Run(ctx)
	}()
	return nil
}

// Stop tears a network down: it cancels the dial loop, waits for the goroutine
// to return, closes the log writer and only then unregisters it from the facade,
// so no line can be routed to a network whose writer is closed.
func (m *netmgr) Stop(name string) error {
	m.mu.Lock()
	rt := m.runtimes[name]
	if rt == nil {
		m.mu.Unlock()
		return fmt.Errorf("network %q is not running", name)
	}
	delete(m.runtimes, name)
	delete(m.conns, name)
	m.mu.Unlock()

	rt.cancel()
	<-rt.done
	if rt.writer != nil {
		rt.writer.Close()
	}
	if err := m.srv.RemoveNetwork(name); err != nil {
		return err
	}
	m.log.Info("network stopped", "network", name,
		"lines_in", rt.conn.LinesIn(), "lines_out", rt.conn.LinesOut(),
		"ring_bytes", rt.set.Bytes())
	return nil
}

// StopAll stops every running network. Shutdown uses it so the writers are
// closed in the same order Start opened them.
func (m *netmgr) StopAll() {
	for _, name := range m.Names() {
		if err := m.Stop(name); err != nil {
			m.log.Warn("network stop failed", "network", name, "err", err)
		}
	}
}

// Names returns the running network names in sorted order.
func (m *netmgr) Names() []string {
	m.mu.Lock()
	names := make([]string, 0, len(m.runtimes))
	for name := range m.runtimes {
		names = append(names, name)
	}
	m.mu.Unlock()
	sort.Strings(names)
	return names
}

// Runtime returns the runtime for a network, or nil.
func (m *netmgr) Runtime(name string) *networkRuntime {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runtimes[name]
}

// Snapshot returns the running networks sorted by name. /stats and shutdown
// iterate over this, never over the manager's map.
func (m *netmgr) Snapshot() []*networkRuntime {
	names := m.Names()
	out := make([]*networkRuntime, 0, len(names))
	for _, name := range names {
		if rt := m.Runtime(name); rt != nil {
			out = append(out, rt)
		}
	}
	return out
}
