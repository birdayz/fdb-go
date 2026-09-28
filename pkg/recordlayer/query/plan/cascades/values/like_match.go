package values

import (
	"unicode/utf16"
	"unicode/utf8"
)

// LikeErrorKind is one of the four semantic errors Java's LIKE raises
// (SemanticException.ErrorCode, SemanticException.java:47-57).
type LikeErrorKind int

const (
	// LikeOperandNotString is OPERAND_OF_LIKE_OPERATOR_IS_NOT_STRING (22F00).
	LikeOperandNotString LikeErrorKind = iota + 1
	// LikeEscapeNotSingleChar is ESCAPE_CHAR_OF_LIKE_OPERATOR_IS_NOT_SINGLE_CHAR (22019).
	LikeEscapeNotSingleChar
	// LikeEscapeConflict is ESCAPE_CHARACTER_CONFLICT (2200B).
	LikeEscapeConflict
	// LikeInvalidEscapeSequence is INVALID_ESCAPE_SEQUENCE (22025).
	LikeInvalidEscapeSequence
)

// LikeError is a LIKE semantic error, with Java's message.
type LikeError struct{ Kind LikeErrorKind }

func (e *LikeError) Error() string {
	switch e.Kind {
	case LikeOperandNotString:
		return "The like operator expects string operands but was invoked with an operand of another type."
	case LikeEscapeNotSingleChar:
		return "The like operator expects an escape character of length 1."
	case LikeEscapeConflict:
		return "The like operator rejects wildcards as the escape character."
	case LikeInvalidEscapeSequence:
		return "The like operator pattern requires all escape characters to be followed by a special character."
	}
	return "like operator error"
}

// validateLikeEscape is PatternForLikeValue.validateEscapeChar
// (PatternForLikeValue.java:133-139): exactly one UTF-16 unit, not a
// surrogate, and neither wildcard.
func validateLikeEscape(escape string) (uint16, error) {
	units := utf16.Encode([]rune(escape))
	if len(units) != 1 || utf16.IsSurrogate(rune(units[0])) {
		return 0, &LikeError{Kind: LikeEscapeNotSingleChar}
	}
	if units[0] == '_' || units[0] == '%' {
		return 0, &LikeError{Kind: LikeEscapeConflict}
	}
	return units[0], nil
}

// validateLikePattern is PatternForLikeValue.validatePattern
// (PatternForLikeValue.java:141-153): an escape must be followed by a
// wildcard or by itself.
func validateLikePattern(pattern []uint16, escape uint16) error {
	for i := 0; i < len(pattern); {
		if pattern[i] != escape {
			i++
			continue
		}
		if i+1 >= len(pattern) {
			return &LikeError{Kind: LikeInvalidEscapeSequence}
		}
		if literal := pattern[i+1]; literal != '_' && literal != '%' && literal != escape {
			return &LikeError{Kind: LikeInvalidEscapeSequence}
		}
		i += 2
	}
	return nil
}

// MatchLike is LikeOperatorValue.matchLike (LikeOperatorValue.java:145-241),
// ported over the same UTF-16 code units Java's String holds: `%` matches
// any run, `_` one unit or one surrogate pair, an escaped character that
// unit literally; nothing treats a line terminator specially. A nil escape
// is no escape. The escape and every escape sequence met are re-checked as
// Java re-checks them.
func MatchLike(text, pattern string, escape *string) (bool, error) {
	if isASCII(text) && isASCII(pattern) && (escape == nil || isASCII(*escape)) {
		// One byte is one UTF-16 unit; no surrogate can occur.
		var esc uint8
		if escape != nil {
			e, err := validateLikeEscape(*escape)
			if err != nil {
				return false, err
			}
			esc = uint8(e)
		}
		return matchLikeUnits([]byte(text), []byte(pattern), escape != nil, esc)
	}
	var esc uint16
	if escape != nil {
		e, err := validateLikeEscape(*escape)
		if err != nil {
			return false, err
		}
		esc = e
	}
	return matchLikeUnits(javaUTF16(text), utf16.Encode([]rune(pattern)), escape != nil, esc)
}

type likeUnit interface{ ~uint8 | ~uint16 }

func matchLikeUnits[T likeUnit](text, pattern []T, hasEscape bool, escape T) (bool, error) {
	t, p := 0, 0
	starP, starT := -1, -1
	tLen, pLen := len(text), len(pattern)
	for t < tLen {
		matched := false
		if p < pLen {
			pc := pattern[p]
			switch {
			case hasEscape && pc == escape:
				if p+1 >= pLen {
					return false, &LikeError{Kind: LikeInvalidEscapeSequence}
				}
				literal := pattern[p+1]
				if literal != '%' && literal != '_' && literal != escape {
					return false, &LikeError{Kind: LikeInvalidEscapeSequence}
				}
				if literal == text[t] {
					t++
					p += 2
					matched = true
				}
			case pc == '%':
				if p+1 == pLen {
					return true, nil
				}
				starP = p
				p++
				starT = t
				matched = true
			case pc == '_':
				if isHighSurrogateUnit(uint16(text[t])) && t+1 < tLen && isLowSurrogateUnit(uint16(text[t+1])) {
					t += 2
				} else {
					t++
				}
				p++
				matched = true
			case pc == text[t]:
				t++
				p++
				matched = true
			}
		}
		if !matched {
			if starP < 0 {
				return false, nil
			}
			p = starP + 1
			starT++
			t = starT
		}
	}
	for p < pLen && pattern[p] == '%' {
		p++
	}
	return p == pLen, nil
}

func isHighSurrogateUnit(u uint16) bool { return u >= 0xD800 && u <= 0xDBFF }
func isLowSurrogateUnit(u uint16) bool  { return u >= 0xDC00 && u <= 0xDFFF }

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// javaUTF16 is the UTF-16 Java holds for a stored string field. A valid
// string is its UTF-16 encoding. Protobuf-java reads a proto2 string field
// leniently (new String(bytes, UTF_8)), replacing each MAXIMAL invalid
// subsequence with one U+FFFD, the Unicode recommended practice
// (Unicode 15, section 3.9, "U+FFFD Substitution of Maximal Subparts");
// Go's own decoding replaces each invalid BYTE, so an invalid string is
// decoded here the Java way.
func javaUTF16(s string) []uint16 {
	if utf8.ValidString(s) {
		return utf16.Encode([]rune(s))
	}
	out := make([]uint16, 0, len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r != utf8.RuneError || size > 1 {
			out = utf16.AppendRune(out, r)
			i += size
			continue
		}
		out = append(out, 0xFFFD)
		i += maximalSubpartLength(s[i:])
	}
	return out
}

// maximalSubpartLength is the length of the maximal subpart of an
// ill-formed sequence starting at s[0] (Unicode Table 3-7's well-formed
// byte ranges): the lead byte plus the continuation bytes that could still
// begin a well-formed sequence, at least one byte.
func maximalSubpartLength(s string) int {
	lead := s[0]
	var need int
	lo, hi := byte(0x80), byte(0xBF) // the second byte's range
	switch {
	case lead >= 0xC2 && lead <= 0xDF:
		need = 1
	case lead == 0xE0:
		need, lo = 2, 0xA0
	case lead >= 0xE1 && lead <= 0xEC, lead == 0xEE, lead == 0xEF:
		need = 2
	case lead == 0xED:
		need, hi = 2, 0x9F
	case lead == 0xF0:
		need, lo = 3, 0x90
	case lead >= 0xF1 && lead <= 0xF3:
		need = 3
	case lead == 0xF4:
		need, hi = 3, 0x8F
	default:
		return 1
	}
	n := 1
	for k := 0; k < need && n < len(s); k++ {
		b := s[n]
		if k == 0 && (b < lo || b > hi) || k > 0 && (b < 0x80 || b > 0xBF) {
			break
		}
		n++
	}
	return n
}
