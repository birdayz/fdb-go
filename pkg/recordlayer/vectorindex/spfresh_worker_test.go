package vectorindex

import (
	"context"
	"time"

	"fdb.dev/pkg/recordlayer"

	. "github.com/onsi/ginkgo/v2"

	"fdb.dev/gen"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("SPFresh maintenance worker (RFC-156 reference runner)", func() {
	ctx := context.Background()

	workerMetadata := func() *recordlayer.RecordMetaDataBuilder {
		b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		return b
	}
	workerIndex := func(name string) *recordlayer.Index {
		idx := recordlayer.NewIndex(name, recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("quantity")))
		idx.Type = recordlayer.IndexTypeVectorSPFresh
		idx.Options = map[string]string{
			recordlayer.IndexOptionSPFreshNumDimensions: "2",
			recordlayer.IndexOptionSPFreshLmax:          "32",
			recordlayer.IndexOptionSPFreshCellTarget:    "4",
			recordlayer.IndexOptionSPFreshCellMax:       "8",
		}
		return idx
	}

	It("drains the rebalance queue on its cadence and stops cleanly on ctx cancel", func() {
		ks := specSubspace()
		idx := workerIndex("spf_worker")
		b := workerMetadata()
		b.AddIndex("Order", idx)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		storeBuilder := func(rtx *recordlayer.FDBRecordContext) (*recordlayer.FDBRecordStore, error) {
			return recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		}

		// Foreground insert-first a tight cluster past 2xLmax=64 so a split
		// task is enqueued — pending maintenance the worker must drain.
		_, err = sharedDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			for id := int64(1); id <= 120; id++ {
				if _, serr := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(id), Price: proto.Int32(int32(5 + id%6)), Quantity: proto.Int32(int32(5 + (id/6)%6)),
				}); serr != nil {
					return nil, serr
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		pending, err := SPFreshHasPendingMaintenance(ctx, sharedDB, storeBuilder, "spf_worker")
		Expect(err).NotTo(HaveOccurred())
		Expect(pending).To(BeTrue(), "120 clustered inserts must enqueue a split task")

		timer := recordlayer.NewStoreTimer()
		wctx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			done <- RunSPFreshMaintenance(wctx, sharedDB, SPFreshMaintenanceOptions{
				Tenants:       []SPFreshTenant{{StoreBuilder: storeBuilder, IndexName: "spf_worker"}},
				SweepInterval: 50 * time.Millisecond,
				DisableRefine: true,
				Sweep:         SPFreshSweepOptions{Timer: timer, MaxActionsPerTenant: 256, MaxRoundsPerTenant: 16},
			})
		}()

		// The worker's immediate first sweep + cadence must drain the queue.
		Eventually(func() bool {
			p, perr := SPFreshHasPendingMaintenance(ctx, sharedDB, storeBuilder, "spf_worker")
			Expect(perr).NotTo(HaveOccurred())
			return !p
		}, "15s", "100ms").Should(BeTrue(), "worker must drain the maintenance queue")

		cancel()
		Eventually(done, "5s").Should(Receive(BeNil()), "worker must return nil on ctx cancel")
		Expect(timer.GetCount(CountSPFreshSplits)).To(BeNumerically(">", 0),
			"worker must have executed at least one split (metrics wired)")
	})

	It("is a quiet no-op loop with no tenants and stops cleanly", func() {
		timer := recordlayer.NewStoreTimer()
		wctx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			done <- RunSPFreshMaintenance(wctx, sharedDB, SPFreshMaintenanceOptions{
				Tenants:       nil,
				SweepInterval: 20 * time.Millisecond,
				DisableRefine: true,
				Sweep:         SPFreshSweepOptions{Timer: timer},
			})
		}()
		time.Sleep(120 * time.Millisecond) // a few idle sweep passes
		cancel()
		Eventually(done, "5s").Should(Receive(BeNil()))
		Expect(timer.GetCount(CountSPFreshSplits)).To(BeZero(), "no tenants => no actions")
	})

	It("returns immediately (nil) when started with an already-canceled ctx", func() {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		Expect(RunSPFreshMaintenance(cctx, sharedDB, SPFreshMaintenanceOptions{DisableRefine: true})).To(Succeed())
	})
})
