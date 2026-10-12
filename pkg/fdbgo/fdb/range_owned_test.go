package fdb_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
)

func TestNativeRangeOwnedBoundsAcrossBatches(t *testing.T) {
	t.Parallel()
	for _, snapshot := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("snapshot=%t/reverse=%t", snapshot, reverse), func(t *testing.T) {
				t.Parallel()
				db, gate := openGRVHoldDB(t)
				defer gate.release()
				key := func(i int) fdb.Key { return fdb.Key(fmt.Sprintf("%s/%02d", t.Name(), i)) }
				value := bytes.Repeat([]byte{'v'}, 100)
				if _, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
					for i := 0; i < 20; i++ {
						tr.Set(key(i), value)
					}
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
				tr, err := db.CreateTransaction()
				if err != nil {
					t.Fatal(err)
				}
				defer tr.Cancel()
				begin, end := key(1), key(19)
				var reader fdb.ReadTransaction = tr
				if snapshot {
					reader = tr.Snapshot()
				}
				gate.arm()
				result := reader.GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{Mode: fdb.StreamingModeSmall, Reverse: reverse})
				scribbleKeyBuf(begin)
				scribbleKeyBuf(end)
				select {
				case <-gate.heldCh:
				case <-time.After(30 * time.Second):
					t.Fatal("range did not issue its first batch before consumption")
				}
				gate.release()
				check := func(rows []fdb.KeyValue, err error) {
					t.Helper()
					if err != nil || len(rows) != 18 {
						t.Fatalf("range = %d rows, %v; want 18", len(rows), err)
					}
					for i, row := range rows {
						n := i + 1
						if reverse {
							n = 18 - i
						}
						if !bytes.Equal(row.Key, key(n)) || !bytes.Equal(row.Value, value) {
							t.Fatalf("row %d = %q/%q", i, row.Key, row.Value)
						}
					}
				}
				for consumer := 0; consumer < 2; consumer++ {
					it := result.Iterator()
					var rows []fdb.KeyValue
					batches := 0
					it.SetTraceLog(func(_, _, _ int, _ bool, _ error) { batches++ })
					for it.Advance() {
						kv, err := it.Get()
						if err != nil {
							t.Fatal(err)
						}
						again, err := it.Get()
						if err != nil || !bytes.Equal(again.Key, kv.Key) || !bytes.Equal(again.Value, kv.Value) {
							t.Fatalf("repeated Get = %v, %v", again, err)
						}
						rows = append(rows, kv)
					}
					check(rows, nil)
					if batches < 2 {
						t.Fatalf("only %d traced batches; continuation path not exercised", batches)
					}
					rows[0].Key[0], rows[0].Value[0] = '!', '!'
					materialized, err := result.GetSliceWithError()
					check(materialized, err)
					materialized[0].Key[0], materialized[0].Value[0] = '?', '?'
				}
				// A snapshot must not gain a read conflict; an ordinary read must retain it.
				if _, err := db.Transact(func(w fdb.WritableTransaction) (any, error) { w.Set(key(10), []byte("changed")); return nil, nil }); err != nil {
					t.Fatal(err)
				}
				tr.Set(key(99), []byte("commit-marker"))
				err = tr.Commit().Get()
				var fe fdb.Error
				if snapshot && err != nil {
					t.Fatalf("snapshot commit: %v", err)
				}
				if !snapshot && (!errors.As(err, &fe) || fe.Code != 1020) {
					t.Fatalf("ordinary read commit = %v, want 1020", err)
				}
			})
		}
	}
}

func TestNativeRangeOwnedBoundsCancelledBeforeReply(t *testing.T) {
	t.Parallel()
	for _, snapshot := range []bool{false, true} {
		t.Run(fmt.Sprintf("snapshot=%t", snapshot), func(t *testing.T) {
			t.Parallel()
			db, gate := openGRVHoldDB(t)
			defer gate.release()
			begin, end := fdb.Key(t.Name()+"/a"), fdb.Key(t.Name()+"/z")
			tr, err := db.CreateTransaction()
			if err != nil {
				t.Fatal(err)
			}
			defer tr.Cancel()
			var reader fdb.ReadTransaction = tr
			if snapshot {
				reader = tr.Snapshot()
			}
			gate.arm()
			result := reader.GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{})
			select {
			case <-gate.heldCh:
			case <-time.After(30 * time.Second):
				t.Fatal("GRV not held")
			}
			tr.Cancel()
			scribbleKeyBuf(begin)
			scribbleKeyBuf(end)
			_, err = awaitResult(t, "cancelled range", result.GetSliceWithError)
			var fe fdb.Error
			if !errors.As(err, &fe) || fe.Code != 1025 {
				t.Fatalf("cancelled range = %v, want 1025", err)
			}
			gate.release()
			err = tr.OnError(fdb.Error{Code: 1020}).Get()
			if !errors.As(err, &fe) || fe.Code != 1025 {
				t.Fatalf("cancelled transaction OnError = %v, want 1025", err)
			}
			tr.Reset()
			if err := tr.OnError(fdb.Error{Code: 1020}).Get(); err != nil {
				t.Fatal(err)
			}
			rows, err := tr.GetRange(fdb.KeyRange{Begin: fdb.Key(t.Name() + "/a"), End: fdb.Key(t.Name() + "/z")}, fdb.RangeOptions{}).GetSliceWithError()
			if err != nil || len(rows) != 0 {
				t.Fatalf("retry after cancelled range = %v, %v", rows, err)
			}
		})
	}
}
