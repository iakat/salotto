package upstream

import (
	"bufio"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iakat/salotto/internal/winring"
)

// TestJoinChannelsSuppressesDisabledOnReconnect proves the one effect ZNC's
// disabled flag has: IRCNetwork.cpp:998 checks IsDisabled before joining a
// channel on a (re)connect, and nothing else in ZNC consults it for buffering.
// The callback is what makes that possible here: the daemon owns the flag, the
// dialling reads it.
func TestJoinChannelsSuppressesDisabledOnReconnect(t *testing.T) {
	ln := listen(t)
	defer ln.Close()

	var mu sync.Mutex
	disabled := map[string]bool{}

	w := winring.New(8, 8, 4096)
	cfg := Config{
		Name: "test",
		Addr: ln.Addr().String(),
		Nick: "nick",
		// The callback is consulted at every connect, so flipping the map
		// between the two dials is exactly what DisableChan does at runtime.
		Channels: []string{"#a", "#b"},
		JoinChannels: func() []string {
			mu.Lock()
			defer mu.Unlock()
			out := make([]string, 0, 2)
			for _, ch := range []string{"#a", "#b"} {
				if !disabled[ch] {
					out = append(out, ch)
				}
			}
			return out
		},
		Windows:    w,
		Route:      routeStatus,
		BackoffMin: 10 * time.Millisecond,
		BackoffMax: 20 * time.Millisecond,
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go c.Run(ctx)

	first := accept(t, ln)
	br := bufio.NewReader(first)
	serverRegister(t, first, br, "multi-prefix", "", "nick", "USER nick 0 * :")
	expectLine(t, first, br, "JOIN #a")
	expectLine(t, first, br, "JOIN #b")

	// Disable #b, then drop the connection: the reconnect must join #a only.
	mu.Lock()
	disabled["#b"] = true
	mu.Unlock()
	first.Close()

	second := accept(t, ln)
	defer second.Close()
	br2 := bufio.NewReader(second)
	serverRegister(t, second, br2, "multi-prefix", "", "nick", "USER nick 0 * :")
	expectLine(t, second, br2, "JOIN #a")

	// Nothing else may arrive: a second JOIN would be the bug. The read
	// deadline is how this test states "no more joins".
	second.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if line, err := br2.ReadString('\n'); err == nil {
		if strings.HasPrefix(strings.TrimSpace(line), "JOIN") {
			t.Fatalf("reconnect joined a disabled channel: %q", line)
		}
	}

	// Re-enabling puts it back on the next dial.
	mu.Lock()
	disabled["#b"] = false
	mu.Unlock()
	second.Close()

	third := accept(t, ln)
	defer third.Close()
	br3 := bufio.NewReader(third)
	serverRegister(t, third, br3, "multi-prefix", "", "nick", "USER nick 0 * :")
	expectLine(t, third, br3, "JOIN #a")
	expectLine(t, third, br3, "JOIN #b")
}
