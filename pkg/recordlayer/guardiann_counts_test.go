package recordlayer

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/vectorcodec"
)

// A cluster's underreplicated count is the number of its physical
// underreplicated primaries, and reference dedup keeps a live primary in
// either encounter order. Java never decrements the count for a deleted
// underreplicated primary, drops an underreplication-only delta, and replaces
// an earlier primary with a later replica (RFC-257 WS-D declared (h)).
var _ = Describe("GuardiANN reference counts", func() {
	ctx := context.Background()
	cfg := defaultGuardiannConfig(2)
	cfg.primaryClusterMin, cfg.primaryClusterMax, cfg.primaryClusterHardMax = 1, 12, 20
	cfg.deterministicRandomness = true
	vector := func(i int64) gVector { return gVector{data: []float64{float64(i), 1}, typ: vectorcodec.TypeDouble} }
	runner := func(name string) func(func(g *guardiann, tx fdb.WritableTransaction) error) {
		return func(f func(g *guardiann, tx fdb.WritableTransaction) error) {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				return nil, f(newGuardiann(specSubspace().Sub(name), cfg, nil, nil), rtx.Transaction())
			})
			Expect(err).NotTo(HaveOccurred())
		}
	}
	lone := func(g *guardiann, tx fdb.WritableTransaction) guardiannClusterMetadata {
		snap := readGuardiann(tx, g)
		Expect(snap.clusters).To(HaveLen(1))
		for id, m := range snap.clusters {
			Expect(m.numUnderrep).To(Equal(snap.underrep[id]), "the count is the physical underreplicated primaries")
			return m
		}
		return guardiannClusterMetadata{}
	}

	It("decrements the underreplicated count only for an underreplicated primary's delete", func() {
		run := runner("underrep-delete")
		run(func(g *guardiann, tx fdb.WritableTransaction) error {
			for i := int64(0); i < 5; i++ {
				if err := g.insert(tx, tuple.Tuple{i}, vector(i), nil, false); err != nil {
					return err
				}
			}
			return nil
		})
		run(func(g *guardiann, tx fdb.WritableTransaction) error {
			m := lone(g, tx)
			ref, err := g.fetchVectorRef(tx, m.id, tuple.Tuple{int64(0)})
			Expect(err).NotTo(HaveOccurred())
			Expect(g.writeVectorRef(tx, m.id, ref.toPrimaryUnderreplicated())).To(Succeed())
			m.numUnderrep = 1
			g.writeClusterMetadata(tx, m)
			return nil
		})
		run(func(g *guardiann, tx fdb.WritableTransaction) error {
			return g.delete(tx, tuple.Tuple{int64(1)}, vector(1), false)
		})
		run(func(g *guardiann, tx fdb.WritableTransaction) error {
			m := lone(g, tx)
			Expect(m.numPrimary()).To(Equal(4))
			Expect(m.numUnderrep).To(Equal(1), "another primary's delete leaves underreplication unchanged")
			return nil
		})
		run(func(g *guardiann, tx fdb.WritableTransaction) error {
			return g.delete(tx, tuple.Tuple{int64(0)}, vector(0), false)
		})
		run(func(g *guardiann, tx fdb.WritableTransaction) error {
			m := lone(g, tx)
			Expect(m.numPrimary()).To(Equal(3))
			Expect(m.numUnderrep).To(Equal(0))
			return nil
		})
	})

	It("persists an underreplication-only delta", func() {
		run := runner("underrep-only")
		run(func(g *guardiann, tx fdb.WritableTransaction) error {
			return g.insert(tx, tuple.Tuple{int64(0)}, vector(0), nil, false)
		})
		pk := tuple.Tuple{int64(0)}
		run(func(g *guardiann, tx fdb.WritableTransaction) error {
			m := lone(g, tx)
			ref, err := g.fetchVectorRef(tx, m.id, pk)
			Expect(err).NotTo(HaveOccurred())
			Expect(g.writeVectorRef(tx, m.id, ref.toPrimaryUnderreplicated())).To(Succeed())
			m.numUnderrep = 1
			g.writeClusterMetadata(tx, m)
			return nil
		})
		// The primary is replicated again: an underreplication-only change.
		run(func(g *guardiann, tx fdb.WritableTransaction) error {
			m := lone(g, tx)
			Expect(m.numUnderrep).To(Equal(1))
			ref, err := g.fetchVectorRef(tx, m.id, pk)
			Expect(err).NotTo(HaveOccurred())
			Expect(g.writeVectorRef(tx, m.id, ref.toPrimary())).To(Succeed())
			_, err = g.updateAndEnqueueReassign(tx, newSplittableRandomForUUID(tuple.UUID{1}), m, vector(0), 0, -1, 0, m.stats, nil)
			return err
		})
		run(func(g *guardiann, tx fdb.WritableTransaction) error {
			Expect(lone(g, tx).numUnderrep).To(Equal(0))
			return nil
		})
	})

	It("keeps the primary when a replica of the same vector follows it", func() {
		run := runner("primary-preferred")
		run(func(g *guardiann, tx fdb.WritableTransaction) error {
			id := guardiannVectorID{pk: tuple.Tuple{int64(7)}, uuid: tuple.UUID{7}}
			g.writeVectorMetadata(tx, guardiannVectorMetadata{id: id})
			primary := guardiannVectorRef{id: id, vector: vector(7), primary: true}
			replica := primary.toReplicated(0.5)
			for _, order := range [][]guardiannCluster{
				{{refs: []guardiannVectorRef{primary}}, {refs: []guardiannVectorRef{replica}}},
				{{refs: []guardiannVectorRef{replica}}, {refs: []guardiannVectorRef{primary}}},
			} {
				refs, err := g.cleanUpVectorReferences(tx, order, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(refs).To(HaveLen(1))
				Expect(refs[0].primary).To(BeTrue())
			}
			return nil
		})
	})
})
