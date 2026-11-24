// Package plugin is salotto's plugin registry and lifecycle: named units of Starlark that
// declare the hooks they implement, live in their own directory beside their own data, and can
// be listed, enabled, disabled and reloaded while the daemon is running.
//
// The interpreter is internal/script, not this package. One direction of dependency only:
// plugin imports script, script never imports plugin. That keeps the sandbox testable without a
// bouncer and the bouncer testable without a plugin, and it means a plugin cannot reach a piece
// of the daemon the engine did not hand it.
//
// What this package adds on top of the engine is the thing a user would call a plugin:
//
//   - A directory is the unit. <plugins>/<name>/plugin.star is required and declares its own
//     manifest in Starlark, in the same file as the code, so the manifest cannot go stale
//     against the functions it describes. <plugins>/<name>/data/ is optional and is the one
//     directory a plugin can read, through the read_data builtin this package predeclares.
//   - A declared hook must be implemented. A manifest that lists on_line while the file defines
//     no on_line is refused at load. The alternative - quietly never calling it - is invisible
//     at runtime and costs an operator an hour of asking why their plugin does nothing.
//   - A bad edit never takes a working plugin offline. Reload compiles the new file first and
//     swaps only what succeeded; a syntax error leaves the previous version running and reports
//     the error. That is the whole value of hot reload.
//   - One plugin's failure never skips the rest. Every call is recovered and deadline-bounded
//     per plugin, so a wedged hook costs that plugin's line, not the line for everyone.
//
// Why not Go's plugin package: it only works on platforms where the plugin and the host were
// built by the same toolchain, it cannot unload a plugin, and a build with a different Go
// version refuses to open the file at all. A hot-reloadable plugin system that requires a
// rebuild with the same compiler is not hot reload. Why not Python-in-Go: import os works
// unless someone remembers to break it, and "someone remembered" is not a sandbox - the same
// refusal internal/script records. Why not a subprocess host: it adds a second process between
// the daemon and its own traffic, with a lifetime and a budget the daemon cannot see. Starlark
// is the only one of these that is a sandbox by construction, and it is already here.
package plugin

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.starlark.net/starlark"

	"github.com/iakat/salotto/internal/ircline"
	"github.com/iakat/salotto/internal/script"
)

const (
	// manifestFile is the only file a plugin directory must contain.
	manifestFile = "plugin.star"
	// manifestName is the global the file declares its metadata in. It is a convention of this
	// package, not of Starlark, which is why it is spelled once here and in the docs.
	manifestName = "PLUGIN"
	// dataDirName is the one directory a plugin may read, and only through read_data. The
	// engine still has no file system of its own: the reachable world is exactly what this
	// package predeclares, which is state, log and this one path-bound builtin.
	dataDirName = "data"
	// maxDataFile bounds one read_data call. Without it a plugin could ask the daemon to
	// allocate the size of any file under its data directory, which turns a decoration into
	// a way to spend the process's memory from a hook that runs on every line.
	maxDataFile = 1 << 20
)

// hookNames is the complete v1 hook set. It is deliberately three entries: every hook is a
// place where script code runs during a hot path, and a set nobody can hold in their head is a
// set where a plugin declares a hook the daemon never calls. Declaring anything else is refused
// at load, not silently accepted and never invoked.
var hookNames = []string{"on_load", "on_line", "on_command"}

// hookSet is hookNames as a lookup. Built once so validation allocates nothing per plugin.
var hookSet = func() map[string]bool {
	m := make(map[string]bool, len(hookNames))
	for _, name := range hookNames {
		m[name] = true
	}
	return m
}()

// lineOverrideKeys is the complete set of keys an on_line dict may contain. It is the one
// place the list is written: validation, the refusal message and the switch below all read it,
// so adding a key is one edit and removing one cannot leave a stale name in an error message.
// The set is small on purpose - text, kind and drop are the three things a relayed line can be
// changed to, and a bigger set is a bigger surface for a hook that runs on every line.
var lineOverrideKeys = []string{"text", "kind", "drop"}

// manifestKeys is the fixed manifest schema. A key outside this list is refused rather than
// ignored for the same reason an unknown on_line override is: a typo in "hook" for "hooks"
// otherwise reads as "my plugin does nothing" with no error anywhere.
var manifestKeys = []string{"name", "version", "description", "hooks", "commands"}

// manifest is PLUGIN after validation.
type manifest struct {
	name        string
	version     string
	description string
	hooks       []string
	commands    []string
}

// Config is what the daemon tells the registry at start.
type Config struct {
	// Dir is the directory holding one subdirectory per plugin. Empty means no plugins, and
	// New returns nil: a daemon that loads code from disk by default is a daemon whose
	// deployment changes what the process does by writing a file.
	Dir string
	// Limits is the per-hook budget. Zero Steps means script.DefaultLimits.
	Limits script.Limits
	// Log receives load errors, hook errors and refused overrides. Nil drops them, which is
	// only useful in tests: an operator needs to see what their plugin broke.
	Log func(string)
}

// Plugin is one successfully loaded plugin.star. Its fields other than enabled are immutable
// after build; a reload replaces the whole value rather than mutating one, which is what makes
// the snapshot swap atomic and a concurrent OnLine safe without a lock.
type Plugin struct {
	Name        string
	Version     string
	Description string
	Hooks       []string
	Commands    []string

	dir      string
	dataDir  string
	source   []byte
	eng      *script.Engine
	scr      *script.Script
	lineHook bool
	loadHook bool
	cmdHook  bool

	// enabled lives outside the immutable snapshot so DISABLE/ENABLE does not rebuild the
	// registry and does not race a line being relayed. It is deliberately in memory only:
	// persisting it is a config-file feature, not this change, and a plugin that stays
	// disabled across a restart would otherwise need a second source of truth next to the
	// directory.
	enabled atomic.Bool
}

// snapshot is the immutable set the hot path reads. Replacing one pointer is the atomic swap;
// nothing in a snapshot is mutated afterwards.
type snapshot struct {
	plugins  []*Plugin
	problems map[string]string
}

// Registry is the loaded plugins and the directory they came from. A nil *Registry is usable
// and means "no plugins configured"; every method checks for it first, which is what lets the
// daemon call the registry unconditionally and change no behaviour when the operator set no
// plugin_dir.
type Registry struct {
	dir  string
	lim  script.Limits
	log  func(string)
	snap atomic.Pointer[snapshot]

	// mu serialises reloads and the enable/disable map mutation. It is not on the line path:
	// OnLine reads the snapshot pointer and the per-plugin atomic flag, so a reload that is
	// waiting for a slow disk cannot stall a relayed line.
	mu sync.Mutex
}

// New builds the registry and performs the first load. It returns nil when dir is empty, which
// is the "no plugins configured" state the daemon compiles to a nil check.
func New(cfg Config) *Registry {
	if cfg.Dir == "" {
		return nil
	}
	lim := cfg.Limits
	if lim.Steps == 0 {
		lim = script.DefaultLimits()
	}
	r := &Registry{dir: cfg.Dir, lim: lim, log: cfg.Log}
	r.snap.Store(&snapshot{problems: map[string]string{}})
	r.PluginReload()
	return r
}

// PluginList returns one line per plugin for the admin surface: "<name> <version>
// <enabled|disabled>". A plugin directory that failed to load is listed too, with its refusal,
// because an operator whose edit broke a plugin needs to see that from the client they have
// open - a list that silently omits it looks exactly like a directory they forgot to create.
func (r *Registry) PluginList() []string {
	if r == nil {
		return nil
	}
	snap := r.snap.Load()
	if snap == nil {
		return nil
	}
	out := make([]string, 0, len(snap.plugins)+len(snap.problems))
	for _, p := range snap.plugins {
		state := "disabled"
		if p.enabled.Load() {
			state = "enabled"
		}
		out = append(out, fmt.Sprintf("%s %s %s", p.Name, p.Version, state))
	}
	names := make([]string, 0, len(snap.problems))
	for name := range snap.problems {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, fmt.Sprintf("%s refused: %s", name, snap.problems[name]))
	}
	return out
}

// PluginReload re-reads every plugin directory, compiles the files that changed, and swaps the
// registry atomically. It returns the names that were reloaded and the failures, both sorted.
//
// A plugin whose file changed but will not compile keeps its previous version running: the new
// value is built completely before anything is swapped, and a build error simply does not
// enter the next snapshot. The failure is recorded and returned. A plugin whose directory was
// removed is removed from the registry - the directory is the source of truth, and a plugin
// that keeps running after its files are gone is a surprise with a security shape.
//
// A changed plugin comes back enabled even if the previous version was disabled. The operator
// edited the code; hiding that edit behind a flag set against the old code would make "reload"
// silently mean "nothing happened". Unchanged plugins keep their enabled state, because a
// reload of something else is not a reason to turn a disabled plugin back on.
func (r *Registry) PluginReload() (reloaded, failed []string) {
	if r == nil {
		return nil, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	entries, err := os.ReadDir(r.dir)
	if err != nil {
		r.logf("plugin: cannot read %s: %v", r.dir, err)
		return nil, []string{"(" + r.dir + "): " + err.Error()}
	}

	old := map[string]*Plugin{}
	if snap := r.snap.Load(); snap != nil {
		for _, p := range snap.plugins {
			old[p.Name] = p
		}
	}

	next := make([]*Plugin, 0, len(entries))
	problems := make(map[string]string)
	for _, ent := range entries {
		if !ent.IsDir() || strings.HasPrefix(ent.Name(), ".") {
			continue
		}
		name := ent.Name()
		dir := filepath.Join(r.dir, name)
		src, rerr := os.ReadFile(filepath.Join(dir, manifestFile))
		if rerr != nil {
			problems[name] = rerr.Error()
			failed = append(failed, name+": "+rerr.Error())
			// A missing or unreadable file is a broken edit, not a request to unload: the
			// directory is still there, so the previous version keeps running.
			if prev := old[name]; prev != nil {
				next = append(next, prev)
			}
			continue
		}
		prev := old[name]
		if prev != nil && bytes.Equal(prev.source, src) {
			next = append(next, prev)
			continue
		}
		p, berr := r.build(name, dir, src)
		if berr != nil {
			problems[name] = berr.Error()
			failed = append(failed, name+": "+berr.Error())
			if prev != nil {
				next = append(next, prev)
			}
			continue
		}
		next = append(next, p)
		reloaded = append(reloaded, name)
	}

	sort.Slice(next, func(i, j int) bool { return next[i].Name < next[j].Name })
	sort.Strings(reloaded)
	sort.Strings(failed)
	r.snap.Store(&snapshot{plugins: next, problems: problems})

	// on_load runs after the swap, so a plugin that asks the registry what else is loaded sees
	// the set it is actually part of. Its return value is ignored by contract; an error is
	// reported and does not unload the plugin, because a decoration that fails on load is not
	// a reason to lose the code the operator just edited.
	for _, p := range next {
		if !p.loadHook || !contains(reloaded, p.Name) {
			continue
		}
		if _, err := r.call(p, "on_load", p.context("")); err != nil {
			r.logf("plugin %s: on_load: %v", p.Name, err)
		}
	}
	return reloaded, failed
}

// build compiles one plugin.star and validates its manifest. Nothing is installed until the
// whole thing succeeds; that is what makes "a bad edit keeps the old version" true rather than
// a race between a partial swap and a line.
func (r *Registry) build(name, dir string, src []byte) (*Plugin, error) {
	eng := script.New(r.lim)
	eng.SetLog(func(msg string) { r.logf("plugin %s: %s", name, msg) })
	eng.SetHookError(func(err error) { r.logf("plugin %s: %v", name, err) })
	// The only file access any plugin gets, and it is predeclared per plugin with the data
	// directory baked in. The engine has no file system of its own; handing it a path string
	// would be information, not a capability, so the capability is this one builtin. It
	// resolves symlinks before checking containment because a data/note.txt pointing at
	// /etc/shadow would otherwise pass a prefix check.
	data := dataDir(dir)
	eng.Predeclare("read_data", readDataBuiltin(data))

	var mod time.Time
	path := filepath.Join(dir, manifestFile)
	if info, err := os.Stat(path); err == nil {
		mod = info.ModTime()
	}
	scr := eng.LoadFile(manifestFile, path, src, mod)
	if scr == nil {
		return nil, errors.New("the script engine is not available")
	}
	if scr.Err != nil {
		return nil, scr.Err
	}
	value, ok := eng.Global(scr, manifestName)
	if !ok {
		return nil, fmt.Errorf("%s is not declared", manifestName)
	}
	m, err := parseManifest(name, value)
	if err != nil {
		return nil, err
	}
	// Declared hooks must exist as functions. This is the check the brief calls out by name:
	// a plugin that declares on_line and defines no on_line would otherwise be loaded
	// successfully, called with ok=false, and look like a plugin that "does nothing".
	for _, hook := range m.hooks {
		v, ok := eng.Global(scr, hook)
		if !ok {
			return nil, fmt.Errorf("declares %s but does not implement %s", hook, hook)
		}
		if _, ok := v.(starlark.Callable); !ok {
			return nil, fmt.Errorf("declares %s but %s is not a function", hook, hook)
		}
	}
	p := &Plugin{
		Name:        m.name,
		Version:     m.version,
		Description: m.description,
		Hooks:       m.hooks,
		Commands:    m.commands,
		dir:         dir,
		dataDir:     dataDir(dir),
		source:      src,
		eng:         eng,
		scr:         scr,
	}
	for _, hook := range m.hooks {
		switch hook {
		case "on_line":
			p.lineHook = true
		case "on_load":
			p.loadHook = true
		case "on_command":
			p.cmdHook = true
		}
	}
	p.enabled.Store(true)
	return p, nil
}

// parseManifest validates PLUGIN against the fixed schema. Every validation names the plugin
// directory because that is the thing an operator can go and look at; a bare "invalid manifest"
// would leave them reading every file under the plugin root.
func parseManifest(dirName string, value starlark.Value) (manifest, error) {
	var m manifest
	d, ok := value.(*starlark.Dict)
	if !ok {
		return m, fmt.Errorf("%s must be a dict, got %s", manifestName, value.Type())
	}
	allowed := make(map[string]bool, len(manifestKeys))
	for _, key := range manifestKeys {
		allowed[key] = true
	}
	for _, item := range d.Items() {
		key, ok := item[0].(starlark.String)
		if !ok {
			return m, fmt.Errorf("%s has a non-string key %s", manifestName, item[0].Type())
		}
		if !allowed[string(key)] {
			return m, fmt.Errorf("%s has unknown key %q (allowed: %s)",
				manifestName, string(key), strings.Join(manifestKeys, ", "))
		}
	}

	name, err := requiredString(d, "name")
	if err != nil {
		return m, err
	}
	if name != dirName {
		return m, fmt.Errorf("name %q does not match its directory %q", name, dirName)
	}
	m.name = name

	version, err := requiredString(d, "version")
	if err != nil {
		return m, err
	}
	if strings.TrimSpace(version) == "" {
		return m, errors.New("version must not be empty")
	}
	m.version = version

	if v, ok, err := d.Get(starlark.String("description")); err != nil {
		return m, err
	} else if ok {
		s, ok := v.(starlark.String)
		if !ok {
			return m, fmt.Errorf("description must be a string, got %s", v.Type())
		}
		m.description = string(s)
	}

	m.hooks, err = stringList(d, "hooks")
	if err != nil {
		return m, err
	}
	seenHook := make(map[string]bool, len(m.hooks))
	for _, hook := range m.hooks {
		if !hookSet[hook] {
			return m, fmt.Errorf("unknown hook %q (allowed: %s)", hook, strings.Join(hookNames, ", "))
		}
		if seenHook[hook] {
			return m, fmt.Errorf("hook %q is declared twice", hook)
		}
		seenHook[hook] = true
	}

	m.commands, err = stringList(d, "commands")
	if err != nil {
		return m, err
	}
	seenCommand := make(map[string]bool, len(m.commands))
	for _, command := range m.commands {
		if !strings.HasPrefix(command, "/") || len(command) < 2 {
			return m, fmt.Errorf("command %q must start with / and name a command", command)
		}
		if strings.ContainsAny(command, " \t\r\n\x00") {
			return m, fmt.Errorf("command %q contains whitespace or a control byte", command)
		}
		if seenCommand[command] {
			return m, fmt.Errorf("command %q is declared twice", command)
		}
		seenCommand[command] = true
	}
	return m, nil
}

// requiredString reads a required string key and distinguishes "missing" from "not a string",
// because the two need different fixes and the operator only sees the error.
func requiredString(d *starlark.Dict, key string) (string, error) {
	v, ok, err := d.Get(starlark.String(key))
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%s has no %q", manifestName, key)
	}
	s, ok := v.(starlark.String)
	if !ok {
		return "", fmt.Errorf("%s must be a string, got %s", key, v.Type())
	}
	return string(s), nil
}

// stringList reads an optional list of strings. A missing key is the empty list: a plugin with
// no hooks is pointless but not invalid, and refusing it would make a manifest with only a name
// and version fail for a reason nobody asked about.
func stringList(d *starlark.Dict, key string) ([]string, error) {
	v, ok, err := d.Get(starlark.String(key))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	list, ok := v.(*starlark.List)
	if !ok {
		return nil, fmt.Errorf("%s must be a list of strings, got %s", key, v.Type())
	}
	out := make([]string, 0, list.Len())
	for i := 0; i < list.Len(); i++ {
		s, ok := list.Index(i).(starlark.String)
		if !ok {
			return nil, fmt.Errorf("%s[%d] must be a string, got %s", key, i, list.Index(i).Type())
		}
		out = append(out, string(s))
	}
	return out, nil
}

// dataDir returns the plugin's data directory when it exists. It is the root read_data is bound
// to; an empty string means the plugin has no data directory and read_data always refuses.
func dataDir(dir string) string {
	path := filepath.Join(dir, dataDirName)
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return path
	}
	return ""
}

// readDataBuiltin builds the plugin's read-only window into its own data directory. It is the
// only file access a plugin gets, and it is deliberately one function bound to one root rather
// than a general open: the engine has no file system, so the capability exists only where this
// package creates it, and it cannot be widened by a plugin.
//
// Containment is checked twice, before and after resolving symlinks. A lexically clean
// "notes/today.txt" is inside the root, but data/notes could be a symlink to /etc; resolving
// only the cleaned path would then read outside the one directory the plugin was given. The
// size bound is enforced on the opened file rather than after reading, because the point is to
// not allocate the file, not to notice afterwards.
func readDataBuiltin(root string) starlark.Value {
	return starlark.NewBuiltin("read_data", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		if len(kwargs) != 0 {
			return nil, errors.New("read_data takes one positional argument")
		}
		if len(args) != 1 {
			return nil, fmt.Errorf("read_data takes one argument, got %d", len(args))
		}
		name, ok := args[0].(starlark.String)
		if !ok {
			return nil, fmt.Errorf("read_data wants a string, got %s", args[0].Type())
		}
		if root == "" {
			return nil, errors.New("this plugin has no data directory")
		}
		rel := string(name)
		if rel == "" || strings.ContainsRune(rel, 0) || filepath.IsAbs(rel) {
			return nil, fmt.Errorf("read_data %q must be a relative path inside data/", rel)
		}
		clean := filepath.Clean(rel)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("read_data %q leaves the data directory", rel)
		}
		resolvedRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, fmt.Errorf("read_data: cannot resolve the data directory: %w", err)
		}
		resolved, err := filepath.EvalSymlinks(filepath.Join(resolvedRoot, clean))
		if err != nil {
			return nil, fmt.Errorf("read_data %q: %w", rel, err)
		}
		inside, err := filepath.Rel(resolvedRoot, resolved)
		if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("read_data %q resolves outside the data directory", rel)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, fmt.Errorf("read_data %q: %w", rel, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("read_data %q is not a regular file", rel)
		}
		if info.Size() > maxDataFile {
			return nil, fmt.Errorf("read_data %q is %d bytes, over the %d-byte limit", rel, info.Size(), maxDataFile)
		}
		data, err := os.ReadFile(resolved)
		if err != nil {
			return nil, fmt.Errorf("read_data %q: %w", rel, err)
		}
		return starlark.String(data), nil
	})
}

// PluginEnable turns a loaded plugin's hooks back on. It is in memory only: persisting the flag
// is a config-file feature (the operator writes plugin_dir and decides what is enabled at
// start), not part of this change.
func (r *Registry) PluginEnable(name string) error {
	return r.setEnabled(name, true)
}

// PluginDisable keeps the plugin loaded but skips its hooks, so a reload can turn it back on
// without the file having to change. Like enable, the state is per-process and in memory.
func (r *Registry) PluginDisable(name string) error {
	return r.setEnabled(name, false)
}

func (r *Registry) setEnabled(name string, on bool) error {
	if r == nil {
		return errors.New("no plugins are configured (set plugin_dir)")
	}
	snap := r.snap.Load()
	if snap == nil {
		return fmt.Errorf("no plugin called %q", name)
	}
	for _, p := range snap.plugins {
		if p.Name == name {
			p.enabled.Store(on)
			return nil
		}
	}
	return fmt.Errorf("no plugin called %q", name)
}

// OnLine runs the on_line hook of every enabled plugin, in name order, and returns the line the
// daemon should relay. keep=false means a plugin asked for the line to be dropped.
//
// Only the relay path calls this: on_line is a decoration of live traffic, and a hook whose
// cost is paid on every line is the one hook that needs the smallest possible reach.
//
// network is accepted so the daemon has one call shape for both hooks and a later on_line
// revision that wants it does not have to change every caller; v1's on_line contract is
// line-only, so it is not passed to the script.
//
// The returned line may be the argument itself when nothing changed. When no enabled plugin
// implements on_line the function returns before building anything - no string conversion, no
// context dict, no timer - which is what keeps a configured-but-idle registry at zero
// allocations per line.
func (r *Registry) OnLine(network string, line []byte) ([]byte, bool) {
	if r == nil {
		return line, true
	}
	snap := r.snap.Load()
	if snap == nil {
		return line, true
	}
	for _, p := range snap.plugins {
		if !p.lineHook || !p.enabled.Load() {
			continue
		}
		res, err := r.call(p, "on_line", starlark.String(string(line)))
		if err != nil {
			r.logf("plugin %s: on_line: %v", p.Name, err)
			continue
		}
		next, drop, err := p.applyLine(line, res)
		if err != nil {
			// A refused override applies nothing. Keeping the line the previous plugin left
			// is deliberate: the refusal is about this plugin's answer, not about undoing
			// work another plugin already did.
			r.logf("plugin %s: on_line refused: %v", p.Name, err)
			continue
		}
		line = next
		if drop {
			return nil, false
		}
	}
	return line, true
}

// OnCommand offers a command to the plugins that declared it, in name order. The first plugin
// whose on_command returns true has handled it. A plugin error is logged and the next plugin
// gets the command, for the same reason one bad on_line does not skip the rest.
//
// v1 wires this hook into the registry but not into the daemon's command dispatch: the brief
// scopes daemon wiring to the relay path, and a command path that is only reachable from a test
// is better than a second call site nobody asked for. The API is here so the wiring is a
// dispatcher change, not a registry redesign.
func (r *Registry) OnCommand(network string, argv []string) bool {
	if r == nil || len(argv) == 0 {
		return false
	}
	snap := r.snap.Load()
	if snap == nil {
		return false
	}
	for _, p := range snap.plugins {
		if !p.cmdHook || !p.enabled.Load() || !p.declaresCommand(argv[0]) {
			continue
		}
		res, err := r.call(p, "on_command", p.context(network), stringListValue(argv))
		if err != nil {
			r.logf("plugin %s: on_command: %v", p.Name, err)
			continue
		}
		if res != nil && res != starlark.None && res.Truth() {
			return true
		}
	}
	return false
}

// declaresCommand answers whether this plugin asked for the command. Matching is case-folded
// because IRC commands are, and a plugin declaring "/HL" while a client types "/hl" meaning
// two different things would be a puzzle with no answer.
func (p *Plugin) declaresCommand(command string) bool {
	for _, declared := range p.Commands {
		if strings.EqualFold(declared, command) {
			return true
		}
	}
	return false
}

// applyLine validates and applies one on_line return value.
//
// The allowed keys are enumerated in exactly one place - this function - and anything else is
// an error rather than something skipped. A typo in a dict key otherwise reads as "my plugin
// does nothing": the hook ran, returned a dict, and nobody told the operator that "txt" is not
// a field. Refusing the whole dict also means a plugin cannot half-apply an edit it believed
// was complete.
func (p *Plugin) applyLine(line []byte, res starlark.Value) ([]byte, bool, error) {
	if res == nil || res == starlark.None {
		return line, false, nil
	}
	d, ok := res.(*starlark.Dict)
	if !ok {
		return line, false, fmt.Errorf("returned %s, want None or a dict", res.Type())
	}
	var text, kind starlark.String
	var hasText, hasKind, drop bool
	for _, item := range d.Items() {
		key, ok := item[0].(starlark.String)
		if !ok {
			return line, false, fmt.Errorf("override key %s is not a string", item[0].Type())
		}
		if !contains(lineOverrideKeys, string(key)) {
			return line, false, fmt.Errorf("unknown override %q (allowed: %s)",
				string(key), strings.Join(lineOverrideKeys, ", "))
		}
		switch string(key) {
		case "text":
			s, ok := item[1].(starlark.String)
			if !ok {
				return line, false, fmt.Errorf("text must be a string, got %s", item[1].Type())
			}
			if badText(string(s)) {
				return line, false, errors.New("text contains a line break or NUL, which would inject a second IRC line")
			}
			text, hasText = s, true
		case "kind":
			s, ok := item[1].(starlark.String)
			if !ok {
				return line, false, fmt.Errorf("kind must be a string, got %s", item[1].Type())
			}
			if strings.TrimSpace(string(s)) == "" {
				return line, false, errors.New("kind must not be empty")
			}
			if strings.ContainsAny(string(s), "\r\n\x00") {
				return line, false, errors.New("kind contains a line break or NUL")
			}
			kind, hasKind = s, true
		case "drop":
			b, ok := item[1].(starlark.Bool)
			if !ok {
				return line, false, fmt.Errorf("drop must be a bool, got %s", item[1].Type())
			}
			drop = bool(b)
		default:
			// The key passed lineOverrideKeys, so this is a bug in this package rather than a
			// plugin error: a name was added to the list without a branch to implement it.
			return line, false, fmt.Errorf("override %q is listed but not implemented", string(key))
		}
	}
	out := line
	if hasText {
		next, ok := replaceTrailing(out, string(text))
		if !ok {
			return line, false, errors.New("text override but the line has no trailing parameter")
		}
		out = next
	}
	if hasKind {
		out = setKind(out, string(kind))
	}
	return out, drop, nil
}

// replaceTrailing replaces the IRC trailing parameter, the part after " :". That is the only
// part of a line that is free text; rewriting a target or a verb would turn a decoration into
// a different message, and the allowed key is "text", not "line".
func replaceTrailing(line []byte, text string) ([]byte, bool) {
	// A prefix cannot contain a space and tag values escape spaces, so the first " :" is the
	// trailing parameter's colon. Searching for the last space instead would be wrong for a
	// line whose text itself contains " :".
	i := bytes.Index(line, []byte(" :"))
	if i < 0 {
		return nil, false
	}
	out := make([]byte, 0, i+2+len(text))
	out = append(out, line[:i+2]...)
	out = append(out, text...)
	return out, true
}

// badText refuses the two bytes that end or split an IRC line. A plugin that could return
// "hello\r\nPRIVMSG #chan :owned" would be able to relay a message the upstream never sent, so
// the check belongs here rather than in a comment telling plugin authors not to.
func badText(text string) bool {
	return strings.ContainsAny(text, "\r\n\x00")
}

// setKind attaches or replaces the kind tag. The daemon does not act on it: kind is a hint for
// the client, which owns how a line is drawn, and the wire is the only place both ends can
// agree on it.
func setKind(line []byte, kind string) []byte {
	tags, rest := ircline.Tags(line)
	key := []byte(ircline.TagKind)
	filtered := dropTag(tags, key)
	out := make([]byte, 0, len(line)+len(key)+len(kind)+4)
	out = append(out, '@')
	out = append(out, filtered...)
	if len(filtered) > 0 {
		out = append(out, ';')
	}
	out = append(out, key...)
	out = append(out, '=')
	out = ircline.AppendTagValue(out, kind)
	if len(rest) > 0 {
		out = append(out, ' ')
		out = append(out, rest...)
	}
	return out
}

// dropTag removes one key from a tag blob, returning the remaining blob without '@' or the
// trailing space. It is the inverse of AppendTagValue for the one tag on_line owns.
func dropTag(tags, key []byte) []byte {
	if len(tags) == 0 {
		return nil
	}
	out := make([]byte, 0, len(tags))
	first := true
	for i := 0; i <= len(tags); {
		j := i
		for j < len(tags) && tags[j] != ';' {
			j++
		}
		seg := tags[i:j]
		eq := bytes.IndexByte(seg, '=')
		name := seg
		if eq >= 0 {
			name = seg[:eq]
		}
		if !bytes.EqualFold(name, key) {
			if !first {
				out = append(out, ';')
			}
			out = append(out, seg...)
			first = false
		}
		if j >= len(tags) {
			break
		}
		i = j + 1
	}
	return out
}

// call invokes one hook under the engine's step budget and this registry's deadline, and
// recovers a panic. The recover is outside the engine's own because a panic in this package
// (a bad type assertion, a nil map) is also a panic in the daemon, and a plugin call is the only
// place it can happen.
func (r *Registry) call(p *Plugin, fn string, args ...starlark.Value) (starlark.Value, error) {
	if p == nil || p.eng == nil || p.scr == nil {
		return nil, nil
	}
	thread := &starlark.Thread{Name: p.Name}
	thread.Print = func(*starlark.Thread, string) {}

	type outcome struct {
		value starlark.Value
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		var out outcome
		defer func() {
			if rec := recover(); rec != nil {
				out.err = fmt.Errorf("panic in %s: %v", fn, rec)
			}
			done <- out
		}()
		out.value, _, out.err = p.eng.Call(thread, p.scr, fn, args...)
	}()

	if r.lim.Deadline <= 0 {
		out := <-done
		return out.value, out.err
	}
	select {
	case out := <-done:
		return out.value, out.err
	case <-time.After(r.lim.Deadline):
		// Cancel rather than wait: the guarantee is that the caller is bounded, not that the
		// script noticed. A script that never reaches a step check costs one abandoned
		// goroutine, which is the same bargain internal/script makes.
		thread.Cancel(fmt.Sprintf("deadline of %s", r.lim.Deadline))
		return nil, fmt.Errorf("stopped at its %s deadline", r.lim.Deadline)
	}
}

// context is the copy a hook is handed. It is rebuilt for every call so a plugin that writes
// into it changes only its own copy: handing a cached dict would let one plugin corrupt what
// the next one sees, and the daemon would have no way to tell a decoration from a mutation.
func (p *Plugin) context(network string) *starlark.Dict {
	d := starlark.NewDict(6)
	_ = d.SetKey(starlark.String("name"), starlark.String(p.Name))
	_ = d.SetKey(starlark.String("version"), starlark.String(p.Version))
	_ = d.SetKey(starlark.String("description"), starlark.String(p.Description))
	_ = d.SetKey(starlark.String("dir"), starlark.String(p.dir))
	_ = d.SetKey(starlark.String("data"), starlark.String(p.dataDir))
	_ = d.SetKey(starlark.String("network"), starlark.String(network))
	return d
}

// stringListValue turns argv into a fresh Starlark list. A plugin that appends to it gets its
// own list, not the daemon's slice.
func stringListValue(argv []string) *starlark.List {
	values := make([]starlark.Value, len(argv))
	for i, arg := range argv {
		values[i] = starlark.String(arg)
	}
	return starlark.NewList(values)
}

func (r *Registry) logf(format string, args ...any) {
	if r == nil || r.log == nil {
		return
	}
	r.log(fmt.Sprintf(format, args...))
}

func contains(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}
