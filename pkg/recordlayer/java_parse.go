package recordlayer

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// IllegalArgumentError is Java's IllegalArgumentException, raised by the
// option parsers Java runs through the JDK: an out-of-range RankedSet level
// count (RankedSet.ConfigBuilder.setNLevels) or an unknown enum name
// (Enum.valueOf, as RTree.Storage.valueOf).
type IllegalArgumentError struct {
	Message string
}

func (e *IllegalArgumentError) Error() string { return e.Message }

// NumberFormatError is Java's NumberFormatException, raised by
// Integer.parseInt on an index option that is not a decimal int. Java's class
// extends IllegalArgumentException, so errors.As matches it as an
// *IllegalArgumentError too.
type NumberFormatError struct {
	// Input is the option value Integer.parseInt refused.
	Input string
}

// Error is NumberFormatException.forInputString's text for radix 10.
func (e *NumberFormatError) Error() string { return `For input string: "` + e.Input + `"` }

// As lets errors.As match a NumberFormatError as the IllegalArgumentError
// its Java class extends.
func (e *NumberFormatError) As(target any) bool {
	if t, ok := target.(**IllegalArgumentError); ok {
		*t = &IllegalArgumentError{Message: e.Error()}
		return true
	}
	return false
}

// javaParseInt is Java's Integer.parseInt(s) (radix 10): an optional leading
// '+' or '-', then one or more decimal digits, within int32. A digit is what
// Character.digit(char, 10) accepts, any Unicode decimal digit (general
// category Nd) in the Basic Multilingual Plane: Java walks UTF-16 chars, so a
// supplementary digit is two surrogates, neither of them a digit.
func javaParseInt(s string) (int32, error) {
	if s == "" {
		return 0, &NumberFormatError{Input: s}
	}
	negative := false
	i := 0
	if s[0] == '-' || s[0] == '+' {
		negative = s[0] == '-'
		i = 1
		if len(s) == 1 {
			return 0, &NumberFormatError{Input: s}
		}
	}
	// Accumulated negatively, as Java does, so MinInt32 parses.
	var limit int64 = -(1<<31 - 1)
	if negative {
		limit = -(1 << 31)
	}
	var result int64
	for _, r := range s[i:] {
		d := javaDecimalDigit(r)
		if d < 0 {
			return 0, &NumberFormatError{Input: s}
		}
		result = result*10 - int64(d)
		if result < limit {
			return 0, &NumberFormatError{Input: s}
		}
	}
	if negative {
		return int32(result), nil
	}
	return int32(-result), nil
}

// javaDecimalDigit is Character.digit(ch, 10) for one rune, -1 for a
// non-digit. Unicode encodes every decimal digit (Nd) in contiguous runs of
// ten, zero first, so a digit's value is its offset in its run; Go's Nd table
// may merge adjacent runs into one range, which keeps the offset modulo ten.
func javaDecimalDigit(r rune) int {
	if r == utf8.RuneError || r > 0xFFFF {
		return -1
	}
	if r >= '0' && r <= '9' {
		return int(r - '0')
	}
	if !unicode.Is(unicode.Nd, r) {
		return -1
	}
	for _, rg := range unicode.Nd.R16 {
		if uint16(r) >= rg.Lo && uint16(r) <= rg.Hi {
			return int(uint16(r)-rg.Lo) % 10
		}
	}
	return -1
}

// javaParseBoolean is Java's Boolean.parseBoolean (and Boolean.valueOf,
// which Index.getBooleanOption uses): "true" in any case is true, every other
// value false.
func javaParseBoolean(s string) bool {
	return strings.EqualFold(s, "true")
}
