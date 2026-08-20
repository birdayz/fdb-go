package recordlayer

import (
	"fmt"
	"sort"
	"testing"
)

// TestRecordStoreKeyspaceMatchesJava pins the record-store subspace prefixes.
//
// These are WIRE constants: a Go store and a Java store put the same thing in
// the same place or they cannot read each other. Java's
// FDBRecordStoreKeyspace.java is the source of truth, and it is @API(UNSTABLE)
// — it has grown at least once (INDEX_SLIDING_WINDOW_SPACE(10) is newer than
// this file's original 0-9), which is exactly why the set is asserted rather
// than assumed.
//
// It is deliberately a VALUE table rather than a count: a count would pass while
// two prefixes swapped meaning, which is the mis-read that produces a store
// neither engine can interpret.
//
// The test cannot read Java at runtime — the checkout is gitignored and absent
// in CI — so it pins Go's side and names the file to diff against. Adding a
// prefix must be a deliberate edit here, not something that lands silently.
func TestRecordStoreKeyspaceMatchesJava(t *testing.T) {
	t.Parallel()

	// name -> value, transcribed from
	// FDBRecordStoreKeyspace.java (tag 4.12.11.0), enum values 0-10.
	want := map[string]int{
		"STORE_INFO":                        StoreInfoKey,
		"RECORD":                            RecordKey,
		"INDEX":                             IndexKey,
		"INDEX_SECONDARY_SPACE":             IndexSecondarySpaceKey,
		"RECORD_COUNT":                      RecordCountKey,
		"INDEX_STATE_SPACE":                 IndexStateSpaceKey,
		"INDEX_RANGE_SPACE":                 IndexRangeSpaceKey,
		"INDEX_UNIQUENESS_VIOLATIONS_SPACE": IndexUniquenessViolationsKey,
		"RECORD_VERSION_SPACE":              RecordVersionKey,
		"INDEX_BUILD_SPACE":                 IndexBuildSpaceKey,
		"INDEX_SLIDING_WINDOW_SPACE":        IndexSlidingWindowSpaceKey,
	}
	expected := map[string]int{
		"STORE_INFO": 0, "RECORD": 1, "INDEX": 2, "INDEX_SECONDARY_SPACE": 3,
		"RECORD_COUNT": 4, "INDEX_STATE_SPACE": 5, "INDEX_RANGE_SPACE": 6,
		"INDEX_UNIQUENESS_VIOLATIONS_SPACE": 7, "RECORD_VERSION_SPACE": 8,
		"INDEX_BUILD_SPACE": 9, "INDEX_SLIDING_WINDOW_SPACE": 10,
	}

	for name, got := range want {
		if exp, ok := expected[name]; !ok {
			t.Fatalf("%s is not a Java keyspace name — the table drifted", name)
		} else if got != exp {
			t.Fatalf("%s = %d, want %d. This is a WIRE constant: a store written with "+
				"the wrong prefix is unreadable by the other engine, and silently so — "+
				"the data is present, just somewhere nobody looks.\n"+
				"  Source of truth: FDBRecordStoreKeyspace.java", name, got, exp)
		}
	}

	// The values must PARTITION 0..N with no gap and no duplicate. A gap means a
	// prefix nothing is guarding, which the next feature wanting space inside a
	// record store would take — and then collide with the Java release that
	// claims it. A duplicate means two things share a prefix.
	seen := make(map[int]string, len(want))
	values := make([]int, 0, len(want))
	for name, v := range want {
		if prev, dup := seen[v]; dup {
			t.Fatalf("prefix %d is claimed by BOTH %s and %s — two keyspaces sharing a "+
				"prefix interleave their keys", v, prev, name)
		}
		seen[v] = name
		values = append(values, v)
	}
	sort.Ints(values)
	for i, v := range values {
		if v != i {
			t.Fatalf("keyspace values are not contiguous from 0: got %v, first gap at %d.\n"+
				"  A gap is an UNRESERVED prefix. Java's enum is @API(UNSTABLE) and grows; "+
				"reserve every value it defines even if Go writes nothing there, so the "+
				"prefix is spoken for rather than available.", values, i)
		}
	}

	// A statement about the population, so a shrunk table cannot pass quietly.
	if len(want) != 11 {
		t.Fatalf("pinned %d keyspace prefixes, want 11 (Java 4.12.11.0 defines 0-10). %s",
			len(want), func() string {
				names := make([]string, 0, len(want))
				for n := range want {
					names = append(names, n)
				}
				sort.Strings(names)
				return fmt.Sprintf("have: %v", names)
			}())
	}
}
