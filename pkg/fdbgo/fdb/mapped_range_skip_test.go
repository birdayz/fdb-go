package fdb_test

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

func TestMappedRangeSkipCapability(t *testing.T) {
	t.Parallel()
	if _, ok := any(fdb.Transaction{}).(fdb.MappedRangeSkipReader); !ok {
		t.Fatal("pure-Go transaction lacks initial-selector Skip capability")
	}
	if _, ok := any(fdb.Snapshot{}).(fdb.MappedRangeSkipReader); ok {
		t.Fatal("snapshot unexpectedly exposes serializable mapped Skip")
	}
}

func TestMappedRangeSkipArgumentOwnershipAndRetiredIncarnation(t *testing.T) {
	t.Parallel()
	db, r, mapper := mappedFixture(t, 6)
	tr, err := db.CreateTransaction()
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Cancel()
	begin, end := bytes.Clone(r.Begin.FDBKey()), bytes.Clone(r.End.FDBKey())
	read := tr.GetMappedRangeWithSkip(fdb.KeyRange{Begin: fdb.Key(begin), End: fdb.Key(end)}, mapper,
		fdb.RangeOptions{Mode: fdb.StreamingModeSmall}, 2)
	for _, input := range [][]byte{begin, end, mapper} {
		clear(input)
	}
	rows, err := read.GetSliceWithError()
	if err != nil || len(rows) != 4 {
		t.Fatalf("captured positive Skip rows=%d error=%v, want four", len(rows), err)
	}
	key, err := tuple.Unpack(rows[0].Key)
	if err != nil || key[2] != int64(2) {
		t.Fatalf("captured Skip first key=%v error=%v, want primary 2", key, err)
	}
	tr.Cancel()
	retired := tr.GetMappedRangeWithSkip(r, tuple.Tuple{t.Name(), "REC", "{K[3]}", "{...}"}.Pack(),
		fdb.RangeOptions{Mode: fdb.StreamingModeSmall}, 2)
	tr.Reset()
	_, err = retired.GetSliceWithError()
	var native fdb.Error
	if !errors.As(err, &native) || native.Code != 1025 {
		t.Fatalf("positive Skip migrated across Reset: %v, want 1025", err)
	}
}

func TestMappedRangeSkipMapsOnlyUnskippedRecords(t *testing.T) {
	t.Parallel()
	const count = 12
	db, rng, mapper := mappedFixture(t, count)
	prefix := t.Name()
	for _, reverse := range []bool{false, true} {
		for _, skip := range []int32{-100, -1, 0, 3, count, count + 2, math.MaxInt32 - 1, math.MaxInt32} {
			for _, write := range []string{"none", "skipped_secondary", "skipped_primary"} {
				if skip <= 0 && write != "none" {
					continue
				}
				t.Run(fmt.Sprintf("reverse=%v/skip=%d/write=%s", reverse, skip, write), func(t *testing.T) {
					t.Parallel()
					tr, err := db.CreateTransaction()
					if err != nil {
						t.Fatal(err)
					}
					defer tr.Cancel()
					reader, ok := any(tr).(fdb.MappedRangeSkipReader)
					if !ok {
						t.Fatal("pure-Go transaction lacks initial-selector Skip capability")
					}
					first := 0
					if reverse {
						first = count - 1
					}
					switch write {
					case "skipped_secondary":
						tr.Set(fdb.Key(tuple.Tuple{prefix, "REC", first, 0}.Pack()), []byte("pending-secondary"))
					case "skipped_primary":
						tr.Set(fdb.Key(tuple.Tuple{prefix, "IDX", first, first}.Pack()), []byte("pending-primary"))
					}
					rows, err := reader.GetMappedRangeWithSkip(rng, mapper, fdb.RangeOptions{Limit: 4, Mode: fdb.StreamingModeSmall, Reverse: reverse}, skip).GetSliceWithError()
					if write == "skipped_primary" {
						var native fdb.Error
						if !errors.As(err, &native) || native.Code != 2039 {
							t.Fatalf("skipped primary was still read: error=%v want2039", err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					want := 4
					if skip >= count {
						want = 0
					}
					if len(rows) != want {
						t.Fatalf("returned %d rows want%d", len(rows), want)
					}
					for i, row := range rows {
						key, err := tuple.Unpack(row.Key)
						if err != nil {
							t.Fatal(err)
						}
						want := int(max(skip, 0)) + i
						if reverse {
							want = count - 1 - int(max(skip, 0)) - i
						}
						if key[2] != int64(want) || len(row.GetRange.Rows) != 3 {
							t.Fatalf("row%d key=%v secondary=%d want PK%d", i, key, len(row.GetRange.Rows), want)
						}
					}
				})
			}
		}
	}
}
