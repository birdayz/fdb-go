package recordlayer

import (
	"context"
	"fmt"
	"math/rand"
	"sort"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

var _ = Describe("SPFresh build + query e2e", func() {
	ctx := context.Background()

	// testConfig: small dims for speed; Lmax low so the build creates several
	// cells and fine centroids (a REAL two-level topology, not a degenerate
	// one-posting index).
	testConfig := func(dims int) SPFreshConfig {
		c := DefaultSPFreshConfig(dims)
		c.Lmax = 32
		c.CellTarget = 4
		c.CellMax = 8
		return c
	}

	makeInputs := func(n, dims int, seed int64) []spfreshBuildInput {
		rng := rand.New(rand.NewSource(seed))
		inputs := make([]spfreshBuildInput, n)
		for i := range inputs {
			vec := make([]float64, dims)
			for d := range vec {
				vec[d] = rng.NormFloat64() * 5
			}
			inputs[i] = spfreshBuildInput{pk: tuple.Tuple{int64(i)}, vec: vec}
		}
		return inputs
	}

	bruteForceKNN := func(inputs []spfreshBuildInput, query []float64, k int) []int64 {
		type idD struct {
			id int64
			d  float64
		}
		all := make([]idD, len(inputs))
		for i, in := range inputs {
			all[i] = idD{id: in.pk[0].(int64), d: spfreshSquaredDistance(query, in.vec)}
		}
		sort.Slice(all, func(i, j int) bool {
			if all[i].d != all[j].d {
				return all[i].d < all[j].d
			}
			return all[i].id < all[j].id
		})
		ids := make([]int64, k)
		for i := 0; i < k; i++ {
			ids[i] = all[i].id
		}
		return ids
	}

	It("builds a two-level index and answers kNN with high recall", func() {
		const (
			n    = 2000
			dims = 16
			k    = 10
		)
		config := testConfig(dims)
		Expect(ValidateSPFreshConfig(config)).To(Succeed())
		storage := newSPFreshStorage(specSubspace().Sub("spfresh-e2e").Sub("recall"), 1)
		inputs := makeInputs(n, dims, 7)

		builder := newSPFreshBuilder(sharedDB, storage, config, "builder-1")
		Expect(builder.build(ctx, inputs, 42)).To(Succeed())

		// The build must have produced a genuine two-level shape.
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			gen, gerr := spfreshReadGenerationSnapshot(tx, storage)
			Expect(gerr).NotTo(HaveOccurred())
			Expect(gen).To(Equal(int64(1)), "generation flipped readable")
			ids, _, lerr := spfreshLoadAllCoarse(tx, storage)
			Expect(lerr).NotTo(HaveOccurred())
			Expect(len(ids)).To(BeNumerically(">", 1), "multiple coarse cells")
			// Staging fully cleared (FINALIZED contract).
			for _, cellID := range ids {
				pks, _, serr := spfreshLoadStagingCell(tx, storage, cellID)
				Expect(serr).NotTo(HaveOccurred())
				Expect(pks).To(BeEmpty(), "staging cleared at finalization")
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		cache := newSPFreshRoutingCache(0)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return nil, cache.fullReload(rtx.Transaction(), storage, 1)
		})
		Expect(err).NotTo(HaveOccurred())

		searcher := newSPFreshSearcher(storage, config, cache)

		// Recall@10 across 50 queries (held-in: the vectors themselves — exact
		// self-recall plus neighborhood recall).
		hits, total := 0, 0
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			for qi := 0; qi < 50; qi++ {
				query := inputs[qi*37%n].vec
				want := bruteForceKNN(inputs, query, k)
				got, serr := searcher.search(tx, query, k)
				Expect(serr).NotTo(HaveOccurred())
				Expect(got).To(HaveLen(k))
				wantSet := map[int64]bool{}
				for _, id := range want {
					wantSet[id] = true
				}
				for _, r := range got {
					if wantSet[r.PrimaryKey[0].(int64)] {
						hits++
					}
					total++
				}
				// Self-retrieval: the query IS an indexed vector; with exact
				// re-rank it must be the top hit.
				Expect(got[0].PrimaryKey[0].(int64)).To(Equal(inputs[qi*37%n].pk[0].(int64)),
					"query %d: indexed vector must retrieve itself first", qi)
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		recall := float64(hits) / float64(total)
		Expect(recall).To(BeNumerically(">=", 0.90),
			fmt.Sprintf("recall@%d = %.3f, want >= 0.90", k, recall))

		// Distances are exact (re-ranked) and ascending.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			got, serr := searcher.search(rtx.Transaction(), inputs[0].vec, k)
			Expect(serr).NotTo(HaveOccurred())
			Expect(got[0].Distance).To(BeNumerically("~", 0, 1e-2), "self-distance ~0 (fp16 rounding)")
			for i := 1; i < len(got); i++ {
				Expect(got[i].Distance).To(BeNumerically(">=", got[i-1].Distance))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("closure replication writes multiple posting copies and dedups at query time", func() {
		config := testConfig(8)
		storage := newSPFreshStorage(specSubspace().Sub("spfresh-e2e").Sub("closure"), 1)
		inputs := makeInputs(600, 8, 11)
		builder := newSPFreshBuilder(sharedDB, storage, config, "builder-1")
		Expect(builder.build(ctx, inputs, 5)).To(Succeed())

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			replicated := 0
			for _, in := range inputs {
				ids, merr := spfreshReadMembership(tx, storage, in.pk)
				Expect(merr).NotTo(HaveOccurred())
				Expect(ids).NotTo(BeEmpty())
				Expect(len(ids)).To(BeNumerically("<=", config.Replication))
				if len(ids) > 1 {
					replicated++
				}
				// Every membership entry has a posting row (the chaos
				// invariant, asserted directly here).
				for _, fineID := range ids {
					entries, _, _, _, lerr := spfreshLoadPostingSnapshot(tx, storage, fineID, 2*config.Lmax+1)
					Expect(lerr).NotTo(HaveOccurred())
					found := false
					for _, e := range entries {
						if string(e.pk.Pack()) == string(in.pk.Pack()) {
							found = true
							break
						}
					}
					Expect(found).To(BeTrue(), "membership row without posting entry")
				}
			}
			Expect(replicated).To(BeNumerically(">", 0),
				"alpha=1.2 must replicate SOME boundary vectors (the rev-3 alpha bug regression)")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Dedup: no pk appears twice in results.
		cache := newSPFreshRoutingCache(0)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			Expect(cache.fullReload(tx, storage, 1)).To(Succeed())
			searcher := newSPFreshSearcher(storage, config, cache)
			got, serr := searcher.search(tx, inputs[3].vec, 20)
			Expect(serr).NotTo(HaveOccurred())
			seen := map[string]bool{}
			for _, r := range got {
				key := string(r.PrimaryKey.Pack())
				Expect(seen[key]).To(BeFalse(), "closure replica leaked through dedup")
				seen[key] = true
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("wave B re-run after FINALIZED is a no-op; wave order is enforced", func() {
		config := testConfig(8)
		storage := newSPFreshStorage(specSubspace().Sub("spfresh-e2e").Sub("waves"), 1)
		inputs := makeInputs(100, 8, 3)
		builder := newSPFreshBuilder(sharedDB, storage, config, "builder-1")
		Expect(builder.build(ctx, inputs, 5)).To(Succeed())

		// Re-running wave B on a FINALIZED cell must be a clean no-op
		// (commit_unknown retry semantics).
		router := builder.buildRouter(map[int64][]int64{}, map[int64][][]float64{})
		for _, cellID := range builder.cellIDs {
			Expect(builder.waveB(ctx, cellID, router)).To(Succeed())
		}

		// Wave B before wave A is a state error: enqueue a fresh task on a
		// new cell and try to finalize it directly.
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			wrote, terr := spfreshTaskSetIfAbsent(tx, storage, spfreshTaskCellfin, 999)
			Expect(terr).NotTo(HaveOccurred())
			Expect(wrote).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(builder.waveB(ctx, 999, router)).To(MatchError(ContainSubstring("wave B before wave A")))
	})

	It("flip retried after its own commit is an idempotent success, not a concurrent-build error (codex r4)", func() {
		config := testConfig(8)
		storage := newSPFreshStorage(specSubspace().Sub("spfresh-e2e").Sub("flip-retry"), 1)
		builder := newSPFreshBuilder(sharedDB, storage, config, "builder-1")
		Expect(builder.build(ctx, makeInputs(100, 8, 3), 5)).To(Succeed())

		// A commit_unknown_result retry re-runs the flip closure AFTER the flip
		// committed: it observes cur == target. With our token still in place
		// that is OUR flip — it must succeed, not report a concurrent build.
		Expect(builder.flip(ctx)).To(Succeed())

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			gen, gerr := spfreshReadGenerationSnapshot(rtx.Transaction(), storage)
			Expect(gerr).NotTo(HaveOccurred())
			Expect(gen).To(Equal(int64(1)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("a takeover token aborts a stale builder's transactions", func() {
		config := testConfig(8)
		storage := newSPFreshStorage(specSubspace().Sub("spfresh-e2e").Sub("takeover"), 1)
		builder := newSPFreshBuilder(sharedDB, storage, config, "builder-1")
		Expect(builder.build(ctx, makeInputs(100, 8, 3), 5)).To(Succeed())

		// A newer build takes ownership (the maintainer does this atomically
		// with its pre-build clear). The stale builder's flip retry must now
		// fail loudly instead of treating gen == target as its own commit.
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			spfreshTakeBuilderToken(rtx.Transaction(), storage, []byte("other-builder-token"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(builder.flip(ctx)).To(MatchError(ContainSubstring("took over")))
		// Wave transactions carry the same fence.
		router := builder.buildRouter(map[int64][]int64{}, map[int64][][]float64{})
		Expect(builder.waveB(ctx, builder.cellIDs[0], router)).To(MatchError(ContainSubstring("took over")))
	})
})
