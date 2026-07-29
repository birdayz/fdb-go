package cascades

// The windowed (rank / leaderboard) candidate answers the covering question the
// same way the value-index candidate does — in domain-checked ORDINALS, never
// by leaf name (RFC-197 item 1). Its covered set is the index's own column
// names plus the primary key, so the shapes that must be distinguished are the
// same: a reference into the record layout pushes, a reference carrying the
// same NAME from another layout does not.
//
// This candidate has no production constructor yet (the rank-index planning
// path does not build one), so the coverage is necessarily white-box. That is
// stated rather than hidden: an end-to-end scenario cannot exist until a caller
// does, and the migration must still be pinned against silent re-nameification.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestWindowedCandidate_CoveredPushIsOrdinalAndDomainChecked(t *testing.T) {
	t.Parallel()

	rowType := testRecordRowType("LEADER", "ID", "SCORE", "TEAM")
	src := values.NamedCorrelationIdentifier("SRC")
	tgt := values.NamedCorrelationIdentifier("TGT")
	c := NewWindowedIndexScanMatchCandidate(
		"leader$score",
		[]string{"LEADER"},
		[]string{"SCORE"},
		[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()},
		values.UniqueCorrelationIdentifier(),
		values.UniqueCorrelationIdentifier(),
		[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()},
		nil,
		rowType,
		false,
		[]string{"ID"},
	)

	// A covered column of the record layout pushes, and keeps its ordinal: a
	// reference that arrives below the fetch as a lazy name read is a loud
	// failure at evaluation, not a soft one.
	covered := testColumnRef(values.NewQuantifiedObjectValue(src), rowType, "SCORE", values.UnknownType)
	out, ok := c.PushValueThroughFetch(covered, src, tgt)
	if !ok {
		t.Fatal("covered column SCORE must push through the rank-index fetch")
	}
	fv, isFV := out.(*values.FieldValue)
	if !isFV || fv.Resolved == nil {
		t.Fatalf("pushed reference must stay BAKED, got %#v", out)
	}
	if got := fv.Resolved.Root().Ordinal; got != 1 {
		t.Fatalf("pushed ordinal = %d, want 1 (SCORE's slot)", got)
	}
	if _, stated := fv.OrdinalIn(values.OrdinalDomainOfType(rowType)); !stated {
		t.Fatal("pushed reference must state the layout its ordinal indexes")
	}

	// A NON-covered column of the same layout declines.
	if out, ok := c.PushValueThroughFetch(
		testColumnRef(values.NewQuantifiedObjectValue(src), rowType, "TEAM", values.UnknownType),
		src, tgt,
	); ok {
		t.Fatalf("uncovered column TEAM must not push, got %#v", out)
	}

	// The same NAME at the same ORDINAL in a DIFFERENT layout declines: the
	// integer alone is not identity, which is the failure mode a name check
	// cannot even express.
	foreign := testRecordRowType("OTHER", "X", "SCORE")
	if out, ok := c.PushValueThroughFetch(
		testColumnRef(values.NewQuantifiedObjectValue(src), foreign, "SCORE", values.UnknownType),
		src, tgt,
	); ok {
		t.Fatalf("SCORE@1 of a foreign layout must not be read as the record's SCORE, got %#v", out)
	}

	// A LAZY reference carrying only the covered display name declines.
	if out, ok := c.PushValueThroughFetch(
		values.NewFieldValue(values.NewQuantifiedObjectValue(src), "SCORE", values.UnknownType),
		src, tgt,
	); ok {
		t.Fatalf("a LAZY reference must not push on the strength of its name, got %#v", out)
	}
}

// A candidate whose row layout is UNKNOWN — what a multi-record-type index
// degrades to when no per-type descriptors are available — can prove nothing
// about any ordinal, so it declines every push rather than falling back to the
// index definition's column names.
func TestCoveredOrdinalSets_UnknownLayoutFailsClosed(t *testing.T) {
	t.Parallel()

	covered := map[string]struct{}{"A": {}}
	if sets := buildCoveredOrdinalSets([]values.Type{values.UnknownType}, covered); len(sets) != 0 {
		t.Fatalf("an UnknownType layout yielded %d ordinal set(s); it names no layout", len(sets))
	}
	if sets := buildCoveredOrdinalSets(nil, covered); len(sets) != 0 {
		t.Fatalf("no layouts yielded %d ordinal set(s)", len(sets))
	}

	// With per-record-type layouts, each type gets its OWN set and a value is
	// answered by the set for the layout it states — the multi-type case the
	// single flowed type cannot express. A and B place column X at different
	// ordinals precisely so a single merged set would be provably wrong.
	typeA := testRecordRowType("A", "X", "Y")
	typeB := testRecordRowType("B", "Y", "X")
	sets := buildCoveredOrdinalSets([]values.Type{typeA, typeB}, map[string]struct{}{"X": {}})
	if len(sets) != 2 {
		t.Fatalf("per-record-type sets = %d, want 2", len(sets))
	}
	inA := values.NewFieldValueWithResolvedOrdinalInDomain("X", 0, values.UnknownType, values.OrdinalDomainOfType(typeA))
	inB := values.NewFieldValueWithResolvedOrdinalInDomain("X", 1, values.UnknownType, values.OrdinalDomainOfType(typeB))
	if ord, _, ok := pushCoveredOrdinal(sets, inA); !ok || ord != 0 {
		t.Fatalf("A.X = (%d,%v), want (0,true)", ord, ok)
	}
	if ord, _, ok := pushCoveredOrdinal(sets, inB); !ok || ord != 1 {
		t.Fatalf("B.X = (%d,%v), want (1,true)", ord, ok)
	}
	// A's ordinal 1 is Y, which is not covered — and the answer must be a
	// definite NO, not "try the other type's set", which would let B's X
	// answer for A's Y.
	notCovered := values.NewFieldValueWithResolvedOrdinalInDomain("Y", 1, values.UnknownType, values.OrdinalDomainOfType(typeA))
	if ord, _, ok := pushCoveredOrdinal(sets, notCovered); ok {
		t.Fatalf("A.Y answered %d — an uncovered ordinal must not be rescued by another type's set", ord)
	}
}
