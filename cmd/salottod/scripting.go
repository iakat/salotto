package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.starlark.net/starlark"

	"github.com/iakat/salotto/internal/script"
)

// scripting wires the Starlark engine into the daemon.
//
// It exists as its own type because the engine knows nothing about IRC and the facade knows
// nothing about Starlark: this is the only file that has to know both, and keeping it that way
// is what makes the engine testable without a bouncer and the bouncer usable without an
// engine.
type scripting struct {
	eng  *script.Engine
	dir  string
	stop chan struct{}

	mu    sync.Mutex
	errs  int      // errors logged since start
	lines []string // the bounded log ring scripts write into
}

// newScripting builds an engine for dir and starts the hot-reload timer.
//
// The timer is a poll rather than fsnotify on purpose. The script directory lives on a volume
// that may be a ConfigMap, and a ConfigMap's files are replaced by a rename of a symlinked
// directory: an inotify watch can be left watching an inode nothing writes to any more, which
// looks exactly like "hot reload does not work". A stat every two seconds cannot be fooled,
// costs nothing, and the delay is invisible next to a human editing a file.
func newScripting(dir string, steps uint64, timeout time.Duration, note func(string)) *scripting {
	if dir == "" {
		return nil
	}
	lim := script.DefaultLimits()
	if steps > 0 {
		lim.Steps = steps
	}
	if timeout != 0 {
		lim.Deadline = timeout
	}
	eng := script.New(lim)
	// The reachable world: one table of facts about where a line came from. Nothing that can
	// dial, read, write, spawn or block, because a script that can do any of those is not
	// sandboxed however much the interpreter is.
	eng.Predeclare("salotto", starlark.String("salotto"))

	s := &scripting{eng: eng, dir: dir, stop: make(chan struct{})}
	eng.SetHookError(s.noteHookError)
	eng.SetLog(s.noteLog)
	n := eng.LoadAll(dir)
	if note != nil {
		note(fmt.Sprintf("scripting: loaded %d script(s) from %s", n, dir))
	}
	return s
}

// Engine exposes the raw engine for the hook wiring.
func (s *scripting) Engine() *script.Engine {
	if s == nil {
		return nil
	}
	return s.eng
}

// StartHotReload polls the directory and reloads when a file's mtime changes.
func (s *scripting) StartHotReload(every time.Duration, onChange func(int)) {
	if s == nil {
		return
	}
	if every <= 0 {
		every = 2 * time.Second
	}
	// The baseline is taken here, on the caller's goroutine, before the timer exists. Inside
	// the goroutine it raced the caller: a change made between StartHotReload returning and the
	// goroutine being scheduled became the baseline, so that change was never seen. In the test
	// that race is the difference between "hot reload works" and "hot reload never fires", and
	// the same window exists in production for anyone who edits at the moment of start.
	last := s.fingerprint()
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-t.C:
				if fp := s.fingerprint(); fp != last {
					last = fp
					n := s.eng.Reload()
					if onChange != nil {
						onChange(n)
					}
				}
			}
		}
	}()
}

// Stop ends the reload timer.
func (s *scripting) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
}

// fingerprint detects a change to the script directory by hashing what the files contain.
//
// Three detectors were tried, and the first two were wrong in the deployment this exists for.
//
// mtime of the loaded scripts: could only change after a reload, so a reload never ran.
// mtime and size from the directory entry: DirEntry.Info is an lstat, and on a ConfigMap volume
// every file is a symlink into a timestamped directory swapped wholesale, so it reports the
// symlink's time, which never changes. os.Stat follows the link and fixed that, but a swap can
// still land in the same mtime tick as the previous write and leave the fingerprint identical -
// observed, not theorised.
//
// Content is the only thing that means what "the script changed" means. The directory holds a
// handful of small files, so hashing them every two seconds costs nothing measurable, and it is
// also correct in two cases the timestamps got wrong anyway: a file restored from a backup with
// its old mtime, and an edit that puts back the exact bytes that were there before (no reload
// needed, and none happens).
func (s *scripting) fingerprint() string {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return "err:" + err.Error()
	}
	h := sha256.New()
	for _, ent := range entries {
		if ent.IsDir() || filepath.Ext(ent.Name()) != ".star" {
			continue
		}
		body, rerr := os.ReadFile(filepath.Join(s.dir, ent.Name()))
		if rerr != nil {
			fmt.Fprintf(h, "%s:err:%v;", ent.Name(), rerr)
			continue
		}
		fmt.Fprintf(h, "%s:%d:", ent.Name(), len(body))
		h.Write(body)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ScriptList implements facade.Scripts.
func (s *scripting) ScriptList() []string {
	if s == nil {
		return nil
	}
	list := s.eng.List()
	out := make([]string, 0, len(list))
	for _, sc := range list {
		if sc.Err != nil {
			out = append(out, fmt.Sprintf("%-24s ERROR %v", sc.Name, sc.Err))
			continue
		}
		out = append(out, fmt.Sprintf("%-24s ok", sc.Name))
	}
	return out
}

// ScriptReload implements facade.Scripts.
func (s *scripting) ScriptReload() int {
	if s == nil {
		return 0
	}
	return s.eng.Reload()
}

// onLine runs the on_line hook for one incoming line.
//
// It is called from the dispatcher, which is the goroutine every incoming line goes through,
// so it must never block and never panic. The engine's own guards cover the panic and the
// deadline; the work here is limited to building the arguments.
func (s *scripting) onLine(network, line string) {
	if s == nil {
		return
	}
	eng := s.Engine()
	eng.RunAllHooks("on_line", starlark.String(network), starlark.String(line))
}

// noteLog records what a script logged, into the same bounded ring the admin surface reads.
// A script that can log without a bound can fill memory from the wire, which is why the sink
// is the daemon's ring and not a file.
func (s *scripting) noteLog(msg string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.lines = append(s.lines, msg)
	if len(s.lines) > 200 {
		s.lines = s.lines[len(s.lines)-200:]
	}
	s.mu.Unlock()
}

// ScriptLog returns the last lines scripts logged, newest last.
func (s *scripting) ScriptLog() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.lines))
	copy(out, s.lines)
	return out
}

// noteHookError records a hook failure so the admin surface can show it. It does not stop the
// daemon and does not disable the script: a script that is wrong on one line should not be
// silently switched off, because "why did my script stop running" is a worse question than
// "my script is logging errors".
func (s *scripting) noteHookError(err error) {
	if s == nil || err == nil {
		return
	}
	s.mu.Lock()
	s.errs++
	s.mu.Unlock()
}
