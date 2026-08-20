package recordlayer

import (
	"bytes"
	"fmt"
	"sort"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
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

	// The constants are untyped, so Go widens them to `int` at the call site
	// while Java's Tuple.from(0) boxes an Integer. Both must pack to the FDB
	// tuple layer's INTEGER encoding, and these are its bytes from the spec —
	// 0x14 is the zero code point, 0x14+n prefixes an n-byte big-endian
	// positive integer. Asserting the BYTES rather than the Go values is what
	// makes this a Java claim: a Go tuple encoder that treated `int` as
	// anything else would still round-trip against itself.
	for _, tc := range []struct {
		name string
		key  int
		want []byte
	}{
		{"ENTRIES", slidingWindowEntriesSubspaceKey, []byte{0x14}},
		{"META", slidingWindowMetaSubspaceKey, []byte{0x15, 0x01}},
		{"COUNT", slidingWindowCountKey, []byte{0x15, 0x03}},
		{"BOUNDARY", slidingWindowBoundaryKey, []byte{0x15, 0x04}},
	} {
		got := tuple.Tuple{tc.key}.Pack()
		if !bytes.Equal(got, tc.want) {
			t.Errorf("%s subspace key packs to %#v, FDB's tuple encoding of %d is %#v",
				tc.name, got, tc.key, tc.want)
		}
		// The same value as an int64 must pack identically, or a maintainer
		// that happened to hold one width would address a different key than a
		// test holding the other.
		if wide := (tuple.Tuple{int64(tc.key)}).Pack(); !bytes.Equal(got, wide) {
			t.Errorf("%s packs differently as int (%#v) and int64 (%#v)", tc.name, got, wide)
		}
	}
}
