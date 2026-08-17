package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestFlowedObjectTypeMemoSeesAMemberChange drives the sequence a memo can only
// be wrong in: READ, then MUTATE, then RE-READ.
//
// GetFlowedObjectType caches its answer on the Reference, keyed on
// memberVersion. Every path that appends to a member slice bumps that counter,
// so the key is sound — but "every path" is a claim about the whole file, and a
// counter-keyed cache fails silently when it stops being true: the stale answer
// is a well-formed type of the right shape, so nothing downstream reports an
// error. It flows the wrong row.
//
// A test that reads once cannot see any of that. This one establishes the first
// answer, changes the member set underneath it, and requires the second answer
// to differ — so a missing bump reddens here rather than surfacing later as a
// column resolving into the wrong leg.
//
// The mutation is an INSERT of a member with a different leg table, because
// that is the change the memo is most likely to miss: RecordType.Equals ignores
// Legs, so a stale row and a fresh one compare EQUAL on every channel except
// the one being asserted. Comparing the two answers to each other would
// therefore be vacuous; the leg COUNT is asserted directly.
func TestFlowedObjectTypeMemoSeesAMemberChange(t *testing.T) {
	t.Parallel()

	row := func() *values.RecordType {
		return &values.RecordType{Fields: []values.Field{
			{Name: "K", Ordinal: 0, FieldType: values.NotNullLong},
			{Name: "K", Ordinal: 1, FieldType: values.NotNullLong},
		}}
	}
	flat := &legSeedStubExpr{name: "flat", typ: row(), tile: false}
	tiling := &legSeedStubExpr{name: "tiling", typ: row(), tile: true}

	ref := InitialOf(flat)
	q := NamedForEachQuantifier(values.NamedCorrelationIdentifier("Q"), ref)

	// READ. The only member states no boundaries, so the flowed row has none.
	first, err := q.GetFlowedObjectType()
	if err != nil {
		t.Fatalf("first GetFlowedObjectType: %v", err)
	}
	firstLegs := len(first.(*values.RecordType).Legs)
	if firstLegs != 0 {
		t.Fatalf("fixture's first answer already states %d legs, want 0 — the mutation below "+
			"could then not change anything and this test would pass vacuously", firstLegs)
	}

	// MUTATE. A second member that DOES state boundaries; populated-wins means
	// the correct answer is now a 2-leg row.
	if !ref.Insert(tiling) {
		t.Fatal("fixture failed to retain the second member; nothing was mutated")
	}

	// RE-READ. A memo that did not see the insert returns the 0-leg row, and it
	// is a perfectly well-formed type — which is exactly why this has to be
	// asserted as a VALUE and not as "the two answers differ".
	second, err := q.GetFlowedObjectType()
	if err != nil {
		t.Fatalf("second GetFlowedObjectType: %v", err)
	}
	if got := len(second.(*values.RecordType).Legs); got != 2 {
		t.Fatalf("after inserting a member that states leg boundaries the flowed row states %d "+
			"legs, want 2 — the cached answer survived a member change. Its key is "+
			"Reference.memberVersion; some path that appends a member is not bumping it. A row "+
			"that has forgotten its boundaries does not read downstream as \"no legs\" but as ONE "+
			"run spanning the whole concat, so an alias-qualified read of the SECOND leg lands in "+
			"the first", got)
	}
}
