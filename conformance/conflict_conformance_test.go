package conformance_test

import (
	"context"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/conformance/helpers"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// TestJavaEquivalent: FDBRecordStore.java:1217-1231 (addRecordReadConflict, addRecordWriteConflict)
// Java uses TupleRange.allOf() to create conflict ranges
// PENDING: AddRecordReadConflict and AddRecordWriteConflict methods not yet implemented
var _ = PDescribe("Conflict Range Conformance", func() {
	var (
		ctx   context.Context
		env   *helpers.TestEnvironment
		store *helpers.ConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		env, err = helpers.SetupTestEnvironment(ctx, "conflict_conformance")
		Expect(err).NotTo(HaveOccurred())

		store = helpers.NewConformanceStore(env.RecordDB, env.MetaData, env.Keyspace, env.ClusterFile)
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("AddRecordReadConflict", func() {
		// Tests that AddRecordReadConflict() causes write conflicts
		// Java: recordStore.addRecordReadConflict(primaryKey)

		It("should cause conflict when another transaction writes", func() {
			orderID := int64(40001)

			// Create initial record
			err := store.SaveRecord(ctx, helpers.StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			tx1Started := make(chan struct{})
			tx2Committed := make(chan struct{})

			var tx1Err error
			var wg sync.WaitGroup
			wg.Add(2)

			// Transaction 1: Add read conflict
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				tx1Err = func() error {
					_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
						fdbStore, err := recordlayer.NewStoreBuilder().
							SetContext(rtx).
							SetMetaDataProvider(env.MetaData).
							SetSubspace(env.Keyspace).
							CreateOrOpen()
						if err != nil {
							return nil, err
						}

						// Add read conflict on the key
						fdbStore.AddRecordReadConflict(tuple.Tuple{orderID})

						// Signal TX2 can proceed
						close(tx1Started)

						// Wait for TX2 to commit its write
						<-tx2Committed

						// Try to commit - should fail due to conflict
						return nil, nil
					})
					return err
				}()
			}()

			// Transaction 2: Write to same key
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				// Wait for TX1 to add read conflict
				<-tx1Started

				_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
					fdbStore, err := recordlayer.NewStoreBuilder().
						SetContext(rtx).
						SetMetaDataProvider(env.MetaData).
						SetSubspace(env.Keyspace).
						CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())

					// Write to the same key - this will conflict with TX1's read conflict
					order := helpers.NewOrder(orderID).WithPrice(999).Build()
					_, err = fdbStore.SaveRecord(order)
					Expect(err).NotTo(HaveOccurred())

					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())

				// Signal TX1 that we've committed
				close(tx2Committed)
			}()

			wg.Wait()

			// TX1 should have failed with a conflict error
			Expect(tx1Err).To(HaveOccurred(), "TX1 should fail due to write conflict on read-conflicted key")
		})

		It("should NOT conflict with reads", func() {
			orderID := int64(40002)

			// Create initial record
			err := store.SaveRecord(ctx, helpers.StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			var wg sync.WaitGroup
			wg.Add(2)

			tx1Done := make(chan struct{})
			tx2Done := make(chan struct{})

			// Transaction 1: Add read conflict
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
					fdbStore, err := recordlayer.NewStoreBuilder().
						SetContext(rtx).
						SetMetaDataProvider(env.MetaData).
						SetSubspace(env.Keyspace).
						CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())

					fdbStore.AddRecordReadConflict(tuple.Tuple{orderID})

					// Wait for TX2 to complete
					<-tx2Done

					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred(), "TX1 should commit (read conflicts don't conflict with reads)")
				close(tx1Done)
			}()

			// Transaction 2: Read from same key
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
					fdbStore, err := recordlayer.NewStoreBuilder().
						SetContext(rtx).
						SetMetaDataProvider(env.MetaData).
						SetSubspace(env.Keyspace).
						CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())

					// Just read - should not conflict
					storedRecord, err := fdbStore.LoadRecord(tuple.Tuple{orderID})
					Expect(err).NotTo(HaveOccurred())
					Expect(storedRecord).NotTo(BeNil())

					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
				close(tx2Done)
			}()

			wg.Wait()
			<-tx1Done
		})
	})

	Describe("AddRecordWriteConflict", func() {
		// Tests that AddRecordWriteConflict() causes read conflicts
		// Java: recordStore.addRecordWriteConflict(primaryKey)

		It("should cause conflict when another transaction reads", func() {
			orderID := int64(40003)

			// Create initial record
			err := store.SaveRecord(ctx, helpers.StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			tx1Started := make(chan struct{})
			tx2Committed := make(chan struct{})

			var tx1Err error
			var wg sync.WaitGroup
			wg.Add(2)

			// Transaction 1: Add write conflict
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				tx1Err = func() error {
					_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
						fdbStore, err := recordlayer.NewStoreBuilder().
							SetContext(rtx).
							SetMetaDataProvider(env.MetaData).
							SetSubspace(env.Keyspace).
							CreateOrOpen()
						if err != nil {
							return nil, err
						}

						// Add write conflict on the key
						fdbStore.AddRecordWriteConflict(tuple.Tuple{orderID})

						// Signal TX2 can proceed
						close(tx1Started)

						// Wait for TX2 to commit its read
						<-tx2Committed

						// Try to commit - should fail due to conflict
						return nil, nil
					})
					return err
				}()
			}()

			// Transaction 2: Read from same key
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				// Wait for TX1 to add write conflict
				<-tx1Started

				_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
					fdbStore, err := recordlayer.NewStoreBuilder().
						SetContext(rtx).
						SetMetaDataProvider(env.MetaData).
						SetSubspace(env.Keyspace).
						CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())

					// Read from the same key - this will conflict with TX1's write conflict
					storedRecord, err := fdbStore.LoadRecord(tuple.Tuple{orderID})
					Expect(err).NotTo(HaveOccurred())
					Expect(storedRecord).NotTo(BeNil())

					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())

				// Signal TX1 that we've committed
				close(tx2Committed)
			}()

			wg.Wait()

			// TX1 should have failed with a conflict error
			Expect(tx1Err).To(HaveOccurred(), "TX1 should fail due to read conflict on write-conflicted key")
		})

		It("should be self-consistent within same transaction", func() {
			orderID := int64(40004)

			// Create initial record
			err := store.SaveRecord(ctx, helpers.StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			// Single transaction should be able to add conflict and operate without self-conflict
			_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
				fdbStore, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(env.MetaData).
					SetSubspace(env.Keyspace).
					CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Add write conflict
				fdbStore.AddRecordWriteConflict(tuple.Tuple{orderID})

				// Should still be able to operate on same key in same transaction
				exists, err := fdbStore.RecordExists(tuple.Tuple{orderID}, recordlayer.IsolationLevelSerializable)
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeTrue())

				// Should be able to write too
				order := helpers.NewOrder(orderID).WithPrice(123).Build()
				_, err = fdbStore.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred(), "Transaction should not conflict with itself")
		})
	})

	Describe("Conflict Range Correctness", func() {
		// Tests that conflict ranges match Java's TupleRange.allOf() behavior
		// Java creates ranges with RANGE_INCLUSIVE endpoints

		It("should handle multiple conflicts on same key idempotently", func() {
			orderID := int64(40005)

			err := store.SaveRecord(ctx, helpers.StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
				fdbStore, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(env.MetaData).
					SetSubspace(env.Keyspace).
					CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Adding multiple read conflicts should be idempotent
				fdbStore.AddRecordReadConflict(tuple.Tuple{orderID})
				fdbStore.AddRecordReadConflict(tuple.Tuple{orderID})
				fdbStore.AddRecordReadConflict(tuple.Tuple{orderID})

				// Adding multiple write conflicts should be idempotent
				fdbStore.AddRecordWriteConflict(tuple.Tuple{orderID})
				fdbStore.AddRecordWriteConflict(tuple.Tuple{orderID})

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred(), "Multiple conflicts on same key should not error")
		})

		It("should handle conflicts on different keys independently", func() {
			orderID1 := int64(40006)
			orderID2 := int64(40007)

			err := store.SaveRecord(ctx, helpers.StandardOrder(orderID1))
			Expect(err).NotTo(HaveOccurred())
			err = store.SaveRecord(ctx, helpers.StandardOrder(orderID2))
			Expect(err).NotTo(HaveOccurred())

			tx1Started := make(chan struct{})
			tx2Done := make(chan struct{})

			var wg sync.WaitGroup
			wg.Add(2)

			// Transaction 1: Add read conflict on orderID1 only
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
					fdbStore, err := recordlayer.NewStoreBuilder().
						SetContext(rtx).
						SetMetaDataProvider(env.MetaData).
						SetSubspace(env.Keyspace).
						CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())

					// Add read conflict only on orderID1
					fdbStore.AddRecordReadConflict(tuple.Tuple{orderID1})

					close(tx1Started)

					// Wait for TX2 to write to orderID2
					<-tx2Done

					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred(), "TX1 should commit (no conflict on different key)")
			}()

			// Transaction 2: Write to orderID2 (different key, should not conflict)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				<-tx1Started

				_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
					fdbStore, err := recordlayer.NewStoreBuilder().
						SetContext(rtx).
						SetMetaDataProvider(env.MetaData).
						SetSubspace(env.Keyspace).
						CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())

					// Write to orderID2 - should not conflict with orderID1
					order := helpers.NewOrder(orderID2).WithPrice(888).Build()
					_, err = fdbStore.SaveRecord(order)
					Expect(err).NotTo(HaveOccurred())

					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())

				close(tx2Done)
			}()

			wg.Wait()
		})

		It("should create range that covers record key correctly", func() {
			orderID := int64(40008)

			// This test verifies that the range calculation is correct
			// Java: TupleRange.allOf(primaryKey) creates range:
			//   low = pack(primaryKey)
			//   high = pack(primaryKey) + 0xFF

			err := store.SaveRecord(ctx, helpers.StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
				fdbStore, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(env.MetaData).
					SetSubspace(env.Keyspace).
					CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Add both types of conflicts - tests range calculation
				fdbStore.AddRecordReadConflict(tuple.Tuple{orderID})
				fdbStore.AddRecordWriteConflict(tuple.Tuple{orderID})

				// If ranges were wrong, this would likely error or cause issues
				// The fact that we can still operate is a good sign
				exists, err := fdbStore.RecordExists(tuple.Tuple{orderID}, recordlayer.IsolationLevelSerializable)
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeTrue())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Conflict Behavior with RecordStore Operations", func() {
		// Integration tests: How conflicts interact with actual CRUD operations

		It("should handle conflict when SaveRecord happens after AddRecordReadConflict", func() {
			orderID := int64(50001)

			tx1Started := make(chan struct{})
			tx2Done := make(chan struct{})

			var tx1Err error
			var wg sync.WaitGroup
			wg.Add(2)

			// Transaction 1: AddRecordReadConflict, wait, then try to proceed
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				tx1Err = func() error {
					_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
						fdbStore, err := recordlayer.NewStoreBuilder().
							SetContext(rtx).
							SetMetaDataProvider(env.MetaData).
							SetSubspace(env.Keyspace).
							CreateOrOpen()
						if err != nil {
							return nil, err
						}

						// Add read conflict first
						fdbStore.AddRecordReadConflict(tuple.Tuple{orderID})

						close(tx1Started)

						// Wait for TX2 to save
						<-tx2Done

						return nil, nil
					})
					return err
				}()
			}()

			// Transaction 2: SaveRecord to same key
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				<-tx1Started

				err := store.SaveRecord(ctx, helpers.StandardOrder(orderID))
				Expect(err).NotTo(HaveOccurred())

				close(tx2Done)
			}()

			wg.Wait()

			// TX1 should fail
			Expect(tx1Err).To(HaveOccurred())
		})

		It("should handle conflict when DeleteRecord happens after AddRecordReadConflict", func() {
			orderID := int64(50002)

			// Pre-create the record
			err := store.SaveRecord(ctx, helpers.StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			tx1Started := make(chan struct{})
			tx2Done := make(chan struct{})

			var tx1Err error
			var wg sync.WaitGroup
			wg.Add(2)

			// Transaction 1: AddRecordReadConflict
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				tx1Err = func() error {
					_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
						fdbStore, err := recordlayer.NewStoreBuilder().
							SetContext(rtx).
							SetMetaDataProvider(env.MetaData).
							SetSubspace(env.Keyspace).
							CreateOrOpen()
						if err != nil {
							return nil, err
						}

						fdbStore.AddRecordReadConflict(tuple.Tuple{orderID})

						close(tx1Started)
						<-tx2Done

						return nil, nil
					})
					return err
				}()
			}()

			// Transaction 2: Delete the record
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				<-tx1Started

				deleted, err := store.DeleteRecord(ctx, orderID)
				Expect(err).NotTo(HaveOccurred())
				Expect(deleted).To(BeTrue())

				close(tx2Done)
			}()

			wg.Wait()

			// TX1 should fail
			Expect(tx1Err).To(HaveOccurred())
		})
	})
})
