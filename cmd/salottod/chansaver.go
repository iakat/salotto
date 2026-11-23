package main

import (
	"errors"
	"strings"
	"sync"

	"github.com/iakat/salotto/internal/ircline"
)

// channelSet is ZNC's chansaver: the channels this account wants joined,
// updated by watching what we actually do on IRC rather than by hand-editing
// the config.
//
// ZNC's module keeps a per-channel `InConfig` flag and flips it on our own JOIN
// and PART (modules/chansaver.cpp:69-92). The seed is whatever the config says,
// so a fresh start behaves exactly like the config-only bouncer we had before;
// the difference is that from then on the list follows reality.
//
// This is in memory only. Persisting it is the admin surface's job (§2.0.2),
// which is also the thing that may write the config — the same boundary ZNC
// draws between a module flipping a flag and CZNC::WriteConfig.
type channelSet struct {
	mu    sync.Mutex
	set   map[string]struct{}
	order []string
	seen  bool
}

func newChannelSet(configured []string) *channelSet {
	c := &channelSet{set: make(map[string]struct{}, len(configured))}
	for _, ch := range configured {
		if key := channelKey(ch); key != "" {
			if _, dup := c.set[key]; dup {
				continue
			}
			c.set[key] = struct{}{}
			c.order = append(c.order, key)
		}
	}
	return c
}

// channelKey normalises a channel name for the set. RFC1459 casemapping makes
// #Chan and #chan the same channel, which is the casemapping the facade
// advertises in 005.
func channelKey(name string) string {
	if name == "" {
		return ""
	}
	return strings.ToLower(name)
}

// List returns the channels to join, in the order they are kept, so reconnect
// order is stable and MOVECHAN means something.
func (c *channelSet) List() []string {
	c.mu.Lock()
	out := make([]string, len(c.order))
	copy(out, c.order)
	c.mu.Unlock()
	return out
}

// MoveChan moves a channel to a 0-based index, which is ZNC's
// IRCNetwork::MoveChan (IRCNetwork.cpp:942-961). ZNC range-checks the index
// before looking the channel up, so an out-of-range index reports "Invalid index"
// even when the channel is not in the list; that order is reproduced because the
// error text is the observable.
func (c *channelSet) MoveChan(channel string, index int) error {
	key := channelKey(channel)
	c.mu.Lock()
	defer c.mu.Unlock()
	if index < 0 || index >= len(c.order) {
		return errors.New("Invalid index")
	}
	at := -1
	for i, name := range c.order {
		if name == key {
			at = i
			break
		}
	}
	if at < 0 {
		return errors.New("You are not on " + channel)
	}
	name := c.order[at]
	c.order = append(c.order[:at], c.order[at+1:]...)
	c.order = append(c.order, "")
	copy(c.order[index+1:], c.order[index:])
	c.order[index] = name
	return nil
}

// Has reports whether a channel is in the set.
func (c *channelSet) Has(name string) bool {
	c.mu.Lock()
	_, ok := c.set[channelKey(name)]
	c.mu.Unlock()
	return ok
}

// Add records a channel we joined.
func (c *channelSet) Add(name string) {
	key := channelKey(name)
	if key == "" {
		return
	}
	c.mu.Lock()
	if _, exists := c.set[key]; !exists {
		c.set[key] = struct{}{}
		c.order = append(c.order, key)
	}
	c.mu.Unlock()
}

// Remove forgets a channel we left.
func (c *channelSet) Remove(name string) {
	key := channelKey(name)
	if key == "" {
		return
	}
	c.mu.Lock()
	delete(c.set, key)
	for i, name := range c.order {
		if name == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
	c.mu.Unlock()
}

// noteSelfJoinPart applies one inbound line to the set, and reports whether it
// changed anything. Only lines whose prefix is our own nick count, which is the
// NickEquals check chansaver does (modules/chansaver.cpp:74).
//
// JOIN and PART only. chansaver has no kick handler, and that omission is
// deliberate on ZNC's part rather than an oversight: being kicked out of a
// channel is handled by the disable flag (B12, IRCSock.cpp:734), which keeps the
// channel in the list but stops the rejoin. A separate module, kickrejoin,
// exists upstream for the opposite policy. Copying ZNC here means the two
// mechanisms stay distinct: "we are not in it right now" and "do not rejoin"
// answer different questions.
func (c *channelSet) noteSelfJoinPart(line []byte, ownNick string) bool {
	if ownNick == "" {
		return false
	}
	return c.noteSelfJoinPartAny(line, []string{ownNick})
}

// noteSelfJoinPartAny is noteSelfJoinPart against every nick we may be holding.
//
// The single-nick form is wrong whenever the nick moves between the JOIN we send and
// the JOIN the server echoes back: with two bouncers on one IRCd they trade the nick,
// so the line can arrive as `:e2e` while we believe we are `e2e_`. The result was a
// chansaver that recorded nothing and never re-joined anything, while each individual
// piece looked correct.
func (c *channelSet) noteSelfJoinPartAny(line []byte, ownNicks []string) bool {
	if len(ownNicks) == 0 {
		return false
	}
	_, rest := ircline.Tags(line)
	prefix, rest := ircline.Prefix(rest)
	from := ircline.Nick(prefix)
	if from == "" {
		return false
	}
	mine := false
	for _, n := range ownNicks {
		if strings.EqualFold(from, n) {
			mine = true
			break
		}
	}
	if !mine {
		return false
	}

	switch {
	case ircline.IsVerb(rest, "JOIN"):
		// With extended-join the channel is the first param; with a plain JOIN
		// the param may carry a key, but a key is never present on our own
		// rejoin and the channel is still the first token.
		params := ircline.FirstParam(ircline.Params(rest))
		name := strings.TrimPrefix(string(params), ":")
		if name == "" {
			return false
		}
		if c.Has(name) {
			return false
		}
		c.Add(name)
		return true

	case ircline.IsVerb(rest, "PART"):
		name := string(ircline.FirstParam(ircline.Params(rest)))
		if name == "" || !c.Has(name) {
			return false
		}
		c.Remove(name)
		return true
	}
	return false
}
