package logfile

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/iakat/salotto/internal/ircline"
)

// Entry is one (window, body) pair produced from a single IRC line.
type Entry struct {
	Window string
	Body   string
}

func isChannelTarget(s string) bool {
	if s == "" {
		return false
	}
	c := s[0]
	return c == '#' || c == '&' || c == '!' || c == '+'
}

func sanitizeWindow(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, "\\", "-")
	return s
}

func nickFromPrefix(prefix []byte) string {
	s := string(prefix)
	if i := bytes.IndexByte(prefix, '!'); i >= 0 {
		return s[:i]
	}
	return s
}

func userhostFromPrefix(prefix []byte) string {
	s := string(prefix)
	if i := bytes.IndexByte(prefix, '!'); i >= 0 {
		return s[i+1:]
	}
	return ""
}

func splitTargetText(params []byte) (string, string) {
	// params: target :text  or target text
	p := bytes.TrimSpace(params)
	if len(p) == 0 {
		return "", ""
	}
	// find target
	var target []byte
	rest := p
	if i := bytes.IndexByte(p, ' '); i >= 0 {
		target = p[:i]
		rest = bytes.TrimSpace(p[i+1:])
	} else {
		target = p
		rest = nil
	}
	text := string(rest)
	if len(rest) > 0 && rest[0] == ':' {
		text = string(rest[1:])
	}
	return string(target), text
}

func firstField(params []byte) string {
	fs := bytes.Fields(params)
	if len(fs) == 0 {
		return ""
	}
	return string(fs[0])
}

func secondField(params []byte) string {
	fs := bytes.Fields(params)
	if len(fs) < 2 {
		return ""
	}
	return string(fs[1])
}

func trailingField(params []byte) string {
	s := string(params)
	if i := bytes.IndexByte(params, ':'); i >= 0 {
		return s[i+1:]
	}
	fs := bytes.Fields(params)
	if len(fs) == 0 {
		return ""
	}
	return string(fs[len(fs)-1])
}

func fieldsAfter(params []byte, n int) string {
	fs := bytes.Fields(params)
	if len(fs) <= n {
		return ""
	}
	out := make([]string, 0, len(fs)-n)
	for i := n; i < len(fs); i++ {
		out = append(out, string(fs[i]))
	}
	return strings.Join(out, " ")
}

func isCTCPAction(text string) bool {
	t := strings.TrimSpace(text)
	return strings.HasPrefix(t, "\x01ACTION") && strings.HasSuffix(t, "\x01")
}

func ctcpActionText(text string) string {
	t := strings.TrimSpace(text)
	t = strings.TrimPrefix(t, "\x01ACTION")
	t = strings.TrimSuffix(t, "\x01")
	t = strings.TrimSpace(t)
	return t
}

// Format converts one IRC line into zero or more log entries. outbound indicates
// a line we sent. hasEcho indicates echo-message negotiated (skip own echoes).
func Format(tr *Tracker, now interface{}, line []byte, outbound bool, hasEcho bool, server string) []Entry {
	_ = now
	entries := []Entry{}

	tags, rest := ircline.Tags(line)
	prefix, rest := ircline.Prefix(rest)
	verb := strings.ToUpper(string(ircline.Verb(rest)))
	params := ircline.Params(rest)

	if outbound {
		v := verb
		if strings.EqualFold(v, "PRIVMSG") {
			target, text := splitTargetText(params)
			own := tr.OwnNick()
			window := sanitizeWindow(target)
			var body string
			if isCTCPAction(text) {
				body = fmt.Sprintf("* %s %s", own, ctcpActionText(text))
			} else {
				body = fmt.Sprintf("<%s> %s", own, text)
			}
			entries = append(entries, Entry{Window: window, Body: body})
		} else if strings.EqualFold(v, "NOTICE") {
			target, text := splitTargetText(params)
			own := tr.OwnNick()
			window := sanitizeWindow(target)
			body := fmt.Sprintf("-%s- %s", own, text)
			entries = append(entries, Entry{Window: window, Body: body})
		}
		return entries
	}

	// skip own echoes
	if hasEcho && tr.OwnNick() != "" {
		if verb == "PRIVMSG" || verb == "NOTICE" {
			n := nickFromPrefix(prefix)
			if bytes.EqualFold([]byte(n), []byte(tr.OwnNick())) {
				return nil
			}
		}
	}

	nick := nickFromPrefix(prefix)
	uh := userhostFromPrefix(prefix)

	switch verb {
	case "PRIVMSG":
		target, text := splitTargetText(params)
		if isCTCPAction(text) {
			msg := ctcpActionText(text)
			if isChannelTarget(target) {
				entries = append(entries, Entry{Window: sanitizeWindow(target), Body: fmt.Sprintf("* %s %s", nick, msg)})
			} else {
				entries = append(entries, Entry{Window: sanitizeWindow(nick), Body: fmt.Sprintf("* %s %s", nick, msg)})
			}
		} else {
			if isChannelTarget(target) {
				entries = append(entries, Entry{Window: sanitizeWindow(target), Body: fmt.Sprintf("<%s> %s", nick, text)})
			} else {
				entries = append(entries, Entry{Window: sanitizeWindow(nick), Body: fmt.Sprintf("<%s> %s", nick, text)})
			}
		}
	case "NOTICE":
		target, text := splitTargetText(params)
		body := fmt.Sprintf("-%s- %s", nick, text)
		if isChannelTarget(target) {
			entries = append(entries, Entry{Window: sanitizeWindow(target), Body: body})
		} else {
			entries = append(entries, Entry{Window: sanitizeWindow(nick), Body: body})
		}
	case "JOIN":
		channel := firstField(params)
		// account detection
		account := ""
		fs := bytes.Fields(params)
		// extended-join: trailing realname
		_ = trailingField(params)
		// check if last field came from colon
		hasTrailing := bytes.IndexByte(params, ':') >= 0
		if hasTrailing && len(fs) >= 2 {
			// account is fs[1] if there are at least 3 fields
			if len(fs) >= 3 {
				acct := string(fs[1])
				if acct != "*" && acct != "" {
					account = acct
				}
			}
		}
		// tag account
		if tags != nil {
			// parse account tag
			tv := tagValue(tags, []byte("account"))
			if tv != nil {
				a := string(tv)
				if a != "" && a != "*" {
					account = a
				} else if a == "*" {
					account = ""
				}
			}
		}
		body := fmt.Sprintf("*** Joins: %s (%s)", nick, uh)
		if account != "" {
			body += " " + account
		}
		entries = append(entries, Entry{Window: sanitizeWindow(channel), Body: body})
	case "PART":
		channel := firstField(params)
		reason := trailingField(params)
		entries = append(entries, Entry{Window: sanitizeWindow(channel), Body: fmt.Sprintf("*** Parts: %s (%s) (%s)", nick, uh, reason)})
	case "QUIT":
		reason := trailingField(params)
		body := fmt.Sprintf("*** Quits: %s (%s) (%s)", nick, uh, reason)
		chans := tr.ChannelsWithMember(nick)
		// case-insensitive member lookup
		if len(chans) == 0 {
			// try case-insensitive
			for ch, ms := range trMembersMap(tr) {
				for m := range ms {
					if bytes.EqualFold([]byte(m), []byte(nick)) {
						chans = append(chans, ch)
					}
				}
			}
		}
		if len(chans) == 0 {
			entries = append(entries, Entry{Window: "status", Body: body})
		} else {
			// unique
			seen := map[string]bool{}
			for _, ch := range chans {
				if seen[ch] {
					continue
				}
				seen[ch] = true
				entries = append(entries, Entry{Window: sanitizeWindow(ch), Body: body})
			}
		}
	case "NICK":
		newNick := firstField(params)
		if newNick == "" {
			newNick = trailingField(params)
		}
		body := fmt.Sprintf("*** %s is now known as %s", nick, newNick)
		chans := tr.ChannelsWithMember(nick)
		if len(chans) == 0 {
			// case-insensitive
			for ch, ms := range trMembersMap(tr) {
				for m := range ms {
					if bytes.EqualFold([]byte(m), []byte(nick)) {
						chans = append(chans, ch)
					}
				}
			}
		}
		if len(chans) == 0 {
			entries = append(entries, Entry{Window: "status", Body: body})
		} else {
			seen := map[string]bool{}
			for _, ch := range chans {
				if seen[ch] {
					continue
				}
				seen[ch] = true
				entries = append(entries, Entry{Window: sanitizeWindow(ch), Body: body})
			}
		}
	case "TOPIC":
		channel := firstField(params)
		topic := trailingField(params)
		entries = append(entries, Entry{Window: sanitizeWindow(channel), Body: fmt.Sprintf("*** %s changes topic to '%s'", nick, topic)})
	case "KICK":
		channel := firstField(params)
		kicked := secondField(params)
		op := nick
		reason := ""
		fs := bytes.Fields(params)
		if len(fs) >= 3 {
			reason = trailingField(params)
		}
		entries = append(entries, Entry{Window: sanitizeWindow(channel), Body: fmt.Sprintf("*** %s was kicked by %s (%s)", kicked, op, reason)})
	case "MODE":
		target := firstField(params)
		mode := secondField(params)
		args := fieldsAfter(params, 2)
		op := nick
		if op == "" {
			op = "Server"
		}
		window := "status"
		if isChannelTarget(target) {
			window = sanitizeWindow(target)
		}
		body := fmt.Sprintf("*** %s sets mode: %s %s", op, mode, args)
		entries = append(entries, Entry{Window: window, Body: body})
	case "001":
		entries = append(entries, Entry{Window: "status", Body: fmt.Sprintf("Connected to IRC (%s)", server)})
	default:
		// numerics, MOTD, ERROR, server notices not otherwise handled -> status
		// Only log server-originated numerics and ERROR/PING-class lines.
		if verb == "ERROR" || verb == "PING" || isNumeric(verb) {
			// Drop transport tags (e.g. @time=...) from the body; keep raw line
			// untouched on the wire and in the ring.
			_, rest := ircline.Tags(line)
			entries = append(entries, Entry{Window: "status", Body: string(bytes.TrimSpace(rest))})
		}
	}
	_ = tags
	_ = uh
	_ = sort.Strings
	return entries
}

func isNumeric(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func tagValue(tags, key []byte) []byte {
	// simple parse key=value
	i := 0
	for i < len(tags) {
		j := i
		for j < len(tags) && tags[j] != ';' {
			j++
		}
		seg := tags[i:j]
		if eq := bytes.IndexByte(seg, '='); eq >= 0 {
			if bytes.EqualFold(seg[:eq], key) {
				return seg[eq+1:]
			}
		} else {
			if bytes.EqualFold(seg, key) {
				return []byte{}
			}
		}
		if j >= len(tags) {
			break
		}
		i = j + 1
	}
	return nil
}

func trMembersMap(tr *Tracker) map[string]map[string]bool {
	// access unexported via same package
	return tr.members
}
