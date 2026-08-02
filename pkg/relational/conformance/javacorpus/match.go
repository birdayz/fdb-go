package javacorpus

import (
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"

	"fdb.dev/pkg/relational/conformance/javayamsql"
)

// resultSet is one materialised result: the column identity the by-name cells
// resolve against, plus the rows.
type resultSet struct {
	Cols []string
	// Types are the driver's DatabaseTypeName per column. They are what lets
	// the matcher reconstruct the Java runtime type of a cell, which Java's
	// Matchers compares against directly (Integer vs Long is a real
	// distinction there: Objects.equals(1L, 1) is false).
	Types []string
	Rows  [][]any
}

// matchResultSet mirrors Matchers.matchResultSet.
//
// The ordered/unordered split is Java's: an ordered expectation walks both
// sequences in lockstep, an unordered one treats the expectation as a MULTISET
// and consumes one element per actual row, so duplicate rows must appear the
// same number of times on both sides.
func matchResultSet(cfg *javayamsql.Config, rs *resultSet, ordered bool) error {
	if cfg.Raw.TagName() == javayamsql.TagIgnore {
		return nil
	}
	if cfg.Raw.IsNull() {
		// Java: a null expectation requires a null result set, i.e. the
		// statement produced none at all.
		if rs != nil {
			return fmt.Errorf("actual result set is non-NULL, expecting NULL result set")
		}
		return nil
	}
	if rs == nil {
		return fmt.Errorf("actual result set is NULL, expecting non-NULL result set")
	}
	if ordered {
		return matchOrdered(cfg.Rows, rs)
	}
	return matchUnordered(cfg.Rows, rs)
}

func matchOrdered(expected []javayamsql.Row, rs *resultSet) error {
	for i, exp := range expected {
		if i >= len(rs.Rows) {
			return fmt.Errorf("result does not contain all expected rows! expected %d rows, got %d",
				len(expected), len(rs.Rows))
		}
		if err := matchRow(exp, rs, rs.Rows[i], i+1); err != nil {
			return err
		}
	}
	if len(rs.Rows) > len(expected) {
		return fmt.Errorf("too many rows in actual result set! expected %d rows, got %d",
			len(expected), len(rs.Rows))
	}
	return nil
}

func matchUnordered(expected []javayamsql.Row, rs *resultSet) error {
	// Java removes the matched element from a HashMultiset, so an expectation
	// listed twice must be satisfied twice. Tracking consumption by index is
	// the same thing without needing rows to be hashable.
	used := make([]bool, len(expected))
	for i, actual := range rs.Rows {
		if len(expected) == 0 {
			return fmt.Errorf("too many rows in actual result set! expected 0 row(s), got %d row(s) instead", len(rs.Rows))
		}
		found := false
		for j, exp := range expected {
			if used[j] {
				continue
			}
			if matchRow(exp, rs, actual, i+1) == nil {
				used[j] = true
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("actual row at %d does not match any expected records:\n  actual: %s",
				i+1, renderRow(rs, actual))
		}
	}
	remaining := 0
	for _, u := range used {
		if !u {
			remaining++
		}
	}
	if remaining > 0 {
		return fmt.Errorf("result does not contain all expected rows, expected %d row(s), got %d row(s) instead",
			len(expected), len(rs.Rows))
	}
	return nil
}

// matchRow mirrors Matchers.matchRow + matchMap at the top level.
func matchRow(exp javayamsql.Row, rs *resultSet, actual []any, rowNum int) error {
	if len(actual) != len(exp.Cells) {
		return fmt.Errorf("row cardinality mismatch at %d! expected a row comprising %d column(s), received %d column(s) instead",
			rowNum, len(exp.Cells), len(actual))
	}
	for i, cell := range exp.Cells {
		var (
			val     any
			sqlType string
			ref     string
		)
		if cell.Positional() {
			pos := cell.ColumnIndex(i + 1)
			if pos < 1 || int(pos) > len(actual) {
				return fmt.Errorf("cell mismatch at row %d: column position %d is out of range (row has %d columns)",
					rowNum, pos, len(actual))
			}
			val = actual[pos-1]
			sqlType = typeAt(rs, int(pos-1))
			ref = fmt.Sprintf("pos<%d>", pos)
		} else {
			name, _ := cell.ColumnName()
			idx := columnIndex(rs.Cols, name)
			if idx < 0 {
				return fmt.Errorf("cell mismatch at row %d: no column named %q in result (columns: %v)",
					rowNum, name, rs.Cols)
			}
			val = actual[idx]
			sqlType = typeAt(rs, idx)
			ref = name
		}
		if err := matchField(cell.Expected(), val, sqlType, rowNum, ref); err != nil {
			return err
		}
	}
	return nil
}

func typeAt(rs *resultSet, i int) string {
	if rs == nil || i < 0 || i >= len(rs.Types) {
		return ""
	}
	return rs.Types[i]
}

// columnIndex resolves a column name case-insensitively, matching
// RelationalStruct.getOneBasedPosition's equalsIgnoreCase walk.
func columnIndex(cols []string, name string) int {
	for i, c := range cols {
		if strings.EqualFold(c, name) {
			return i
		}
	}
	return -1
}

// matchField mirrors Matchers.matchField, in the same order — the order is
// load-bearing, because several branches overlap (a String expectation against
// a byte[] actual is handled before the exact comparison that would reject it).
func matchField(exp *javayamsql.Value, actual any, sqlType string, rowNum int, cellRef string) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("cell mismatch at row %d, cell %s: %s", rowNum, cellRef, fmt.Sprintf(format, args...))
	}

	// Every Matchable tag routes its null rejection through
	// Matchable.shouldNotBeNull, which emits one generic sentence rather than a
	// per-tag one. Sharing the wording here keeps a failure diagnosable against
	// the Java message it corresponds to.
	notNull := func() error {
		return fmt.Errorf("unexpected NULL at row: %d, cell: %s", rowNum, cellRef)
	}

	switch exp.TagName() {
	case javayamsql.TagIgnore:
		return nil
	case javayamsql.TagNull:
		if actual == nil {
			return nil
		}
		return fail("expected NULL, got '%v' instead", actual)
	case javayamsql.TagNotNull:
		if actual == nil {
			return notNull()
		}
		return nil
	case javayamsql.TagStringContains:
		want, _ := exp.AsString()
		if actual == nil {
			return notNull()
		}
		s, ok := actualString(actual)
		if !ok {
			return fail("expected to match against a String value, however we got %v of type %T", actual, actual)
		}
		if !strings.Contains(s, want) {
			return fail("expected string containing '%s' instead got '%s'", want, s)
		}
		return nil
	case javayamsql.TagUUID:
		want, _ := exp.AsString()
		if actual == nil {
			return notNull()
		}
		s, ok := actualString(actual)
		if !ok || !strings.EqualFold(s, want) {
			return fail("expected UUID: '%s' got %v", want, actual)
		}
		return nil
	case javayamsql.TagRandomStr:
		desc, _ := exp.AsString()
		want, err := expandRandomStr(desc)
		if err != nil {
			return fail("%v", err)
		}
		if actual == nil {
			return notNull()
		}
		s, ok := actualString(actual)
		if !ok || s != want {
			return fail("!randomStr mismatch (expected length %d, actual %v)", len(want), lengthOf(actual))
		}
		return nil
	}

	// Untagged YAML null as an expectation: Java's `expected == null` branch.
	if exp.Kind == javayamsql.KindNull {
		if actual == nil {
			return nil
		}
		return fail("actual result set is non-NULL, expecting NULL result set")
	}
	// Every remaining expectation is a concrete value, and Java rejects a null
	// actual for all of them (only IsNullMatcher, handled above, tolerates it).
	if actual == nil {
		return fail("actual result set is NULL, expecting non-NULL result set")
	}

	switch exp.TagName() {
	case javayamsql.TagLong:
		n, err := expectedLong(exp)
		if err != nil {
			return fail("%v", err)
		}
		return matchLong(n, actual, sqlType, fail)
	case javayamsql.TagFloat:
		f, err := expectedFloat(exp)
		if err != nil {
			return fail("%v", err)
		}
		return matchFloat(f, actual, sqlType, fail)
	case javayamsql.TagBytes:
		want, err := decodeBytesTag(exp)
		if err != nil {
			return fail("%v", err)
		}
		got, ok := actual.([]byte)
		if !ok || string(got) != string(want) {
			return fail("expected bytes x'%s', got %v", hex.EncodeToString(want), actual)
		}
		return nil
	}

	// Java has one more arm here, between the tags and the scalar comparisons:
	// a String expectation against a protobuf EnumValueDescriptor actual
	// succeeds when the string equals the descriptor's NAME. It is not ported,
	// because whether it is needed depends on what the Go driver hands back for
	// an enum column — if that is already the name as a string, matchString
	// below covers it and the arm is dead weight; if it is an ordinal or a
	// typed value, the arm is required and its absence is a silent mismatch.
	// That question is unanswerable today: every corpus file with an enum
	// column is skipped before a row is compared, so nothing exercises it
	// either way. Booked under CQ-72 rather than guessed at — an untested arm
	// written on a hunch is worse than a stated omission.

	u := exp.Untag()
	switch u.Kind {
	case javayamsql.KindMap:
		return fail("expected to match a struct, got %v (%T) — struct-typed results are RFC-201 Phase 3", actual, actual)
	case javayamsql.KindSeq:
		return matchArray(u, actual, rowNum, cellRef, fail)
	case javayamsql.KindString:
		return matchString(u.Str, actual, fail)
	case javayamsql.KindBool:
		b, ok := actual.(bool)
		if !ok || b != u.Bool {
			return fail("expected %v (Boolean), got %v (%T)", u.Bool, actual, actual)
		}
		return nil
	case javayamsql.KindInt:
		// SnakeYAML resolves a plain integer to Integer when it fits and to
		// Long otherwise, and Matchers treats the two differently: an Integer
		// expectation promotes to a Long actual, a Long expectation does not
		// demote to an Integer one.
		if u.Int >= math.MinInt32 && u.Int <= math.MaxInt32 {
			return matchInteger(u.Int, actual, sqlType, fail)
		}
		return matchLong(u.Int, actual, sqlType, fail)
	case javayamsql.KindFloat:
		// A plain YAML float is a Double, which Objects.equals compares
		// without promotion.
		return matchDouble(u.Float, actual, sqlType, fail)
	}
	return fail("unsupported expectation kind %s", u.Kind)
}

// matchArray mirrors the nested-array branch: element i of the expectation is
// matched against element i of the actual array.
//
// A SURPLUS actual element is deliberately not an error, and the asymmetry is
// Java's, not an oversight here. Matchers' array loop is bounded by
// `expectedArray.size()` and returns success without ever asking whether the
// actual array had more — so an expectation of [1, 2] matches an actual of
// [1, 2, 3]. Its ROW loop does the opposite: after consuming the expected rows
// it drains the rest and fails with "too many rows in actual result set", which
// this file matches in matchOrdered.
//
// Being stricter than Java here would be the costliest direction to diverge in:
// the corpus's expectations are Java-blessed, so a Go-only rejection turns a
// file Java passes into a spurious red — and it would surface as a wave of
// false failures exactly when struct and array columns start working.
func matchArray(exp *javayamsql.Value, actual any, rowNum int, cellRef string, fail func(string, ...any) error) error {
	got, ok := actual.([]any)
	if !ok {
		return fail("expected to match an array, got %v (%T)", actual, actual)
	}
	for i, e := range exp.Seq {
		if i >= len(got) {
			return fail("expected (containing %d array items) does not match actual (containing %d array items)",
				len(exp.Seq), i)
		}
		if err := matchField(e, got[i], "", rowNum, fmt.Sprintf("%s[%d]", cellRef, i)); err != nil {
			return err
		}
	}
	return nil
}

// matchString mirrors the String branch, which is where the corpus's bare
// `x'deadbeef'` byte literals land: they are plain YAML strings, and Java
// reaches them through the String-vs-byte[] special case rather than through a
// tag.
func matchString(want string, actual any, fail func(string, ...any) error) error {
	if got, ok := actual.(string); ok {
		if got == want {
			return nil
		}
		return fail("expected %q (String), got %q", want, got)
	}
	if got, ok := actual.([]byte); ok {
		if string(got) == want {
			return nil
		}
		lower := strings.ToLower(want)
		if strings.HasPrefix(lower, "xstartswith_") && strings.HasSuffix(want, "'") {
			prefix, length, err := parseStartsWith(want)
			if err == nil && len(got) == length && len(prefix) <= len(got) &&
				string(got[:len(prefix)]) == string(prefix) {
				return nil
			}
		}
		if strings.HasPrefix(lower, "x'") && strings.HasSuffix(want, "'") {
			if b, err := hex.DecodeString(want[2 : len(want)-1]); err == nil && string(b) == string(got) {
				return nil
			}
		}
		return fail("expected %q (String), got bytes x'%s'", want, hex.EncodeToString(got))
	}
	return fail("expected %q (String), got %v (%T)", want, actual, actual)
}

// parseStartsWith decodes `xstartswith_<len>'<hex>'`, the corpus's way of
// asserting a byte-array prefix plus an exact total length.
func parseStartsWith(s string) ([]byte, int, error) {
	us := strings.Index(s, "_")
	q := strings.Index(s, "'")
	if us < 0 || q < 0 || q < us {
		return nil, 0, fmt.Errorf("malformed xstartswith_ literal %q", s)
	}
	length, err := strconv.Atoi(s[us+1 : q])
	if err != nil {
		return nil, 0, fmt.Errorf("malformed xstartswith_ length in %q", s)
	}
	b, err := hex.DecodeString(s[q+1 : len(s)-1])
	if err != nil {
		return nil, 0, fmt.Errorf("malformed xstartswith_ hex in %q", s)
	}
	return b, length, nil
}

// javaNumClass reconstructs the Java runtime class of an actual cell.
//
// The driver's declared column type is the authority where there is one:
// database/sql widens every integral driver.Value to int64, so the Go type
// alone cannot tell an INTEGER column from a BIGINT one, and that is exactly
// the distinction Matchers' Objects.equals is sensitive to.
func javaNumClass(actual any, sqlType string) string {
	switch strings.ToUpper(sqlType) {
	case "INTEGER":
		return "Integer"
	case "BIGINT":
		return "Long"
	case "FLOAT":
		return "Float"
	case "DOUBLE":
		return "Double"
	}
	switch actual.(type) {
	case int32:
		return "Integer"
	case int64, int:
		return "Long"
	case float32:
		return "Float"
	case float64:
		return "Double"
	}
	return ""
}

func actualInt(actual any) (int64, bool) {
	switch v := actual.(type) {
	case int64:
		return v, true
	case int32:
		return int64(v), true
	case int:
		return int64(v), true
	}
	return 0, false
}

func actualFloat(actual any) (float64, bool) {
	switch v := actual.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	}
	return 0, false
}

func actualString(actual any) (string, bool) {
	switch v := actual.(type) {
	case string:
		return v, true
	case []byte:
		return string(v), true
	}
	return "", false
}

func lengthOf(actual any) int {
	if s, ok := actualString(actual); ok {
		return len(s)
	}
	return -1
}

// matchInteger mirrors Matchers.matchIntField: an Integer expectation matches
// an Integer or a Long actual.
func matchInteger(want int64, actual any, sqlType string, fail func(string, ...any) error) error {
	got, ok := actualInt(actual)
	if ok {
		switch javaNumClass(actual, sqlType) {
		case "Integer", "Long":
			if got == want {
				return nil
			}
		}
	}
	return fail("expected %d (Integer), got %v (%s)", want, actual, describe(actual, sqlType))
}

// matchLong mirrors the Long branch's Objects.equals: no demotion to Integer.
func matchLong(want int64, actual any, sqlType string, fail func(string, ...any) error) error {
	got, ok := actualInt(actual)
	if ok && javaNumClass(actual, sqlType) == "Long" && got == want {
		return nil
	}
	return fail("expected %d (Long), got %v (%s)", want, actual, describe(actual, sqlType))
}

// matchFloat mirrors Matchers.matchFloatField: a Float expectation matches a
// Float actual via Objects.equals and a Double actual via
// Float.doubleValue()'s boxed comparison. Both arms are BIT equality
// (Float.equals is floatToIntBits, Double.equals is doubleToLongBits), which
// is not `==`: NaN equals NaN and +0.0 does NOT equal -0.0 — and the signed
// zero is the case this repo has already shipped a wrong-rows bug on.
func matchFloat(want float32, actual any, sqlType string, fail func(string, ...any) error) error {
	got, ok := actualFloat(actual)
	if ok {
		switch javaNumClass(actual, sqlType) {
		case "Float":
			if math.Float32bits(float32(got)) == math.Float32bits(want) {
				return nil
			}
		case "Double":
			if math.Float64bits(got) == math.Float64bits(float64(want)) {
				return nil
			}
		}
	}
	return fail("expected %v (Float), got %v (%s)", want, actual, describe(actual, sqlType))
}

// matchDouble mirrors the plain-YAML-float expectation: Objects.equals on a
// boxed Double, i.e. doubleToLongBits equality (see matchFloat), with no
// promotion from a Float actual.
func matchDouble(want float64, actual any, sqlType string, fail func(string, ...any) error) error {
	got, ok := actualFloat(actual)
	if ok && javaNumClass(actual, sqlType) == "Double" &&
		math.Float64bits(got) == math.Float64bits(want) {
		return nil
	}
	return fail("expected %v (Double), got %v (%s)", want, actual, describe(actual, sqlType))
}

func describe(actual any, sqlType string) string {
	cls := javaNumClass(actual, sqlType)
	if cls == "" {
		return fmt.Sprintf("%T", actual)
	}
	return cls
}

func expectedLong(v *javayamsql.Value) (int64, error) {
	if n, ok := v.AsInt(); ok {
		return n, nil
	}
	s, ok := v.AsString()
	if !ok {
		return 0, fmt.Errorf("!l expects a scalar")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("!l value %q is not a long", s)
	}
	return n, nil
}

func expectedFloat(v *javayamsql.Value) (float32, error) {
	u := v.Untag()
	switch u.Kind {
	case javayamsql.KindFloat:
		return float32(u.Float), nil
	case javayamsql.KindInt:
		return float32(u.Int), nil
	}
	s, ok := v.AsString()
	if !ok {
		return 0, fmt.Errorf("!f expects a scalar")
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 32)
	if err != nil {
		return 0, fmt.Errorf("!f value %q is not a float", s)
	}
	return float32(f), nil
}

// decodeBytesTag decodes a `!b x'...'` value. BytesTag asserts the x'...'
// shape while CONSTRUCTING the node, so a malformed one is an error before any
// query runs.
func decodeBytesTag(v *javayamsql.Value) ([]byte, error) {
	s, ok := v.AsString()
	if !ok {
		return nil, fmt.Errorf("!b expects a scalar")
	}
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(strings.ToLower(s), "x'") || !strings.HasSuffix(s, "'") {
		return nil, fmt.Errorf("the value of the bytes (!b) tag must be in x'...' hex format, however %q is found", s)
	}
	b, err := hex.DecodeString(s[2 : len(s)-1])
	if err != nil {
		return nil, fmt.Errorf("!b value %q is not valid hex: %w", s, err)
	}
	return b, nil
}

// expandRandomStr resolves a `!randomStr seed <N> length <M>` descriptor.
func expandRandomStr(desc string) (string, error) {
	fields := strings.Fields(strings.TrimSpace(desc))
	if len(fields) != 4 || !strings.EqualFold(fields[0], "seed") || !strings.EqualFold(fields[2], "length") {
		return "", fmt.Errorf("invalid !randomStr descriptor %q, expected \"seed <N> length <M>\"", desc)
	}
	seed, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return "", fmt.Errorf("invalid !randomStr seed in %q", desc)
	}
	length, err := strconv.Atoi(fields[3])
	if err != nil {
		return "", fmt.Errorf("invalid !randomStr length in %q", desc)
	}
	return randomString(seed, length), nil
}

func renderRow(rs *resultSet, row []any) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, v := range row {
		if i > 0 {
			b.WriteString(", ")
		}
		name := ""
		if i < len(rs.Cols) {
			name = rs.Cols[i]
		}
		switch x := v.(type) {
		case nil:
			fmt.Fprintf(&b, "%s: <NULL>", name)
		case []byte:
			fmt.Fprintf(&b, "%s: x'%s'", name, hex.EncodeToString(x))
		default:
			fmt.Fprintf(&b, "%s: %v", name, x)
		}
	}
	b.WriteByte('}')
	return b.String()
}
