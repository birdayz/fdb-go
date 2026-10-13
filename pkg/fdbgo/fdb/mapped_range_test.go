package fdb_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

func TestMappedRangeCapability(t *testing.T) {
	t.Parallel()
	if _, ok := any(fdb.Transaction{}).(fdb.MappedRangeReader); !ok {
		t.Fatal("pure-Go transaction lacks mapped-range capability")
	}
	if _, ok := any(fdb.Snapshot{}).(fdb.MappedRangeReader); ok {
		t.Fatal("snapshot unexpectedly exposes serializable mapped-range reads")
	}
}

func mappedFixture(t *testing.T, n int) (fdb.Database, fdb.KeyRange, []byte) {
	t.Helper()
	db := openTestDB(t)
	prefix := t.Name()
	_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		for i := 0; i < n; i++ {
			tr.Set(fdb.Key(tuple.Tuple{prefix, "IDX", i, i}.Pack()), []byte(fmt.Sprint(i)))
			for j := 0; j < 3; j++ {
				tr.Set(fdb.Key(tuple.Tuple{prefix, "REC", i, j}.Pack()), bytes.Repeat([]byte{byte(i), byte(j)}, 512))
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := fdb.PrefixRange(tuple.Tuple{prefix, "IDX"}.Pack())
	if err != nil {
		t.Fatal(err)
	}
	return db, r, tuple.Tuple{prefix, "REC", "{K[3]}", "{...}"}.Pack()
}

func TestMappedRangeIteratorLimitsAndOwnership(t *testing.T) {
	t.Parallel()
	const n = 96
	db, r, mapper := mappedFixture(t, n)
	prefix := t.Name()
	for _, reverse := range []bool{false, true} {
		for _, mode := range []fdb.StreamingMode{fdb.StreamingModeSmall, fdb.StreamingModeIterator, fdb.StreamingModeExact, fdb.StreamingModeWantAll} {
			t.Run(fmt.Sprintf("reverse=%v/mode=%d", reverse, mode), func(t *testing.T) {
				t.Parallel()
				tr, err := db.CreateTransaction()
				if err != nil {
					t.Fatal(err)
				}
				defer tr.Cancel()
				const limit = 57
				rr := tr.GetMappedRange(r, mapper, fdb.RangeOptions{Mode: mode, Limit: limit, Reverse: reverse})
				it := rr.Iterator()
				before, err := it.Get()
				if err != nil || before.Key != nil {
					t.Fatalf("before advance: %v %v", before, err)
				}
				var batches, returned, requested int
				it.SetTraceLog(func(iteration, budget, count int, more bool, err error) {
					if iteration != batches+1 || budget != limit-returned || err != nil {
						t.Errorf("batch %d/%d budget=%d want=%d: %v", iteration, batches+1, budget, limit-returned, err)
					}
					batches++
					returned += count
					requested = budget
				})
				var retained fdb.MappedKeyValue
				var savedKey, savedValue []byte
				count := 0
				for it.Advance() {
					row := it.MustGet()
					want := count
					if reverse {
						want = n - 1 - count
					}
					if !bytes.Equal(row.Key, tuple.Tuple{prefix, "IDX", want, want}.Pack()) {
						t.Fatalf("primary row %d: key=%x want index=%d", count, row.Key, want)
					}
					if row.Kind != fdb.MappedResultGetRange || len(row.GetRange.Rows) != 3 || row.GetRange.More {
						t.Fatalf("secondary row %d: %+v", count, row)
					}
					for j, split := range row.GetRange.Rows {
						if !bytes.Equal(split.Value, bytes.Repeat([]byte{byte(want), byte(j)}, 512)) {
							t.Fatalf("secondary order/value at row %d split %d", count, j)
						}
					}
					if count == 0 {
						retained = row
						savedKey, savedValue = bytes.Clone(row.Key), bytes.Clone(row.GetRange.Rows[0].Value)
					}
					if again := it.MustGet(); !bytes.Equal(again.Key, row.Key) {
						t.Fatal("Get advanced the iterator")
					}
					count++
				}
				if _, err := it.Get(); err != nil {
					t.Fatal(err)
				}
				if count != limit || returned != limit || requested <= 0 || batches == 0 {
					t.Fatalf("rows=%d returned=%d requested=%d batches=%d", count, returned, requested, batches)
				}
				if mode == fdb.StreamingModeSmall && batches <= 1 {
					t.Fatal("SMALL did not cross a mapped batch boundary")
				}
				if mode == fdb.StreamingModeExact && batches != 1 {
					t.Fatalf("EXACT batches=%d want=1", batches)
				}
				if !bytes.Equal(retained.Key, savedKey) || !bytes.Equal(retained.GetRange.Rows[0].Value, savedValue) {
					t.Fatal("a later batch overwrote retained mapped bytes")
				}
				rows, err := rr.GetSliceWithError()
				if err != nil || len(rows) != limit {
					t.Fatalf("slice rows=%d: %v", len(rows), err)
				}
			})
		}
	}
}

func TestMappedRangeArgumentOwnershipAndReset(t *testing.T) {
	t.Parallel()
	db, r, mapper := mappedFixture(t, 3)
	tr, err := db.CreateTransaction()
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Cancel()
	begin, end := bytes.Clone(r.Begin.FDBKey()), bytes.Clone(r.End.FDBKey())
	rr := tr.GetMappedRange(fdb.KeyRange{Begin: fdb.Key(begin), End: fdb.Key(end)}, mapper, fdb.RangeOptions{Mode: fdb.StreamingModeSmall})
	for _, b := range [][]byte{begin, end, mapper} {
		clear(b)
	}
	if rows := rr.GetSliceOrPanic(); len(rows) != 3 {
		t.Fatalf("caller argument reuse changed read: rows=%d", len(rows))
	}
	tr.Cancel()
	retired := tr.GetMappedRange(r, tuple.Tuple{t.Name(), "REC", "{K[3]}", "{...}"}.Pack(), fdb.RangeOptions{})
	tr.Reset()
	_, err = retired.GetSliceWithError()
	var ferr fdb.Error
	if !errors.As(err, &ferr) || ferr.Code != 1025 {
		t.Fatalf("pending read migrated across reset: %v", err)
	}
}

// A caught mapped RYW error still leaves the unmodified prefix in the conflict
// map (ReadYourWrites.actor.cpp:335-350). Do not read the probe key separately.
func TestMappedRangeRYWErrorPreservesConflictPrefix(t *testing.T) {
	t.Parallel()
	for _, secondary := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			for _, before := range []bool{false, true} {
				t.Run(fmt.Sprintf("secondary=%v/reverse=%v/before=%v", secondary, reverse, before), func(t *testing.T) {
					t.Parallel()
					db, r, mapper := mappedFixture(t, 3)
					pending := tuple.Tuple{t.Name(), "IDX", 1, 1}.Pack()
					probe := tuple.Tuple{t.Name(), "IDX", 2, 2}.Pack()
					if before {
						probe = tuple.Tuple{t.Name(), "IDX", 0, 0}.Pack()
					}
					if secondary {
						pending = tuple.Tuple{t.Name(), "REC", 0, 1}.Pack()
						probe = tuple.Tuple{t.Name(), "REC", 0, 2}.Pack()
						if before {
							probe = tuple.Tuple{t.Name(), "REC", 0, 0}.Pack()
						}
					}
					tr, err := db.CreateTransaction()
					if err != nil {
						t.Fatal(err)
					}
					defer tr.Cancel()
					tr.Set(fdb.Key(pending), []byte("pending"))
					_, err = tr.GetMappedRange(r, mapper, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll, Reverse: reverse}).GetSliceWithError()
					var fe fdb.Error
					if !errors.As(err, &fe) || fe.Code != 2039 {
						t.Fatalf("mapped read: %v, want 2039", err)
					}
					_, err = db.Transact(func(writer fdb.WritableTransaction) (any, error) {
						writer.Set(fdb.Key(probe), []byte("external"))
						return nil, nil
					})
					if err != nil {
						t.Fatal(err)
					}
					want := 0
					if before {
						want = 1020
					}
					code := 0
					if err := tr.Commit().Get(); err != nil {
						if !errors.As(err, &fe) {
							t.Fatal(err)
						}
						code = fe.Code
					}
					if code != want {
						t.Fatalf("caught 2039 then external prefix write: commit=%d, want %d", code, want)
					}
				})
			}
		}
	}
}

func TestMappedRangeErrors(t *testing.T) {
	t.Parallel()
	db, r, mapper := mappedFixture(t, 1)
	for _, tc := range []struct {
		name string
		opts fdb.RangeOptions
		code int
	}{
		{"exact_zero", fdb.RangeOptions{Mode: fdb.StreamingModeExact}, 2210},
		{"exact_unlimited", fdb.RangeOptions{Mode: fdb.StreamingModeExact, Limit: -1}, 2210},
		{"invalid_mode", fdb.RangeOptions{Mode: fdb.StreamingMode(99)}, 2000},
		{"invalid_limit", fdb.RangeOptions{Limit: -2}, 2012},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr, err := db.CreateTransaction()
			if err != nil {
				t.Fatal(err)
			}
			defer tr.Cancel()
			rr := tr.GetMappedRange(r, mapper, tc.opts)
			it := rr.Iterator()
			if it.Advance() {
				t.Fatal("invalid read yielded a row")
			}
			_, err = it.Get()
			var ferr fdb.Error
			if !errors.As(err, &ferr) || ferr.Code != tc.code {
				t.Fatalf("error=%v want code=%d", err, tc.code)
			}
			if _, err = rr.GetSliceWithError(); !errors.As(err, &ferr) || ferr.Code != tc.code {
				t.Fatalf("slice error=%v want code=%d", err, tc.code)
			}
		})
	}
}
