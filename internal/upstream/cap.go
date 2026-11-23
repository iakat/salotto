package upstream

import (
	"bytes"
	"strings"
)

// wantedCaps is what this bouncer asks the *server* for. echo-message is
// deliberately absent: ZNC does not request it upstream either
// (IRCSock.cpp's mSupportedCaps has no echo-message entry), because the bouncer
// already echoes an outbound line itself from its own send path. Asking the
// server as well makes every message arrive twice — once from our own echo and
// once from the server's — and the second copy reaches every attached client,
// including the author who never negotiated the capability.
var wantedCaps = []string{
	"server-time",
	"message-tags",
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
