package vectorindex

import (
	. "github.com/onsi/gomega"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// guardiannSnapshot reads the structure's bookkeeping back.
type guardiannSnapshot struct {
	clusters  map[tuple.UUID]guardiannClusterMetadata
	primaries map[tuple.UUID]int // primary references per cluster
	underrep  map[tuple.UUID]int // underreplicated primary references per cluster
	tasks     int
	centroids int
	vectors   int
	collapsed int
}

func readGuardiann(tx fdb.ReadTransaction, g *guardiann) guardiannSnapshot {
	s, err := readGuardiannErr(tx, g)
	Expect(err).NotTo(HaveOccurred())
	return s
}

// readGuardiannErr returns read errors instead of asserting, for callers inside
// a closure that Run may retry.
func readGuardiannErr(tx fdb.ReadTransaction, g *guardiann) (guardiannSnapshot, error) {
	s := guardiannSnapshot{
		clusters: map[tuple.UUID]guardiannClusterMetadata{}, primaries: map[tuple.UUID]int{},
		underrep: map[tuple.UUID]int{},
	}
	count := func(ss subspace.Subspace) ([]fdb.KeyValue, error) {
		r, err := fdb.PrefixRange(ss.Bytes())
		if err != nil {
			return nil, err
		}
		return tx.GetRange(r, fdb.RangeOptions{}).GetSliceWithError()
	}
	kvs, err := count(g.sub(gSubClusterMetadata))
	if err != nil {
		return s, err
	}
	for _, kv := range kvs {
		k, err := g.sub(gSubClusterMetadata).Unpack(kv.Key)
		if err != nil {
			return s, err
		}
		m, err := g.fetchClusterMetadata(tx, k[0].(tuple.UUID))
		if err != nil {
			return s, err
		}
		s.clusters[m.id] = *m
	}
	if kvs, err = count(g.sub(gSubVectorRefs)); err != nil {
		return s, err
	}
	for _, kv := range kvs {
		k, err := g.sub(gSubVectorRefs).Unpack(kv.Key)
		if err != nil {
			return s, err
		}
		ref, err := vectorRefFromValue(k[1].(tuple.Tuple), kv.Value, decodeGVector)
		if err != nil {
			return s, err
		}
		if ref.primary {
			s.primaries[k[0].(tuple.UUID)]++
		}
		if ref.isUnderreplicated() {
			s.underrep[k[0].(tuple.UUID)]++
		}
	}
	for _, c := range []struct {
		ss  subspace.Subspace
		out *int
	}{
		{g.sub(gSubTasks), &s.tasks},
		{g.centroids.storage.dataSubspace.Sub(int64(0)), &s.centroids},
		{g.sub(gSubVectorMetadata), &s.vectors},
		{g.sub(gSubCollapsed), &s.collapsed},
	} {
		if kvs, err = count(c.ss); err != nil {
			return s, err
		}
		*c.out = len(kvs)
	}
	return s, nil
}
