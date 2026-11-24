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

	// Only the dispatch switch: slice to the end of that function. It is dispatchControlplane
	// rather than handleControlplane because the audit trail records from the wrapper, so the
	// table lives one level down - and a guard that looked at the wrapper would see no commands
	// at all and fail for the wrong reason.
	start := strings.Index(src, "func (c *client) dispatchControlplane(")
	if start < 0 {
		t.Fatal("dispatchControlplane not found")
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

// fakeScripts is a scripting engine as far as the admin surface can tell.
type fakeScripts struct {
	list   []string
	reload int
	log    []string
}

func (f *fakeScripts) ScriptList() []string { return f.list }
func (f *fakeScripts) ScriptReload() int    { return f.reload }
func (f *fakeScripts) ScriptLog() []string  { return f.log }

// TestScriptingOffIsSaidPlainly: "no scripts" and "scripting is off" are different states, and
// an operator looking for scripts needs to know which one they are in.
func TestScriptingOffIsSaidPlainly(t *testing.T) {
	s, _, sub, _ := smallRings(t)
	out := strings.Join(notices(runWindowCmd(t, s, sub, "PRIVMSG *controlplane SCRIPTS")), "\n")
	if !strings.Contains(out, "script_dir") {
		t.Fatalf("with scripting off SCRIPTS said %q", out)
	}
}

// TestScriptErrorsAreListed: an operator whose edit broke a file has to see it from the client
// they already have open, not from a log they have to go and find.
func TestScriptErrorsAreListed(t *testing.T) {
	s, _, sub, _ := smallRings(t)
	s.scripts = &fakeScripts{list: []string{"good.star ok", "bad.star ERROR line 2"}}
	s.sdir = "/var/lib/salotto/scripts"

	out := strings.Join(notices(runWindowCmd(t, s, sub, "PRIVMSG *controlplane SCRIPTS")), "\n")
	if !strings.Contains(out, "bad.star") || !strings.Contains(out, "ERROR") {
		t.Fatalf("a broken script was not reported: %q", out)
	}
}

// TestReloadCanBeAskedFor: the timer is the hot part, this is the impatient part.
func TestReloadCanBeAskedFor(t *testing.T) {
	s, _, sub, _ := smallRings(t)
	s.scripts = &fakeScripts{reload: 3}
	out := strings.Join(notices(runWindowCmd(t, s, sub, "PRIVMSG *controlplane SCRIPTRELOAD")), "\n")
	if !strings.Contains(out, "3") {
		t.Fatalf("SCRIPTRELOAD said %q", out)
	}
}

// TestAPanicDuringReloadIsReportedRatherThanFatal: -1 is the engine's "I panicked and caught
// it" answer, and the surface has to distinguish it from "zero scripts".
func TestAPanicDuringReloadIsReportedRatherThanFatal(t *testing.T) {
	s, _, sub, _ := smallRings(t)
	s.scripts = &fakeScripts{reload: -1}
	out := strings.Join(notices(runWindowCmd(t, s, sub, "PRIVMSG *controlplane SCRIPTRELOAD")), "\n")
	if !strings.Contains(out, "panicked") {
		t.Fatalf("a caught panic was reported as %q", out)
	}
}

// fakePlugins is a plugin registry as far as the admin surface can tell.
type fakePlugins struct {
	list     []string
	reloaded []string
	failed   []string
	enabled  []string
	disabled []string
}

func (f *fakePlugins) PluginList() []string { return f.list }
func (f *fakePlugins) PluginReload() (reloaded, failed []string) {
	return f.reloaded, f.failed
}
func (f *fakePlugins) PluginEnable(name string) error {
	f.enabled = append(f.enabled, name)
	return nil
}
func (f *fakePlugins) PluginDisable(name string) error {
	f.disabled = append(f.disabled, name)
	return nil
}

// TestPluginsAreSaidPlainly: "no plugin registry" and "an empty plugin directory" are different
// states, and the operator needs to know which one they configured.
func TestPluginsAreSaidPlainly(t *testing.T) {
	s, _, sub, _ := smallRings(t)
	out := strings.Join(notices(runWindowCmd(t, s, sub, "PRIVMSG *controlplane PLUGINS")), "\n")
	if !strings.Contains(out, "plugin_dir") {
		t.Fatalf("with plugins off PLUGINS said %q", out)
	}
}

// TestPluginRowsCarryNameVersionAndState is the wire contract from the brief: one NOTICE per
// plugin, and the state is part of the row rather than a separate command.
func TestPluginRowsCarryNameVersionAndState(t *testing.T) {
	s, _, sub, _ := smallRings(t)
	s.plugins = &fakePlugins{list: []string{"highlight-words 1 enabled"}}
	out := strings.Join(notices(runWindowCmd(t, s, sub, "PRIVMSG *controlplane PLUGINS")), "\n")
	if !strings.Contains(out, "highlight-words 1 enabled") {
		t.Fatalf("PLUGINS said %q", out)
	}
}

// TestPluginReloadReportsBothSides: a reload that only lists successes hides the one thing the
// operator needs to see, which is the file that would not compile.
func TestPluginReloadReportsBothSides(t *testing.T) {
	s, _, sub, _ := smallRings(t)
	s.plugins = &fakePlugins{reloaded: []string{"good"}, failed: []string{"broken: line 2"}}
	out := strings.Join(notices(runWindowCmd(t, s, sub, "PRIVMSG *controlplane PLUGIN RELOAD")), "\n")
	if !strings.Contains(out, "reloaded good") || !strings.Contains(out, "failed broken: line 2") {
		t.Fatalf("PLUGIN RELOAD said %q", out)
	}
}

// TestPluginEnableAndDisableReachTheRegistry: the command is not a message about a wish, it
// has to call the registry.
func TestPluginEnableAndDisableReachTheRegistry(t *testing.T) {
	s, _, sub, _ := smallRings(t)
	f := &fakePlugins{list: []string{"p 1 enabled"}}
	s.plugins = f
	if got := strings.Join(notices(runWindowCmd(t, s, sub, "PRIVMSG *controlplane PLUGIN DISABLE p")), "\n"); !strings.Contains(got, "disabled") {
		t.Fatalf("PLUGIN DISABLE said %q", got)
	}
	if got := strings.Join(notices(runWindowCmd(t, s, sub, "PRIVMSG *controlplane PLUGIN ENABLE p")), "\n"); !strings.Contains(got, "enabled") {
		t.Fatalf("PLUGIN ENABLE said %q", got)
	}
	if len(f.disabled) != 1 || f.disabled[0] != "p" || len(f.enabled) != 1 || f.enabled[0] != "p" {
		t.Fatalf("the registry was not called: enabled=%q disabled=%q", f.enabled, f.disabled)
	}
}

// TestScriptLogIsReadable: a script's log() has to go somewhere the operator can see.
func TestScriptLogIsReadable(t *testing.T) {
	s, _, sub, _ := smallRings(t)
	s.scripts = &fakeScripts{log: []string{"mention #1", "mention #2"}}
	out := strings.Join(notices(runWindowCmd(t, s, sub, "PRIVMSG *controlplane SCRIPTLOG")), "\n")
	if !strings.Contains(out, "mention #2") {
		t.Fatalf("SCRIPTLOG said %q", out)
	}
}
