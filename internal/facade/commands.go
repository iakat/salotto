package facade

import (
	"fmt"

	"github.com/iakat/salotto/internal/ircline"
)

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
