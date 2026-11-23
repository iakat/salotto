package facade

import (
	"fmt"

	"github.com/iakat/salotto/internal/ircline"
)

// fpOp selects which of the three B14 commands is being run: ZNC has them as
// three tokens in one chain (ClientCommand.cpp:918-960) with identical
// preconditions and different bodies.
type fpOp int

const (
	fpAdd fpOp = iota
	fpDel
	fpList
)

func (c *client) handleTrustedFingerprint(rest []byte, op fpOp) bool {
	network, ok := c.requireNetwork()
	if network == "" {
		return ok
	}

	switch op {
	case fpList:
		pins := c.srv.ListTrustedServerFingerprints(network)
		if len(pins) == 0 {
			return c.status("No fingerprints added.")
		}
		for i, fp := range pins {
			if !c.status(fmt.Sprintf("%d. %s", i+1, fp)) {
				return false
			}
		}
		return true
	}

	verb := "AddTrustedServerFingerprint"
	if op == fpDel {
		verb = "DelTrustedServerFingerprint"
	}
	arg := string(ircline.FirstParam(ircline.Params(rest)))
	if arg == "" {
		return c.status("Usage: " + verb + " <fi:ng:er>")
	}

	if op == fpAdd {
		if !c.srv.AddTrustedServerFingerprint(network, arg) {
			// A pin that is not a SHA-256 digest would silently never match,
			// which is worse than refusing it: the operator would believe the
			// server is pinned.
			return c.status("Not a SHA-256 fingerprint: " + arg)
		}
		return c.status("Done.")
	}
	if !c.srv.DelTrustedServerFingerprint(network, arg) {
		return c.status("No such fingerprint: " + arg)
	}
	return c.status("Done.")
}
