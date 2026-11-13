package client

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"testing"
)

// A pending read keeps its rows uncopied until Get, after its walk is back in the
// pool: a resolved AppendIfFits value must outlive the walk's reused scratch.
func TestPendingRangeKeepsAppendedValuesAcrossWalks(t *testing.T) {
	// One P, so each read's goroutine takes the walk the previous one released
	// (sync.Pool is per P). Not parallel: GOMAXPROCS is process-wide.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	const pairs = 32
	st := newBenchStorage(2 * pairs)
	ctx := context.Background()
	for round := range 40 {
		tx := &Transaction{}
		tx.nativeRange = st.native
		key := func(i int) []byte { return st.m.kvs[i].Key }
		// Even keys' bases come from storage into the snapshot cache; odd keys
		// stay unknown, so a read over a pair needs storage again.
		for j := range pairs {
			k := key(2 * j)
			b, e := KeySelector{Key: k, Offset: 1}, KeySelector{Key: k, OrEqual: true, Offset: 1}
			if kvs, _, err := tx.Snapshot().GetRangeSelectors(ctx, b, e, 0, 0, false); err != nil || len(kvs) != 1 {
				t.Fatalf("round %d: storage read %d: %v %v", round, j, kvs, err)
			}
		}
		want := make([][]byte, pairs)
		for j := range pairs {
			operand := fmt.Appendf(nil, "+%02d-%02d", round, j)
			tx.Atomic(MutAppendIfFits, key(2*j), operand)
			want[j] = append([]byte("signpost"), operand...)
		}
		pending := make([]*PendingRange, pairs)
		for j := range pairs {
			b := KeySelector{Key: key(2 * j), Offset: 1}
			e := KeySelector{Key: key(2*j + 1), OrEqual: true, Offset: 1}
			pending[j] = tx.Snapshot().StartGetRangeSelectors(ctx, b, e, 0, 0, false)
			// The read executes in its goroutine, not in a Get: it keeps its
			// rows uncopied and its walk is back in the pool for the next read.
			<-pending[j].done
			if pending[j].ranByCaller {
				t.Fatalf("round %d: read %d ran in the caller", round, j)
			}
		}
		for j := pairs - 1; j >= 0; j-- {
			kvs, _, err := pending[j].Get()
			if err != nil || len(kvs) != 2 {
				t.Fatalf("round %d: read %d: %v %v", round, j, kvs, err)
			}
			if !bytes.Equal(kvs[0].Value, want[j]) {
				t.Fatalf("round %d: read %d returned %q for the appended key, want %q", round, j, kvs[0].Value, want[j])
			}
		}
	}
}
