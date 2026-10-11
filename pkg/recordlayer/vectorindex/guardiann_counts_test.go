package vectorindex

import (
	"context"
	"errors"

	"fdb.dev/pkg/recordlayer"

	. "github.com/onsi/ginkgo/v2"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/vectorcodec"
	. "github.com/onsi/gomega"
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
	runner := func(name string) func(func(g *guardiann, tx fdb.WritableTransaction) error) guardiannSnapshot {
		return func(f func(g *guardiann, tx fdb.WritableTransaction) error) guardiannSnapshot {
			db := recordlayer.NewFDBDatabaseWithTransactor(sharedRawDB, sharedRawDB)
			attempts, successfulBodies, committed := 0, 0, 0
			tooOld, conflicts := 0, 0
			db.SetAttemptObserver(func(_ recordlayer.AttemptCall, err error) {
				if err == nil {
					committed++
				}
				var fe fdb.Error
				if errors.As(err, &fe) {
					switch fe.Code {
					case 1007:
						tooOld++
					case 1020:
						conflicts++
					}
				}
			})
			fired := false
			result, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				attempts++
				tx := guardiannCountsRetryTx{WritableTransaction: rtx.Transaction(), fired: &fired}
				g := newGuardiann(specSubspace().Sub(name), cfg, nil, nil)
				if err := f(g, tx); err != nil {
					return nil, err
				}
				snap, err := readGuardiannErr(tx, g)
				if err != nil {
					return nil, err
				}
				successfulBodies++
				if successfulBodies == 1 {
					return nil, fdb.Error{Code: 1020}
				}
				return snap, nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(fired).To(BeTrue())
			Expect(tooOld).To(BeNumerically(">=", 1))
			Expect(conflicts).To(BeNumerically(">=", 1))
			Expect(attempts).To(BeNumerically(">=", 3))
			Expect(successfulBodies).To(BeNumerically(">=", 2))
			Expect(committed).To(Equal(1))
			return result.(guardiannSnapshot)
		}
	}
	lone := func(snap guardiannSnapshot) guardiannClusterMetadata {
		Expect(snap.clusters).To(HaveLen(1))
		for id, m := range snap.clusters {
			Expect(m.numUnderrep).To(Equal(snap.underrep[id]), "the count is the physical underreplicated primaries")
			return m
		}
		return guardiannClusterMetadata{}
	}

	It("decrements the underreplicated count only for an underreplicated primary's delete", func() {
		run := runner("underrep-delete")
		m := lone(run(func(g *guardiann, tx fdb.WritableTransaction) error {
			for i := int64(0); i < 5; i++ {
				if err := g.insert(tx, tuple.Tuple{i}, vector(i), nil, false); err != nil {
					return err
				}
			}
			return nil
		}))
		m = lone(run(func(g *guardiann, tx fdb.WritableTransaction) error {
			ref, err := g.fetchVectorRef(tx, m.id, tuple.Tuple{int64(0)})
			if err != nil {
				return err
			}
			if err := g.writeVectorRef(tx, m.id, ref.toPrimaryUnderreplicated()); err != nil {
				return err
			}
			updated := m
			updated.numUnderrep = 1
			g.writeClusterMetadata(tx, updated)
			return nil
		}))
		Expect(m.numUnderrep).To(Equal(1))
		m = lone(run(func(g *guardiann, tx fdb.WritableTransaction) error {
			return g.delete(tx, tuple.Tuple{int64(1)}, vector(1), false)
		}))
		Expect(m.numPrimary()).To(Equal(4))
		Expect(m.numUnderrep).To(Equal(1), "another primary's delete leaves underreplication unchanged")
		m = lone(run(func(g *guardiann, tx fdb.WritableTransaction) error {
			return g.delete(tx, tuple.Tuple{int64(0)}, vector(0), false)
		}))
		Expect(m.numPrimary()).To(Equal(3))
		Expect(m.numUnderrep).To(Equal(0))
	})

	It("persists an underreplication-only delta", func() {
		run := runner("underrep-only")
		m := lone(run(func(g *guardiann, tx fdb.WritableTransaction) error {
			return g.insert(tx, tuple.Tuple{int64(0)}, vector(0), nil, false)
		}))
		pk := tuple.Tuple{int64(0)}
		m = lone(run(func(g *guardiann, tx fdb.WritableTransaction) error {
			ref, err := g.fetchVectorRef(tx, m.id, pk)
			if err != nil {
				return err
			}
			if err := g.writeVectorRef(tx, m.id, ref.toPrimaryUnderreplicated()); err != nil {
				return err
			}
			updated := m
			updated.numUnderrep = 1
			g.writeClusterMetadata(tx, updated)
			return nil
		}))
		Expect(m.numUnderrep).To(Equal(1))
		// The primary is replicated again: an underreplication-only change.
		m = lone(run(func(g *guardiann, tx fdb.WritableTransaction) error {
			ref, err := g.fetchVectorRef(tx, m.id, pk)
			if err != nil {
				return err
			}
			if err := g.writeVectorRef(tx, m.id, ref.toPrimary()); err != nil {
				return err
			}
			_, err = g.updateAndEnqueueReassign(tx, newSplittableRandomForUUID(tuple.UUID{1}), m, vector(0), 0, -1, 0, m.stats, nil)
			return err
		}))
		Expect(m.numUnderrep).To(Equal(0))
	})

	It("keeps the primary when a replica of the same vector follows it", func() {
		run := runner("primary-preferred")
		var got [][]guardiannVectorRef
		run(func(g *guardiann, tx fdb.WritableTransaction) error {
			got = nil
			id := guardiannVectorID{pk: tuple.Tuple{int64(7)}, uuid: tuple.UUID{7}}
			g.writeVectorMetadata(tx, guardiannVectorMetadata{id: id})
			primary := guardiannVectorRef{id: id, vector: vector(7), primary: true}
			replica := primary.toReplicated(0.5)
			for _, order := range [][]guardiannCluster{
				{{refs: []guardiannVectorRef{primary}}, {refs: []guardiannVectorRef{replica}}},
				{{refs: []guardiannVectorRef{replica}}, {refs: []guardiannVectorRef{primary}}},
			} {
				refs, err := g.cleanUpVectorReferences(tx, order, false)
				if err != nil {
					return err
				}
				got = append(got, refs)
			}
			return nil
		})
		Expect(got).To(HaveLen(2))
		for _, refs := range got {
			Expect(refs).To(HaveLen(1))
			Expect(refs[0].primary).To(BeTrue())
		}
	})
})

// Read errors must reach Run; a successful body is also replayed before commit.
type guardiannCountsRetryTx struct {
	fdb.WritableTransaction
	fired *bool
}

func (t guardiannCountsRetryTx) Get(key fdb.KeyConvertible) fdb.FutureByteSlice {
	return (tooOldOnceReadTx{ReadTransaction: t.WritableTransaction, fired: t.fired}).Get(key)
}
