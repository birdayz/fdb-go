package executor

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// qovField builds a FieldValue qualified via a QuantifiedObjectValue child —
// the "QOV(alias).col" shape that re-enumerated multi-way join predicates use
// (vs the legacy flat "ALIAS.COL" Field string).
func qovField(alias, col string) *values.FieldValue {
	return values.NewFieldValue(
		values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(alias)),
		col, values.UnknownType,
	)
}

// TestExtractEquijoinColumns_QOVChildQualified pins the fix for the NLJ
// hash-join column extraction. The equijoin `t2.t1_id = t1.id` (inner=T2,
// outer=T1) must map to outerCol="ID", innerCol="T1_ID" regardless of which
// side of the predicate carries the inner vs outer alias.
//
// Before the fix, fieldName() returned only the bare Field ("T1_ID"/"ID") for
// a QOV-child FieldValue, so splitQualified() saw an empty table qualifier,
// matchesAlias("", x) was always true, and the FIRST classification branch
// matched unconditionally — picking outer/inner BACKWARDS. The hash index was
// then keyed on the wrong column, the outer lookup missed, and the join
// returned 0 rows (only on the ≥100-row hash-join path, making the bug
// data-dependent). This is the exact failure that blocked re-enumerated
// index-nested-loop joins from returning rows (RFC-042 L3).
func TestExtractEquijoinColumns_QOVChildQualified(t *testing.T) {
	t.Parallel()

	// p: t2.t1_id = t1.id  (inner side on the LHS, outer side on the RHS)
	p := predicates.NewComparisonPredicate(
		qovField("T2", "T1_ID"),
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: qovField("T1", "ID")},
	)

	outerCol, innerCol := extractEquijoinColumns(
		[]predicates.QueryPredicate{p}, "T1", "T2")

	if outerCol != "ID" {
		t.Errorf("outerCol = %q, want %q", outerCol, "ID")
	}
	if innerCol != "T1_ID" {
		t.Errorf("innerCol = %q, want %q", innerCol, "T1_ID")
	}
}

// TestExtractEquijoinColumns_QOVChild_OuterOnLHS is the mirror: outer alias on
// the LHS, inner on the RHS. Must yield the same (outerCol, innerCol) mapping.
func TestExtractEquijoinColumns_QOVChild_OuterOnLHS(t *testing.T) {
	t.Parallel()

	// p: t1.id = t2.t1_id
	p := predicates.NewComparisonPredicate(
		qovField("T1", "ID"),
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: qovField("T2", "T1_ID")},
	)

	outerCol, innerCol := extractEquijoinColumns(
		[]predicates.QueryPredicate{p}, "T1", "T2")

	if outerCol != "ID" || innerCol != "T1_ID" {
		t.Errorf("got (outerCol=%q, innerCol=%q), want (ID, T1_ID)", outerCol, innerCol)
	}
}

// TestExtractEquijoinColumns_FlatQualified verifies the legacy flat
// "ALIAS.COL" Field form is unaffected by the fix.
func TestExtractEquijoinColumns_FlatQualified(t *testing.T) {
	t.Parallel()

	p := predicates.NewComparisonPredicate(
		values.NewFlatFieldValue("T2.T1_ID", values.UnknownType),
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.NewFlatFieldValue("T1.ID", values.UnknownType)},
	)

	outerCol, innerCol := extractEquijoinColumns(
		[]predicates.QueryPredicate{p}, "T1", "T2")

	if outerCol != "ID" || innerCol != "T1_ID" {
		t.Errorf("got (outerCol=%q, innerCol=%q), want (ID, T1_ID)", outerCol, innerCol)
	}
}
