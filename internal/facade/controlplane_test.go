package facade

import (
	"os"
	"strings"
	"testing"
)

// TestControlplaneIsItsOwnNamespace is the whole design decision, stated as a test.
//
// *status is the client talking to its bouncer about itself; *controlplane is the operator
// talking about the process. Keeping them apart is what gives an access check somewhere to go
// later - a single namespace with a per-command flag is how an admin command ends up reachable
// because someone forgot the flag.
func TestControlplaneIsItsOwnNamespace(t *testing.T) {
	s, _, sub, _ := smallRings(t)

	// An admin command must not be answerable through the client namespace.
	for _, line := range notices(runWindowCmd(t, s, sub, "RUNTIME")) {
		if strings.Contains(line, "goroutines") {
			t.Fatalf("RUNTIME answered through *status, so the namespaces are not separate: %q", line)
		}
	}
}

// TestControlplaneAnswersItsOwnCommands: the namespace has to actually work, or the split is
// a comment rather than a design.
func TestControlplaneAnswersItsOwnCommands(t *testing.T) {
	s, _, sub, _ := smallRings(t)

	version := strings.Join(notices(runWindowCmd(t, s, sub, "PRIVMSG *controlplane VERSION")), "\n")
	if !strings.Contains(version, "salotto") {
		t.Fatalf("controlplane VERSION said %q", version)
	}

	runtimeLines := strings.Join(notices(runWindowCmd(t, s, sub, "PRIVMSG *controlplane RUNTIME")), "\n")
	if !strings.Contains(runtimeLines, "goroutines") {
		t.Fatalf("controlplane RUNTIME said %q", runtimeLines)
	}

	users := strings.Join(notices(runWindowCmd(t, s, sub, "PRIVMSG *controlplane USERS")), "\n")
	if !strings.Contains(users, "one user") {
		t.Fatalf("controlplane USERS said %q", users)
	}
}

// TestAdminHelpListsEveryAdminCommand is the same drift guard as TestEveryCommandIsInTheTable,
// for the second namespace. Two hand-written lists would drift twice as fast.
func TestAdminHelpListsEveryAdminCommand(t *testing.T) {
	s, _, sub, _ := smallRings(t)
	out := strings.Join(notices(runWindowCmd(t, s, sub, "PRIVMSG *controlplane ADMINHELP")), "\n")

	for _, cmd := range controlplaneCommands {
		if cmd.name == "ADMINHELP" {
			continue
		}
		if !strings.Contains(out, cmd.name) {
			t.Fatalf("ADMINHELP does not list %s:\n%s", cmd.name, out)
		}
	}
}

// TestTheAdminTablesMatchTheDispatch walks controlplane.go's dispatch the same way the *status
// test walks server.go: a command implemented without documentation is a command nobody can
// find, and a documented command that does not exist is a lie in the help output.
func TestTheAdminTablesMatchTheDispatch(t *testing.T) {
	data, err := os.ReadFile("controlplane.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)

	// Only the handleControlplane switch: slice to the end of that function.
	start := strings.Index(src, "func (c *client) handleControlplane(")
	if start < 0 {
		t.Fatal("handleControlplane not found")
	}
	body := src[start:]
	if end := strings.Index(body[1:], "\nfunc "); end >= 0 {
		body = body[:end+1]
	}

	dispatched := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "case \"") {
			continue
		}
		rest := strings.TrimPrefix(line, "case ")
		parts := strings.Split(rest, ":")
		for _, p := range strings.Split(parts[0], ",") {
			name := strings.Trim(strings.TrimSpace(p), "\"")
			if name != "" && name != "HELP" {
				dispatched[name] = true
			}
		}
	}

	documented := map[string]bool{}
	for _, cmd := range controlplaneCommands {
		documented[cmd.name] = true
	}

	for name := range dispatched {
		if !documented[name] {
			t.Errorf("controlplane command %s is dispatched but not in the admin help table", name)
		}
	}
	for name := range documented {
		if name == "ADMINHELP" {
			continue
		}
		if !dispatched[name] {
			t.Errorf("controlplane command %s is documented but not dispatched", name)
		}
	}
}

// TestShutdownRefusesWithoutAuth is the one command that has to stay unimplemented, and the
// refusal is the feature.
//
// A shutdown command on a surface with no authentication is a remote off switch with a
// friendly name. Saying so is the honest state; silently doing nothing would look like a bug,
// and implementing it would be worse than a bug.
func TestShutdownRefusesWithoutAuth(t *testing.T) {
	s, _, sub, _ := smallRings(t)
	out := strings.Join(notices(runWindowCmd(t, s, sub, "PRIVMSG *controlplane SHUTDOWN")), "\n")
	if !strings.Contains(strings.ToLower(out), "refused") {
		t.Fatalf("SHUTDOWN did not refuse: %q", out)
	}
	if !strings.Contains(strings.ToLower(out), "auth") {
		t.Fatalf("SHUTDOWN refused without saying why: %q", out)
	}
}

// TestConfigNeverPrintsSecrets. An admin surface that prints a password is a password leak
// with a command name; the operator who needs the value can read the file.
func TestConfigNeverPrintsSecrets(t *testing.T) {
	s, _, sub, _ := smallRings(t)
	out := strings.ToLower(strings.Join(notices(runWindowCmd(t, s, sub, "PRIVMSG *controlplane CONFIG")), "\n"))
	if strings.Contains(out, "password") || strings.Contains(out, "saslpass") {
		t.Fatalf("CONFIG printed a credential: %q", out)
	}
}
