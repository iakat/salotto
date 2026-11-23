package facade

import (
	"net"
	"strconv"
	"strings"

	"github.com/iakat/salotto/internal/ircline"
)

// The list commands. Every one answers with ZNC's grid table — LISTCLIENTS
// (ClientCommand.cpp:177), LISTNETWORKS (:658), LISTSERVERS (:881), TRAFFIC
// (:1575) — and uses ZNC's column names in ZNC's order, because the output is
// read by someone who already knows what those tables look like.

// stateRegistered is upstream.StateRegistered as the string the Network
// interface reports. The facade must not import internal/upstream — that is the
// whole point of the interface — so the value is pinned here and asserted
// against the real one in the daemon's tests.
const stateRegistered = "registered"

// handleListNetworks renders LISTNETWORKS. A network that is not on IRC prints
// "No" and leaves the remaining cells empty, which is what CTable does with an
// unset cell; only a registered network reports its server, nick mask and
// channel count (ClientCommand.cpp:658-698).
func (c *client) handleListNetworks() bool {
	nets := c.srv.netSnapshot()
	t := newTable("Network", "On IRC", "IRC Server", "IRC User", "Channels")
	for _, ne := range nets {
		if ne.net.State() != stateRegistered {
			t.add(ne.name, "No")
			continue
		}
		t.add(ne.name, "Yes", ne.net.Server(), ne.net.NickMask(),
			itoa(uint64(len(ne.net.Windows()))))
	}
	if !c.sendTable(t) {
		return false
	}
	if t.len() == 0 {
		return c.status("No networks")
	}
	return true
}

// handleListServers renders LISTSERVERS. There is exactly one server per
// network by construction, and it is the current one, so it carries ZNC's
// trailing asterisk. A configured password is shown as ****** and never echoed
// (ClientCommand.cpp:881-913).
func (c *client) handleListServers() bool {
	network, ok := c.requireNetwork()
	if network == "" {
		return ok
	}
	ne := c.srv.networkEntry(network)
	if ne == nil {
		return c.status("You must be connected with a network to use this command")
	}
	host, port := splitServer(ne.net.Server())
	ssl := ""
	if strings.HasPrefix(port, "+") {
		ssl = "SSL"
		port = strings.TrimPrefix(port, "+")
	}
	pass := ""
	if ne.net.HasPassword() {
		pass = "******"
	}
	t := newTable("Host", "Port", "SSL", "Password")
	t.add(host+"*", port, ssl, pass)
	return c.sendTable(t)
}

// handleListClients renders LISTCLIENTS: every attached client's address, the
// network it is looking at, and its identifier (ClientCommand.cpp:177-215).
func (c *client) handleListClients() bool {
	subs := c.srv.hub.Snapshot()
	t := newTable("Host", "Network", "Identifier")
	for _, sub := range subs {
		t.add(sub.RemoteHost(), sub.NetworkName(), sub.Identifier())
	}
	if !c.sendTable(t) {
		return false
	}
	if t.len() == 0 {
		return c.status("No clients are connected")
	}
	return true
}

// handleConnect clears a DISCONNECT and dials again, answering with ZNC's
// "Connecting to <server>..." (ClientCommand.cpp:336-386). This bouncer has one
// server per network, so the "jump to the next server" branch does not exist.
func (c *client) handleConnect() bool {
	network, ok := c.requireNetwork()
	if network == "" {
		return ok
	}
	ne := c.srv.networkEntry(network)
	if ne == nil {
		return c.status("You must be connected with a network to use this command")
	}
	host, _ := splitServer(ne.net.Server())
	ne.net.SetConnectEnabled(true)
	return c.status("Connecting to ", host, "...")
}

// handleDisconnect stops the upstream and keeps it down until CONNECT, which is
// exactly what ZNC's DISCONNECT does — including the wording, because a user
// script that greps for it is a user script that should keep working
// (ClientCommand.cpp:387-403).
func (c *client) handleDisconnect() bool {
	network, ok := c.requireNetwork()
	if network == "" {
		return ok
	}
	ne := c.srv.networkEntry(network)
	if ne == nil {
		return c.status("You must be connected with a network to use this command")
	}
	ne.net.SetConnectEnabled(false)
	return c.status("Disconnected from IRC. Use 'connect' to reconnect.")
}

// handleTraffic renders TRAFFIC: bytes in and out per user, then the same for
// the bouncer and the two combined (ClientCommand.cpp:1575-1624). There is one
// user, so the per-user row and <Users> are the same numbers.
func (c *client) handleTraffic() bool {
	in, out := c.srv.Traffic()
	t := newTable("Username", "In", "Out", "Total")
	user := c.srv.cfg.UserName
	if user == "" {
		user = "user"
	}
	t.add(user, byteStr(in), byteStr(out), byteStr(in+out))
	// Bytes the bouncer itself moved for its own handshakes: folded into the
	// user total above, so the ZNC row is zero and the total matches the user.
	t.add("<ZNC>", byteStr(0), byteStr(0), byteStr(0))
	t.add("<Total>", byteStr(in), byteStr(out), byteStr(in+out))
	return c.sendTable(t)
}

// handleSetBuffer implements SETBUFFER: a glob over window names sets each
// matching window's buffer size, and the answer counts the failures rather than
// reporting per window (ClientCommand.cpp:1532-1574). The glob is ZNC's
// MatchChans rule, already implemented for CLEARBUFFER, so the two agree.
func (c *client) handleSetBuffer(rest []byte) bool {
	network, ok := c.requireNetwork()
	if network == "" {
		return ok
	}
	pattern := string(ircline.FirstParam(ircline.Params(rest)))
	if pattern == "" {
		return c.status("Usage: SetBuffer <#chan|query> [linecount]")
	}
	countStr := string(ircline.FirstParam(afterFirst(ircline.Params(rest))))
	count := 0
	if countStr != "" {
		n, err := strconv.Atoi(countStr)
		if err != nil || n < 0 {
			n = 0
		}
		count = n
	}
	if count > c.srv.cfg.MaxBufferLines {
		// ZNC refuses above GetMaxBufferSize and says what the cap is.
		return c.status("Setting buffer size failed for ",
			itoa(uint64(len(c.srv.matchWindows(network, pattern)))), " buffers") &&
			c.status("Maximum buffer size is ", itoa(uint64(c.srv.cfg.MaxBufferLines)), " lines")
	}

	names := c.srv.matchWindows(network, pattern)
	failed := 0
	for _, name := range names {
		if !c.srv.ResizeWindow(network, name, count) {
			failed++
		}
	}
	if failed > 0 {
		if !c.status("Setting buffer size failed for ", itoa(uint64(failed)),
			plural(failed, " buffer", " buffers")) {
			return false
		}
		return c.status("Maximum buffer size is ", itoa(uint64(c.srv.cfg.MaxBufferLines)),
			plural(c.srv.cfg.MaxBufferLines, " line", " lines"))
	}
	return c.status("Size of every buffer was set to ", itoa(uint64(count)),
		plural(count, " line", " lines"))
}

// afterFirst returns params with its first whitespace-delimited token removed,
// so the next FirstParam reads the second argument.
func afterFirst(params []byte) []byte {
	i := 0
	for i < len(params) && params[i] == ' ' {
		i++
	}
	for i < len(params) && params[i] != ' ' {
		i++
	}
	for i < len(params) && params[i] == ' ' {
		i++
	}
	return params[i:]
}

// sendTable writes every line of a table as a STATUS notice, in order. ZNC's
// PutStatus(CTable) does exactly this (Client.cpp:718).
func (c *client) sendTable(t *table) bool {
	for _, line := range t.lines() {
		if !c.status(line) {
			return false
		}
	}
	return true
}

// splitServer splits a ZNC display-form server string ("host port" or
// "host +port") back into its parts.
func splitServer(s string) (host, port string) {
	if i := strings.LastIndexByte(s, ' '); i >= 0 {
		return s[:i], s[i+1:]
	}
	if h, p, err := net.SplitHostPort(s); err == nil {
		return h, p
	}
	return s, ""
}
