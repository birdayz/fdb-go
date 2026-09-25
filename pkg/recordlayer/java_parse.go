package recordlayer

import (
	"errors"
	"math"
	"strconv"
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
	// Input is the value Integer.parseInt or Double.parseDouble refused
	// (Double.parseDouble's trimmed).
	Input string
	// Empty is Double.parseDouble's refusal of a value that trims to nothing.
	Empty bool
}

// Error is NumberFormatException.forInputString's text for radix 10, and
// Double.parseDouble's for an empty value.
func (e *NumberFormatError) Error() string {
	if e.Empty {
		return "empty String"
	}
	return `For input string: "` + e.Input + `"`
}

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

// javaParseDouble is Java's Double.parseDouble (FloatingDecimal
// .readJavaFormatString): the value trimmed of characters at or below U+0020,
// then an optional sign and "NaN" or "Infinity", or a hexadecimal significand
// with its required binary exponent ("0x1.8p1"), or ASCII decimal digits with
// an optional point (at least one digit) and exponent, each optionally ended by
// one of f, F, d or D. A decimal too large is an infinity and one too small a
// zero, as in Java; Go's strconv grammar (underscores, "inf", "nan", no
// suffix) is not accepted.
func javaParseDouble(s string) (float64, error) {
	t := strings.TrimFunc(s, func(r rune) bool { return r <= ' ' })
	if t == "" {
		return 0, &NumberFormatError{Empty: true}
	}
	refuse := &NumberFormatError{Input: t}
	body := t
	negative := false
	if body[0] == '+' || body[0] == '-' {
		negative = body[0] == '-'
		body = body[1:]
	}
	switch body {
	case "NaN":
		return math.NaN(), nil
	case "Infinity":
		if negative {
			return math.Inf(-1), nil
		}
		return math.Inf(1), nil
	}
	if n := len(body); n > 0 && strings.ContainsRune("fFdD", rune(body[n-1])) {
		body = body[:n-1]
	}
	var ok bool
	if len(body) > 2 && body[0] == '0' && (body[1] == 'x' || body[1] == 'X') {
		ok = javaHexFloat(body[2:])
	} else {
		ok = javaDecimalFloat(body)
	}
	if !ok {
		return 0, refuse
	}
	v, err := strconv.ParseFloat(body, 64)
	var numErr *strconv.NumError
	if err != nil && !(errors.As(err, &numErr) && errors.Is(numErr.Err, strconv.ErrRange)) {
		return 0, refuse
	}
	if negative {
		v = -v
	}
	return v, nil
}

// javaDecimalFloat reports whether s is Java's decimal floating-point form:
// digits, an optional point, digits (at least one digit in all), then an
// optional exponent of an optional sign and one or more digits; ASCII digits.
func javaDecimalFloat(s string) bool {
	i, digits := 0, 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i, digits = i+1, digits+1
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i, digits = i+1, digits+1
		}
	}
	if digits == 0 {
		return false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		exp := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i, exp = i+1, exp+1
		}
		if exp == 0 {
			return false
		}
	}
	return i == len(s)
}

// javaHexFloat reports whether s, after "0x", is Java's hexadecimal
// floating-point form: hex digits with an optional point (at least one digit),
// then the required p or P, an optional sign and one or more decimal digits.
func javaHexFloat(s string) bool {
	isHex := func(c byte) bool {
		return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
	}
	i, digits := 0, 0
	for i < len(s) && isHex(s[i]) {
		i, digits = i+1, digits+1
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && isHex(s[i]) {
			i, digits = i+1, digits+1
		}
	}
	if digits == 0 || i >= len(s) || (s[i] != 'p' && s[i] != 'P') {
		return false
	}
	i++
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	exp := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i, exp = i+1, exp+1
	}
	return exp > 0 && i == len(s)
}

// javaParseBoolean is Java's Boolean.parseBoolean (and Boolean.valueOf,
// which Index.getBooleanOption uses): "true" in any case is true, every other
// value false.
func javaParseBoolean(s string) bool {
	return strings.EqualFold(s, "true")
}
