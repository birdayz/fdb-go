package plans

import (
	"reflect"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/indexfetch"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

var allIndexFetchMethods = []indexfetch.Method{
	indexfetch.ScanAndFetch, indexfetch.UseRemoteFetch, indexfetch.UseRemoteFetchWithFallback,
}

// TestIndexPlan_FetchMethodDefaultsToScanAndFetch pins Java's short
// RecordQueryIndexPlan constructor (:171-173): a plan nobody stamped scans and
// fetches. The aggregate and windowed candidates rely on it for their non-BY_VALUE
// scans.
func TestIndexPlan_FetchMethodDefaultsToScanAndFetch(t *testing.T) {
	t.Parallel()
	plan := coveringTestIndexPlan(t, "IDX_A", nil, false)
	if got := plan.GetIndexFetchMethod(); got != indexfetch.ScanAndFetch {
		t.Fatalf("unstamped plan fetch method = %v, want SCAN_AND_FETCH", got)
	}
}

// TestIndexPlan_FetchMethodSurvivesEveryCopy pins Java's copies (strictlySorted
// :472-475, minimize :507-512, withIndexScanParameters :514-518): every
// metadata-preserving copy keeps the method, or a rule that rewrites a stamped
// scan would silently turn remote fetch off. The census is of the type's own
// With* methods, so a new one without a case here fails.
func TestIndexPlan_FetchMethodSurvivesEveryCopy(t *testing.T) {
	t.Parallel()
	for _, method := range allIndexFetchMethods {
		base := coveringTestIndexPlan(t, "IDX_A", []*predicates.ComparisonRange{pkOrderingEq(t, int64(5))}, false).
			WithIndexFetchMethod(method)
		copies := map[string]*RecordQueryIndexPlan{
			"WithScanComparisons":             base.WithScanComparisons([]*predicates.ComparisonRange{pkOrderingEq(t, int64(7))}),
			"WithKeyComponentTypes":           base.WithKeyComponentTypes([]values.Type{values.NotNullLong}),
			"WithPrimaryKeyComponentTypes":    base.WithPrimaryKeyComponentTypes([]values.Type{values.NotNullLong}),
			"WithPhysicalGroupingPrefixCount": base.WithPhysicalGroupingPrefixCount(1),
			"WithCommonPrimaryKey":            base.WithCommonPrimaryKey(nil),
			"WithValueColumnNames":            base.WithValueColumnNames([]string{"B"}),
			"WithColumnPaths":                 base.WithColumnPaths(nil, nil),
			"WithDistinctRecordsSignal":       base.WithDistinctRecordsSignal(false),
			"WithIndexMetadata":               base.WithIndexMetadata([]string{"A"}, []string{"ID"}, true),
			"WithOrderingColumns":             base.WithOrderingColumns(nil),
			"WithOrderingKeyNamesUnavailable": base.WithOrderingKeyNamesUnavailable(),
			"WithStrictlySorted":              base.WithStrictlySorted(),
			"WithDistinctProofIndexName":      base.WithDistinctProofIndexName("IDX_U").(*RecordQueryIndexPlan),
			"WithMatchedIndexPredicate":       base.WithMatchedIndexPredicate([]byte{1}),
			"WithEntryReader":                 base.WithEntryReader(nil),
		}
		planType := reflect.TypeOf(base)
		for i := range planType.NumMethod() {
			name := planType.Method(i).Name
			if !strings.HasPrefix(name, "With") || name == "WithIndexFetchMethod" || name == "WithQuantifiers" {
				continue
			}
			if _, ok := copies[name]; !ok {
				t.Fatalf("%s copies RecordQueryIndexPlan with no case here", name)
			}
		}
		for name, copied := range copies {
			if _, ok := planType.MethodByName(name); !ok {
				t.Fatalf("case %s names no method", name)
			}
			if got := copied.GetIndexFetchMethod(); got != method {
				t.Errorf("%s dropped the fetch method: got %v, want %v", name, got, method)
			}
		}
	}
}

// TestIndexPlan_FetchMethodIsPlanIdentityNotExplain pins Java's
// equalsWithoutChildren (:534-551) and computeHashCodeWithoutChildren
// (:562-566), which both include the method, and ExplainPlanVisitor
// (:449-451), which never renders it. Two plans differing only in the method
// are different memo members with the same EXPLAIN text.
func TestIndexPlan_FetchMethodIsPlanIdentityNotExplain(t *testing.T) {
	t.Parallel()
	base := coveringTestIndexPlan(t, "IDX_A", []*predicates.ComparisonRange{pkOrderingEq(t, int64(5))}, true)
	for i, a := range allIndexFetchMethods {
		for j, b := range allIndexFetchMethods {
			pa, pb := base.WithIndexFetchMethod(a), base.WithIndexFetchMethod(b)
			if equal := pa.EqualsPlanWithoutChildren(pb); equal != (i == j) {
				t.Errorf("%v equals %v = %v, want %v", a, b, equal, i == j)
			}
			if sameHash := pa.HashCodeWithoutChildren() == pb.HashCodeWithoutChildren(); sameHash != (i == j) {
				t.Errorf("%v and %v hash equal = %v, want %v", a, b, sameHash, i == j)
			}
			if pa.Explain() != pb.Explain() {
				t.Errorf("EXPLAIN differs by fetch method: %q vs %q", pa.Explain(), pb.Explain())
			}
			ca := mustChecked(t, func() (*RecordQueryCoveringIndexPlan, error) { return NewRecordQueryCoveringIndexPlan(pa) })
			cb := mustChecked(t, func() (*RecordQueryCoveringIndexPlan, error) { return NewRecordQueryCoveringIndexPlan(pb) })
			// RecordQueryCoveringIndexPlan.equalsWithoutChildren compares its
			// index plans, method included.
			if equal := ca.EqualsPlanWithoutChildren(cb); equal != (i == j) {
				t.Errorf("covering over %v equals covering over %v = %v, want %v", a, b, equal, i == j)
			}
			if ca.Explain() != cb.Explain() {
				t.Errorf("covering EXPLAIN differs by fetch method: %q vs %q", ca.Explain(), cb.Explain())
			}
		}
	}
	if got := base.Explain(); got != "IndexScan(IDX_A, [=]) REVERSE" {
		t.Fatalf("EXPLAIN = %q; the text the fetch method must not change moved", got)
	}
}
