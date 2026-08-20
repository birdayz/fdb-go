package recordlayer

import (
	"sort"
	"testing"
)

// TestRecordStoreKeyspaceMatchesJava pins every record-store keyspace prefix by
// VALUE against Java's FDBRecordStoreKeyspace enum at tag 4.12.11.0.
//
// These integers are the first tuple item inside a record store's subspace, so
// they are wire: a Go store that writes records under a different prefix than
// Java is not a store Java can read. The enum is @API(UNSTABLE) in Java and HAS
// grown — INDEX_SLIDING_WINDOW_SPACE(10) is the most recent addition — so this
// test is also where a future Java bump gets recorded.
//
// Three separate claims are checked, because each fails differently:
//   - every constant equals its Java value (a renumber);
//   - no two constants share a value (a copy-paste collision, which a
//     value-by-value check alone would not catch, since both copies can be
//     individually "right" against different Java names);
//   - the set is contiguous from 0 (a GAP, which is how a prefix that Java has
//     allocated goes missing from Go without any existing constant being wrong).
func TestRecordStoreKeyspaceMatchesJava(t *testing.T) {
	t.Parallel()

	// FDBRecordStoreKeyspace.java, tag 4.12.11.0.
	javaKeyspaces := []struct {
		name string
		id   int
	}{
		{"STORE_INFO", StoreInfoKey},
		{"RECORD", RecordKey},
		{"INDEX", IndexKey},
		{"INDEX_SECONDARY_SPACE", IndexSecondarySpaceKey},
		{"RECORD_COUNT", RecordCountKey},
		{"INDEX_STATE_SPACE", IndexStateSpaceKey},
		{"INDEX_RANGE_SPACE", IndexRangeSpaceKey},
		{"INDEX_UNIQUENESS_VIOLATIONS_SPACE", IndexUniquenessViolationsKey},
		{"RECORD_VERSION_SPACE", RecordVersionKey},
		{"INDEX_BUILD_SPACE", IndexBuildSpaceKey},
		{"INDEX_SLIDING_WINDOW_SPACE", IndexSlidingWindowSpaceKey},
	}

	wantIDs := map[string]int{
		"STORE_INFO":                        0,
		"RECORD":                            1,
		"INDEX":                             2,
		"INDEX_SECONDARY_SPACE":             3,
		"RECORD_COUNT":                      4,
		"INDEX_STATE_SPACE":                 5,
		"INDEX_RANGE_SPACE":                 6,
		"INDEX_UNIQUENESS_VIOLATIONS_SPACE": 7,
		"RECORD_VERSION_SPACE":              8,
		"INDEX_BUILD_SPACE":                 9,
		"INDEX_SLIDING_WINDOW_SPACE":        10,
	}

	if len(javaKeyspaces) != len(wantIDs) {
		t.Fatalf("the table under test lists %d keyspaces but %d expected values are declared; "+
			"a Java bump must add a row to BOTH", len(javaKeyspaces), len(wantIDs))
	}

	// (1) value-by-value.
	for _, ks := range javaKeyspaces {
		want, ok := wantIDs[ks.name]
		if !ok {
			t.Fatalf("keyspace %q has no expected value declared", ks.name)
		}
		if ks.id != want {
			t.Errorf("keyspace %s = %d, Java's FDBRecordStoreKeyspace.%s is %d — "+
				"this prefix is wire; a Go store writing under %d is unreadable by Java",
				ks.name, ks.id, ks.name, want, ks.id)
		}
	}

	// (2) no duplicates. Two constants sharing a prefix put two kinds of data in
	// one keyspace, where writes of one silently overwrite the other.
	seen := make(map[int]string, len(javaKeyspaces))
	for _, ks := range javaKeyspaces {
		if prev, dup := seen[ks.id]; dup {
			t.Errorf("keyspaces %s and %s both use prefix %d", prev, ks.name, ks.id)
		}
		seen[ks.id] = ks.name
	}

	// (3) contiguous from 0. A gap is how a Java-allocated prefix goes missing
	// from Go while every constant Go DOES have is still correct — the failure
	// (1) and (2) both pass through.
	ids := make([]int, 0, len(javaKeyspaces))
	for _, ks := range javaKeyspaces {
		ids = append(ids, ks.id)
	}
	sort.Ints(ids)
	for i, id := range ids {
		if id != i {
			t.Errorf("keyspace prefixes are not contiguous from 0: sorted[%d] = %d; "+
				"got %v — a gap means Java has allocated a prefix Go has not declared",
				i, id, ids)
			break
		}
	}
}

// TestSlidingWindowSubspaceLayout pins the BYTES of the sliding-window
// bookkeeping keys — the wire contract this whole index type rests on.
//
// Java composes them scope-by-partition-then-region
// (SlidingWindowIndexMaintainer.java:441-444):
//
//	<storeSubspace>/10/<index.getSubspaceTupleKey()>/<partition...>/{0|1}/...
//
// with ENTRIES=Tuple.from(0), META=Tuple.from(1), COUNT=Tuple.from(3) and
// BOUNDARY=Tuple.from(4). 2 and 5 are deliberately absent from the meta region.
func TestSlidingWindowSubspaceLayout(t *testing.T) {
	t.Parallel()

	if slidingWindowEntriesSubspaceKey != 0 {
		t.Errorf("ENTRIES subspace key = %d, Java's Tuple.from(0)", slidingWindowEntriesSubspaceKey)
	}
	if slidingWindowMetaSubspaceKey != 1 {
		t.Errorf("META subspace key = %d, Java's Tuple.from(1)", slidingWindowMetaSubspaceKey)
	}
	if slidingWindowCountKey != 3 {
		t.Errorf("COUNT key = %d, Java's Tuple.from(3)", slidingWindowCountKey)
	}
	if slidingWindowBoundaryKey != 4 {
		t.Errorf("BOUNDARY key = %d, Java's Tuple.from(4)", slidingWindowBoundaryKey)
	}

	// The meta region deliberately has holes at 2 and 5. Renumbering to close
	// them would be a silent wire break, so the holes are asserted rather than
	// merely commented.
	for _, taken := range []int{slidingWindowCountKey, slidingWindowBoundaryKey} {
		if taken == 2 || taken == 5 {
			t.Errorf("meta key %d occupies one of Java's deliberate holes (2, 5)", taken)
		}
	}
}
