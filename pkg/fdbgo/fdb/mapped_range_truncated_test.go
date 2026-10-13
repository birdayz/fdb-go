package fdb_test

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

func TestMappedRangeTruncatedSecondaryAllowsUnreadTailWrites(t *testing.T) {
	t.Parallel()
	db, r, mapper := mappedFixture(t, 1)
	const splits = 2500
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		for i := 3; i < splits; i++ {
			tr.Set(fdb.Key(tuple.Tuple{t.Name(), "REC", 0, i}.Pack()), []byte("split"))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, reverse := range []bool{false, true} {
		tr, err := db.CreateTransaction()
		if err != nil {
			t.Fatal(err)
		}
		control := tr.GetMappedRange(r, mapper, fdb.RangeOptions{Reverse: reverse}).GetSliceOrPanic()
		tr.Cancel()
		if len(control) != 1 || !control[0].GetRange.More || len(control[0].GetRange.Rows) == 0 || len(control[0].GetRange.Rows) >= splits {
			t.Fatalf("secondary truncation control reverse=%v: %+v", reverse, control)
		}
		tr, err = db.CreateTransaction()
		if err != nil {
			t.Fatal(err)
		}
		tr.Set(fdb.Key(tuple.Tuple{t.Name(), "REC", 0, splits - 1}.Pack()), []byte("pending-unread-tail"))
		rows, err := tr.GetMappedRange(r, mapper, fdb.RangeOptions{Reverse: reverse}).GetSliceWithError()
		tr.Cancel()
		if err != nil {
			t.Fatalf("truncated secondary did not read the pending tail write reverse=%v: %v", reverse, err)
		}
		if len(rows) != 1 || !rows[0].GetRange.More || len(rows[0].GetRange.Rows) != len(control[0].GetRange.Rows) {
			t.Fatalf("truncated secondary changed reverse=%v: %+v", reverse, rows)
		}
	}
}
