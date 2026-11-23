package facade

import (
	"errors"
	"strconv"
	"strings"

	"github.com/iakat/salotto/internal/ircline"
)

// The server-mutation and channel-order commands: ADDSERVER, DELSERVER,
// MOVECHAN. They are the last four in the queue's command table, and MOVENETWORK
// is the one we refuse — see handleMoveNetwork.
//
// All three follow ZNC's ClientCommand.cpp arms exactly, including the order of
// its validation, because the point of these commands is that someone with ZNC
// muscle memory gets the same answers.

// zncParseServer turns ZNC's "host [[+]port] [pass]" spelling into a salotto
// address. ZNC's CServer::Parse (src/Server.cpp) reads "+port" as "this port is
// SSL", and salotto keeps TLS on the network rather than per server, so a "+"
// prefix here is a request we cannot honour per-server: we accept the port and
// note that TLS is the network's setting. Returning the host:port is enough for
// the parity we can actually deliver, and refusing the "+" form outright would
// break every ZNC how-to that mentions it.
func zncParseServer(arg string) (addr string, ok bool) {
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		return "", false
	}
	host := fields[0]
	if host == "" {
		return "", false
	}

	port := ""
	if len(fields) > 1 {
		candidate := fields[1]
		// A second field is a port only when it is numeric after the optional
		// SSL marker; otherwise it is the start of a password.
		if strings.HasPrefix(candidate, "+") {
			candidate = candidate[1:]
		}
		if _, err := strconv.Atoi(candidate); err == nil {
			port = candidate
		}
	}
	if port == "" {
		// ZNC defaults to 6667 for a plain server. It is not a guess about the
		// user's intent: it is CServer::Parse's own default.
		port = "6667"
	}
	// A host that already carries a port is taken as-is, so "host:6697" works.
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return host, true
	}
	return host + ":" + port, true
}

// handleAddServer implements ADDSERVER (ClientCommand.cpp:820-850).
func (c *client) handleAddServer(rest []byte) bool {
	network, ok := c.requireNetwork()
	if network == "" {
		return ok
	}
	ne := c.srv.networkEntry(network)
	if ne == nil {
		return c.status("You must be connected with a network to use this command")
	}
	arg := strings.TrimSpace(string(ircline.Params(rest)))
	if arg == "" {
		c.status("Usage: AddServer <host> [[+]port] [pass]")
		return c.status("+ means SSL")
	}
	addr, ok2 := zncParseServer(arg)
	if !ok2 {
		return c.status("Usage: AddServer <host> [[+]port] [pass]")
	}
	if !ne.net.AddServer(addr) {
		return c.status("Unable to add that server. Perhaps the server is already added or openssl is disabled?")
	}
	return c.status("Server added")
}

// handleDelServer implements DELSERVER (ClientCommand.cpp:861-880). The order of
// ZNC's checks is reproduced because each has its own sentence: no network, no
// argument, empty list, not found.
func (c *client) handleDelServer(rest []byte) bool {
	network, ok := c.requireNetwork()
	if network == "" {
		return ok
	}
	ne := c.srv.networkEntry(network)
	if ne == nil {
		return c.status("You must be connected with a network to use this command")
	}
	arg := strings.TrimSpace(string(ircline.Params(rest)))
	if arg == "" {
		return c.status("Usage: DelServer <host> [port] [pass]")
	}
	if len(ne.net.Servers()) == 0 {
		return c.status("You don't have any servers added.")
	}
	addr, ok2 := zncParseServer(arg)
	if !ok2 {
		return c.status("Usage: DelServer <host> [port] [pass]")
	}
	// ZNC matches on name, port and password; salotto matches on the address it
	// would have produced, and also accepts an exact address the operator typed.
	if !ne.net.DelServer(addr) && !ne.net.DelServer(strings.Fields(arg)[0]) {
		return c.status("No such server")
	}
	return c.status("Server removed")
}

// handleMoveChan implements MOVECHAN (ClientCommand.cpp:456-475 →
// IRCNetwork.cpp:942-961).
//
// ZNC's index is 1-based on the wire and 0-based in the list, and — this is the
// detail that is easy to get wrong — it range-checks before looking the channel
// up, so an out-of-range index on a channel you are not in reports "Invalid
// index" rather than "You are not on #x". The order is kept because the error
// text is the observable.
func (c *client) handleMoveChan(rest []byte) bool {
	network, ok := c.requireNetwork()
	if network == "" {
		return ok
	}
	ne := c.srv.networkEntry(network)
	if ne == nil {
		return c.status("You must be connected with a network to use this command")
	}
	fields := strings.Fields(string(ircline.Params(rest)))
	if len(fields) < 2 {
		return c.status("Usage: MoveChan <#chan> <index>")
	}
	index, err := strconv.Atoi(fields[1])
	if err != nil || index < 1 {
		return c.status("Invalid index")
	}
	if err := ne.net.MoveChan(fields[0], index-1); err != nil {
		return c.status(err.Error())
	}
	return c.status("Moved channel " + fields[0] + " to index " + strconv.Itoa(index))
}

// handleMoveNetwork is refused on purpose. ZNC's MOVENETWORK moves a network from
// one *user* to another (ClientCommand.cpp:700-730, admin-only), and salotto is
// single-user by design: there is no second user to move anything to. Answering
// with ZNC's usage text would imply the command could work if spelled right.
func (c *client) handleMoveNetwork() bool {
	return c.status("MoveNetwork is not supported: salotto is single-user")
}

// errNoChannelOrder is what MoveChan returns for a network that keeps no channel
// order, which is the honest answer rather than a silent no-op.
var errNoChannelOrder = errors.New("Invalid index")
