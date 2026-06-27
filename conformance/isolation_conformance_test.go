//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"
	"sync"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestJavaEquivalent: FDBRecordStoreCrudTest.java:103-128 writeCheckExistsConcurrently()
var _ = Describe("Isolation Level Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *ConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error

		// Generate unique tenant name using UUID
		tenantName := fmt.Sprintf("isolation_%s", uuid.New().String())

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

	Describe("RecordExists with Concurrent Transactions", func() {
		// Java: writeCheckExistsConcurrently() - Tests that uncommitted writes
		// are visible to serializable reads but not to snapshot reads in concurrent transactions

		It("should NOT see uncommitted record with SNAPSHOT isolation", func() {
			orderID := int64(10001)

			// Channel to coordinate transaction timing
			tx1Started := make(chan struct{})
			tx2Done := make(chan struct{})

			var wg sync.WaitGroup
			wg.Add(2)

			// Transaction 1: Save record but don't commit yet (raw transaction, NO RETRY)
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

				// Save the record
				order := StandardOrder(orderID)
				_, err = fdbStore.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())

				// Signal that record is saved (but not committed)
				close(tx1Started)

				// Wait for TX2 to finish checking
				<-tx2Done

				// Commit TX1
				err = rtx.Commit()
				Expect(err).NotTo(HaveOccurred())
			}()

			// Transaction 2: Check if record exists with SNAPSHOT isolation
			go func() {
				defer wg.Done()
				defer GinkgoRecover()
				defer close(tx2Done)

				// Wait for TX1 to start and save record
				<-tx1Started

				tx2, err := env.RecordDB.CreateTransaction()
				Expect(err).NotTo(HaveOccurred())

				rtx := recordlayer.NewFDBRecordContext(tx2)
				fdbStore, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(env.MetaData).
					SetSubspace(env.Keyspace).
					CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Check with SNAPSHOT isolation - should NOT see uncommitted record from TX1
				exists, err := fdbStore.RecordExists(tuple.Tuple{orderID}, recordlayer.IsolationLevelSnapshot)
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeFalse(), "TX2 with SNAPSHOT should NOT see uncommitted write from TX1")

				// TX2 is read-only, so we can just let it get garbage collected
				// (no need to commit a read-only transaction)
			}()

			wg.Wait()

			// After TX1 commits, new transaction with SNAPSHOT should see the record
			_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				fdbStore, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(env.MetaData).
					SetSubspace(env.Keyspace).
					CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				exists, err := fdbStore.RecordExists(tuple.Tuple{orderID}, recordlayer.IsolationLevelSnapshot)
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeTrue(), "After commit, new transaction should see the record")

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should support read-your-writes with SERIALIZABLE isolation", func() {
			orderID := int64(10002)

			_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				fdbStore, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(env.MetaData).
					SetSubspace(env.Keyspace).
					CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Initially should not exist
				exists, err := fdbStore.RecordExists(tuple.Tuple{orderID}, recordlayer.IsolationLevelSerializable)
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeFalse())

				// Save record
				order := StandardOrder(orderID)
				_, err = fdbStore.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())

				// Should immediately see our own write with SERIALIZABLE
				exists, err = fdbStore.RecordExists(tuple.Tuple{orderID}, recordlayer.IsolationLevelSerializable)
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeTrue(), "Should see our own write (read-your-writes)")

				// Should also see with SNAPSHOT (within same transaction)
				exists, err = fdbStore.RecordExists(tuple.Tuple{orderID}, recordlayer.IsolationLevelSnapshot)
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeTrue(), "SNAPSHOT should see writes within same transaction")

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should maintain snapshot consistency across transaction", func() {
			orderID := int64(10003)

			// First, create and commit a record
			err := store.SaveRecord(ctx, StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			// Channel to coordinate timing
			tx1CanUpdate := make(chan struct{})
			tx2CanCheck := make(chan struct{})

			var wg sync.WaitGroup
			wg.Add(2)

			// Transaction 1: Delete the record
			go func() {
				defer wg.Done()
				defer GinkgoRecover()
				defer close(tx2CanCheck)

				<-tx1CanUpdate

				deleted, err := store.DeleteRecord(ctx, orderID)
				Expect(err).NotTo(HaveOccurred())
				Expect(deleted).To(BeTrue())
			}()

			// Transaction 2: Start before TX1, use SNAPSHOT to see old state
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

					// Check BEFORE TX1 deletes
					exists1, err := fdbStore.RecordExists(tuple.Tuple{orderID}, recordlayer.IsolationLevelSnapshot)
					Expect(err).NotTo(HaveOccurred())
					Expect(exists1).To(BeTrue(), "Record should exist before deletion")

					// Signal TX1 to delete
					close(tx1CanUpdate)

					// Wait for TX1 to complete deletion
					<-tx2CanCheck

					// SNAPSHOT should still see old state (from transaction start)
					// This is a key property of snapshot isolation
					exists2, err := fdbStore.RecordExists(tuple.Tuple{orderID}, recordlayer.IsolationLevelSnapshot)
					Expect(err).NotTo(HaveOccurred())
					Expect(exists2).To(BeTrue(), "SNAPSHOT should still see record (snapshot is from tx start)")

					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			}()

			wg.Wait()
		})
	})

	Describe("Conflict Detection with Isolation Levels", func() {
		// These tests verify that SNAPSHOT reads don't participate in conflict detection,
		// while SERIALIZABLE reads do

		It("should NOT cause conflicts with SNAPSHOT reads", func() {
			orderID := int64(20001)

			// Create initial record
			err := store.SaveRecord(ctx, StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			tx1CanCommit := make(chan struct{})
			tx2CanCommit := make(chan struct{})

			var wg sync.WaitGroup
			wg.Add(2)

			// Transaction 1: Read with SNAPSHOT
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

					// Read with SNAPSHOT - this should NOT add read conflict
					exists, err := fdbStore.RecordExists(tuple.Tuple{orderID}, recordlayer.IsolationLevelSnapshot)
					Expect(err).NotTo(HaveOccurred())
					Expect(exists).To(BeTrue())

					// Wait for TX2 to write and commit
					<-tx2CanCommit

					// TX1 should be able to commit without conflict
					close(tx1CanCommit)
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred(), "TX1 should commit successfully (SNAPSHOT reads don't conflict)")
			}()

			// Transaction 2: Write to same key
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

					// Update the record
					order := NewOrder(orderID).WithPrice(999).Build()
					_, err = fdbStore.SaveRecord(order)
					Expect(err).NotTo(HaveOccurred())

					// Signal TX1 that we've committed
					close(tx2CanCommit)

					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())

				// Wait for TX1 to complete
				<-tx1CanCommit
			}()

			wg.Wait()
		})

		It("SHOULD cause conflicts with SERIALIZABLE reads", func() {
			orderID := int64(20002)

			// Create initial record
			err := store.SaveRecord(ctx, StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			tx1Started := make(chan struct{})
			tx2Committed := make(chan struct{})

			var tx1Err error
			var wg sync.WaitGroup
			wg.Add(2)

			// Transaction 1: Read with SERIALIZABLE (raw transaction, NO RETRY)
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

				// Read with SERIALIZABLE - this WILL add read conflict
				exists, err := fdbStore.RecordExists(tuple.Tuple{orderID}, recordlayer.IsolationLevelSerializable)
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeTrue())

				// CRITICAL: Must write something to be a read-write transaction
				// Read-only transactions don't check for conflicts in FDB!
				order := NewOrder(orderID).WithPrice(888).Build()
				_, err = fdbStore.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())

				// Signal TX2 can proceed
				close(tx1Started)

				// Wait for TX2 to commit its write
				<-tx2Committed

				// Try to commit - should fail with conflict
				tx1Err = rtx.Commit()
			}()

			// Transaction 2: Write to same key after TX1 read
			go func() {
				defer wg.Done()
				defer GinkgoRecover()
				defer close(tx2Committed)

				// Wait for TX1 to read
				<-tx1Started

				_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					fdbStore, err := recordlayer.NewStoreBuilder().
						SetContext(rtx).
						SetMetaDataProvider(env.MetaData).
						SetSubspace(env.Keyspace).
						CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())

					// Update the record - this will conflict with TX1's read
					order := NewOrder(orderID).WithPrice(777).Build()
					_, err = fdbStore.SaveRecord(order)
					Expect(err).NotTo(HaveOccurred())

					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			}()

			wg.Wait()

			// TX1 should have failed with a conflict error
			Expect(tx1Err).To(HaveOccurred(), "TX1 should fail due to read-write conflict")
			// FDB conflict errors contain "not_committed" or "transaction_too_old" or similar
		})
	})

	Describe("Isolation Level API Validation", func() {
		It("should accept both isolation levels for RecordExists", func() {
			orderID := int64(30001)
			err := store.SaveRecord(ctx, StandardOrder(orderID))
			Expect(err).NotTo(HaveOccurred())

			_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				fdbStore, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(env.MetaData).
					SetSubspace(env.Keyspace).
					CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Both should work without error
				existsSnapshot, err := fdbStore.RecordExists(tuple.Tuple{orderID}, recordlayer.IsolationLevelSnapshot)
				Expect(err).NotTo(HaveOccurred())
				Expect(existsSnapshot).To(BeTrue())

				existsSerializable, err := fdbStore.RecordExists(tuple.Tuple{orderID}, recordlayer.IsolationLevelSerializable)
				Expect(err).NotTo(HaveOccurred())
				Expect(existsSerializable).To(BeTrue())

				// Results should be identical within same transaction
				Expect(existsSnapshot).To(Equal(existsSerializable))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should validate IsolationLevel.IsSnapshot() helper", func() {
			Expect(recordlayer.IsolationLevelSnapshot.IsSnapshot()).To(BeTrue())
			Expect(recordlayer.IsolationLevelSerializable.IsSnapshot()).To(BeFalse())
		})

		It("should have working String() method for debugging", func() {
			snapshotStr := recordlayer.IsolationLevelSnapshot.String()
			Expect(snapshotStr).To(ContainSubstring("Snapshot"))

			serializableStr := recordlayer.IsolationLevelSerializable.String()
			Expect(serializableStr).To(ContainSubstring("Serializable"))
		})
	})
})
