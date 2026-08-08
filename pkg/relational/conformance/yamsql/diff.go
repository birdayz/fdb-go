package yamsql

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"fdb.dev/pkg/relational/api"
)

// diffRows compares an expected row set against an actual row set.
// Returns a non-empty diff string iff they differ.
//
// Normalisation:
//   - int expected literals promoted to int64 to match sql.Scan.
//   - uint expected literals promoted to int64 (small constants only).
//   - float expected literals promoted to float64.
//   - Byte slices compared by content, not identity.
//
// When unordered is true, both sides are sorted by a canonical string key
// before comparison.
func diffRows(expected, actual [][]any, unordered bool) string {
	ex := normalizeRows(expected)
	ac := normalizeRows(actual)
	if unordered {
		sortRows(ex)
		sortRows(ac)
	}
	if rowsEqual(ex, ac) {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "row set mismatch (unordered=%v)\n", unordered)
	fmt.Fprintf(&b, "  expected (%d rows):\n", len(ex))
	for _, r := range ex {
		fmt.Fprintf(&b, "    %s\n", formatRow(r))
	}
	fmt.Fprintf(&b, "  actual (%d rows):\n", len(ac))
	for _, r := range ac {
		fmt.Fprintf(&b, "    %s\n", formatRow(r))
	}
	return b.String()
}

func normalizeRows(rows [][]any) [][]any {
	out := make([][]any, len(rows))
	for i, r := range rows {
		nr := make([]any, len(r))
		for j, v := range r {
			nr[j] = normalizeValue(v)
		}
		out[i] = nr
	}
	return out
}

func normalizeValue(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case int64:
		return x
	case uint:
		return int64(x)
	case uint32:
		return int64(x)
	case uint64:
		return int64(x)
	case float32:
		return float64(x)
	case float64:
		return x
	case bool:
		return x
	case string:
		return x
	case []byte:
		// Defensive copy — the row may be reused by the driver.
		cp := make([]byte, len(x))
		copy(cp, x)
		return cp
	case api.Struct:
		// A STRUCT column, normalised POSITIONALLY into a plain slice so it
		// compares against a nested YAML sequence (`- [3, [30, 3], true]`).
		// Position is the right key because that is what the metadata's
		// ordinals mean, and it is Java's own contract in MessageTuple.
		//
		// Without this arm a struct column fell to the `%T:%v` default and
		// rendered as a POINTER ADDRESS, so no scenario could ever assert one:
		// the expectation could not be written down at all, and the whole
		// struct-valued-column axis was silently unassertable.
		attrs := x.Attributes()
		out := make([]any, len(attrs))
		for i, a := range attrs {
			out[i] = normalizeValue(a)
		}
		return out
	case []any:
		// A nested sequence from the YAML side (the expected spelling of a
		// struct column), normalised elementwise so `[30, 3]` promotes its
		// int literals exactly as a top-level row value would.
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = normalizeValue(e)
		}
		return out
	default:
		// Unknown types (complex values from the driver) compared by %v.
		return fmt.Sprintf("%T:%v", v, v)
	}
}

func rowsEqual(a, b [][]any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if !valueEqual(a[i][j], b[i][j]) {
				return false
			}
		}
	}
	return true
}

func valueEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	switch av := a.(type) {
	case []byte:
		bv, ok := b.([]byte)
		return ok && bytes.Equal(av, bv)
	case []any:
		// A normalised STRUCT column (or an expected nested sequence). Slices
		// are never comparable with ==, so the default arm below would PANIC
		// rather than answer; compare elementwise and recurse so a struct of
		// structs works too.
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !valueEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	case int64:
		bv, ok := b.(int64)
		if ok {
			return av == bv
		}
		// int64 vs float64 — compare as float (SUM in SQL may return float).
		if bf, ok := b.(float64); ok {
			return float64(av) == bf
		}
		return false
	case float64:
		bv, ok := b.(float64)
		if ok {
			return av == bv
		}
		if bi, ok := b.(int64); ok {
			return av == float64(bi)
		}
		return false
	default:
		return a == b
	}
}

// sortRows sorts in-place using a canonical key built from each row's values.
// Used to support unordered equality.
func sortRows(rows [][]any) {
	sort.SliceStable(rows, func(i, j int) bool {
		return rowKey(rows[i]) < rowKey(rows[j])
	})
}

func rowKey(r []any) string {
	var b strings.Builder
	for i, v := range r {
		if i > 0 {
			b.WriteByte('|')
		}
		fmt.Fprintf(&b, "%T:%v", v, v)
	}
	return b.String()
}

func formatRow(r []any) string {
	parts := make([]string, len(r))
	for i, v := range r {
		if v == nil {
			parts[i] = "NULL"
		} else {
			parts[i] = fmt.Sprintf("%v", v)
		}
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
