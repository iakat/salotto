package ircline

import (
	"bytes"
	"testing"
)

func TestParseLines(t *testing.T) {
	tests := []struct {
		name          string
		line          string
		wantTags      string
		wantPrefix    string
		wantVerb      string
		wantParams    string
		wantFirst     string
		wantTagsNil   bool
		wantPrefixNil bool
		wantFirstNil  bool
		wantIsPing    bool
	}{
		{
			name:          "empty",
			line:          "",
			wantTagsNil:   true,
			wantPrefixNil: true,
			wantFirstNil:  true,
		},
		{
			name:          "command only",
			line:          "PING",
			wantVerb:      "PING",
			wantTagsNil:   true,
			wantPrefixNil: true,
			wantFirstNil:  true,
			wantIsPing:    true,
		},
		{
			name:          "prefix and command",
			line:          ":irc.example.net 001 nick :Welcome to the network",
			wantPrefix:    "irc.example.net",
			wantVerb:      "001",
			wantParams:    "nick :Welcome to the network",
			wantFirst:     "nick",
			wantTagsNil:   true,
			wantPrefixNil: false,
		},
		{
			name:          "tags prefix command and trailing param",
			line:          "@time=2026-01-01T00:00:00Z;msgid=abc :nick!user@host PRIVMSG #chan :hello world",
			wantTags:      "time=2026-01-01T00:00:00Z;msgid=abc",
			wantPrefix:    "nick!user@host",
			wantVerb:      "PRIVMSG",
			wantParams:    "#chan :hello world",
			wantFirst:     "#chan",
			wantTagsNil:   false,
			wantPrefixNil: false,
		},
		{
			name:          "value-less tag",
			line:          "@draft/foo;time=123 :srv PING :token",
			wantTags:      "draft/foo;time=123",
			wantPrefix:    "srv",
			wantVerb:      "PING",
			wantParams:    ":token",
			wantFirst:     ":token",
			wantTagsNil:   false,
			wantPrefixNil: false,
			wantIsPing:    true,
		},
		{
			name:          "only tags",
			line:          "@time=1;msgid=abc",
			wantTags:      "time=1;msgid=abc",
			wantTagsNil:   false,
			wantPrefixNil: true,
			wantFirstNil:  true,
		},
		{
			name:          "only prefix",
			line:          ":irc.example.net",
			wantPrefix:    "irc.example.net",
			wantTagsNil:   true,
			wantPrefixNil: false,
			wantFirstNil:  true,
		},
		{
			name:          "multiple spaces between params",
			line:          "PRIVMSG   #chan   :hello   world",
			wantVerb:      "PRIVMSG",
			wantParams:    "#chan   :hello   world",
			wantFirst:     "#chan",
			wantTagsNil:   true,
			wantPrefixNil: true,
		},
		{
			name:          "multiple spaces after prefix",
			line:          ":srv   PING   :token",
			wantPrefix:    "srv",
			wantVerb:      "PING",
			wantParams:    ":token",
			wantFirst:     ":token",
			wantTagsNil:   true,
			wantPrefixNil: false,
			wantIsPing:    true,
		},
		{
			name:          "leading spaces before command",
			line:          "   PING :token",
			wantVerb:      "PING",
			wantParams:    ":token",
			wantFirst:     ":token",
			wantTagsNil:   true,
			wantPrefixNil: true,
			wantIsPing:    true,
		},
		{
			name:          "trailing carriage return",
			line:          "PING :token\r",
			wantVerb:      "PING",
			wantParams:    ":token",
			wantFirst:     ":token",
			wantTagsNil:   true,
			wantPrefixNil: true,
			wantIsPing:    true,
		},
		{
			name:          "tags only with carriage return",
			line:          "@a=b\r",
			wantTags:      "a=b",
			wantTagsNil:   false,
			wantPrefixNil: true,
			wantFirstNil:  true,
		},
		{
			name:          "prefix only with carriage return",
			line:          ":srv\r",
			wantPrefix:    "srv",
			wantTagsNil:   true,
			wantPrefixNil: false,
			wantFirstNil:  true,
		},
		{
			name:          "only spaces",
			line:          "   ",
			wantTagsNil:   true,
			wantPrefixNil: true,
			wantFirstNil:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tags, rest := Tags([]byte(tc.line))
			prefix, rest := Prefix(rest)
			verb := Verb(rest)
			params := Params(rest)
			first := FirstParam(params)
			isPing := IsVerb(rest, "PING")

			if got := string(tags); got != tc.wantTags {
				t.Fatalf("Tags = %q, want %q", got, tc.wantTags)
			}
			if (tags == nil) != tc.wantTagsNil {
				t.Fatalf("Tags nil = %v, want %v", tags == nil, tc.wantTagsNil)
			}
			if got := string(prefix); got != tc.wantPrefix {
				t.Fatalf("Prefix = %q, want %q", got, tc.wantPrefix)
			}
			if (prefix == nil) != tc.wantPrefixNil {
				t.Fatalf("Prefix nil = %v, want %v", prefix == nil, tc.wantPrefixNil)
			}
			if got := string(verb); got != tc.wantVerb {
				t.Fatalf("Verb = %q, want %q", got, tc.wantVerb)
			}
			if got := string(params); got != tc.wantParams {
				t.Fatalf("Params = %q, want %q", got, tc.wantParams)
			}
			if got := string(first); got != tc.wantFirst {
				t.Fatalf("FirstParam = %q, want %q", got, tc.wantFirst)
			}
			if (first == nil) != tc.wantFirstNil {
				t.Fatalf("FirstParam nil = %v, want %v", first == nil, tc.wantFirstNil)
			}
			if got := isPing; got != tc.wantIsPing {
				t.Fatalf("IsVerb = %v, want %v", got, tc.wantIsPing)
			}
		})
	}
}

func TestTagValue(t *testing.T) {
	tags, _ := Tags([]byte("@time=2026-01-01T00:00:00Z;msgid=abc;draft/foo;empty= :srv PRIVMSG #c :hi"))

	tests := []struct {
		name    string
		key     string
		want    string
		wantNil bool
	}{
		{name: "value", key: "time", want: "2026-01-01T00:00:00Z"},
		{name: "case insensitive key", key: "MSGID", want: "abc"},
		{name: "value-less tag", key: "draft/foo"},
		{name: "empty value", key: "empty"},
		{name: "missing", key: "nope", wantNil: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := TagValue(tags, []byte(tc.key))
			if tc.wantNil {
				if got != nil {
					t.Fatalf("TagValue = %q, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("TagValue = nil, want non-nil")
			}
			if string(got) != tc.want {
				t.Fatalf("TagValue = %q, want %q", got, tc.want)
			}
		})
	}

	if got := TagValue(nil, []byte("time")); got != nil {
		t.Fatalf("TagValue(nil) = %q, want nil", got)
	}
}

var allocSink int

func TestNoAllocations(t *testing.T) {
	line := []byte("@time=1;msgid=abc :nick!user@host PRIVMSG #chan :hello world")

	f := func() {
		tags, rest := Tags(line)
		prefix, rest := Prefix(rest)
		verb := Verb(rest)
		params := Params(rest)
		first := FirstParam(params)
		if IsVerb(rest, "PRIVMSG") {
			allocSink += len(tags) + len(prefix) + len(verb) + len(params) + len(first)
		}
	}

	if got := testing.AllocsPerRun(1000, f); got != 0 {
		t.Fatalf("allocations per run = %v, want 0", got)
	}
}

func TestSlicesAliasInput(t *testing.T) {
	line := []byte("@time=1 :nick!user PRIVMSG #chan :hello world")

	tags, rest := Tags(line)
	prefix, rest := Prefix(rest)
	verb := Verb(rest)
	params := Params(rest)
	first := FirstParam(params)
	tagValue := TagValue(tags, []byte("time"))

	checks := []struct {
		name string
		got  []byte
	}{
		{name: "tags", got: tags},
		{name: "tag value", got: tagValue},
		{name: "prefix", got: prefix},
		{name: "verb", got: verb},
		{name: "params", got: params},
		{name: "first param", got: first},
	}

	for _, c := range checks {
		if len(c.got) == 0 {
			t.Fatalf("%s is empty", c.name)
		}
		off := bytes.Index(line, c.got)
		if off < 0 {
			t.Fatalf("%s not found in input", c.name)
		}

		old := line[off]
		line[off] = old ^ 0x20
		if c.got[0] != old^0x20 {
			t.Fatalf("%s does not alias input", c.name)
		}
		line[off] = old
	}
}
