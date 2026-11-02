package fdb_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
)

// A selector range read is one NativeAPI getRange, as fdb_transaction_get_range
// is: the RYW walk resolves the selectors against the write map and cache and
// sends the rest to storage inside the range request
// (ReadYourWrites.actor.cpp:592-813, 896-1123), so TransactionGetRangeRequests
// (NativeAPI.actor.cpp:5808) rises by one. Resolving each selector first with
// getKey, which under RYW also reads through getRange (:140-160), made it
// three sequential reads. The shapes are BunchedMap's put read and a
// lastLessThan..firstGreaterThan scan, forward and reverse.
func TestGetRange_SelectorRangeIsOneStorageRead(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	prefix := []byte(t.Name() + "/")
	key := func(s string) fdb.Key { return fdb.Key(append(bytes.Clone(prefix), s...)) }
	if _, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		for _, k := range []string{"a", "c", "e", "g", "i"} {
			tr.Set(key(k), []byte(k))
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	bunched := fdb.FirstGreaterThan(key("d"))
	bunched.Offset++
	cases := []struct {
		name    string
		r       fdb.SelectorRange
		reverse bool
		want    string
	}{
		{"bunched-put", fdb.SelectorRange{Begin: fdb.LastLessOrEqual(key("d")), End: bunched}, false, "c,e"},
		{"lastLessThan-firstGreaterThan", fdb.SelectorRange{Begin: fdb.LastLessThan(key("e")), End: fdb.FirstGreaterThan(key("g"))}, false, "c,e,g"},
		{"reverse", fdb.SelectorRange{Begin: fdb.LastLessThan(key("e")), End: fdb.FirstGreaterThan(key("g"))}, true, "g,e,c"},
	}
	for _, tc := range cases {
		for _, snapshot := range []bool{false, true} {
			name := fmt.Sprintf("%s/snapshot=%t", tc.name, snapshot)
			if _, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
				// Pin the read version and warm the location cache outside the range under test.
				if _, err := tr.Get(key("z")).Get(); err != nil {
					return nil, err
				}
				var reader fdb.ReadTransaction = tr
				if snapshot {
					reader = tr.Snapshot()
				}
				before := db.Metrics().TransactionGetRangeRequests
				rows, err := reader.GetRange(tc.r, fdb.RangeOptions{Reverse: tc.reverse}).GetSliceWithError()
				if err != nil {
					return nil, err
				}
				if n := db.Metrics().TransactionGetRangeRequests - before; n != 1 {
					t.Errorf("%s: %d getRange reads, want 1", name, n)
				}
				var got []string
				for _, kv := range rows {
					got = append(got, string(bytes.TrimPrefix(kv.Key, prefix)))
				}
				if strings.Join(got, ",") != tc.want {
					t.Errorf("%s: rows %v, want %s", name, got, tc.want)
				}
				return nil, nil
			}); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
	}
}
