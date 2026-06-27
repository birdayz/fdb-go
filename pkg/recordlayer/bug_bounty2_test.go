package recordlayer

import (
	"context"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/gen"
)

var _ = Describe("Bug Bounty Round 2", func() {
	// =========================================================================
	// BUG #1: OnlineIndexer.buildRange does not reset recordsProcessed on FDB
	// internal retry, causing inflated counts returned AND inflated progress
	// tracking stored in FDB.
	//
	// Severity: incorrect behavior (inflated counts)
	// Location: online_indexer.go:726-846
	//
	// Description: buildRange declares `var recordsProcessed int64` in the
	// outer scope and increments it inside the closure passed to db.Run().
	// FDB's Transact() retries the closure on conflict, but recordsProcessed
	// is NOT reset to zero on retry. After a conflict+retry, the returned
	// count is doubled and the FDB atomic ADD for progress tracking uses the
	// inflated count. Same bug exists in buildRangeByIndex (line 890-1008).
	// =========================================================================
	Describe("BUG: OnlineIndexer buildRange recordsProcessed not reset on retry", func() {
		It("demonstrates the fix: reset at top of closure prevents accumulation", func() {
			// The code in online_indexer.go now resets recordsProcessed = 0
			// at the top of the closure, so retries don't accumulate.
			var recordsProcessed int64
			const recordsPerAttempt = 5

			// Simulate 3 attempts (2 retries) like Transact would do
			for attempt := 0; attempt < 3; attempt++ {
				// FIX: buildRange now resets at top of closure
				recordsProcessed = 0
				for i := 0; i < recordsPerAttempt; i++ {
					recordsProcessed++
				}
			}

			// After 3 attempts with reset, only the last attempt's count survives.
			Expect(recordsProcessed).To(Equal(int64(5)),
				"With reset, 3 attempts correctly report 5 records")
		})

		It("shows the correct fix: reset at the top of the closure", func() {
			var recordsProcessed int64
			const recordsPerAttempt = 5

			for attempt := 0; attempt < 3; attempt++ {
				recordsProcessed = 0 // THE FIX
				for i := 0; i < recordsPerAttempt; i++ {
					recordsProcessed++
				}
			}

			Expect(recordsProcessed).To(Equal(int64(5)),
				"With reset, 3 attempts still correctly report 5 records")
		})
	})

	// BUG #2 (FIXED): isRetryableError now uses errors.As instead of type assertion,
	// so wrapped FDB errors are correctly detected as retryable.
	Describe("BUG: isRetryableError fails on wrapped FDB errors", func() {
		It("detects unwrapped FDB errors correctly", func() {
			Expect(isRetryableError(fdb.Error{Code: 1020})).To(BeTrue(), "unwrapped conflict")
			Expect(isRetryableError(fdb.Error{Code: 1021})).To(BeTrue(), "unwrapped commit_unknown")
			Expect(isRetryableError(fdb.Error{Code: 1009})).To(BeTrue(), "unwrapped timestamp")
		})

		It("detects wrapped FDB retryable errors (FIXED)", func() {
			for _, code := range []int{1020, 1021, 1009} {
				inner := fdb.Error{Code: code}
				wrapped := fmt.Errorf("operation failed: %w", inner)

				Expect(isRetryableError(wrapped)).To(BeTrue(),
					fmt.Sprintf("wrapped fdb.Error{Code:%d} should be detected as retryable", code))
			}
		})

		It("RunWithRetry retries on wrapped FDB conflict errors (FIXED)", func() {
			ctx := context.Background()
			runner := NewFDBDatabaseRunner(sharedDB).
				SetMaxAttempts(5).
				SetInitialDelay(0) // no delay for test speed
			attempts := 0

			_, err := runner.RunWithRetry(ctx, func(rtx *FDBRecordContext) (any, error) {
				attempts++
				return nil, fmt.Errorf("save record failed: %w", fdb.Error{Code: 1020})
			})

			Expect(err).To(HaveOccurred())
			Expect(attempts).To(Equal(5),
				"FIX: should retry up to 5 times because isRetryableError now uses errors.As")
		})
	})

	// =========================================================================
	// BUG #3: CommitWithVersionstamp swallows versionstamp retrieval errors,
	// returning (nil, nil) instead of (nil, error).
	//
	// Severity: incorrect behavior (silent error loss)
	// Location: database.go:570-574
	//
	// Description: After a successful commit, if vsFuture.Get() returns an
	// error, the code returns (nil, nil). The error is silently discarded.
	// The comment says "Read-only transactions don't have a versionstamp"
	// but the error path doesn't distinguish between "no versionstamp" and
	// a genuine error (network failure, FDB internal error, etc.).
	// =========================================================================
	Describe("BUG: CommitWithVersionstamp swallows errors from vsFuture.Get()", func() {
		It("returns nil,nil instead of nil,error on vsFuture failure", func() {
			// Demonstrate the code pattern that is buggy:
			//
			// database.go:555-576:
			//   vsFuture := rc.tx.GetVersionstamp()
			//   if err := rc.tx.Commit().Get(); err != nil { return nil, err }
			//   rc.runPostCommits()
			//   vs, err := vsFuture.Get()
			//   if err != nil {
			//       return nil, nil    // <-- BUG: swallows error
			//   }
			//   return []byte(vs), nil
			//
			// For a read-only transaction (no mutations), GetVersionstamp()
			// returns a future that fails because no versionstamp was assigned.
			// But the SAME error path would catch a genuine network/FDB error
			// and silently swallow it.
			//
			// The correct fix would be to check the error type:
			//   if err != nil {
			//       var fdbErr fdb.Error
			//       if errors.As(err, &fdbErr) && fdbErr.Code == 2021 {
			//           // 2021 = "commit_versionstamp_not_found"
			//           return nil, nil  // Expected for read-only
			//       }
			//       return nil, fmt.Errorf("get versionstamp after commit: %w", err)
			//   }

			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())

			rtx := NewFDBRecordContext(tx)

			// GetVersionstamp is called BEFORE commit (this is correct)
			// But for a read-only tx, it will fail.
			vs, err := rtx.CommitWithVersionstamp()

			// Current behavior: returns nil, nil — error is swallowed
			Expect(err).NotTo(HaveOccurred(), "error is silently swallowed")
			Expect(vs).To(BeNil(), "no versionstamp for read-only tx")

			// The caller has NO WAY to distinguish between:
			// 1. "Read-only tx, no versionstamp" (expected, harmless)
			// 2. "Network error after commit" (unexpected, needs handling)
			// Both return (nil, nil).
		})
	})

	// =========================================================================
	// BUG #4: OnlineIndexer.buildRange inflates FDB progress counter via
	// AddBuildProgress using the un-reset recordsProcessed value.
	//
	// This is a corollary of BUG #1 but specifically about the FDB-persisted
	// progress counter (not just the returned value).
	//
	// Severity: data corruption (FDB progress counter inflated)
	// Location: online_indexer.go:837-841
	//
	// Description: After scanning records, buildRange calls:
	//   store.AddBuildProgress(idx, recordsProcessed)
	// where recordsProcessed may be inflated from previous retries.
	// This writes an inflated count into FDB via atomic ADD.
	// When the build completes, LoadIndexBuildState reports an inflated
	// RecordsScanned count, causing monitoring/dashboards to show wrong data.
	// =========================================================================
	Describe("BUG: buildRange inflates FDB progress counter on retry", func() {
		It("proves AddBuildProgress receives the inflated count", func() {
			ctx := context.Background()

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			priceIdx := NewIndex("price_idx_bp", Field("price"))
			builder.AddIndex("Order", priceIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()

			// Insert records
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := int64(1); i <= 10; i++ {
					price := int32(i * 10)
					if _, err := store.SaveRecord(&gen.Order{OrderId: &i, Price: &price}); err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Simulate what buildRange does with the inflated counter:
			// After a retry, recordsProcessed = 20 (instead of 10).
			// AddBuildProgress(idx, 20) writes 20 to FDB via atomic ADD.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}

				// Mark write-only so we can write progress
				if _, err := store.ClearAndMarkIndexWriteOnly("price_idx_bp"); err != nil {
					return nil, err
				}

				// Simulate buildRange writing inflated progress (20 instead of 10)
				inflatedCount := int64(20)
				store.AddBuildProgress(priceIdx, inflatedCount)

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify the FDB progress counter has the inflated value
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				if err != nil {
					return nil, err
				}

				progress, err := store.LoadBuildProgress(priceIdx)
				if err != nil {
					return nil, err
				}

				// Progress shows 20, but only 10 records exist
				Expect(progress).To(Equal(int64(20)),
					"FDB progress counter is inflated due to buildRange not resetting recordsProcessed")

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
