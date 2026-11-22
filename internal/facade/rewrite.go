package facade

import "github.com/iakat/salotto/internal/ircline"

// upstreamNickSource is an optional extension implemented by a network that
// already knows the nick it registered upstream. The POC's production adapter
// does not implement it; in that case the facade learns the nick from the
// first numeric in the ring (or from a live numeric) instead.
type upstreamNickSource interface {
	Nick() string
}

func isNumericVerb(v []byte) bool {
	if len(v) != 3 {
		return false
	}
	for _, c := range v {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func prefixNick(prefix []byte) []byte {
	for i, c := range prefix {
		if c == '!' || c == '@' {
			return prefix[:i]
		}
	}
	return prefix
}

func prefixSuffix(prefix []byte) []byte {
	for i, c := range prefix {
		if c == '!' || c == '@' {
			return prefix[i:]
		}
	}
	return nil
}

// detectUpstreamNick derives the upstream nick from a numeric addressed to it.
// The first parameter of a numeric is the target nick unless it is a channel
// or the pre-registration "*" placeholder.
func detectUpstreamNick(line []byte) string {
	_, rest := ircline.Tags(line)
	_, rest = ircline.Prefix(rest)
	if !isNumericVerb(ircline.Verb(rest)) {
		return ""
	}
	p := ircline.FirstParam(ircline.Params(rest))
	if len(p) == 0 {
		return ""
	}
	if p[0] == '#' || p[0] == '&' || (len(p) == 1 && p[0] == '*') {
		return ""
	}
	return string(p)
}

// upstreamNickChange reports the new nick when line is a NICK change sent by
// the upstream identity itself.
func upstreamNickChange(line []byte, un string) (string, bool) {
	if un == "" {
		return "", false
	}
	_, rest := ircline.Tags(line)
	prefix, rest := ircline.Prefix(rest)
	if !equalFold(prefixNick(prefix), un) || !ircline.IsVerb(rest, "NICK") {
		return "", false
	}
	nick := ircline.FirstParam(ircline.Params(rest))
	if len(nick) == 0 {
		return "", false
	}
	return string(nick), true
}

// rewriteLine rewrites one framed (CRLF-terminated) line for a client whose
// nick is cn. un is the upstream nick at the time the line was received. The
// returned changed flag is false when line was passed through unchanged, and
// drop is true for an upstream NICK change (which the POC intentionally drops).
//
// The caller supplies a scratch buffer; appending to it does not allocate when
// it has enough capacity. The untouched fast path returns line itself.
func rewriteLine(dst, line []byte, cn, un string, extendedJoin bool) (out []byte, changed, drop bool) {
	tags, rest := ircline.Tags(line)
	prefix, after := ircline.Prefix(rest)
	verb := ircline.Verb(after)
	params := ircline.Params(after)

	prefixIsUpstream := un != "" && equalFold(prefixNick(prefix), un)
	numeric := isNumericVerb(verb)
	first := ircline.FirstParam(params)
	numericTarget := numeric && un != "" && equalFold(first, un)
	selfTarget := !numeric && prefixIsUpstream && un != "" && equalFold(first, un)
	isJoin := ircline.IsVerb(after, "JOIN")
	isNames := ircline.IsVerb(after, "353")
	stripJoin := isJoin && !extendedJoin && hasSecondParam(params)
	namesChange := isNames && un != "" && namesHasNick(params, un)

	if prefixIsUpstream && ircline.IsVerb(after, "NICK") {
		// The POC deliberately drops upstream NICK changes rather than
		// pretending the shared upstream identity changed for every client.
		// The facade still advances its cached upstream nick so later lines
		// are rewritten against the new identity.
		return dst, true, true
	}

	if !prefixIsUpstream && !numericTarget && !selfTarget && !stripJoin && !namesChange {
		return line, false, false
	}

	out = dst[:0]
	if tags != nil {
		out = append(out, '@')
		out = append(out, tags...)
		out = append(out, ' ')
	}
	if prefix != nil {
		out = append(out, ':')
		if prefixIsUpstream {
			out = append(out, cn...)
			out = append(out, prefixSuffix(prefix)...)
		} else {
			out = append(out, prefix...)
		}
		out = append(out, ' ')
	}
	out = append(out, verb...)

	switch {
	case isNames:
		out = append353Params(out, params, cn, un)
	case stripJoin:
		tok, _ := nextToken(params)
		if len(tok) > 0 {
			out = append(out, ' ')
			out = append(out, tok...)
		}
	default:
		out = appendParams(out, params, cn, numericTarget || selfTarget)
	}

	out = append(out, '\r', '\n')
	return out, true, false
}

func append353Params(dst, params []byte, cn, un string) []byte {
	rest := params
	for i := 0; i < 3; i++ {
		tok, next := nextToken(rest)
		if len(tok) == 0 {
			return dst
		}
		dst = append(dst, ' ')
		if i == 0 && un != "" && equalFold(tok, un) {
			dst = append(dst, cn...)
		} else {
			dst = append(dst, tok...)
		}
		rest = next
	}
	if len(rest) > 0 {
		dst = append(dst, ' ')
		dst = appendNames(dst, rest, cn, un)
	}
	return dst
}

func appendParams(dst, params []byte, cn string, rewriteFirst bool) []byte {
	rest := params
	first := true
	for {
		tok, next := nextToken(rest)
		if len(tok) == 0 {
			break
		}
		dst = append(dst, ' ')
		if first && rewriteFirst {
			dst = append(dst, cn...)
		} else {
			dst = append(dst, tok...)
		}
		first = false
		rest = next
	}
	return dst
}

func appendNames(dst, names []byte, cn, un string) []byte {
	if len(names) > 0 && names[0] == ':' {
		dst = append(dst, ':')
		names = names[1:]
	}

	first := true
	for len(names) > 0 {
		for len(names) > 0 && names[0] == ' ' {
			names = names[1:]
		}
		if len(names) == 0 {
			break
		}
		end := 0
		for end < len(names) && names[end] != ' ' {
			end++
		}
		tok := names[:end]
		names = names[end:]

		if !first {
			dst = append(dst, ' ')
		}
		first = false
		dst = appendNamesToken(dst, tok, cn, un)
	}
	return dst
}

func appendNamesToken(dst, tok []byte, cn, un string) []byte {
	i := 0
	for i < len(tok) && isNamePrefix(tok[i]) {
		i++
	}
	nick := tok[i:]
	if un != "" && equalFold(nick, un) {
		dst = append(dst, tok[:i]...)
		dst = append(dst, cn...)
		return dst
	}
	return append(dst, tok...)
}

func namesHasNick(params []byte, un string) bool {
	rest := params
	for i := 0; i < 3; i++ {
		_, next := nextToken(rest)
		rest = next
	}
	if len(rest) == 0 {
		return false
	}
	if rest[0] == ':' {
		rest = rest[1:]
	}
	for len(rest) > 0 {
		for len(rest) > 0 && rest[0] == ' ' {
			rest = rest[1:]
		}
		if len(rest) == 0 {
			return false
		}
		end := 0
		for end < len(rest) && rest[end] != ' ' {
			end++
		}
		tok := rest[:end]
		rest = rest[end:]

		i := 0
		for i < len(tok) && isNamePrefix(tok[i]) {
			i++
		}
		if equalFold(tok[i:], un) {
			return true
		}
	}
	return false
}

func hasSecondParam(params []byte) bool {
	_, rest := nextToken(params)
	if len(rest) == 0 {
		return false
	}
	tok, _ := nextToken(rest)
	return len(tok) > 0
}

func isNamePrefix(c byte) bool {
	switch c {
	case '~', '&', '@', '%', '+':
		return true
	default:
		return false
	}
}
