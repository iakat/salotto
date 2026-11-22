package logfile

import (
	"strings"
	"testing"
	"time"
)

func TestFormatTable(t *testing.T) {
	tr := NewTracker("me")
	// seed membership for quit/nick fanout
	tr.Observe([]byte(":a JOIN #c"))
	tr.Observe([]byte(":a JOIN #d"))
	now := time.Date(2026, 11, 22, 15, 4, 5, 0, time.Local)

	cases := []struct {
		name string
		line string
		out  bool
		want []string // window:body substrings
	}{
		{"chan text", ":nick!u@h PRIVMSG #c :hello", false, []string{"#c:<nick> hello"}},
		{"pm", ":nick!u@h PRIVMSG me :hi", false, []string{"nick:<nick> hi"}},
		{"own text chan", "", true, nil},
		{"chan action", ":nick!u@h PRIVMSG #c :\x01ACTION waves\x01", false, []string{"#c:* nick waves"}},
		{"pm action", ":nick!u@h PRIVMSG me :\x01ACTION pokes\x01", false, []string{"nick:* nick pokes"}},
		{"chan notice", ":nick!u@h NOTICE #c :hi", false, []string{"#c:-nick- hi"}},
		{"pm notice", ":nick!u@h NOTICE me :hi", false, []string{"nick:-nick- hi"}},
		{"join no acct", ":nick!u@h JOIN #c", false, []string{"#c:*** Joins: nick (u@h)"}},
		{"join acct", ":nick!u@h JOIN #c acct :real", false, []string{"#c:*** Joins: nick (u@h) acct"}},
		{"part", ":nick!u@h PART #c :bye", false, []string{"#c:*** Parts: nick (u@h) (bye)"}},
		{"quit", ":a!u@h QUIT :gone", false, []string{"#c:*** Quits: a (u@h) (gone)", "#d:*** Quits: a (u@h) (gone)"}},
		{"nick", ":a!u@h NICK b", false, []string{"#c:*** a is now known as b", "#d:*** a is now known as b"}},
		{"topic", ":nick!u@h TOPIC #c :t", false, []string{"#c:*** nick changes topic to 't'"}},
		{"mode", ":op!u@h MODE #c +o other", false, []string{"#c:*** op sets mode: +o other"}},
		{"mode empty", ":op!u@h MODE #c +o", false, []string{"#c:*** op sets mode: +o "}},
		{"kick", ":op!u@h KICK #c victim :reason", false, []string{"#c:*** victim was kicked by op (reason)"}},
		{"connect", ":srv 001 me :welcome", false, []string{"status:Connected to IRC (irc.example 6697)"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "own text chan" {
				tr2 := NewTracker("me")
				es := Format(tr2, now, []byte(":me PRIVMSG #c :mine"), true, false, "srv")
				if len(es) != 1 || es[0].Body != "<me> mine" {
					t.Fatalf("got %+v", es)
				}
				return
			}
			if tc.name == "quit" || tc.name == "nick" {
				// reset tracker membership
				tr3 := NewTracker("me")
				tr3.Observe([]byte(":a JOIN #c"))
				tr3.Observe([]byte(":a JOIN #d"))
				es := Format(tr3, now, []byte(tc.line), false, false, "irc.example 6697")
				got := []string{}
				for _, e := range es {
					got = append(got, e.Window+":"+e.Body)
				}
				if !equalStrings(got, tc.want) {
					t.Fatalf("got %v want %v", got, tc.want)
				}
				return
			}
			if tc.name == "connect" {
				tr4 := NewTracker("me")
				es := Format(tr4, now, []byte(tc.line), false, false, "irc.example 6697")
				got := []string{}
				for _, e := range es {
					got = append(got, e.Window+":"+e.Body)
				}
				if !equalStrings(got, tc.want) {
					t.Fatalf("got %v want %v", got, tc.want)
				}
				return
			}
			es := Format(tr, now, []byte(tc.line), tc.out, false, "irc.example 6697")
			got := []string{}
			for _, e := range es {
				got = append(got, e.Window+":"+e.Body)
			}
			if !equalStrings(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
	// mode empty trailing space check
	es := Format(tr, now, []byte(":op!u@h MODE #c +o"), false, false, "x")
	if len(es) != 1 || !strings.HasSuffix(es[0].Body, "+o ") {
		t.Fatalf("mode empty: %q", es)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestQuitNoChannelStatus(t *testing.T) {
	tr := NewTracker("me")
	es := Format(tr, time.Now(), []byte(":x!u@h QUIT :bye"), false, false, "srv")
	if len(es) != 1 || es[0].Window != "status" {
		t.Fatalf("got %+v", es)
	}
}

func TestNickNoChannelStatus(t *testing.T) {
	tr := NewTracker("me")
	es := Format(tr, time.Now(), []byte(":x!u@h NICK y"), false, false, "srv")
	if len(es) != 1 || es[0].Window != "status" {
		t.Fatalf("got %+v", es)
	}
}

func TestEchoSkip(t *testing.T) {
	tr := NewTracker("me")
	tr.SetOwnNick("me")
	es := Format(tr, time.Now(), []byte(":me!u@h PRIVMSG #c :hi"), false, true, "srv")
	if es != nil && len(es) != 0 {
		t.Fatalf("expected skip, got %+v", es)
	}
}

func TestOwnActionOutbound(t *testing.T) {
	tr := NewTracker("me")
	es := Format(tr, time.Now(), []byte("PRIVMSG #c :\x01ACTION waves\x01"), true, false, "srv")
	if len(es) != 1 || es[0].Body != "* me waves" {
		t.Fatalf("got %+v", es)
	}
}
