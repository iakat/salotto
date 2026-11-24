package ui

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Settings is the client's own configuration: how this terminal looks and behaves.
//
// It is deliberately separate from the bouncer's settings. A bouncer setting changes what
// happens on the wire for every client attached to that account; these change what this
// window looks like. Mixing them in one namespace is how a user ends up surprised that
// turning something off in a browser changed their phone, so the split is enforced by being
// two commands (`/fset` here, `*status`/`*controlplane` there) rather than by documentation.
type Settings struct {
	// Timestamps shows HH:MM or HH:MM:SS before each line.
	Timestamps bool `json:"timestamps"`
	// Seconds widens the timestamp to HH:MM:SS. Separate from Timestamps so the common
	// case - "I want the time, not a wall of digits" - is the one with fewer glyphs.
	Seconds bool `json:"seconds"`
	// DateRule inserts a separator when the conversation crosses midnight.
	DateRule bool `json:"date_rule"`
	// NickColors gives every nick a stable colour derived from its name. Same nick, same
	// colour, in every window and on every reconnect, because it is a hash and not an
	// allocation order.
	NickColors bool `json:"nick_colors"`
	// Hotlist makes Alt-A jump to the next window with unread activity.
	Hotlist bool `json:"hotlist"`
	// ScrollStep is how many lines a wheel notch moves.
	ScrollStep int `json:"scroll_step"`
	// Aliases are command shortcuts: an alias is expanded before the line is parsed, so
	// "/j #chan" and "/join #chan" are the same command rather than two implementations.
	Aliases map[string]string `json:"aliases"`

	// Mutes are nicks whose lines do not light a window up. A muted line is still
	// drawn: hiding it would mean a channel that looks idle while someone is talking to
	// you, and "why did nobody answer" is a worse failure than "that person is noisy".
	// Mute suppresses the signal, not the text.
	Mutes []string `json:"mutes"`
	// Highlights are extra words that light a window up as if they were our nick. The
	// use is a project name, a nickname you answer to, a word you are waiting for - not
	// a second mention system, which is why it shares the mention colour rather than
	// getting a colour of its own.
	Highlights []string `json:"highlights"`

	// GroupByServer draws the window list under a heading per server, with the windows
	// sorted inside each group. On by default because a bouncer with two networks otherwise
	// shows one flat list in which two windows called "status" are indistinguishable; turning
	// it off is the flat arrival-order list the client had before, which is what someone who
	// learnt the old layout wants back.
	GroupByServer bool `json:"group_by_server"`
	// BufferNumbers prefixes every window with its weechat-style number, which is also the
	// argument to /b. On by default: the numbers are the only stable name a buffer has, and
	// they cost one column.
	BufferNumbers bool `json:"buffer_numbers"`
}

// DefaultSettings is what a new installation gets. The defaults are the ones that make a
// buffer readable without configuration: timestamps on, day rules on, nick colours on.
func DefaultSettings() Settings {
	return Settings{
		Timestamps: true,
		Seconds:    true,
		DateRule:   true,
		NickColors: true,
		Hotlist:    true,
		ScrollStep: 3,
		Aliases:    map[string]string{"j": "join", "q": "query", "m": "msg", "w": "go"},
		// Both on: the user asked for grouping and numbers, so a fresh client is the one they
		// asked for. They are settings, not constants, so the old flat list is one /fset away.
		GroupByServer: true,
		BufferNumbers: true,
	}
}

// settingRow is one editable row, as the settings screen and /fset both see it.
//
// The screen and the command are the same list on purpose: a setting that exists in one and
// not the other is a setting half the users cannot reach, and the two drifting is what makes
// "it is in the settings" untrue.
type settingRow struct {
	key   string
	label string
	value string
	// kind says how a value is read back: "bool" toggles, "int" parses a number, "text"
	// takes the rest of the line.
	kind string
}

// rows lists the settings in display order.
func (s Settings) rows() []settingRow {
	onOff := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}
	return []settingRow{
		{"timestamps", "timestamps", onOff(s.Timestamps), "bool"},
		{"seconds", "seconds", onOff(s.Seconds), "bool"},
		{"date_rule", "date rules", onOff(s.DateRule), "bool"},
		{"nick_colors", "nick colours", onOff(s.NickColors), "bool"},
		{"hotlist", "hotlist", onOff(s.Hotlist), "bool"},
		{"group_by_server", "group by server", onOff(s.GroupByServer), "bool"},
		{"buffer_numbers", "buffer numbers", onOff(s.BufferNumbers), "bool"},
		{"scroll_step", "scroll step", strconv.Itoa(s.ScrollStep), "int"},
		{"highlight", "highlights", listOrNone(s.Highlights), "text"},
		{"mute", "mutes", listOrNone(s.Mutes), "text"},
	}
}

// addWord adds a word to a list, case-insensitively and without duplicates. A settings
// list that accumulates "ZNC" and "znc" would silently keep two rules that mean one thing,
// and removing either would look like it did nothing.
func addWord(words []string, word string) []string {
	word = strings.TrimSpace(word)
	if word == "" {
		return words
	}
	for _, w := range words {
		if strings.EqualFold(w, word) {
			return words
		}
	}
	return append(words, word)
}

// matchedBy reports whether text contains any of words as a whole word. It is the same
// rule mentions use, so a highlight and a mention cannot disagree about whether "alice"
// inside "malice" counts.
func matchedBy(text string, words []string) bool {
	for _, w := range words {
		if wordIn(text, w) {
			return true
		}
	}
	return false
}

// listOrNone renders a word list for the settings screen. An empty list says "none"
// rather than showing a blank column, because a blank cell reads as "this setting is
// broken" and an empty highlight list is the normal state of a new client.
func listOrNone(words []string) string {
	if len(words) == 0 {
		return "none"
	}
	return strings.Join(words, " ")
}

// canonical maps every accepted spelling to the one name a row uses. It is the single place
// an alias like "dates" or "colours" becomes a setting name, so /fset, the settings screen
// and the read-back cannot disagree about what a key is called.
func canonical(key string) string {
	switch key = strings.ToLower(strings.TrimSpace(key)); key {
	case "date_rules", "dates":
		return "date_rule"
	case "nick_colours", "colors", "colours":
		return "nick_colors"
	case "scroll":
		return "scroll_step"
	case "highlights":
		return "highlight"
	case "mutes":
		return "mute"
	case "groups", "group", "servers", "group_by_server", "groupby":
		return "group_by_server"
	case "numbers", "buffer_number", "buffers", "buffer_no":
		return "buffer_numbers"
	}
	return key
}

// set applies one key. It returns an error rather than ignoring an unknown name, because a
// typo that silently does nothing is indistinguishable from a setting that does not work.
func (s *Settings) set(key, value string) error {
	key = canonical(key)
	value = strings.TrimSpace(value)
	boolean := func(dst *bool) error {
		switch strings.ToLower(value) {
		case "on", "true", "yes", "1", "":
			*dst = !*dst
			if value != "" {
				*dst = true
			}
		case "off", "false", "no", "0":
			*dst = false
		default:
			return fmt.Errorf("%s takes on or off", key)
		}
		return nil
	}
	switch key {
	case "timestamps":
		return boolean(&s.Timestamps)
	case "seconds":
		return boolean(&s.Seconds)
	case "date_rule", "date_rules", "dates":
		return boolean(&s.DateRule)
	case "nick_colors", "nick_colours", "colors", "colours":
		return boolean(&s.NickColors)
	case "hotlist":
		return boolean(&s.Hotlist)
	case "group_by_server":
		return boolean(&s.GroupByServer)
	case "buffer_numbers":
		return boolean(&s.BufferNumbers)
	case "scroll_step", "scroll":
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 50 {
			return fmt.Errorf("scroll_step takes a number between 1 and 50")
		}
		s.ScrollStep = n
		return nil
	case "highlight", "highlights":
		switch strings.ToLower(value) {
		case "", "none", "clear":
			s.Highlights = nil
			return nil
		}
		s.Highlights = addWord(s.Highlights, value)
		return nil
	case "mute", "mutes":
		switch strings.ToLower(value) {
		case "", "none", "clear":
			s.Mutes = nil
			return nil
		}
		s.Mutes = addWord(s.Mutes, value)
		return nil
	default:
		return fmt.Errorf("no setting called %s", key)
	}
}

// applyAlias recognises an alias definition: "name target".
func (s *Settings) applyAlias(rest string) error {
	name, target, ok := strings.Cut(strings.TrimSpace(rest), " ")
	if !ok || name == "" || strings.TrimSpace(target) == "" {
		return fmt.Errorf("usage: /alias <name> <command>")
	}
	name = strings.TrimPrefix(name, "/")
	if s.Aliases == nil {
		s.Aliases = make(map[string]string)
	}
	// The target is stored without its leading slash: the expansion puts it back, and
	// storing it both ways is how "/j" ends up expanding to "//join".
	s.Aliases[name] = strings.TrimPrefix(strings.TrimSpace(target), "/")
	return nil
}

// expand rewrites a line whose first word is an alias. It repeats until nothing changes, so
// an alias may point at another alias, with a depth bound: a cycle is a mistake, and the
// bound is what turns "the client hung" into "the client ignored it".
func (s Settings) expand(text string, depth int) string {
	if depth > 8 || !strings.HasPrefix(text, "/") {
		return text
	}
	body := strings.TrimPrefix(text, "/")
	word, rest, _ := strings.Cut(body, " ")
	target, ok := s.Aliases[word]
	if !ok {
		return text
	}
	next := "/" + target
	if rest != "" {
		next += " " + rest
	}
	return s.expand(next, depth+1)
}

// aliasList renders the aliases sorted, for /alias with no arguments.
func (s Settings) aliasList() []string {
	names := make([]string, 0, len(s.Aliases))
	for name := range s.Aliases {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, fmt.Sprintf("/%s -> /%s", name, s.Aliases[name]))
	}
	return out
}

// decodeSettings is where a settings document becomes a Settings, from the file and from the
// bouncer's PREF blob alike, and both callers go through here on purpose: a client that
// decoded the wire one way and the file another would have two answers to "what are my
// settings", and the divergence would show up as a value that survives a restart but not a
// reconnect.
//
// Decoding over the defaults is what makes an older document usable: a client that predates a
// key keeps the default for it instead of zeroing it, so a setting added by a newer client
// cannot make an older client's file unreadable and an older blob cannot silently clear a
// newer client's keys.
func decodeSettings(data []byte) Settings {
	s := DefaultSettings()
	if err := json.Unmarshal(data, &s); err != nil {
		return DefaultSettings()
	}
	if s.ScrollStep < 1 {
		s.ScrollStep = DefaultSettings().ScrollStep
	}
	if s.Aliases == nil {
		s.Aliases = DefaultSettings().Aliases
	}
	return s
}

// LoadSettings reads a settings file, falling back to the defaults. A missing file is not an
// error: the first run of a new installation has no file, and refusing to start over that
// would be a strange way to greet someone.
func LoadSettings(path string) Settings {
	if path == "" {
		return DefaultSettings()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return DefaultSettings()
	}
	return decodeSettings(data)
}

// token is the wire form of the settings: their JSON, base64-encoded so the
// whole document survives as one IRC parameter. A raw JSON blob happens to have
// no spaces after json.Marshal today, but a value containing one - or a future
// MarshalIndent - would be truncated at the first space, so the encoding is
// not decoration.
func (s Settings) token() (string, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// SaveSettings writes the settings file. It writes a temporary file and renames it, so a
// crash mid-write cannot leave a half-written file that the next start cannot parse.
func SaveSettings(path string, s Settings) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// nickPalette is the set of 256-colour indices a nickname may take.
//
// Mid-range and saturated: dark enough to read on a light terminal and light enough to read
// on a dark one, and spread across the wheel so that neighbours in a member list do not
// usually collide. The palette is fixed rather than computed so that every terminal shows the
// same colours - a hash into the whole cube would land on near-black and near-white too often.
var nickPalette = []string{
	"204", "114", "180", "147", "222", "141", "203", "150",
	"186", "117", "175", "121", "215", "156", "210", "153",
}

// nickColor is the colour for a nickname: a stable hash into the palette. Stability is the
// whole point - a colour that changed per render would be noise, and one that changed per
// reconnect would make people unable to recognise each other.
func nickColor(nick string) lipgloss.Style {
	// Case-folded: on IRC "Alice" and "alice" are the same person, and giving them two
	// colours is a bug a user sees every day.
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.ToLower(nick)))
	return lipgloss.NewStyle().Foreground(lipgloss.Color(nickPalette[h.Sum32()%uint32(len(nickPalette))]))
}

// nickText renders a nickname with its colour and the prefix the channel gave it ("@", "+").
func nickText(m Model, nick, prefix string) string {
	style := nickStyle
	if m.settings.NickColors {
		style = nickColor(nick)
	}
	if prefix == "@" || prefix == "~" || prefix == "&" {
		style = style.Bold(true)
	}
	return style.Render(prefix + nick)
}
