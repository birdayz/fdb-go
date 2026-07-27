package functions

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
)

// CastValue implements SQL CAST(v AS typeName). Splits INTEGER
// (32-bit) from BIGINT (64-bit) per Java CastValue range-check
// semantics: `CAST(9223372036854775807 AS INTEGER)` errors 22F3H
// because the value exceeds Integer.MAX_VALUE, even though the
// runtime representation of int64 doesn't need narrowing.
//
// NULL casts to NULL of the target type. Unsupported source/target
// combinations error with ErrCodeUnsupportedOperation.
func CastValue(v any, typeName string) (any, error) {
	// SQL: CAST(NULL AS <type>) is NULL of the target type.
	if v == nil {
		return nil, nil
	}
	// Split INTEGER (32-bit) from BIGINT (64-bit) so CAST range-checks
	// match Java: `CAST(9223372036854775807 AS INTEGER)` errors 22F3H
	// because the value exceeds Integer.MAX_VALUE. Java's CastValue
	// applies LONG_TO_INT validation even though our runtime value
	// type stays int64 (Go doesn't need a narrower representation).
	is32BitInteger := typeName == "INTEGER" || typeName == "INT"
	switch {
	case is32BitInteger, strings.HasPrefix(typeName, "BIGINT"), strings.HasPrefix(typeName, "INT"), typeName == "LONG":
		switch n := v.(type) {
		case int64:
			if is32BitInteger && (n < math.MinInt32 || n > math.MaxInt32) {
				return nil, api.NewErrorf(api.ErrCodeInvalidCast,
					"Invalid cast operation Value out of range for INT: %d", n)
			}
			return n, nil
		case float64:
			// Java CastValue.DOUBLE_TO_LONG: reject NaN/Inf, round to nearest
			// using ties-to-positive-infinity (`Math.round` = floor(x + 0.5)),
			// error on range overflow. Previously Go truncated silently and
			// relied on int64() wrap on overflow — both diverged from Java.
			if math.IsNaN(n) || math.IsInf(n, 0) {
				return nil, api.NewErrorf(api.ErrCodeInvalidCast,
					"cannot CAST NaN or Infinity to integer")
			}
			// Java's Math.round(double) returns floor(x + 0.5).
			rounded := math.Floor(n + 0.5)
			// Guard overflow before the int64() conversion. float64 can't
			// represent every int64 exactly near the limits, so use a strict
			// comparison against the max/min-as-float (values that *do* fit
			// exactly into float64).
			if rounded > 9.2233720368547748e18 || rounded < -9.2233720368547758e18 {
				// Java CastValue uses INVALID_CAST (22F3H) for all CAST
				// failures including range overflow — matches our
				// ErrCodeInvalidCast. Distinct from arithmetic-overflow
				// sites (which use 22003) because Java specifically
				// categorises CAST failures separately.
				return nil, api.NewErrorf(api.ErrCodeInvalidCast,
					"value out of range for integer: %v", n)
			}
			r := int64(rounded)
			if is32BitInteger && (r < math.MinInt32 || r > math.MaxInt32) {
				return nil, api.NewErrorf(api.ErrCodeInvalidCast,
					"Invalid cast operation Value out of range for INT: %d", r)
			}
			return r, nil
		case string:
			// Java CastValue.STRING_TO_LONG: Integer.parseInt(in.trim()) —
			// trims whitespace before parsing.
			i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
			if err != nil {
				// Java verbatim: 'Invalid cast operation Cannot cast
				// string 'X' to LONG: For input string: "X"' — the
				// quirky duplication is Java's stock NumberFormatException
				// message, wrapped by fdb-relational's "Invalid cast
				// operation" prefix.
				return nil, api.NewErrorf(api.ErrCodeInvalidCast,
					"Invalid cast operation Cannot cast string '%s' to LONG: For input string: \"%s\"", n, n)
			}
			if is32BitInteger && (i < math.MinInt32 || i > math.MaxInt32) {
				return nil, api.NewErrorf(api.ErrCodeInvalidCast,
					"Invalid cast operation Value out of range for INT: %d", i)
			}
			return i, nil
		case bool:
			if n {
				return int64(1), nil
			}
			return int64(0), nil
		}
	case strings.HasPrefix(typeName, "FLOAT"):
		// FLOAT is binary32 (a FLOAT column's index entries pack as tuple code
		// 0x20), so a cast TO it must ROUND, matching Java's DOUBLE_TO_FLOAT
		// (`value.floatValue()`), INT/LONG_TO_FLOAT (`Float.valueOf`) and
		// STRING_TO_FLOAT (`Float.parseFloat`) — CastValue.java:126-205.
		// Sharing the DOUBLE arm below made the cast a no-op, wrong in BOTH
		// directions on a value binary32 cannot represent: it matched DOUBLE
		// rows the rounded constant differs from, and missed the FLOAT rows it
		// equals. The rounding, the NaN/Infinite rejection and the range check
		// match values.CastValue's FLOAT arm exactly — the two CAST
		// implementations must agree on those or a constant fold and its
		// runtime disagree about the same expression. They are NOT identical
		// overall: this copy has no bool source arm (neither here nor in the
		// DOUBLE case below), so CAST(<bool> AS FLOAT) is unsupported on the
		// map path while the query engine converts it. That gap predates the
		// float work and is left alone rather than widened silently.
		switch n := v.(type) {
		case float64:
			if math.IsNaN(n) || math.IsInf(n, 0) {
				return nil, api.NewErrorf(api.ErrCodeInvalidCast,
					"Invalid cast operation Cannot cast NaN or Infinite to FLOAT")
			}
			if n > math.MaxFloat32 || n < -math.MaxFloat32 {
				return nil, api.NewErrorf(api.ErrCodeInvalidCast,
					"Invalid cast operation Value out of range for FLOAT: %s", javaDoubleToString(n))
			}
			return float64(float32(n)), nil
		case float32:
			return float64(n), nil
		case int64:
			return float64(float32(n)), nil
		case string:
			// bitSize 32 rounds to binary32. ErrRange is NOT a failure: Go
			// reports binary32 overflow by returning ±Inf WITH an error, while
			// Float.parseFloat returns Infinity and throws only on malformed
			// text, so rejecting it would refuse `CAST('1e39' AS FLOAT)` that
			// Java accepts. The returned value is already the ±Inf Java gives.
			f, err := strconv.ParseFloat(strings.TrimSpace(n), 32)
			if err != nil && !errors.Is(err, strconv.ErrRange) {
				return nil, api.NewErrorf(api.ErrCodeInvalidCast, "cannot CAST %q to float: %v", n, err)
			}
			return f, nil
		}
	case strings.HasPrefix(typeName, "DOUBLE"), strings.HasPrefix(typeName, "DECIMAL"), strings.HasPrefix(typeName, "NUMERIC"):
		switch n := v.(type) {
		case float64:
			return n, nil
		case int64:
			return float64(n), nil
		case string:
			// Java CastValue.STRING_TO_DOUBLE: Double.parseDouble(in.trim()) —
			// trims whitespace before parsing.
			f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
			if err != nil {
				return nil, api.NewErrorf(api.ErrCodeInvalidCast, "cannot CAST %q to float: %v", n, err)
			}
			return f, nil
		}
	case strings.HasPrefix(typeName, "VARCHAR"), strings.HasPrefix(typeName, "CHAR"), typeName == "TEXT", typeName == "STRING":
		switch n := v.(type) {
		case string:
			return n, nil
		case int64:
			return strconv.FormatInt(n, 10), nil
		case float64:
			return javaDoubleToString(n), nil
		case bool:
			if n {
				return "true", nil
			}
			return "false", nil
		case [16]byte:
			// A UUID flows through the engine as a neutral [16]byte (RFC-162);
			// CAST(uuid AS STRING) renders the canonical 36-char form, matching
			// Java's UUID.toString().
			return uuid.UUID(n).String(), nil
		}
	case typeName == "UUID":
		// CAST(<expr> AS UUID): only string → UUID is supported (matches
		// Java's CastValue.STRING_TO_UUID via java.util.UUID.fromString).
		// Validate the canonical 36-char form; the SQL layer carries
		// UUID values as canonical strings, with the proto-write
		// boundary in ConvertToProtoValue encoding them as the
		// tuple_fields.UUID message.
		if n, ok := v.(string); ok {
			u, err := uuid.Parse(strings.TrimSpace(n))
			if err != nil {
				// Java verbatim: 'Invalid UUID value for the UUID type X'
				// (where X is the input string, no quotes). Aligned
				// .
				return nil, api.NewErrorf(api.ErrCodeInvalidCast,
					"Invalid UUID value for the UUID type %s", n)
			}
			return u.String(), nil
		}
	case typeName == "BOOLEAN", typeName == "BOOL":
		switch n := v.(type) {
		case bool:
			return n, nil
		case int64:
			return nil, api.NewErrorf(api.ErrCodeInvalidCast,
				"Invalid cast operation No cast defined from LONG to BOOLEAN")
		case string:
			s := strings.ToLower(strings.TrimSpace(n))
			switch s {
			case "true", "1":
				return true, nil
			case "false", "0":
				return false, nil
			}
			return nil, api.NewErrorf(api.ErrCodeInvalidCast, "cannot CAST %q to boolean", n)
		}
	case typeName == "DATE":
		switch n := v.(type) {
		case time.Time:
			return FormatDate(n), nil
		case string:
			if t, ok := ParseTimestamp(strings.TrimSpace(n)); ok {
				return FormatDate(t), nil
			}
			return nil, api.NewErrorf(api.ErrCodeInvalidCast, "cannot CAST %q to DATE", n)
		case int64:
			return FormatDate(time.Unix(n*86400, 0).UTC()), nil
		}
	case typeName == "TIMESTAMP":
		switch n := v.(type) {
		case time.Time:
			return FormatTimestamp(n), nil
		case string:
			if t, ok := ParseTimestamp(strings.TrimSpace(n)); ok {
				return FormatTimestamp(t), nil
			}
			return nil, api.NewErrorf(api.ErrCodeInvalidCast, "cannot CAST %q to TIMESTAMP", n)
		case int64:
			return FormatTimestamp(time.UnixMilli(n).UTC()), nil
		}
	}
	return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported CAST from %T to %s", v, typeName)
}

// javaDoubleToString matches Java's Double.toString(double); the
// contract (decimal inside [1e-3, 1e7), scientific outside, Infinity
// spellings) lives in ONE place — values.JavaDoubleToString.
func javaDoubleToString(n float64) string {
	return values.JavaDoubleToString(n)
}

// javaFloatToString is Float.toString — the float32 sibling.
func javaFloatToString(n float32) string {
	return values.JavaFloatToString(n)
}

// StripStringLiteralQuotes removes a single pair of surrounding
// single-quotes and unescapes doubled-quote ” sequences to a single
// quote. Used by every SQL string literal that reaches our code via
// the parser (the parser leaves the literal's raw source text,
// including quotes).
func StripStringLiteralQuotes(s string) string {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		s = s[1 : len(s)-1]
	}
	return strings.ReplaceAll(s, "''", "'")
}
