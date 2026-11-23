package facade

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// newRuntimeServer is a facade with no network at boot: the B5 case is exactly
// a server that gains and loses networks while clients are attached.
func newRuntimeServer(t *testing.T) (*Server, *Hub) {
	t.Helper()
	hub := NewHub(8)
	s, err := New(Config{
		ServerName: "salotto",
		Hub:        hub,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxClients: 8,
	})
	if err != nil {
		t.Fatalf("facade.New: %v", err)
	}
	return s, hub
}

// TestAddRemoveNetworkRoundTrip: a network added at runtime routes and replays,
// and removing it makes its name free again and forgets everything keyed under
// it, so a later network with the same name starts clean.
func TestAddRemoveNetworkRoundTrip(t *testing.T) {
	s, _ := newRuntimeServer(t)

	fn := newFakeNet("runtime")
	if err := s.AddNetwork("runtime", fn); err != nil {
		t.Fatalf("AddNetwork: %v", err)
	}
	if got := s.Networks(); len(got) != 1 || got[0] != "runtime" {
		t.Fatalf("Networks = %v, want [runtime]", got)
	}
	if err := s.AddNetwork("runtime", fn); err == nil {
		t.Fatal("duplicate AddNetwork succeeded")
	}

	fn.add("#c", ":n!u@h PRIVMSG #c :hello", 0)
	var out bytes.Buffer
	sub := NewSubscriber(&out, 64*1024)
	defer sub.Close()
	s.AttachWindow(sub, "runtime", "#c")

	if !s.PlayBuffer(sub, "runtime", "#c") {
		t.Fatal("PlayBuffer failed")
	}
	if err := sub.Flush(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "hello") {
		t.Fatalf("replay = %q, want the buffered line", out.String())
	}
	if _, has := s.WindowCursor(sub, "runtime", "#c"); !has {
		t.Fatal("replay left no cursor: the per-window state was not created")
	}

	if err := s.RemoveNetwork("runtime"); err != nil {
		t.Fatalf("RemoveNetwork: %v", err)
	}
	if got := s.Networks(); len(got) != 0 {
		t.Fatalf("Networks = %v, want empty", got)
	}
	if err := s.RemoveNetwork("runtime"); err == nil {
		t.Fatal("RemoveNetwork of an unknown network succeeded")
	}

	// A window keyed under the removed network must be gone: no cursor, not
	// known, not disabled.
	if _, has := s.WindowCursor(sub, "runtime", "#c"); has {
		t.Fatal("cursor survived the network removal")
	}
	if s.Known("runtime", "#c") {
		t.Fatal("window key survived the network removal")
	}
	if s.Disabled("runtime", "#c") {
		t.Fatal("disabled flag survived the network removal")
	}
	if s.WindowAttached(sub, "runtime", "#c") {
		t.Fatal("attachment survived the network removal")
	}

	// Re-adding the name must not inherit the removed network's state.
	fn2 := newFakeNet("runtime")
	if err := s.AddNetwork("runtime", fn2); err != nil {
		t.Fatalf("re-AddNetwork: %v", err)
	}
	if _, has := s.WindowCursor(sub, "runtime", "#c"); has {
		t.Fatal("a reused network name inherited the old cursor")
	}
	out.Reset()
	if !s.PlayBuffer(sub, "runtime", "#c") {
		t.Fatal("PlayBuffer on the re-added network failed")
	}
	_ = sub.Flush()
	if strings.Contains(out.String(), "hello") {
		t.Fatal("a reused network name replayed the old ring")
	}
}

// TestNetworksStress hammers add/remove/list concurrently. Under -race this is
// the check that the facade's network list is not read while it is written.
func TestNetworksStress(t *testing.T) {
	s, _ := newRuntimeServer(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "n" + string(rune('a'+i))
			for j := 0; j < 100; j++ {
				fn := newFakeNet(name)
				if err := s.AddNetwork(name, fn); err == nil {
					_ = s.Networks()
					_ = s.networkName()
					_ = s.RemoveNetwork(name)
				}
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 400; i++ {
			_ = s.Networks()
		}
	}()
	wg.Wait()
	if got := s.Networks(); len(got) != 0 {
		t.Fatalf("Networks = %v, want empty after the stress", got)
	}
}
