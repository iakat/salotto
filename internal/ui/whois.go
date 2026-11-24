package ui

import (
	"strconv"
	"strings"
	"time"
)

// A whois answer is one question with up to eight answers, spread over as many numerics.
//
// Drawn raw, the user sees this:
//
//	311 nicholas alice ~a h * :Alice Smith
//	319 nicholas alice :#salotto #dev
//	312 nicholas alice irc.libera.chat :Libera Chat
//	318 nicholas alice :End of /WHOIS list
//
// which is a protocol dump, not an answer to "who is alice". The numbers are the server's naming
// for the fields, the colons are separators the wire needs and the reader does not, and "End of
// /WHOIS list" is bookkeeping that only exists because the reply had no other way to say it was
// finished.
//
// So the numerics in the whois family are collected and drawn once, when 318 says the reply is
// complete - one block, in words. Anything in the family that this file does not name falls
// through to the old behaviour, because a numeric drawn raw is not as bad as a numeric swallowed.
//
// The result goes to the window the question was asked *from*, not to status. A whois typed in
// #salotto whose answer lands in a window the user is not looking at is an answer they will not
// read, and "I typed /whois and nothing happened" is the same complaint the /msg fix was about.
// The bouncer does not know where the user was looking, so the transport remembers it: see
// noteWhois.
type whoisPending struct {
	// window is where the answer will be drawn. Resolved when the question was asked, because
	// by the time the answer arrives the user may have moved and the answer belongs where the
	// question was, not where they wandered.
	window string
	// fields are the finished lines, in the order the numerics arrived, which is the order the
	// server considers natural: identity, then channels, then server.
	fields []string
}

// noteWhois records where to draw the answer for a nick. It is called when the command is sent,
// so it sees the active window at the moment of asking.
func (t *Transport) noteWhois(nick string) {
	if nick == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.whois == nil {
		t.whois = make(map[string]*whoisPending)
	}
	// A second /whois for the same nick before the first answered replaces the first: the older
	// answer is stale by definition, and keeping it would draw the reply twice.
	t.whois[strings.ToLower(nick)] = &whoisPending{window: t.active}
}

// handleWhois renders one numeric of the whois family. It reports whether the verb belonged to the
// family and was consumed, so the caller does not also draw it raw.
func (t *Transport) handleWhois(verb, params string) bool {
	switch verb {
	case "311", "312", "313", "317", "319", "330", "671", "318":
	default:
		return false
	}

	// Every one of these is addressed to us and names its subject second:
	//   311 nicholas alice ~a h * :Alice Smith
	// The first two fields are ours and theirs; the rest is the body.
	_, after, ok := strings.Cut(params, " ")
	if !ok {
		return false
	}
	subject, body, _ := strings.Cut(after, " ")
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return false
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	key := strings.ToLower(subject)
	if t.whois == nil {
		// A Transport built field by field - a test, or a future driver that restores a
		// session - must not panic on the first whois. A nil map is a map with nothing in it.
		t.whois = make(map[string]*whoisPending)
	}
	pending, seen := t.whois[key]
	if !seen {
		// A reply to a question this client did not ask - a replay of an old one, or a script
		// asking on our behalf. Still worth drawing, and status is the honest default because
		// there is no window that asked.
		pending = &whoisPending{window: "status"}
		t.whois[key] = pending
	}

	if line, drawn := whoisField(verb, subject, body); drawn {
		pending.fields = append(pending.fields, line)
	}

	if verb != "318" {
		return true
	}
	// 318 closes the reply, so the block is complete and can be drawn.
	delete(t.whois, key)
	window := pending.window
	if window == "" {
		window = "status"
	}
	if len(pending.fields) == 0 {
		// A whois that produced nothing but its own end. Drawing an empty header would be a
		// heading over nothing; saying so is shorter and true.
		t.push(Incoming(Line{Window: window, Text: subject + ": no information", Kind: LineSystem,
			Time: time.Now()}))
		return true
	}
	t.push(Incoming(Line{Window: window, Text: subject, Kind: LineSystem, Time: time.Now()}))
	for _, field := range pending.fields {
		t.push(Incoming(Line{Window: window, Text: "  " + field, Kind: LineSystem, Time: time.Now()}))
	}
	return true
}

// whoisField turns one numeric's body into a line of words. It reports false for a numeric it does
// not know how to say anything about, which leaves the block without that field rather than with a
// raw wire line inside it.
func whoisField(verb, subject, body string) (string, bool) {
	trailing := strings.TrimPrefix(strings.TrimSpace(body), ":")
	switch verb {
	case "311":
		// <user> <host> * :<realname> - the "*" is the server's field, not information.
		fields := strings.Fields(body)
		if len(fields) < 2 {
			return "", false
		}
		user, host := fields[0], fields[1]
		realname := trailing
		if i := strings.Index(body, " :"); i >= 0 {
			realname = strings.TrimSpace(body[i+1:])
		}
		realname = strings.TrimPrefix(realname, ":")
		who := user + "@" + host
		if realname != "" && realname != who {
			return who + " (" + realname + ")", true
		}
		return who, true

	case "319":
		// The channel list, already space separated by the server.
		if trailing == "" {
			return "", false
		}
		return "on " + trailing, true

	case "312":
		// <server> :<server info>
		server, info, _ := strings.Cut(strings.TrimSpace(body), " ")
		info = strings.TrimPrefix(strings.TrimSpace(info), ":")
		if server == "" {
			return "", false
		}
		if info != "" {
			return "on " + server + " (" + info + ")", true
		}
		return "on " + server, true

	case "330":
		// <account> :is logged in as
		account, _, _ := strings.Cut(strings.TrimSpace(body), " ")
		if account == "" {
			return "", false
		}
		return "logged in as " + account, true

	case "313":
		return "is an IRC operator", true

	case "671":
		return "is using a secure connection", true

	case "317":
		// <idle seconds> <signon unix time> :seconds idle, signon time
		fields := strings.Fields(body)
		if len(fields) < 2 {
			return "", false
		}
		idle, err := strconv.Atoi(fields[0])
		if err != nil {
			return "", false
		}
		signon, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return "", false
		}
		return "idle " + humanIdle(idle) + ", signed on " + time.Unix(signon, 0).Format("Mon 2 Jan 15:04"), true
	}
	return "", false
}

// humanIdle says an idle time the way a person would: seconds are useless past a minute, and
// "7200 seconds" makes the reader do arithmetic the client can do for them.
func humanIdle(seconds int) string {
	switch {
	case seconds < 60:
		return strconv.Itoa(seconds) + "s"
	case seconds < 3600:
		return strconv.Itoa(seconds/60) + "m"
	case seconds < 86400:
		return strconv.Itoa(seconds/3600) + "h" + strconv.Itoa((seconds%3600)/60) + "m"
	}
	return strconv.Itoa(seconds/86400) + "d" + strconv.Itoa((seconds%86400)/3600) + "h"
}
