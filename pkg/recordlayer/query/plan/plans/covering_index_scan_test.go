package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// coveringTestIndexPlan builds an index scan over IDX(A) with the given
// comparison ranges — the inner half of a covering plan under test.
func coveringTestIndexPlan(name string, comps []*predicates.ComparisonRange, reverse bool) *RecordQueryIndexPlan {
	return NewRecordQueryIndexPlan(name, comps, []string{"T"}, values.UnknownType, reverse).
		WithIndexMetadata([]string{"A"}, []string{"ID"}, false)
}

// TestCoveringPlan_InnerIsAFieldNotAChild pins RFC-220's first acceptance
// criterion: the wrapped index plan is a plain field, invisible to generic
// child traversal.
//
// This is not cosmetic. Two things ride on the inner staying invisible:
//
//   - Memo soundness. If the inner were reachable as a child quantifier, rules
//     matching a bare index scan would yield into the inner's Reference a group
//     member emitting FULL records, while the covering plan emits PARTIAL
//     records. They are not interchangeable and must not share a group.
//   - Cost classification. The cost model's census counts index scans it can
//     reach. Reaching the inner restores indexScanCount == 1 on a plan that
//     performs no fetch, which isSingularIndexScanWithFetch reads as a singular
//     index scan WITH a fetch — routing a fetchless covering scan to the
//     contested tier.
//
// If this test goes red because GetChildren started reporting the inner, the
// fix is to restore the field, NOT to update the expectation.
func TestCoveringPlan_InnerIsAFieldNotAChild(t *testing.T) {
	t.Parallel()

	inner := coveringTestIndexPlan("IDX_A", nil, false)
	cov := NewRecordQueryCoveringIndexPlan(inner, []string{"A", "ID"})

	if got := cov.GetChildren(); len(got) != 0 {
		t.Fatalf("GetChildren() returned %d children, want 0 — the inner index "+
			"plan must be a FIELD, not a child. Exposing it as a child re-arms "+
			"both the unsound-group and the cost-misclassification failures "+
			"described in RFC-220 §4.1.", len(got))
	}

	if got := cov.WithQuantifiers(nil); got != cov {
		t.Fatalf("WithQuantifiers(nil) returned a different expression; the "+
			"covering plan has no quantifiers to replace, so it must return "+
			"itself unchanged (got %T)", got)
	}

	// The inner is still reachable by NAME — invisible to traversal is not the
	// same as unreachable, and the executor needs it.
	if cov.GetIndexPlan() != inner {
		t.Fatal("GetIndexPlan() did not return the wrapped index plan")
	}
}

// TestCoveringPlan_StructuralKeyFoldsInnerScanRange pins RFC-220's second
// acceptance criterion, on the memo side.
//
// Because the inner is a field (see the test above), NOTHING in generic child
// traversal folds it into this node's identity. A structural key that folded
// only the covering columns would give two covering scans over the SAME index
// with DIFFERENT scan ranges the same key — collapsing them into a single memo
// Reference from which extraction can materialize the wrong-range scan.
//
// This is the quiet direction of the design: it produces wrong ROWS, not a
// build break or a loud error.
func TestCoveringPlan_StructuralKeyFoldsInnerScanRange(t *testing.T) {
	t.Parallel()

	sameCols := []string{"A", "ID"}

	eq5 := NewRecordQueryCoveringIndexPlan(
		coveringTestIndexPlan("IDX_A", []*predicates.ComparisonRange{pkOrderingEq(t, int64(5))}, false),
		sameCols,
	)
	eq7 := NewRecordQueryCoveringIndexPlan(
		coveringTestIndexPlan("IDX_A", []*predicates.ComparisonRange{pkOrderingEq(t, int64(7))}, false),
		sameCols,
	)

	if eq5.EqualsPlanWithoutChildren(eq7) {
		t.Fatal("covering scans over IDX_A [=5] and IDX_A [=7] compared EQUAL. " +
			"The inner is a field, so its identity is folded by structuralKey or " +
			"by nothing at all. Equal here means the memo collapses them into one " +
			"Reference and extraction can materialize the wrong-comparand scan.")
	}
	if eq5.HashCodeWithoutChildren() == eq7.HashCodeWithoutChildren() {
		t.Fatal("covering scans over IDX_A [=5] and IDX_A [=7] hashed EQUAL; " +
			"structuralKey is not folding the inner's scan comparands")
	}

	// Index name is the other half of the inner's identity.
	otherIndex := NewRecordQueryCoveringIndexPlan(
		coveringTestIndexPlan("IDX_B", []*predicates.ComparisonRange{pkOrderingEq(t, int64(5))}, false),
		sameCols,
	)
	if eq5.EqualsPlanWithoutChildren(otherIndex) {
		t.Fatal("covering scans over DIFFERENT indexes (IDX_A, IDX_B) compared " +
			"EQUAL; structuralKey is not folding the inner's index name")
	}

	// Direction is the third: a reverse scan emits the same rows in the
	// opposite order, so it is a different plan.
	reversed := NewRecordQueryCoveringIndexPlan(
		coveringTestIndexPlan("IDX_A", []*predicates.ComparisonRange{pkOrderingEq(t, int64(5))}, true),
		sameCols,
	)
	if eq5.EqualsPlanWithoutChildren(reversed) {
		t.Fatal("forward and REVERSE covering scans compared EQUAL; " +
			"structuralKey is not folding the inner's scan direction")
	}
}

// TestCoveringPlan_StructuralKeyFoldsCoveringColumns pins the mirror-image
// collision the RFC calls out as already latent on the covering BOOL: two scans
// over the same range covering DIFFERENT column sets emit different partial
// records, so they are not interchangeable group members either.
//
// The old covering bool folded only Bool(covering) and never coveringColumns,
// so this case collapsed. The new type must not reproduce that.
func TestCoveringPlan_StructuralKeyFoldsCoveringColumns(t *testing.T) {
	t.Parallel()

	comps := []*predicates.ComparisonRange{pkOrderingEq(t, int64(5))}

	narrow := NewRecordQueryCoveringIndexPlan(coveringTestIndexPlan("IDX_A", comps, false), []string{"A"})
	wide := NewRecordQueryCoveringIndexPlan(coveringTestIndexPlan("IDX_A", comps, false), []string{"A", "ID"})

	if narrow.EqualsPlanWithoutChildren(wide) {
		t.Fatal("covering scans over the same range with covered columns [A] and " +
			"[A ID] compared EQUAL. They emit DIFFERENT partial records, so " +
			"sharing a memo Reference is an unsound group.")
	}
	if narrow.HashCodeWithoutChildren() == wide.HashCodeWithoutChildren() {
		t.Fatal("covering scans with different covered-column sets hashed EQUAL")
	}
}

// TestCoveringPlan_IdenticalPlansStillDedup is the control for the three tests
// above: folding MORE into identity is only correct if genuinely identical
// plans still compare equal. Without this, a structuralKey that folded a
// per-instance value (an address, a fresh correlation id) would pass every
// inequality assertion above while destroying memo dedup entirely.
func TestCoveringPlan_IdenticalPlansStillDedup(t *testing.T) {
	t.Parallel()

	mk := func() *RecordQueryCoveringIndexPlan {
		return NewRecordQueryCoveringIndexPlan(
			coveringTestIndexPlan("IDX_A", []*predicates.ComparisonRange{pkOrderingEq(t, int64(5))}, false),
			[]string{"A", "ID"},
		)
	}
	a, b := mk(), mk()

	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("two structurally identical covering plans compared UNEQUAL — " +
			"structuralKey is folding a per-instance value (the stable resultValue " +
			"carries a unique correlation id and must stay excluded). The memo " +
			"would never dedup a covering scan.")
	}
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("two structurally identical covering plans hashed UNEQUAL")
	}
}

// TestCoveringPlan_ExplainCarriesCoveringMarker pins the rendering the plan
// goldens assert. Go keeps the `IndexScan(IDX, [..] COVERING)` shape rather
// than converging on Java's `COVERING(IDX [..] -> ..)`; that convergence is
// deliberately out of RFC-220's scope.
func TestCoveringPlan_ExplainCarriesCoveringMarker(t *testing.T) {
	t.Parallel()

	comps := []*predicates.ComparisonRange{pkOrderingEq(t, int64(5))}

	forward := NewRecordQueryCoveringIndexPlan(coveringTestIndexPlan("IDX_A", comps, false), []string{"A"})
	if got, want := forward.Explain(), "IndexScan(IDX_A, [=] COVERING)"; got != want {
		t.Fatalf("Explain() = %q, want %q", got, want)
	}

	// REVERSE renders after the closing paren, so the marker must be inserted
	// before it — not appended to the end of the whole label.
	reversed := NewRecordQueryCoveringIndexPlan(coveringTestIndexPlan("IDX_A", comps, true), []string{"A"})
	if got, want := reversed.Explain(), "IndexScan(IDX_A, [=] COVERING) REVERSE"; got != want {
		t.Fatalf("Explain() on a reverse scan = %q, want %q", got, want)
	}
}

// TestCoveringPlan_DelegatesToInner pins the delegation surface Java's covering
// plan defines (isReverse, getIndexName, isStrictlySorted). A covering wrapper
// that answered these from its own zero values would silently strip a scan's
// direction or sortedness from every property derived above it.
func TestCoveringPlan_DelegatesToInner(t *testing.T) {
	t.Parallel()

	inner := coveringTestIndexPlan("IDX_A", nil, true).WithStrictlySorted()
	cov := NewRecordQueryCoveringIndexPlan(inner, []string{"A"})

	if !cov.IsReverse() {
		t.Error("IsReverse() = false, want true — must delegate to the inner scan")
	}
	if cov.GetIndexName() != "IDX_A" {
		t.Errorf("GetIndexName() = %q, want \"IDX_A\"", cov.GetIndexName())
	}
	if !cov.IsStrictlySorted() {
		t.Error("IsStrictlySorted() = false, want true — must delegate to the inner scan")
	}
}
