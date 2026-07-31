package javacorpus

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/relational/conformance/javayamsql"
)

// This file ports CheckResultMetadataConfig: the `resultMetadata:` directive
// asserts a query's result-set column NAMES and SQL TYPE NAMES, recursively for
// struct and array columns.
//
// The expected side is an arbitrarily nested YAML shape and is ported whole,
// because the shape is what the corpus writes and a partial reader would accept
// a file it cannot actually check. The actual side is bounded by what the Go
// driver surfaces, which today is one flat type name per column — so the
// descending forms are declined by NAME (SkipResultMetadataNested) rather than
// compared against a descriptor that was never populated. Comparing against an
// unpopulated descriptor is the failure mode this split exists to prevent: it
// would read as a Go/Java divergence when it is a metadata-pipeline truncation.

// columnDescriptor is CheckResultMetadataConfig.ColumnDescriptor.
//
// The Java fields `structTypeName` and `fields` are nullable and their
// NULL-ness is load-bearing in matchesExpected (a struct expectation against a
// column with `fields == null` is a mismatch, not an empty-field match), so
// each carries an explicit presence flag rather than relying on a zero value
// that a legitimately empty struct would also produce.
type columnDescriptor struct {
	// Name is the result-set column name.
	Name string
	// TypeName is the SQL type name: "BIGINT", "STRUCT", "ARRAY(INTEGER)", …
	TypeName string
	// StructTypeName is the DECLARED struct type name (`CREATE TYPE AS STRUCT
	// point(…)` → "point"). HasStructTypeName distinguishes "absent" from "".
	StructTypeName    string
	HasStructTypeName bool
	// Fields are the nested field descriptors of a struct column, or of an
	// array-of-struct column's ELEMENT struct. HasFields is Java's
	// `fields != null`.
	Fields    []columnDescriptor
	HasFields bool
	// IsArray marks an array whose element type is a struct. It is false for a
	// plain struct column and for every scalar / array-of-scalar column, which
	// is what lets `{PT: [...]}` and `{PTS: {array: [...]}}` be told apart.
	IsArray bool
}

// extractDescriptors is CheckResultMetadataConfig.extractDescriptors over what
// the Go driver actually hands back.
//
// Java walks StructMetaData recursively, branching on Types.STRUCT and
// Types.ARRAY into getStructMetaData / getArrayMetaData. Go has neither branch
// available: `database/sql` exposes ColumnTypeDatabaseTypeName, which the
// driver answers from `executor.ColumnDef.TypeName` — a single string per
// column with no field list, no element type and no declared type name. So
// every descriptor produced here is the scalar form, and the caller must have
// already declined any expectation that descends.
func extractDescriptors(rs *resultSet) []columnDescriptor {
	if rs == nil {
		return nil
	}
	out := make([]columnDescriptor, len(rs.Cols))
	for i, name := range rs.Cols {
		out[i] = columnDescriptor{Name: name, TypeName: typeAt(rs, i)}
	}
	return out
}

// metadataDescends reports whether an expected `resultMetadata:` value asks a
// question about a column's INTERIOR — a struct field list, an `{array: …}`
// map, or a struct type name.
//
// It is a property of the EXPECTATION, deliberately: the actual descriptor
// cannot answer, so the decision has to be made before any comparison. The
// second return names the first column that descends, so the ledger detail
// says which one rather than only that one existed.
func metadataDescends(raw *javayamsql.Value) (bool, string) {
	cols, err := expectedColumns(raw)
	if err != nil {
		// A shape this reader cannot decompose is not a descent decision; let
		// the comparison path report it with the full Java-shaped message.
		return false, ""
	}
	for _, c := range cols {
		if c.Val.Untag().Kind != javayamsql.KindString {
			return true, fmt.Sprintf("column %s expects %s metadata", c.Name, describeExpected(c.Val))
		}
	}
	return false, ""
}

func describeExpected(v *javayamsql.Value) string {
	switch v.Untag().Kind {
	case javayamsql.KindSeq:
		return "nested struct-field"
	case javayamsql.KindMap:
		return "array-element"
	default:
		return "non-scalar"
	}
}

// expectedColumn is one entry of the expected column list: the declared column
// name and the value describing its type.
type expectedColumn struct {
	Name string
	Val  *javayamsql.Value
}

// expectedColumns casts the directive's value the way Java does —
// `(List<Map<?, ?>>) rawExpectedValue`, then `entrySet().iterator().next()` per
// element, so only the FIRST entry of each mapping is read.
//
// A null value maps to Java's `List.of()`: the config carries no expectation
// and every non-empty actual list mismatches on size.
func expectedColumns(raw *javayamsql.Value) ([]expectedColumn, error) {
	if raw == nil || raw.IsNull() {
		return nil, nil
	}
	u := raw.Untag()
	if u.Kind != javayamsql.KindSeq {
		return nil, fmt.Errorf("resultMetadata expects a list of column descriptors, got %s", u.Kind)
	}
	out := make([]expectedColumn, 0, len(u.Seq))
	for _, e := range u.Seq {
		m := e.Untag()
		if m.Kind != javayamsql.KindMap || len(m.Map) == 0 {
			return nil, fmt.Errorf("resultMetadata column descriptor must be a non-empty mapping, got %s", m.Kind)
		}
		first := m.Map[0]
		name, ok := first.Key.AsString()
		if !ok {
			return nil, fmt.Errorf("resultMetadata column name must be a scalar, got %s", first.Key.Untag().Kind)
		}
		out = append(out, expectedColumn{Name: name, Val: first.Val})
	}
	return out, nil
}

// matchMetadata is CheckResultMetadataConfig.checkDescriptorsInternal.
//
// It returns nil on a match and the Java-shaped mismatch report otherwise. The
// `shouldCorrectResultMetadata` / `shouldAddResultMetadata` arms are absent by
// construction: they REWRITE the yamsql source, and the corpus is vendored
// verbatim, so the only reachable outcome here is compare-and-report.
func matchMetadata(raw *javayamsql.Value, actual []columnDescriptor) error {
	expected, err := expectedColumns(raw)
	if err != nil {
		return err
	}

	// Java skips with a warning when the server returned no column metadata,
	// because a multi-server run may include a version that omits it. Go is not
	// an older server — a SELECT whose plan yields no column descriptors is a
	// derivation bug, and swallowing it here would turn a real defect into a
	// silently unchecked directive.
	if len(actual) == 0 && len(expected) != 0 {
		return fmt.Errorf("result metadata mismatch: the driver reported NO columns for a query "+
			"expecting %d — a SELECT always has at least one column descriptor, so this is a "+
			"column-derivation gap, not a metadata mismatch", len(expected))
	}

	if matchesExpectedMetadata(expected, actual) {
		return nil
	}
	return reportMetadataMismatch(expected, actual)
}

// matchesExpectedMetadata is Java's matchesExpected, branch for branch.
func matchesExpectedMetadata(expected []expectedColumn, actual []columnDescriptor) bool {
	if len(expected) != len(actual) {
		return false
	}
	for i, exp := range expected {
		act := actual[i]
		if !strings.EqualFold(exp.Name, act.Name) {
			return false
		}
		val := exp.Val.Untag()
		switch val.Kind {
		case javayamsql.KindSeq:
			// A plain struct column: [{field: type}, …] optionally led by the
			// struct type name.
			if !act.HasFields || act.IsArray {
				return false
			}
			tail, ok := checkStructTypeName(val.Seq, act)
			if !ok {
				return false
			}
			fields, err := expectedColumns(&javayamsql.Value{Kind: javayamsql.KindSeq, Seq: tail})
			if err != nil || !matchesExpectedMetadata(fields, act.Fields) {
				return false
			}
		case javayamsql.KindMap:
			// {array: VALUE} — an array column, of struct or of scalar.
			arrayValue, ok := getArrayValue(val)
			if !ok {
				// Java's getArrayValue returns null for a map with no `array`
				// key; buildExpectedArrayTypeName then yields "ARRAY(null)",
				// which no actual type name equals.
				return false
			}
			if av := arrayValue.Untag(); av.Kind == javayamsql.KindSeq {
				if !act.HasFields || !act.IsArray {
					return false
				}
				tail, ok := checkStructTypeName(av.Seq, act)
				if !ok {
					return false
				}
				fields, err := expectedColumns(&javayamsql.Value{Kind: javayamsql.KindSeq, Seq: tail})
				if err != nil || !matchesExpectedMetadata(fields, act.Fields) {
					return false
				}
				continue
			}
			if act.HasFields {
				return false
			}
			if !strings.EqualFold(buildExpectedArrayTypeName(val), act.TypeName) {
				return false
			}
		case javayamsql.KindString:
			if !strings.EqualFold(val.Str, act.TypeName) {
				return false
			}
		default:
			// Java: `entry.getValue() instanceof String` fails for a number,
			// boolean or null, and the column mismatches.
			return false
		}
	}
	return true
}

// checkStructTypeName is Java's checkStructTypeName: an optional leading STRING
// element of a field list names the declared struct type.
//
// It returns the remaining field descriptors and whether the name held. When no
// leading string is present the list is returned unchanged and the type name is
// not checked at all — omitting it is always valid.
func checkStructTypeName(list []*javayamsql.Value, act columnDescriptor) ([]*javayamsql.Value, bool) {
	if len(list) == 0 {
		return list, true
	}
	head := list[0].Untag()
	if head.Kind != javayamsql.KindString {
		return list, true
	}
	if !act.HasStructTypeName || !strings.EqualFold(head.Str, act.StructTypeName) {
		return nil, false
	}
	return list[1:], true
}

// getArrayValue is Java's case-insensitive lookup of the `array` key.
func getArrayValue(m *javayamsql.Value) (*javayamsql.Value, bool) {
	for _, e := range m.Map {
		if k, ok := e.Key.AsString(); ok && strings.EqualFold(k, "array") {
			return e.Val, true
		}
	}
	return nil, false
}

// buildExpectedArrayTypeName is Java's buildExpectedArrayTypeName:
// `{array: INTEGER}` → "ARRAY(INTEGER)", `{array: {array: INTEGER}}` →
// "ARRAY(ARRAY(INTEGER))", anything else → "ARRAY(null)".
func buildExpectedArrayTypeName(m *javayamsql.Value) string {
	v, ok := getArrayValue(m)
	if !ok {
		return "ARRAY(null)"
	}
	u := v.Untag()
	switch u.Kind {
	case javayamsql.KindString:
		return "ARRAY(" + u.Str + ")"
	case javayamsql.KindMap:
		return "ARRAY(" + buildExpectedArrayTypeName(u) + ")"
	default:
		return "ARRAY(null)"
	}
}

// reportMetadataMismatch renders Java's two-column report. The wording is
// shared with Java deliberately: a corpus failure should be diagnosable against
// the Java run that blessed the expectation.
func reportMetadataMismatch(expected []expectedColumn, actual []columnDescriptor) error {
	var b strings.Builder
	b.WriteString("result metadata mismatch:\n")
	fmt.Fprintf(&b, "expected columns (%d):\n", len(expected))
	for _, e := range expected {
		fmt.Fprintf(&b, "    %s: %s\n", e.Name, renderExpectedValue(e.Val))
	}
	fmt.Fprintf(&b, "actual columns (%d):\n", len(actual))
	for _, a := range actual {
		appendDescriptor(&b, a, "    ")
	}
	return fmt.Errorf("%s", strings.TrimRight(b.String(), "\n"))
}

func appendDescriptor(b *strings.Builder, d columnDescriptor, prefix string) {
	b.WriteString(prefix)
	b.WriteString(d.Name)
	b.WriteString(": ")
	b.WriteString(d.TypeName)
	if d.HasStructTypeName {
		b.WriteString("(")
		b.WriteString(d.StructTypeName)
		b.WriteString(")")
	}
	b.WriteString("\n")
	if d.HasFields {
		for _, f := range d.Fields {
			appendDescriptor(b, f, prefix+"    ")
		}
	}
}

// renderExpectedValue prints an expected type in the source's own shape, so the
// report echoes what the file says rather than a normalisation of it.
func renderExpectedValue(v *javayamsql.Value) string {
	u := v.Untag()
	switch u.Kind {
	case javayamsql.KindString:
		return u.Str
	case javayamsql.KindSeq:
		parts := make([]string, 0, len(u.Seq))
		for _, e := range u.Seq {
			parts = append(parts, renderExpectedElement(e))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case javayamsql.KindMap:
		parts := make([]string, 0, len(u.Map))
		for _, e := range u.Map {
			k, _ := e.Key.AsString()
			parts = append(parts, k+": "+renderExpectedValue(e.Val))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		s, _ := v.AsString()
		return s
	}
}

func renderExpectedElement(v *javayamsql.Value) string {
	u := v.Untag()
	if u.Kind == javayamsql.KindMap && len(u.Map) > 0 {
		k, _ := u.Map[0].Key.AsString()
		return "{" + k + ": " + renderExpectedValue(u.Map[0].Val) + "}"
	}
	return renderExpectedValue(v)
}
