package main

import (
	"testing"

	"github.com/iakat/salotto/internal/facade"
)

// nickedNet is the fake upstream plus the one method the facade uses to learn
// our own nick, which is what noteSelfKick compares a KICK target against.
type nickedNet struct {
	*fakeNet
	nick string
}

func (n *nickedNet) Nick() string { return n.nick }

func newKickServer(t *testing.T, nick string) *facade.Server {
	t.Helper()
	fn := &nickedNet{fakeNet: newFakeNet("probe"), nick: nick}
	net := facade.Network(fn)
	s, err := facade.New(facade.Config{
		ServerName: "salotto",
		Network:    "ProbeNet",
		Networks:   map[string]*facade.Network{"probe": &net},
	})
	if err != nil {
		t.Fatalf("facade.New: %v", err)
	}
	if got := s.KnownNick("probe"); got != nick {
		t.Fatalf("KnownNick = %q, want %q", got, nick)
	}
	return s
}

// A kick of our own nick disables the channel, matching ZNC (IRCSock.cpp:734):
// the operator was removed on purpose, so the bouncer must not re-join it.
func TestNoteSelfKickDisablesChannel(t *testing.T) {
	s := newKickServer(t, "mynick")

	noteSelfKick(s, "probe", []byte(":op!o@h KICK #chan other :bye"))
	if s.Disabled("probe", "#chan") {
		t.Fatal("a kick of another nick disabled the channel")
	}

	noteSelfKick(s, "probe", []byte(":op!o@h KICK #chan mynick :bye"))
	if !s.Disabled("probe", "#chan") {
		t.Fatal("a kick of our own nick did not disable the channel")
	}

	// A case-only difference still refers to us.
	s2 := newKickServer(t, "MyNick")
	noteSelfKick(s2, "probe", []byte(":op!o@h KICK #other mynick :bye"))
	if !s2.Disabled("probe", "#other") {
		t.Fatal("case-insensitive self kick did not disable the channel")
	}
}

// joinableChannels is what the dialling consults: disabled channels are dropped,
// everything else keeps its order.
func TestJoinableChannelsSkipsDisabled(t *testing.T) {
	s := newKickServer(t, "mynick")
	s.SetDisabled("probe", "#b", true)
	got := joinableChannels([]string{"#a", "#b", "#c"}, s, "probe")
	if len(got) != 2 || got[0] != "#a" || got[1] != "#c" {
		t.Fatalf("joinableChannels = %v, want [#a #c]", got)
	}
	s.SetDisabled("probe", "#b", false)
	if got := joinableChannels([]string{"#a", "#b"}, s, "probe"); len(got) != 2 {
		t.Fatalf("after enable joinableChannels = %v, want both", got)
	}
}
