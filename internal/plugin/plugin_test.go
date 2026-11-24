package plugin

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// logSink is the operator's log as far as the registry can tell. Tests assert that a refusal
// was said out loud, because a plugin that silently does nothing is the failure this package
// exists to prevent.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (s *logSink) add(line string) {
	s.mu.Lock()
	s.lines = append(s.lines, line)
	s.mu.Unlock()
}

func (s *logSink) all() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.lines, "\n")
}

// source renders a plugin.star with a manifest and the hook code. The manifest is written in
// the same file as the code, which is the property that keeps it from going stale.
func source(name, version string, hooks, commands []string, code string) string {
	return fmt.Sprintf("PLUGIN = {\n    \"name\": %q,\n    \"version\": %q,\n    \"description\": \"test plugin\",\n    \"hooks\": %s,\n    \"commands\": %s,\n}\n%s",
		name, version, starlarkList(hooks), starlarkList(commands), code)
}

func starlarkList(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = fmt.Sprintf("%q", v)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// writePlugin creates <root>/<name>/plugin.star.
func writePlugin(t *testing.T, root, name, body string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifestFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func newRegistry(t *testing.T, root string) (*Registry, *logSink) {
	t.Helper()
	sink := &logSink{}
	r := New(Config{Dir: root, Log: sink.add})
	if r == nil {
		t.Fatal("New returned nil for a configured directory")
	}
	return r, sink
}

// TestAPluginMustDeclareHooksItImplements: the declared hook is the plugin's promise to the
// daemon, and a promise the file does not keep is refused at load. The alternative - loading it
// and never calling it - is invisible at runtime: the operator sees a loaded plugin and asks
// why it does nothing, with no error to read anywhere.
func TestAPluginMustDeclareHooksItImplements(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "liar", source("liar", "1", []string{"on_line"}, nil,
		"def on_load(ctx):\n    return None\n"))

	r, _ := newRegistry(t, root)
	list := strings.Join(r.PluginList(), "\n")
	if !strings.Contains(list, "liar refused") {
		t.Fatalf("a plugin declaring on_line without implementing it was loaded:\n%s", list)
	}
	if !strings.Contains(list, "does not implement on_line") {
		t.Fatalf("the refusal did not name the missing hook:\n%s", list)
	}

	line := []byte(":alice!a@b PRIVMSG #chan :hello")
	got, keep := r.OnLine("libera", line)
	if !keep || !bytes.Equal(got, line) {
		t.Fatalf("a refused plugin ran anyway: keep=%v line=%q", keep, got)
	}
}

// TestABadManifestRefusesTheWholePlugin pins all three shape checks from the brief. The point
// is "the whole plugin": a manifest that names the wrong directory must not half-load, because
// a plugin the admin list calls by one name while its file believes another is worse than no
// plugin at all.
func TestABadManifestRefusesTheWholePlugin(t *testing.T) {
	cases := []struct {
		name string
		dir  string
		body string
		want string
	}{
		{
			name: "name does not match the directory",
			dir:  "right",
			body: source("wrong", "1", nil, nil, ""),
			want: "does not match its directory",
		},
		{
			name: "empty version",
			dir:  "empty-version",
			body: source("empty-version", "", nil, nil, ""),
			want: "version must not be empty",
		},
		{
			name: "command without a slash",
			dir:  "bad-command",
			body: source("bad-command", "1", nil, []string{"hl"}, ""),
			want: "must start with /",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writePlugin(t, root, tc.dir, tc.body)
			r, _ := newRegistry(t, root)
			_, failed := r.PluginReload()
			if len(failed) == 0 {
				t.Fatalf("the plugin was accepted despite %s", tc.name)
			}
			joined := strings.Join(failed, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("failure said %q, want it to contain %q", joined, tc.want)
			}
			if rows := r.PluginList(); len(rows) == 0 || !strings.Contains(strings.Join(rows, "\n"), "refused") {
				t.Fatalf("a refused plugin vanished from the list: %q", rows)
			}
		})
	}
}

// TestOnLineCanOverrideText: the dictionary is the hook's whole purpose. If it does not reach
// the line the daemon relays, a plugin can only decorate by side effect, which is not the API
// the brief describes.
func TestOnLineCanOverrideText(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "words", source("words", "1", []string{"on_line"}, nil,
		"def on_line(line):\n    return {\"text\": \"rewritten\"}\n"))

	r, _ := newRegistry(t, root)
	line := []byte(":alice!a@b PRIVMSG #chan :hello")
	got, keep := r.OnLine("libera", line)
	if !keep {
		t.Fatal("a text override dropped the line")
	}
	want := ":alice!a@b PRIVMSG #chan :rewritten"
	if string(got) != want {
		t.Fatalf("relayed line = %q, want %q", got, want)
	}
}

// TestAnUnknownOverrideKeyIsRefusedNotIgnored: a typo must be an error. `continue` on an
// unknown key makes the plugin look loaded and working while doing nothing, which is the exact
// silent failure the manifest validation also exists to prevent.
func TestAnUnknownOverrideKeyIsRefusedNotIgnored(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "typo", source("typo", "1", []string{"on_line"}, nil,
		"def on_line(line):\n    return {\"txt\": \"oops\"}\n"))

	r, sink := newRegistry(t, root)
	line := []byte(":alice!a@b PRIVMSG #chan :hello")
	got, keep := r.OnLine("libera", line)
	if !keep || !bytes.Equal(got, line) {
		t.Fatalf("an unknown override was partly applied: keep=%v line=%q", keep, got)
	}
	logged := sink.all()
	if !strings.Contains(logged, "unknown override") || !strings.Contains(logged, "txt") {
		t.Fatalf("the unknown key was not reported; log was %q", logged)
	}
}

// TestOneBadPluginDoesNotSkipTheOthers: the first plugin fails, the second must still run. An
// error escaping the loop is a bug in one plugin that silently disables every plugin after it,
// and the sort order makes that deterministic rather than intermittent.
func TestOneBadPluginDoesNotSkipTheOthers(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "a-bad", source("a-bad", "1", []string{"on_line"}, nil,
		"def on_line(line):\n    fail(\"deliberate\")\n"))
	writePlugin(t, root, "b-good", source("b-good", "1", []string{"on_line"}, nil,
		"def on_line(line):\n    return {\"text\": \"good\"}\n"))

	r, sink := newRegistry(t, root)
	got, keep := r.OnLine("libera", []byte(":alice!a@b PRIVMSG #chan :hello"))
	if !keep || !strings.Contains(string(got), ":good") {
		t.Fatalf("the good plugin did not run after the bad one: keep=%v line=%q", keep, got)
	}
	if logged := sink.all(); !strings.Contains(logged, "a-bad") || !strings.Contains(logged, "deliberate") {
		t.Fatalf("the failing plugin was not reported; log was %q", logged)
	}
}

// TestAReloadOfABrokenFileKeepsTheOldVersion is the whole value of hot reload stated as a test:
// an operator saving a half-finished edit must not take a working plugin offline. The failed
// build never reaches the snapshot, and the previous compiled value keeps serving.
func TestAReloadOfABrokenFileKeepsTheOldVersion(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "live", source("live", "1", []string{"on_line"}, nil,
		"def on_line(line):\n    return {\"text\": \"one\"}\n"))

	r, _ := newRegistry(t, root)
	line := []byte(":alice!a@b PRIVMSG #chan :hello")
	got, _ := r.OnLine("libera", line)
	if !strings.Contains(string(got), ":one") {
		t.Fatalf("setup: first version did not run: %q", got)
	}

	// A syntax error: the file is valid Starlark up to the point it stops being Starlark.
	writePlugin(t, root, "live", source("live", "1", []string{"on_line"}, nil,
		"def on_line(line)\n    return {\"text\": \"two\"}\n"))
	reloaded, failed := r.PluginReload()
	if len(reloaded) != 0 || len(failed) != 1 {
		t.Fatalf("broken reload reported reloaded=%q failed=%q", reloaded, failed)
	}
	got, keep := r.OnLine("libera", line)
	if !keep || !strings.Contains(string(got), ":one") {
		t.Fatalf("a broken edit took the old version offline: keep=%v line=%q", keep, got)
	}

	writePlugin(t, root, "live", source("live", "2", []string{"on_line"}, nil,
		"def on_line(line):\n    return {\"text\": \"two\"}\n"))
	reloaded, failed = r.PluginReload()
	if len(reloaded) != 1 || reloaded[0] != "live" || len(failed) != 0 {
		t.Fatalf("fixed reload reported reloaded=%q failed=%q", reloaded, failed)
	}
	got, _ = r.OnLine("libera", line)
	if !strings.Contains(string(got), ":two") {
		t.Fatalf("the fixed version did not take over: %q", got)
	}
}

// TestPluginsRunInNameOrder: two plugins editing the same field make order observable, and a
// map iteration would make the result change between runs. Deterministic order is the only
// thing that makes a chain of overrides explainable.
func TestPluginsRunInNameOrder(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "a-first", source("a-first", "1", []string{"on_line"}, nil,
		"def on_line(line):\n    return {\"text\": line + \"a\"}\n"))
	writePlugin(t, root, "b-second", source("b-second", "1", []string{"on_line"}, nil,
		"def on_line(line):\n    return {\"text\": line + \"b\"}\n"))

	r, _ := newRegistry(t, root)
	for i := 0; i < 20; i++ {
		got, _ := r.OnLine("libera", []byte(":alice!a@b PRIVMSG #chan :x"))
		if !bytes.HasSuffix(got, []byte("xab")) {
			t.Fatalf("run %d ended %q, want the a b order", i, got)
		}
	}
}

// TestAPluginCannotMutateDaemonState: state is per plugin, so one plugin writing to it cannot
// change what the next plugin sees. The alternative - a shared mutable dict - lets a plugin
// corrupt a neighbour, and the daemon has no way to tell a decoration from a mutation.
func TestAPluginCannotMutateDaemonState(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "a-writer", source("a-writer", "1", []string{"on_line"}, nil,
		"def on_line(line):\n    state[\"poison\"] = \"yes\"\n    return None\n"))
	writePlugin(t, root, "b-reader", source("b-reader", "1", []string{"on_line"}, nil,
		"def on_line(line):\n    return {\"text\": str(state.get(\"poison\", \"clean\"))}\n"))

	r, _ := newRegistry(t, root)
	got, _ := r.OnLine("libera", []byte(":alice!a@b PRIVMSG #chan :hello"))
	if !strings.Contains(string(got), ":clean") {
		t.Fatalf("one plugin's state reached another: %q", got)
	}
}

// TestDataReadStaysInsideTheDataDirectory pins the one file capability a plugin has. A lexical
// ".." and a symlink that points out of data/ must both be refused; the engine has no file
// system of its own, so this builtin is the only place the boundary can be enforced and the
// only place it can be lost.
func TestDataReadStaysInsideTheDataDirectory(t *testing.T) {
	root := t.TempDir()
	dir := writePlugin(t, root, "a-reader", source("a-reader", "1", []string{"on_line"}, nil,
		"def on_line(line):\n    return {\"text\": read_data(\"note.txt\")}\n"))
	if err := os.MkdirAll(filepath.Join(dir, dataDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, dataDirName, "note.txt"), []byte("from data"), 0o644); err != nil {
		t.Fatal(err)
	}
	writePlugin(t, root, "b-escape", source("b-escape", "1", []string{"on_line"}, nil,
		"def on_line(line):\n    return {\"text\": read_data(\"../secret.txt\")}\n"))
	if err := os.MkdirAll(filepath.Join(root, "b-escape", dataDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b-escape", "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	zdir := writePlugin(t, root, "z-link", source("z-link", "1", []string{"on_line"}, nil,
		"def on_line(line):\n    return {\"text\": read_data(\"link\")}\n"))
	if err := os.MkdirAll(filepath.Join(zdir, dataDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(zdir, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "secret.txt"), filepath.Join(zdir, dataDirName, "link")); err != nil {
		t.Fatal(err)
	}

	r, sink := newRegistry(t, root)
	got, keep := r.OnLine("libera", []byte(":alice!a@b PRIVMSG #chan :hello"))
	if !keep || !strings.HasSuffix(string(got), ":from data") {
		t.Fatalf("the in-data read did not reach the line: keep=%v line=%q", keep, got)
	}
	logged := sink.all()
	if !strings.Contains(logged, "leaves the data directory") {
		t.Fatalf("the lexical escape was not refused; log was %q", logged)
	}
	if !strings.Contains(logged, "resolves outside the data directory") {
		t.Fatalf("the symlink escape was not refused; log was %q", logged)
	}
}

// TestNoPluginsConfiguredChangesNothing is the unconfigured path: a nil registry, the line
// untouched and still relayed. Every call site in the daemon is this nil check, and removing
// the check has to panic here rather than silently work.
func TestNoPluginsConfiguredChangesNothing(t *testing.T) {
	var r *Registry
	line := []byte(":alice!a@b PRIVMSG #chan :hello")
	got, keep := r.OnLine("libera", line)
	if !keep {
		t.Fatal("a nil registry dropped the line")
	}
	if !bytes.Equal(got, line) {
		t.Fatalf("a nil registry changed the line: %q", got)
	}
	if rows := r.PluginList(); rows != nil {
		t.Fatalf("a nil registry listed %q", rows)
	}
	if reloaded, failed := r.PluginReload(); reloaded != nil || failed != nil {
		t.Fatalf("a nil registry reloaded %q/%q", reloaded, failed)
	}
	if err := r.PluginEnable("anything"); err == nil {
		t.Fatal("a nil registry accepted an enable")
	}
}

// TestAllocGate is the hot-path guarantee: a registry that is configured but has no enabled
// on_line hook must not allocate per line. Building a context dict before checking for the hook
// is the mutation this rejects, and it is the difference between "plugins cost nothing when
// unused" and "plugins cost an allocation on every line forever".
func TestAllocGate(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "load-only", source("load-only", "1", []string{"on_load"}, nil,
		"def on_load(ctx):\n    return None\n"))

	r, _ := newRegistry(t, root)
	line := []byte(":alice!a@b PRIVMSG #chan :hello")
	if got := testing.AllocsPerRun(1000, func() {
		_, _ = r.OnLine("libera", line)
	}); got != 0 {
		t.Fatalf("OnLine allocated %v times per line with no on_line hook", got)
	}
}

// TestDisableSkipsHooksAndEnableRestoresThem: disabled means loaded but silent, so the plugin
// survives a reload and ENABLE does not need the file to change.
func TestDisableSkipsHooksAndEnableRestoresThem(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "toggle", source("toggle", "1", []string{"on_line"}, nil,
		"def on_line(line):\n    return {\"text\": \"on\"}\n"))

	r, _ := newRegistry(t, root)
	line := []byte(":alice!a@b PRIVMSG #chan :hello")
	if err := r.PluginDisable("toggle"); err != nil {
		t.Fatal(err)
	}
	got, _ := r.OnLine("libera", line)
	if !bytes.Equal(got, line) {
		t.Fatalf("a disabled plugin still ran: %q", got)
	}
	if err := r.PluginEnable("toggle"); err != nil {
		t.Fatal(err)
	}
	got, _ = r.OnLine("libera", line)
	if !strings.Contains(string(got), ":on") {
		t.Fatalf("an enabled plugin did not run: %q", got)
	}
}

// TestOnLineCanOverrideKindAndDrop pins the other two allowed overrides: kind is a tag the
// client can style, and drop removes the line from the live relay.
func TestOnLineCanOverrideKindAndDrop(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "a-kind", source("a-kind", "1", []string{"on_line"}, nil,
		"def on_line(line):\n    return {\"kind\": \"highlight\", \"text\": \"hi\"}\n"))
	writePlugin(t, root, "z-drop", source("z-drop", "1", []string{"on_line"}, nil,
		"def on_line(line):\n    return {\"drop\": True}\n"))

	r, _ := newRegistry(t, root)
	if err := r.PluginDisable("z-drop"); err != nil {
		t.Fatal(err)
	}
	got, keep := r.OnLine("libera", []byte(":alice!a@b PRIVMSG #chan :hello"))
	if !keep {
		t.Fatal("the kind plugin dropped the line")
	}
	if !strings.Contains(string(got), "salotto.kind=highlight") {
		t.Fatalf("kind did not reach the line: %q", got)
	}
	if !strings.HasSuffix(string(got), ":hi") {
		t.Fatalf("text did not reach the line: %q", got)
	}

	if err := r.PluginDisable("a-kind"); err != nil {
		t.Fatal(err)
	}
	if err := r.PluginEnable("z-drop"); err != nil {
		t.Fatal(err)
	}
	_, keep = r.OnLine("libera", []byte(":alice!a@b PRIVMSG #chan :hello"))
	if keep {
		t.Fatal("drop=True did not drop the line")
	}
}

// TestAPluginCanHandleACommand pins the on_command contract even though v1 wires it only at the
// registry boundary: the argv is a copy and the first handler wins in name order.
func TestAPluginCanHandleACommand(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "cmd", source("cmd", "1", []string{"on_command"}, []string{"/hl"},
		"def on_command(ctx, argv):\n    return argv[0] in [\"/hl\", \"/HL\"] and ctx[\"network\"] == \"libera\"\n"))

	r, _ := newRegistry(t, root)
	if !r.OnCommand("libera", []string{"/HL", "word"}) {
		t.Fatal("the declaring plugin did not handle /HL")
	}
	if r.OnCommand("other", []string{"/hl"}) {
		t.Fatal("the plugin handled a command on the wrong network, so ctx is not the hook's own")
	}
	if r.OnCommand("libera", []string{"/other"}) {
		t.Fatal("the plugin handled a command it did not declare")
	}
}

// TestOnLoadRunsAfterASuccessfulLoad: on_load is where a plugin builds what it needs, and the
// log line proves it actually ran rather than being declared and skipped.
func TestOnLoadRunsAfterASuccessfulLoad(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "greet", source("greet", "1", []string{"on_load"}, nil,
		"def on_load(ctx):\n    log(\"loaded \" + ctx[\"name\"])\n"))

	_, sink := newRegistry(t, root)
	if logged := sink.all(); !strings.Contains(logged, "loaded greet") {
		t.Fatalf("on_load did not run; log was %q", logged)
	}
}
