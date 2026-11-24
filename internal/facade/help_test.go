package facade

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestEveryCommandIsInTheTable is the guard that keeps HELP honest.
//
// The help text is a list of commands, and the dispatch is another list of commands. Two
// lists of the same thing drift: the command added tomorrow is a command the help does not
// mention, which to a user is indistinguishable from one that does not work. This reads the
// arms out of handle() itself and compares, so adding a command without documenting it
// fails the build instead of the documentation.
func TestEveryCommandIsInTheTable(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	// Only the dispatch inside handle(): the fenced block starting at its signature.
	body := string(src)
	start := strings.Index(body, "func (c *client) handle(line, rest []byte) bool {")
	if start < 0 {
		t.Fatal("cannot find client.handle")
	}
	end := strings.Index(body[start:], "\nfunc ")
	if end < 0 {
		t.Fatal("cannot find the end of client.handle")
	}
	block := body[start : start+end]

	re := regexp.MustCompile(`IsVerb\(rest, "([A-Z]+)"\)`)
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(block, -1) {
		seen[m[1]] = true
	}
	// The registration arms are protocol, not user commands: a person cannot type them
	// from the input box, and listing them would make HELP read like a spec. QUIT and PING
	// are left in, because those are things a person does.
	for _, protocol := range []string{"CAP", "NICK", "USER", "WEBIRC"} {
		delete(seen, protocol)
	}
	// DELSERVER and REMSERVER share one arm; the table names the canonical spelling.
	delete(seen, "REMSERVER")

	documented := map[string]bool{}
	for _, c := range statusCommands {
		documented[c.name] = true
	}

	var missing, extra []string
	for name := range seen {
		if !documented[name] {
			missing = append(missing, name)
		}
	}
	for name := range documented {
		if !seen[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("dispatched but not in HELP: %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("in HELP but not dispatched: %v", extra)
	}
}

// TestStatusPrivmsgIsTheCommandWorthHaving is why "/msg *status help" exists at all: it is
// the only spelling a client's /msg box produces, and without it a user staring at a status
// window has no way to discover that the commands are bare verbs.
func TestStatusPrivmsgIsTheCommandWorthHaving(t *testing.T) {
	for _, tc := range []struct {
		line, want string
		ok         bool
	}{
		{"PRIVMSG *status :help", "help", true},
		{"PRIVMSG status :listchans", "listchans", true},
		{"PRIVMSG *status :", "", false},
		{"PRIVMSG #status :hello", "", false},
		{"PRIVMSG bob :help", "", false},
		{"JOIN #status", "", false},
	} {
		_, text, ok := statusPrivmsg([]byte(tc.line))
		if ok != tc.ok || text != tc.want {
			t.Errorf("%q: got (%q, %v), want (%q, %v)", tc.line, text, ok, tc.want, tc.ok)
		}
	}
}

// TestHelpListsTheCommandsAndNamesOne checks both halves of the answer: the list, and the
// single-command form. A HELP that only prints the list leaves "HELP CLEARBUFFER" to guess.
func TestHelpListsTheCommandsAndNamesOne(t *testing.T) {
	s, _, sub, out := newWindowFixture(t)
	_ = flushLines(t, sub, out)
	c := &client{srv: s, sub: sub, nick: "test"}
	drain := func() string { return strings.Join(notices(flushLines(t, sub, out)), "\n") }

	if !c.handleHelp([]byte("HELP")) {
		t.Fatal("HELP failed")
	}
	got := drain()
	if !strings.Contains(got, "LISTCHANS") {
		t.Fatalf("HELP does not mention LISTCHANS:\n%s", got)
	}
	if !strings.Contains(got, "HELP") {
		t.Fatalf("HELP does not mention itself:\n%s", got)
	}

	if !c.handleHelp([]byte("HELP LISTCHANS")) {
		t.Fatal("HELP LISTCHANS failed")
	}
	one := drain()
	if !strings.Contains(one, "LISTCHANS") || !strings.Contains(one, "every window") {
		t.Fatalf("HELP LISTCHANS gave %q", one)
	}
	if strings.Contains(one, "ADDSERVER") {
		t.Fatalf("HELP LISTCHANS printed the whole table: %q", one)
	}

	c.handleHelp([]byte("HELP NOSUCHCOMMAND"))
	if got := drain(); !strings.Contains(got, "Unknown command") {
		t.Fatalf("an unknown command was not named: %q", got)
	}
}
