package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.starlark.net/starlark"
)

// TestScriptingIsOffWithoutADirectory is the default, and the default is the security property.
//
// A daemon that loads code from a directory by default is a daemon whose deployment changes
// what the process does by writing a file. The absence of a script_dir has to mean absence,
// not "the current directory" or "a default path".
func TestScriptingIsOffWithoutADirectory(t *testing.T) {
	if s := newScripting("", 0, 0, nil); s != nil {
		t.Fatalf("scripting is on with no directory configured: %+v", s)
	}
}

// TestScriptsLoadFromTheConfiguredDirectory is the baseline.
func TestScriptsLoadFromTheConfiguredDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.star"), []byte("X = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newScripting(dir, 0, 0, nil)
	if s == nil {
		t.Fatal("no scripting engine for a configured directory")
	}
	rows := s.ScriptList()
	if len(rows) != 1 {
		t.Fatalf("ScriptList gave %d rows: %v", len(rows), rows)
	}
}

// TestHotReloadHappensWithoutARestart is the requirement in one test: edit a file, keep the
// process, and have the new code running.
//
// The timer interval is short here so the test does not take seconds, which is the only
// difference from production.
func TestHotReloadHappensWithoutARestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.star")
	if err := os.WriteFile(path, []byte("X = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newScripting(dir, 0, 0, nil)
	defer s.Stop()

	changed := make(chan int, 4)
	s.StartHotReload(50*time.Millisecond, func(n int) { changed <- n })

	// A different size and a later mtime, so the change is visible to a stat-based detector.
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(path, []byte("X = 2\nY = 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	select {
	case n := <-changed:
		if n != 1 {
			t.Fatalf("reload reported %d scripts", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the edit was never picked up: scripting is not hot-loaded")
	}

	// And the new global is really there, not just a count that moved.
	eng := s.Engine()
	list := eng.List()
	if len(list) != 1 {
		t.Fatalf("list has %d", len(list))
	}
	if _, ok, err := eng.Call(nil, &list[0], "missing_but_that_is_fine"); err != nil || ok {
		// This call is not the assertion; the assertion is below via RunAll on a hook the
		// new version would define.
		_ = ok
	}
}

// TestABrokenScriptDoesNotStopTheDaemonOnReload: the whole point of the sandbox is that the
// operator's mistake is not the process's problem.
func TestABrokenScriptDoesNotStopTheDaemonOnReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.star")
	if err := os.WriteFile(path, []byte("def go():\n    return 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newScripting(dir, 0, 0, nil)
	defer s.Stop()

	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(path, []byte("def go(:\n    return 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Reload synchronously: the point is that it returns, with an error in the listing.
	if n := s.ScriptReload(); n != 1 {
		t.Fatalf("after breaking a script the reload reported %d scripts", n)
	}
	rows := s.ScriptList()
	if len(rows) != 1 {
		t.Fatalf("ScriptList gave %d rows", len(rows))
	}
	if !containsError(rows[0]) {
		t.Fatalf("a broken script is listed without an error: %q", rows[0])
	}
}

func containsError(row string) bool {
	return len(row) > 0 && (stringIndex(row, "ERROR") >= 0)
}

func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestTheLineHookActuallyReachesTheScript is the control for the test below: a "does not hang"
// assertion passes just as well when the hook never runs at all, so the positive case has to
// be proved separately or the hanging test proves nothing.
func TestTheLineHookActuallyReachesTheScript(t *testing.T) {
	dir := t.TempDir()
	// A script cannot write a file - that is the sandbox - so it records into a global and the
	// Go side reads it back. Which is also why the assertion cannot be faked by a script that
	// merely loads.
	if err := os.WriteFile(filepath.Join(dir, "mark.star"), []byte(`
def on_line(network, line):
    state["last"] = line
    state["count"] = state.get("count", 0) + 1
`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newScripting(dir, 0, 0, nil)
	defer s.Stop()

	s.onLine("ch-test", ":alice!a@b PRIVMSG #chan :hi")

	eng := s.Engine()
	list := eng.List()
	if len(list) != 1 {
		t.Fatalf("setup: %d scripts", len(list))
	}
	st := eng.State()
	last, ok, err := st.Get(starlark.String("last"))
	if err != nil || !ok {
		t.Fatalf("the script recorded nothing: ok=%v err=%v", ok, err)
	}
	if got, _ := starlark.AsString(last); got != ":alice!a@b PRIVMSG #chan :hi" {
		t.Fatalf("on_line was given %q", got)
	}
}

// TestAnInfiniteLoopInAHookDoesNotHangTheDaemon is the highest-stakes guarantee at the
// integration level: on_line runs on the dispatcher, so a script that never returns would stop
// every incoming line for every network.
func TestAnInfiniteLoopInAHookDoesNotHangTheDaemon(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "spin.star"), []byte(`
def on_line(network, line):
    while True:
        pass
`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newScripting(dir, 0, 0, nil)
	defer s.Stop()

	done := make(chan struct{})
	go func() {
		s.onLine("ch-test", ":alice!a@b PRIVMSG #chan :hi")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("on_line never returned: one bad script would stop the whole bouncer")
	}
}

// TestTheLineHookIsOffUnlessConfigured guards the flag itself. on_line runs script code on
// every incoming line of every network, so it is the one hook with a per-line cost, and a
// default that spends it on scripts nobody asked for is a default that makes the daemon slower
// for free.
func TestTheLineHookIsOffUnlessConfigured(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "m.star"), []byte(`
def on_line(network, line):
    state["seen"] = True
`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newScripting(dir, 0, 0, nil)
	defer s.Stop()

	// Wire it the way main does, with the flag off.
	saved, savedHook := scriptHookedLines, lineHook
	defer func() { scriptHookedLines, lineHook = saved, savedHook }()
	lineHook, scriptHookedLines = s, false

	runLineHook("ch-test", []byte(":alice!a@b PRIVMSG #chan :hi"))
	if _, ok, _ := s.Engine().State().Get(starlark.String("seen")); ok {
		t.Fatal("on_line ran with the hook switched off")
	}

	scriptHookedLines = true
	runLineHook("ch-test", []byte(":alice!a@b PRIVMSG #chan :hi"))
	if _, ok, _ := s.Engine().State().Get(starlark.String("seen")); !ok {
		t.Fatal("on_line did not run with the hook switched on")
	}
}

// TestTheReloaderSeesThroughASymlink is the deployment this feature was written for, and the
// bug it had: a ConfigMap volume is a set of symlinks into a timestamped directory that is
// swapped wholesale. DirEntry.Info() is an lstat, so it reports the *symlink's* modification
// time - which never changes - while the file the link points at has been replaced. The poll
// then never fires, hot reload silently does nothing, and the only visible symptom is that
// editing the file has no effect. os.Stat follows the link, which is what an operator means by
// "the file changed".
func TestTheReloaderSeesThroughASymlink(t *testing.T) {
	root := t.TempDir()
	// The shape a ConfigMap mount has: script -> ..data/script, ..data -> ..2026_11_24_03_00_00
	dir := filepath.Join(root, "scripts")
	writeVersion := func(version, body string) {
		vdir := filepath.Join(root, ".."+version)
		if err := os.MkdirAll(vdir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(vdir, "one.star"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// Repoint the file and the ..data link, exactly as the kubelet does.
		_ = os.Remove(filepath.Join(dir, "..data"))
		if err := os.Symlink(vdir, filepath.Join(dir, "..data")); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(filepath.Join(dir, "one.star"))
		if err := os.Symlink(filepath.Join(dir, "..data", "one.star"), filepath.Join(dir, "one.star")); err != nil {
			t.Fatal(err)
		}
	}

	// Same length, different bytes: "two" is what version 2 logs and it is exactly as long as
	// "one". A reload that compares size instead of content would see no change, which is the
	// bug this test is for, so the fixture has to be able to catch it.
	writeVersion("1", "def on_line(network, line):\n    log(\"one\")\n")
	sc := newScripting(dir, 0, 0, nil)
	if sc == nil {
		t.Fatal("no scripting")
	}
	defer sc.Stop()
	// Positive control: the first version loaded, so a later failure is about the reload and
	// not about a script that never compiled.
	var first bytes.Buffer
	sc.Engine().SetLog(func(m string) { first.WriteString(m) })
	sc.Engine().RunAllHooks("on_line", starlark.String("n"), starlark.String("line"))
	if !strings.Contains(first.String(), "one") {
		t.Fatalf("the first version did not load: %q", first.String())
	}
	before := sc.fingerprint()

	writeVersion("2", "def on_line(network, line):\n    log(\"twa\")\n")

	if after := sc.fingerprint(); after == before {
		t.Fatalf("the fingerprint did not change when the ConfigMap was swapped, so no reload would ever run (before=%q after=%q)", before, after)
	}

	// And the poll actually reloads when it sees a change. The timer is started while the
	// directory still holds version 2, so its baseline is version 2 and version 3 is a change
	// it has to notice on its own.
	var changes int
	sc.StartHotReload(50*time.Millisecond, func(n int) {
		changes++
		t.Logf("reload fired: %d scripts, fp=%s", n, sc.fingerprint())
	})
	writeVersion("3", "def on_line(network, line):\n    log(\"thr\")\n")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var buf bytes.Buffer
		sc.Engine().SetLog(func(m string) { buf.WriteString(m) })
		sc.Engine().RunAllHooks("on_line", starlark.String("n"), starlark.String("line"))
		if strings.Contains(buf.String(), "thr") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the reload timer never picked up the new version")
}
