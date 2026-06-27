package recordlayer

// Concurrent stress tests for FDBRecordStore.
// Verify that concurrent Save/Load/Delete/Scan operations don't corrupt
// data or produce panics.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("Store Stress", func() {
	var (
		ctx context.Context
		md  *RecordMetaData
	)

	BeforeEach(func() {
		ctx = context.Background()
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.SetRecordCountKey(&EmptyKeyExpression{})
		var err error
		md, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())
	})

	It("handles concurrent saves to different keys", func() {
		ss := specSubspace()
		numWorkers := 10
		recordsPerWorker := 5

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			var wg sync.WaitGroup
			var errors atomic.Int64

			for w := 0; w < numWorkers; w++ {
				wg.Add(1)
				go func(worker int) {
					defer wg.Done()
					for i := 0; i < recordsPerWorker; i++ {
						id := int64(worker*1000 + i)
						_, err := store.SaveRecord(&gen.Order{
							OrderId: proto.Int64(id),
							Price:   proto.Int32(int32(id)),
						})
						if err != nil {
							errors.Add(1)
						}
					}
				}(w)
			}
			wg.Wait()
			Expect(errors.Load()).To(Equal(int64(0)))

			// Verify all records exist.
			count, err := store.GetRecordCount()
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(int64(numWorkers * recordsPerWorker)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("handles concurrent save and scan", func() {
		ss := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Pre-populate 20 records.
			for i := int64(0); i < 20; i++ {
				_, err := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 10)),
				})
				if err != nil {
					return nil, err
				}
			}

			// Concurrent: 5 goroutines scan, 5 goroutines save new records.
			var wg sync.WaitGroup
			var scanErrors, saveErrors atomic.Int64

			// Scanners.
			for s := 0; s < 5; s++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					cursor := store.ScanRecords(nil, ForwardScan())
					count := 0
					for _, err := range Seq2(cursor, ctx) {
						if err != nil {
							scanErrors.Add(1)
							return
						}
						count++
					}
					if count < 20 {
						// Should see at least the pre-populated records.
						// May see more if saves completed before scan.
						scanErrors.Add(1)
					}
				}()
			}

			// Writers.
			for w := 0; w < 5; w++ {
				wg.Add(1)
				go func(worker int) {
					defer wg.Done()
					for i := 0; i < 5; i++ {
						id := int64(100 + worker*100 + i)
						_, err := store.SaveRecord(&gen.Order{
							OrderId: proto.Int64(id),
							Price:   proto.Int32(int32(id)),
						})
						if err != nil {
							saveErrors.Add(1)
						}
					}
				}(w)
			}

			wg.Wait()
			Expect(scanErrors.Load()).To(Equal(int64(0)))
			Expect(saveErrors.Load()).To(Equal(int64(0)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("handles save-delete-load race", func() {
		ss := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			if err != nil {
				return nil, err
			}

			// Pre-save a record.
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(42),
				Price:   proto.Int32(100),
			})
			if err != nil {
				return nil, err
			}

			// Concurrent: one goroutine deletes, another loads.
			var wg sync.WaitGroup
			var loadResult atomic.Value
			var deleteResult atomic.Value

			wg.Add(2)
			go func() {
				defer wg.Done()
				deleted, err := store.DeleteRecord(tuple.Tuple{int64(42)})
				if err != nil {
					deleteResult.Store(fmt.Sprintf("error: %v", err))
				} else {
					deleteResult.Store(fmt.Sprintf("deleted=%v", deleted))
				}
			}()

			go func() {
				defer wg.Done()
				rec, err := store.LoadRecord(tuple.Tuple{int64(42)})
				if err != nil {
					loadResult.Store(fmt.Sprintf("error: %v", err))
				} else if rec == nil {
					loadResult.Store("nil")
				} else {
					loadResult.Store("found")
				}
			}()

			wg.Wait()

			// Both should complete without panic. Result is either:
			// - Load found the record, delete succeeded (race: load ran first)
			// - Load got nil, delete succeeded (race: delete ran first)
			// - Load found the record, delete already gone (delete ran, but load
			//   sees pre-delete snapshot via RYW)
			GinkgoWriter.Printf("delete: %v, load: %v\n",
				deleteResult.Load(), loadResult.Load())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// tuple import used above for tuple.Tuple{int64(42)}
