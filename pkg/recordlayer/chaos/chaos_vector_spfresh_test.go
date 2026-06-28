package chaos

import (
	"context"
	"math/rand/v2"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// Model-based chaos for the SPFresh (RFC-094) vector index — the production
// hardening gate from RFC-156. Until this file the chaos harness modeled only
// HNSW (verify_vector.go); the SPFresh maintenance lifecycle (split / merge /
// coarse-split / NPA / GC) and RFC-104 refinement had ZERO fault coverage. A
// vector index corrupts SILENTLY (wrong answers, not a crash), so the verifier
// (verify_vector_spfresh.go) checks the queryable + structural invariants:
// every record self-searchable, no orphans, exactly one membership row per
// record, membership ⊆ postings, every target ACTIVE post-drain.
//
// The scenario drains the maintenance queue through the CLEAN transactor before
// each Verify: that both makes the strict integrity invariant valid AND proves
// the post-fault state is recoverable — whatever a commit_unknown / conflict
// left mid-lifecycle, a clean pass completes it.

const spfreshChaosIndex = "spf_chaos"

// spfreshChaosMetadata builds an Order metadata with a 2D SPFresh vector index
// over (price, quantity) — small Lmax/cell targets so a few hundred records
// exercise the full split/merge lifecycle.
func spfreshChaosMetadata(t testing.TB) *recordlayer.RecordMetaData {
	t.Helper()
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	idx := recordlayer.NewIndex(spfreshChaosIndex, recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("quantity")))
	idx.Type = recordlayer.IndexTypeVectorSPFresh
	idx.Options = map[string]string{
		recordlayer.IndexOptionSPFreshNumDimensions: "2",
		recordlayer.IndexOptionSPFreshLmax:          "32",
		recordlayer.IndexOptionSPFreshCellTarget:    "4",
		recordlayer.IndexOptionSPFreshCellMax:       "8",
		// Cooldown 0 so the merge lifecycle executes deterministically in a
		// fast test: split children carry epoch=creation-ms and the default
		// 600s post-split cooldown (spfresh_merge.go) would skip every merge.
		// The cooldown TIMING is an oscillation heuristic, not what's under
		// test here — the merge transaction's fault-idempotence is. GC's
		// retired-topology window keys off the same value, so this also
		// exercises GC reaping promptly.
		recordlayer.IndexOptionSPFreshCooldownSec: "0",
	}
	b.AddIndex("Order", idx)
	md, err := b.Build()
	if err != nil {
		t.Fatalf("build spfresh chaos metadata: %v", err)
	}
	return md
}

func spfOrder(id int64, price, quantity int32) *gen.Order {
	return &gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(price), Quantity: proto.Int32(quantity)}
}

// Sanity: with no faults the verifier, the SPFresh search wrapper, and the
// structured integrity check all agree on a clean index across insert / update
// / delete. Proves the harness before faults enter.
func TestSPFreshChaos_BasicVerify(t *testing.T) {
	t.Parallel()
	md := spfreshChaosMetadata(t)
	s := NewScenario(t, testRealDB, md)

	for id := int64(1); id <= 24; id++ {
		s.SaveRecord(spfOrder(id, int32(5+id%6), int32(5+(id/6)%6)))
	}
	s.DrainSPFresh(spfreshChaosIndex)
	s.Verify()

	// Update (move) some records.
	for id := int64(1); id <= 8; id++ {
		s.SaveRecord(spfOrder(id, int32(20+id), int32(20+id)))
	}
	s.DrainSPFresh(spfreshChaosIndex)
	s.Verify()

	// Delete some.
	for id := int64(9); id <= 20; id++ {
		s.DeleteRecord(tuple.Tuple{id})
	}
	s.DrainSPFresh(spfreshChaosIndex)
	s.Verify()
}

// commit_unknown / conflict on the foreground insert + update path must not
// double-insert, lose, or duplicate a record's index entry (the re-execution
// idempotence the §6b write path claims).
func TestSPFreshChaos_WritePathFaults(t *testing.T) {
	t.Parallel()
	md := spfreshChaosMetadata(t)
	s := NewScenario(t, testRealDB, md)

	for id := int64(1); id <= 6; id++ {
		s.SaveRecord(spfOrder(id, 10, 10))
	}

	// commit_unknown on a fresh insert.
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(spfOrder(7, 11, 12))
	s.DrainSPFresh(spfreshChaosIndex)
	s.Verify()

	// conflict on an overwrite that MOVES the vector (clear-old + write-new,
	// the path most exposed to a non-idempotent replay).
	s.InjectOnce(FaultConflict)
	s.SaveRecord(spfOrder(7, 40, 41))
	s.DrainSPFresh(spfreshChaosIndex)
	s.Verify()

	// commit_unknown on a delete (membership-driven clear must be idempotent).
	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(3)})
	s.DrainSPFresh(spfreshChaosIndex)
	s.Verify()
}

// A fault injected INTO the split lifecycle: pile records past the overfill
// ceiling, then rebalance with commit_unknown landing mid-seal/mid-split. The
// split must be idempotent under replay — no orphaned children, no lost
// entries, membership ⊆ postings after a clean drain.
func TestSPFreshChaos_SplitUnderFault(t *testing.T) {
	t.Parallel()
	md := spfreshChaosMetadata(t)
	s := NewScenario(t, testRealDB, md)

	// 120 records over a tight 6x6 grid: varied enough that k-means can
	// separate a split (not degenerate-identical), dense enough to blow a
	// posting past 2xLmax=64.
	for id := int64(1); id <= 120; id++ {
		s.SaveRecord(spfOrder(id, int32(5+id%6), int32(5+(id/6)%6)))
	}

	// Inject a commit_unknown into the very next rebalance transaction (a
	// lifecycle step), then a conflict into a later one.
	s.InjectOnce(FaultCommitUnknown)
	_, _ = s.RebalanceSPFresh(spfreshChaosIndex) // may error under the fault — fine
	s.InjectOnce(FaultConflict)
	_, _ = s.RebalanceSPFresh(spfreshChaosIndex)

	// Clean drain to quiescence (proves recoverability), then verify.
	s.DrainSPFresh(spfreshChaosIndex)
	s.Verify()
}

// The main soak: phased grow/shrink cycles that GUARANTEE the split and merge
// lifecycles fire, all maintenance driven through the fault transactor with a
// timer, then asserted (splits>0 AND merges>0) so this is real coverage, not a
// fake checkbox. Each cycle grows a tight cluster (postings overfill -> splits),
// sweeps + refines under faults, drains clean, verifies; then deletes most of
// the cluster (postings drain below Lmin -> merges), sweeps under faults,
// drains, verifies.
func TestSPFreshChaos_LifecycleSoakFaults(t *testing.T) {
	t.Parallel()
	seeds := []uint64{0x5901, 0x5902}
	for _, seed := range seeds {
		seed := seed
		t.Run("seed_"+itoa(seed), func(t *testing.T) {
			t.Parallel()
			md := spfreshChaosMetadata(t)
			s := NewScenario(t, testRealDB, md, WithSeed(seed), WithFaults(FaultsRetryHeavy))
			timer := recordlayer.NewStoreTimer()

			const cycles = 4
			const growN = 150
			const shrinkN = 130 // leaves 20 live per cycle
			for c := 0; c < cycles; c++ {
				base := int64(c)*1000 + 1

				// GROW: pile a tight 4x4 cluster -> a fine overfills past
				// 2xLmax=64 -> split (and, as fines multiply in a cell past
				// CellMax=8, coarse-split + the NPA follow-up).
				for i := int64(0); i < growN; i++ {
					price := int32(s.Rng.Int64N(4))
					qty := int32(s.Rng.Int64N(4))
					s.SaveRecord(spfOrder(base+i, price, qty))
				}
				if _, err := s.SweepSPFresh(spfreshChaosIndex, timer); err != nil {
					t.Logf("cycle %d grow sweep (faults): %v", c, err) // bounded/undrained ok
				}
				_, _, _ = s.RefineSPFresh(spfreshChaosIndex, 64) // refine under faults
				s.DrainSPFresh(spfreshChaosIndex)
				s.Verify()

				// SHRINK: delete most -> postings below Lmin=Lmax/8 -> merges.
				for i := int64(0); i < shrinkN; i++ {
					s.DeleteRecord(tuple.Tuple{base + i})
				}
				if _, err := s.SweepSPFresh(spfreshChaosIndex, timer); err != nil {
					t.Logf("cycle %d shrink sweep (faults): %v", c, err)
				}
				s.DrainSPFresh(spfreshChaosIndex)
				s.Verify()
			}

			splits := timer.GetCount(recordlayer.CountSPFreshSplits)
			merges := timer.GetCount(recordlayer.CountSPFreshMerges)
			t.Logf("lifecycle soak seed=%d: splits=%d merges=%d csplits=%d npas=%d refineMoves=%d faults=%d",
				seed, splits, merges,
				timer.GetCount(recordlayer.CountSPFreshCSplits),
				timer.GetCount(recordlayer.CountSPFreshNPAs),
				timer.GetCount(recordlayer.CountSPFreshRefineMoves),
				len(s.FaultLog()))
			if splits == 0 {
				t.Fatalf("soak exercised no SPLITS under faults — not actually testing the split lifecycle")
			}
			if merges == 0 {
				t.Fatalf("soak exercised no MERGES under faults — not actually testing the merge lifecycle")
			}
			if len(s.FaultLog()) == 0 {
				t.Fatalf("soak injected no faults — FaultsRetryHeavy misconfigured")
			}
		})
	}
}

// The refiner-vs-rebalancer race under faults — the surface RFC-104's
// isolation specs (single goroutine, no concurrent rebalancer) never touched.
// Concurrent writers + a rebalancer loop + a refiner loop, all through the
// fault transactor; after a clean drain the snapshot-rebuilt model must agree
// with the index.
func TestSPFreshChaos_ConcurrentRefineRebalanceFaults(t *testing.T) {
	t.Parallel()
	md := spfreshChaosMetadata(t)
	s := NewScenario(t, testRealDB, md, WithSeed(0x5905), WithFaults(FaultsRetryHeavy))
	ctx := context.Background()

	// Bootstrap a generation so the maintenance loops have something to scan.
	for id := int64(1); id <= 8; id++ {
		s.SaveRecord(spfOrder(id, int32(id%6), int32(id%6)))
	}

	const writers = 4
	const perWriter = 50

	var writerWg, loopWg sync.WaitGroup
	stop := make(chan struct{})

	// Concurrent writers: disjoint pk bands (no inter-writer pk contention so
	// the snapshot truth is unambiguous), varied tight-cluster vectors.
	for w := 0; w < writers; w++ {
		writerWg.Add(1)
		go func(band int) {
			defer writerWg.Done()
			base := int64(1000 + band*1000)
			rng := rand.New(rand.NewPCG(uint64(band)+1, 0x5905))
			for i := 0; i < perWriter; i++ {
				pk := base + int64(i)
				price := int32(rng.Int64N(6))
				qty := int32(rng.Int64N(6))
				_, err := s.ChaosDB().Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					store, err := s.OpenStore(rtx)
					if err != nil {
						return nil, err
					}
					_, err = store.SaveRecord(spfOrder(pk, price, qty))
					return nil, err
				})
				if err != nil {
					t.Errorf("concurrent writer %d save pk=%d: %v", band, pk, err)
					return
				}
			}
		}(w)
	}

	// Rebalancer + refiner loops, racing each other and the writers, under faults.
	loopWg.Add(2)
	go func() {
		defer loopWg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = s.RebalanceSPFresh(spfreshChaosIndex)
			}
		}
	}()
	go func() {
		defer loopWg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _, _ = s.RefineSPFresh(spfreshChaosIndex, 40)
			}
		}
	}()

	writerWg.Wait()
	close(stop)
	loopWg.Wait()

	// Clean drain, then snapshot-verify (the model is rebuilt from store state,
	// so concurrent ops need no shared model).
	s.DrainSPFresh(spfreshChaosIndex)
	violations, err := s.CleanDB().Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, serr := s.OpenStore(rtx)
		if serr != nil {
			return nil, serr
		}
		return VerifySnapshot(store, md), nil
	})
	if err != nil {
		t.Fatalf("concurrent verify: %v", err)
	}
	vs, _ := violations.([]Violation)
	if len(vs) > 0 {
		msg := ""
		for _, v := range vs {
			msg += "\n  - " + v.String()
		}
		t.Fatalf("concurrent refine/rebalance under faults: %d violation(s):%s\nfaults: %d", len(vs), msg, len(s.FaultLog()))
	}
	t.Logf("concurrent done: %d records written, %d faults injected", writers*perWriter+8, len(s.FaultLog()))
}

// itoa renders a uint64 seed for subtest names without importing strconv at
// every call site.
func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	const hexdigits = "0123456789abcdef"
	var buf [16]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = hexdigits[v&0xf]
		v >>= 4
	}
	return string(buf[i:])
}
