package embedded

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query/logical"
)

func nestedGroupKey(t testing.TB, correlation, leaf string, ordinal int) values.Value {
	t.Helper()
	nestedType := &values.RecordType{Fields: []values.Field{
		{Name: "SK", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "CO", Ordinal: 1, FieldType: values.NotNullLong},
		{Name: "OTHER", Ordinal: 2, FieldType: values.NotNullLong},
	}}
	rootType := &values.RecordType{Fields: []values.Field{{Name: "N", Ordinal: 0, FieldType: nestedType}}}
	qov, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(correlation), rootType)
	if err != nil {
		t.Fatalf("NewQuantifiedObjectValue: %v", err)
	}
	value, err := values.ResolveFieldOrdinals(qov, []int{0, ordinal})
	if err != nil {
		t.Fatalf("resolve N.%s: %v", leaf, err)
	}
	return value
}

func exactFlatGroupKey(t testing.TB, correlation, name string) values.Value {
	t.Helper()
	typ := &values.RecordType{Fields: []values.Field{{Name: name, Ordinal: 0, FieldType: values.NotNullString}}}
	qov, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(correlation), typ)
	if err != nil {
		t.Fatalf("NewQuantifiedObjectValue: %v", err)
	}
	value, err := values.ResolveFieldOrdinals(qov, []int{0})
	if err != nil {
		t.Fatalf("resolve %s: %v", name, err)
	}
	return value
}

func TestAggregateGroupKeyMirrorsTakeTheExactNestedPath(t *testing.T) {
	t.Parallel()

	sk := nestedGroupKey(t, "T1", "SK", 0)
	co := nestedGroupKey(t, "T1", "CO", 1)

	t.Run("aggregateGroupKeyOutputName", func(t *testing.T) {
		t.Parallel()
		gotSK, gotCO := aggregateGroupKeyOutputName(sk), aggregateGroupKeyOutputName(co)
		if gotSK != "T1.N.SK" || gotCO != "T1.N.CO" {
			t.Fatalf("aggregateGroupKeyOutputName = %q / %q, want T1.N.SK / T1.N.CO", gotSK, gotCO)
		}
		if gotSK != expressions.AggregateKeyColumnName(sk) || gotCO != expressions.AggregateKeyColumnName(co) {
			t.Fatalf("group-key naming mirror drifted from authority: got %q/%q, authority %q/%q",
				gotSK, gotCO, expressions.AggregateKeyColumnName(sk), expressions.AggregateKeyColumnName(co))
		}
	})

	t.Run("flat exact key keeps its bare output name", func(t *testing.T) {
		t.Parallel()
		flat := exactFlatGroupKey(t, "T1", "STATUS")
		if got := aggregateGroupKeyOutputName(flat); got != "STATUS" {
			t.Fatalf("flat exact group key names its output %q, want STATUS", got)
		}
	})
}

func TestGroupKeyStripRetainsBoundIdentity(t *testing.T) {
	t.Parallel()
	value := nestedGroupKey(t, "A", "SK", 0)
	for _, key := range []logical.GroupKey{
		{Display: "A.N.SK", Bare: "SK", Qualifier: "A.N", Qualified: true, Segs: []string{"A", "N", "SK"}, Value: value},
		{Display: "A.N.SK", Bare: "SK", Qualifier: "A.N", Qualified: true, Value: value},
	} {
		stripped := stripGroupKeyLeadingSegments(key, "N.SK")
		if stripped.Value != value {
			t.Fatalf("prefix stripping lost the bound grouping value: %+v", stripped)
		}
		if stripped.Display != "N.SK" {
			t.Fatalf("stripped display = %q", stripped.Display)
		}
	}
}

// TestGroupKeyStripDecidesBySegments pins that the single-source strip reads the
// reference's segments. The caller tests the display TEXT for the source's name
// plus a dot, so one quoted identifier whose text begins that way
// ("foo.tableA.A2" under FROM "foo.tableA", valid-identifiers.yamsql) reaches the
// helper with a stripped text of "A2". Its one segment does not account for the
// strip, so it keeps its own name, as does a key of several segments whose tail
// is not the stripped text; a qualified reference still strips, and a key with
// no segments (an expression) keeps the text rebuild.
func TestGroupKeyStripDecidesBySegments(t *testing.T) {
	t.Parallel()
	value := exactFlatGroupKey(t, "T", "foo.tableA.A2")

	oneSegment := logical.GroupKey{Display: "foo.tableA.A2", Bare: "foo.tableA.A2", Segs: []string{"foo.tableA.A2"}, Value: value}
	if got := stripGroupKeyLeadingSegments(oneSegment, "A2"); got.Display != "foo.tableA.A2" || got.Bare != "foo.tableA.A2" ||
		got.Qualified || len(got.Segs) != 1 || got.Value != value {
		t.Fatalf("a single quoted identifier was stripped to %+v, want it unchanged", got)
	}

	qualified := logical.GroupKey{
		Display: "foo.tableA.A2", Bare: "A2", Qualifier: "foo.tableA", Qualified: true,
		Segs: []string{"foo.tableA", "A2"}, Value: value,
	}
	if got := stripGroupKeyLeadingSegments(qualified, "A2"); got.Display != "A2" || got.Bare != "A2" ||
		got.Qualified || got.Qualifier != "" || len(got.Segs) != 1 || got.Value != value {
		t.Fatalf("the qualified reference stripped to %+v, want bare A2", got)
	}

	// Several segments that do not account for the strip either: `"a.b".c`
	// under the alias A reads as the text A.B.C, whose strip leaves B.C, but
	// its qualifier is the one segment "a.b", not A. It keeps its own
	// segments rather than losing a qualifier it never had.
	quotedQualifier := logical.GroupKey{
		Display: "A.B.C", Bare: "C", Qualifier: "A.B", Qualified: true,
		Segs: []string{"A.B", "C"}, Value: value,
	}
	if got := stripGroupKeyLeadingSegments(quotedQualifier, "B.C"); got.Display != "A.B.C" || got.Bare != "C" ||
		got.Qualifier != "A.B" || !got.Qualified || len(got.Segs) != 2 || got.Value != value {
		t.Fatalf("a quoted qualifier's key stripped to %+v, want it unchanged", got)
	}

	expression := logical.GroupKey{Display: "T.A + 1", Value: value}
	if got := stripGroupKeyLeadingSegments(expression, "A + 1"); got.Display != "A + 1" || got.Bare != "A + 1" || got.Value != value {
		t.Fatalf("a key without segments stripped to %+v, want the text rebuild", got)
	}
}

func TestGroupAliasPreservesBareBoundStar(t *testing.T) {
	t.Parallel()
	q, err := parseQueryFromSelect(t, "SELECT * FROM t GROUP BY 2 AS id, 1")
	if err != nil {
		t.Fatal(err)
	}
	simple := q.QueryExpressionBody().(*antlrgen.QueryTermDefaultContext).QueryTerm().(*antlrgen.SimpleTableContext)
	a := nestedGroupKey(t, "SOURCE", "SK", 0)
	b := nestedGroupKey(t, "SOURCE", "CO", 1)
	cls, err := classifySelectElements(simple, func(string) ([]projCol, bool) {
		return []projCol{{name: "ID", bare: "ID", bound: a}, {name: "V", bare: "V", bound: b}}, true
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cls.aggCols) != 2 || !groupKeysPullUpEqual(cls.aggCols[0].groupColValue, a) || !groupKeysPullUpEqual(cls.aggCols[1].groupColValue, b) {
		t.Fatalf("GROUP alias replaced a bound bare star attribute: %+v", cls.aggCols)
	}
}
