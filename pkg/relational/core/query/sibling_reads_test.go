package query

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// A block over a derived source that reads the source's own leaf alias reads a
// quantifier bound only inside that source. The invariant check must see it,
// and after the rewrite the block must read the column through its own
// quantifier's row, leaving nothing for the check to report.
func TestBuriedReadsAreReportedAndReadThroughTheOwningQuantifier(t *testing.T) {
	t.Parallel()

	rowType := &values.RecordType{Fields: []values.Field{{Name: "ID", Ordinal: 0, FieldType: values.NullableLong}}}
	scan, err := expressions.NewFullUnorderedScanExpression([]string{"A"}, rowType)
	if err != nil {
		t.Fatal(err)
	}
	leaf := expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("A"), expressions.InitialOf(scan))
	leafRow, err := leaf.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	leafID, err := values.ResolveFieldOrdinals(leafRow, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	derived, err := expressions.NewSelectExpression(
		values.NewRecordConstructorValue(values.RecordConstructorField{Name: "ID", Value: leafID}),
		[]expressions.Quantifier{leaf}, nil)
	if err != nil {
		t.Fatal(err)
	}
	owner := expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("D"), expressions.InitialOf(derived))
	// The block reads A.ID although only D is its quantifier.
	block, err := expressions.NewSelectExpression(
		values.NewRecordConstructorValue(values.RecordConstructorField{Name: "ID", Value: leafID}),
		[]expressions.Quantifier{owner}, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := expressions.InitialOf(block)

	before := buriedReadViolations(root, nil)
	if len(before) != 1 || !strings.Contains(before[0], "reads A below D") {
		t.Fatalf("violations before the rewrite = %q, want the one read of A below D", before)
	}

	rewritten, err := readSiblingsThroughRows(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if after := buriedReadViolations(rewritten, nil); len(after) != 0 {
		t.Fatalf("violations after the rewrite = %q, want none", after)
	}
	if got := values.ExplainValue(rewritten.Get().GetResultValue()); got != "{ID: D.ID#0}" {
		t.Fatalf("rewritten result = %s, want the column read through D's row", got)
	}

	// When D is a derived-table body, A is out of the block's scope: the same
	// read names an enclosing query's A, and stays as written.
	bodies := scopeBodies{derived: true}
	if hidden := buriedReadViolations(root, bodies); len(hidden) != 0 {
		t.Fatalf("violations through a body = %q, want none: its sources are out of scope", hidden)
	}
	outer, err := readSiblingsThroughRows(root, bodies)
	if err != nil {
		t.Fatal(err)
	}
	if got := values.ExplainValue(outer.Get().GetResultValue()); got != "{ID: A.ID#0}" {
		t.Fatalf("result above a body = %s, want the outer read kept", got)
	}
}
