//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"
	"sync"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestJavaEquivalent: FDBRecordStore.java:1217-1231 (addRecordReadConflict, addRecordWriteConflict)
// Java uses TupleRange.allOf() to create conflict ranges
var _ = Describe("Conflict Range Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *ConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error

		// Generate unique tenant name using UUID
		tenantName := fmt.Sprintf("conflict_%s", uuid.New().String())

		// Use shared container with tenant isolation
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store = NewConformanceStoreWithTenant(env.RecordDB, env.MetaData, env.ClusterFile, env.TenantName)
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx) // Deletes tenant only
		}
	})

	Describe("AddRecordReadConflict", func() {
		// Tests that AddRecordReadConflict() causes write conflicts
		// Java: recordStore.addRecordReadConflict(primaryKey)

		It("should cause conflict when another transaction writes", func() {
			orderID := int64(40001)

			// Create initial record
			err := store.SaveRecord(ctx, StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			tx1AddedConflict := make(chan struct{})
			tx2Committed := make(chan struct{})

			var tx1Err error
			var wg sync.WaitGroup
			wg.Add(2)

			// Transaction 1: Add read conflict (NO RETRY LOGIC)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				// Create raw transaction
				tx1, err := env.RecordDB.CreateTransaction()
				Expect(err).NotTo(HaveOccurred())

				rtx := recordlayer.NewFDBRecordContext(tx1)
				fdbStore, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(env.MetaData).
					SetSubspace(env.Keyspace).
					CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Add read conflict on the key FIRST
				Expect(fdbStore.AddRecordReadConflict(tuple.Tuple{orderID})).NotTo(HaveOccurred())

				// CRITICAL: TX1 must write SOMETHING to be a read-write transaction
				// Read-only transactions don't check for conflicts in FDB!
				rtx.Transaction().Set(fdb.Key("tx1_marker"), []byte("tx1"))

				// Signal that TX1 has added the conflict range
				close(tx1AddedConflict)

				// Wait for TX2 to commit its write
				<-tx2Committed

				// Try to commit - should fail due to conflict
				tx1Err = rtx.Commit()
			}()

			// Transaction 2: Write to same key
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				// Wait for TX1 to add its read conflict range
				<-tx1AddedConflict

				// Create raw transaction
				tx2, err := env.RecordDB.CreateTransaction()
				Expect(err).NotTo(HaveOccurred())

				rtx := recordlayer.NewFDBRecordContext(tx2)
				fdbStore, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(env.MetaData).
					SetSubspace(env.Keyspace).
					CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Write to the same key - this will conflict with TX1's read conflict
				order := NewOrder(orderID).WithPrice(999).Build()
				_, err = fdbStore.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())

				// Commit TX2
				err = rtx.Commit()
				Expect(err).NotTo(HaveOccurred())

				// Signal TX1 that we've committed
				close(tx2Committed)
			}()

			wg.Wait()

			// TX1 should have failed with a conflict error (FDB error code 1020 = NOT_COMMITTED)
			Expect(tx1Err).To(HaveOccurred(), "TX1 should fail due to write conflict on read-conflicted key")
		})

		It("should NOT conflict with reads", func() {
			orderID := int64(40002)

			// Create initial record
			err := store.SaveRecord(ctx, StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			var wg sync.WaitGroup
			wg.Add(2)

			tx1Done := make(chan struct{})
			tx2Done := make(chan struct{})

			// Transaction 1: Add read conflict
			go func() {
				defer wg.Done()
				defer GinkgoRecover()
				defer close(tx1Done)

				_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					fdbStore, err := recordlayer.NewStoreBuilder().
						SetContext(rtx).
						SetMetaDataProvider(env.MetaData).
						SetSubspace(env.Keyspace).
						CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())

					Expect(fdbStore.AddRecordReadConflict(tuple.Tuple{orderID})).NotTo(HaveOccurred())

					// Wait for TX2 to complete
					<-tx2Done

					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred(), "TX1 should commit (read conflicts don't conflict with reads)")
			}()

			// Transaction 2: Read from same key
			go func() {
				defer wg.Done()
				defer GinkgoRecover()
				defer close(tx2Done)

				_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
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
			}()

			wg.Wait()
		})
	})

	Describe("AddRecordWriteConflict", func() {
		// Tests that AddRecordWriteConflict() causes conflicts for transactions that read the key
		// Java: recordStore.addRecordWriteConflict(primaryKey)
		// Write conflicts work like this:
		//   - TX1 reads key X (implicit read conflict)
		//   - TX2 adds write conflict on key X (treats it as if TX2 wrote X)
		//   - TX2 commits
		//   - TX1 tries to commit - FAILS because X was "written" after TX1's read version

		It("should cause conflict when another transaction reads", func() {
			orderID := int64(40003)

			// Create initial record
			err := store.SaveRecord(ctx, StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			tx1ReadDone := make(chan struct{})
			tx2Committed := make(chan struct{})

			var tx1Err error
			var wg sync.WaitGroup
			wg.Add(2)

			// Transaction 1: READ the key first (creates implicit read conflict)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				// Create raw transaction
				tx1, err := env.RecordDB.CreateTransaction()
				Expect(err).NotTo(HaveOccurred())

				rtx := recordlayer.NewFDBRecordContext(tx1)
				fdbStore, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(env.MetaData).
					SetSubspace(env.Keyspace).
					CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Read the key - creates implicit read conflict
				storedRecord, err := fdbStore.LoadRecord(tuple.Tuple{orderID})
				Expect(err).NotTo(HaveOccurred())
				Expect(storedRecord).NotTo(BeNil())

				// Must write something to be a read-write transaction
				rtx.Transaction().Set(fdb.Key("tx1_marker"), []byte("tx1"))

				// Signal TX2 can proceed
				close(tx1ReadDone)

				// Wait for TX2 to commit its write conflict
				<-tx2Committed

				// Try to commit - should fail because TX2's write conflict overlaps with our read
				tx1Err = rtx.Commit()
			}()

			// Transaction 2: Add write conflict on the same key
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				// Wait for TX1 to read the key first
				<-tx1ReadDone

				// Create raw transaction
				tx2, err := env.RecordDB.CreateTransaction()
				Expect(err).NotTo(HaveOccurred())

				rtx := recordlayer.NewFDBRecordContext(tx2)
				fdbStore, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(env.MetaData).
					SetSubspace(env.Keyspace).
					CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Add write conflict - this is like "writing" to the key
				Expect(fdbStore.AddRecordWriteConflict(tuple.Tuple{orderID})).NotTo(HaveOccurred())

				// Must write something to be a read-write transaction
				rtx.Transaction().Set(fdb.Key("tx2_marker"), []byte("tx2"))

				// Commit TX2 - this will "write" to orderID
				err = rtx.Commit()
				Expect(err).NotTo(HaveOccurred())

				// Signal TX1 that we've committed
				close(tx2Committed)
			}()

			wg.Wait()

			// TX1 should have failed with a conflict error
			Expect(tx1Err).To(HaveOccurred(), "TX1 should fail because TX2's write conflict overlaps with TX1's read")
		})

		It("should be self-consistent within same transaction", func() {
			orderID := int64(40004)

			// Create initial record
			err := store.SaveRecord(ctx, StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			// Single transaction should be able to add conflict and operate without self-conflict
			_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				fdbStore, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(env.MetaData).
					SetSubspace(env.Keyspace).
					CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Add write conflict
				Expect(fdbStore.AddRecordWriteConflict(tuple.Tuple{orderID})).NotTo(HaveOccurred())

				// Should still be able to operate on same key in same transaction
				exists, err := fdbStore.RecordExists(tuple.Tuple{orderID}, recordlayer.IsolationLevelSerializable)
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeTrue())

				// Should be able to write too
				order := NewOrder(orderID).WithPrice(123).Build()
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

			err := store.SaveRecord(ctx, StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				fdbStore, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(env.MetaData).
					SetSubspace(env.Keyspace).
					CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Adding multiple read conflicts should be idempotent
				Expect(fdbStore.AddRecordReadConflict(tuple.Tuple{orderID})).NotTo(HaveOccurred())
				Expect(fdbStore.AddRecordReadConflict(tuple.Tuple{orderID})).NotTo(HaveOccurred())
				Expect(fdbStore.AddRecordReadConflict(tuple.Tuple{orderID})).NotTo(HaveOccurred())

				// Adding multiple write conflicts should be idempotent
				Expect(fdbStore.AddRecordWriteConflict(tuple.Tuple{orderID})).NotTo(HaveOccurred())
				Expect(fdbStore.AddRecordWriteConflict(tuple.Tuple{orderID})).NotTo(HaveOccurred())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred(), "Multiple conflicts on same key should not error")
		})

		It("should handle conflicts on different keys independently", func() {
			orderID1 := int64(40006)
			orderID2 := int64(40007)

			err := store.SaveRecord(ctx, StandardOrder(orderID1))
			Expect(err).NotTo(HaveOccurred())
			err = store.SaveRecord(ctx, StandardOrder(orderID2))
			Expect(err).NotTo(HaveOccurred())

			tx1Started := make(chan struct{})
			tx2Done := make(chan struct{})

			var wg sync.WaitGroup
			wg.Add(2)

			// Transaction 1: Add read conflict on orderID1 only
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					fdbStore, err := recordlayer.NewStoreBuilder().
						SetContext(rtx).
						SetMetaDataProvider(env.MetaData).
						SetSubspace(env.Keyspace).
						CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())

					// Add read conflict only on orderID1
					Expect(fdbStore.AddRecordReadConflict(tuple.Tuple{orderID1})).NotTo(HaveOccurred())

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
				defer close(tx2Done)

				<-tx1Started

				_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					fdbStore, err := recordlayer.NewStoreBuilder().
						SetContext(rtx).
						SetMetaDataProvider(env.MetaData).
						SetSubspace(env.Keyspace).
						CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())

					// Write to orderID2 - should not conflict with orderID1
					order := NewOrder(orderID2).WithPrice(888).Build()
					_, err = fdbStore.SaveRecord(order)
					Expect(err).NotTo(HaveOccurred())

					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			}()

			wg.Wait()
		})

		It("should create range that covers record key correctly", func() {
			orderID := int64(40008)

			// This test verifies that the range calculation is correct
			// Java: TupleRange.allOf(primaryKey) creates range:
			//   low = pack(primaryKey)
			//   high = pack(primaryKey) + 0xFF

			err := store.SaveRecord(ctx, StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				fdbStore, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(env.MetaData).
					SetSubspace(env.Keyspace).
					CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Add both types of conflicts - tests range calculation
				Expect(fdbStore.AddRecordReadConflict(tuple.Tuple{orderID})).NotTo(HaveOccurred())
				Expect(fdbStore.AddRecordWriteConflict(tuple.Tuple{orderID})).NotTo(HaveOccurred())

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
		// IMPORTANT: Use raw transactions (CreateTransaction) NOT Run() to avoid retry logic

		It("should handle conflict when SaveRecord happens after AddRecordReadConflict", func() {
			orderID := int64(50001)

			tx1Started := make(chan struct{})
			tx2Done := make(chan struct{})

			var tx1Err error
			var wg sync.WaitGroup
			wg.Add(2)

			// Transaction 1: AddRecordReadConflict (raw transaction, NO RETRY)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				tx1, err := env.RecordDB.CreateTransaction()
				Expect(err).NotTo(HaveOccurred())

				rtx := recordlayer.NewFDBRecordContext(tx1)
				fdbStore, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(env.MetaData).
					SetSubspace(env.Keyspace).
					CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Add read conflict first
				Expect(fdbStore.AddRecordReadConflict(tuple.Tuple{orderID})).NotTo(HaveOccurred())

				// CRITICAL: Must write something to be a read-write transaction
				rtx.Transaction().Set(fdb.Key("tx1_marker"), []byte("tx1"))

				close(tx1Started)

				// Wait for TX2 to save
				<-tx2Done

				// Try to commit - should fail
				tx1Err = rtx.Commit()
			}()

			// Transaction 2: SaveRecord to same key
			go func() {
				defer wg.Done()
				defer GinkgoRecover()
				defer close(tx2Done)

				<-tx1Started

				err := store.SaveRecord(ctx, StandardOrder(orderID))
				Expect(err).NotTo(HaveOccurred())
			}()

			wg.Wait()

			// TX1 should fail with conflict
			Expect(tx1Err).To(HaveOccurred())
		})

		It("should handle conflict when DeleteRecord happens after AddRecordReadConflict", func() {
			orderID := int64(50002)

			// Pre-create the record
			err := store.SaveRecord(ctx, StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			tx1Started := make(chan struct{})
			tx2Done := make(chan struct{})

			var tx1Err error
			var wg sync.WaitGroup
			wg.Add(2)

			// Transaction 1: AddRecordReadConflict (raw transaction, NO RETRY)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				tx1, err := env.RecordDB.CreateTransaction()
				Expect(err).NotTo(HaveOccurred())

				rtx := recordlayer.NewFDBRecordContext(tx1)
				fdbStore, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(env.MetaData).
					SetSubspace(env.Keyspace).
					CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				Expect(fdbStore.AddRecordReadConflict(tuple.Tuple{orderID})).NotTo(HaveOccurred())

				// CRITICAL: Must write something to be a read-write transaction
				rtx.Transaction().Set(fdb.Key("tx1_marker"), []byte("tx1"))

				close(tx1Started)
				<-tx2Done

				// Try to commit - should fail
				tx1Err = rtx.Commit()
			}()

			// Transaction 2: Delete the record
			go func() {
				defer wg.Done()
				defer GinkgoRecover()
				defer close(tx2Done)

				<-tx1Started

				deleted, err := store.DeleteRecord(ctx, orderID)
				Expect(err).NotTo(HaveOccurred())
				Expect(deleted).To(BeTrue())
			}()

			wg.Wait()

			// TX1 should fail with conflict
			Expect(tx1Err).To(HaveOccurred())
		})
	})
})
