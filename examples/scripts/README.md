# Scripts

Starlark (`.star`), loaded from `script_dir` and hot-reloaded: edit a file, and the running
daemon picks it up within a couple of seconds. No restart, no signal, no admin command needed
(the admin command exists anyway, for when you want it now).

## Why Starlark

It is Python's syntax with Go's embedding story, and it has no I/O at all: no imports, no
file system, no network, no clock, no `os`, no goroutines. The reachable world is exactly what
the daemon predeclares, which is `state` and `log`. That is the difference between a sandbox
and a promise - a Python-in-Go would have `import os` unless someone remembered to break it.

Three guarantees hold, and the test suite proves each one by mutating the guard:

1. **A bad script cannot crash the daemon.** Every call into script code is recovered. A
   builtin that panics is a panic in Go, and it is caught.
2. **A bad script cannot hang the daemon.** Each call gets a step budget, checked inside
   Starlark's own interpreter loop, plus a wall-clock deadline for anything a step check cannot
   see. `while True: pass` is stopped in milliseconds.
3. **A bad script cannot do nothing quietly.** A file that does not compile stays in the list
   with its error, so the operator sees what broke instead of an empty list.

## Hooks

| Hook | Arguments | When |
| --- | --- | --- |
| `on_line` | `network`, `line` | every incoming line, if `script_hooked_lines = true` |

A script implements the hooks it cares about; a missing hook is not an error. `on_line` is off
by default because it runs on every line of every network, and a per-line cost should be
something the operator asked for.

Hooks cannot change traffic yet: they are given values and their return value is discarded. A
first version that can rewrite traffic is a first version whose first bug rewrites traffic.

## What a script can do

- read and write `state`, one mutable dict shared by every script
- `log(...)` - goes to the daemon's bounded script ring, readable with
  `/msg *controlplane SCRIPTLOG`
- use the whole Starlark language: functions, comprehensions, string methods, sorting

## What a script cannot do

Read a file, write a file, open a socket, spawn a process, look at the clock, run forever, or
crash the process. Not because a policy forbids it: because the interpreter has no such
operation, and the globals are enumerated in `internal/script/engine.go`.

## Configuration

```toml
script_dir        = "/var/lib/salotto/scripts"
script_steps      = 5000000    # interpreter steps per call, 0 = default
script_timeout_ms = 2000       # wall clock per call, 0 = default
script_hooked_lines = true     # enable on_line
```

## Example

```python
# highlights.star - count mentions and remember the last one.
def on_line(network, line):
    if "PRIVMSG" not in line:
        return
    if "salotto" in line:
        state["mentions"] = state.get("mentions", 0) + 1
        state["last_mention"] = line
        log(network, "mention #%d" % state["mentions"])
```
