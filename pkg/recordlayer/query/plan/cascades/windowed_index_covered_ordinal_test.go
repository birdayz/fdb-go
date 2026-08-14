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

// windowedFieldImpostor deliberately embeds the public read view. It satisfies
// values.Value, but values.AsFieldValue must reject it: only the values-owned
// immutable concrete node can carry a physical ordinal contract.
type windowedFieldImpostor struct {
	values.FieldValue
}

func windowedRowType(name string, columns ...string) *values.RecordType {
	fields := make([]values.Field, len(columns))
	for ordinal, column := range columns {
		fields[ordinal] = values.Field{Name: column, FieldType: values.NullableLong, Ordinal: ordinal}
	}
	return values.NewRecordType(name, false, fields)
}

func windowedQOV(
	t testing.TB,
	alias values.CorrelationIdentifier,
	typ values.Type,
) values.QuantifiedObjectValue {
	t.Helper()
	qov, err := values.NewQuantifiedObjectValue(alias, typ)
	if err != nil {
		t.Fatalf("construct windowed QOV: %v", err)
	}
	return qov
}

func windowedFieldAt(
	t testing.TB,
	alias values.CorrelationIdentifier,
	typ values.Type,
	ordinal int,
) values.FieldValue {
	t.Helper()
	resolved, err := values.ResolveFieldOrdinals(windowedQOV(t, alias, typ), []int{ordinal})
	if err != nil {
		t.Fatalf("resolve windowed field: %v", err)
	}
	field, ok := values.AsFieldValue(resolved)
	if !ok {
		t.Fatalf("resolved ordinal %d produced %T, want exact FieldValue", ordinal, resolved)
	}
	return field
}

func TestWindowedCandidate_CoveredPushIsOrdinalAndDomainChecked(t *testing.T) {
	t.Parallel()

	rowType := windowedRowType("LEADER", "ID", "SCORE", "TEAM")
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
	covered := windowedFieldAt(t, src, rowType, 1)
	out, ok := c.PushValueThroughFetch(covered, src, tgt)
	if !ok {
		t.Fatal("covered column SCORE must push through the rank-index fetch")
	}
	fv, isFV := values.AsFieldValue(out)
	if !isFV || fv.Path() == nil {
		t.Fatalf("pushed reference must stay BAKED, got %#v", out)
	}
	ordinals := fv.Path().Ordinals()
	if len(ordinals) != 1 || ordinals[0] != 1 {
		t.Fatalf("pushed ordinal path = %v, want [1] (SCORE's descriptor slot)", ordinals)
	}
	if identity, stated := values.CorrelatedFieldIdentityIn(fv, values.OrdinalDomainOfType(rowType)); !stated || identity.Ordinal != 1 {
		t.Fatal("pushed reference must state the layout its ordinal indexes")
	}

	// A NON-covered column of the same layout declines.
	if out, ok := c.PushValueThroughFetch(
		windowedFieldAt(t, src, rowType, 2),
		src, tgt,
	); ok {
		t.Fatalf("uncovered column TEAM must not push, got %#v", out)
	}

	// The same NAME at the same ORDINAL in a DIFFERENT layout declines: the
	// integer alone is not identity, which is the failure mode a name check
	// cannot even express.
	foreign := windowedRowType("OTHER", "X", "SCORE")
	if out, ok := c.PushValueThroughFetch(
		windowedFieldAt(t, src, foreign, 1),
		src, tgt,
	); ok {
		t.Fatalf("SCORE@1 of a foreign layout must not be read as the record's SCORE, got %#v", out)
	}

	// The public constructors cannot mint a lazy/name-only FieldValue. An
	// embedded read-view impostor carrying the exact covered field underneath is
	// therefore the mutation-sensitive control: accepting it would re-open a
	// structural-interface escape hatch around exact recognition.
	if out, ok := c.PushValueThroughFetch(
		&windowedFieldImpostor{FieldValue: covered},
		src, tgt,
	); ok {
		t.Fatalf("a foreign FieldValue view must not push on the strength of its display name, got %#v", out)
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
	typeA := windowedRowType("A", "X", "Y")
	typeB := windowedRowType("B", "Y", "X")
	sets := buildCoveredOrdinalSets([]values.Type{typeA, typeB}, map[string]struct{}{"X": {}})
	if len(sets) != 2 {
		t.Fatalf("per-record-type sets = %d, want 2", len(sets))
	}
	inA := windowedFieldAt(t, values.NamedCorrelationIdentifier("A"), typeA, 0)
	inB := windowedFieldAt(t, values.NamedCorrelationIdentifier("B"), typeB, 1)
	if ord, _, _, ok := pushCoveredOrdinalWithType(sets, inA); !ok || ord != 0 {
		t.Fatalf("A.X = (%d,%v), want (0,true)", ord, ok)
	}
	if ord, _, _, ok := pushCoveredOrdinalWithType(sets, inB); !ok || ord != 1 {
		t.Fatalf("B.X = (%d,%v), want (1,true)", ord, ok)
	}
	// A's ordinal 1 is Y, which is not covered — and the answer must be a
	// definite NO, not "try the other type's set", which would let B's X
	// answer for A's Y.
	notCovered := windowedFieldAt(t, values.NamedCorrelationIdentifier("A_Y"), typeA, 1)
	if ord, _, _, ok := pushCoveredOrdinalWithType(sets, notCovered); ok {
		t.Fatalf("A.Y answered %d — an uncovered ordinal must not be rescued by another type's set", ord)
	}
}

// The CORRELATION element on the rank candidate, on the axis only it can
// decide: two quantifiers over ONE table share a layout token, so the domain
// and ordinal checks pass for both references and the site would rebase the
// foreign one onto the fetch target — reading this row's index entry for
// another row's column. Same shape, same reasoning, same refusal as the
// value-index candidate.
func TestWindowedCandidate_ForeignQuantifierDoesNotPush(t *testing.T) {
	t.Parallel()

	rowType := windowedRowType("LEADER", "ID", "SCORE", "TEAM")
	q1 := values.NamedCorrelationIdentifier("q1")
	q2 := values.NamedCorrelationIdentifier("q2")
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

	foreign := windowedFieldAt(t, q2, rowType, 1)
	if out, ok := c.PushValueThroughFetch(foreign, q1, tgt); ok {
		t.Fatalf("another quantifier's SCORE must not push through q1's rank fetch — only the "+
			"correlation separates the two, got %#v", out)
	}
	// The same value pushes through its OWN quantifier's fetch, so the refusal
	// above is the pairing and not some other ground for declining.
	if _, ok := c.PushValueThroughFetch(foreign, q2, tgt); !ok {
		t.Fatal("q2's SCORE must push through q2's own fetch")
	}
}
