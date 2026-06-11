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
)

// The 094.3 chaos-flavored churn: concurrent writers (insert/update/delete)
// race a live rebalancer running every lifecycle (splits, NPA, merges, coarse
// splits, GC). After quiescing, the §6/§12 invariants must hold exactly:
// every live record's membership names existing posting entries (and nothing
// else), deleted records are gone everywhere, advisory counters equal posting
// sizes, and every live record is findable by kNN at its own vector.
func contains(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

var _ = Describe("SPFresh churn: writers vs rebalancer", func() {
	ctx := context.Background()

	It("invariants and recall hold after concurrent churn across every lifecycle", func() {
		const (
			writers      = 4
			opsPerWriter = 60
		)
		ks := specSubspace()
		idx := NewIndex("spf_churn", Concat(Field("price"), Field("quantity")))
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
			_, serr = store.MarkIndexDisabled("spf_churn")
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			for i := int64(1); i <= 32; i++ {
				if _, serr := store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i % 7)), Quantity: proto.Int32(int32(i % 5))}); serr != nil {
					return nil, serr
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(BuildSPFreshIndex(ctx, sharedDB, storeBuilder, "spf_churn", 42)).To(Succeed())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			_, serr = store.MarkIndexReadable("spf_churn")
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())

		// Writers: disjoint pk ranges; each tracks its own expected end-state.
		// Op mix: insert (always), update every 3rd, delete every 5th.
		type state struct {
			live map[int64][2]int32
		}
		states := make([]state, writers)
		var wgWriters, wgRebalancer sync.WaitGroup
		var writersDone atomic.Bool
		errs := make(chan error, writers+1)
		for w := 0; w < writers; w++ {
			states[w] = state{live: map[int64][2]int32{}}
			wgWriters.Add(1)
			go func(w int) {
				defer wgWriters.Done()
				defer GinkgoRecover()
				st := &states[w]
				base := int64(10_000 * (w + 1))
				for i := 0; i < opsPerWriter; i++ {
					id := base + int64(i)
					p, q := int32((w*37+i*13)%200), int32((w*11+i*7)%200)
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
					st.live[id] = [2]int32{p, q}
					if i%3 == 2 { // update an earlier record to a new vector
						uid := base + int64(i/2)
						up, uq := int32((i*29)%200), int32((i*23)%200)
						if _, ok := st.live[uid]; ok {
							_, werr := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
								store, serr := storeBuilder(rtx)
								if serr != nil {
									return nil, serr
								}
								_, serr = store.SaveRecord(&gen.Order{OrderId: proto.Int64(uid), Price: proto.Int32(up), Quantity: proto.Int32(uq)})
								return nil, serr
							})
							if werr != nil {
								errs <- werr
								return
							}
							st.live[uid] = [2]int32{up, uq}
						}
					}
					if i%5 == 4 { // delete an earlier record
						did := base + int64(i/3)
						if _, ok := st.live[did]; ok {
							_, werr := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
								store, serr := storeBuilder(rtx)
								if serr != nil {
									return nil, serr
								}
								_, derr := store.DeleteRecord(tuple.Tuple{did})
								return nil, derr
							})
							if werr != nil {
								errs <- werr
								return
							}
							delete(st.live, did)
						}
					}
				}
			}(w)
		}
		// The rebalancer races the writers continuously.
		wgRebalancer.Add(1)
		go func() {
			defer wgRebalancer.Done()
			defer GinkgoRecover()
			storage := newSPFreshStorage(indexSubspace, 1)
			config := parseSPFreshConfig(idx)
			for !writersDone.Load() {
				if _, rerr := spfreshRebalanceOnce(ctx, sharedDB, storage, config, "churn-rebalancer", 7, 0); rerr != nil {
					errs <- fmt.Errorf("rebalancer: %w", rerr)
					return
				}
			}
		}()
		wgWriters.Wait()
		writersDone.Store(true)
		wgRebalancer.Wait()
		close(errs)
		for werr := range errs {
			Expect(werr).NotTo(HaveOccurred())
		}

		// Quiesce everything that's left, then assert the §6/§12 invariants.
		_, err = RebalanceSPFreshIndex(ctx, sharedDB, storeBuilder, "spf_churn")
		Expect(err).NotTo(HaveOccurred())

		live := map[int64][2]int32{}
		for i := int64(1); i <= 32; i++ {
			live[i] = [2]int32{int32(i % 7), int32(i % 5)}
		}
		for w := range states {
			for id, v := range states[w].live {
				live[id] = v
			}
		}
		deleted := map[int64]bool{}
		for w := 0; w < writers; w++ {
			base := int64(10_000 * (w + 1))
			for i := 0; i < opsPerWriter; i++ {
				id := base + int64(i)
				if _, ok := states[w].live[id]; !ok {
					deleted[id] = true
				}
			}
		}

		storage := newSPFreshStorage(indexSubspace, 1)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			for id := range live {
				pk := tuple.Tuple{id}
				mem, merr := spfreshReadMembership(tx, storage, pk)
				Expect(merr).NotTo(HaveOccurred(), "live record %d has no membership", id)
				Expect(mem).NotTo(BeEmpty())
				for _, fineID := range mem {
					data, gerr := tx.Get(storage.postingKey(fineID, pk)).Get()
					Expect(gerr).NotTo(HaveOccurred())
					Expect(data).NotTo(BeNil(), "live record %d: membership names a missing posting entry", id)
				}
			}
			for id := range deleted {
				_, merr := spfreshReadMembership(tx, storage, tuple.Tuple{id})
				Expect(merr).To(MatchError(errSPFreshNotFound), "deleted record %d still has membership", id)
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Counters reconcile to exact posting sizes after quiescence.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
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
					Expect(count).To(Equal(int64(len(entries))), "fine counter drift on posting %d after quiescence", r.fineID)
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Recall: a sample of live records is findable at its own vector.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			maintainer, merr := store.getIndexMaintainer(idx)
			Expect(merr).NotTo(HaveOccurred())
			sbd := maintainer.(interface {
				ScanByDistance(TupleRange, []byte, ScanProperties) RecordCursor[*IndexEntry]
			})
			checked := 0
			for id, v := range live {
				if checked >= 40 {
					break
				}
				checked++
				// The op-mix formulas collide on the mod-200 grid, so several
				// live records can share one exact vector — k must cover every
				// tie or the target can be legitimately outranked by clones.
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
				if !contains(got, id) {
					// Diagnose before failing: where does the record live and
					// what state is its posting's centroid in?
					var diag string
					mem, _ := spfreshReadMembership(rtx.Transaction(), storage, tuple.Tuple{id})
					diag += fmt.Sprintf("membership=%v", mem)
					for _, fid := range mem {
						cell, ferr := spfreshFindCentroidCell(rtx.Transaction(), storage, fid)
						if ferr != nil {
							diag += fmt.Sprintf(" fine %d: cell NOT FOUND (%v)", fid, ferr)
							continue
						}
						row, rerr := spfreshReadCentroidForWrite(rtx.Transaction(), storage, cell, fid)
						if rerr != nil {
							diag += fmt.Sprintf(" fine %d@cell %d: row err %v", fid, cell, rerr)
							continue
						}
						diag += fmt.Sprintf(" fine %d@cell %d state=%d", fid, cell, row.state)
					}
					Fail(fmt.Sprintf("live record %d not findable at its own vector after churn: got %v; %s", id, got, diag))
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
