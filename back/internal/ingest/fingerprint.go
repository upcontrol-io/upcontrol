package ingest

import (
	"hash/fnv"
	"strings"
)

// Fingerprint hashes the message's shape, not its text: numbers, hex runs and
// quoted strings collapse to placeholders, so "user 42 not found" and "user 7
// not found" land in one error group. FNV-64a of the masked template; an empty
// message is 0 (zero is silence).
func Fingerprint(msg string) uint64 {
	if msg == "" {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(mask(msg)))
	return h.Sum64()
}

// mask is the template: the message with its variable parts replaced. A hex
// run collapses to '#' when it is 8+ bytes or carries a digit (an id, not a
// word); digit-free runs like "added" or "cafe" are dictionary and survive.
func mask(msg string) string {
	var b strings.Builder
	b.Grow(len(msg))
	for i := 0; i < len(msg); {
		c := msg[i]
		switch {
		case c == '"' || c == '\'':
			// A quoted string masks only when the same quote closes it on
			// this line; a lone quote is punctuation, not a string.
			j := i + 1
			for j < len(msg) && msg[j] != '\n' && msg[j] != c {
				j++
			}
			if j < len(msg) && msg[j] == c {
				b.WriteByte(c)
				b.WriteByte('?')
				b.WriteByte(c)
				i = j + 1
			} else {
				b.WriteByte(c)
				i++
			}
		case isHex(c):
			j := i
			digit := false
			for j < len(msg) && isHex(msg[j]) {
				if msg[j] >= '0' && msg[j] <= '9' {
					digit = true
				}
				j++
			}
			if j-i >= 8 || digit {
				b.WriteByte('#')
			} else {
				b.WriteString(msg[i:j])
			}
			i = j
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
