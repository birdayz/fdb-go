package recordlayer

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

var _ = Describe("SPFresh recall monitor (RFC-156 ground-truth)", func() {
	ctx := context.Background()

	recallMetadata := func() *RecordMetaDataBuilder {
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return b
	}
	recallIndex := func(name string) *Index {
		idx := NewIndex(name, Concat(Field("price"), Field("quantity")))
		idx.Type = IndexTypeVectorSPFresh
		idx.Options = map[string]string{
			IndexOptionSPFreshNumDimensions: "2",
			IndexOptionSPFreshLmax:          "32",
			IndexOptionSPFreshCellTarget:    "4",
			IndexOptionSPFreshCellMax:       "8",
		}
		return idx
	}

	It("reports high recall on a healthy bulk-built index", func() {
		ks := specSubspace()
		idx := recallIndex("spf_recall")
		b := recallMetadata()
		b.AddIndex("Order", idx)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		storeBuilder := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		}

		// 64 DISTINCT grid points (price 0..7 x quantity 0..7) so ground truth
		// is unambiguous; bulk build => the converged-topology ideal.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			if _, serr := store.MarkIndexDisabled("spf_recall"); serr != nil {
				return nil, serr
			}
			id := int64(1)
			for p := int32(0); p < 8; p++ {
				for q := int32(0); q < 8; q++ {
					if _, serr := store.SaveRecord(&gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(p), Quantity: proto.Int32(q)}); serr != nil {
						return nil, serr
					}
					id++
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(BuildSPFreshIndex(ctx, sharedDB, storeBuilder, "spf_recall", 42)).To(Succeed())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			_, serr = store.MarkIndexReadable("spf_recall")
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())

		var report SPFreshRecallReport
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			report, serr = MeasureSPFreshRecall(ctx, store, "spf_recall", 5, 30, 7)
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(report.CorpusSize).To(Equal(64))
		Expect(report.QueriesRun).To(Equal(30))
		Expect(report.K).To(Equal(5))
		// A healthy bulk-built index recalls high; the residual gap from 1.0 is
		// grid-point distance ties (several equidistant neighbors at the k-th
		// rank), not index error — which is exactly what makes this a real
		// recall measurement. The threshold has headroom so a genuine
		// regression (corruption / maintenance behind) drops well below it.
		Expect(report.MeanRecall).To(BeNumerically(">=", 0.90),
			"healthy SPFresh index must have high recall@5 (got %.4f)", report.MeanRecall)
		GinkgoWriter.Printf("recall@5: mean=%.4f min=%.4f perfectFrac=%.4f\n",
			report.MeanRecall, report.MinRecall, report.PerfectFraction)
	})

	It("returns a zero-query report for an empty index", func() {
		ks := specSubspace()
		idx := recallIndex("spf_recall_empty")
		b := recallMetadata()
		b.AddIndex("Order", idx)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		storeBuilder := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		}
		var report SPFreshRecallReport
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			report, serr = MeasureSPFreshRecall(ctx, store, "spf_recall_empty", 10, 10, 1)
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(report.CorpusSize).To(BeZero())
		Expect(report.QueriesRun).To(BeZero())
	})

	It("detects silent index corruption that the integrity check cannot", func() {
		ks := specSubspace()
		idx := recallIndex("spf_recall_corrupt")
		b := recallMetadata()
		b.AddIndex("Order", idx)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		storeBuilder := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		}

		// Healthy bulk-built index over 64 distinct grid points.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			if _, serr := store.MarkIndexDisabled("spf_recall_corrupt"); serr != nil {
				return nil, serr
			}
			id := int64(1)
			for p := int32(0); p < 8; p++ {
				for q := int32(0); q < 8; q++ {
					if _, serr := store.SaveRecord(&gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(p), Quantity: proto.Int32(q)}); serr != nil {
						return nil, serr
					}
					id++
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(BuildSPFreshIndex(ctx, sharedDB, storeBuilder, "spf_recall_corrupt", 42)).To(Succeed())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			_, serr = store.MarkIndexReadable("spf_recall_corrupt")
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())

		measure := func() SPFreshRecallReport {
			var rep SPFreshRecallReport
			_, merr := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, serr := storeBuilder(rtx)
				if serr != nil {
					return nil, serr
				}
				rep, serr = MeasureSPFreshRecall(ctx, store, "spf_recall_corrupt", 5, 40, 7)
				return nil, serr
			})
			Expect(merr).NotTo(HaveOccurred())
			return rep
		}
		integrityBad := func() int {
			var rep SPFreshIntegrityReport
			_, ierr := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, serr := storeBuilder(rtx)
				if serr != nil {
					return nil, serr
				}
				rep, serr = SPFreshCheckIntegrity(rtx, store, "spf_recall_corrupt", 1<<30)
				return nil, serr
			})
			Expect(ierr).NotTo(HaveOccurred())
			return rep.MembershipWithoutEntry + rep.BadTargets
		}

		healthy := measure()
		Expect(healthy.MeanRecall).To(BeNumerically(">=", 0.90))
		Expect(integrityBad()).To(BeZero(), "healthy index has no integrity violations")

		// Silently drop HALF the records from the index — posting entries,
		// membership row, and sidecar all cleared — WITHOUT deleting the
		// records. The records still exist (ground truth still expects them),
		// but the index can no longer return them. Because membership is
		// removed too, this leaves NO dangling membership, so the integrity
		// check stays green. Only recall sees it.
		var idxSub subspace.Subspace
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			idxSub = store.indexSubspace(idx)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			g, gerr := spfreshReadGenerationSnapshot(tx, newSPFreshStorage(idxSub, 0))
			if gerr != nil {
				return nil, gerr
			}
			s := newSPFreshStorage(idxSub, g)
			for pk := int64(1); pk <= 64; pk += 2 { // half the records
				p := tuple.Tuple{pk}
				mem, merr := spfreshReadMembership(tx, s, p)
				if merr != nil {
					continue
				}
				for _, fineID := range mem {
					tx.Clear(s.postingKey(fineID, p))
				}
				tx.Clear(s.membershipKey(p))
				tx.Clear(s.sidecarKey(p))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Integrity is STILL clean (no dangling membership) — it cannot see
		// this class of corruption...
		Expect(integrityBad()).To(BeZero(),
			"clean removal leaves no structural violation — integrity is blind to it")
		// ...but recall catches it: dropped records remain in the corpus
		// (ground truth) and the index can no longer return them.
		corrupted := measure()
		GinkgoWriter.Printf("recall@5: healthy mean=%.4f, corrupted mean=%.4f\n",
			healthy.MeanRecall, corrupted.MeanRecall)
		Expect(corrupted.MeanRecall).To(BeNumerically("<", healthy.MeanRecall-0.15),
			"recall monitor must detect the silent drop (healthy %.4f -> corrupted %.4f)",
			healthy.MeanRecall, corrupted.MeanRecall)
	})

	// spfresh-reviewer (RFC-156) finding 1: SPANN §3.2.1 balanced-postings /
	// LIRE split guarantee — no ACTIVE posting may exceed the 4xLmax hard
	// envelope. SPFreshCheckIntegrity must catch an over-envelope posting (the
	// precise failure LIRE's split exists to prevent: a split that never drained).
	It("flags a posting over the 4xLmax hard envelope", func() {
		ks := specSubspace()
		idx := recallIndex("spf_oversize") // Lmax=32 -> 4xLmax=128
		b := recallMetadata()
		b.AddIndex("Order", idx)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		storeBuilder := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		}

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			if _, serr := store.MarkIndexDisabled("spf_oversize"); serr != nil {
				return nil, serr
			}
			for id := int64(1); id <= 30; id++ {
				if _, serr := store.SaveRecord(&gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(int32(id % 6)), Quantity: proto.Int32(int32(id / 6))}); serr != nil {
					return nil, serr
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(BuildSPFreshIndex(ctx, sharedDB, storeBuilder, "spf_oversize", 42)).To(Succeed())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			_, serr = store.MarkIndexReadable("spf_oversize")
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())

		check := func() SPFreshIntegrityReport {
			var rep SPFreshIntegrityReport
			_, cerr := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, serr := storeBuilder(rtx)
				if serr != nil {
					return nil, serr
				}
				rep, serr = SPFreshCheckIntegrity(rtx, store, "spf_oversize", 1<<30)
				return nil, serr
			})
			Expect(cerr).NotTo(HaveOccurred())
			return rep
		}

		// Healthy: the field is populated and nothing is over the envelope.
		healthy := check()
		Expect(healthy.ActiveFines).To(BeNumerically(">", 0))
		Expect(healthy.OversizedHard).To(BeZero(), "a balanced index has no over-envelope postings")
		Expect(healthy.MaxPostingLen).To(BeNumerically("<=", 4*32))

		// Inflate one ACTIVE fine's posting past 4xLmax=128 by writing bogus
		// entries (valid posting keys; the size scan counts keys, not values) —
		// simulating a split that failed to drain.
		var idxSub subspace.Subspace
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			idxSub = store.indexSubspace(idx)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			g, gerr := spfreshReadGenerationSnapshot(tx, newSPFreshStorage(idxSub, 0))
			if gerr != nil {
				return nil, gerr
			}
			s := newSPFreshStorage(idxSub, g)
			mem, merr := spfreshReadMembership(tx, s, tuple.Tuple{int64(1)})
			if merr != nil {
				return nil, merr
			}
			Expect(mem).NotTo(BeEmpty())
			fineID := mem[0]
			for i := 0; i < 4*32+5; i++ { // push total over 128
				tx.Set(s.postingKey(fineID, tuple.Tuple{int64(900000 + i)}), []byte{1})
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Now the integrity check must flag the over-envelope posting.
		bad := check()
		Expect(bad.OversizedHard).To(BeNumerically(">=", 1),
			"SPFreshCheckIntegrity must flag the posting over 4xLmax")
		Expect(bad.MaxPostingLen).To(BeNumerically(">", 4*32))
	})

	// codex-review findings (PR #388): the recall monitor must work on the
	// SERIALIZED-vector index shape (vector_data []byte — what the SPFresh
	// benchmarks and real deployments use), the search wrapper must reject
	// wrong-dimensional queries instead of panicking, and the integrity sample
	// must honor its cap. All three were missed by the 2D-int-vector tests above.
	It("works on a serialized-vector index, rejects wrong-dim queries, caps the integrity sample", func() {
		const n = 40
		const dims = 8
		ks := specSubspace()
		idx := NewIndex("spf_recall_serialized", KeyWithValue(Field("vector_data"), 0))
		idx.Type = IndexTypeVectorSPFresh
		idx.Options = map[string]string{
			IndexOptionSPFreshNumDimensions: "8",
			IndexOptionSPFreshLmax:          "32",
			IndexOptionSPFreshCellTarget:    "4",
			IndexOptionSPFreshCellMax:       "8",
		}
		b := recallMetadata()
		b.AddIndex("Order", idx)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		storeBuilder := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		}
		mkVec := func(i int) []float64 {
			v := make([]float64, dims)
			v[0] = float64(i) // distinct lead component => unambiguous ground truth
			for j := 1; j < dims; j++ {
				v[j] = float64((i*7 + j) % 13)
			}
			return v
		}
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			if _, serr := store.MarkIndexDisabled("spf_recall_serialized"); serr != nil {
				return nil, serr
			}
			for i := 1; i <= n; i++ {
				if _, serr := store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i)), VectorData: SerializeVector(mkVec(i))}); serr != nil {
					return nil, serr
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(BuildSPFreshIndex(ctx, sharedDB, storeBuilder, "spf_recall_serialized", 42)).To(Succeed())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			_, serr = store.MarkIndexReadable("spf_recall_serialized")
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			// codex P1: the corpus must populate on a serialized-vector index
			// (previously CorpusSize=0 — the monitor silently did nothing).
			rep, serr := MeasureSPFreshRecall(ctx, store, "spf_recall_serialized", 5, 30, 7)
			if serr != nil {
				return nil, serr
			}
			Expect(rep.CorpusSize).To(Equal(n), "serialized-vector corpus must populate (codex P1)")
			Expect(rep.QueriesRun).To(Equal(30))
			Expect(rep.MeanRecall).To(BeNumerically(">=", 0.90))

			// codex P2: a wrong-dimensional query returns an error, not a panic.
			_, derr := SearchSPFreshIndex(store, "spf_recall_serialized", []float64{1, 2, 3}, 5)
			Expect(derr).To(HaveOccurred(), "wrong-dim query must error, not panic (codex P2)")
			Expect(derr.Error()).To(ContainSubstring("dimensions"))

			// codex P3: the integrity sample never exceeds the requested cap.
			irep, serr := SPFreshCheckIntegrity(rtx, store, "spf_recall_serialized", 10)
			if serr != nil {
				return nil, serr
			}
			Expect(irep.Members).To(Equal(n))
			Expect(irep.Sampled).To(BeNumerically("<=", 10), "integrity sample must honor the cap (codex P3)")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
