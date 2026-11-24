package script

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.starlark.net/starlark"
)

// write puts a script in a temporary directory and returns the directory.
func write(t *testing.T, name, src string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestAScriptCanBeLoadedAndCalled is the baseline: the engine runs a file and a hook defined
// in it is reachable.
func TestAScriptCanBeLoadedAndCalled(t *testing.T) {
	dir := write(t, "hello.star", `
def on_message(nick, text):
    return nick + ": " + text
`)
	e := New(DefaultLimits())
	if n := e.LoadAll(dir); n != 1 {
		t.Fatalf("loaded %d scripts", n)
	}

	got, ok, err := e.Call(nil, &e.List()[0], "on_message", starlark.String("alice"), starlark.String("hi"))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the hook was not found, so nothing ran")
	}
	if s, _ := starlark.AsString(got); s != "alice: hi" {
		t.Fatalf("the hook returned %q", s)
	}
}

// TestAnInfiniteLoopIsStopped is guarantee 2, and it is the one that matters most.
//
// A script with a `while True` in a hook would otherwise hang the daemon's dispatcher - which
// is the goroutine every incoming line goes through - so the whole bouncer goes quiet while a
// CPU burns. The step budget is checked from inside Starlark's own interpreter loop, so it
// stops the computation rather than waiting it out.
func TestAnInfiniteLoopIsStopped(t *testing.T) {
	dir := write(t, "spin.star", `
def spin():
    while True:
        pass
`)
	e := New(DefaultLimits())
	e.LoadAll(dir)

	done := make(chan error, 1)
	go func() {
		done <- e.RunAll("spin")
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an infinite loop returned success")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "step") &&
			!strings.Contains(strings.ToLower(err.Error()), "cancel") &&
			!strings.Contains(strings.ToLower(err.Error()), "exceeded") {
			t.Fatalf("the loop was stopped but the error does not say why: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("an infinite loop was not stopped; the daemon would hang")
	}
}

// TestAnInfiniteRecursionIsStopped: recursion is the other way to not return, and the guard
// has to hold for it too.
func TestAnInfiniteRecursionIsStopped(t *testing.T) {
	dir := write(t, "rec.star", `
def down(n):
    return down(n + 1)

def go():
    return down(0)
`)
	e := New(DefaultLimits())
	e.LoadAll(dir)

	done := make(chan error, 1)
	go func() { done <- e.RunAll("go") }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("infinite recursion returned success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("infinite recursion was not stopped")
	}
}

// TestABrokenScriptIsReportedNotSilent is guarantee 3. A script that does not compile has to
// stay in the list with its error, because the operator needs to see what they broke - a
// script that vanished from a list is indistinguishable from one that was never there.
func TestABrokenScriptIsReportedNotSilent(t *testing.T) {
	dir := write(t, "broken.star", "def oops(:\n    pass\n")
	e := New(DefaultLimits())
	if n := e.LoadAll(dir); n != 1 {
		t.Fatalf("a broken script was not kept in the list: %d loaded", n)
	}

	list := e.List()
	if len(list) != 1 {
		t.Fatalf("the list has %d entries", len(list))
	}
	if list[0].Err == nil {
		t.Fatal("a script with a syntax error has no error recorded")
	}
}

// TestABrokenScriptDoesNotRun is the pair to the above: it is kept for its error message, not
// so that a half-compiled version can be called.
func TestABrokenScriptDoesNotRun(t *testing.T) {
	dir := write(t, "broken.star", "def go(:\n    pass\n")
	e := New(DefaultLimits())
	e.LoadAll(dir)
	if err := e.RunAll("go"); err != nil {
		t.Fatalf("a script that did not compile produced a call error instead of being skipped: %v", err)
	}
}

// TestAThrowingHookDoesNotTakeTheDaemonDown is guarantee 1, in its likeliest form: a script
// that is valid but wrong at runtime.
func TestAThrowingHookDoesNotTakeTheDaemonDown(t *testing.T) {
	dir := write(t, "boom.star", `
def boom():
    return {}["missing"]
`)
	e := New(DefaultLimits())
	e.LoadAll(dir)

	// The assertion is that this returns at all.
	err := e.RunAll("boom")
	if err == nil {
		t.Fatal("a failing hook reported success")
	}
}

// TestAPanicInsideABuiltinIsContained: the guards have to hold for a panic raised by Go code
// called from the script, not only for Starlark's own errors. A panic in a builtin is a panic
// in this process unless it is recovered.
func TestAPanicInsideABuiltinIsContained(t *testing.T) {
	e := New(DefaultLimits())
	e.Predeclare("boom", starlark.NewBuiltin("boom", func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
		panic("a builtin exploded")
	}))

	dir := write(t, "p.star", "def go():\n    boom()\n")
	e.LoadAll(dir)

	err := e.RunAll("go")
	if err == nil {
		t.Fatal("a panicking builtin reported success")
	}
	if !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("the panic was swallowed but not reported: %v", err)
	}
}

// TestAHotReloadPicksUpAnEdit is the "hot-loaded without restarting" requirement.
func TestAHotReloadPicksUpAnEdit(t *testing.T) {
	dir := write(t, "v.star", `
VALUE = 1
def get():
    return VALUE
`)
	e := New(DefaultLimits())
	e.LoadAll(dir)

	first, _, _ := e.Call(nil, &e.List()[0], "get")
	if v, _ := starlark.AsInt32(first); v != 1 {
		t.Fatalf("first load gave %d", v)
	}

	// Rewrite the file. The modification time has to differ, or a cache keyed on it - which
	// is what a real reload does - would legitimately skip the file and the test would be
	// asserting on a race rather than on a reload.
	time.Sleep(1100 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(dir, "v.star"), []byte("VALUE = 2\ndef get():\n    return VALUE\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if n := e.Reload(); n != 1 {
		t.Fatalf("reload loaded %d", n)
	}
	second, _, _ := e.Call(nil, &e.List()[0], "get")
	if v, _ := starlark.AsInt32(second); v != 2 {
		t.Fatalf("after the edit the script still returns %d, so nothing was hot-loaded", v)
	}
}

// TestADeletedScriptIsRemoved: the file is the source of truth. A script the operator deleted
// still running is a surprise with a security shape.
func TestADeletedScriptIsRemoved(t *testing.T) {
	dir := write(t, "del.star", "def go():\n    return 1\n")
	e := New(DefaultLimits())
	if n := e.LoadAll(dir); n != 1 {
		t.Fatalf("setup loaded %d", n)
	}
	if err := os.Remove(filepath.Join(dir, "del.star")); err != nil {
		t.Fatal(err)
	}
	if n := e.Reload(); n != 0 {
		t.Fatalf("a deleted script is still loaded: %d", n)
	}
}

// TestAScriptCannotReachTheFileSystem is the sandbox property, stated as a test.
//
// Starlark has no import statement and no default globals, so the reachable world is exactly
// what Predeclare put there. If this ever stops being true - a dependency adding an `open`
// builtin, say - the sandbox is gone and the test is the only thing that would say so.
func TestAScriptCannotReachTheFileSystem(t *testing.T) {
	for _, src := range []string{
		"def go():\n    return open('/etc/passwd')\n",
		"def go():\n    return __import__('os').getcwd()\n",
		"import os\ndef go():\n    return os.getcwd()\n",
	} {
		dir := write(t, "escape.star", src)
		e := New(DefaultLimits())
		e.LoadAll(dir)
		list := e.List()
		if len(list) != 1 {
			t.Fatalf("setup loaded %d scripts", len(list))
		}
		// Either it fails to compile (import) or it fails at call time (open); both are a
		// refusal. What must not happen is a value coming back.
		if list[0].Err == nil {
			got, ok, err := e.Call(nil, &list[0], "go")
			if err == nil && ok && got != nil {
				t.Fatalf("a script reached beyond its sandbox and got %v", got)
			}
		}
	}
}

// TestAnUnknownHookIsNotAnError: a script implements the hooks it cares about, and calling one
// it does not define must be a no-op rather than an error the dispatcher has to special-case.
func TestAnUnknownHookIsNotAnError(t *testing.T) {
	dir := write(t, "empty.star", "X = 1\n")
	e := New(DefaultLimits())
	e.LoadAll(dir)
	if err := e.RunAll("on_message"); err != nil {
		t.Fatalf("calling a hook nobody defined failed: %v", err)
	}
}

// TestNoEngineIsSafe: the daemon runs without scripting unless configured, and a nil engine
// has to be callable or every call site needs a nil check - and one of them will not have it.
func TestNoEngineIsSafe(t *testing.T) {
	var e *Engine
	if n := e.LoadAll("/nonexistent"); n != 0 {
		t.Fatalf("a nil engine loaded %d scripts", n)
	}
	if err := e.RunAll("go"); err != nil {
		t.Fatalf("a nil engine errored: %v", err)
	}
	if got := e.List(); got != nil {
		t.Fatalf("a nil engine listed %v", got)
	}
}

// TestTheStepBudgetIsReportedAsAStepBudget: the error has to name the limit that was hit, or
// an operator whose script is slow cannot tell whether to raise the budget or fix the script.
func TestTheStepBudgetIsReportedAsAStepBudget(t *testing.T) {
	dir := write(t, "spin2.star", "def spin():\n    while True:\n        pass\n")
	e := New(Limits{Steps: 10_000, Deadline: 5 * time.Second})
	e.LoadAll(dir)

	err := e.RunAll("spin")
	if err == nil {
		t.Fatal("a tiny step budget did not stop the loop")
	}
	low := strings.ToLower(err.Error())
	if !strings.Contains(low, "step") && !strings.Contains(low, "cancel") {
		t.Fatalf("the error does not identify the budget: %v", err)
	}
}

// TestADeadlineStopsAHookThatNeverReachesAStepCheck is the reason the wall clock exists.
//
// The step budget is checked from inside Starlark's interpreter loop, so it can only stop code
// the interpreter is running. A builtin that blocks reaches no step check at all, and steps
// would never fire however many of them there are. This is the case the deadline is for, and
// without it the test hangs until the sleep ends - which is what a wedged daemon looks like.
func TestADeadlineStopsAHookThatNeverReachesAStepCheck(t *testing.T) {
	e := New(Limits{Steps: 5_000_000, Deadline: 200 * time.Millisecond})
	e.Predeclare("slow", starlark.NewBuiltin("slow", func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
		time.Sleep(3 * time.Second)
		return starlark.None, nil
	}))

	dir := write(t, "slow.star", "def go():\n    slow()\n")
	e.LoadAll(dir)

	start := time.Now()
	err := e.RunAll("go")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a blocking hook reported success")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("the deadline did not bound the caller: it waited %s for a 3s sleep with a 200ms budget", elapsed)
	}
}
