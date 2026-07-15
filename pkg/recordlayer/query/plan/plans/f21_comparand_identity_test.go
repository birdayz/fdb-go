package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// equalityRange builds a single-column equality ComparisonRange `= lit`.
func equalityRange(t *testing.T, lit any) *predicates.ComparisonRange {
	t.Helper()
	res := predicates.EmptyComparisonRange().Merge(&predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: values.LiteralValue(lit),
	})
	if !res.Ok {
		t.Fatalf("EmptyComparisonRange().Merge(= %v) failed", lit)
	}
	return res.Range
}

// TestRecordQueryIndexPlan_ComparandIdentity (F21) pins that an index scan's
// identity includes the scan COMPARANDS, not just the range shape. Before the
// fix EqualsWithoutChildren/HashCodeWithoutChildren compared only
// GetRangeType(), so IndexScan(idx, [= 5]) EQUALED IndexScan(idx, [= 7]) — two
// scans that read DIFFERENT key ranges. Because an index scan is a memo leaf
// deduped by hash+equality, that collapsed them into one Reference and let
// extraction materialize the wrong-comparand scan. Java's
// RecordQueryIndexPlan.equalsWithoutChildren compares
// Objects.equals(scanParameters) — full comparand equality.
func TestRecordQueryIndexPlan_ComparandIdentity(t *testing.T) {
	t.Parallel()

	p5 := NewRecordQueryIndexPlan("idx", []*predicates.ComparisonRange{equalityRange(t, int64(5))}, []string{"T"}, values.UnknownType, false)
	p7 := NewRecordQueryIndexPlan("idx", []*predicates.ComparisonRange{equalityRange(t, int64(7))}, []string{"T"}, values.UnknownType, false)

	// Different comparands MUST be unequal (was TRUE before the fix — the bug).
	if p5.EqualsWithoutChildren(p7) {
		t.Error("IndexScan([= 5]) must NOT EqualsWithoutChildren IndexScan([= 7]) — comparand-blind collapse")
	}
	if p5.HashCodeWithoutChildren() == p7.HashCodeWithoutChildren() {
		t.Error("IndexScan([= 5]) and IndexScan([= 7]) must hash apart — comparand-blind hash")
	}

	// SAME comparand MUST stay equal + same hash (preserve dedup for real twins).
	p5b := NewRecordQueryIndexPlan("idx", []*predicates.ComparisonRange{equalityRange(t, int64(5))}, []string{"T"}, values.UnknownType, false)
	if !p5.EqualsWithoutChildren(p5b) {
		t.Error("IndexScan([= 5]) must EqualsWithoutChildren an identical IndexScan([= 5])")
	}
	if p5.HashCodeWithoutChildren() != p5b.HashCodeWithoutChildren() {
		t.Error("identical IndexScan([= 5]) must hash identically")
	}

	// A string comparand of the same range shape is still distinct.
	pStr := NewRecordQueryIndexPlan("idx", []*predicates.ComparisonRange{equalityRange(t, "5")}, []string{"T"}, values.UnknownType, false)
	if p5.EqualsWithoutChildren(pStr) {
		t.Error("IndexScan([= int64(5)]) must NOT equal IndexScan([= \"5\"]) — type+value comparand")
	}
}

// TestRecordQueryScanPlan_ComparandIdentity (F21 sibling — primary scan) pins
// that a primary-key scan's identity includes the PK bound comparands, not just
// the range shape. Same memo-leaf collapse risk as the index scan.
func TestRecordQueryScanPlan_ComparandIdentity(t *testing.T) {
	t.Parallel()

	s5 := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithScanComparisons([]*predicates.ComparisonRange{equalityRange(t, int64(5))})
	s7 := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithScanComparisons([]*predicates.ComparisonRange{equalityRange(t, int64(7))})

	if s5.EqualsWithoutChildren(s7) {
		t.Error("Scan(pk = 5) must NOT EqualsWithoutChildren Scan(pk = 7) — comparand-blind collapse")
	}
	if s5.HashCodeWithoutChildren() == s7.HashCodeWithoutChildren() {
		t.Error("Scan(pk = 5) and Scan(pk = 7) must hash apart")
	}

	s5b := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithScanComparisons([]*predicates.ComparisonRange{equalityRange(t, int64(5))})
	if !s5.EqualsWithoutChildren(s5b) {
		t.Error("identical Scan(pk = 5) must remain EqualsWithoutChildren-equal")
	}
	if s5.HashCodeWithoutChildren() != s5b.HashCodeWithoutChildren() {
		t.Error("identical Scan(pk = 5) must hash identically")
	}
}

// TestRecordQueryInJoinPlan_InValuesIdentity (F21 InJoin mirror) pins that the
// static IN-list comparand is part of an InJoin's identity, while the binding
// alias stays excluded (RFC-164 WS-4).
func TestRecordQueryInJoinPlan_InValuesIdentity(t *testing.T) {
	t.Parallel()
	inner := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)

	mk := func(alias string, vals []any) *RecordQueryInJoinPlan {
		p := NewRecordQueryInJoinPlan(inner, alias, true, false)
		p.SetInValues(vals)
		return p
	}

	a := mk("q$1", []any{int64(1), int64(2), int64(3)})
	b := mk("q$1", []any{int64(4), int64(5), int64(6)}) // same shape, DIFFERENT IN-list

	// Different inValues MUST be unequal (was TRUE before the fix — the bug).
	if a.EqualsWithoutChildren(b) {
		t.Error("InJoins with different inValues must NOT be EqualsWithoutChildren-equal")
	}
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Error("InJoins with different inValues must hash apart")
	}

	// A binding-alias-only difference MUST stay equal + same hash (RFC-164 preserved).
	c := mk("q$9999", []any{int64(1), int64(2), int64(3)}) // same inValues, different alias
	if !a.EqualsWithoutChildren(c) {
		t.Error("InJoins differing ONLY in binding alias must remain EqualsWithoutChildren-equal (RFC-164)")
	}
	if a.HashCodeWithoutChildren() != c.HashCodeWithoutChildren() {
		t.Error("InJoins differing ONLY in binding alias must hash identically (RFC-164)")
	}
}
