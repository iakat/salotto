package facade

import (
	"sort"
	"strings"

	"github.com/iakat/salotto/internal/ircline"
)

// The command table, as one list.
//
// HELP has to answer "what can I type here", and the only way to keep that answer true is
// to derive it from the dispatch itself. A hand-written help text is a second list of the
// commands, and a second list drifts: the first command added after it is written is a
// command nobody knows about, which is indistinguishable from one that does not exist.
//
// So every arm of client.handle is registered here, with the one-line description the help
// prints. TestEveryCommandIsInTheTable walks the source of handle and fails when an arm is
// missing, which makes the drift a build failure instead of a documentation bug.
type commandDoc struct {
	name string
	arg  string
	desc string
}

// statusCommands is the bouncer side: the verbs client.handle answers itself.
var statusCommands = []commandDoc{
	{"ADDSERVER", "<host> [[+]port] [pass]", "add an upstream server to the first network"},
	{"ADDTRUSTEDSERVERFINGERPRINT", "<fp>", "trust a certificate fingerprint"},
	{"ATTACH", "<#chan>", "attach the client to a window and replay its buffer"},
	{"AWAY", "[reason]", "set or clear the bouncer's away state"},
	{"CLEARALLBUFFERS", "", "clear the buffer of every window"},
	{"CLEARALLCHANNELBUFFERS", "", "clear the buffer of every channel"},
	{"CLEARALLQUERYBUFFERS", "", "clear the buffer of every query"},
	{"CLEARBINDHOST", "", "forget the outbound bind host"},
	{"CLEARBUFFER", "<#chan>", "clear one window's buffer"},
	{"CLEARUSERBINDHOST", "", "forget the per-user outbound bind host"},
	{"CONNECT", "", "connect the network upstream now"},
	{"DELSERVER", "<host>", "remove an upstream server"},
	{"DELTRUSTEDSERVERFINGERPRINT", "<fp>", "stop trusting a certificate fingerprint"},
	{"DETACH", "<#chan>", "detach the client from a window"},
	{"DISABLECHAN", "<#chan>", "stop keeping a window entirely"},
	{"DISCONNECT", "", "drop the upstream connection"},
	{"ENABLECHAN", "<#chan>", "keep a window again"},
	{"HELP", "[command]", "this list, or what one command does"},
	{"CHATHISTORY", "LATEST|BEFORE <target> <ts> <n>", "buffered history for a window, as an IRCv3 batch"},
	{"LISTCHANS", "", "every window with its status"},
	{"LISTCLIENTS", "", "the clients attached to this bouncer"},
	{"LISTNETWORKS", "", "the configured networks"},
	{"LISTSERVERS", "", "the upstream servers of the first network"},
	{"LISTTRUSTEDSERVERFINGERPRINTS", "", "the trusted certificate fingerprints"},
	{"MOVECHAN", "<#chan> <#before|#>", "move a window in the list"},
	{"MOVENETWORK", "", "refused: salotto has a single network per user"},
	{"PING", "<arg>", "the bouncer answers PONG"},
	{"PLAYBUFFER", "<#chan>", "replay a window's buffer"},
	{"PREF", "GET|SET <base64>", "the client settings the bouncer holds for every client"},
	{"QUIT", "", "close the connection"},
	{"SEARCH", "[window:<name>] [network:<name>] [since:<YYYY-MM-DD>] [until:<YYYY-MM-DD>] <query>", "search the conversation logs across every window"},
	{"SETBINDHOST", "<host>", "bind outbound connections to an address"},
	{"SETBUFFER", "<lines>", "how many lines a window keeps"},
	{"SETUSERBINDHOST", "<host>", "bind outbound connections per user"},
	{"SHOWBINDHOST", "", "print the current bind hosts"},
	{"TRAFFIC", "", "bytes in and out"},
}

// helpText renders the table. ZNC prints two aligned columns joined by a tab, and a client
// with a narrow window is the reason this is padded to the widest name rather than fixed.
func (c *client) helpText() bool {
	rows := append([]commandDoc(nil), statusCommands...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })

	width := 0
	for _, r := range rows {
		if n := len(r.name) + len(r.arg) + 1; n > width {
			width = n
		}
	}
	if !c.status("The following commands are available:") {
		return false
	}
	for _, r := range rows {
		usage := r.name
		if r.arg != "" {
			usage += " " + r.arg
		}
		line := "  " + usage + strings.Repeat(" ", width-len(usage)+2) + r.desc
		if !c.status(line) {
			return false
		}
	}
	return c.status("")
}

// handleHelp answers HELP, and "HELP <command>" with the single row for that command. An
// unknown command is named as unknown instead of silently printing the whole list: a typo
// is exactly when someone needs to be told which word was not recognised.
func (c *client) handleHelp(rest []byte) bool {
	arg := strings.ToUpper(strings.TrimSpace(string(ircline.FirstParam(ircline.Params(rest)))))
	if arg == "" {
		return c.helpText()
	}
	for _, r := range statusCommands {
		if r.name == arg {
			usage := r.name
			if r.arg != "" {
				usage += " " + r.arg
			}
			return c.status(usage + " - " + r.desc)
		}
	}
	return c.status("Unknown command: " + arg)
}
