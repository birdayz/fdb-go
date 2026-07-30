package expressions

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// rowOfTypes builds a row from explicit (name, type) pairs, so a fixture can
// state that two members resolved DIFFERENT AMOUNTS of the same row.
func rowOfTypes(pairs ...any) *values.RecordType {
	fields := make([]values.Field, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		fields = append(fields, values.Field{
			Name:      pairs[i].(string),
			FieldType: pairs[i+1].(values.Type),
			Ordinal:   len(fields),
		})
	}
	return &values.RecordType{Nullable: true, Fields: fields}
}

func flowedTypeOf(t *testing.T, members ...RelationalExpression) (*values.RecordType, error) {
	t.Helper()
	if len(members) == 0 {
		t.Fatal("fixture: no members")
	}
	ref := InitialOf(members[0])
	for _, m := range members[1:] {
		ref.Insert(m)
	}
	if len(ref.AllMembers()) != len(members) {
		t.Fatalf("fixture: reference holds %d members, want %d — the members interned "+
			"together and there is no disagreement left to resolve",
			len(ref.AllMembers()), len(members))
	}
	return NamedForEachQuantifier(values.NamedCorrelationIdentifier("Q"), ref).GetFlowedObjectType()
}

// TestGetFlowedObjectType_AnUnresolvedFieldIsNotADisagreement pins the one place
// Java's member-agreement reduction cannot be ported as literal equality.
//
// Java reduces the members' result types with `Verify.verify(left.equals(right))`
// (Reference.java:504-513) and that is sound there because every Java Value
// carries a resolved type: two members of one equivalence class either describe
// the same row or the memo is broken. Go has a third state — UnknownType, "not
// inferred yet" — and the SAME row reached by two different rules routinely
// arrives with different amounts of it resolved. Literal equality then reports a
// disagreement between a row and itself.
//
// That is not a cosmetic misreport. The callers treat a disagreement as a reason
// to STOP: PartitionSelectRule's Case-2 declines the bipartition outright, and a
// declined bipartition on a multi-EXISTS shape leaves a SelectExpression with no
// physical member at all — measured as `best expression is not a physical plan:
// *expressions.LogicalProjectionExpression` on
// `SELECT "X" FROM A, B, A."ARR" AS "X" WHERE EXISTS (…) AND EXISTS (…)`, where
// the two members differed in exactly one field: the unnest element, `X INT` down
// one path and `X UNKNOWN` down the other.
func TestGetFlowedObjectType_AnUnresolvedFieldIsNotADisagreement(t *testing.T) {
	t.Parallel()

	resolved := rowOfTypes("A", values.NotNullLong, "X", values.NotNullLong)
	unresolved := rowOfTypes("A", values.NotNullLong, "X", values.UnknownType)

	// BOTH ORDERS. The refinement must not depend on which member the scan reaches
	// first, or "the resolved row wins" degrades into "the first row wins" — memo
	// insertion order deciding a row shape, which is the exact choice this
	// verification exists to refuse.
	for _, tc := range []struct {
		name     string
		a, b     *values.RecordType
		wantSame *values.RecordType
	}{
		{"unresolved first", unresolved, resolved, resolved},
		{"resolved first", resolved, unresolved, resolved},
	} {
		got, err := flowedTypeOf(t,
			&typedStubExpr{name: "m1", typ: tc.a},
			&typedStubExpr{name: "m2", typ: tc.b})
		if err != nil {
			t.Errorf("%s: members differing only in an UNRESOLVED field reported a "+
				"disagreement: %v\n"+
				"  An unstated type states nothing, so it cannot contradict a stated one —\n"+
				"  the same rule the member scan already applies to a member carrying no row\n"+
				"  type at all. Callers treat the error as a STOP, so this misreport costs a\n"+
				"  plan, not a log line.", tc.name, err)
			continue
		}
		if got == nil || !got.Equals(tc.wantSame) {
			t.Errorf("%s: resolved to %v, want the more RESOLVED row %v — a later member "+
				"must not un-resolve what an earlier one established",
				tc.name, got, tc.wantSame)
		}
	}
}

// TestGetFlowedObjectType_StillCatchesRealDisagreements is the other direction,
// and it is the one that keeps the fix above from being "never report anything".
// A genuine disagreement is still a memo defect and must still surface: two
// members of one equivalence class flowing different rows means some caller is
// about to pick a row shape by insertion order.
func TestGetFlowedObjectType_StillCatchesRealDisagreements(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		a, b *values.RecordType
	}{
		{
			// Both STATE a type for X, and they state different ones.
			"conflicting stated field types",
			rowOfTypes("A", values.NotNullLong, "X", values.NotNullLong),
			rowOfTypes("A", values.NotNullLong, "X", values.NotNullString),
		},
		{
			"different field count",
			rowOfTypes("A", values.NotNullLong),
			rowOfTypes("A", values.NotNullLong, "B", values.UnknownType),
		},
		{
			"different field names",
			rowOfTypes("A", values.UnknownType),
			rowOfTypes("B", values.UnknownType),
		},
		{
			// A nested row that disagrees on a STATED leaf. Recursion must not
			// turn "descend into records" into "records always agree".
			"nested record with a conflicting stated leaf",
			rowOfTypes("A", rowOfTypes("N", values.NotNullLong)),
			rowOfTypes("A", rowOfTypes("N", values.NotNullString)),
		},
	} {
		got, err := flowedTypeOf(t,
			&typedStubExpr{name: "d1", typ: tc.a},
			&typedStubExpr{name: "d2", typ: tc.b})
		var de *MemberResultTypeDisagreementError
		if !errors.As(err, &de) {
			t.Errorf("%s: resolved to (%v, %v), want a disagreement — two members of one "+
				"equivalence class flowing different rows is a memo defect, and swallowing "+
				"it hands the next caller a row shape chosen by insertion order",
				tc.name, got, err)
		}
	}
}

// TestGetFlowedObjectType_RefinesNestedUnresolvedFields pins that the refinement
// reaches BELOW the top level. A row whose only difference is an unresolved field
// two levels down is the same row for exactly the reason it is one level down, and
// a top-level-only refinement would report a disagreement on it.
func TestGetFlowedObjectType_RefinesNestedUnresolvedFields(t *testing.T) {
	t.Parallel()

	nestedResolved := rowOfTypes("OUT", rowOfTypes("IN", values.NotNullLong))
	nestedUnresolved := rowOfTypes("OUT", rowOfTypes("IN", values.UnknownType))

	got, err := flowedTypeOf(t,
		&typedStubExpr{name: "n1", typ: nestedUnresolved},
		&typedStubExpr{name: "n2", typ: nestedResolved})
	if err != nil {
		t.Fatalf("members differing only in a NESTED unresolved field reported a "+
			"disagreement: %v", err)
	}
	if got == nil || !got.Equals(nestedResolved) {
		t.Errorf("nested refinement resolved %v, want %v", got, nestedResolved)
	}
}
