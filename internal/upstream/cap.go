package upstream

import (
	"bytes"
	"strings"
)

var wantedCaps = []string{
	"server-time",
	"message-tags",
	"echo-message",
	"away-notify",
	"account-notify",
	"extended-join",
	"cap-notify",
	"batch",
	"chathistory",
	"sasl",
}

func splitParams(p []byte) [][]byte {
	var out [][]byte
	i := 0
	for i < len(p) {
		for i < len(p) && p[i] == ' ' {
			i++
		}
		if i >= len(p) {
			break
		}
		if p[i] == ':' {
			out = append(out, p[i+1:])
			break
		}
		start := i
		for i < len(p) && p[i] != ' ' {
			i++
		}
		out = append(out, p[start:i])
	}
	return out
}

func parseCapLS(params []byte) (caps []string, more, ok bool) {
	toks := splitParams(params)
	if len(toks) < 2 || !bytes.EqualFold(toks[1], []byte("LS")) {
		return nil, false, false
	}
	if len(toks) >= 2 && bytes.Equal(toks[len(toks)-2], []byte("*")) {
		more = true
	}
	start := 2
	if start < len(toks) && bytes.Equal(toks[start], []byte("*")) {
		start++
	}
	if start >= len(toks) {
		return nil, more, true
	}
	var b strings.Builder
	for i := start; i < len(toks); i++ {
		if i > start {
			b.WriteByte(' ')
		}
		b.Write(toks[i])
	}
	return strings.Fields(b.String()), more, true
}

func selectCaps(offered []string, allowSASL bool) (want []string, sasl bool) {
	has := make(map[string]bool, len(offered))
	for _, c := range offered {
		name := c
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		has[strings.ToLower(name)] = true
	}
	for _, w := range wantedCaps {
		if w == "sasl" && !allowSASL {
			continue
		}
		if has[w] {
			want = append(want, w)
		}
	}
	sasl = allowSASL && has["sasl"]
	return want, sasl
}
