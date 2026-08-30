package plans

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// coveringTestIndexPlan builds an index scan over IDX(A) with the given
// comparison ranges — the inner half of a covering plan under test.
func coveringTestIndexPlan(t testing.TB, name string, comps []*predicates.ComparisonRange, reverse bool) *RecordQueryIndexPlan {
	return mustChecked(t, func() (*RecordQueryIndexPlan, error) {
		return NewRecordQueryIndexPlan(name, comps, []string{"T"}, exactTestRecordType(), reverse)
	}).WithIndexMetadata([]string{"A"}, []string{"ID"}, false)
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

	inner := coveringTestIndexPlan(t, "IDX_A", nil, false)
	cov := mustChecked(t, func() (*RecordQueryCoveringIndexPlan, error) {
		return NewRecordQueryCoveringIndexPlan(inner)
	})

	if got := cov.GetChildren(); len(got) != 0 {
		t.Fatalf("GetChildren() returned %d children, want 0 — the inner index "+
			"plan must be a FIELD, not a child. Exposing it as a child re-arms "+
			"both the unsound-group and the cost-misclassification failures "+
			"described in RFC-220 §4.1.", len(got))
	}

	got, err := cov.WithQuantifiers(nil)
	if err != nil {
		t.Fatalf("WithQuantifiers(nil): %v", err)
	}
	if got != cov {
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

	eq5 := mustChecked(t, func() (*RecordQueryCoveringIndexPlan, error) {
		return NewRecordQueryCoveringIndexPlan(coveringTestIndexPlan(t, "IDX_A", []*predicates.ComparisonRange{pkOrderingEq(t, int64(5))}, false))
	})
	eq7 := mustChecked(t, func() (*RecordQueryCoveringIndexPlan, error) {
		return NewRecordQueryCoveringIndexPlan(coveringTestIndexPlan(t, "IDX_A", []*predicates.ComparisonRange{pkOrderingEq(t, int64(7))}, false))
	})

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
	otherIndex := mustChecked(t, func() (*RecordQueryCoveringIndexPlan, error) {
		return NewRecordQueryCoveringIndexPlan(coveringTestIndexPlan(t, "IDX_B", []*predicates.ComparisonRange{pkOrderingEq(t, int64(5))}, false))
	})
	if eq5.EqualsPlanWithoutChildren(otherIndex) {
		t.Fatal("covering scans over DIFFERENT indexes (IDX_A, IDX_B) compared " +
			"EQUAL; structuralKey is not folding the inner's index name")
	}

	// Direction is the third: a reverse scan emits the same rows in the
	// opposite order, so it is a different plan.
	reversed := mustChecked(t, func() (*RecordQueryCoveringIndexPlan, error) {
		return NewRecordQueryCoveringIndexPlan(coveringTestIndexPlan(t, "IDX_A", []*predicates.ComparisonRange{pkOrderingEq(t, int64(5))}, true))
	})
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

	// The covered surface is DERIVED from the inner (entry key columns ++ the
	// KeyWithValue VALUE part), so the two sides differ by the inner's value
	// columns — the only way an inconsistent pair can still be expressed now
	// that the constructor no longer accepts a caller-supplied list.
	narrow := mustChecked(t, func() (*RecordQueryCoveringIndexPlan, error) {
		return NewRecordQueryCoveringIndexPlan(coveringTestIndexPlan(t, "IDX_A", comps, false))
	})
	wide := mustChecked(t, func() (*RecordQueryCoveringIndexPlan, error) {
		return NewRecordQueryCoveringIndexPlan(
			coveringTestIndexPlan(t, "IDX_A", comps, false).WithValueColumnNames([]string{"B"}))
	})

	if got, want := len(narrow.GetCoveringColumns()), 1; got != want {
		t.Fatalf("narrow covered columns = %v, want %d — derived from the inner's "+
			"AllCoveredEntryColumns", narrow.GetCoveringColumns(), want)
	}
	if got, want := len(wide.GetCoveringColumns()), 2; got != want {
		t.Fatalf("wide covered columns = %v, want %d", wide.GetCoveringColumns(), want)
	}

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
// plans still compare equal. Without this, a structuralKey that folded a value
// that genuinely varies per instance would pass every inequality assertion
// above while destroying memo dedup entirely.
//
// It does NOT catch resultValue specifically, which an earlier version of this
// comment and of the failure message below both claimed. Measured by mutation:
// adding StructVal(p.resultValue) — the ALIAS-SENSITIVE fold, the strictest one
// available — to this type's structuralKey leaves this test GREEN. The reason
// is that resultValue's per-instance uniqueness comes from
// newPlanExprBaseForType's ERASED-record branch, which mints a
// UniqueCorrelationIdentifier; this fixture's index plan has an exact record
// type, so it takes the layout branch and gets a DETERMINISTIC carrier that two
// separately-built plans share.
//
// So the guard is real for anything that varies per instance, and resultValue's
// exclusion rests on a branch no fixture here constructs. Stated rather than
// closed because manufacturing an erased-type covering fixture to guard an
// exclusion nothing currently threatens would be the more expensive half.
func TestCoveringPlan_IdenticalPlansStillDedup(t *testing.T) {
	t.Parallel()

	mk := func() *RecordQueryCoveringIndexPlan {
		return mustChecked(t, func() (*RecordQueryCoveringIndexPlan, error) {
			return NewRecordQueryCoveringIndexPlan(
				coveringTestIndexPlan(t, "IDX_A", []*predicates.ComparisonRange{pkOrderingEq(t, int64(5))}, false))
		})
	}
	a, b := mk(), mk()

	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("two structurally identical covering plans compared UNEQUAL — " +
			"structuralKey has started folding something that varies per instance, so " +
			"the memo would never dedup a covering scan. Note this fixture cannot " +
			"implicate resultValue: see the comment above for why folding it leaves " +
			"this test green.")
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

	forward := mustChecked(t, func() (*RecordQueryCoveringIndexPlan, error) {
		return NewRecordQueryCoveringIndexPlan(coveringTestIndexPlan(t, "IDX_A", comps, false))
	})
	if got, want := forward.Explain(), "IndexScan(IDX_A, [=] COVERING)"; got != want {
		t.Fatalf("Explain() = %q, want %q", got, want)
	}

	// REVERSE renders after the closing paren, so the marker must be inserted
	// before it — not appended to the end of the whole label.
	reversed := mustChecked(t, func() (*RecordQueryCoveringIndexPlan, error) {
		return NewRecordQueryCoveringIndexPlan(coveringTestIndexPlan(t, "IDX_A", comps, true))
	})
	if got, want := reversed.Explain(), "IndexScan(IDX_A, [=] COVERING) REVERSE"; got != want {
		t.Fatalf("Explain() on a reverse scan = %q, want %q", got, want)
	}

	// The marker appears EXACTLY ONCE. The previous rendering spliced " COVERING"
	// into the inner's already-rendered label at the last "]", which cannot tell
	// "the marker is absent" from "the marker is already there" and produced
	// `IndexScan(IDX_A, [] COVERING COVERING)` whenever the inner carried it.
	// Passing the flag down to the label builder makes the double stamp
	// unrepresentable; this pins that it stays so.
	if got := strings.Count(forward.Explain(), "COVERING"); got != 1 {
		t.Fatalf("Explain() = %q contains COVERING %d times, want exactly 1 — "+
			"the marker must be RENDERED by the inner's label builder, never "+
			"spliced into its finished output", forward.Explain(), got)
	}
	// The plain scan is the other half of the pin: a bare index plan must NOT
	// render the marker, or the covering type stops being what carries it.
	if got := coveringTestIndexPlan(t, "IDX_A", comps, false).Explain(); strings.Contains(got, "COVERING") {
		t.Fatalf("a bare index scan rendered %q — coveringness is a plan TYPE now, "+
			"so only the covering plan may render the marker", got)
	}
}

// TestCoveringPlan_DelegatesToInner pins the delegation surface Java's covering
// plan defines (isReverse, getIndexName, isStrictlySorted). A covering wrapper
// that answered these from its own zero values would silently strip a scan's
// direction or sortedness from every property derived above it.
func TestCoveringPlan_DelegatesToInner(t *testing.T) {
	t.Parallel()

	inner := coveringTestIndexPlan(t, "IDX_A", nil, true).WithStrictlySorted()
	cov := mustChecked(t, func() (*RecordQueryCoveringIndexPlan, error) {
		return NewRecordQueryCoveringIndexPlan(inner)
	})

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

// TestCoveringPlan_DelegatesRangeDescription pins the accessors that describe
// the physical RANGE. These are not conveniences: a covering scan is the shape
// the access path emits for every index-backed access, and it holds its index
// scan as a FIELD, so anything reasoning about the range from a plan tree can
// only reach these facts through this plan. Two consumers depend on them —
// the executor's baked-reference walk (which reads the scan comparisons to
// recover a correlated outer's type) and the row-diff harness's ordering
// derivation (which reads the key columns and their physical types). A
// delegator answering from a zero value would not fail loudly in either: the
// walk would report "no baked references" and the derivation "order
// unprovable", both of which read as a clean result.
func TestCoveringPlan_DelegatesRangeDescription(t *testing.T) {
	t.Parallel()

	comps := []*predicates.ComparisonRange{pkOrderingEq(t, int64(5))}
	inner := coveringTestIndexPlan(t, "IDX_A", comps, false).
		WithKeyComponentTypes([]values.Type{values.NotNullLong}).
		WithPrimaryKeyComponentTypes([]values.Type{values.NotNullLong})
	cov := mustChecked(t, func() (*RecordQueryCoveringIndexPlan, error) {
		return NewRecordQueryCoveringIndexPlan(inner)
	})

	if got := cov.GetScanComparisons(); len(got) != 1 || got[0] != comps[0] {
		t.Errorf("GetScanComparisons() = %v, want the inner scan's %v — the range is the inner's range", got, comps)
	}
	if got := cov.GetColumnNames(); len(got) != 1 || got[0] != "A" {
		t.Errorf("GetColumnNames() = %v, want the inner scan's [A]", got)
	}
	if got := cov.GetPKColumnNames(); len(got) != 1 || got[0] != "ID" {
		t.Errorf("GetPKColumnNames() = %v, want the inner scan's [ID]", got)
	}
	if got := cov.GetKeyComponentTypes(); len(got) != len(inner.GetKeyComponentTypes()) {
		t.Errorf("GetKeyComponentTypes() = %v, want the inner scan's %v", got, inner.GetKeyComponentTypes())
	}
	if got := cov.GetPrimaryKeyComponentTypes(); len(got) != len(inner.GetPrimaryKeyComponentTypes()) {
		t.Errorf("GetPrimaryKeyComponentTypes() = %v, want the inner scan's %v", got, inner.GetPrimaryKeyComponentTypes())
	}
	if !cov.IsReverse() && inner.IsReverse() {
		t.Error("IsReverse() dropped the inner scan's direction")
	}
}

// TestCoveringPlanDelegatesUniqueAndFlowedType is the consumer that makes the
// IsUnique and GetFlowedType delegators non-vacuous, and it exists because
// DELETING a delegator cannot fail loudly.
//
// Both names are reachable through anonymous-interface probes — the tree has
// two on IsUnique today (abstract_data_access_rule.go: candidateScanProps,
// candidateUnique). When such a probe stops matching there is no compiler error
// and no panic: the type assertion returns false, and the caller reads that as
// "not unique" rather than as "I asked a type that cannot answer". A wrong
// answer arriving silently, which is the failure class RFC-220's carrier work
// exists to remove.
//
// The unique arm drives BOTH values. A delegator that returned a hardcoded
// constant would satisfy a single-value assertion.
func TestCoveringPlanDelegatesUniqueAndFlowedType(t *testing.T) {
	t.Parallel()

	for _, unique := range []bool{true, false} {
		inner := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
			return NewRecordQueryIndexPlan("IDX_A", nil, []string{"T"}, exactTestRecordType(), false)
		}).
			WithIndexMetadata([]string{"A"}, []string{"ID"}, unique)
		cov := mustChecked(t, func() (*RecordQueryCoveringIndexPlan, error) {
			return NewRecordQueryCoveringIndexPlan(inner)
		})

		if got := cov.IsUnique(); got != unique {
			t.Errorf("covering.IsUnique() = %v, want %v (the wrapped scan's answer) — "+
				"uniqueness is a property of the INDEX and a covering scan reads the same index",
				got, unique)
		}
		// The probe SHAPE, not just the method: this is how the two live call
		// sites reach the name, and it is the form that fails silently.
		u, ok := any(cov).(interface{ IsUnique() bool })
		if !ok {
			t.Fatalf("covering plan no longer satisfies interface{ IsUnique() bool }. "+
				"Anonymous-interface probes on this name return FALSE when they stop "+
				"matching — no compile error, no panic — so removing the delegator "+
				"silently answers 'not unique' for every covering scan (unique=%v)", unique)
		}
		if u.IsUnique() != unique {
			t.Errorf("probed IsUnique() = %v, want %v", u.IsUnique(), unique)
		}
	}

	inner := coveringTestIndexPlan(t, "IDX_A", nil, false)
	cov := mustChecked(t, func() (*RecordQueryCoveringIndexPlan, error) {
		return NewRecordQueryCoveringIndexPlan(inner)
	})
	if cov.GetFlowedType() != inner.GetFlowedType() {
		t.Errorf("covering.GetFlowedType() = %v, want the wrapped scan's %v",
			cov.GetFlowedType(), inner.GetFlowedType())
	}
	if _, ok := any(cov).(interface{ GetFlowedType() values.Type }); !ok {
		t.Fatal("covering plan no longer satisfies interface{ GetFlowedType() values.Type }; " +
			"same silent-false hazard as IsUnique above")
	}
}
