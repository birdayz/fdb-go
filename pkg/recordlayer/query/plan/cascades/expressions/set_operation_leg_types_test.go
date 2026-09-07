package expressions

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// A set operation's legs are aligned before the logical expression exists, so
// a leg that states a different row is a construction error and is refused at
// construction — not later, in an implementation rule's body, where the only
// visible symptom is a group with no realizable plan (RFC-242). Names compare
// exactly: a field spelled in another case is a different field, which is the
// dimension the planner fuzz found unprobed.

func legQuantifier(t *testing.T, alias string, row *values.RecordType) Quantifier {
	t.Helper()
	return NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(alias),
		InitialOf(&typedStubExpr{name: "src" + alias, typ: row}),
	)
}

func TestLogicalUnion_RejectsLegsWhoseNamesDifferOnlyInCase(t *testing.T) {
	t.Parallel()
	lower := legQuantifier(t, "L", rowOfTypes("id", values.NullableLong, "k", values.NullableLong))
	upper := legQuantifier(t, "U", rowOfTypes("ID", values.NullableLong, "K", values.NullableLong))

	_, err := NewLogicalUnionExpression([]Quantifier{lower, upper})
	if err == nil {
		t.Fatal("RECORD<id, k> and RECORD<ID, K> were accepted as one union's legs: names compared under a fold, not exactly")
	}
	if !strings.Contains(err.Error(), "input quantifier 0 type") || !strings.Contains(err.Error(), "disagrees with input quantifier 1") {
		t.Fatalf("rejection names the wrong thing: %v", err)
	}

	if _, err := NewLogicalIntersectionExpression([]Quantifier{lower, upper}, nil); err == nil {
		t.Fatal("an intersection accepted legs a union refuses; the set operations share one contract")
	}
	if _, err := NewRecursiveUnionExpression(lower, upper,
		values.NamedCorrelationIdentifier("scan"), values.NamedCorrelationIdentifier("insert"),
		TraversalAny); err == nil {
		t.Fatal("a recursive union accepted states a union refuses; the set operations share one contract")
	}
}

func TestLogicalUnion_ExistentialLegIsNotCompared(t *testing.T) {
	t.Parallel()
	// An existential quantifier flows no row, so it is neither the stated
	// result nor compared against it — the pre-existing exemption, pinned
	// beside the new check so the two cannot drift apart.
	existential := legQuantifier(t, "E", rowOfTypes("OTHER", values.NotNullString))
	existential.kind = QuantifierExistential
	flowing := legQuantifier(t, "R", rowOfTypes("id", values.NullableLong))

	union, err := NewLogicalUnionExpression([]Quantifier{existential, flowing})
	if err != nil {
		t.Fatalf("existential leg was compared as if it flowed a row: %v", err)
	}
	if !union.GetResultValue().Type().Equals(flowing.mustFlowedType(t)) {
		t.Fatalf("union states %s, want the flowing leg's row", union.GetResultValue().Type())
	}
}

func TestLogicalUnion_AcceptsLegsStatingTheSameRow(t *testing.T) {
	t.Parallel()
	// Positive control: two legs, two different sources, one row.
	left := legQuantifier(t, "L", rowOfTypes("id", values.NullableLong, "k", values.NullableLong))
	right := legQuantifier(t, "R", rowOfTypes("id", values.NullableLong, "k", values.NullableLong))
	if _, err := NewLogicalUnionExpression([]Quantifier{left, right}); err != nil {
		t.Fatalf("legs stating the same row were refused: %v", err)
	}
}

func (q Quantifier) mustFlowedType(t *testing.T) values.Type {
	t.Helper()
	flowed, err := q.GetFlowedObjectType()
	if err != nil {
		t.Fatalf("flowed type: %v", err)
	}
	return flowed
}
