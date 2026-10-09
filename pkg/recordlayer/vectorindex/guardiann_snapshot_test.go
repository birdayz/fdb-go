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
	s := guardiannSnapshot{
		clusters: map[tuple.UUID]guardiannClusterMetadata{}, primaries: map[tuple.UUID]int{},
		underrep: map[tuple.UUID]int{},
	}
	count := func(ss subspace.Subspace) []fdb.KeyValue {
		r, err := fdb.PrefixRange(ss.Bytes())
		Expect(err).NotTo(HaveOccurred())
		kvs, err := tx.GetRange(r, fdb.RangeOptions{}).GetSliceWithError()
		Expect(err).NotTo(HaveOccurred())
		return kvs
	}
	for _, kv := range count(g.sub(gSubClusterMetadata)) {
		k, err := g.sub(gSubClusterMetadata).Unpack(kv.Key)
		Expect(err).NotTo(HaveOccurred())
		m, err := g.fetchClusterMetadata(tx, k[0].(tuple.UUID))
		Expect(err).NotTo(HaveOccurred())
		s.clusters[m.id] = *m
	}
	for _, kv := range count(g.sub(gSubVectorRefs)) {
		k, err := g.sub(gSubVectorRefs).Unpack(kv.Key)
		Expect(err).NotTo(HaveOccurred())
		ref, err := vectorRefFromValue(k[1].(tuple.Tuple), kv.Value, decodeGVector)
		Expect(err).NotTo(HaveOccurred())
		if ref.primary {
			s.primaries[k[0].(tuple.UUID)]++
		}
		if ref.isUnderreplicated() {
			s.underrep[k[0].(tuple.UUID)]++
		}
	}
	s.tasks = len(count(g.sub(gSubTasks)))
	s.centroids = len(count(g.centroids.storage.dataSubspace.Sub(int64(0))))
	s.vectors = len(count(g.sub(gSubVectorMetadata)))
	s.collapsed = len(count(g.sub(gSubCollapsed)))
	return s
}
