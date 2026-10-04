package recordlayer

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// Stored GuardiANN values are read through checked accessors: a malformed or
// foreign value is a RecordCoreError, as Java's Tuple getters throw, never a
// process panic.
func TestGuardiannDecodersRefuseMalformedValues(t *testing.T) {
	t.Parallel()
	id := tuple.UUID{1}
	stats := tuple.Tuple{int64(1), 0.5, 0.25, 2.0}
	decode := func(b []byte) (gVector, error) { return decodeGVector(b) }
	vector := serializeVector([]float64{1, 2})
	ref := tuple.Tuple{id, int64(roleCodeReplicated), false, vector, 0.5}

	cases := []struct {
		name string
		run  func() error
		want string
	}{
		{"cluster metadata, four fields (an intermediate GuardiANN build)", func() error {
			_, err := clusterMetadataFromValue(id, tuple.Tuple{int64(1), int64(2), stats, int64(0)}.Pack())
			return err
		}, "has 4 elements, want 5"},
		{"cluster metadata, a wrong element type", func() error {
			_, err := clusterMetadataFromValue(id, tuple.Tuple{"x", int64(2), stats, int64(0), int64(1)}.Pack())
			return err
		}, "element 0"},
		{"cluster metadata, short running stats", func() error {
			_, err := clusterMetadataFromValue(id, tuple.Tuple{int64(1), int64(2), tuple.Tuple{int64(1)}, int64(0), int64(1)}.Pack())
			return err
		}, "running stats"},
		{"cluster metadata, an int above Java's int range", func() error {
			_, err := clusterMetadataFromValue(id, tuple.Tuple{int64(1) << 40, int64(2), stats, int64(0), int64(1)}.Pack())
			return err
		}, "int range"},
		{"access info, empty", func() error {
			_, err := accessInfoFromValue(tuple.Tuple{}.Pack())
			return err
		}, "element 0"},
		{"access info, centroid not bytes", func() error {
			_, err := accessInfoFromValue(tuple.Tuple{int64(1), tuple.Tuple{int64(3)}}.Pack())
			return err
		}, "element 0"},
		{"vector metadata, uuid missing", func() error {
			_, err := vectorMetadataFromValue(tuple.Tuple{int64(1)}, tuple.Tuple{int64(3)}.Pack())
			return err
		}, "element 0"},
		{"vector reference, truncated", func() error {
			_, err := vectorRefFromValue(tuple.Tuple{int64(1)}, ref[:3].Pack(), decode)
			return err
		}, "element 3"},
		{"vector reference, replicated without a priority", func() error {
			_, err := vectorRefFromValue(tuple.Tuple{int64(1)}, ref[:4].Pack(), decode)
			return err
		}, "element 4"},
		{"vector reference, collapsed not a bool", func() error {
			bad := append(tuple.Tuple{}, ref...)
			bad[2] = int64(1)
			_, err := vectorRefFromValue(tuple.Tuple{int64(1)}, bad.Pack(), decode)
			return err
		}, "element 2"},
		{"collapsed id, not a uuid", func() error {
			_, err := collapsedIDFromValue(tuple.Tuple{int64(1)}, tuple.Tuple{"x"}.Pack())
			return err
		}, "element 0"},
		{"cluster reference, short", func() error {
			_, err := clusterRefFromTuple(tuple.Tuple{id}, decode)
			return err
		}, "element 1"},
		{"uuid set, not uuids", func() error {
			_, err := uuidSetFromTuple(tuple.Tuple{id, int64(1)})
			return err
		}, "element 1"},
		{"task, no kind", func() error {
			_, err := taskFromTuples(tuple.Tuple{id}, tuple.Tuple{}, decode)
			return err
		}, "element 0"},
		{"task, key not a uuid", func() error {
			_, err := taskFromTuples(tuple.Tuple{int64(1)}, tuple.Tuple{int64(taskCollapse), id, vector}, decode)
			return err
		}, "element 0"},
		{"task, split/merge without its nearest clusters", func() error {
			_, err := taskFromTuples(tuple.Tuple{id}, tuple.Tuple{int64(taskSplitMerge), id, vector}, decode)
			return err
		}, "element 3"},
		{"task, nearest entry not a tuple", func() error {
			_, err := taskFromTuples(tuple.Tuple{id}, tuple.Tuple{int64(taskSplitMerge), id, vector, tuple.Tuple{int64(1)}}, decode)
			return err
		}, "element 0"},
		{"task, bounce with an unknown final kind", func() error {
			_, err := taskFromTuples(tuple.Tuple{id}, tuple.Tuple{int64(taskBounce), tuple.Tuple{}, tuple.Tuple{}, "NOPE"}, decode)
			return err
		}, "NOPE"},
		{"merge lock, owner not a uuid", func() error {
			_, _, err := mergeLockFromValue(tuple.Tuple{"x", int64(1)}.Pack())
			return err
		}, "element 0"},
		{"merge lock, timestamp missing", func() error {
			_, _, err := mergeLockFromValue(tuple.Tuple{id}.Pack())
			return err
		}, "element 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("panicked: %v", r)
					}
				}()
				err = c.run()
			}()
			var rce *RecordCoreError
			if !errors.As(err, &rce) {
				t.Fatalf("err = %v (%T), want a RecordCoreError", err, err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %q, want it to mention %q", err, c.want)
			}
		})
	}
}

// The well-formed values every decoder reads back are the ones the writers emit.
func TestGuardiannDecodersRoundTrip(t *testing.T) {
	t.Parallel()
	id := tuple.UUID{7}
	md := guardiannClusterMetadata{id: id, numUnderrep: 1, numReplicated: 2, stats: guardiannRunningStats{n: 3, mean: 0.5, m2: 0.25, maxEver: 4}, states: 5, maxEverPrimary: 6}
	got, err := clusterMetadataFromValue(id, clusterMetaValue(md))
	if err != nil || *got != md {
		t.Fatalf("cluster metadata = %+v, %v; want %+v", got, err, md)
	}
	owner, ts, err := mergeLockFromValue(tuple.Tuple{id, int64(42)}.Pack())
	if err != nil || owner != id || ts != 42 {
		t.Fatalf("merge lock = %v %d %v", owner, ts, err)
	}
	ids, err := uuidSetFromTuple(tuple.Tuple{id, tuple.UUID{8}})
	if err != nil || len(ids) != 2 || ids[1] != (tuple.UUID{8}) {
		t.Fatalf("uuid set = %v, %v", ids, err)
	}
}
