package facade

import (
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/iakat/salotto/internal/ircline"
)

// The admin namespace.
//
// "*controlplane" is a second command target, alongside "*status". The split is the same one
// every bouncer makes and the reason it is worth making: *status is the client talking to its
// own bouncer about itself - my windows, my buffers, my connection - and is available to
// whoever holds the connection. *controlplane is the operator talking about the process -
// what it is doing, who else is connected, what the configuration says - and is the surface
// that should ever grow an access check.
//
// Keeping them apart now means that check has one place to go later. A single namespace with
// a permission flag per command is how an admin command ends up reachable because someone
// forgot the flag, and the flag is invisible in the code that dispatches.
//
// ZNC's equivalent is *controlpanel, which exists only when its "controlpanel" module is
// loaded. salotto has no modules, so this is always present - but it refuses to run when the
// operator has not configured it, rather than being absent and unexplainable.

// controlplaneCommands is the admin command table. Like statusCommands it is the single
// source for both dispatch and HELP, and the same test walks the dispatch to prove it.
var controlplaneCommands = []commandDoc{
	{"ADMINHELP", "[command]", "this list, or what one admin command does"},
	{"ADMINLOG", "[n]", "the last n operator actions, with actor, target and outcome"},
	{"CLIENTS", "", "every client attached, with its address and idle time"},
	{"CONFIG", "", "the effective configuration, secrets withheld"},
	{"JOINUSER", "<user> <#chan>", "refused: salotto is single-user"},
	{"NETWORKS", "", "the runtime networks and their connection state"},
	{"RUNTIME", "", "goroutines, heap and uptime"},
	{"SCRIPTS", "", "the loaded scripts, their hooks and their errors"},
	{"SCRIPTLOG", "", "the last lines the scripts logged"},
	{"SCRIPTRELOAD", "", "re-read the script directory without restarting"},
	{"SHUTDOWN", "[reason]", "stop the daemon"},
	{"TRAFFICCTL", "", "the traffic counters, resetting them"},
	{"USERS", "", "the users of this bouncer"},
	{"VERSION", "", "the daemon version and protocol support"},
}

// controlplanePrivmsg recognises "PRIVMSG *controlplane <command>" and returns the command.
// Both spellings of the star are accepted, for the same reason *status accepts both: a user
// typing either means the same thing and refusing one is a puzzle with no answer.
func controlplanePrivmsg(rest []byte) (text string, ok bool) {
	if !ircline.IsVerb(rest, "PRIVMSG") {
		return "", false
	}
	params := ircline.Params(rest)
	to := string(ircline.FirstParam(params))
	if to != "*controlplane" && to != "controlplane" {
		return "", false
	}
	_, body, _ := strings.Cut(string(params), " ")
	body = strings.TrimSpace(strings.TrimPrefix(body, ":"))
	if body == "" {
		return "", false
	}
	return body, true
}

// handleControlplane dispatches one admin command. The target is a parameter, not a flag on
// the client, so the reply goes to the window the operator asked from and a client that is
// not an operator never sees a difference between "refused" and "does not exist".
//
// Every command is audited from here, once, rather than in each handler. A handler that has to
// remember to log its own action is a handler that will eventually forget, and the action missing
// from the trail is exactly the one somebody will ask about. The audit records the refusal as well
// as the success: "SHUTDOWN was attempted" is the line an operator wants after an incident, and a
// trail of successes only cannot answer it.
func (c *client) handleControlplane(rest []byte) bool {
	verb, params, _ := strings.Cut(string(rest), " ")
	verb = strings.TrimSpace(verb)
	sent, refusal := c.dispatchControlplane(verb, params)
	c.srv.adminLog.auditAction(c.nick, verb, strings.TrimSpace(params), refusal == "", refusal)
	return sent
}

// dispatchControlplane is the command table.
//
// It returns two separate things and they are not the same question:
//
//   - sent: did the reply reach the client. Every handler returns c.status(...), which is a
//     write, so this is the value the caller has to propagate.
//   - refusal: empty when the command did something, otherwise why it did not.
//
// The distinction matters for the audit trail. A log that recorded "sent" as "accepted" would
// claim SHUTDOWN succeeded, because a refusal is also a message that gets written. So each arm
// that refuses says so explicitly, and the trail records the refusal as a refusal. Deriving it
// from the reply text instead would mean the audit's meaning could be changed by editing prose.
func (c *client) dispatchControlplane(verb, params string) (sent bool, refusal string) {
	switch strings.ToUpper(verb) {
	case "ADMINHELP", "HELP":
		return c.handleAdminHelp(params), ""
	case "VERSION":
		return c.controlplaneVersion(), ""
	case "RUNTIME":
		return c.controlplaneRuntime(), ""
	case "ADMINLOG":
		return c.controlplaneAdminLog(params), ""
	case "CLIENTS":
		return c.controlplaneClients(), ""
	case "NETWORKS":
		return c.handleListNetworks(), ""
	case "USERS":
		return c.controlplaneUsers(), ""
	case "CONFIG":
		return c.controlplaneConfig(), ""
	case "TRAFFICCTL":
		return c.handleTraffic(), ""
	case "JOINUSER":
		return c.status("JOINUSER refused: salotto is single-user; a client of this bouncer *is* the user"),
			"single-user bouncer: a client is the user"
	case "SCRIPTS":
		return c.cpScripts(), ""
	case "SCRIPTLOG":
		return c.cpScriptLog(), ""
	case "SCRIPTRELOAD":
		return c.cpScriptReload(), ""
	case "SHUTDOWN":
		// Deliberately not implemented here. A shutdown command on an unauthenticated
		// surface is a denial of service with a friendly name, and the honest state of that
		// is "there is no auth yet". It says so instead of pretending.
		return c.status("SHUTDOWN refused: no admin authentication is configured, and a shutdown command without it is a remote off switch"),
			"no admin authentication is configured"
	}
	return c.status("Unknown controlplane command: " + verb + ". Try ADMINHELP."),
		"unknown command"
}

// controlplaneAdminLog prints the audit trail.
//
// The default is deliberately small: an admin command that can return an unbounded slice is a way
// for the wire to allocate the operator's memory, so n is clamped to what the ring holds anyway.
func (c *client) controlplaneAdminLog(arg string) bool {
	n := 20
	if arg = strings.TrimSpace(arg); arg != "" {
		if v, err := strconv.Atoi(arg); err == nil {
			n = v
		}
	}
	if n <= 0 || n > logRingCap {
		n = logRingCap
	}
	events := c.srv.adminLog.Recent(n)
	if len(events) == 0 {
		return c.status("No operator actions recorded yet.")
	}
	if !c.status(fmt.Sprintf("The last %d operator action(s), oldest first:", len(events))) {
		return false
	}
	for _, e := range events {
		status := "ok"
		if !e.OK {
			status = "REFUSED"
		}
		if !c.status(fmt.Sprintf("  %s  %-8s %-12s %s  %s",
			e.At.UTC().Format("2006-01-02 15:04:05"), status, e.Actor, e.Action, e.Result)) {
			return false
		}
	}
	// A sink that has stopped writing is the failure worth surfacing: a trail that silently
	// stops is worse than none, because it is the one people trust.
	if err := c.srv.adminLog.Err(); err != nil {
		return c.status("WARNING: the audit file sink is failing: " + err.Error())
	}
	return true
}

func (c *client) handleAdminHelp(arg string) bool {
	arg = strings.ToUpper(strings.TrimSpace(arg))
	rows := append([]commandDoc(nil), controlplaneCommands...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })

	if arg != "" {
		for _, r := range rows {
			if r.name != arg {
				continue
			}
			usage := r.name
			if r.arg != "" {
				usage += " " + r.arg
			}
			return c.status(usage, " - ", r.desc)
		}
		return c.status("No controlplane command called " + arg)
	}

	width := 0
	for _, r := range rows {
		if n := len(r.name) + len(r.arg) + 1; n > width {
			width = n
		}
	}
	if !c.status("The following controlplane commands are available (PRIVMSG *controlplane <command>):") {
		return false
	}
	for _, r := range rows {
		usage := r.name
		if r.arg != "" {
			usage += " " + r.arg
		}
		if !c.status("  " + usage + strings.Repeat(" ", width-len(usage)+2) + r.desc) {
			return false
		}
	}
	return c.status("")
}

func (c *client) controlplaneVersion() bool {
	return c.status("salotto ", c.srv.cfg.ServerName, " | protocol: IRCv3 message-tags, server-time, echo-message, away-notify, account-notify, extended-join, cap-notify, batch, chathistory, labeled-response")
}

func (c *client) controlplaneRuntime() bool {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return c.status(fmt.Sprintf(
		"uptime %s · goroutines %d · heap %.1f MiB (in use) · sys %.1f MiB · GC %d",
		time.Since(c.srv.startTime()).Round(time.Second), runtime.NumGoroutine(),
		float64(m.HeapInuse)/(1<<20), float64(m.Sys)/(1<<20), m.NumGC))
}

func (c *client) controlplaneClients() bool {
	subs := c.srv.hub.Snapshot()
	if !c.status(fmt.Sprintf("%d client(s) attached:", len(subs))) {
		return false
	}
	for _, sub := range subs {
		nick := sub.Nickname()
		if nick == "" {
			nick = "(registering)"
		}
		if !c.status("  " + nick) {
			return false
		}
	}
	return true
}

func (c *client) controlplaneUsers() bool {
	// One user, by construction: salotto is a single-user bouncer and every client of it is
	// that user. Saying so is more useful than printing a list of one.
	name := c.srv.cfg.UserName
	if name == "" {
		name = "(unnamed)"
	}
	return c.status("one user: ", name, " (salotto is single-user; a client of this bouncer *is* the user)")
}

func (c *client) controlplaneConfig() bool {
	cfg := c.srv.cfg
	lines := []string{
		"server_name  " + cfg.ServerName,
		fmt.Sprintf("listen       %s (tls=%v)", cfg.Addr, cfg.TLS),
		fmt.Sprintf("max_clients  %d", cfg.MaxClients),
		fmt.Sprintf("max_buffer   %d", cfg.MaxBufferLines),
		fmt.Sprintf("motd         blocked=%v missing=%v", cfg.MOTDBlocked, cfg.MOTDMissing),
		fmt.Sprintf("networks     %d", len(cfg.Networks)),
	}
	if !c.status("effective configuration:") {
		return false
	}
	for _, l := range lines {
		if !c.status("  " + l) {
			return false
		}
	}
	// Secrets are listed by name, never by value. An admin surface that prints a password is
	// a password leak with a command name, and the operator who needs the value can read the
	// file.
	names := make([]string, 0, len(cfg.Networks))
	for name := range cfg.Networks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !c.status("  network " + name) {
			return false
		}
	}
	return true
}

func (c *client) controlplaneLogs(arg string) bool {
	n := 10
	if arg != "" {
		if v, err := strconv.Atoi(arg); err == nil && v > 0 && v <= 200 {
			n = v
		}
	}
	lines := c.srv.RecentLog(n)
	if len(lines) == 0 {
		return c.status("no log lines buffered")
	}
	for _, l := range lines {
		if !c.status("  " + l) {
			return false
		}
	}
	return true
}

// Scripts is what the admin surface needs from the scripting engine. It is an interface so
// this package does not import the Starlark interpreter, and so the tests can hand it a table
// instead of a script directory.
type Scripts interface {
	// ScriptList returns one line per loaded script: name, hook count, or the load error.
	ScriptList() []string
	// ScriptReload re-reads the directory and returns how many scripts are loaded.
	ScriptReload() int
	// ScriptLog returns the last lines scripts logged.
	ScriptLog() []string
}

// cpScripts answers SCRIPTS. A script with a load error is listed with the error rather than
// omitted: an operator whose edit broke the file needs to see that, and a list that silently
// drops what will not compile looks identical to a list of everything that works.
func (c *client) cpScripts() bool {
	if c.srv.scripts == nil {
		return c.status("no scripts: scripting is off (set script_dir)")
	}
	rows := c.srv.scripts.ScriptList()
	if len(rows) == 0 {
		return c.status("no scripts loaded from " + c.srv.scriptDirLabel())
	}
	for _, row := range rows {
		if !c.status("  " + row) {
			return false
		}
	}
	return c.status("end of scripts")
}

// cpScriptReload answers SCRIPTRELOAD: the hot reload, from the client.
func (c *client) cpScriptReload() bool {
	if c.srv.scripts == nil {
		return c.status("no scripts: scripting is off (set script_dir)")
	}
	n := c.srv.scripts.ScriptReload()
	if n < 0 {
		return c.status("reload failed: the script engine panicked, and that is exactly why it is contained")
	}
	return c.status(fmt.Sprintf("reloaded %d script(s)", n))
}

// cpScriptLog answers SCRIPTLOG.
func (c *client) cpScriptLog() bool {
	if c.srv.scripts == nil {
		return c.status("no scripts: scripting is off (set script_dir)")
	}
	lines := c.srv.scripts.ScriptLog()
	if len(lines) == 0 {
		return c.status("no script output yet")
	}
	for _, line := range lines {
		if !c.status("  " + line) {
			return false
		}
	}
	return c.status(fmt.Sprintf("end of script log (%d line(s))", len(lines)))
}

// scriptDirLabel names the directory in the admin output. It is a method so the label lives
// next to the code that prints it.
func (s *Server) scriptDirLabel() string {
	if s.sdir == "" {
		return "nowhere"
	}
	return s.sdir
}
