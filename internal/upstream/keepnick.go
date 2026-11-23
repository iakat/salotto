package upstream

// keepnick ports ZNC's keepnick module: while connected under a nick that is not
// the configured one, retry the configured nick on a timer. ZNC's version is a
// CTimer at 30 s (keepnick.cpp: Start(30000)); the interval is the same here so
// the behaviour is recognisable, and it stops as soon as the nick is ours.

import "time"

// keepNickInterval is ZNC's CTimer interval in keepnick.cpp.
const keepNickInterval = 30 * time.Second

// CurrentNick is the nick the server has us under. It is empty until 001.
func (c *Conn) CurrentNick() string {
	if v, ok := c.selfNick.Load().(string); ok {
		return v
	}
	return ""
}

// keepNickLoop retries the configured nick while we are someone else. It runs
// only between a successful registration and the next disconnect, and it is
// deliberately time-based: a server that refuses the nick answers 433/437, and
// hammering instantly on that reply is how a bouncer gets k-lined.
func (c *Conn) keepNickLoop(stop <-chan struct{}) {
	want := c.cfg.Nick
	if want == "" || !c.cfg.KeepNick {
		return
	}
	t := time.NewTicker(keepNickInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if c.CurrentNick() == want {
				continue
			}
			// Best effort: a full write queue means the connection is not
			// healthy, and the next tick tries again.
			_ = c.Send([]byte("NICK " + want))
		}
	}
}
