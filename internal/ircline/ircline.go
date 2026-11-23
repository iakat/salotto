package ircline

import "bytes"

// Tags splits a leading @tag1=v1;tag2=v2 blob off the line. If there is no tag
// blob, tags is nil and rest is the whole line. The returned slices alias line.
func Tags(line []byte) (tags, rest []byte) {
	if len(line) == 0 || line[0] != '@' {
		return nil, line
	}

	i := 1
	for i < len(line) && !isSpace(line[i]) {
		i++
	}
	tags = line[1:i]
	if i >= len(line) {
		return tags, line[len(line):]
	}
	for i < len(line) && isSpace(line[i]) {
		i++
	}
	return tags, line[i:]
}

// TagValue returns the value bytes for key inside a tag blob produced by Tags,
// or nil. key is compared case-insensitively. Handles the value-less tag form
// ("key" with no '='), returning an empty non-nil slice in that case.
func TagValue(tags, key []byte) []byte {
	if len(key) == 0 {
		return nil
	}
	i := 0
	for i <= len(tags) {
		j := i
		for j < len(tags) && tags[j] != ';' {
			j++
		}
		seg := tags[i:j]

		eq := -1
		for k := 0; k < len(seg); k++ {
			if seg[k] == '=' {
				eq = k
				break
			}
		}
		if eq < 0 {
			if bytes.EqualFold(seg, key) {
				return tags[j:j]
			}
		} else if bytes.EqualFold(seg[:eq], key) {
			return tags[i+eq+1 : j]
		}

		if j >= len(tags) {
			break
		}
		i = j + 1
	}
	return nil
}

// Prefix splits a leading :prefix off the line (after Tags has been applied).
func Prefix(line []byte) (prefix, rest []byte) {
	if len(line) == 0 || line[0] != ':' {
		return nil, line
	}

	i := 1
	for i < len(line) && !isSpace(line[i]) {
		i++
	}
	prefix = line[1:i]
	if i >= len(line) {
		return prefix, line[len(line):]
	}
	for i < len(line) && isSpace(line[i]) {
		i++
	}
	return prefix, line[i:]
}

// Verb returns the command token: the first whitespace-delimited token of the
// line passed to it (after Tags and Prefix). Upper case as sent; callers compare
// with bytes.EqualFold.
func Verb(line []byte) []byte {
	i := 0
	for i < len(line) && isSpace(line[i]) {
		i++
	}
	start := i
	for i < len(line) && !isSpace(line[i]) {
		i++
	}
	return line[start:i]
}

// Params returns everything after the command verb, with the leading space
// removed. Aliases line.
func Params(line []byte) []byte {
	i := 0
	for i < len(line) && isSpace(line[i]) {
		i++
	}
	for i < len(line) && !isSpace(line[i]) {
		i++
	}
	for i < len(line) && isSpace(line[i]) {
		i++
	}
	return trimCR(line[i:])
}

// FirstParam returns the first whitespace-delimited parameter of params, or
// nil.
func FirstParam(params []byte) []byte {
	i := 0
	for i < len(params) && isSpace(params[i]) {
		i++
	}
	start := i
	for i < len(params) && !isSpace(params[i]) {
		i++
	}
	if start == i {
		return nil
	}
	return params[start:i]
}

// IsVerb reports whether the command of line equals v, case-insensitively,
// without allocating or converting to string.
func IsVerb(line []byte, v string) bool {
	i := 0
	for i < len(line) && isSpace(line[i]) {
		i++
	}
	start := i
	for i < len(line) && !isSpace(line[i]) {
		j := i - start
		if j >= len(v) {
			return false
		}
		if lowerASCII(line[i]) != lowerASCII(v[j]) {
			return false
		}
		i++
	}
	return i-start == len(v)
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

func trimCR(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\r' {
		return b[:n-1]
	}
	return b
}

func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// Nick returns the nickname from a prefix ("nick!user@host" -> "nick"). An empty
// prefix, or a server prefix with no "!", returns "".
func Nick(prefix []byte) string {
	if len(prefix) == 0 {
		return ""
	}
	i := 0
	for i < len(prefix) && prefix[i] != '!' && prefix[i] != '@' {
		i++
	}
	if i == len(prefix) || prefix[i] == '@' {
		// A server name has no "!", so a prefix without one is not a nick.
		return ""
	}
	return string(prefix[:i])
}
