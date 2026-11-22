package facade

import (
	"bufio"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"sort"
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

// Network is a handle to one upstream. The facade deliberately does not depend
// on internal/upstream.
type Network interface {
	Name() string
	Send(line []byte) error
	State() string
	Replay(from uint64, emit func(seq uint64, line []byte) bool) uint64
	Newest() uint64
	Oldest() uint64
	Bytes() int
}

// stampSource is an optional extension implemented by networks that can expose
// the timestamp stored with a ring record. A network that does not implement it
// gets a replay-time timestamp for lines that have no time tag of their own.
type stampSource interface {
	Stamp(seq uint64) (int64, bool)
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

func (ne *networkEntry) knownNick() string {
	if v := ne.nick.get(); v != "" {
		return v
	}
	if src, ok := ne.net.(upstreamNickSource); ok {
		if v := src.Nick(); v != "" {
			ne.nick.set(v)
			return v
		}
	}
	return ""
}

// advanceNick returns the upstream nick to use for this line and the nick to
// use for the following line. An upstream NICK change is dropped from the
// client stream but still advances the cached identity.
func (ne *networkEntry) advanceNick(un string, line []byte) (forLine, next string) {
	if un == "" {
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
	cfg     Config
	log     *slog.Logger
	hub     *Hub
	nets    []networkEntry
	windows *windowState
	created string
	active  atomic.Int64

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

	s := &Server{
		cfg:     cfg,
		log:     cfg.Log,
		windows: newWindowState(),
		created: time.Now().UTC().Format("Mon Jan 02 2006 15:04:05 MST"),
	}

	names := make([]string, 0, len(cfg.Networks))
	for name, net := range cfg.Networks {
		if net == nil {
			return nil, errors.New("facade: nil network " + name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	s.nets = make([]networkEntry, 0, len(names))
	for _, name := range names {
		s.nets = append(s.nets, networkEntry{name: name, net: *cfg.Networks[name], nick: &nickCache{}})
	}

	if s.cfg.Network == "" {
		if len(s.nets) == 1 {
			s.cfg.Network = s.nets[0].name
		} else {
			s.cfg.Network = s.cfg.ServerName
		}
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
	for i := range s.nets {
		if s.nets[i].name == name {
			return &s.nets[i]
		}
	}
	return nil
}

// Attached returns the number of clients currently attached to the hub.
func (s *Server) Attached() int { return s.hub.Len() }

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
	case ircline.IsVerb(rest, "LISTCHANS"):
		return c.handleListChans()
	default:
		return c.forward(line)
	}
}

// commandNetwork returns the network the bind-host commands act on. Like
// forward, the proof of concept has no in-band network selector: it is the first
// network in sorted order.
func (c *client) commandNetwork() string {
	if len(c.srv.nets) == 0 {
		return ""
	}
	return c.srv.nets[0].name
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
	return true
}

func (c *client) sendRegistration() bool {
	srv := c.srv.cfg.ServerName
	net := c.srv.cfg.Network
	nick := c.nick

	return c.send(":", srv, " 001 ", nick, " :Welcome to ", net, " ", nick, "!", c.user, "@salotto") &&
		c.send(":", srv, " 002 ", nick, " :Your host is ", srv, ", running salotto-0.1") &&
		c.send(":", srv, " 003 ", nick, " :This server was created ", c.srv.created) &&
		c.send(":", srv, " 004 ", nick, " ", srv, " salotto-0.1 i s") &&
		c.send(":", srv, " 005 ", nick, " CHANTYPES=# PREFIX=(ov)@+ CASEMAPPING=rfc1459 NETWORK=", net, " :are supported by this server") &&
		c.send(":", srv, " 375 ", nick, " :- ", srv, " Message of the day -") &&
		c.send(":", srv, " 372 ", nick, " :- salotto proof of concept: replayed history follows.") &&
		c.send(":", srv, " 376 ", nick, " :End of /MOTD command.")
}

func (c *client) replayAll() bool {
	for _, ne := range c.srv.nets {
		if !c.replayNetwork(ne) {
			return false
		}
	}
	return true
}

func (c *client) replayNetwork(ne networkEntry) bool {
	id := ne.name + "-replay"
	if !c.send(":", c.srv.cfg.ServerName, " BATCH +replay ", id) {
		return false
	}

	un := ne.knownNick()
	if un == "" {
		ne.net.Replay(ne.net.Oldest(), func(seq uint64, line []byte) bool {
			if v := detectUpstreamNick(line); v != "" {
				ne.nick.set(v)
				un = v
				return false
			}
			return true
		})
	}

	st, _ := ne.net.(stampSource)
	buf := c.replayBuf
	ok := true
	ne.net.Replay(ne.net.Oldest(), func(seq uint64, line []byte) bool {
		ts := time.Now().UnixNano()
		if st != nil {
			if v, has := st.Stamp(seq); has && v != 0 {
				ts = v
			}
		}
		forLine, next := ne.advanceNick(un, line)
		un = next
		buf = appendReplayLine(buf[:0], line, id, ts)
		if !c.sub.WriteFrom(buf, forLine) {
			ok = false
			return false
		}
		return true
	})
	c.replayBuf = buf
	if !ok {
		return false
	}

	return c.send(":", c.srv.cfg.ServerName, " BATCH -replay ", id)
}

// forward sends a line upstream. With more than one network there is no
// in-band way for a plain IRC client to select one, so selection is
// deterministic: the first network in sorted name order. The limitation is
// deliberate for the proof of concept; a real client would use a management
// protocol to choose a network. No 402 is emitted because the choice is never
// ambiguous.
func (c *client) forward(line []byte) bool {
	if len(c.srv.nets) == 0 {
		return c.notConnected()
	}
	if err := c.srv.nets[0].net.Send(line); err != nil {
		return c.notConnected()
	}
	return true
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
