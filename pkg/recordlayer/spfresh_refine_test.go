package recordlayer

import (
	"context"
	"errors"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/vectorcodec"
)

// RFC-104 assignment refinement. The headline recovery (drifted fast-fill →
// recall recovers to the bulk baseline) is measured in the env-gated
// foreground-fill bench (SIFT_REFINE). These FDB specs pin the correctness
// invariants that gate the design:
//   - a converged bulk index refines to ZERO moves (the no-op-on-converged
//     property — what pins kc = 4·spfreshClosurePool), for r ∈ {2,4};
//   - the budget bounds pks SCANNED (not moves), so a quiescent index advances
//     its cursor incrementally instead of walking the whole keyspace in one call;
//   - the move count is retry-safe (a conflict re-run never double-counts);
//   - the lifecycle fence drops a NEW closure copy whose target fine is sealing.
var _ = Describe("SPFresh refinement (RFC-104)", func() {
	ctx := context.Background()

	buildMeta := func(idx *Index) *RecordMetaData {
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		b.AddIndex("Order", idx)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}
	newVecIndex := func(name string, replication int) *Index {
		idx := NewIndex(name, Concat(Field("price"), Field("quantity")))
		idx.Type = IndexTypeVectorSPFresh
		idx.Options = map[string]string{
			IndexOptionSPFreshNumDimensions: "2",
			IndexOptionSPFreshLmax:          "32",
			IndexOptionSPFreshCellTarget:    "4",
			IndexOptionSPFreshCellMax:       "8",
			IndexOptionSPFreshReplication:   strconv.Itoa(replication),
		}
		return idx
	}

	// buildConverged loads n Order records and bulk-builds (build-then-read) the
	// index, leaving it readable and converged. Returns the store builder.
	buildConverged := func(ks subspace.Subspace, md *RecordMetaData, indexName string, n int) func(*FDBRecordContext) (*FDBRecordStore, error) {
		storeBuilder := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		}
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			_, serr = store.MarkIndexDisabled(indexName)
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			for i := 0; i < n; i++ {
				if _, serr = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(int64(i)),
					Price:    proto.Int32(int32((i * 13) % 50)),
					Quantity: proto.Int32(int32((i*7)%40 + 1)),
				}); serr != nil {
					return nil, serr
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(BuildSPFreshIndex(ctx, sharedDB, storeBuilder, indexName, 42)).To(Succeed())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			_, serr = store.MarkIndexReadable(indexName)
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())
		return storeBuilder
	}

	DescribeTable("a converged bulk index refines to ZERO moves",
		func(replication int) {
			ks := specSubspace()
			name := "spf_refine_r" + strconv.Itoa(replication)
			idx := newVecIndex(name, replication)
			md := buildMeta(idx)
			storeBuilder := buildConverged(ks, md, name, 120)

			before := knnIDs(ctx, storeBuilder, idx)

			// Every vector re-routes to the SAME closure set the wide build placed
			// (kc=4·spfreshClosurePool matches the build pool), so NOTHING moves. A
			// narrower kc would drop replicas here — and wider replication (r=4)
			// has more replicas to lose, so it gates kc harder.
			total := 0
			for {
				m, wrapped, rerr := RefineSPFreshIndex(ctx, sharedDB, storeBuilder, name, 1000)
				Expect(rerr).NotTo(HaveOccurred())
				total += m
				if wrapped {
					break
				}
			}
			Expect(total).To(Equal(0), "a converged bulk index must refine to zero moves (gates kc = 4·spfreshClosurePool)")

			after := knnIDs(ctx, storeBuilder, idx)
			Expect(after).To(Equal(before), "zero moves ⇒ identical kNN results")
		},
		Entry("default replication r=2", 2),
		Entry("wide replication r=4", 4),
	)

	It("bounds work by the budget (pks scanned), advancing the cursor incrementally", func() {
		// On a converged index NOTHING moves, so a budget that counted moves would
		// never trip and a single call would walk the ENTIRE keyspace. The budget
		// must bound pks RE-EVALUATED: budget=50 over n=120 covers the keyspace in
		// 50+50+20 → exactly three calls, and the first call must NOT wrap.
		ks := specSubspace()
		idx := newVecIndex("spf_refine_budget", 2)
		md := buildMeta(idx)
		storeBuilder := buildConverged(ks, md, "spf_refine_budget", 120)

		m1, w1, err := RefineSPFreshIndex(ctx, sharedDB, storeBuilder, "spf_refine_budget", 50)
		Expect(err).NotTo(HaveOccurred())
		Expect(m1).To(Equal(0))
		Expect(w1).To(BeFalse(), "budget<n must not cover the whole keyspace in one call (budget bounds pks, not moves)")

		calls := 1
		for !w1 {
			_, w1, err = RefineSPFreshIndex(ctx, sharedDB, storeBuilder, "spf_refine_budget", 50)
			Expect(err).NotTo(HaveOccurred())
			calls++
		}
		Expect(calls).To(Equal(3), "ceil(120/50) calls of budget 50 to cover the keyspace")
	})

	It("never double-counts a move when the batch tx conflict-retries", func() {
		// The move tally lives in a Go variable mutated inside the auto-retried
		// transaction body. If it were the OUTER variable, a conflict retry would
		// re-run the body and count the move twice. Drop one copy from one pk (so
		// exactly one move is owed), force every refine tx through exactly one
		// conflict retry, and assert the reported move count is 1, not 2.
		ks := specSubspace()
		idx := newVecIndex("spf_refine_retry", 2)
		md := buildMeta(idx)
		storeBuilder := buildConverged(ks, md, "spf_refine_retry", 120)

		s, config, err := spfreshResolveRefineTarget(ctx, sharedDB, storeBuilder, "spf_refine_retry")
		Expect(err).NotTo(HaveOccurred())
		Expect(s).NotTo(BeNil())
		cache, err := spfreshLoadRefineCache(ctx, sharedDB, s)
		Expect(err).NotTo(HaveOccurred())

		// Drop the last closure copy of the first pk that has >=2 — leaving exactly
		// one move owed (refine re-routes and re-adds it).
		plans := findCopyDrops(ctx, s, config, cache, 1)
		Expect(plans).To(HaveLen(1), "need a pk with >=2 closure copies")
		dropCopies(ctx, s, plans, -1 /* seal none */)

		retryDB := NewFDBDatabaseWithTransactor(&retryOnceTransactor{inner: sharedDB.transactor}, sharedDB.db)
		moved, _, err := RefineSPFreshIndex(ctx, retryDB, storeBuilder, "spf_refine_retry", 1000)
		Expect(err).NotTo(HaveOccurred())
		Expect(moved).To(Equal(1), "a conflict retry of the move batch must not double-count the move")
	})

	It("drops a NEW closure copy whose target fine is sealing — lifecycle fence", func() {
		// route is permissive: it returns ACTIVE and SEALED fines (a sealed posting
		// is still readable). But a MOVE must not deposit a NEW posting into a fine
		// that seals/splits concurrently — its members are about to be redistributed
		// and a late copy would be orphaned. The fence REAL-reads each kept NEW
		// copy's state and drops non-ACTIVE targets. A/B: drop a copy from two pks;
		// SEAL one's target (the cache still believes it ACTIVE — the real race).
		ks := specSubspace()
		idx := newVecIndex("spf_refine_fence", 2)
		md := buildMeta(idx)
		storeBuilder := buildConverged(ks, md, "spf_refine_fence", 120)

		s, config, err := spfreshResolveRefineTarget(ctx, sharedDB, storeBuilder, "spf_refine_fence")
		Expect(err).NotTo(HaveOccurred())
		Expect(s).NotTo(BeNil())
		// Cache loaded BEFORE the seal: it still holds the to-be-sealed fine as
		// ACTIVE, so route offers it and ONLY the REAL-read fence can reject it —
		// the tightest test of the fence (not route exclusion).
		cache, err := spfreshLoadRefineCache(ctx, sharedDB, s)
		Expect(err).NotTo(HaveOccurred())
		quantizer := newSPFreshQuantizer(config)
		kc := spfreshRefineKc(config)

		plans := findCopyDrops(ctx, s, config, cache, 2)
		Expect(plans).To(HaveLen(2), "need two pks with >=2 closure copies and distinct dropped fines")
		ctrl, fenced := plans[0], plans[1]

		// Drop both copies; SEAL only the fenced pk's dropped fine.
		dropCopies(ctx, s, plans, 1 /* seal index 1 */)

		var ctrlMoved, fencedMoved bool
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			var perr error
			if ctrlMoved, perr = spfreshRefinePKInTx(tx, s, config, quantizer, cache, kc, ctrl.pk); perr != nil {
				return nil, perr
			}
			fencedMoved, perr = spfreshRefinePKInTx(tx, s, config, quantizer, cache, kc, fenced.pk)
			return nil, perr
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(ctrlMoved).To(BeTrue(), "control: dropped copy is ACTIVE → refine re-adds it")
		Expect(fencedMoved).To(BeFalse(), "fenced: dropped copy targets a SEALED fine → refine drops the new copy (no-op)")

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			cm, merr := spfreshReadMembership(tx, s, ctrl.pk)
			if merr != nil {
				return nil, merr
			}
			Expect(idSetEqual(cm, ctrl.full)).To(BeTrue(), "control membership restored to the full closure")
			cp, gerr := tx.Snapshot().Get(s.postingKey(ctrl.dropID, ctrl.pk)).Get()
			if gerr != nil {
				return nil, gerr
			}
			Expect(cp).NotTo(BeNil(), "control posting re-written")

			fm, merr := spfreshReadMembership(tx, s, fenced.pk)
			if merr != nil {
				return nil, merr
			}
			Expect(idSetEqual(fm, withoutID(fenced.full, fenced.dropID))).To(BeTrue(), "fenced membership stays reduced (copy NOT re-added)")
			fp, gerr := tx.Snapshot().Get(s.postingKey(fenced.dropID, fenced.pk)).Get()
			if gerr != nil {
				return nil, gerr
			}
			Expect(fp).To(BeNil(), "fenced posting NOT written into the sealing fine")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("the fleet driver refines across tenants, recovers drift, and reports convergence", func() {
		// RefineSPFreshIndexes is the production driver — the refinement loop
		// beside the rebalancer loop. Two converged tenants; drift ONE (drop K
		// closure copies); the fleet pass must re-add exactly K (the other tenant
		// stays at zero) and, once recovered, report both tenants converged.
		base := specSubspace()
		md := buildMeta(newVecIndex("spf", 2))
		sb1 := buildConverged(base.Sub("t1"), md, "spf", 120)
		sb2 := buildConverged(base.Sub("t2"), md, "spf", 120)

		s1, cfg1, err := spfreshResolveRefineTarget(ctx, sharedDB, sb1, "spf")
		Expect(err).NotTo(HaveOccurred())
		Expect(s1).NotTo(BeNil())
		cache1, err := spfreshLoadRefineCache(ctx, sharedDB, s1)
		Expect(err).NotTo(HaveOccurred())
		plans := findCopyDrops(ctx, s1, cfg1, cache1, 3)
		Expect(plans).To(HaveLen(3), "need three drifted pks with distinct dropped fines")
		dropCopies(ctx, s1, plans, -1 /* seal none */)
		k := len(plans)

		tenants := []SPFreshTenant{
			{StoreBuilder: sb1, IndexName: "spf"},
			{StoreBuilder: sb2, IndexName: "spf"},
		}
		// budget ≥ n ⇒ each pass is one full cursor cycle per tenant, so
		// convergence is per-pass clean (the small-budget cursor path is pinned
		// by the budget spec above).
		totalMoves := 0
		var res SPFreshRefineResult
		for i := 0; i < 5; i++ {
			res, err = RefineSPFreshIndexes(ctx, sharedDB, tenants, SPFreshRefineOptions{BudgetPerTenant: 200})
			Expect(err).NotTo(HaveOccurred())
			totalMoves += res.Moves
			if res.Converged == len(tenants) {
				break
			}
		}
		Expect(res.Converged).To(Equal(len(tenants)), "both tenants converge (cursor wraps, zero moves)")
		Expect(totalMoves).To(Equal(k), "the fleet re-adds exactly the K dropped copies (drifted tenant only)")

		// The drifted tenant's memberships are restored to their full closure.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			for _, p := range plans {
				m, merr := spfreshReadMembership(tx, s1, p.pk)
				if merr != nil {
					return nil, merr
				}
				Expect(idSetEqual(m, p.full)).To(BeTrue(), "drifted membership recovered by the fleet pass")
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("the fleet driver isolates per-tenant errors and honors ctx cancellation", func() {
		base := specSubspace()
		md := buildMeta(newVecIndex("spf", 2))
		sb := buildConverged(base.Sub("good"), md, "spf", 120)
		tenants := []SPFreshTenant{
			{StoreBuilder: sb, IndexName: "spf"},
			{StoreBuilder: sb, IndexName: "does_not_exist"}, // bad: not in metadata
		}

		res, err := RefineSPFreshIndexes(ctx, sharedDB, tenants, SPFreshRefineOptions{BudgetPerTenant: 200})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("does_not_exist"), "the failing tenant is reported")
		Expect(res.Refined).To(Equal(1), "the good tenant refines despite the bad tenant's error")
		Expect(res.Converged).To(Equal(1), "the good (converged) tenant wraps with zero moves")

		// A cancelled context ends the pass before touching any tenant.
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		cres, cerr := RefineSPFreshIndexes(cctx, sharedDB, tenants, SPFreshRefineOptions{})
		Expect(errors.Is(cerr, context.Canceled)).To(BeTrue())
		Expect(cres.Refined).To(Equal(0))
	})
})

// copyDropPlan records a pk whose last closure copy we drop (and, for the fenced
// case, seal): the original membership, the dropped fine, its cell, and the
// dropped centroid's verbatim row fields (to re-encode it SEALED preserving its
// vector bytes).
type copyDropPlan struct {
	pk       tuple.Tuple
	full     []int64
	dropID   int64
	dropCell int64
	row      spfreshCentroidRow
}

// findCopyDrops scans memberships and returns up to `want` pks that each carry
// >=2 closure copies, with DISTINCT dropped fines (so sealing one never fences
// another). The dropped fine is the pk's last membership id; its cell comes from
// routing the pk's vector against the current topology.
func findCopyDrops(ctx context.Context, s *spfreshStorage, config SPFreshConfig, cache *spfreshRoutingCache, want int) []copyDropPlan {
	var plans []copyDropPlan
	kc := spfreshRefineKc(config)
	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		tx := rtx.Transaction()
		mr, rerr := fdb.PrefixRange(s.membership.Bytes())
		if rerr != nil {
			return nil, rerr
		}
		kvs, kerr := tx.Snapshot().GetRange(mr, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
		if kerr != nil {
			return nil, kerr
		}
		used := map[int64]bool{}
		for _, kv := range kvs {
			if len(plans) == want {
				break
			}
			pk, uerr := s.membership.Unpack(kv.Key)
			if uerr != nil {
				return nil, uerr
			}
			cur, derr := decodeMembership(kv.Value)
			if derr != nil {
				return nil, derr
			}
			if len(cur) < 2 {
				continue
			}
			dropID := cur[len(cur)-1]
			if used[dropID] {
				continue
			}
			sc, gerr := tx.Snapshot().Get(s.sidecarKey(pk)).Get()
			if gerr != nil {
				return nil, gerr
			}
			if sc == nil {
				continue
			}
			vec, verr := vectorcodec.Deserialize(sc)
			if verr != nil {
				return nil, verr
			}
			routed, rrerr := cache.route(tx, s, vec, config.BuildAssignCells, kc)
			if rrerr != nil {
				return nil, rrerr
			}
			cell := int64(-1)
			for _, r := range routed {
				if r.fineID == dropID {
					cell = r.cellID
					break
				}
			}
			if cell < 0 {
				continue // dropped copy not currently routed — skip
			}
			row, crerr := spfreshReadCentroidForWrite(tx, s, cell, dropID)
			if crerr != nil {
				return nil, crerr
			}
			used[dropID] = true
			plans = append(plans, copyDropPlan{pk: pk, full: cur, dropID: dropID, dropCell: cell, row: row})
		}
		return nil, nil
	})
	Expect(err).NotTo(HaveOccurred())
	return plans
}

// dropCopies removes each plan's dropped fine from its pk (clear the posting,
// −1 the fine counter, rewrite the reduced membership). If sealIdx >= 0, the
// plan at that index additionally has its dropped fine SEALED (preserving its
// vector bytes) — modelling a fine that begins sealing under a concurrent refine.
func dropCopies(ctx context.Context, s *spfreshStorage, plans []copyDropPlan, sealIdx int) {
	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		tx := rtx.Transaction()
		for i, p := range plans {
			tx.Clear(s.postingKey(p.dropID, p.pk))
			spfreshCounterAdd(tx, s, spfreshCounterFine, p.dropID, -1)
			tx.Set(s.membershipKey(p.pk), encodeMembership(withoutID(p.full, p.dropID)))
			if i == sealIdx {
				sealed := encodeCentroidRowRaw(spfreshStateSealed, p.row.epoch, p.row.childA, p.row.childB, p.row.vecBytes)
				spfreshSaveCentroid(tx, s, p.dropCell, p.dropID, sealed)
			}
		}
		return nil, nil
	})
	Expect(err).NotTo(HaveOccurred())
}

// retryOnceTransactor forces EXACTLY ONE conflict retry of each transaction body
// it runs (1020 not_committed is retryable), to exercise the refine op's
// retry-safe move accounting. The injected error is returned only on the first
// body invocation per Transact call; the inner retry loop rolls that attempt back
// and re-runs the body, which then commits.
type retryOnceTransactor struct {
	inner fdb.Transactor
}

func (t *retryOnceTransactor) Transact(fn func(fdb.Transaction) (any, error)) (any, error) {
	first := true
	return t.inner.Transact(func(tx fdb.Transaction) (any, error) {
		res, err := fn(tx)
		if err != nil {
			return res, err
		}
		if first {
			first = false
			return nil, fdb.Error{Code: 1020} // not_committed — retryable
		}
		return res, nil
	})
}

func (t *retryOnceTransactor) ReadTransact(fn func(fdb.ReadTransaction) (any, error)) (any, error) {
	return t.inner.ReadTransact(fn)
}

// withoutID returns ids with the first occurrence of drop removed.
func withoutID(ids []int64, drop int64) []int64 {
	out := make([]int64, 0, len(ids))
	removed := false
	for _, id := range ids {
		if id == drop && !removed {
			removed = true
			continue
		}
		out = append(out, id)
	}
	return out
}

// idSetEqual reports whether a and b hold the same fine ids (order-independent).
func idSetEqual(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[int64]int{}
	for _, id := range a {
		seen[id]++
	}
	for _, id := range b {
		seen[id]--
	}
	for _, c := range seen {
		if c != 0 {
			return false
		}
	}
	return true
}

// knnIDs runs a fixed kNN query and returns the result order_ids.
func knnIDs(ctx context.Context, storeBuilder func(*FDBRecordContext) (*FDBRecordStore, error), idx *Index) []int64 {
	var got []int64
	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, serr := storeBuilder(rtx)
		if serr != nil {
			return nil, serr
		}
		maintainer, merr := store.getIndexMaintainer(idx)
		if merr != nil {
			return nil, merr
		}
		sbd := maintainer.(interface {
			ScanByDistance(TupleRange, []byte, ScanProperties) RecordCursor[*IndexEntry]
		})
		cursor := sbd.ScanByDistance(TupleRange{
			Low:  tuple.Tuple{SerializeVector([]float64{15, 15})},
			High: tuple.Tuple{int64(10)},
		}, nil, ScanProperties{})
		got = got[:0]
		for {
			res, cerr := cursor.OnNext(ctx)
			if cerr != nil {
				return nil, cerr
			}
			if !res.HasNext() {
				break
			}
			got = append(got, res.GetValue().Key[0].(int64))
		}
		return nil, nil
	})
	Expect(err).NotTo(HaveOccurred())
	return got
}
