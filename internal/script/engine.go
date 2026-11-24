// Package script is salotto's scripting layer: Starlark, hot-loaded, and unable to take the
// daemon down.
//
// Why Starlark and not Python: Starlark is Python's syntax with Go's embedding story. It has
// no imports, no file system, no network, no clock, no os, no reflection over Go values it was
// not handed, and no way to start a goroutine. A script that cannot do any of those things is
// a script that cannot exfiltrate a buffer, cannot reach the network the operator did not
// expose, and cannot hang the process on a blocking call - and that is the difference between
// a sandbox and a promise. gpython and friends exist and are impressive, but "Python" means
// `import os` works unless someone remembers to break it.
//
// The three properties this package has to hold, in order of how bad it is to lose them:
//
//  1. A bad script must not crash the daemon. Every call into script code is wrapped in
//     recover, because a panic in a Starlark builtin is a panic in this process.
//  2. A bad script must not hang the daemon. Every execution has a step budget and a wall
//     clock deadline, and Starlark checks both from inside its own interpreter loop, so an
//     infinite loop is interrupted rather than waited out.
//  3. A bad script must not silently do nothing. Load errors are kept and reported, and a
//     script that fails goes to an error state with its message rather than being removed -
//     an operator needs to see what they broke, not an empty list.
package script

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// Limits is the budget one script execution gets. The defaults are chosen so that a script
// that is doing real work never notices them and a script that is looping forever is stopped
// in well under a second.
type Limits struct {
	// Steps is the interpreter's step budget. Starlark calls the step function from its own
	// loop, so this interrupts computation rather than merely measuring it.
	Steps uint64
	// Deadline is the wall-clock bound. It exists because a step budget cannot bound a
	// builtin that blocks - and no builtin here blocks, which is the reason this is a
	// backstop rather than the primary defence.
	Deadline time.Duration
}

// DefaultLimits are the limits a script gets unless the operator says otherwise.
func DefaultLimits() Limits {
	return Limits{Steps: 5_000_000, Deadline: 2 * time.Second}
}

// Script is one loaded file.
type Script struct {
	// Name is the file name, which is how scripts are referred to in errors and in the admin
	// list.
	Name string
	// ModTime is when the file was last changed, which is what reload compares.
	ModTime time.Time
	// Err is the load error, if the script failed to compile or run at load time. A script
	// with an error stays in the list: an operator needs to see what broke.
	Err error

	globals starlark.StringDict
	source  []byte
}

// Hook is the set of callbacks a script may receive. Each is optional; a hook that is nil is
// simply never called.
//
// The set is small on purpose. Every hook is a place where script code runs during a hot path,
// and a hook that runs on every line of every channel is a hook that can make the bouncer slow
// in a way the operator cannot see from the script.
type Hook struct {
	// Name is the Starlark function name.
	Name string
	// Arg is a short description of what the hook is given, for the admin listing.
	Arg string
	// Call is the Go side; it receives the arguments the engine built and returns a value the
	// script produced, already converted.
	Call func(args []starlark.Value) (starlark.Value, error)
}

// Engine holds the loaded scripts and the hooks they may use.
type Engine struct {
	mu   sync.RWMutex
	dir  string
	pre  []starlark.Value // predeclared globals for every script
	hook func(starlark.Value, string, []starlark.Value) (starlark.Value, error)
	lim  Limits

	scripts map[string]*Script
	order   []string
	onHook  func(error)
	onLog   func(string)
	state   *starlark.Dict
}

// New builds an engine with no directory and no hooks. A nil engine is safe to call, which is
// what lets the daemon run without scripting at all.
func New(lim Limits) *Engine {
	if lim.Steps == 0 {
		lim = DefaultLimits()
	}
	e := &Engine{
		lim:     lim,
		scripts: make(map[string]*Script),
	}
	e.prelude()
	return e
}

// prelude installs the two globals every script gets: a mutable state dict and a log function.
//
// Why a dict for state: Starlark freezes a module's globals once it has been executed, so a
// script cannot keep a counter in a global - and that freeze is a feature, because it is what
// makes a script's behaviour a function of its inputs. What a script does need is somewhere to
// remember things between calls, so the engine hands it one mutable dict that Go owns. A
// predeclared value is not frozen, so this is mutable while the script's own globals are not:
// the state lives in one place, and the daemon can clear it whenever it likes.
//
// Why a log builtin: `print` cannot be the answer. The daemon's stdout is its log, and a script
// that can write there can forge log lines and drown real ones. This goes to a bounded ring the
// operator reads through the admin command, which cannot be flooded past the ring's size.
func (e *Engine) prelude() {
	e.state = starlark.NewDict(16)
	e.Predeclare("state", e.state)
	e.Predeclare("log", starlark.NewBuiltin("log", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		parts := make([]string, 0, len(args))
		for _, a := range args {
			parts = append(parts, a.String())
		}
		if e.onLog != nil {
			e.onLog(strings.Join(parts, " "))
		}
		return starlark.None, nil
	}))
}

// State returns the mutable dict every script shares. Sharing one is deliberate: two scripts
// that both watch the same channel should not each keep half the state, and a per-script dict
// would look identical until someone tried to coordinate.
func (e *Engine) State() *starlark.Dict {
	if e == nil {
		return nil
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.state
}

// SetLog installs the sink for the log builtin.
func (e *Engine) SetLog(fn func(string)) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onLog = fn
}

// Predeclare adds a global available to every script, by name. Anything not predeclared here
// does not exist in the script's universe: Starlark has no default globals and no import
// statement, so this list is the whole reachable world.
func (e *Engine) Predeclare(name string, v starlark.Value) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pre = append(e.pre, starlark.String(name), v)
}

// SetDispatcher installs the function that routes a script's hook call back into the daemon.
// It is a function rather than an interface so the script package has no dependency on the
// bouncer.
func (e *Engine) SetDispatcher(fn func(starlark.Value, string, []starlark.Value) (starlark.Value, error)) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.hook = fn
}

// LoadAll reads every *.star file in dir, replacing what was loaded. It returns the number of
// scripts loaded and whether anything changed.
//
// The directory is read once, not watched: a watcher is a goroutine that runs forever and
// misses events when the filesystem is a network mount, and the daemon already has a reason to
// look at the clock. Reload is called from the admin command and from a timer.
func (e *Engine) LoadAll(dir string) int {
	if e == nil {
		return 0
	}
	e.mu.Lock()
	e.dir = dir
	e.mu.Unlock()
	if dir == "" {
		return 0
	}
	e.Reload()
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.order)
}

// Reload re-reads the directory, recompiling only the files whose modification time or size
// changed. It returns how many scripts are now loaded.
//
// A file that fails to compile is loaded with its error rather than dropped: the operator sees
// the failure in the admin listing, and the previously working script is replaced by an error
// entry - deliberately, because silently keeping the old code while the file on disk says
// something else is the worst of both.
func (e *Engine) Reload() (loaded int) {
	if e == nil || e.dir == "" {
		return 0
	}
	// A panic in a script must not escape into the daemon. This is the outermost recover in
	// the package, and it exists so that even a bug in this file cannot kill the process.
	defer func() {
		if r := recover(); r != nil {
			loaded = -1
		}
	}()

	entries, err := os.ReadDir(e.dir)
	if err != nil {
		return 0
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	seen := make(map[string]bool, len(entries))
	for _, ent := range entries {
		if ent.IsDir() || filepath.Ext(ent.Name()) != ".star" {
			continue
		}
		seen[ent.Name()] = true
		// Compare content, not timestamps. On a ConfigMap volume the files are symlinks into a
		// directory that is swapped wholesale, and ent.Info() is an lstat: it reports the
		// symlink's time, which never changes, so a reload compared by mtime is a reload that
		// never happens. os.Stat fixes that but a swap can still land in the same clock tick.
		// The bytes are the only thing that means "the script changed", and these files are a
		// few hundred bytes each.
		body, rerr := os.ReadFile(filepath.Join(e.dir, ent.Name()))
		if rerr != nil {
			e.scripts[ent.Name()] = &Script{Name: ent.Name(), Err: rerr}
			continue
		}
		if old, ok := e.scripts[ent.Name()]; ok && old.Err == nil && bytes.Equal(old.source, body) {
			continue
		}
		var mod time.Time
		if info, ierr := os.Stat(filepath.Join(e.dir, ent.Name())); ierr == nil {
			mod = info.ModTime()
		}
		e.scripts[ent.Name()] = e.compileLocked(ent.Name(), filepath.Join(e.dir, ent.Name()), body, mod)
	}

	// A script that was deleted is removed: the file is the source of truth, and a script the
	// operator deleted still running is a surprise with a security shape.
	for name := range e.scripts {
		if !seen[name] {
			delete(e.scripts, name)
		}
	}

	order := make([]string, 0, len(e.scripts))
	for name := range e.scripts {
		order = append(order, name)
	}
	sort.Strings(order)
	e.order = order
	return len(order)
}

// compileLocked compiles and runs one file. It runs the file's top level, which is where a
// script builds its state; function definitions land in the globals but are not called.
func (e *Engine) compileLocked(name, path string, src []byte, mod time.Time) *Script {
	s := &Script{Name: name, ModTime: mod}
	s.source = src
	var err error

	thread := &starlark.Thread{
		Name: name,
		Print: func(_ *starlark.Thread, msg string) {
			// A print from a script is not a place to write to stdout: the daemon's stdout
			// is its log, and a script that can flood it can hide every real message.
			// There is no log sink wired here yet, so prints are dropped - which is honest,
			// because the alternative would be a script that believes it is logging.
			_ = msg
		},
	}
	thread.SetMaxExecutionSteps(e.lim.Steps)

	globals, err := starlark.ExecFileOptions(
		&syntax.FileOptions{Set: false, While: true, TopLevelControl: true},
		thread, path, src, predeclared(e.pre),
	)
	if err != nil {
		s.Err = err
		return s
	}
	s.globals = globals
	return s
}

// predeclared turns the flat name/value slice into the dict Starlark wants.
func predeclared(pairs []starlark.Value) starlark.StringDict {
	out := make(starlark.StringDict, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		name, ok := pairs[i].(starlark.String)
		if !ok {
			continue
		}
		out[string(name)] = pairs[i+1]
	}
	return out
}

// Call invokes a global function in one script. A script that does not define it is not an
// error - a script only implements the hooks it cares about - so the caller gets ok=false.
func (e *Engine) Call(thread *starlark.Thread, s *Script, fn string, args ...starlark.Value) (starlark.Value, bool, error) {
	if e == nil || s == nil || s.globals == nil {
		return nil, false, nil
	}
	v, ok := s.globals[fn]
	if !ok {
		return nil, false, nil
	}
	callable, ok := v.(starlark.Callable)
	if !ok {
		return nil, false, fmt.Errorf("%s is not a function", fn)
	}
	if thread == nil {
		thread = &starlark.Thread{Name: s.Name}
	}
	thread.SetMaxExecutionSteps(e.lim.Steps)
	res, err := starlark.Call(thread, callable, args, nil)
	return res, true, err
}

// RunAll calls a hook in every loaded script that defines it, returning the first error. It
// recovers from panics: the call into script code is the one place in this package that can
// reach user-written code, and a panic there is a panic in this process.
func (e *Engine) RunAll(fn string, args ...starlark.Value) error {
	if e == nil {
		return nil
	}
	e.mu.RLock()
	scripts := make([]*Script, 0, len(e.order))
	for _, name := range e.order {
		if s := e.scripts[name]; s != nil && s.Err == nil {
			scripts = append(scripts, s)
		}
	}
	e.mu.RUnlock()

	for _, s := range scripts {
		if err := e.runOne(s, fn, args...); err != nil {
			return err
		}
	}
	return nil
}

// runOne is the guarded single call.
//
// Two limits, and they do different jobs, which is why both exist.
//
// The step budget is checked by Starlark from inside its own interpreter loop: it stops a
// computation (a loop, deep recursion, an expensive comprehension) promptly and without any
// goroutine on our side.
//
// The wall clock stops a hook that never reaches a step check at all - a builtin that blocks,
// which no builtin here does but which a future one might, and a script is only as safe as the
// worst thing we hand it. It is enforced by running the call on its own goroutine and
// cancelling it, which Starlark documents as safe from any goroutine. The caller returns when
// the deadline fires even if the script has not noticed yet: the goroutine is cancelled and
// will stop at its next step check, so the cost of a wedged hook is one abandoned goroutine,
// not a wedged daemon. Waiting for it would mean the deadline did not bound the caller.
func (e *Engine) runOne(s *Script, fn string, args ...starlark.Value) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("script %s panicked in %s: %v", s.Name, fn, r)
		}
	}()

	start := time.Now()
	thread := &starlark.Thread{Name: s.Name}
	// A script's print goes nowhere. The daemon's stdout is its log, and a script that can
	// flood it can hide every real message.
	thread.Print = func(_ *starlark.Thread, _ string) {}

	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		var inner error
		defer func() {
			if r := recover(); r != nil {
				inner = fmt.Errorf("script %s panicked in %s: %v", s.Name, fn, r)
			}
			done <- result{inner}
		}()
		_, _, inner = e.Call(thread, s, fn, args...)
	}()

	select {
	case r := <-done:
		if r.err != nil {
			return fmt.Errorf("script %s: %w", s.Name, r.err)
		}
		if e.lim.Deadline > 0 {
			if el := time.Since(start); el > e.lim.Deadline {
				return fmt.Errorf("script %s took %s, over its %s budget", s.Name, el.Round(time.Millisecond), e.lim.Deadline)
			}
		}
		return nil
	case <-time.After(e.lim.Deadline):
		thread.Cancel(fmt.Sprintf("deadline of %s", e.lim.Deadline))
		return fmt.Errorf("script %s was stopped at its %s deadline", s.Name, e.lim.Deadline)
	}
}

// RunAllHooks is RunAll with the error swallowed and reported. It is the form the dispatcher
// uses: a hook that fails must not fail the line it was looking at, because an incoming line
// that vanishes because a script had a bug is data loss caused by a decoration.
func (e *Engine) RunAllHooks(fn string, args ...starlark.Value) {
	if e == nil {
		return
	}
	if err := e.RunAll(fn, args...); err != nil {
		// Reported through RunAll's caller; there is no log sink at this layer by design, so
		// the error is returned to whoever wired the engine.
		if e.onHook != nil {
			e.onHook(err)
		}
	}
}

// SetHookError installs the sink for hook errors.
func (e *Engine) SetHookError(fn func(error)) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onHook = fn
}

// Global reads one global from a script by name. It exists so that a test - or an admin
// command - can observe what a script did without that script needing a side effect, which it
// cannot have by design.
func (e *Engine) Global(s *Script, name string) (starlark.Value, bool) {
	if e == nil || s == nil || s.globals == nil {
		return nil, false
	}
	v, ok := s.globals[name]
	return v, ok
}

// List returns the loaded scripts, sorted, for the admin surface.
func (e *Engine) List() []Script {
	if e == nil {
		return nil
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]Script, 0, len(e.order))
	for _, name := range e.order {
		if s := e.scripts[name]; s != nil {
			out = append(out, *s)
		}
	}
	return out
}

// Len reports how many scripts are loaded.
func (e *Engine) Len() int {
	if e == nil {
		return 0
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.order)
}
