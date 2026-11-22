package facade

import (
	"fmt"
	"strings"

	"github.com/iakat/salotto/internal/ircline"
)

// windowKind selects which known windows a clear-all command acts on.
type windowKind int

const (
	windowChannels windowKind = iota
	windowQueries
	windowAll
)

// isChannelWindow reports whether a window name is a channel. The facade
// advertises CHANTYPES=# (see sendRegistration), so a leading '#' is the whole
// rule. Everything else, including "status", is a non-channel window.
func isChannelWindow(name string) bool {
	return strings.HasPrefix(name, "#")
}

// isQueryWindow reports whether a window is a query. The server window
// "status" is a window but neither a channel nor a query.
func isQueryWindow(name string) bool {
	return name != "status" && !isChannelWindow(name)
}

func (k windowKind) includes(name string) bool {
	switch k {
	case windowChannels:
		return isChannelWindow(name)
	case windowQueries:
		return isQueryWindow(name)
	default:
		return true
	}
}

// requireNetwork returns the first network in sorted order, or false after
// answering with ZNC's "must be connected" notice.
func (c *client) requireNetwork() (string, bool) {
	network := c.commandNetwork()
	if network == "" {
		return "", c.status("You must be connected with a network to use this command")
	}
	return network, true
}

// plural picks ZNC's t_p() singular/plural form.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func (c *client) handleAttach(rest []byte) bool {
	network, ok := c.requireNetwork()
	if !ok {
		return false
	}
	arg := string(ircline.Params(rest))
	if arg == "" {
		return c.status("Usage: Attach <#chans>")
	}
	matches := c.srv.matchWindows(network, arg)
	changed := 0
	for _, window := range matches {
		if c.srv.AttachWindow(c.sub, network, window) {
			changed++
		}
	}
	if !c.status(plural(len(matches),
		fmt.Sprintf("There was %d channel matching [%s]", len(matches), arg),
		fmt.Sprintf("There were %d channels matching [%s]", len(matches), arg))) {
		return false
	}
	return c.status(plural(changed,
		fmt.Sprintf("Attached %d channel", changed),
		fmt.Sprintf("Attached %d channels", changed)))
}

func (c *client) handleDetach(rest []byte) bool {
	network, ok := c.requireNetwork()
	if !ok {
		return false
	}
	arg := string(ircline.Params(rest))
	if arg == "" {
		return c.status("Usage: Detach <#chans>")
	}
	matches := c.srv.matchWindows(network, arg)
	changed := 0
	for _, window := range matches {
		if c.srv.DetachWindow(c.sub, network, window) {
			changed++
		}
	}
	if !c.status(plural(len(matches),
		fmt.Sprintf("There was %d channel matching [%s]", len(matches), arg),
		fmt.Sprintf("There were %d channels matching [%s]", len(matches), arg))) {
		return false
	}
	return c.status(plural(changed,
		fmt.Sprintf("Detached %d channel", changed),
		fmt.Sprintf("Detached %d channels", changed)))
}

func (c *client) handlePlayBuffer(rest []byte) bool {
	network, ok := c.requireNetwork()
	if !ok {
		return false
	}
	window := string(ircline.FirstParam(ircline.Params(rest)))
	if window == "" {
		return c.status("Usage: PlayBuffer <#chan|query>")
	}

	if isChannelWindow(window) {
		if !c.srv.Known(network, window) {
			return c.status("You are not on " + window)
		}
		if c.srv.BufferEmpty(network, window) {
			return c.status("The buffer for channel " + window + " is empty")
		}
	} else {
		if !c.srv.Known(network, window) {
			return c.status("No active query with " + window)
		}
		if c.srv.BufferEmpty(network, window) {
			return c.status("The buffer for " + window + " is empty")
		}
	}

	if !c.srv.PlayBuffer(c.sub, network, window) {
		// The subscriber's out buffer filled: a slow client, not a command
		// error.
		return false
	}
	return true
}

func (c *client) handleClearBuffer(rest []byte) bool {
	network, ok := c.requireNetwork()
	if !ok {
		return false
	}
	arg := string(ircline.FirstParam(ircline.Params(rest)))
	if arg == "" {
		return c.status("Usage: ClearBuffer <#chan|query>")
	}

	matches := c.srv.matchWindows(network, arg)
	for _, window := range matches {
		_, _ = c.srv.ClearBuffer(network, window)
	}
	return c.status(plural(len(matches),
		fmt.Sprintf("%d buffer matching %s has been cleared", len(matches), arg),
		fmt.Sprintf("%d buffers matching %s have been cleared", len(matches), arg)))
}

func (c *client) handleClearAllBuffers(rest []byte, kind windowKind) bool {
	network, ok := c.requireNetwork()
	if !ok {
		return false
	}
	for _, window := range c.srv.windows.knownWindows(network) {
		if !kind.includes(window) {
			continue
		}
		_, _ = c.srv.ClearBuffer(network, window)
	}
	switch kind {
	case windowChannels:
		return c.status("All channel buffers have been cleared")
	case windowQueries:
		return c.status("All query buffers have been cleared")
	default:
		return c.status("All buffers have been cleared")
	}
}
