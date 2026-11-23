package facade

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iakat/salotto/internal/ircline"
)

// Config describes the client-facing IRC server.
type Config struct {
	Addr       string // listen address, e.g. "127.0.0.1:6667"
	TLS        bool
	CertFile   string
	KeyFile    string
	ServerName string // shown in numerics, e.g. "salotto"
	Network    string // 005 NETWORK=
	Hub        *Hub
	Networks   map[string]*Network // key: network name as the client will see it
	Log        *slog.Logger
	MaxClients int
	OutBuf     int // per-client outbound buffer, default 64 KiB
	MaxLineLen int // default 8192; longer client lines are truncated
	// BindHosts, when set, answers the SetBindHost/ClearBindHost family. It is
	// optional so the facade stays usable without an upstream dialer.
	BindHosts BindHosts
	// MOTDBlocked suppresses 375/372 and replaces 376's text, as ZNC's
	// block_motd does; the client still gets a complete 375/376 pair.
	MOTDBlocked bool
	// MOTDMissing answers login with 422 instead of a MOTD, as missingmotd does.
	MOTDMissing bool
	// ClearBuffersOnMsg clears every channel and query buffer when the user
	// speaks, which is ZNC's clearbufferonmsg module. The module's default rule
	// set is every message type except QUIT, so a QUIT does not clear.
	ClearBuffersOnMsg bool
	// AwayEnabled turns on ZNC's simple_away behaviour: mark the user away on
	// IRC after AwayWait with no client attached, and come back on attach.
	AwayEnabled bool
	// AwayReason is the away message; %awaytime% expands to when it was set.
	AwayReason string
	// AwayWait is how long to wait after the last client detaches before
	// setting away. Zero disables the automatic part, as ZNC's SetTimer 0 does.
	AwayWait time.Duration
	// MaxBufferLines is the largest buffer SETBUFFER will accept, which is
	// ZNC's GetMaxBufferSize. Zero means the default.
	MaxBufferLines int
	// UserName is the name TRAFFIC prints for the single local user. ZNC's
	// TRAFFIC is admin-only and takes the name from the user map; this daemon
	// has one user, so the name is configuration.
	UserName string
}

// BindHosts is the bind-host state the wire commands mutate. Resolution is
// network value, else user value, else empty (kernel picks).
type BindHosts interface {
	Resolve(network string) (host, origin string)
	SetNetwork(network, host string) error
	ClearNetwork(network string) error
	SetUser(host string) error
	ClearUser() error
}

// WindowRing is one window's replay ring. Sequences are the window's own; two
// windows sharing a line have independent sequences and cursors.
type WindowRing interface {
	Replay(from uint64, emit func(seq uint64, line []byte) bool) uint64
	Newest() uint64
	Oldest() uint64
	Stamp(seq uint64) (int64, bool)
	// Clear physically drops the window's buffered lines. The window's
	// sequences stay monotonic, so a stale cursor remains a well-formed gap.
	Clear()
}

// Network is a handle to one upstream. The facade deliberately does not depend
// on internal/upstream.
type Network interface {
	Name() string
	Send(line []byte) error
	State() string
	// Window returns the replay ring for one window, if the window has ever
	// buffered a line. The name is the log window name ("status", "#chan", a
	// query nick).
	Window(name string) (WindowRing, bool)
	// Windows returns the window names with buffered lines, sorted.
	Windows() []string
	// Bytes is the total buffered bytes over every window. It is for /stats.
	Bytes() int
	// Server is the current upstream server string in ZNC's form: "host port",
	// or "host +port" when the connection is over TLS. LISTNETWORKS and
	// LISTSERVERS print it.
	Server() string
	// NickMask is "nick!ident@host" as the network knows itself. ZNC prints
	// this in LISTNETWORKS; when the ident or host is not known yet only the
	// nick is present.
	NickMask() string
	// HasPassword reports whether the configured upstream sends a PASS. The
	// password itself is never exposed: LISTSERVERS prints ****** for it.
	HasPassword() bool
	// Traffic is bytes received and sent on the upstream socket.
	Traffic() (in, out uint64)
	// SetConnectEnabled turns the upstream on or off, as CONNECT and
	// DISCONNECT do (ZNC's m_bIRCConnectEnabled).
	SetConnectEnabled(enabled bool)
	// Servers is the ordered server list ADDSERVER and DELSERVER mutate, and
	// LISTSERVERS renders. A network with one configured address returns one
	// entry, which is the pre-existing behaviour.
	Servers() []string
	// AddServer appends a server and reports whether it was added (false when it
	// was already there, which is ZNC's "Perhaps the server is already added").
	AddServer(addr string) bool
	// DelServer removes a server and reports whether it was there.
	DelServer(addr string) bool
	// MoveChan moves a channel to a 0-based index in the saved channel order and
	// reports an error string ZNC would print. It is nil-safe: a network that
	// keeps no order returns an error.
	MoveChan(channel string, index int) error
}

type networkEntry struct {
	name string
	net  Network
	nick *nickCache
}

type nickCache struct {
	mu   sync.RWMutex
	nick string
}

func (n *nickCache) get() string {
	n.mu.RLock()
	v := n.nick
	n.mu.RUnlock()
	return v
}

func (n *nickCache) set(v string) {
	n.mu.Lock()
	n.nick = v
	n.mu.Unlock()
}

// OwnNicks is every nick the network may be holding upstream, current first. It is
// what "is this line ours?" must consult: the live nick alone misses a line that
// arrived while the nick was moving.
func (s *Server) OwnNicks(network string) []string {
	ne := s.network(network)
	if ne == nil || ne.net == nil {
		return nil
	}
	if src, ok := ne.net.(upstreamNickCandidates); ok {
		return src.NickCandidates()
	}
	if n := ne.knownNick(); n != "" {
		return []string{n}
	}
	return nil
}

// knownNick is the nick we hold upstream, as opposed to the nick the client chose:
// a bouncer client routinely asks for a different one, and a client nick is not an
// identity on the IRC side.
//
// The live network is asked first and the cache is only a fallback. The other order
// was a real bug: the cache got seeded from the target of the first numeric on the
// wire, the facade also synthesises numerics addressed to the *client's* nick, and
// so the daemon came to believe it was "e2e" while upstream it was "e2e_". Every
// consumer of "is this line ours?" — chansaver's JOIN/PART tracking, the self-kick
// that disables a channel — then answered no, silently and forever.
func (ne *networkEntry) knownNick() string {
	if ne.net != nil {
		if src, ok := ne.net.(upstreamNickSource); ok {
			if v := src.Nick(); v != "" {
				if ne.nick != nil {
					ne.nick.set(v)
				}
				return v
			}
		}
	}
	if ne.nick == nil {
		return ""
	}
	return ne.nick.get()
}

// advanceNick returns the upstream nick to use for this line and the nick to
// use for the following line. An upstream NICK change is dropped from the
// client stream but still advances the cached identity.
func (ne *networkEntry) advanceNick(un string, line []byte) (forLine, next string) {
	if ne.nick == nil {
		// A zero networkEntry has no cache; deriving from the wire still works and
		// is what the fallback below does, so give it one.
		ne.nick = &nickCache{}
	}
	if un == "" {
		// Fall back to asking the live network before guessing from the wire.
		// detectUpstreamNick reads the target of the first numeric, which is the
		// upstream nick only while the client has not chosen a different nick —
		// and the client's nick is exactly the one a bouncer client tends to pick.
		// Caching the guess here is how the facade came to believe its own nick
		// was "e2e" while upstream it was "e2e_", which made every "is this line
		// ours?" test answer no.
		if v := ne.knownNick(); v != "" {
			return v, v
		}
		if v := detectUpstreamNick(line); v != "" {
			ne.nick.set(v)
			return v, v
		}
		return "", ""
	}
	if v, ok := upstreamNickChange(line, un); ok {
		ne.nick.set(v)
		return un, v
	}
	return un, un
}

// Server is the client-facing IRC facade.
type Server struct {
	cfg          Config
	log          *slog.Logger
	hub          *Hub
	netsMu       sync.RWMutex
	nets         []*networkEntry
	windows      *windowState
	fingerprints *Fingerprints
	created      string

	// outSource is the client whose line is being sent right now, read by the
	// daemon's OnSend hook. outMu makes the attribution exact; see forward.
	outMu     sync.Mutex
	outSource atomic.Pointer[Subscriber]
	active    atomic.Int64

	// away is ZNC's simple_away: away on detach, back on attach.
	away *away

	linePool sync.Pool
}

const (
	defaultOutBuf     = 64 * 1024
	defaultMaxLineLen = 8192
	maxRingLine       = 65535
)

var capNames = [...]string{
	"server-time",
	"message-tags",
	"echo-message",
	"away-notify",
	"account-notify",
	"extended-join",
	"cap-notify",
	"batch",
	"chathistory",
	"labeled-response",
}

const supportedCaps = "server-time message-tags echo-message away-notify account-notify extended-join cap-notify batch chathistory labeled-response"

var tagTime = []byte("time")

// New validates cfg, applies defaults and returns a server. The listener is
// supplied to Serve.
func New(cfg Config) (*Server, error) {
	if cfg.ServerName == "" {
		cfg.ServerName = "salotto"
	}
	if cfg.OutBuf <= 0 {
		cfg.OutBuf = defaultOutBuf
	}
	if cfg.MaxLineLen <= 0 {
		cfg.MaxLineLen = defaultMaxLineLen
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.MaxBufferLines <= 0 {
		cfg.MaxBufferLines = 10000
	}
	if cfg.AwayReason == "" {
		cfg.AwayReason = "Auto away at %awaytime%"
	}
	if cfg.AwayWait <= 0 && cfg.AwayEnabled {
		cfg.AwayWait = 60 * time.Second
	}

	s := &Server{
		cfg:          cfg,
		log:          cfg.Log,
		windows:      newWindowState(),
		fingerprints: NewFingerprints(),
		created:      time.Now().UTC().Format("Mon Jan 02 2006 15:04:05 MST"),
	}
	// One away policy for the process: every network is the same user, so an
	// AWAY fans out over all of them exactly as simple_away's PutIRC does per
	// network (the module is loaded per network but the state is the user's).
	s.away = newAway(cfg.AwayEnabled, cfg.AwayReason, cfg.AwayWait, s.broadcastAway)

	names := make([]string, 0, len(cfg.Networks))
	for name, net := range cfg.Networks {
		if net == nil {
			return nil, errors.New("facade: nil network " + name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	s.nets = make([]*networkEntry, 0, len(names))
	for _, name := range names {
		s.nets = append(s.nets, &networkEntry{name: name, net: *cfg.Networks[name], nick: &nickCache{}})
	}

	if s.cfg.Hub == nil {
		max := s.cfg.MaxClients
		if max <= 0 {
			max = 64
		}
		s.cfg.Hub = NewHub(max)
	}
	s.hub = s.cfg.Hub

	maxLine := s.cfg.MaxLineLen + 512
	s.linePool.New = func() any {
		b := make([]byte, 0, maxLine)
		return &b
	}
	return s, nil
}

// Serve accepts clients until the listener is closed.
func (s *Server) Serve(l net.Listener) error {
	if s.cfg.TLS {
		cert, err := tls.LoadX509KeyPair(s.cfg.CertFile, s.cfg.KeyFile)
		if err != nil {
			return err
		}
		l = tls.NewListener(l, &tls.Config{Certificates: []tls.Certificate{cert}})
	}

	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}

		if s.cfg.MaxClients > 0 {
			if int(s.active.Add(1)) > s.cfg.MaxClients {
				s.active.Add(-1)
				s.refuse(conn)
				continue
			}
		} else {
			s.active.Add(1)
		}
		go s.handleConn(conn)
	}
}

func (s *Server) refuse(conn net.Conn) {
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = io.WriteString(conn, "ERROR :too many clients\r\n")
	_ = conn.Close()
}

// Broadcast fans a live upstream line out to every attached client with a time
// tag, rewriting the upstream identity to each client's own nick. seq is
// currently informational; clients rebuild state from the stream.
func (s *Server) Broadcast(netName string, seq uint64, line []byte, ts int64) {
	_ = seq

	var un string
	if ne := s.network(netName); ne != nil {
		forLine, _ := ne.advanceNick(ne.knownNick(), line)
		un = forLine
	}

	p := s.linePool.Get().(*[]byte)
	b := appendLiveLine((*p)[:0], line, ts)
	s.hub.BroadcastFrom(b, un)
	*p = b
	s.linePool.Put(p)
}

func (s *Server) network(name string) *networkEntry {
	s.netsMu.RLock()
	defer s.netsMu.RUnlock()
	for _, ne := range s.nets {
		if ne.name == name {
			return ne
		}
	}
	return nil
}

// Networks returns the network names in sorted order.
func (s *Server) Networks() []string {
	s.netsMu.RLock()
	names := make([]string, len(s.nets))
	for i, ne := range s.nets {
		names[i] = ne.name
	}
	s.netsMu.RUnlock()
	return names
}

// AddNetwork registers a network with an already-dialling upstream. Runtime
// networks exist so the admin surface (§2.0.2) can create one without a
// restart; there is no IRC command for this because ZNC has none either.
// The caller owns the connection lifecycle: the facade only routes.
func (s *Server) AddNetwork(name string, net Network) error {
	if name == "" {
		return errors.New("facade: empty network name")
	}
	if net == nil {
		return errors.New("facade: nil network " + name)
	}
	s.netsMu.Lock()
	defer s.netsMu.Unlock()
	for _, ne := range s.nets {
		if ne.name == name {
			return fmt.Errorf("facade: network %q already exists", name)
		}
	}
	s.nets = append(s.nets, &networkEntry{name: name, net: net, nick: &nickCache{}})
	sort.Slice(s.nets, func(i, j int) bool { return s.nets[i].name < s.nets[j].name })
	return nil
}

// RemoveNetwork forgets a network and every piece of per-client state keyed
// under it, so a name reused later cannot inherit another network's cursors,
// attaches or disabled flags. Clients stay connected: the removal is a routing
// fact, not a client-visible event.
func (s *Server) RemoveNetwork(name string) error {
	s.netsMu.Lock()
	idx := -1
	for i, ne := range s.nets {
		if ne.name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.netsMu.Unlock()
		return fmt.Errorf("facade: unknown network %q", name)
	}
	s.nets = append(s.nets[:idx], s.nets[idx+1:]...)
	s.netsMu.Unlock()

	s.windows.forgetNetwork(name)
	return nil
}

// netSnapshot is the current network list under the lock: readers that iterate
// (replay, forward) must not hold the lock while writing to a subscriber.
func (s *Server) netSnapshot() []*networkEntry {
	s.netsMu.RLock()
	out := make([]*networkEntry, len(s.nets))
	copy(out, s.nets)
	s.netsMu.RUnlock()
	return out
}

// networkName is the name shown in 001/005. It follows the live network list
// rather than a value frozen at boot, because a runtime network (B5) can change
// what the single network is. An explicit cfg.Network always wins.
func (s *Server) networkName() string {
	if s.cfg.Network != "" {
		return s.cfg.Network
	}
	names := s.Networks()
	if len(names) == 1 {
		return names[0]
	}
	return s.cfg.ServerName
}

// Attached returns the number of clients currently attached to the hub.
func (s *Server) Attached() int { return s.hub.Len() }

// networkEntry returns the entry for a network, or nil.
func (s *Server) networkEntry(name string) *networkEntry {
	for _, ne := range s.netSnapshot() {
		if ne.name == name {
			return ne
		}
	}
	return nil
}

// Traffic sums bytes in and out over every upstream. ZNC keeps per-user
// counters and prints them in TRAFFIC; this daemon has one user, so the sum is
// that user's traffic.
func (s *Server) Traffic() (in, out uint64) {
	for _, ne := range s.netSnapshot() {
		i, o := ne.net.Traffic()
		in += i
		out += o
	}
	return in, out
}

// broadcastAway sends one AWAY line to every network. The reason is already
// expanded, so the line goes on the wire unchanged.
func (s *Server) broadcastAway(line string) {
	for _, ne := range s.netSnapshot() {
		_ = ne.net.Send([]byte(line))
	}
}

// OutboundSource returns the client whose line is being sent right now, or nil
// when the line did not come from a client. The daemon reads it inside OnSend so
// the echo-message gate can tell the author from everyone else.
func (s *Server) OutboundSource() *Subscriber {
	return s.outSource.Load()
}

// KnownNick returns the upstream nick the facade has learned for a network, or
// "". Callers outside the client path use it to answer "is this line about us"
// without keeping a second nickname cache.
func (s *Server) KnownNick(network string) string {
	ne := s.network(network)
	if ne == nil {
		return ""
	}
	return ne.knownNick()
}

type client struct {
	srv *Server
	sub *Subscriber
	br  *bufio.Reader

	nick string
	user string

	caps           uint32
	capNegotiating bool
	capEnded       bool
	registered     bool

	scratch   []byte
	replayBuf []byte
	readBuf   []byte
}

func (s *Server) handleConn(conn net.Conn) {
	defer s.active.Add(-1)

	sub := NewSubscriber(conn, s.cfg.OutBuf)
	sub.closer = conn

	readSize := s.cfg.MaxLineLen
	if readSize < 4096 {
		readSize = 4096
	}
	c := &client{
		srv:       s,
		sub:       sub,
		br:        bufio.NewReaderSize(conn, readSize),
		scratch:   make([]byte, 0, s.cfg.MaxLineLen+512),
		replayBuf: make([]byte, 0, maxRingLine+512),
		readBuf:   make([]byte, s.cfg.MaxLineLen),
	}

	go c.writeLoop()
	defer func() {
		s.hub.Remove(sub)
		s.windows.forget(sub)
		sub.Close()
		// simple_away's OnClientDisconnect: go away only when the *last*
		// client left, which is why the hub count decides.
		if s.hub.Len() == 0 {
			s.away.clientDetached()
		}
	}()

	c.readLoop()
}

func (c *client) writeLoop() {
	for {
		select {
		case <-c.sub.wake:
			if err := c.sub.Flush(); err != nil {
				c.sub.Close()
				return
			}
		case <-c.sub.Done():
			_ = c.sub.Flush()
			return
		}
	}
}

func (c *client) readLoop() {
	for {
		line, err := c.readLine()
		if err != nil {
			return
		}
		if len(line) == 0 {
			continue
		}
		_, rest := ircline.Tags(line)
		_, rest = ircline.Prefix(rest)
		if !c.handle(line, rest) {
			return
		}
	}
}

func (c *client) handle(line, rest []byte) bool {
	switch {
	case ircline.IsVerb(rest, "CAP"):
		return c.handleCap(rest)
	case ircline.IsVerb(rest, "NICK"):
		return c.handleNick(line, rest)
	case ircline.IsVerb(rest, "USER"):
		return c.handleUser(rest)
	case ircline.IsVerb(rest, "PING"):
		return c.handlePing(rest)
	case ircline.IsVerb(rest, "QUIT"):
		return false
	case ircline.IsVerb(rest, "WEBIRC"):
		return c.send(":", c.srv.cfg.ServerName, " NOTICE * :WEBIRC not supported")
	case ircline.IsVerb(rest, "SETBINDHOST"):
		return c.handleSetBindHost(rest)
	case ircline.IsVerb(rest, "CLEARBINDHOST"):
		return c.handleClearBindHost()
	case ircline.IsVerb(rest, "SETUSERBINDHOST"):
		return c.handleSetUserBindHost(rest)
	case ircline.IsVerb(rest, "CLEARUSERBINDHOST"):
		return c.handleClearUserBindHost()
	case ircline.IsVerb(rest, "SHOWBINDHOST"):
		return c.handleShowBindHost()
	case ircline.IsVerb(rest, "ATTACH"):
		return c.handleAttach(rest)
	case ircline.IsVerb(rest, "DETACH"):
		return c.handleDetach(rest)
	case ircline.IsVerb(rest, "PLAYBUFFER"):
		return c.handlePlayBuffer(rest)
	case ircline.IsVerb(rest, "CLEARBUFFER"):
		return c.handleClearBuffer(rest)
	case ircline.IsVerb(rest, "CLEARALLCHANNELBUFFERS"):
		return c.handleClearAllBuffers(rest, windowChannels)
	case ircline.IsVerb(rest, "CLEARALLQUERYBUFFERS"):
		return c.handleClearAllBuffers(rest, windowQueries)
	case ircline.IsVerb(rest, "CLEARALLBUFFERS"):
		return c.handleClearAllBuffers(rest, windowAll)
	case ircline.IsVerb(rest, "DISABLECHAN"):
		return c.handleDisableChan(rest, true)
	case ircline.IsVerb(rest, "ENABLECHAN"):
		return c.handleDisableChan(rest, false)
	case ircline.IsVerb(rest, "SETBUFFER"):
		return c.handleSetBuffer(rest)
	case ircline.IsVerb(rest, "LISTCHANS"):
		return c.handleListChans()
	case ircline.IsVerb(rest, "LISTNETWORKS"):
		return c.handleListNetworks()
	case ircline.IsVerb(rest, "LISTSERVERS"):
		return c.handleListServers()
	case ircline.IsVerb(rest, "LISTCLIENTS"):
		return c.handleListClients()
	case ircline.IsVerb(rest, "TRAFFIC"):
		return c.handleTraffic()
	case ircline.IsVerb(rest, "AWAY"):
		return c.handleAway(rest)
	case ircline.IsVerb(rest, "ADDSERVER"):
		return c.handleAddServer(rest)
	case ircline.IsVerb(rest, "DELSERVER"), ircline.IsVerb(rest, "REMSERVER"):
		return c.handleDelServer(rest)
	case ircline.IsVerb(rest, "MOVECHAN"):
		return c.handleMoveChan(rest)
	case ircline.IsVerb(rest, "MOVENETWORK"):
		return c.handleMoveNetwork()
	case ircline.IsVerb(rest, "CONNECT"):
		return c.handleConnect()
	case ircline.IsVerb(rest, "DISCONNECT"):
		return c.handleDisconnect()
	case ircline.IsVerb(rest, "ADDTRUSTEDSERVERFINGERPRINT"):
		return c.handleTrustedFingerprint(rest, fpAdd)
	case ircline.IsVerb(rest, "DELTRUSTEDSERVERFINGERPRINT"):
		return c.handleTrustedFingerprint(rest, fpDel)
	case ircline.IsVerb(rest, "LISTTRUSTEDSERVERFINGERPRINTS"):
		return c.handleTrustedFingerprint(rest, fpList)
	default:
		return c.forward(line)
	}
}

// commandNetwork returns the network the bind-host commands act on. Like
// forward, the proof of concept has no in-band network selector: it is the first
// network in sorted order.
func (c *client) commandNetwork() string {
	nets := c.srv.netSnapshot()
	if len(nets) == 0 {
		return ""
	}
	return nets[0].name
}

func (c *client) bindHosts() BindHosts {
	return c.srv.cfg.BindHosts
}

func (c *client) status(text ...string) bool {
	nick := c.nick
	if nick == "" {
		nick = "*"
	}
	parts := make([]string, 0, len(text)+4)
	parts = append(parts, ":", c.srv.cfg.ServerName, " NOTICE ", nick, " :")
	parts = append(parts, text...)
	return c.send(parts...)
}

func (c *client) noBindHosts() bool {
	return c.bindHosts() == nil
}

func (c *client) handleSetBindHost(rest []byte) bool {
	if c.noBindHosts() {
		return c.status("SetBindHost is not available")
	}
	arg := string(ircline.FirstParam(ircline.Params(rest)))
	if arg == "" {
		return c.status("Usage: SetBindHost <host>")
	}
	network := c.commandNetwork()
	if current, _ := c.bindHosts().Resolve(network); arg == current {
		return c.status("You already have this bind host!")
	}
	if err := c.bindHosts().SetNetwork(network, arg); err != nil {
		return c.status(err.Error())
	}
	return c.status("Set bind host for network ", network, " to ", arg)
}

func (c *client) handleClearBindHost() bool {
	if c.noBindHosts() {
		return c.status("ClearBindHost is not available")
	}
	network := c.commandNetwork()
	if err := c.bindHosts().ClearNetwork(network); err != nil {
		return c.status(err.Error())
	}
	return c.status("Bind host cleared for this network.")
}

func (c *client) handleSetUserBindHost(rest []byte) bool {
	if c.noBindHosts() {
		return c.status("SetUserBindHost is not available")
	}
	arg := string(ircline.FirstParam(ircline.Params(rest)))
	if arg == "" {
		return c.status("Usage: SetUserBindHost <host>")
	}
	if err := c.bindHosts().SetUser(arg); err != nil {
		return c.status(err.Error())
	}
	return c.status("Set default bind host to ", arg)
}

func (c *client) handleClearUserBindHost() bool {
	if c.noBindHosts() {
		return c.status("ClearUserBindHost is not available")
	}
	if err := c.bindHosts().ClearUser(); err != nil {
		return c.status(err.Error())
	}
	return c.status("Default bind host cleared for your user.")
}

func (c *client) handleShowBindHost() bool {
	if c.noBindHosts() {
		return c.status("ShowBindHost is not available")
	}
	host, origin := c.bindHosts().Resolve(c.commandNetwork())
	return c.status("bind_host=", host, " (", origin, ")")
}

func (c *client) handleCap(rest []byte) bool {
	params := ircline.Params(rest)
	sub, args := nextToken(params)
	if len(sub) == 0 {
		return true
	}

	switch {
	case equalFold(sub, "LS"):
		c.capNegotiating = true
		return c.send(":", c.srv.cfg.ServerName, " CAP * LS :", supportedCaps)
	case equalFold(sub, "REQ"):
		c.capNegotiating = true
		return c.handleCapReq(args)
	case equalFold(sub, "LIST"):
		return c.handleCapList()
	case equalFold(sub, "END"):
		c.capEnded = true
		return c.maybeRegister()
	default:
		return true
	}
}

func (c *client) handleCapReq(args []byte) bool {
	req := args
	if len(req) > 0 && req[0] == ':' {
		req = req[1:]
	}

	var ack [256]byte
	ackN := 0
	mask := c.caps

	for len(req) > 0 {
		var tok []byte
		tok, req = nextToken(req)
		if len(tok) == 0 {
			continue
		}

		neg := false
		name := tok
		if name[0] == '-' {
			neg = true
			name = name[1:]
		} else if name[0] == '+' {
			name = name[1:]
		}
		if i := indexByte(name, '='); i >= 0 {
			name = name[:i]
		}

		idx := capIndex(name)
		if idx < 0 {
			continue
		}
		if neg {
			mask &^= 1 << idx
		} else {
			mask |= 1 << idx
		}
		if ackN > 0 {
			ack[ackN] = ' '
			ackN++
		}
		ackN += copy(ack[ackN:], capNames[idx])
	}

	if ackN == 0 {
		return c.send(":", c.srv.cfg.ServerName, " CAP * NAK :", string(req))
	}
	c.caps = mask
	// echo-message decides whether this client gets its own lines back, which is
	// ZNC's HasEchoMessage gate (Client.cpp:1138). Keep the subscriber in step,
	// including for a CAP REQ that arrives after registration.
	if idx := capIndex([]byte("echo-message")); idx >= 0 {
		c.sub.SetEchoMessage(c.caps&(1<<uint(idx)) != 0)
	}
	if c.registered {
		c.sub.SetIdentity(c.nick, c.extendedJoin())
	}
	return c.send(":", c.srv.cfg.ServerName, " CAP * ACK :", string(ack[:ackN]))
}

func (c *client) extendedJoin() bool {
	idx := capIndex([]byte("extended-join"))
	return idx >= 0 && c.caps&(1<<uint(idx)) != 0
}

func (c *client) handleCapList() bool {
	var ack [256]byte
	n := 0
	for i, name := range capNames {
		if c.caps&(1<<i) == 0 {
			continue
		}
		if n > 0 {
			ack[n] = ' '
			n++
		}
		n += copy(ack[n:], name)
	}
	return c.send(":", c.srv.cfg.ServerName, " CAP * ACK :", string(ack[:n]))
}

func (c *client) handleNick(line, rest []byte) bool {
	nick := ircline.FirstParam(ircline.Params(rest))
	if len(nick) == 0 {
		return true
	}

	changed := string(nick) != c.nick
	c.nick = string(nick)

	if c.registered {
		if changed {
			c.sub.SetIdentity(c.nick, c.extendedJoin())
			return c.forward(line)
		}
		return true
	}
	return c.maybeRegister()
}

func (c *client) handleUser(rest []byte) bool {
	user := ircline.FirstParam(ircline.Params(rest))
	if len(user) == 0 {
		return true
	}
	c.user = string(user)
	return c.maybeRegister()
}

func (c *client) handlePing(rest []byte) bool {
	x := ircline.Params(rest)
	if len(x) > 0 && x[0] == ':' {
		x = x[1:]
	}
	return c.sendBytes([]string{":", c.srv.cfg.ServerName, " PONG ", c.srv.cfg.ServerName, " :"}, x)
}

func (c *client) maybeRegister() bool {
	if c.registered || c.nick == "" || c.user == "" {
		return true
	}
	if c.capNegotiating && !c.capEnded {
		return true
	}

	c.registered = true
	c.sub.SetIdentity(c.nick, c.extendedJoin())
	if !c.sendRegistration() {
		return false
	}
	if !c.replayAll() {
		return false
	}
	if !c.srv.hub.Add(c.sub) {
		return c.send("ERROR :too many clients")
	}
	// simple_away's OnClientLogin: a client is here, come back. awaystore's
	// addition is the count of private messages that arrived while the network
	// said we were gone, announced to the client that just came back — the one
	// whose send path is passed in, not a shared field.
	c.srv.away.clientAttached(func(n int, capped bool) {
		text := "You have " + strconv.Itoa(n) + " messages!"
		if capped {
			text = "You have " + strconv.Itoa(n) + "+ messages!"
		}
		c.status("Welcome back!")
		c.status(text)
	})
	return true
}

func (c *client) sendRegistration() bool {
	srv := c.srv.cfg.ServerName
	net := c.srv.networkName()
	nick := c.nick

	return c.send(":", srv, " 001 ", nick, " :Welcome to ", net, " ", nick, "!", c.user, "@salotto") &&
		c.send(":", srv, " 002 ", nick, " :Your host is ", srv, ", running salotto-0.1") &&
		c.send(":", srv, " 003 ", nick, " :This server was created ", c.srv.created) &&
		c.send(":", srv, " 004 ", nick, " ", srv, " salotto-0.1 i s") &&
		c.send(":", srv, " 005 ", nick, " CHANTYPES=# PREFIX=(ov)@+ CASEMAPPING=rfc1459 NETWORK=", net, " :are supported by this server") &&
		c.sendMOTD(nick)
}

// sendMOTD writes the login-time MOTD according to the operator's policy, which
// is ZNC's block_motd and missingmotd in one place (B15):
//
//   - block (block_motd, default on): 375/372 are suppressed and 376's text is
//     replaced with "MOTD blocked by ZNC", exactly as block_motd does. A client
//     still gets a complete 375/376 pair, so it does not think the server is
//     broken.
//   - missing (missingmotd): a 422 instead of any MOTD, which is what a server
//     without one sends.
//
// The MOTD here is the bouncer's own, not the upstream server's: it explains
// that replayed history follows. Blocking it is therefore about client noise,
// not about hiding the network's MOTD, which the network's own 375/372 reach the
// client unmodified only if the operator allows them.
func (c *client) sendMOTD(nick string) bool {
	srv := c.srv.cfg.ServerName
	switch {
	case c.srv.cfg.MOTDMissing:
		return c.send(":", srv, " 422 ", nick, " :MOTD File is missing")
	case c.srv.cfg.MOTDBlocked:
		// 375 is suppressed too: ZNC HALTs both 375 and 372, so a client sees
		// only the 376 with the replaced text.
		return c.send(":", srv, " 376 ", nick, " :MOTD blocked by ZNC")
	default:
		return c.send(":", srv, " 375 ", nick, " :- ", srv, " Message of the day -") &&
			c.send(":", srv, " 372 ", nick, " :- salotto proof of concept: replayed history follows.") &&
			c.send(":", srv, " 376 ", nick, " :End of /MOTD command.")
	}
}

func (c *client) replayAll() bool {
	for _, ne := range c.srv.netSnapshot() {
		if !c.replayNetwork(ne) {
			return false
		}
	}
	return true
}

// replayNetwork replays every known window as its own batch, in sorted window
// order. There is no merged per-network stream any more: a line shared by two
// windows was buffered into both rings and is replayed once per window.
func (c *client) replayNetwork(ne *networkEntry) bool {
	windows := ne.net.Windows()
	un := ne.knownNick()
	if un == "" {
		for _, window := range windows {
			r, ok := ne.net.Window(window)
			if !ok || r.Newest() == 0 {
				continue
			}
			r.Replay(r.Oldest(), func(seq uint64, line []byte) bool {
				if v := detectUpstreamNick(line); v != "" {
					ne.nick.set(v)
					un = v
					return false
				}
				return true
			})
			if un != "" {
				break
			}
		}
	}

	buf := c.replayBuf
	for _, window := range windows {
		r, exists := ne.net.Window(window)
		if !exists || r.Newest() == 0 {
			continue
		}
		id := ne.name + "-" + window + "-replay"
		if !c.send(":", c.srv.cfg.ServerName, " BATCH +replay ", id) {
			return false
		}
		wrote := true
		r.Replay(r.Oldest(), func(seq uint64, line []byte) bool {
			ts := time.Now().UnixNano()
			if v, has := r.Stamp(seq); has && v != 0 {
				ts = v
			}
			forLine, next := ne.advanceNick(un, line)
			un = next
			buf = appendReplayLine(buf[:0], line, id, ts)
			if !c.sub.WriteFrom(buf, forLine) {
				wrote = false
				return false
			}
			return true
		})
		c.replayBuf = buf
		if !wrote {
			return false
		}
		if !c.send(":", c.srv.cfg.ServerName, " BATCH -replay ", id) {
			return false
		}
	}
	return true
}

// forward sends a line upstream. With more than one network there is no
// in-band way for a plain IRC client to select one, so selection is
// deterministic: the first network in sorted name order. The limitation is
// deliberate for the proof of concept; a real client would use a management
// protocol to choose a network. No 402 is emitted because the choice is never
// ambiguous.
func (c *client) forward(line []byte) bool {
	// The daemon fans outbound lines out from its OnSend hook, which runs
	// synchronously inside Network.Send, and it must know which client is the
	// author so that client's own line is only echoed back when it asked for
	// echo-message (ZNC does the same, Client.cpp:1138). outMu serialises the
	// attribution: Send is enqueue-only and does not block, so holding it across
	// the call costs nothing while making the attribution exact.
	c.srv.outMu.Lock()
	c.srv.outSource.Store(c.sub)
	defer func() {
		c.srv.outSource.Store(nil)
		c.srv.outMu.Unlock()
	}()

	nets := c.srv.netSnapshot()
	if len(nets) == 0 {
		return c.notConnected()
	}
	if err := nets[0].net.Send(line); err != nil {
		return c.notConnected()
	}
	c.clearOnSpeak(line)
	return true
}

// clearOnSpeak implements ZNC's clearbufferonmsg: while the user is talking,
// playback should start at the current conversation, so every channel and query
// buffer is dropped on a message the user sends. ZNC's module clears on msg,
// ctcp, action, notice, part and topic but explicitly not on quit
// (clearbufferonmsg.cpp: m_bRules[RULE_QUIT] = false), and it skips detached
// channels because those had not been read yet.
func (c *client) clearOnSpeak(line []byte) {
	if !c.srv.cfg.ClearBuffersOnMsg {
		return
	}
	_, rest := ircline.Tags(line)
	_, rest = ircline.Prefix(rest)
	verb := ircline.Verb(rest)
	switch {
	case bytes.EqualFold(verb, []byte("PRIVMSG")),
		bytes.EqualFold(verb, []byte("NOTICE")),
		bytes.EqualFold(verb, []byte("PART")),
		bytes.EqualFold(verb, []byte("TOPIC")):
	default:
		return
	}
	network := c.commandNetwork()
	if network == "" {
		return
	}
	for _, window := range c.srv.windows.knownWindows(network) {
		if !c.srv.windows.isAttached(c.sub, windowKey(network, window)) {
			continue
		}
		_, _ = c.srv.ClearBuffer(network, window)
	}
}

func (c *client) notConnected() bool {
	nick := c.nick
	if nick == "" {
		nick = "*"
	}
	return c.send(":", c.srv.cfg.ServerName, " NOTICE ", nick, " :not connected upstream")
}

func (c *client) send(parts ...string) bool {
	b := c.scratch[:0]
	for _, p := range parts {
		b = append(b, p...)
	}
	b = append(b, '\r', '\n')
	c.scratch = b
	return c.sub.Write(b)
}

func (c *client) sendBytes(prefix []string, tail []byte) bool {
	b := c.scratch[:0]
	for _, p := range prefix {
		b = append(b, p...)
	}
	b = append(b, tail...)
	b = append(b, '\r', '\n')
	c.scratch = b
	return c.sub.Write(b)
}

func (c *client) readLine() ([]byte, error) {
	line, err := c.br.ReadSlice('\n')
	if err == bufio.ErrBufferFull {
		n := len(line)
		if n > c.srv.cfg.MaxLineLen {
			n = c.srv.cfg.MaxLineLen
		}
		copy(c.readBuf[:n], line[:n])
		for {
			_, derr := c.br.ReadSlice('\n')
			switch derr {
			case nil:
				return trimEOL(c.readBuf[:n]), nil
			case bufio.ErrBufferFull:
				continue
			case io.EOF:
				return trimEOL(c.readBuf[:n]), nil
			default:
				return nil, derr
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if len(line) > c.srv.cfg.MaxLineLen {
		line = line[:c.srv.cfg.MaxLineLen]
	}
	return trimEOL(line), nil
}

func appendLiveLine(dst, line []byte, ts int64) []byte {
	if ts == 0 {
		ts = time.Now().UnixNano()
	}
	tags, rest := ircline.Tags(line)
	if tags == nil {
		dst = append(dst, '@')
		dst = append(dst, "time="...)
		dst = appendISOTime(dst, ts)
		dst = append(dst, ' ')
		dst = append(dst, line...)
		return append(dst, '\r', '\n')
	}
	dst = append(dst, '@')
	if ircline.TagValue(tags, tagTime) == nil {
		dst = append(dst, "time="...)
		dst = appendISOTime(dst, ts)
		if len(tags) > 0 {
			dst = append(dst, ';')
			dst = append(dst, tags...)
		}
	} else {
		dst = append(dst, tags...)
	}
	if len(rest) > 0 {
		dst = append(dst, ' ')
		dst = append(dst, rest...)
	}
	return append(dst, '\r', '\n')
}

func appendReplayLine(dst, line []byte, batch string, ts int64) []byte {
	tags, rest := ircline.Tags(line)
	if tags == nil {
		dst = append(dst, '@')
		dst = append(dst, "batch="...)
		dst = append(dst, batch...)
		dst = append(dst, ";time="...)
		dst = appendISOTime(dst, ts)
		dst = append(dst, ' ')
		dst = append(dst, line...)
		return append(dst, '\r', '\n')
	}

	dst = append(dst, '@')
	dst = append(dst, "batch="...)
	dst = append(dst, batch...)
	if len(tags) > 0 {
		dst = append(dst, ';')
		dst = append(dst, tags...)
	}
	if ircline.TagValue(tags, tagTime) == nil {
		dst = append(dst, ";time="...)
		dst = appendISOTime(dst, ts)
	}
	if len(rest) > 0 {
		dst = append(dst, ' ')
		dst = append(dst, rest...)
	}
	return append(dst, '\r', '\n')
}

// appendISOTime appends an RFC3339 UTC timestamp with milliseconds. It is
// deliberately allocation-free; time.Time.Format allocates on this path.
func appendISOTime(dst []byte, ts int64) []byte {
	const billion = int64(time.Second)
	sec := ts / billion
	frac := ts % billion
	if frac < 0 {
		frac += billion
		sec--
	}
	ms := frac / int64(time.Millisecond)

	days := floorDiv(sec, 86400)
	sod := sec - days*86400

	// Howard Hinnant's civil_from_days.
	z := days + 719468
	era := floorDiv(z, 146097)
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y := yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d := doy - (153*mp+2)/5 + 1
	m := mp + 3
	if m > 12 {
		m -= 12
		y++
	}

	hour := sod / 3600
	min := (sod % 3600) / 60
	secmin := sod % 60

	dst = append4(dst, int(y))
	dst = append(dst, '-')
	dst = append2(dst, int(m))
	dst = append(dst, '-')
	dst = append2(dst, int(d))
	dst = append(dst, 'T')
	dst = append2(dst, int(hour))
	dst = append(dst, ':')
	dst = append2(dst, int(min))
	dst = append(dst, ':')
	dst = append2(dst, int(secmin))
	dst = append(dst, '.')
	dst = append3(dst, int(ms))
	return append(dst, 'Z')
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b < 0 {
		q--
	}
	return q
}

func append4(dst []byte, v int) []byte {
	return append(dst,
		byte('0'+v/1000%10),
		byte('0'+v/100%10),
		byte('0'+v/10%10),
		byte('0'+v%10),
	)
}

func append3(dst []byte, v int) []byte {
	return append(dst,
		byte('0'+v/100%10),
		byte('0'+v/10%10),
		byte('0'+v%10),
	)
}

func append2(dst []byte, v int) []byte {
	return append(dst, byte('0'+v/10%10), byte('0'+v%10))
}

func nextToken(p []byte) (tok, rest []byte) {
	i := 0
	for i < len(p) && p[i] == ' ' {
		i++
	}
	start := i
	for i < len(p) && p[i] != ' ' {
		i++
	}
	tok = p[start:i]
	for i < len(p) && p[i] == ' ' {
		i++
	}
	return tok, p[i:]
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func capIndex(name []byte) int {
	for i, cap := range capNames {
		if equalFold(name, cap) {
			return i
		}
	}
	return -1
}

func equalFold(b []byte, s string) bool {
	if len(b) != len(s) {
		return false
	}
	for i := range b {
		c := b[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		d := s[i]
		if d >= 'A' && d <= 'Z' {
			d += 'a' - 'A'
		}
		if c != d {
			return false
		}
	}
	return true
}

func trimEOL(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
	}
	if n := len(b); n > 0 && b[n-1] == '\r' {
		b = b[:n-1]
	}
	return b
}
