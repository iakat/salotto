# Plugins

salotto plugins are small Starlark programs with a name, a version and a declared set of hooks.
They live in their own directory, are loaded and reloaded while the daemon runs, and can be
listed, enabled and disabled without a restart. The interpreter is the same sandbox described in
[`internal/script/engine.go`](../internal/script/engine.go): no imports, no network, no clock,
no goroutines. The one file capability is added by `internal/plugin` as a builtin bound to the
plugin's own `data/` directory. `internal/plugin` is the registry and lifecycle; it is not a
second interpreter.

## Why not Go plugins, Python or a subprocess

Go's `plugin` package only loads a `.so` built by the same toolchain for the same platform, and
it cannot unload one. A hot-reloadable plugin system that requires a rebuild with a matching
compiler is not hot reload. Python-in-Go was rejected for the reason `internal/script` records:
`import os` works unless someone remembers to break it, and a sandbox that depends on memory is
not a sandbox. A subprocess host adds a second process with a lifetime and a budget the daemon
cannot see. Starlark is the only one of these that is a sandbox by construction, and it is
already here.

## A plugin is a directory

```
<plugin_dir>/<name>/
    plugin.star      # required: manifest and hooks, one file
    data/            # optional: files the plugin may read with read_data(name)
```

`plugin.star` declares its manifest at the top, in Starlark, in the same file as the code:

```python
PLUGIN = {
    "name": "highlight-words",
    "version": "1",
    "description": "Colour lines that match my words.",
    "hooks": ["on_line", "on_command"],
    "commands": ["/hl"],
}

def on_line(line):
    if "myword" in line:
        return {"kind": "highlight"}
    return None

def on_command(ctx, argv):
    log("highlight command from " + ctx["network"])
    return True
```

The manifest is executable so it cannot go stale against the code it describes. The loader reads
`PLUGIN` without calling any hook, then validates it:

- `name` is a non-empty string and exactly matches the directory name.
- `version` is a non-empty string.
- `hooks` is a list of known hook names (`on_load`, `on_line`, `on_command`), with no duplicates.
- every declared hook is implemented as a function in the same file.
- `commands` is a list of strings that start with `/`, with no whitespace and no duplicates.
- unknown manifest keys are refused, because a typo in `"hook"` otherwise reads as "my plugin
  does nothing" with no error anywhere.

A plugin that fails any check is refused as a whole. There is no half-loaded plugin.

## Hooks

| hook | called when | return |
|---|---|---|
| `on_load(ctx)` | after a successful load or reload | ignored |
| `on_line(line)` | a line is about to be relayed to clients | `None` for unchanged, or a dict of overrides |
| `on_command(ctx, argv)` | a client typed a command the plugin declared | true when handled |

`ctx` is a fresh dict per call: `name`, `version`, `description`, `dir`, `data` (empty when the
plugin has no `data/` directory) and `network`. The daemon hands a copy, so writing into `ctx`
changes only that call. The engine's `state` dict is per plugin, so one plugin cannot read or
corrupt another's state.

### `read_data(name)`

The one file capability: `read_data("words.txt")` returns the contents of a file under that
plugin's own `data/` directory as a string. Nothing else on disk is reachable. The path must be
relative, `..` is refused before it is joined, and the resolved path is checked again after
symlinks so a `data/link -> ../secret` cannot escape. Regular files only, at most 1 MiB, so a
hook cannot ask the daemon to allocate an arbitrary file on the line path. A plugin with no
`data/` directory gets an error that says so.

`on_command` is implemented and tested at the registry boundary but is deliberately not wired
into the daemon's command dispatch in v1: the relay path is the only hook call site this change
adds. `argv` is a copy of the command and its arguments; the first declaring plugin in name
order that returns a true value handles the command.

### `on_line` overrides

The allowed keys are enumerated in one place, `applyLine` in `internal/plugin/plugin.go`:

| key | type | effect |
|---|---|---|
| `text` | string | replaces the IRC trailing parameter |
| `kind` | string | attaches a `salotto.kind` tag the client may style |
| `drop` | bool | keeps the line off the live relay |

Anything else is refused with a log line and applies nothing. A typo is not ignored: the hook ran
and the operator gets an error, which is the difference between a broken plugin and a plugin
that appears to do nothing. `text` cannot contain CR, LF or NUL: a plugin that could return
`"hi\r\nPRIVMSG #chan :owned"` would relay a line the upstream never sent.

`drop` affects the live relay only. The line has already been routed into its ring and written
to the conversation log before the relay hook runs, so a client that reconnects still sees it in
replay. That is the honest limit of wiring the hook at one call site; un-logging history would
need a second call site on the buffering path, which v1 does not add.

Multiple plugins run in name order, and each sees the line as the previous plugin left it.
`kind` is the last plugin's value. `drop` stops the chain.

## Hot reload

`PLUGIN RELOAD` re-reads every plugin directory, compares bytes, compiles the changed files, and
swaps the registry atomically. A file that does not compile leaves the previous version of that
plugin running and reports the error. A plugin whose directory was removed is unloaded. A changed
plugin comes back enabled even if the old version was disabled, because the operator edited the
code and a reload that silently ignores the edit is worse than one that tells them.

Unchanged plugins keep their `state`, their enabled flag and their compiled module: a reload
recompiles only what changed.

Disabled means loaded but skipped. Enable/disable state is per process and in memory only;
persisting it is a config-file feature, not this change. A plugin that stays disabled across a
restart must be a decision written in the configuration.

There is no file watcher. The operator runs `PLUGIN RELOAD` from the control plane; a watcher on
a directory that may be a ConfigMap has the dead-inode problem `internal/script` documents and
would spend a goroutine on a decision the operator can make explicitly.

## Configuration

```toml
# One subdirectory per plugin. Empty means no plugins are loaded; the registry is nil and every
# call site is a nil check that changes no behaviour.
plugin_dir = "/var/lib/salotto/plugins"
```

The hooks use `script.DefaultLimits()`: 5,000,000 interpreter steps and a 2 second wall-clock
deadline per call. Removing the budget would let one plugin's loop delay the daemon's line
relay; the per-plugin recover means one plugin's panic or timeout costs that plugin's answer,
not the line or the daemon.

## Control plane

Send these to `PRIVMSG *controlplane`:

```
PLUGINS                     -> one NOTICE per plugin: <name> <version> <enabled|disabled>
PLUGIN RELOAD               -> NOTICE listing what was reloaded and what failed
PLUGIN DISABLE <name>       -> NOTICE with the result
PLUGIN ENABLE <name>        -> NOTICE with the result
```

A plugin that failed to load appears in `PLUGINS` with its refusal, so the edit that broke it is
visible from the client the operator already has open. The daemon's log gets the same reason.
