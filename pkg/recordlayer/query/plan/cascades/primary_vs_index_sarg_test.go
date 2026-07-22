package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestComparisonSetKey_BareComparisonIdentity pins RFC-188 finding 3: the SARG
// comparison set is keyed by the BARE comparison (type + comparand), never by
// column/position — Java's ComparisonsProperty is a Set<Comparisons.Comparison>
// and Comparison.equals ignores the field, which is implicit in the
// ScanComparisons position. Keying by column would make Sets.difference
// non-empty where Java's is empty and fire the sub-case on the wrong queries.
func TestComparisonSetKey_BareComparisonIdentity(t *testing.T) {
	t.Parallel()

	eq5a := &predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(5))}
	eq5b := &predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(5))}
	eq7 := &predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(7))}
	gt5 := &predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(5))}

	if comparisonSetKey(eq5a) != comparisonSetKey(eq5b) {
		t.Fatal("same type+comparand produced different keys — set membership would double-count")
	}
	if comparisonSetKey(eq5a) == comparisonSetKey(eq7) {
		t.Fatal("different comparand collapsed to the same key")
	}
	if comparisonSetKey(eq5a) == comparisonSetKey(gt5) {
		t.Fatal("different comparison type collapsed to the same key")
	}
}

// TestSetDifferenceEmpty pins the "index SARGs strictly more" predicate: the
// sub-case prefers the index iff (primary − index) is empty AND (index −
// primary) is non-empty.
func TestSetDifferenceEmpty(t *testing.T) {
	t.Parallel()

	key := func(op int64, typ predicates.ComparisonType) string {
		return comparisonSetKey(&predicates.Comparison{Type: typ, Operand: values.LiteralValue(op)})
	}
	pk := key(1, predicates.ComparisonEquals)    // shared: pk = ?
	rt := key(99, predicates.ComparisonEquals)   // index-only: recordType = ?
	extra := key(2, predicates.ComparisonEquals) // primary-only

	primary := map[string]struct{}{pk: {}}
	index := map[string]struct{}{pk: {}, rt: {}}

	// The index has everything the primary has (pk) plus more (rt): prefer index.
	if !setDifferenceEmpty(primary, index) {
		t.Fatal("primary − index should be empty (index covers pk)")
	}
	if setDifferenceEmpty(index, primary) {
		t.Fatal("index − primary should be NON-empty (rt is index-only)")
	}

	// Primary has an extra SARG the index lacks: sub-case must NOT fire.
	primaryExtra := map[string]struct{}{pk: {}, extra: {}}
	if setDifferenceEmpty(primaryExtra, index) {
		t.Fatal("primary − index should be non-empty when primary has an extra SARG")
	}

	// Identical sets: neither difference non-empty → sub-case does not fire.
	if !setDifferenceEmpty(index, index) || !setDifferenceEmpty(primary, primary) {
		t.Fatal("a − a must be empty")
	}
}
