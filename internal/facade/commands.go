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

func (c *client) handleDisableChan(rest []byte, disable bool) bool {
	network, ok := c.requireNetwork()
	if !ok {
		return false
	}
	verb := "DisableChan"
	if !disable {
		verb = "EnableChan"
	}
	arg := string(ircline.Params(rest))
	if arg == "" {
		return c.status("Usage: " + verb + " <#chans>")
	}

	matches := c.srv.matchWindows(network, arg)
	changed := 0
	for _, window := range matches {
		if c.srv.SetDisabled(network, window, disable) {
			changed++
		}
	}
	if !c.status(plural(len(matches),
		fmt.Sprintf("There was %d channel matching [%s]", len(matches), arg),
		fmt.Sprintf("There were %d channels matching [%s]", len(matches), arg))) {
		return false
	}
	if disable {
		return c.status(plural(changed,
			fmt.Sprintf("Disabled %d channel", changed),
			fmt.Sprintf("Disabled %d channels", changed)))
	}
	return c.status(plural(changed,
		fmt.Sprintf("Enabled %d channel", changed),
		fmt.Sprintf("Enabled %d channels", changed)))
}

// handleListChans prints one row per known window plus the summary ZNC prints.
// "status" is reported: it is a window, but it is neither a channel nor a
// query. The summary counts each window once, by the Status column it displays
// (ZNC's own counters overlap when a channel is both detached and disabled).
func (c *client) handleListChans() bool {
	network, ok := c.requireNetwork()
	if !ok {
		return false
	}
	names := c.srv.windows.knownWindows(network)
	if len(names) == 0 {
		return c.status("There are no channels defined.")
	}

	headers := [2]string{"Name", "Status"}
	width := [2]int{len(headers[0]), len(headers[1])}
	rows := make([][2]string, 0, len(names))
	joined, detached, disabled := 0, 0, 0
	for _, name := range names {
		var status string
		switch {
		case c.srv.Disabled(network, name):
			status = "Disabled"
			disabled++
		case c.srv.WindowAttached(c.sub, network, name):
			status = "Joined"
			joined++
		default:
			status = "Detached"
			detached++
		}
		row := [2]string{name, status}
		for i := range row {
			if len(row[i]) > width[i] {
				width[i] = len(row[i])
			}
		}
		rows = append(rows, row)
	}

	sep := "+-" + strings.Repeat("-", width[0]) + "-+-" + strings.Repeat("-", width[1]) + "-+"
	line := func(a, b string) string {
		return "| " + a + strings.Repeat(" ", width[0]-len(a)) +
			" | " + b + strings.Repeat(" ", width[1]-len(b)) + " |"
	}

	if !c.status(sep) || !c.status(line(headers[0], headers[1])) || !c.status(sep) {
		return false
	}
	for _, row := range rows {
		if !c.status(line(row[0], row[1])) {
			return false
		}
	}
	if !c.status(sep) {
		return false
	}
	return c.status(fmt.Sprintf("Total: %d, Joined: %d, Detached: %d, Disabled: %d",
		len(names), joined, detached, disabled))
}
