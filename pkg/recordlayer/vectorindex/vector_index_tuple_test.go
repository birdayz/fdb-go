package vectorindex

import (
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// TestTupleToVector pins the error contract for the SPFresh index write path
// (FDB C++ review of PR #272, finding C1): an absent/null vector is skipped, but a
// NON-null but UNDECODABLE vector must return an ERROR — Java's RealVector.fromBytes
// throws and fails the write. Pre-fix tupleToVector returned nil for an undecodable
// vector too, so the maintainer silently skipped it (`if vector == nil { continue }`),
// saving the record UNINDEXED — a vector search would miss the row. Surfacing the
// error makes the write fail, matching Java and avoiding silent index incompleteness.
func TestTupleToVector(t *testing.T) {
	t.Parallel()

	t.Run("valid-numeric", func(t *testing.T) {
		t.Parallel()
		v, err := tupleToVector(tuple.Tuple{float64(1), float64(2), float64(3)})
		if err != nil {
			t.Fatalf("valid numeric vector: unexpected error %v", err)
		}
		if len(v) != 3 || v[0] != 1 || v[2] != 3 {
			t.Fatalf("valid numeric vector: got %v, want [1 2 3]", v)
		}
	})

	t.Run("absent-empty-skips", func(t *testing.T) {
		t.Parallel()
		v, err := tupleToVector(tuple.Tuple{})
		if err != nil || v != nil {
			t.Fatalf("empty tuple: got (%v, %v), want (nil, nil) — an absent vector is skipped", v, err)
		}
	})

	t.Run("null-component-skips", func(t *testing.T) {
		t.Parallel()
		v, err := tupleToVector(tuple.Tuple{nil})
		if err != nil || v != nil {
			t.Fatalf("null element: got (%v, %v), want (nil, nil) — a null vector is skipped (matches Java)", v, err)
		}
	})

	t.Run("undecodable-bytes-errors", func(t *testing.T) {
		t.Parallel()
		// A non-null but garbage serialized vector. Pre-fix this returned (nil, nil) and
		// the record was saved unindexed; now it must error (Java throws).
		v, err := tupleToVector(tuple.Tuple{[]byte{0xff, 0x01, 0x02}})
		if err == nil {
			t.Fatalf("undecodable serialized vector: want error, got (%v, nil) — would save unindexed", v)
		}
	})

	t.Run("truncated-bytes-errors", func(t *testing.T) {
		t.Parallel()
		// A DOUBLE vector with seven trailing bytes: Java's RealVector.fromBytes
		// sizes it as length>>3 = 2 components and underflows reading the
		// second (DoubleRealVector.java:303), failing the write
		// (VectorIndexMaintainer.java:395). Pre-fix Go floored it to one
		// component and indexed the truncated vector.
		data := append(serializeVector([]float64{1}), 0, 0, 0, 0, 0, 0, 0)
		v, err := tupleToVector(tuple.Tuple{data})
		if err == nil {
			t.Fatalf("truncated serialized vector: want error, got (%v, nil) — would index a truncated vector", v)
		}
	})

	t.Run("non-numeric-errors", func(t *testing.T) {
		t.Parallel()
		v, err := tupleToVector(tuple.Tuple{"not a number"})
		if err == nil {
			t.Fatalf("non-numeric element: want error, got (%v, nil)", v)
		}
	})
}

// TestEntryVector pins the HNSW and GuardiANN read, Java's updateIndexEntry:
// value column 0 as bytes only (Tuple.getBytes), then RealVector.fromBytes.
func TestEntryVector(t *testing.T) {
	t.Parallel()
	entry := func(value ...tuple.TupleElement) recordlayer.EvaluatedIndexEntry {
		return recordlayer.NewEvaluatedIndexEntry(tuple.Tuple{}, tuple.Tuple(value), tuple.Tuple{int64(1)})
	}
	vec := serializeVector([]float64{1, 2})
	var cce *recordlayer.ClassCastError
	var oob *recordlayer.IndexOutOfBoundsError
	for _, c := range []struct {
		name  string
		entry recordlayer.EvaluatedIndexEntry
		check func(v []float64, err error) bool
	}{
		{"a bytes vector", entry(vec), func(v []float64, err error) bool { return err == nil && len(v) == 2 && v[1] == 2 }},
		{"later value columns ignored", entry(vec, int64(7), vec), func(v []float64, err error) bool { return err == nil && len(v) == 2 }},
		{"a null vector skips", entry(nil, int64(7)), func(v []float64, err error) bool { return err == nil && v == nil }},
		{"a numeric column", entry(float64(1), float64(2)), func(_ []float64, err error) bool { return errors.As(err, &cce) }},
		{"a numeric column before bytes", entry(int64(1), vec), func(_ []float64, err error) bool { return errors.As(err, &cce) }},
		{"no value column", entry(), func(_ []float64, err error) bool { return errors.As(err, &oob) }},
		{"empty bytes", entry([]byte{}), func(_ []float64, err error) bool { return errors.As(err, &oob) }},
		{"a nil []byte is empty bytes, not null", entry([]byte(nil)), func(_ []float64, err error) bool { return errors.As(err, &oob) }},
		{"type ordinal 4", entry([]byte{4, 0}), func(_ []float64, err error) bool { return errors.As(err, &oob) }},
		{"a negative type ordinal", entry([]byte{0xff, 1, 2}), func(_ []float64, err error) bool { return errors.As(err, &oob) }},
		{"a RaBitQ ordinal", entry([]byte{3, 0}), func(_ []float64, err error) bool { return err != nil && !errors.As(err, &oob) }},
		{"a truncated payload", entry(append(serializeVector([]float64{1}), 0, 0, 0, 0, 0, 0, 0)), func(_ []float64, err error) bool { return err != nil }},
	} {
		v, _, err := entryVector(c.entry)
		if !c.check(v, err) {
			t.Errorf("%s: got (%v, %T %v)", c.name, v, err, err)
		}
	}
	// A queued entry is packed and unpacked: empty bytes stay a refusal.
	unpacked, err := tuple.Unpack(tuple.Tuple{[]byte{}}.Pack())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := entryVector(recordlayer.NewEvaluatedIndexEntry(tuple.Tuple{}, unpacked, tuple.Tuple{int64(1)})); !errors.As(err, &oob) {
		t.Errorf("round-tripped empty vector bytes: got %T %v, want IndexOutOfBoundsError", err, err)
	}
}
