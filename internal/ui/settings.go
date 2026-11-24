package ui

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/iakat/salotto/internal/ircline"
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

	// unknown holds every key in the document that this build has no field for. It exists
	// because the settings document is shared: one bouncer blob is fanned out to every client
	// attached to the account, and those clients are not necessarily the same build. A client
	// that dropped the keys it did not recognise would delete a newer client's settings the
	// first time the user pressed a key in the older one - and the older client would look
	// innocent, because it only ever wrote what it could see.
	//
	// Kept as raw JSON rather than as a map of strings so a value of any shape (an object, a
	// list, a number) round-trips byte for byte: reformatting someone else's key is a change
	// to it, however faithful the intent.
	unknown map[string]json.RawMessage
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

// dropWord removes a word from a list, case-insensitively. It reports whether the word was
// there, so a caller can say "not in the list" instead of silently doing nothing - which is how
// a rule a user believes they removed stays in effect.
func dropWord(words []string, word string) ([]string, bool) {
	word = strings.TrimSpace(word)
	if word == "" {
		return words, false
	}
	for i, w := range words {
		if strings.EqualFold(w, word) {
			return append(words[:i:i], words[i+1:]...), true
		}
	}
	return words, false
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
		// A leading "-" removes one word, because a list with no way to remove an entry is a
		// list that only grows: the screen can add, and before this the only way to take a
		// word out was to clear the whole list and type the others back. "none" still clears
		// everything, which is a different statement and stays spelled differently.
		if word, remove := strings.CutPrefix(value, "-"); remove {
			next, ok := dropWord(s.Highlights, word)
			if !ok {
				return fmt.Errorf("%s is not in the highlights", word)
			}
			s.Highlights = next
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
		if word, remove := strings.CutPrefix(value, "-"); remove {
			next, ok := dropWord(s.Mutes, word)
			if !ok {
				return fmt.Errorf("%s is not in the mutes", word)
			}
			s.Mutes = next
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
// cannot make an older client's file unreadable.
//
// Decoding over defaults was NOT enough to keep a newer client's keys, and this comment used to
// claim it was. A field the struct does not have is not kept by the decoder - it is dropped by
// the next marshal - so an older client read a newer blob, changed one boolean, and pushed back
// a document with every unfamiliar key gone. The keys live in s.unknown now, and token() splices
// them back.
func decodeSettings(data []byte) Settings {
	s := DefaultSettings()
	if err := json.Unmarshal(data, &s); err != nil {
		return DefaultSettings()
	}
	s.unknown = unknownKeys(data)
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
// maxPrefsToken is the largest token this client will put on the wire.
//
// It is the bouncer's blob cap as base64 plus room for the verb and the tags, computed from the
// shared constant rather than configured: a client-side number that drifts from the daemon's is a
// client that either refuses something the bouncer would accept or sends something the bouncer
// rejects, and the second case used to end with a fragment in the settings store.
const maxPrefsToken = (ircline.MaxPrefsBytes+2)/3*4 + 1024

func (s Settings) token() (string, error) {
	// Marshal the struct, then splice back the keys this build does not know. Doing it this way
	// round - marshalling our own fields and merging - rather than "marshal the original
	// document and overwrite what changed" keeps one source of truth for our own keys: the
	// struct. The merge only ever adds keys, so it cannot resurrect a value we deliberately
	// wrote as null or empty.
	data, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	if len(s.unknown) > 0 {
		data, err = withUnknown(data, s.unknown)
		if err != nil {
			return "", err
		}
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// knownKeys is the set of JSON names this build understands, read off the struct tags.
//
// Derived by reflection rather than written out, because a hand-kept list is a second place to
// update when a field is added and the failure mode of forgetting is invisible: the new field's
// key would be treated as foreign and round-tripped as opaque, which works until two clients of
// the same build disagree about which one owns it.
func knownKeys() map[string]bool {
	keys := make(map[string]bool)
	t := reflect.TypeOf(Settings{})
	for i := 0; i < t.NumField(); i++ {
		// strings.Cut reports ok=false when there is no comma, which is the ordinary case: a
		// plain `json:"timestamps"` has no options. Testing ok here instead of the name read
		// every plain field as untagged and every one of its keys as foreign - which showed up
		// immediately, as a saved document with "timestamps" and "highlights" in it twice.
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		// A field with no tag, one the tag hides with "-", or an unexported field (which has no
		// tag at all) is not on the wire.
		if name == "" || name == "-" || !t.Field(i).IsExported() {
			continue
		}
		keys[name] = true
	}
	return keys
}

// unknownKeys returns the entries of a settings document that this build has no field for.
func unknownKeys(data []byte) map[string]json.RawMessage {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil
	}
	known := knownKeys()
	out := make(map[string]json.RawMessage)
	for k, v := range all {
		if !known[k] {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// withUnknown appends the foreign keys to an encoded document.
//
// The splice is textual because json.Marshal returns a single-line object ending in "}": drop
// that brace, add a comma and the preserved entries, close it again. Going through a generic
// map instead would re-order and re-indent every key on every save, so a client that only
// changed one boolean would rewrite the whole document in a different shape - noise in every
// diff, and a value that changes representation when nobody asked it to.
func withUnknown(data []byte, unknown map[string]json.RawMessage) ([]byte, error) {
	keys := make([]string, 0, len(unknown))
	for k := range unknown {
		keys = append(keys, k)
	}
	// Sorted so the output is stable: two saves of the same settings produce the same bytes,
	// which is what makes the file difffable and the tests deterministic.
	sort.Strings(keys)

	var b strings.Builder
	b.Write(data[:len(data)-1]) // the closing brace
	for _, k := range keys {
		name, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		b.WriteByte(',')
		b.Write(name)
		b.WriteByte(':')
		b.Write(unknown[k])
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

// SaveSettings writes the settings file. It writes a temporary file and renames it, so a
// crash mid-write cannot leave a half-written file that the next start cannot parse.
func SaveSettings(path string, s Settings) error {
	if path == "" {
		return nil
	}
	// The file is the offline copy of the same shared document, so it needs the same forward
	// preservation the wire does: someone running without a bouncer - a plain IRCd, or a client
	// that has not connected yet - is exactly who upgrades a build and finds the file written by
	// the newer one.
	var data []byte
	var err error
	if len(s.unknown) > 0 {
		// Merge into the compact form first, then indent: indenting a textual splice would
		// leave the foreign values on one line inside an otherwise indented document.
		compact, cerr := s.token()
		if cerr != nil {
			return cerr
		}
		raw, derr := base64.StdEncoding.DecodeString(compact)
		if derr != nil {
			return derr
		}
		var buf bytes.Buffer
		if err := json.Indent(&buf, raw, "", "  "); err != nil {
			return err
		}
		data = buf.Bytes()
	} else {
		data, err = json.MarshalIndent(s, "", "  ")
		if err != nil {
			return err
		}
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
