package bindhost

import (
	"sync"

	"github.com/iakat/salotto/internal/config"
)

// State holds the user default bind host, the per-network overrides and the
// operator policy that can refuse changes. Resolution follows ZNC's
// CIRCNetwork::GetBindHost(): network value if non-empty, else the user value,
// else empty (the kernel picks the source address).
type State struct {
	mu      sync.RWMutex
	user    string
	deny    bool
	nets    map[string]string
	changed func(network, host string)
}

// New returns a state with the given user default and deny policy.
func New(user string, deny bool) *State {
	return &State{
		user: user,
		deny: deny,
		nets: make(map[string]string),
	}
}

// SetOnChange registers a callback invoked after every mutation with the
// resolved host for the affected network. The daemon uses it to repoint the
// dialer; the state never imports upstream.
func (s *State) SetOnChange(fn func(network, host string)) {
	s.mu.Lock()
	s.changed = fn
	s.mu.Unlock()
}

// Seed installs a network override from the configuration, bypassing the deny
// policy: the policy only governs the wire commands, not the config file. An
// override equal to the user default is dropped, as ZNC does.
func (s *State) Seed(network, host string) {
	s.mu.Lock()
	if host == s.user {
		s.nets[network] = ""
	} else {
		s.nets[network] = host
	}
	s.mu.Unlock()
}

// Resolve returns the address a dial for network must bind to and where it came
// from: "network", "user" or "kernel".
func (s *State) Resolve(network string) (host, origin string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return config.ResolveBindHost(s.user, s.nets[network])
}

// User returns the current user default.
func (s *State) User() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.user
}

// Network returns the current override for network, or "".
func (s *State) Network(network string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nets[network]
}

// Denied reports whether the operator policy refuses bind-host changes.
func (s *State) Denied() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.deny
}

// SetUser sets the user default. It refuses when the deny policy is on.
func (s *State) SetUser(host string) error {
	if err := config.ValidateBindHost("bind_host", host); err != nil {
		return err
	}
	s.mu.Lock()
	if s.deny {
		s.mu.Unlock()
		return errDenied
	}
	s.user = host
	names := s.names()
	changed := s.changed
	s.mu.Unlock()
	s.notify(changed, names)
	return nil
}

// ClearUser forgets the user default. It refuses when the deny policy is on.
func (s *State) ClearUser() error {
	return s.SetUser("")
}

// SetNetwork sets the override for network. As in ZNC, setting the value equal
// to the user default clears the override instead, because the network value is
// only ever an override and the two would mean the same thing.
func (s *State) SetNetwork(network, host string) error {
	if err := config.ValidateBindHost("bind_host", host); err != nil {
		return err
	}
	s.mu.Lock()
	if s.deny {
		s.mu.Unlock()
		return errDenied
	}
	if host == s.user {
		s.nets[network] = ""
	} else {
		s.nets[network] = host
	}
	resolved, _ := config.ResolveBindHost(s.user, s.nets[network])
	changed := s.changed
	s.mu.Unlock()
	if changed != nil {
		changed(network, resolved)
	}
	return nil
}

// ClearNetwork forgets the override for network. It refuses when the deny policy
// is on.
func (s *State) ClearNetwork(network string) error {
	s.mu.Lock()
	if s.deny {
		s.mu.Unlock()
		return errDenied
	}
	s.nets[network] = ""
	resolved, _ := config.ResolveBindHost(s.user, "")
	changed := s.changed
	s.mu.Unlock()
	if changed != nil {
		changed(network, resolved)
	}
	return nil
}

func (s *State) names() []string {
	names := make([]string, 0, len(s.nets))
	for name := range s.nets {
		names = append(names, name)
	}
	return names
}

func (s *State) notify(changed func(network, host string), names []string) {
	if changed == nil {
		return
	}
	for _, name := range names {
		host, _ := s.Resolve(name)
		changed(name, host)
	}
}

var errDenied = &deniedError{}

type deniedError struct{}

func (*deniedError) Error() string {
	return "bind host changes are disabled by the operator (deny_set_bind_host)"
}

// IsDenied reports whether err is the policy refusal.
func IsDenied(err error) bool {
	_, ok := err.(*deniedError)
	return ok
}
