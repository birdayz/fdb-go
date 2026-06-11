package recordlayer

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/vectorcodec"
)

// newSweeperTenant builds one tenant store with a 2-d SPFresh index for the
// sweeper specs: optionally seeded and bulk-built.
func newSweeperTenant(name string, seedN int, build bool) (SPFreshTenant, subspace.Subspace) {
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
	if seedN > 0 {
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			for i := 0; i < seedN; i++ {
				if _, serr := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(int32(i % 3)), Quantity: proto.Int32(int32(i % 2)),
				}); serr != nil {
					return nil, serr
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	}
	if build {
		Expect(BuildSPFreshIndex(ctx, sharedDB, storeBuilder, name, 42)).To(Succeed())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			_, serr = store.MarkIndexReadable(name)
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())
	}
	return SPFreshTenant{StoreBuilder: storeBuilder, IndexName: name}, indexSubspace
}

// The multi-tenant maintenance sweeper: discovery probe, per-tenant fairness
// budget, pass continuation for undrained tenants, and isolation of tenant
// failures.
var _ = Describe("SPFresh multi-tenant sweeper", func() {
	ctx := context.Background()

	newTenant := newSweeperTenant

	// balloonTenant injects an oversized posting + its split trigger
	// directly (the cascade-test shape): real maintenance work whose drain
	// needs multiple rounds.
	balloonTenant := func(sub subspace.Subspace, entries int) {
		storage := newSPFreshStorage(sub, 1)
		config := DefaultSPFreshConfig(2)
		config.Lmax = 16
		quantizer := newSPFreshQuantizer(config)
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			mem, merr := spfreshReadMembership(tx, storage, tuple.Tuple{int64(1)})
			Expect(merr).NotTo(HaveOccurred())
			fine := mem[0]
			cell, ferr := spfreshFindCentroidCell(tx, storage, fine)
			Expect(ferr).NotTo(HaveOccurred())
			row, rerr := spfreshReadCentroidForWrite(tx, storage, cell, fine)
			Expect(rerr).NotTo(HaveOccurred())
			cvec, verr := row.vector()
			Expect(verr).NotTo(HaveOccurred())
			for i := 0; i < entries; i++ {
				pk := tuple.Tuple{int64(50000 + i)}
				v := []float64{float64(i%40) * 0.3, float64(i%37) * 0.3}
				tx.Set(storage.postingKey(fine, pk), quantizer.Encode([]float64{v[0] - cvec[0], v[1] - cvec[1]}))
				tx.Set(storage.membershipKey(pk), encodeMembership([]int64{fine}))
				tx.Set(storage.sidecarKey(pk), vectorcodec.SerializeHalf(v))
				spfreshCounterAdd(tx, storage, spfreshCounterFine, fine, 1)
			}
			_, terr := spfreshTaskSetIfAbsent(tx, storage, spfreshTaskSplit, fine)
			return nil, terr
		})
		Expect(err).NotTo(HaveOccurred())
	}

	It("probes, budgets fairly across tenants, and drains over passes", func() {
		whale, whaleSub := newTenant("spf_sw_whale", 8, true)
		small, smallSub := newTenant("spf_sw_small", 8, true)
		idle, _ := newTenant("spf_sw_idle", 8, true)
		unbuilt, _ := newTenant("spf_sw_unbuilt", 0, false)

		// The whale needs MANY rounds (a 600-entry cascade); the small tenant
		// needs ~2; idle has no tasks; unbuilt has no generation.
		balloonTenant(whaleSub, 600)
		balloonTenant(smallSub, 40)

		Expect(SPFreshHasPendingMaintenance(ctx, sharedDB, whale.StoreBuilder, whale.IndexName)).To(BeTrue())
		// A FRESH build must probe idle: the flip clears the builder's
		// Cellfin bookkeeping rows (leaking them made every bulk-built index
		// look permanently busy — found by this very assertion).
		Expect(SPFreshHasPendingMaintenance(ctx, sharedDB, idle.StoreBuilder, idle.IndexName)).To(BeFalse())
		Expect(SPFreshHasPendingMaintenance(ctx, sharedDB, unbuilt.StoreBuilder, unbuilt.IndexName)).To(BeFalse())

		tenants := []SPFreshTenant{whale, small, idle, unbuilt}

		// Pass 1, tight budget: BOTH busy tenants must get work (fairness —
		// the whale's backlog must not consume the pass), and the whale must
		// be left undrained.
		res, err := SweepSPFreshIndexes(ctx, sharedDB, tenants, SPFreshSweepOptions{MaxRoundsPerTenant: 2})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Worked).To(Equal(2), "only the two tenants with tasks should be touched")
		Expect(res.Actions).To(BeNumerically(">", 0))
		Expect(res.Undrained).To(BeNumerically(">=", 1), "the whale cannot drain in 2 rounds")

		// Loop passes until the fleet is quiet — the sweeper's deployment
		// shape. Every tenant drains; nothing oscillates.
		for pass := 0; pass < 50; pass++ {
			res, err = SweepSPFreshIndexes(ctx, sharedDB, tenants, SPFreshSweepOptions{MaxRoundsPerTenant: 2})
			Expect(err).NotTo(HaveOccurred())
			if res.Worked == 0 {
				break
			}
		}
		Expect(res.Worked).To(BeZero(), "fleet must quiesce")

		// The whale's cascade actually completed: every ACTIVE posting is
		// within the envelope (the same invariant the single-tenant cascade
		// test pins).
		storage := newSPFreshStorage(whaleSub, 1)
		config := DefaultSPFreshConfig(2)
		config.Lmax = 16
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			cells, _, lerr := spfreshLoadAllCoarse(tx, storage)
			Expect(lerr).NotTo(HaveOccurred())
			worst := 0
			for _, cid := range cells {
				rows, _, _, cerr := spfreshLoadCell(tx, storage, cid)
				Expect(cerr).NotTo(HaveOccurred())
				for _, r := range rows {
					if r.row.state != spfreshStateActive {
						continue
					}
					entries, _, _, _, perr := spfreshLoadPostingSnapshot(tx, storage, r.fineID, 100000)
					Expect(perr).NotTo(HaveOccurred())
					if len(entries) > worst {
						worst = len(entries)
					}
				}
			}
			Expect(worst).To(BeNumerically("<=", 4*config.Lmax))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("isolates a broken tenant: the rest of the pass still runs", func() {
		good, goodSub := newTenant("spf_sw_good", 8, true)
		balloonTenant(goodSub, 40)
		broken := SPFreshTenant{StoreBuilder: good.StoreBuilder, IndexName: "no_such_index"}

		res, err := SweepSPFreshIndexes(ctx, sharedDB, []SPFreshTenant{broken, good}, SPFreshSweepOptions{})
		Expect(err).To(HaveOccurred(), "the broken tenant's failure must be reported")
		Expect(err.Error()).To(ContainSubstring("no_such_index"))
		Expect(res.Worked).To(Equal(1), "the good tenant must still be swept")
		Expect(res.Actions).To(BeNumerically(">", 0))
	})
})

// The two codex MT-review findings, pinned.
var _ = Describe("SPFresh sweeper budgets and legacy bookkeeping", func() {
	ctx := context.Background()

	It("the action budget caps work WITHIN one round (whale with a wide queue)", func() {
		config := DefaultSPFreshConfig(2)
		config.Lmax = 16
		storage := newSPFreshStorage(specSubspace().Sub("spfresh-budget").Sub("wide"), 1)
		quantizer := newSPFreshQuantizer(config)

		// Two independent oversized postings → TWO split tasks in one scan.
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			spfreshSetGeneration(tx, storage, 1)
			spfreshSaveCoarse(tx, storage, 1, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{25, 25}))
			for f, cvec := range map[int64][]float64{10: {0, 0}, 11: {50, 50}} {
				spfreshSaveCentroid(tx, storage, 1, f, encodeCentroidRow(spfreshStateActive, 0, 0, 0, cvec))
				for i := 0; i < config.Lmax+2; i++ {
					pk := tuple.Tuple{f*100000 + int64(i)}
					v := []float64{cvec[0] + float64(i%5)*0.3, cvec[1] + float64(i%7)*0.3}
					tx.Set(storage.postingKey(f, pk), quantizer.Encode([]float64{v[0] - cvec[0], v[1] - cvec[1]}))
					tx.Set(storage.membershipKey(pk), encodeMembership([]int64{f}))
					tx.Set(storage.sidecarKey(pk), vectorcodec.SerializeHalf(v))
				}
				spfreshCounterSet(tx, storage, spfreshCounterFine, f, int64(config.Lmax+2))
				if _, terr := spfreshTaskSetIfAbsent(tx, storage, spfreshTaskSplit, f); terr != nil {
					return nil, terr
				}
			}
			spfreshCounterSet(tx, storage, spfreshCounterCell, 1, 2)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// limit=1: exactly ONE of the two splits runs — pre-fix the round
		// executed the whole queue and a whale monopolized the pass.
		worked, rerr := spfreshRebalanceOnce(ctx, sharedDB, storage, config, "budget-1", 7, 1)
		Expect(rerr).NotTo(HaveOccurred())
		Expect(worked).To(Equal(1), "the per-pass budget must cap work inside a single round")

		// Unlimited drains the rest.
		for round := 0; round < 50; round++ {
			worked, rerr = spfreshRebalanceOnce(ctx, sharedDB, storage, config, "budget-2", int64(round), 0)
			Expect(rerr).NotTo(HaveOccurred())
			if worked == 0 {
				break
			}
		}
		Expect(worked).To(BeZero())
	})

	It("legacy leaked Cellfin rows: probe ignores them, rebalancer clears them", func() {
		good, _ := newSweeperTenant("spf_sw_legacy", 8, true)

		// Plant a leaked build-bookkeeping row the way pre-cleanup builds
		// left them (kind=Cellfin in the READABLE generation's task space).
		var storage *spfreshStorage
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := good.StoreBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			idx := store.GetMetaData().GetIndex(good.IndexName)
			gen0, gerr := spfreshReadGenerationSnapshot(rtx.Transaction(), newSPFreshStorage(store.indexSubspace(idx), 0))
			Expect(gerr).NotTo(HaveOccurred())
			storage = newSPFreshStorage(store.indexSubspace(idx), gen0)
			rtx.Transaction().Set(storage.taskKey(spfreshTaskCellfin, 0), encodeTaskRow(spfreshTaskRow{childA: 1, childB: 2}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// The probe must NOT report the tenant busy for build garbage —
		// pre-fix the sweeper revisited it every pass for zero actions.
		Expect(SPFreshHasPendingMaintenance(ctx, sharedDB, good.StoreBuilder, good.IndexName)).To(BeFalse())

		// And the rebalancer self-heals: one invocation clears the row.
		_, err = RebalanceSPFreshIndex(ctx, sharedDB, good.StoreBuilder, good.IndexName)
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			data, gerr := rtx.Transaction().Get(storage.taskKey(spfreshTaskCellfin, 0)).Get()
			Expect(gerr).NotTo(HaveOccurred())
			Expect(data).To(BeNil(), "the rebalancer must clear legacy Cellfin bookkeeping on sight")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
