package logfile

import (
	"testing"
)

func TestWindowForTable(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		out     bool
		hasEcho bool
		first   string
		all     []string
		ok      bool
	}{
		{name: "channel text", line: ":nick!u@h PRIVMSG #c :hello", first: "#c", all: []string{"#c"}, ok: true},
		{name: "query text", line: ":nick!u@h PRIVMSG me :hi", first: "nick", all: []string{"nick"}, ok: true},
		{name: "own text", line: "PRIVMSG #c :mine", out: true, first: "#c", all: []string{"#c"}, ok: true},
		{name: "own query", line: "PRIVMSG alice :hi", out: true, first: "alice", all: []string{"alice"}, ok: true},
		{name: "notice channel", line: ":nick!u@h NOTICE #c :hi", first: "#c", all: []string{"#c"}, ok: true},
		{name: "join", line: ":nick!u@h JOIN #c", first: "#c", all: []string{"#c"}, ok: true},
		{name: "part", line: ":nick!u@h PART #c :bye", first: "#c", all: []string{"#c"}, ok: true},
		{name: "quit shared", line: ":a!u@h QUIT :gone", first: "#c", all: []string{"#c", "#d"}, ok: true},
		{name: "quit unshared", line: ":z!u@h QUIT :gone", ok: false},
		{name: "nick shared", line: ":a!u@h NICK b", first: "#c", all: []string{"#c", "#d"}, ok: true},
		{name: "topic", line: ":nick!u@h TOPIC #c :t", first: "#c", all: []string{"#c"}, ok: true},
		{name: "mode channel", line: ":op!u@h MODE #c +o other", first: "#c", all: []string{"#c"}, ok: true},
		{name: "mode self", line: ":srv MODE me +i", ok: false},
		{name: "numeric", line: ":srv 254 me 0 :channels", ok: false},
		{name: "ping", line: "PING :x", ok: false},
		{name: "cap reply", line: ":srv CAP * LS :server-time", ok: false},
		{name: "own echo suppressed", line: ":me!u@h PRIVMSG #c :hi", hasEcho: true, ok: false},
		{name: "other not suppressed", line: ":nick!u@h PRIVMSG #c :hi", hasEcho: true, first: "#c", all: []string{"#c"}, ok: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := NewTracker("me")
			tr.Observe([]byte(":a JOIN #c"))
			tr.Observe([]byte(":a JOIN #d"))
			first, all, ok := WindowFor(tr, []byte(tc.line), tc.out, tc.hasEcho)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (first %q all %v)", ok, tc.ok, first, all)
			}
			if !ok {
				if first != "" || all != nil {
					t.Fatalf("not ok but first=%q all=%v", first, all)
				}
				return
			}
			if first != tc.first {
				t.Fatalf("first = %q, want %q", first, tc.first)
			}
			if len(all) != len(tc.all) {
				t.Fatalf("all = %v, want %v", all, tc.all)
			}
			for i := range all {
				if all[i] != tc.all[i] {
					t.Fatalf("all = %v, want %v", all, tc.all)
				}
			}
		})
	}
}

func TestWindowForMatchesFormat(t *testing.T) {
	tr := NewTracker("me")
	tr.Observe([]byte(":a JOIN #c"))
	tr.Observe([]byte(":a JOIN #d"))
	lines := []string{
		":nick!u@h PRIVMSG #c :hi",
		":nick!u@h PRIVMSG me :hi",
		":a!u@h QUIT :gone",
		":a!u@h NICK b",
		":op!u@h MODE #c +o other",
		":srv 254 me 0 :channels",
		"PING :x",
	}
	for _, line := range lines {
		es := Format(tr, nil, []byte(line), false, false, "")
		_, all, ok := WindowFor(tr, []byte(line), false, false)
		if len(es) == 0 {
			if ok {
				t.Fatalf("%q: Format produced nothing but WindowFor ok", line)
			}
			continue
		}
		if !ok {
			continue
		}
		if es[0].Window != all[0] {
			t.Fatalf("%q: Format first window %q, WindowFor first %q", line, es[0].Window, all[0])
		}
	}
}
