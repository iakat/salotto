package logfile

import (
	"bytes"
	"sort"
	"strings"

	"github.com/iakat/salotto/internal/ircline"
)

// Tracker is the minimum read-only IRC state needed to reproduce ZNC's
// fan-out. It observes the same line stream as the logger.
type Tracker struct {
	ownNick  string
	channels map[string]bool
	members  map[string]map[string]bool
	topic    map[string]string
}

func NewTracker(initial ...string) *Tracker {
	initialNick := ""
	if len(initial) > 0 {
		initialNick = initial[0]
	}
	t := &Tracker{
		channels: make(map[string]bool),
		members:  make(map[string]map[string]bool),
		topic:    make(map[string]string),
	}
	t.ownNick = initialNick
	return t
}

func NewTracker0() *Tracker { return NewTracker("") }

func (t *Tracker) OwnNick() string { return t.ownNick }

func (t *Tracker) SetOwnNick(n string) { t.ownNick = n }

func (t *Tracker) Channels() []string {
	out := make([]string, 0, len(t.channels))
	for c := range t.channels {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func (t *Tracker) Members(ch string) map[string]bool {
	if m, ok := t.members[ch]; ok {
		return m
	}
	return nil
}

func (t *Tracker) IsMember(ch, nick string) bool {
	if m, ok := t.members[ch]; ok {
		return m[nick]
	}
	return false
}

func (t *Tracker) Topic(ch string) string { return t.topic[ch] }

func (t *Tracker) ChannelsWithMember(nick string) []string {
	var out []string
	for ch, ms := range t.members {
		if ms[nick] {
			out = append(out, ch)
		}
	}
	// deterministic order
	sort.Strings(out)
	return out
}

func (t *Tracker) Clear() {
	t.ownNick = ""
	t.channels = make(map[string]bool)
	t.members = make(map[string]map[string]bool)
	t.topic = make(map[string]string)
}

func (t *Tracker) Observe(line []byte) {
	_, rest := ircline.Tags(line)
	prefix, rest := ircline.Prefix(rest)
	verb := strings.ToUpper(string(ircline.Verb(rest)))
	params := ircline.Params(rest)

	nick := nickOfPrefix(prefix)

	switch verb {
	case "NICK":
		newNick := strings.TrimSpace(string(params))
		newNick = strings.TrimPrefix(newNick, ":")
		newNick = strings.TrimSpace(newNick)
		if newNick == "" {
			fields := bytes.Fields(params)
			if len(fields) > 0 {
				newNick = string(fields[0])
			}
		}
		if bytes.EqualFold([]byte(nick), []byte(t.ownNick)) {
			t.ownNick = newNick
		}
		for _, ms := range t.members {
			if ms[nick] {
				delete(ms, nick)
				ms[newNick] = true
			}
		}
	case "JOIN":
		chans := splitComma(firstParamStr(params))
		for _, ch := range chans {
			if bytes.EqualFold([]byte(nick), []byte(t.ownNick)) {
				t.channels[ch] = true
			}
			if t.members[ch] == nil {
				t.members[ch] = make(map[string]bool)
			}
			t.members[ch][nick] = true
		}
	case "PART":
		chans := splitComma(firstParamStr(params))
		for _, ch := range chans {
			if bytes.EqualFold([]byte(nick), []byte(t.ownNick)) {
				delete(t.channels, ch)
			}
			if m, ok := t.members[ch]; ok {
				delete(m, nick)
			}
		}
	case "KICK":
		fields := bytes.Fields(params)
		if len(fields) >= 2 {
			ch := string(fields[0])
			kicked := string(fields[1])
			if bytes.EqualFold([]byte(kicked), []byte(t.ownNick)) {
				delete(t.channels, ch)
			}
			if m, ok := t.members[ch]; ok {
				delete(m, kicked)
			}
		}
	case "QUIT":
		for _, ms := range t.members {
			delete(ms, nick)
		}
	case "TOPIC":
		fields := bytes.Fields(params)
		if len(fields) >= 1 {
			ch := string(fields[0])
			t.topic[ch] = trailingStr(params)
		}
	case "332":
		fields := bytes.Fields(params)
		if len(fields) >= 2 {
			ch := string(fields[1])
			t.topic[ch] = trailingStr(params)
		}
	case "353":
		fields := bytes.Fields(params)
		if len(fields) >= 3 {
			ch := string(fields[2])
			if t.members[ch] == nil {
				t.members[ch] = make(map[string]bool)
			}
			names := trailingStr(params)
			for _, n := range strings.Fields(names) {
				n = strings.TrimLeft(n, "@+&~%!")
				if n == "" {
					continue
				}
				t.members[ch][n] = true
			}
		}
	case "001":
		fields := bytes.Fields(params)
		if len(fields) >= 1 {
			t.ownNick = string(fields[0])
		}
	}
}

func nickOfPrefix(prefix []byte) string {
	s := string(prefix)
	if i := bytes.IndexByte(prefix, '!'); i >= 0 {
		return s[:i]
	}
	return s
}

func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstParamStr(params []byte) string {
	f := ircline.FirstParam(params)
	return string(f)
}

func trailingStr(params []byte) string {
	s := string(params)
	if i := bytes.IndexByte(params, ':'); i >= 0 {
		return s[i+1:]
	}
	fields := bytes.Fields(params)
	if len(fields) == 0 {
		return ""
	}
	return string(fields[len(fields)-1])
}
