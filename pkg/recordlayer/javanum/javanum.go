// Package javanum ports the JDK's floating-point text parsers,
// Double.parseDouble and Float.parseFloat (FloatingDecimal
// .readJavaFormatString), which Java's record layer uses for CAST(string AS
// DOUBLE/FLOAT) and for index options. Go's strconv grammar differs (it
// accepts "inf", "nan" and digit underscores and refuses signs on NaN, the
// f/F/d/D suffixes and out-of-range magnitudes), and its NaN is not Java's.
package javanum

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

// NaN64Bits and NaN32Bits are Double.NaN and Float.NaN, the one NaN each
// parser returns: Go's math.NaN() is 0x7ff8000000000001, which packs to a
// different index key and record byte than the NaN Java writes.
const (
	NaN64Bits uint64 = 0x7ff8000000000000
	NaN32Bits uint32 = 0x7fc00000
)

// NaN64 is Double.NaN.
func NaN64() float64 { return math.Float64frombits(NaN64Bits) }

// NaN32 is Float.NaN.
func NaN32() float32 { return math.Float32frombits(NaN32Bits) }

// FormatError is Java's NumberFormatException from the parsers.
type FormatError struct {
	// Input is the refused text, trimmed as the parser trims it.
	Input string
	// Text, when set, is the exception's own message instead of
	// forInputString's: "empty String" for a value that trims to nothing and
	// "multiple points" for a second point in the significand.
	Text string
}

// Error is the exception's getMessage().
func (e *FormatError) Error() string {
	if e.Text != "" {
		return e.Text
	}
	return `For input string: "` + e.Input + `"`
}

// ParseDouble is Double.parseDouble: the value trimmed of characters at or
// below U+0020, then an optional sign and "NaN" or "Infinity", or a
// hexadecimal significand with its required binary exponent ("0x1.8p1"), or
// ASCII decimal digits with an optional point (at least one digit) and
// exponent, each optionally ended by one of f, F, d or D. A magnitude too
// large is an infinity and one too small a zero; every NaN is Double.NaN.
func ParseDouble(s string) (float64, error) {
	body, negative, special, err := scan(s)
	if err != nil {
		return 0, err
	}
	switch special {
	case "NaN":
		return NaN64(), nil
	case "Infinity":
		return math.Copysign(math.Inf(1), sign(negative)), nil
	}
	v, err := parse(s, body, 64)
	if err != nil {
		return 0, err
	}
	if negative {
		v = -v
	}
	return v, nil
}

// ParseFloat is Float.parseFloat: ParseDouble's grammar, the value rounded
// once, directly to binary32 (never through a double, which can round twice);
// every NaN is Float.NaN.
func ParseFloat(s string) (float32, error) {
	body, negative, special, err := scan(s)
	if err != nil {
		return 0, err
	}
	switch special {
	case "NaN":
		return NaN32(), nil
	case "Infinity":
		return float32(math.Copysign(math.Inf(1), sign(negative))), nil
	}
	v, err := parse(s, body, 32)
	if err != nil {
		return 0, err
	}
	f := float32(v)
	if negative {
		f = -f
	}
	return f, nil
}

func sign(negative bool) float64 {
	if negative {
		return -1
	}
	return 1
}

// scan trims s, takes its sign, and recognizes NaN and Infinity. It returns
// the unsigned body with any type suffix removed; FloatingDecimal throws at a
// second significand point before looking at anything after it.
func scan(s string) (body string, negative bool, special string, err error) {
	t := trim(s)
	if t == "" {
		return "", false, "", &FormatError{Text: "empty String"}
	}
	body = t
	if body[0] == '+' || body[0] == '-' {
		negative = body[0] == '-'
		body = body[1:]
	}
	if body == "NaN" || body == "Infinity" {
		return "", negative, body, nil
	}
	if !isHexPrefixed(body) {
		points := 0
		for i := 0; i < len(body) && (body[i] == '.' || body[i] >= '0' && body[i] <= '9'); i++ {
			if body[i] == '.' {
				if points++; points == 2 {
					return "", false, "", &FormatError{Input: t, Text: "multiple points"}
				}
			}
		}
	}
	if n := len(body); n > 0 && strings.ContainsRune("fFdD", rune(body[n-1])) {
		body = body[:n-1]
	}
	var ok bool
	if len(body) > 2 && isHexPrefixed(body) {
		ok = hexFloat(body[2:])
	} else {
		ok = decimalFloat(body)
	}
	if !ok {
		return "", false, "", &FormatError{Input: t}
	}
	return body, negative, "", nil
}

// parse rounds a body scan accepted, correctly, to bitSize. Out of range is
// not a failure: strconv reports overflow as ±Inf WITH ErrRange, and Java
// returns that infinity.
func parse(s, body string, bitSize int) (float64, error) {
	v, err := strconv.ParseFloat(body, bitSize)
	var numErr *strconv.NumError
	if err != nil && !(errors.As(err, &numErr) && errors.Is(numErr.Err, strconv.ErrRange)) {
		return 0, &FormatError{Input: trim(s)}
	}
	return v, nil
}

func trim(s string) string { return strings.TrimFunc(s, func(r rune) bool { return r <= ' ' }) }

func isHexPrefixed(s string) bool { return len(s) > 1 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') }

// decimalFloat reports whether s is Java's decimal floating-point form:
// digits, an optional point, digits (at least one digit in all), then an
// optional exponent of an optional sign and one or more digits; ASCII digits.
func decimalFloat(s string) bool {
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

// hexFloat reports whether s, after "0x", is Java's hexadecimal
// floating-point form: hex digits with an optional point (at least one digit),
// then the required p or P, an optional sign and one or more decimal digits.
func hexFloat(s string) bool {
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
