package embedded

import (
	"bytes"
	"database/sql/driver"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/functions"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
)

// Pure utility helpers with no dependency on EmbeddedConnection
// state — used across every execution path.
//
//   applyArithmeticOp   map-path arithmetic wrapper over
//                       functions.ApplyMathOp (int64 preservation,
//                       div/0 errors, `%` support — single path
//                       shared by proto + map evaluators).
//   substituteParams    positional `?` placeholder substitution
//                       (nil → NULL, bool → TRUE/FALSE, int64,
//                       float64, string with quote escaping,
//                       []byte as quoted hex). Single-quoted
//                       strings + -- and /* */ comments are
//                       preserved verbatim.
//   evalConstant        parse-tree constant → Go value (int64 /
//                       float64 / string / bool / nil / []byte via
//                       hex or base64). Numeric overflow → 22003.
//   stripBytesWrapper / decodeBase64  bytes-literal support.
//   valuesEqual         type-strict value equality with numeric
//                       promotion (int64 / float64 mix OK, exact
//                       int64 path avoids float-precision loss for
//                       values > 2^53). Cross-type compares return
//                       false rather than panicking.
//   rowKey              DISTINCT / UNION DISTINCT deduplication
//                       hash. Length-prefixed fields prevent
//                       separator-character collisions.
//
// Destined for pkg/relational/core/{functions,eval}/ per RFC 021
// Phase 1c.

// applyArithmeticOp is the map-path arithmetic entry. It delegates to the
// canonical `applyMathOp` so proto and map paths stay behaviourally identical
// (div/0 errors per SQL standard, int64 preservation, `%` support).
func applyArithmeticOp(left, right driver.Value, op string) (driver.Value, error) {
	return functions.ApplyMathOp(left, right, op)
}

// substituteParams replaces positional '?' placeholders in a query with
// SQL literal representations of the supplied driver values. Named params
// (@name) are not supported — only positional '?' is handled.
func substituteParams(query string, args []driver.NamedValue) (string, error) {
	if len(args) == 0 {
		return query, nil
	}
	var b strings.Builder
	argIdx := 0
	for i := 0; i < len(query); i++ {
		ch := query[i]
		// Skip single-quoted string literals so a '?' inside a string value
		// is not treated as a placeholder.
		if ch == '\'' {
			b.WriteByte(ch)
			i++
			for i < len(query) {
				c := query[i]
				b.WriteByte(c)
				if c == '\'' {
					if i+1 < len(query) && query[i+1] == '\'' {
						// escaped quote inside string
						i++
						b.WriteByte(query[i])
					} else {
						break
					}
				}
				i++
			}
			continue
		}
		// Skip line comments `-- ...\n`. A '?' in a comment is literal.
		if ch == '-' && i+1 < len(query) && query[i+1] == '-' {
			for i < len(query) && query[i] != '\n' {
				b.WriteByte(query[i])
				i++
			}
			if i < len(query) {
				b.WriteByte(query[i]) // write the trailing newline
			}
			continue
		}
		// Skip block comments `/* ... */`. A '?' in a comment is literal.
		if ch == '/' && i+1 < len(query) && query[i+1] == '*' {
			b.WriteByte(query[i])
			i++
			b.WriteByte(query[i])
			i++
			for i+1 < len(query) {
				if query[i] == '*' && query[i+1] == '/' {
					b.WriteByte(query[i])
					i++
					b.WriteByte(query[i])
					break
				}
				b.WriteByte(query[i])
				i++
			}
			continue
		}
		if ch != '?' {
			b.WriteByte(ch)
			continue
		}
		if argIdx >= len(args) {
			return "", api.NewErrorf(api.ErrCodeInvalidParameter,
				"more '?' placeholders than bound parameters (placeholder %d, have %d args)",
				argIdx+1, len(args))
		}
		v := args[argIdx].Value
		argIdx++
		switch val := v.(type) {
		case nil:
			b.WriteString("NULL")
		case bool:
			if val {
				b.WriteString("TRUE")
			} else {
				b.WriteString("FALSE")
			}
		case int64:
			fmt.Fprintf(&b, "%d", val)
		case float64:
			// NaN/±Inf have no SQL literal form: rendering them with %g yields
			// "NaN"/"+Inf"/"-Inf", which the parser then rejects with a confusing
			// 42601 syntax error (an identifier reference). Reject up front with a
			// clear, type-accurate error. (Proper non-text parameter binding would
			// let a DOUBLE column carry these IEEE-754 values; that is the broader
			// text-interpolation divergence, tracked separately.)
			if math.IsNaN(val) || math.IsInf(val, 0) {
				return "", api.NewErrorf(api.ErrCodeInvalidParameter,
					"non-finite float64 parameter (NaN/Inf) is not supported for placeholder %d", argIdx)
			}
			fmt.Fprintf(&b, "%g", val)
		case string:
			// Escape single quotes by doubling them.
			b.WriteByte('\'')
			b.WriteString(strings.ReplaceAll(val, "'", "''"))
			b.WriteByte('\'')
		case time.Time:
			b.WriteByte('\'')
			if val.Hour() == 0 && val.Minute() == 0 && val.Second() == 0 && val.Nanosecond() == 0 {
				b.WriteString(functions.FormatDate(val))
			} else {
				b.WriteString(functions.FormatTimestamp(val))
			}
			b.WriteByte('\'')
		case []byte:
			// A []byte parameter must render as a BYTES literal `X'<hex>'`, NOT a
			// string literal `'<hex>'`. Without the `X` prefix the value is parsed
			// as a STRING containing the hex digits and stored as those ASCII
			// bytes — so a []byte bound to a BYTES column round-trips as the wrong
			// bytes (and a Java reader sees a hex string, not the intended bytes).
			b.WriteString("X'")
			for _, bv := range val {
				fmt.Fprintf(&b, "%02x", bv)
			}
			b.WriteByte('\'')
		default:
			return "", api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"unsupported parameter type %T for placeholder %d", v, argIdx)
		}
	}
	if argIdx < len(args) {
		return "", api.NewErrorf(api.ErrCodeInvalidParameter,
			"fewer '?' placeholders than bound parameters (%d placeholders, %d args)",
			argIdx, len(args))
	}
	return b.String(), nil
}

// parseDecimalLiteralValue mirrors Java's literal-token parsing rules:
// integer-shape text (no '.', no exponent — DECIMAL_LITERAL) goes to
// int64; on overflow, error byte-equal with Java's
// `NumberFormatException: For input string: "<text>"`. Float-shape text
// (REAL_LITERAL — has '.' or exponent) parses to float64.
func parseDecimalLiteralValue(text string) (any, error) {
	isFloatShape := strings.ContainsAny(text, ".eE")
	if !isFloatShape {
		iv, err := strconv.ParseInt(text, 10, 64)
		if err == nil {
			return iv, nil
		}
		// Java's lexer emits the literal as a Long token; Long.parseLong
		// throws NumberFormatException for any token that overflows long.
		// The conformance harness compares the deepest cause message,
		// which is the NumberFormatException's `For input string: "<text>"`.
		// Match byte-equal (no exception class prefix).
		return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
			"For input string: %q", text)
	}
	fv, err := strconv.ParseFloat(text, 64)
	if err != nil {
		// strconv.ParseFloat returns (±Inf, ErrRange) on magnitude
		// overflow — treat as 22003 NUMERIC_VALUE_OUT_OF_RANGE. Any
		// other parse error is a malformed literal → 22023.
		if errors.Is(err, strconv.ErrRange) {
			return nil, api.NewErrorf(api.ErrCodeNumericValueOutOfRange, "decimal literal %q overflows float64", text)
		}
		return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "cannot parse decimal literal %q: %v", text, err)
	}
	return fv, nil
}

// evalConstant evaluates a constant parse-tree node to a Go value.
// Returns nil for NULL.
func evalConstant(c antlrgen.IConstantContext) (any, error) {
	switch cv := c.(type) {
	case *antlrgen.DecimalConstantContext:
		text := cv.DecimalLiteral().GetText()
		return parseDecimalLiteralValue(text)
	case *antlrgen.NegativeDecimalConstantContext:
		text := "-" + cv.DecimalLiteral().GetText()
		return parseDecimalLiteralValue(text)
	case *antlrgen.StringConstantContext:
		raw := cv.StringLiteral().GetText()
		if len(raw) >= 2 {
			raw = raw[1 : len(raw)-1]
		}
		// Unescape doubled single-quotes produced by substituteParams or typed literally.
		raw = strings.ReplaceAll(raw, "''", "'")
		return raw, nil
	case *antlrgen.NullConstantContext:
		return nil, nil
	case *antlrgen.BooleanConstantContext:
		return cv.BooleanLiteral().TRUE() != nil, nil
	case *antlrgen.BytesConstantContext:
		// Grammar produces either HEXADECIMAL_LITERAL ('x' followed by
		// hex in single quotes) or BASE64_LITERAL ('b64' followed by
		// base64 in single quotes).
		bl := cv.BytesLiteral()
		if bl == nil {
			return nil, api.NewError(api.ErrCodeInvalidParameter, "empty bytes literal")
		}
		if hexLit := bl.HEXADECIMAL_LITERAL(); hexLit != nil {
			text := hexLit.GetText()
			// text looks like: x'deadbeef' or X'deadbeef'
			body := stripBytesWrapper(text, "x")
			// encoding/hex.DecodeString handles both odd-length and
			// non-hex-char failures uniformly.
			out, err := hex.DecodeString(body)
			if err != nil {
				return nil, api.NewErrorf(api.ErrCodeInvalidBinaryRepresentation, "invalid hex literal %q: %v", text, err)
			}
			return out, nil
		}
		if b64 := bl.BASE64_LITERAL(); b64 != nil {
			text := b64.GetText()
			body := stripBytesWrapper(text, "b64")
			out, err := decodeBase64(body)
			if err != nil {
				return nil, api.NewErrorf(api.ErrCodeInvalidBinaryRepresentation, "invalid base64 in %q: %v", text, err)
			}
			return out, nil
		}
		return nil, api.NewError(api.ErrCodeInvalidParameter, "bytes literal must be hex or base64")
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported constant type %T in WHERE", c)
	}
}

// stripBytesWrapper removes the `<prefix>'...'` wrapping from a bytes
// literal text token. Case-insensitive on the prefix to accept x / X
// and b64 / B64.
func stripBytesWrapper(text, prefix string) string {
	lower := strings.ToLower(text)
	if strings.HasPrefix(lower, prefix) {
		text = text[len(prefix):]
	}
	text = strings.TrimPrefix(text, "'")
	text = strings.TrimSuffix(text, "'")
	return text
}

// base64StdStrict is the standard Base64 encoding with strict
// padding (no line breaks, no URL-safe alternative). Mirrors what
// Java's Base64.getDecoder() accepts for the b64'...' literal form.
var base64StdStrict = base64.StdEncoding.Strict()

func decodeBase64(s string) ([]byte, error) {
	return base64StdStrict.DecodeString(s)
}

// valuesEqual compares two driver values that may have different numeric types.
func valuesEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	// Exact int64 comparison avoids float64 precision loss for large integers
	// (> 2^53 cannot be represented exactly as float64).
	if ai, ok1 := a.(int64); ok1 {
		if bi, ok2 := b.(int64); ok2 {
			return ai == bi
		}
	}
	// Normalise mixed int64/float64 pairs to float64.
	toFloat := func(v any) (float64, bool) {
		switch n := v.(type) {
		case int64:
			return float64(n), true
		case float64:
			return n, true
		}
		return 0, false
	}
	fa, aIsNum := toFloat(a)
	fb, bIsNum := toFloat(b)
	if aIsNum && bIsNum {
		return fa == fb
	}
	// One numeric and one non-numeric → not equal. SQL rejects cross-type
	// comparison (PostgreSQL errors; we return false to stay non-fatal).
	if aIsNum != bIsNum {
		return false
	}
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case []byte:
		bv, ok := b.([]byte)
		return ok && bytes.Equal(av, bv)
	}
	// Last resort for exotic driver values: compare only if concrete types
	// match, avoid `'5' = 5` stringification bugs.
	if reflect.TypeOf(a) != reflect.TypeOf(b) {
		return false
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

// rowKey serializes a result row to a collision-free string key for DISTINCT deduplication.
// Each field is length-prefixed: "<type-tag>:<len>:<bytes>|" so that string values
// containing separator characters cannot collide with other fields or NULL markers.
func rowKey(row []driver.Value) string {
	var b strings.Builder
	for _, v := range row {
		if v == nil {
			b.WriteString("N:0:|")
			continue
		}
		s := fmt.Sprintf("%T\x00%v", v, v)
		fmt.Fprintf(&b, "V:%d:%s|", len(s), s)
	}
	return b.String()
}
