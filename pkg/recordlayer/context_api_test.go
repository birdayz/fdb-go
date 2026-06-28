package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/pkg/fdbgo/fdb"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("FDBRecordContext and FDBDatabase APIs", func() {
	ctx := context.Background()

	Describe("GetApproximateTransactionSize", func() {
		It("returns a small value for an empty transaction", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				size, err := rtx.GetApproximateTransactionSize()
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(BeNumerically(">=", 0))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("increases after writing data", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				sizeBefore, err := rtx.GetApproximateTransactionSize()
				Expect(err).NotTo(HaveOccurred())

				ks := specSubspace()
				rtx.Transaction().Set(fdb.Key(ks.Bytes()), make([]byte, 1000))

				sizeAfter, err := rtx.GetApproximateTransactionSize()
				Expect(err).NotTo(HaveOccurred())
				Expect(sizeAfter).To(BeNumerically(">", sizeBefore))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("ClaimLocalVersion", func() {
		It("returns monotonically increasing values starting from 0", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				Expect(rtx.ClaimLocalVersion()).To(Equal(0))
				Expect(rtx.ClaimLocalVersion()).To(Equal(1))
				Expect(rtx.ClaimLocalVersion()).To(Equal(2))
				Expect(rtx.ClaimLocalVersion()).To(Equal(3))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("LocalVersionCache", func() {
		It("round-trips add then get", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				key := []byte("record/1")
				rtx.AddToLocalVersionCache(key, 42)

				v, ok := rtx.GetLocalVersion(key)
				Expect(ok).To(BeTrue())
				Expect(v).To(Equal(42))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns false for non-existent key", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, ok := rtx.GetLocalVersion([]byte("does-not-exist"))
				Expect(ok).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("remove then get returns false", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				key := []byte("record/2")
				rtx.AddToLocalVersionCache(key, 7)

				v, ok := rtx.GetLocalVersion(key)
				Expect(ok).To(BeTrue())
				Expect(v).To(Equal(7))

				rtx.RemoveLocalVersion(key)
				_, ok = rtx.GetLocalVersion(key)
				Expect(ok).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("VersionMutations", func() {
		It("initially has no version mutations", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				Expect(rtx.HasVersionMutations()).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("HasVersionMutations becomes true after add, false after remove", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				key := []byte("vkey1")
				val := []byte("vval1")
				rtx.AddVersionMutation(MutationTypeSetVersionstampedValue, key, val)
				Expect(rtx.HasVersionMutations()).To(BeTrue())

				rtx.RemoveVersionMutation(key)
				Expect(rtx.HasVersionMutations()).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("adding multiple mutations and removing one leaves the rest", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.AddVersionMutation(MutationTypeSetVersionstampedKey, []byte("a"), []byte("va"))
				rtx.AddVersionMutation(MutationTypeSetVersionstampedValue, []byte("b"), []byte("vb"))
				Expect(rtx.HasVersionMutations()).To(BeTrue())

				rtx.RemoveVersionMutation([]byte("a"))
				Expect(rtx.HasVersionMutations()).To(BeTrue())

				rtx.RemoveVersionMutation([]byte("b"))
				Expect(rtx.HasVersionMutations()).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("UpdateVersionMutation", func() {
		It("inserts when no existing mutation", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()

			rctx := NewFDBRecordContext(tx)
			key := []byte("ukey")
			val := []byte("initial")
			rctx.UpdateVersionMutation(MutationTypeSetVersionstampedValue, key, val, func(old, new []byte) []byte {
				// Should not be called for first insert
				Fail("merge should not be called on first insert")
				return nil
			})
			Expect(rctx.HasVersionMutations()).To(BeTrue())
		})

		It("calls merge function when updating existing mutation", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()

			rctx := NewFDBRecordContext(tx)
			key := []byte("ukey2")
			rctx.AddVersionMutation(MutationTypeSetVersionstampedValue, key, []byte("short"))

			var mergeCalled bool
			rctx.UpdateVersionMutation(MutationTypeSetVersionstampedValue, key, []byte("longer-value"), func(old, new []byte) []byte {
				mergeCalled = true
				// Keep the longer value
				if len(old) > len(new) {
					return old
				}
				return new
			})
			Expect(mergeCalled).To(BeTrue())
		})

		It("uses new value when merge is nil", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()

			rctx := NewFDBRecordContext(tx)
			key := []byte("ukey3")
			rctx.AddVersionMutation(MutationTypeSetVersionstampedValue, key, []byte("old"))
			// nil merge should just overwrite
			rctx.UpdateVersionMutation(MutationTypeSetVersionstampedValue, key, []byte("new"), nil)
			Expect(rctx.HasVersionMutations()).To(BeTrue())
		})
	})

	Describe("RemoveVersionMutationsInRange", func() {
		It("removes only mutations within the specified range", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.AddVersionMutation(MutationTypeSetVersionstampedValue, []byte("aaa"), []byte("v1"))
				rtx.AddVersionMutation(MutationTypeSetVersionstampedValue, []byte("bbb"), []byte("v2"))
				rtx.AddVersionMutation(MutationTypeSetVersionstampedValue, []byte("ccc"), []byte("v3"))
				rtx.AddVersionMutation(MutationTypeSetVersionstampedValue, []byte("ddd"), []byte("v4"))
				Expect(rtx.HasVersionMutations()).To(BeTrue())

				// Remove [bbb, ddd) — should remove bbb and ccc but not aaa or ddd
				rtx.RemoveVersionMutationsInRange(fdb.Key("bbb"), fdb.Key("ddd"))

				// aaa and ddd should remain
				Expect(rtx.HasVersionMutations()).To(BeTrue())

				// Remove the remaining two
				rtx.RemoveVersionMutation([]byte("aaa"))
				rtx.RemoveVersionMutation([]byte("ddd"))
				Expect(rtx.HasVersionMutations()).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("RemoveLocalVersionsInRange", func() {
		It("removes only local versions within the specified range", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.AddToLocalVersionCache([]byte("aaa"), 1)
				rtx.AddToLocalVersionCache([]byte("bbb"), 2)
				rtx.AddToLocalVersionCache([]byte("ccc"), 3)
				rtx.AddToLocalVersionCache([]byte("ddd"), 4)

				// Remove [bbb, ddd) — should remove bbb and ccc
				rtx.RemoveLocalVersionsInRange(fdb.Key("bbb"), fdb.Key("ddd"))

				_, ok := rtx.GetLocalVersion([]byte("aaa"))
				Expect(ok).To(BeTrue())
				_, ok = rtx.GetLocalVersion([]byte("bbb"))
				Expect(ok).To(BeFalse())
				_, ok = rtx.GetLocalVersion([]byte("ccc"))
				Expect(ok).To(BeFalse())
				_, ok = rtx.GetLocalVersion([]byte("ddd"))
				Expect(ok).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("SetTransactionPriority", func() {
		It("PriorityDefault returns nil error", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				err := rtx.SetTransactionPriority(PriorityDefault)
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("PriorityBatch returns nil error", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				err := rtx.SetTransactionPriority(PriorityBatch)
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("PrioritySystemImmediate returns nil error", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				err := rtx.SetTransactionPriority(PrioritySystemImmediate)
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("GetConflictingKeys / AddReadConflictRange", func() {
		It("initially returns empty", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				Expect(rtx.GetConflictingKeys()).To(BeEmpty())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("tracks added read conflict ranges", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				r := fdb.KeyRange{Begin: fdb.Key("aaa"), End: fdb.Key("zzz")}
				err := rtx.AddReadConflictRange(r)
				Expect(err).NotTo(HaveOccurred())

				ranges := rtx.GetConflictingKeys()
				Expect(ranges).To(HaveLen(1))
				Expect(ranges[0].Begin.(fdb.Key)).To(Equal(fdb.Key("aaa")))
				Expect(ranges[0].End.(fdb.Key)).To(Equal(fdb.Key("zzz")))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("accumulates multiple ranges", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				r1 := fdb.KeyRange{Begin: fdb.Key("a"), End: fdb.Key("b")}
				r2 := fdb.KeyRange{Begin: fdb.Key("c"), End: fdb.Key("d")}
				Expect(rtx.AddReadConflictRange(r1)).To(Succeed())
				Expect(rtx.AddReadConflictRange(r2)).To(Succeed())

				Expect(rtx.GetConflictingKeys()).To(HaveLen(2))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("HasDirtyStoreState / SetDirtyStoreState", func() {
		It("initially false", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				Expect(rtx.HasDirtyStoreState()).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("set to true then back to false", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.SetDirtyStoreState(true)
				Expect(rtx.HasDirtyStoreState()).To(BeTrue())

				rtx.SetDirtyStoreState(false)
				Expect(rtx.HasDirtyStoreState()).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("SetMetaDataVersionStamp / GetMetaDataVersionStamp", func() {
		It("returns nil after SetMetaDataVersionStamp (dirty)", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.SetMetaDataVersionStamp()
				stamp, err := rtx.GetMetaDataVersionStamp()
				Expect(err).NotTo(HaveOccurred())
				Expect(stamp).To(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("GetMetaDataVersionStamp without set does not error", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				// On a fresh transaction the metadata version key may or may not exist.
				// Either way, the call should not error.
				_, err := rtx.GetMetaDataVersionStamp()
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("GetStoreStateCache", func() {
		It("new database has PassThroughStoreStateCache by default", func() {
			cache := sharedDB.GetStoreStateCache()
			Expect(cache).NotTo(BeNil())
			// PassThroughStoreStateCache is the default
			_, ok := cache.(*PassThroughRecordStoreStateCache)
			Expect(ok).To(BeTrue())
		})

		It("set and get a different cache", func() {
			originalCache := sharedDB.GetStoreStateCache()
			defer sharedDB.SetStoreStateCache(originalCache)

			newCache := NewMetaDataVersionStampStoreStateCache()
			sharedDB.SetStoreStateCache(newCache)
			Expect(sharedDB.GetStoreStateCache()).To(BeIdenticalTo(newCache))
		})
	})

	Describe("Timer / SetTimer", func() {
		It("initially nil", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				Expect(rtx.Timer()).To(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("set and retrieve timer", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				timer := NewStoreTimer()
				rtx.SetTimer(timer)
				Expect(rtx.Timer()).To(BeIdenticalTo(timer))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("nil timer methods are safe no-ops", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				// Timer() is nil by default. RecordSince and other methods should be no-ops.
				Expect(rtx.Timer()).To(BeNil())
				rtx.Timer().Record(EventSaveRecord, 100)
				rtx.Timer().Increment(CountReads)
				Expect(rtx.Timer().GetCount(EventSaveRecord)).To(Equal(int64(0)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("NewFDBRecordContext", func() {
		It("creates a valid context with Transaction and Context accessors", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()

			rctx := NewFDBRecordContext(tx)
			Expect(rctx).NotTo(BeNil())
			Expect(rctx.Transaction()).To(Equal(tx))
			Expect(rctx.Context()).NotTo(BeNil())
		})

		It("has zero-valued local version counter", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()

			rctx := NewFDBRecordContext(tx)
			Expect(rctx.ClaimLocalVersion()).To(Equal(0))
			Expect(rctx.ClaimLocalVersion()).To(Equal(1))
		})

		It("starts with no version mutations or dirty state", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()

			rctx := NewFDBRecordContext(tx)
			Expect(rctx.HasVersionMutations()).To(BeFalse())
			Expect(rctx.HasDirtyStoreState()).To(BeFalse())
			Expect(rctx.Timer()).To(BeNil())
			Expect(rctx.GetConflictingKeys()).To(BeEmpty())
		})
	})

	Describe("CheckTransactionSize", func() {
		It("returns nil when thresholds are disabled", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()

			rctx := NewFDBRecordContext(tx)
			Expect(rctx.CheckTransactionSize()).NotTo(HaveOccurred())
		})

		It("returns nil when size is below warn threshold", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()

			rctx := NewFDBRecordContext(tx)
			rctx.txSizeWarnBytes = 1_000_000
			Expect(rctx.CheckTransactionSize()).NotTo(HaveOccurred())
		})

		It("returns TransactionSizeWarningError once when warn threshold exceeded", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()

			rctx := NewFDBRecordContext(tx)
			rctx.txSizeWarnBytes = 1 // 1 byte = always warn after any write

			ks := specSubspace()
			tx.Set(fdb.Key(ks.Bytes()), make([]byte, 100))

			err = rctx.CheckTransactionSize()
			Expect(err).To(HaveOccurred())
			var warnErr *TransactionSizeWarningError
			Expect(errors.As(err, &warnErr)).To(BeTrue())
			Expect(warnErr.CurrentBytes).To(BeNumerically(">", 0))

			// Second call should NOT warn again (once per tx)
			err = rctx.CheckTransactionSize()
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns TransactionSizeExceededError when error threshold exceeded", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()

			rctx := NewFDBRecordContext(tx)
			rctx.txSizeErrorBytes = 1 // 1 byte = always error after any write

			ks := specSubspace()
			tx.Set(fdb.Key(ks.Bytes()), make([]byte, 100))

			err = rctx.CheckTransactionSize()
			Expect(err).To(HaveOccurred())
			var exceedErr *TransactionSizeExceededError
			Expect(errors.As(err, &exceedErr)).To(BeTrue())
			Expect(exceedErr.CurrentBytes).To(BeNumerically(">", 0))

			// Error repeats on subsequent calls (not once-only like warn)
			err = rctx.CheckTransactionSize()
			Expect(err).To(HaveOccurred())
			Expect(errors.As(err, &exceedErr)).To(BeTrue())
		})

		It("error threshold takes precedence over warn threshold", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()

			rctx := NewFDBRecordContext(tx)
			rctx.txSizeWarnBytes = 1
			rctx.txSizeErrorBytes = 1

			ks := specSubspace()
			tx.Set(fdb.Key(ks.Bytes()), make([]byte, 100))

			err = rctx.CheckTransactionSize()
			Expect(err).To(HaveOccurred())
			var exceedErr *TransactionSizeExceededError
			Expect(errors.As(err, &exceedErr)).To(BeTrue())
		})
	})
})
