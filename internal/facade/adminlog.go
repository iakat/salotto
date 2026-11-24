package facade

// adminlog is the audit trail for operator actions.
//
// The whole value of an audit log is that it describes things that happened, once, in order, and
// cannot be quietly edited by the thing it is describing. So:
//
//   - It records the **action and the outcome**, not just the attempt. "TRAFFICCTL" alone does not
//     answer "did the operator actually throttle that network"; the result has to be in the line.
//   - It is written **before** the reply goes out. If the log write fails, that is logged, but the
//     ordering means an action that reached the user is an action that is on the record.
//   - It has **two sinks and they fail independently**: a bounded in-memory ring (so `ADMINLOG`
//     can show recent history with no filesystem) and an optional append-only file, because the
//     ring dies with the process and an audit trail that disappears on restart is not one.
//
// What it deliberately does not do: it is not a security control. There is no admin
// authentication yet (§2.0.2), so every line says who asked by nick, and the nick is a claim the
// connection made. When OIDC lands, the actor in these lines becomes verifiable and nothing else
// about the format needs to change - which is why the actor is a named field now.

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// AdminEvent is one operator action.
type AdminEvent struct {
	At     time.Time
	Actor  string // the nick that asked; a claim until OIDC, see the file comment
	Action string // the verb, canonical case
	Target string // the parameter, if any
	Result string // what happened, in the bouncer's own words
	OK     bool
}

// String renders one line in the form the spec asks for: actor, action, target, result,
// timestamp. It is a single line by construction - every field is flattened - because an audit
// log whose entries can contain a newline can have entries that look like two entries.
func (e AdminEvent) String() string {
	status := "ok"
	if !e.OK {
		status = "refused"
	}
	target := e.Target
	if target == "" {
		target = "-"
	}
	// The reason is appended only when there is one: "result=ok -" is a field that says nothing,
	// and a log full of them makes the lines that do say something harder to find.
	if e.Result == "" {
		return fmt.Sprintf("%s actor=%s action=%s target=%s result=%s",
			e.At.UTC().Format(time.RFC3339), quoteField(e.Actor), e.Action,
			quoteField(target), status)
	}
	return fmt.Sprintf("%s actor=%s action=%s target=%s result=%s reason=%s",
		e.At.UTC().Format(time.RFC3339), quoteField(e.Actor), e.Action,
		quoteField(target), status, quoteField(e.Result))
}

// quoteField flattens anything that could break the line format. A nick cannot contain these
// characters, but a parameter can, and an audit line is not allowed to be ambiguous.
func quoteField(s string) string {
	if s == "" {
		return "-"
	}
	var b strings.Builder
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', '"':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 160 {
		out = out[:160] + "..."
	}
	return out
}

// adminLog is the ring plus an optional file.
type adminLog struct {
	mu    sync.Mutex
	ring  []AdminEvent
	max   int
	file  *os.File
	path  string
	err   error // the last file error, reported once so a broken sink is visible
	nowFn func() time.Time
}

// newAdminLog returns a log keeping the last n events in memory. A path of "" keeps the ring only.
//
// An unopenable file is not fatal: refusing to start the bouncer because an audit file cannot be
// opened would mean the audit log can take the service down, which makes it an availability risk
// rather than a control. The error is kept and reported through Err, where an operator sees it.
func newAdminLog(n int, path string) *adminLog {
	if n <= 0 {
		n = 200
	}
	l := &adminLog{max: n, path: path, nowFn: time.Now}
	if path != "" {
		// O_APPEND with 0640: append-only in practice, and not world-readable because audit
		// lines name who did what.
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		if err != nil {
			l.err = err
		} else {
			l.file = f
		}
	}
	return l
}

// Record appends one event. It stamps the time itself so a caller cannot forget to, and it writes
// the ring first: the sink that always works is updated before the one that might not.
func (l *adminLog) Record(actor, action, target, result string, ok bool) AdminEvent {
	e := AdminEvent{At: l.nowFn(), Actor: actor, Action: action, Target: target, Result: result, OK: ok}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ring = append(l.ring, e)
	if len(l.ring) > l.max {
		// Drop from the front by reslicing; the ring holds events, not a queue of promises, so
		// nothing is waiting on the dropped entries.
		l.ring = l.ring[len(l.ring)-l.max:]
	}
	if l.file != nil {
		if _, err := l.file.WriteString(e.String() + "\n"); err != nil {
			// Keep the first error: a full disk repeats the same error on every write and the
			// interesting fact is that the sink broke, not that it stayed broken.
			if l.err == nil {
				l.err = err
			}
		}
	}
	return e
}

// Recent returns the last n events, oldest first, so a reader sees them in the order they happened.
func (l *adminLog) Recent(n int) []AdminEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n <= 0 || n > len(l.ring) {
		n = len(l.ring)
	}
	out := make([]AdminEvent, n)
	copy(out, l.ring[len(l.ring)-n:])
	return out
}

// Err reports the last sink error, if any. An audit log that has silently stopped writing is worse
// than none, because it is the one that gets trusted.
func (l *adminLog) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// Close releases the file sink.
func (l *adminLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// auditAction records one controlplane command and returns the result text it was given, so the
// caller can both log and answer from one value. It exists so that logging is one expression at
// the dispatch site rather than a line each handler has to remember to add - a handler that
// forgets is exactly the action missing from the trail.
//
// Read-only verbs are recorded too. "Who looked at the user list" is a question an audit trail is
// expected to answer, and the cost is one line.
func (l *adminLog) auditAction(actor, verb, target string, ok bool, result string) {
	if l == nil {
		return
	}
	l.Record(actor, strings.ToUpper(verb), target, result, ok)
}
