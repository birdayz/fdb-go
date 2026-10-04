package values

import (
	"strconv"
	"unicode"
	"unicode/utf16"
)

// ParseJavaUUID parses s exactly as java.util.UUID.fromString does: five
// dash-separated hex components, each read by Long.parseLong(radix 16) (so a
// sign, Unicode digits and short or over-wide components are accepted) and
// masked to its field width; no braces, URN prefix or undashed form.
func ParseJavaUUID(s string) ([16]byte, bool) {
	var out [16]byte
	units := utf16.Encode([]rune(s))
	if len(units) > 36 {
		return out, false
	}
	var dashes []int
	for i, u := range units {
		if u == '-' {
			dashes = append(dashes, i)
		}
	}
	// Java takes the first four dashes and rejects a fifth; a leading '-' is a
	// sign only when it is not one of those separators.
	if len(dashes) != 4 {
		return out, false
	}
	bounds := [][2]int{{0, dashes[0]}, {dashes[0] + 1, dashes[1]}, {dashes[1] + 1, dashes[2]}, {dashes[2] + 1, dashes[3]}, {dashes[3] + 1, len(units)}}
	var parts [5]uint64
	for i, b := range bounds {
		v, ok := javaParseHexLong(units[b[0]:b[1]])
		if !ok {
			return out, false
		}
		parts[i] = uint64(v)
	}
	msb := (parts[0]&0xffffffff)<<32 | (parts[1]&0xffff)<<16 | parts[2]&0xffff
	lsb := (parts[3]&0xffff)<<48 | parts[4]&0xffffffffffff
	for i := 0; i < 8; i++ {
		out[i] = byte(msb >> (56 - 8*i))
		out[8+i] = byte(lsb >> (56 - 8*i))
	}
	return out, true
}

// javaParseHexLong is Long.parseLong(s, 16): optional sign, at least one
// digit per Character.digit, overflow rejected.
func javaParseHexLong(units []uint16) (int64, bool) {
	if len(units) == 0 {
		return 0, false
	}
	neg := false
	i := 0
	if units[0] == '+' || units[0] == '-' {
		neg = units[0] == '-'
		i = 1
		if len(units) == 1 {
			return 0, false
		}
	}
	var digits []byte
	for _, u := range units[i:] {
		d := javaHexDigit(rune(u))
		if d < 0 {
			return 0, false
		}
		digits = append(digits, "0123456789abcdef"[d])
	}
	text := string(digits)
	if neg {
		text = "-" + text
	}
	v, err := strconv.ParseInt(text, 16, 64)
	return v, err == nil
}

// javaHexDigit is Character.digit(c, 16) for a BMP code unit.
func javaHexDigit(c rune) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	case c >= 0xFF21 && c <= 0xFF26: // fullwidth A-F
		return int(c-0xFF21) + 10
	case c >= 0xFF41 && c <= 0xFF46: // fullwidth a-f
		return int(c-0xFF41) + 10
	case unicode.IsDigit(c):
		// Nd digits come in contiguous blocks of ten starting at zero.
		k := 0
		for unicode.IsDigit(c - rune(k) - 1) {
			k++
		}
		return k % 10
	}
	return -1
}
