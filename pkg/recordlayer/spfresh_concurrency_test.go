package recordlayer

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/vectorcodec"
)

// These two specs close the dimensional coverage gaps the adversarial bug-hunt
// flagged: the existing churn test (spfresh_churn_test.go) gives each writer a
// DISJOINT pk range and runs a SINGLE rebalancer goroutine, so neither the
// same-pk membership serialization fence nor cross-executor lease exclusion is
// stressed end-to-end — yet both are the load-bearing paths for the
// lost-update and orphaned-entry bug classes on the default deployment shape
// (concurrent writers + ≥2 rebalancers).

func newConcurrencyIndex(name string) (*Index, func(*FDBRecordContext) (*FDBRecordStore, error), subspace.Subspace) {
	ctx := context.Background()
	ks := specSubspace()
	idx := NewIndex(name, Concat(Field("price"), Field("quantity")))
	idx.Type = IndexTypeVectorSPFresh
	idx.Options = map[string]string{
		IndexOptionSPFreshNumDimensions: "2",
		IndexOptionSPFreshLmax:          "16",
		IndexOptionSPFreshCellTarget:    "4",
		IndexOptionSPFreshCellMax:       "8",
	}
	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	builder.AddIndex("Order", idx)
	md, err := builder.Build()
	Expect(err).NotTo(HaveOccurred())
	storeBuilder := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
		return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
	}
	var indexSubspace subspace.Subspace
	_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, serr := storeBuilder(rtx)
		Expect(serr).NotTo(HaveOccurred())
		indexSubspace = store.indexSubspace(idx)
		_, serr = store.MarkIndexDisabled(name)
		return nil, serr
	})
	Expect(err).NotTo(HaveOccurred())
	// Seed + build a starting topology so splits/merges/coarse-splits actually
	// fire during the concurrent phase (a cold-start empty index would just
	// grow, never stressing the lifecycle moves).
	_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, serr := storeBuilder(rtx)
		Expect(serr).NotTo(HaveOccurred())
		for i := int64(1); i <= 40; i++ {
			if _, serr := store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i % 11)), Quantity: proto.Int32(int32(i % 9))}); serr != nil {
				return nil, serr
			}
		}
		return nil, nil
	})
	Expect(err).NotTo(HaveOccurred())
	Expect(BuildSPFreshIndex(ctx, sharedDB, storeBuilder, name, 42)).To(Succeed())
	_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, serr := storeBuilder(rtx)
		Expect(serr).NotTo(HaveOccurred())
		_, serr = store.MarkIndexReadable(name)
		return nil, serr
	})
	Expect(err).NotTo(HaveOccurred())
	return idx, storeBuilder, indexSubspace
}

// assertSPFreshSelfConsistent pins the §6/§12 structural invariants after
// quiescence: every membership target holds the pk's posting entry, the fine
// counters equal the live posting sizes, and every ACTIVE posting is within
// the 4×Lmax search-visibility envelope.
func assertSPFreshSelfConsistent(ctx context.Context, indexSubspace subspace.Subspace, config SPFreshConfig) {
	storage := newSPFreshStorage(indexSubspace, 1)
	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		tx := rtx.Transaction()
		cells, _, lerr := spfreshLoadAllCoarse(tx, storage)
		Expect(lerr).NotTo(HaveOccurred())
		for _, cellID := range cells {
			rows, _, _, cerr := spfreshLoadCell(tx, storage, cellID)
			Expect(cerr).NotTo(HaveOccurred())
			for _, r := range rows {
				if r.row.state != spfreshStateActive {
					continue
				}
				entries, perr := spfreshLoadPostingForSplit(tx, storage, r.fineID)
				Expect(perr).NotTo(HaveOccurred())
				count, cterr := spfreshCounterReadSnapshot(tx, storage, spfreshCounterFine, r.fineID)
				Expect(cterr).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(len(entries))), "fine counter drift on posting %d", r.fineID)
				Expect(len(entries)).To(BeNumerically("<=", 4*config.Lmax),
					"posting %d over the 4×Lmax envelope (%d entries) — tail invisible to queries", r.fineID, len(entries))
				// Every posting entry's membership names this posting (no
				// orphaned residual whose owner disclaims it).
				for _, e := range entries {
					mem, merr := spfreshReadMembership(tx, storage, e.pk)
					Expect(merr).NotTo(HaveOccurred(), "posting %d holds pk %v with no membership", r.fineID, e.pk)
					Expect(mem).To(ContainElement(r.fineID),
						"pk %v sits in posting %d but its membership %v disclaims it (orphan)", e.pk, r.fineID, mem)
				}
			}
		}
		return nil, nil
	})
	Expect(err).NotTo(HaveOccurred())
}

var _ = Describe("SPFresh concurrency: same-pk writers + multi-rebalancer", func() {
	ctx := context.Background()

	It("same-pk concurrent writers leave every live record findable at its OWN stored vector", func() {
		const (
			writers      = 6
			opsPerWriter = 80
			pkPool       = 40 // writers contend on this shared pk set
		)
		idx, storeBuilder, indexSubspace := newConcurrencyIndex("spf_samepk")
		config := parseSPFreshConfig(idx)

		var wgW, wgR sync.WaitGroup
		var done atomic.Bool
		errs := make(chan error, writers+2)
		for w := 0; w < writers; w++ {
			wgW.Add(1)
			go func(w int) {
				defer wgW.Done()
				defer GinkgoRecover()
				for i := 0; i < opsPerWriter; i++ {
					// Shared pk pool: writers collide on the SAME pks, so the
					// same-pk membership serialization fence is exercised.
					pk := int64(((w*7 + i*13) % pkPool) + 1)
					_, werr := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
						store, serr := storeBuilder(rtx)
						if serr != nil {
							return nil, serr
						}
						if i%5 == 4 {
							_, derr := store.DeleteRecord(tuple.Tuple{pk})
							return nil, derr
						}
						p, q := int32((w*37+i*17)%200), int32((w*23+i*11)%200)
						_, serr = store.SaveRecord(&gen.Order{OrderId: proto.Int64(pk), Price: proto.Int32(p), Quantity: proto.Int32(q)})
						return nil, serr
					})
					if werr != nil {
						errs <- werr
						return
					}
				}
			}(w)
		}
		// Two concurrent rebalancers race the writers AND each other.
		for r := 0; r < 2; r++ {
			wgR.Add(1)
			go func(r int) {
				defer wgR.Done()
				defer GinkgoRecover()
				storage := newSPFreshStorage(indexSubspace, 1)
				for round := int64(0); !done.Load(); round++ {
					owner := fmt.Sprintf("samepk-reb%d-%d", r, round)
					if _, rerr := spfreshRebalanceOnce(ctx, sharedDB, storage, config, owner, round*131+int64(r), 0, nil); rerr != nil {
						errs <- fmt.Errorf("rebalancer %d: %w", r, rerr)
						return
					}
				}
			}(r)
		}
		wgW.Wait()
		done.Store(true)
		wgR.Wait()
		close(errs)
		for werr := range errs {
			Expect(werr).NotTo(HaveOccurred())
		}

		_, err := RebalanceSPFreshIndex(ctx, sharedDB, storeBuilder, "spf_samepk")
		Expect(err).NotTo(HaveOccurred())

		assertSPFreshSelfConsistent(ctx, indexSubspace, config)

		// The lost-update invariant: for every pk the INDEX still holds, the
		// vector it stored (sidecar) must be findable at itself — a concurrent
		// update that raced a lifecycle move and left the pk indexed against a
		// stale centroid for a new vector would make the query at its own
		// stored vector miss it.
		storage := newSPFreshStorage(indexSubspace, 1)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			maintainer, merr := store.getIndexMaintainer(idx)
			Expect(merr).NotTo(HaveOccurred())
			sbd := maintainer.(interface {
				ScanByDistance(TupleRange, []byte, ScanProperties) RecordCursor[*IndexEntry]
			})
			tx := rtx.Transaction()
			for pk := int64(1); pk <= pkPool; pk++ {
				mem, merr := spfreshReadMembership(tx, storage, tuple.Tuple{pk})
				if merr != nil {
					continue // not currently indexed (last op was a delete)
				}
				Expect(mem).NotTo(BeEmpty())
				data, gerr := tx.Get(storage.sidecarKey(tuple.Tuple{pk})).Get()
				Expect(gerr).NotTo(HaveOccurred())
				Expect(data).NotTo(BeNil(), "pk %d has membership but no sidecar vector", pk)
				vec, derr := vectorcodec.Deserialize(data)
				Expect(derr).NotTo(HaveOccurred())
				// k covers any ties on the mod-200 grid.
				dups := 0
				for o := int64(1); o <= pkPool; o++ {
					if od, _ := tx.Get(storage.sidecarKey(tuple.Tuple{o})).Get(); od != nil {
						if ov, e := vectorcodec.Deserialize(od); e == nil && ov[0] == vec[0] && ov[1] == vec[1] {
							dups++
						}
					}
				}
				cursor := sbd.ScanByDistance(TupleRange{
					Low:  tuple.Tuple{SerializeVector(vec)},
					High: tuple.Tuple{int64(dups + 5)},
				}, nil, ScanProperties{})
				var got []int64
				for {
					res, cerr := cursor.OnNext(ctx)
					Expect(cerr).NotTo(HaveOccurred())
					if !res.HasNext() {
						break
					}
					got = append(got, res.GetValue().Key[0].(int64))
				}
				Expect(got).To(ContainElement(pk),
					"pk %d not findable at its own stored vector %v after same-pk churn: got %v", pk, vec, got)
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("two concurrent rebalancers under short leases never orphan entries", func() {
		const (
			writers      = 4
			opsPerWriter = 70
		)
		idx, storeBuilder, indexSubspace := newConcurrencyIndex("spf_2reb")
		config := parseSPFreshConfig(idx)

		// Short leases force mid-lifecycle takeover between the two executors —
		// the exact "two executors interleave multi-tx lifecycles" scenario the
		// 300k fill orphaned ¾ of its entries on (pre unique-owner fix).
		restore := spfreshLeaseDurationMs
		spfreshLeaseDurationMs = 50
		defer func() { spfreshLeaseDurationMs = restore }()

		live := map[int64][2]int32{}
		var liveMu sync.Mutex
		for i := int64(1); i <= 40; i++ {
			live[i] = [2]int32{int32(i % 11), int32(i % 9)}
		}
		var wgW, wgR sync.WaitGroup
		var done atomic.Bool
		errs := make(chan error, writers+3)
		for w := 0; w < writers; w++ {
			wgW.Add(1)
			go func(w int) {
				defer wgW.Done()
				defer GinkgoRecover()
				base := int64(1000 * (w + 1))
				for i := 0; i < opsPerWriter; i++ {
					id := base + int64(i)
					p, q := int32((w*29+i*7)%200), int32((w*13+i*19)%200)
					_, werr := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
						store, serr := storeBuilder(rtx)
						if serr != nil {
							return nil, serr
						}
						_, serr = store.SaveRecord(&gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(p), Quantity: proto.Int32(q)})
						return nil, serr
					})
					if werr != nil {
						errs <- werr
						return
					}
					liveMu.Lock()
					live[id] = [2]int32{p, q}
					liveMu.Unlock()
				}
			}(w)
		}
		for r := 0; r < 2; r++ {
			wgR.Add(1)
			go func(r int) {
				defer wgR.Done()
				defer GinkgoRecover()
				storage := newSPFreshStorage(indexSubspace, 1)
				for round := int64(0); !done.Load(); round++ {
					owner := fmt.Sprintf("twoReb-%d-%d", r, round)
					if _, rerr := spfreshRebalanceOnce(ctx, sharedDB, storage, config, owner, round*977+int64(r)*7, 0, nil); rerr != nil {
						errs <- fmt.Errorf("rebalancer %d: %w", r, rerr)
						return
					}
				}
			}(r)
		}
		wgW.Wait()
		done.Store(true)
		wgR.Wait()
		close(errs)
		for werr := range errs {
			Expect(werr).NotTo(HaveOccurred())
		}

		_, err := RebalanceSPFreshIndex(ctx, sharedDB, storeBuilder, "spf_2reb")
		Expect(err).NotTo(HaveOccurred())

		assertSPFreshSelfConsistent(ctx, indexSubspace, config)

		// Every live record (seed + inserts) is findable at its own vector —
		// no entry orphaned by the two executors interleaving lifecycle moves.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			maintainer, merr := store.getIndexMaintainer(idx)
			Expect(merr).NotTo(HaveOccurred())
			sbd := maintainer.(interface {
				ScanByDistance(TupleRange, []byte, ScanProperties) RecordCursor[*IndexEntry]
			})
			checked := 0
			liveMu.Lock()
			defer liveMu.Unlock()
			for id, v := range live {
				if checked >= 50 {
					break
				}
				checked++
				dups := 0
				for _, ov := range live {
					if ov == v {
						dups++
					}
				}
				cursor := sbd.ScanByDistance(TupleRange{
					Low:  tuple.Tuple{SerializeVector([]float64{float64(v[0]), float64(v[1])})},
					High: tuple.Tuple{int64(dups + 5)},
				}, nil, ScanProperties{})
				var got []int64
				for {
					res, cerr := cursor.OnNext(ctx)
					Expect(cerr).NotTo(HaveOccurred())
					if !res.HasNext() {
						break
					}
					got = append(got, res.GetValue().Key[0].(int64))
				}
				Expect(got).To(ContainElement(id),
					"live record %d not findable at its own vector after 2-rebalancer churn: got %v", id, got)
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("a SPLIT lease lost between SEAL and SPLIT is a benign skip, not a failure", func() {
		// Deterministic regression for the seal->split lease-takeover race the
		// concurrent test above only hits intermittently. The
		// spfreshSealSplitGapHook seam forcibly hands the SPLIT task's lease to
		// a foreign owner in the exact window between its SEAL (which claimed the
		// lease) and its SPLIT re-claim, so spfreshSplitFine's re-claim returns
		// errSPFreshLeaseHeld deterministically. The rebalancer must treat that
		// as a benign skip (the new owner finishes the split) — NOT a failure.
		idx, storeBuilder, indexSubspace := newConcurrencyIndex("spf_splitleasegap")
		config := parseSPFreshConfig(idx)
		storage := newSPFreshStorage(indexSubspace, 1)

		// Overfill one centroid: a tight (4..6, 4..6) cluster lands on one
		// centroid and pushes its posting well past Lmax=16, so the next pass
		// seals + splits it.
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			for id := int64(100); id < 160; id++ {
				p, q := int32(4+id%3), int32(4+(id/3)%3)
				if _, serr := store.SaveRecord(&gen.Order{OrderId: proto.Int64(id), Price: &p, Quantity: &q}); serr != nil {
					return nil, serr
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// A live lease cannot be claimed away, so the foreign takeover overwrites
		// the task row's owner directly (preserving its SEALED state + children).
		var stolen []int64
		restore := spfreshSealSplitGapHook
		spfreshSealSplitGapHook = func(cellID, fineID int64) {
			_, e := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				tx := rtx.Transaction()
				key := storage.taskKey(spfreshTaskSplit, fineID)
				data, ge := tx.Get(key).Get()
				if ge != nil {
					return nil, ge
				}
				Expect(data).NotTo(BeNil())
				row, de := decodeTaskRow(data)
				if de != nil {
					return nil, de
				}
				row.owner = "foreign-worker"
				row.leaseDeadlineMs = spfreshLeaseDeadline()
				tx.Set(key, encodeTaskRow(row))
				return nil, nil
			})
			Expect(e).NotTo(HaveOccurred())
			stolen = append(stolen, fineID)
		}
		defer func() { spfreshSealSplitGapHook = restore }()

		// One pass as "me": it seals a split, the hook hands the lease to
		// foreign-worker, and the SPLIT re-claim returns errSPFreshLeaseHeld.
		_, err = spfreshRebalanceOnce(ctx, sharedDB, storage, config, "me-rebalancer", 7, 0, nil)
		Expect(err).NotTo(HaveOccurred()) // the fix: a stolen split lease is a skip, not a failure
		Expect(stolen).NotTo(BeEmpty(), "the hook must fire on a real SPLIT task, or the test proves nothing")

		// The foreign lease survived untouched — we skipped the split, did not
		// steal it back.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			data, ge := rtx.Transaction().Get(storage.taskKey(spfreshTaskSplit, stolen[0])).Get()
			Expect(ge).NotTo(HaveOccurred())
			Expect(data).NotTo(BeNil())
			row, de := decodeTaskRow(data)
			Expect(de).NotTo(HaveOccurred())
			Expect(row.owner).To(Equal("foreign-worker"),
				"the skipped split's foreign lease must survive untouched")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
