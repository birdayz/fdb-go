package client

import "testing"

// A transaction's first Set copies its key once, for the map and the sorted
// index: the map, its first group, the key and the value.
func TestRYWSetFirstEntryAllocs(t *testing.T) {
	c := &rywCache{}
	key, value := []byte("key"), []byte("value")
	allocs := testing.AllocsPerRun(100, func() {
		*c = rywCache{}
		c.set(key, value)
	})
	if allocs > 4 {
		t.Fatalf("ALLOC_RYW_FIRST_SET: %v allocations, want at most 4", allocs)
	}
}

// An owned write-conflict vector is captured without a copy; any other is copied.
func TestCaptureCommitVectorsConflictOwnership(t *testing.T) {
	tx := &Transaction{readVersionState: &readVersionState{}}
	ws := []KeyRange{{Begin: []byte("a"), End: []byte("b")}}
	if in := tx.captureCommitVectors(nil, false, ws, true, nil, false); &in.writeConflicts[0] != &ws[0] {
		t.Fatal("ALLOC_CAPTURE_OWNED: an owned vector was copied")
	}
	if in := tx.captureCommitVectors(nil, false, ws, false, nil, false); &in.writeConflicts[0] == &ws[0] {
		t.Fatal("a borrowed vector was captured without a copy")
	}
}
