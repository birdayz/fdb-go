package embedded

import (
	"database/sql/driver"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/functions"
)

// SQL driver parameter text transport. Expression evaluation belongs to the
// shared typed expression compiler, including INFORMATION_SCHEMA filters.

// renderableNaNs maps each NaN bit pattern the SQL text form can express to the
// text that produces it. There are TWO, because the CAST target changes the
// answer, and missing the second one is not academic:
//
//	CAST('NaN' AS DOUBLE) -> 0x7ff8000000000001
//	CAST('NaN' AS FLOAT)  -> 0x7ff8000000000000
//
// 0x7ff8000000000000 IS JAVA'S Double.NaN, and it is what a Go caller gets from
// float32(math.NaN()) widened back. Refusing it would mean a Go client could not
// re-bind a NaN it had just read out of a record Java wrote — a shared-cluster
// invariant violation, which is the whole point of this port, over a value that
// is perfectly representable.
//
// Both patterns are DERIVED from the same strconv.ParseFloat calls
// functions/cast.go makes (bitSize 64 and 32 respectively, each returning its
// result unchanged) rather than written as constants. Deriving matters here more
// than usual: ParseFloat with bitSize 32 returning float64(float32(NaN)) is
// exactly the sort of chain that reads as an implementation detail, and a
// hardcoded pair would quietly become a rule about patterns nothing produces.
var renderableNaNs = func() map[uint64]string {
	out := make(map[uint64]string, 2)
	for _, form := range []struct {
		bitSize int
		text    string
	}{
		{bitSize: 64, text: "CAST('NaN' AS DOUBLE)"},
		// The OUTER DOUBLE cast is not decoration. A bound float64 must
		// advertise DOUBLE, and the inner FLOAT cast is what produces the bits
		// — left bare it also changes the expression's STATIC type, which is
		// observable in two ways that have nothing to do with the value:
		// `SELECT ?` reported FLOAT column metadata where every other float64
		// parameter reports DOUBLE, and `CAST(? AS FLOAT)` collapsed into an
		// identity cast that SUCCEEDED, where a genuine DOUBLE→FLOAT cast of a
		// NaN must fail 22F3H (cast.go's float64 arm, matching
		// CastValue.java:168-170). Both MEASURED before this wrapper existed.
		//
		// The outer cast restores the type without touching the bits: the
		// DOUBLE arm is `case float64: return n, nil`, a plain identity for a
		// value that is already float64. Also measured end to end —
		// CAST(CAST('NaN' AS FLOAT) AS DOUBLE) evaluates to
		// 0x7ff8000000000000.
		{bitSize: 32, text: "CAST(CAST('NaN' AS FLOAT) AS DOUBLE)"},
	} {
		parsed, err := strconv.ParseFloat("NaN", form.bitSize)
		if err != nil {
			// Unreachable: "NaN" is a valid ParseFloat input by definition.
			continue
		}
		// First writer wins, so the DOUBLE spelling stays canonical if the two
		// ever collapse onto one pattern.
		if _, seen := out[math.Float64bits(parsed)]; !seen {
			out[math.Float64bits(parsed)] = form.text
		}
	}
	return out
}()

// renderableNaNPatterns lists the accepted patterns for an error message, in a
// stable order so the text does not depend on map iteration.
func renderableNaNPatterns() string {
	bits := make([]uint64, 0, len(renderableNaNs))
	for b := range renderableNaNs {
		bits = append(bits, b)
	}
	sort.Slice(bits, func(i, j int) bool { return bits[i] < bits[j] })
	parts := make([]string, 0, len(bits))
	for _, b := range bits {
		parts = append(parts, fmt.Sprintf("%#016x (%s)", b, renderableNaNs[b]))
	}
	return strings.Join(parts, ", ")
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
			// NaN and ±Infinity have no BARE literal form — Go formats them
			// as NaN/+Inf/-Inf, which the parser reads as identifiers and
			// rejects with a confusing 42601. They do have a CAST form, and it
			// is the same one on both sides of the port: 'NaN', 'Infinity' and
			// '-Infinity' are exactly the strings Go's strconv.ParseFloat and
			// Java's Double.parseDouble both accept.
			//
			// A blanket refusal of non-finite parameters was wrong: it made a
			// DOUBLE column unable to carry values it stores perfectly well
			// through every other syntax, on the grounds of how this driver
			// happens to transport parameters. A transport limitation must not
			// become a type restriction.
			//
			// But that argument reaches exactly as far as VALUES THE TEXT FORM
			// PRESERVES, and it does not license rewriting the ones it does
			// not. Every finite double and both infinities render exactly; NaN
			// PAYLOADS do not, and the NaN arm below refuses precisely those
			// rather than quietly substituting the parse constant. Refusing to
			// carry a value states a limit; changing it while reporting success
			// is corruption.
			switch {
			case math.IsNaN(val):
				// A NaN carries a SIGN and a 51-bit PAYLOAD, and those bits are
				// OBSERVABLE: they come back on readback and they are what an
				// index or aggregate-index key is packed from — two payloads are
				// two physical entries. The 'NaN' literal parses to exactly one
				// of them (strconv.ParseFloat, the same call the CAST path
				// makes), so rendering every NaN that way would SILENTLY
				// REPLACE the user's bits with the parse constant.
				//
				// That is the defect this whole item was fixing, one level
				// down: a write whose stored value depends on which syntax
				// carried it. Preserving arbitrary bits is not possible here —
				// the SQL driver passes parameters only as interpolated SQL
				// TEXT, not through the executor's typed bindings (see the
				// bound-parameter entry in DIVERGENCES.md and RFC-254), and no
				// literal in this grammar denotes an
				// arbitrary double bit pattern. Arithmetic reaches ±Infinity
				// and one negative NaN, not a payload.
				//
				// So the honest answer is a NARROW refusal: each NaN the text
				// form round-trips EXACTLY is rendered with the spelling that
				// produces it, and only a pattern no spelling reaches is
				// rejected. Silently rewriting bits would be worse than either
				// alternative, and refusing too much breaks the two ordinary
				// cases — math.NaN() and Java's Double.NaN are BOTH in the
				// renderable set, so binding either works and round-trips
				// bit-exact.
				//
				// "Bit-exact" is a promise about the TRANSPORT, not about the
				// destination column: writing into a FLOAT column narrows, and
				// narrowing loses a NaN payload exactly as it loses a mantissa
				// bit of 0.1. That is a declared type conversion, which Java
				// does too; this arm's job is only to not corrupt the value on
				// its way in.
				rendering, renderable := renderableNaNs[math.Float64bits(val)]
				if !renderable {
					return "", api.NewErrorf(api.ErrCodeInvalidParameter,
						"NaN parameter for placeholder %d has bit pattern %#016x, which the "+
							"driver's text parameter path cannot represent without changing it "+
							"(representable NaN patterns: %s); bind one of those, or write the "+
							"value with an expression",
						argIdx, math.Float64bits(val), renderableNaNPatterns())
				}
				b.WriteString(rendering)
			case math.IsInf(val, 1):
				b.WriteString("CAST('Infinity' AS DOUBLE)")
			case math.IsInf(val, -1):
				b.WriteString("CAST('-Infinity' AS DOUBLE)")
			default:
				// Exponent syntax preserves DOUBLE's type as well as its bits.
				// %g renders whole doubles as integer literals: -0 loses its
				// sign, and 3 / 2 selects integer division. Precision -1 keeps
				// every finite value exact, including subnormals and -0.
				b.WriteString(strconv.FormatFloat(val, 'e', -1, 64))
			}
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
