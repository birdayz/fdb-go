package cascades

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// WHERE ProjectionElimRule WENT. It eliminated a LogicalProjection whose list
// was exactly the inner quantifier's flowed object — `SELECT *`-shaped
// identities — by yielding the inner expression into the PROJECTION's own memo
// reference. That yield is what RFC-226 §4.4(c) books as owed: two
// differently-shaped plans become co-members of one equivalence class, and the
// reference's declared row is then one of them arbitrarily.
//
// It became unreachable for the shape it was written for and reachable only for
// the shape it must not touch. Unreachable, because the identity projection it
// matched can no longer be CONSTRUCTED — the derivation refuses it
// (ErrWholeRowProjection), which the two admission pins below hold. Reachable
// wrongly, because its record-typed guard admitted `SELECT x FROM t, t.items AS
// x`: x is a STRUCT element, so the lone projected QOV is record-typed, but the
// projection emits RECORD<X: SITEM> over an inner row of SITEM — one projected
// column, not an identity. Eliminating it yielded the 4-column merged row into
// a reference declaring one column, and the query stopped planning:
//
//	member RELATION result type RELATION<RECORD<ID, ITEMS, NAME, X>> disagrees
//	with its Reference type RELATION<RECORD<X RECORD<SKU, QTY>>>
//
// So the rule is gone rather than re-guarded: what it existed to simplify is
// unbuildable, and the memo would prefer the cheaper member anyway (Java has no
// such rule — it relies on exactly that). The admission refusal it depended on
// is what this file now pins, because deleting the rule must not delete the
// evidence that its input is impossible.

func projectionElimRowType() *values.RecordType {
	return values.NewRecordType("ProjectionElimRow", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
	})
}

func mustProjectionElimConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct projection-elim fixture: " + err.Error())
	}
	return value
}

func projectionElimScanQ() (*expressions.FullUnorderedScanExpression, expressions.Quantifier) {
	scan := mustProjectionElimConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"T"}, projectionElimRowType()))
	return scan, expressions.ForEachQuantifier(expressions.InitialOf(scan))
}

func TestWholeRowIdentityProjectionIsRejectedAtAdmission(t *testing.T) {
	t.Parallel()
	_, q := projectionElimScanQ()
	root := mustProjectionElimConstruct(q.RequireFlowedObjectValue())
	p, err := expressions.NewLogicalProjectionExpression([]values.Value{root}, q)
	if !errors.Is(err, values.ErrWholeRowProjection) || p != nil {
		t.Fatalf("whole-row identity projection = %T, %v; want ErrWholeRowProjection", p, err)
	}
}

func TestWholeRowIdentityProjectionWithAnExplicitEmptyAliasIsRejectedToo(t *testing.T) {
	t.Parallel()
	_, q := projectionElimScanQ()
	root := mustProjectionElimConstruct(q.RequireFlowedObjectValue())
	p, err := expressions.NewLogicalProjectionExpressionWithAliases(
		[]values.Value{root},
		[]string{""},
		q,
	)
	if !errors.Is(err, values.ErrWholeRowProjection) || p != nil {
		t.Fatalf("empty-alias whole-row projection = %T, %v; want ErrWholeRowProjection", p, err)
	}
}

// TestBareQOVOverANamedSourceIsAColumnNotAWholeRowWrap is the other half of the
// admission rule, and it is what the deleted rule's record-typed guard got
// wrong. A machinery quantifier (`_current`, a minted `q$N`) names a row the
// SQL never named, so wrapping it has nothing to call its field; a NAMED
// correlation is a source the user wrote, and projecting it whole is
// `SELECT x` — with x as the column name — whatever the element's type.
func TestBareQOVOverANamedSourceIsAColumnNotAWholeRowWrap(t *testing.T) {
	t.Parallel()
	element := values.NewRecordType("SITEM", false, []values.Field{
		{Name: "SKU", FieldType: values.NullableString},
		{Name: "QTY", FieldType: values.NullableLong},
	})
	named := mustProjectionElimConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("X"), element))
	row, err := values.ProjectionResultValue([]values.Value{named}, nil)
	if err != nil {
		t.Fatalf("a struct element projected as one column must build, got: %v", err)
	}
	if len(row.Fields) != 1 || row.Fields[0].Name != "X" {
		t.Fatalf("row = %v, want one field named X", row.Fields)
	}
}
