package recordlayer

import (
	"context"
	"errors"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("FDBDatabase", func() {
	ctx := context.Background()

	Describe("Run", func() {
		It("passes the result through", func() {
			result, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				return "hello", nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal("hello"))
		})

		It("propagates user function errors", func() {
			sentinel := errors.New("boom")
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				return nil, sentinel
			})
			Expect(err).To(MatchError(sentinel))
		})

		It("flushes version mutations before commit", func() {
			ss := specSubspace()

			// Write a versionstamped value via AddVersionMutation inside Run.
			// After commit, the key should exist with the versionstamp filled in.
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				key := ss.Pack(tuple.Tuple{"vskey"})
				// Build a 14-byte value: 10 zero bytes (placeholder) + 4-byte LE offset (0).
				// FDB replaces the first 10 bytes with the commit versionstamp.
				value := make([]byte, 14)
				rtx.AddVersionMutation(MutationTypeSetVersionstampedValue, key, value)
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify the key was written (version mutations were flushed).
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				key := ss.Pack(tuple.Tuple{"vskey"})
				val := rtx.Transaction().Get(fdb.Key(key)).MustGet()
				Expect(val).NotTo(BeNil(), "versionstamped key should exist after flush")
				Expect(len(val)).To(Equal(10), "value should be 10 bytes (versionstamp, offset stripped)")
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("provides a fresh context on each call", func() {
			// Two consecutive Runs should get independent contexts (local version starts at 0).
			for i := 0; i < 3; i++ {
				_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					Expect(rtx.ClaimLocalVersion()).To(Equal(0))
					Expect(rtx.HasVersionMutations()).To(BeFalse())
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			}
		})
	})

	Describe("RunWithVersionstamp", func() {
		It("returns nil versionstamp for read-only transaction", func() {
			result, vs, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				// No writes, no version mutations.
				return "read-only", nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal("read-only"))
			Expect(vs).To(BeNil(), "read-only tx should have nil versionstamp")
		})

		It("returns non-nil versionstamp when version mutations are queued", func() {
			ss := specSubspace()

			result, vs, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				key := ss.Pack(tuple.Tuple{"vs-run"})
				value := make([]byte, 14) // placeholder for SET_VERSIONSTAMPED_VALUE
				rtx.AddVersionMutation(MutationTypeSetVersionstampedValue, key, value)
				return 42, nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(42))
			Expect(vs).NotTo(BeNil(), "tx with version mutations should return versionstamp")
			Expect(len(vs)).To(Equal(10), "versionstamp should be 10 bytes")
		})

		It("propagates user function errors without versionstamp", func() {
			sentinel := errors.New("fail")
			_, vs, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				return nil, sentinel
			})
			Expect(err).To(MatchError(sentinel))
			Expect(vs).To(BeNil())
		})

		It("runs post-commit hooks after success", func() {
			ss := specSubspace()
			postRan := false

			_, _, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				key := ss.Pack(tuple.Tuple{"vs-post"})
				value := make([]byte, 14)
				rtx.AddVersionMutation(MutationTypeSetVersionstampedValue, key, value)
				rtx.AddPostCommit(func() {
					postRan = true
				})
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(postRan).To(BeTrue())
		})

		It("does not run post-commit hooks on pre-commit check failure", func() {
			postRan := false
			_, vs, err := sharedDB.RunWithVersionstamp(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.AddCommitCheck(func() error {
					return errors.New("check failed")
				})
				rtx.AddPostCommit(func() {
					postRan = true
				})
				return nil, nil
			})
			Expect(err).To(HaveOccurred())
			Expect(vs).To(BeNil())
			Expect(postRan).To(BeFalse())
		})
	})

	Describe("CommitWithVersionstamp", func() {
		It("returns nil for a read-only transaction", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()

			rtx := NewFDBRecordContext(tx)
			// Write something so the tx is not completely empty (avoids potential FDB quirks).
			tx.Set(fdb.Key(specSubspace().Pack(tuple.Tuple{"cwvs-ro"})), []byte("x"))

			vs, err := rtx.CommitWithVersionstamp()
			Expect(err).NotTo(HaveOccurred())
			Expect(vs).To(BeNil(), "no version mutations => nil versionstamp")
		})

		It("returns 10-byte versionstamp when version mutations exist", func() {
			ss := specSubspace()

			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())

			rtx := NewFDBRecordContext(tx)
			key := ss.Pack(tuple.Tuple{"cwvs-mut"})
			value := make([]byte, 14) // placeholder
			rtx.AddVersionMutation(MutationTypeSetVersionstampedValue, key, value)

			vs, err := rtx.CommitWithVersionstamp()
			Expect(err).NotTo(HaveOccurred())
			Expect(vs).NotTo(BeNil())
			Expect(len(vs)).To(Equal(10))
		})

		It("pre-commit check failure prevents commit", func() {
			ss := specSubspace()

			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())

			rtx := NewFDBRecordContext(tx)
			tx.Set(fdb.Key(ss.Pack(tuple.Tuple{"cwvs-fail"})), []byte("should-not-persist"))
			rtx.AddCommitCheck(func() error {
				return errors.New("abort")
			})

			vs, err := rtx.CommitWithVersionstamp()
			Expect(err).To(HaveOccurred())
			Expect(vs).To(BeNil())
			tx.Cancel()

			// Verify the write did not persist.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				val := rtx.Transaction().Get(fdb.Key(ss.Pack(tuple.Tuple{"cwvs-fail"}))).MustGet()
				Expect(val).To(BeNil(), "aborted tx should not persist data")
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("runs post-commit hooks after successful commit", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())

			rtx := NewFDBRecordContext(tx)
			tx.Set(fdb.Key(specSubspace().Pack(tuple.Tuple{"cwvs-post"})), []byte("ok"))

			postRan := false
			rtx.AddPostCommit(func() {
				postRan = true
			})

			_, err = rtx.CommitWithVersionstamp()
			Expect(err).NotTo(HaveOccurred())
			Expect(postRan).To(BeTrue())
		})

		It("does not run post-commit hooks when pre-commit check fails", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()

			rtx := NewFDBRecordContext(tx)
			postRan := false
			rtx.AddCommitCheck(func() error { return errors.New("nope") })
			rtx.AddPostCommit(func() { postRan = true })

			_, err = rtx.CommitWithVersionstamp()
			Expect(err).To(HaveOccurred())
			Expect(postRan).To(BeFalse())
		})
	})

	Describe("CreateTransaction", func() {
		It("creates a usable transaction", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()

			ss := specSubspace()
			tx.Set(fdb.Key(ss.Pack(tuple.Tuple{"create-tx"})), []byte("value"))
			err = tx.Commit().Get()
			Expect(err).NotTo(HaveOccurred())

			// Verify data persisted.
			tx2, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx2.Cancel()

			val := tx2.Get(fdb.Key(ss.Pack(tuple.Tuple{"create-tx"}))).MustGet()
			Expect(val).To(Equal([]byte("value")))
		})
	})

	Describe("RunWithWeakReads", func() {
		It("executes successfully with causal read risky", func() {
			ss := specSubspace()

			// Write a key.
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.Transaction().Set(fdb.Key(ss.Pack(tuple.Tuple{"weak-read"})), []byte("value"))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Read with weak read semantics.
			result, err := sharedDB.RunWithWeakReads(ctx, WeakReadSemantics{
				IsCausalReadRisky: true,
			}, func(rtx *FDBRecordContext) (any, error) {
				return rtx.Transaction().Get(fdb.Key(ss.Pack(tuple.Tuple{"weak-read"}))).MustGet(), nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([]byte("value")))
		})

		It("executes successfully without causal read risky", func() {
			result, err := sharedDB.RunWithWeakReads(ctx, WeakReadSemantics{
				MinVersion:           0,
				StalenessBoundMillis: 5000,
			}, func(rtx *FDBRecordContext) (any, error) {
				return "ok", nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal("ok"))
		})
	})

	Describe("FDBDatabaseFactory", func() {
		It("returns the same instance for the same cluster file", func() {
			factory := NewFDBDatabaseFactory()
			db1, err := factory.GetDatabase(clusterTmpFilePath)
			Expect(err).NotTo(HaveOccurred())
			Expect(db1).NotTo(BeNil())

			db2, err := factory.GetDatabase(clusterTmpFilePath)
			Expect(err).NotTo(HaveOccurred())
			// Same pointer — cached instance.
			Expect(db2).To(BeIdenticalTo(db1))
		})

		It("supports custom store state cache factory", func() {
			factory := NewFDBDatabaseFactory()
			factory.StoreStateCacheFactory = func() FDBRecordStoreStateCache {
				return PassThroughStoreStateCache()
			}
			db, err := factory.GetDatabase(clusterTmpFilePath)
			Expect(err).NotTo(HaveOccurred())
			Expect(db).NotTo(BeNil())
		})
	})

	Describe("NewFDBDatabaseWithTransactor", func() {
		It("uses the custom transactor for Run", func() {
			// Create a wrapping transactor that records whether Transact was called.
			transactCalled := false
			wrapper := &spyTransactor{
				inner:    sharedDB.transactor,
				onCalled: func() { transactCalled = true },
			}

			db := NewFDBDatabaseWithTransactor(wrapper, sharedDB.db)
			result, err := db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				return "via-wrapper", nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal("via-wrapper"))
			Expect(transactCalled).To(BeTrue(), "custom transactor should be used by Run")
		})

		It("uses the underlying db for CreateTransaction", func() {
			wrapper := &spyTransactor{inner: sharedDB.transactor, onCalled: func() {}}
			db := NewFDBDatabaseWithTransactor(wrapper, sharedDB.db)

			tx, err := db.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()
			// If CreateTransaction used the wrapper it would panic or fail; it uses db directly.
		})

		It("defaults to PassThroughStoreStateCache", func() {
			wrapper := &spyTransactor{inner: sharedDB.transactor, onCalled: func() {}}
			db := NewFDBDatabaseWithTransactor(wrapper, sharedDB.db)
			_, ok := db.GetStoreStateCache().(*PassThroughRecordStoreStateCache)
			Expect(ok).To(BeTrue())
		})
	})

	Describe("RemoveVersionMutationsInRange edge cases", func() {
		It("no-ops on empty mutations map", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()

			rtx := NewFDBRecordContext(tx)
			// Should not panic on nil map.
			rtx.RemoveVersionMutationsInRange(fdb.Key("a"), fdb.Key("z"))
			Expect(rtx.HasVersionMutations()).To(BeFalse())
		})

		It("includes begin boundary, excludes end boundary", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()

			rtx := NewFDBRecordContext(tx)
			rtx.AddVersionMutation(MutationTypeSetVersionstampedValue, []byte("bbb"), []byte("v1"))
			rtx.AddVersionMutation(MutationTypeSetVersionstampedValue, []byte("ccc"), []byte("v2"))

			// Range [bbb, ccc) should remove "bbb" but not "ccc".
			rtx.RemoveVersionMutationsInRange(fdb.Key("bbb"), fdb.Key("ccc"))
			Expect(rtx.HasVersionMutations()).To(BeTrue(), "ccc should remain")

			rtx.RemoveVersionMutation([]byte("ccc"))
			Expect(rtx.HasVersionMutations()).To(BeFalse())
		})

		It("empty range [x, x) removes nothing", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()

			rtx := NewFDBRecordContext(tx)
			rtx.AddVersionMutation(MutationTypeSetVersionstampedValue, []byte("aaa"), []byte("v"))

			rtx.RemoveVersionMutationsInRange(fdb.Key("aaa"), fdb.Key("aaa"))
			Expect(rtx.HasVersionMutations()).To(BeTrue(), "empty range should not remove anything")
		})
	})

	Describe("RemoveLocalVersionsInRange edge cases", func() {
		It("no-ops on empty cache", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()

			rtx := NewFDBRecordContext(tx)
			// Should not panic on nil map.
			rtx.RemoveLocalVersionsInRange(fdb.Key("a"), fdb.Key("z"))
			_, ok := rtx.GetLocalVersion([]byte("a"))
			Expect(ok).To(BeFalse())
		})

		It("includes begin boundary, excludes end boundary", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()

			rtx := NewFDBRecordContext(tx)
			rtx.AddToLocalVersionCache([]byte("bbb"), 1)
			rtx.AddToLocalVersionCache([]byte("ccc"), 2)

			rtx.RemoveLocalVersionsInRange(fdb.Key("bbb"), fdb.Key("ccc"))
			_, ok := rtx.GetLocalVersion([]byte("bbb"))
			Expect(ok).To(BeFalse(), "begin boundary should be removed")
			v, ok := rtx.GetLocalVersion([]byte("ccc"))
			Expect(ok).To(BeTrue(), "end boundary should remain")
			Expect(v).To(Equal(2))
		})

		It("empty range [x, x) removes nothing", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()

			rtx := NewFDBRecordContext(tx)
			rtx.AddToLocalVersionCache([]byte("aaa"), 5)

			rtx.RemoveLocalVersionsInRange(fdb.Key("aaa"), fdb.Key("aaa"))
			v, ok := rtx.GetLocalVersion([]byte("aaa"))
			Expect(ok).To(BeTrue())
			Expect(v).To(Equal(5))
		})
	})

	Describe("AddVersionMutation overwrite semantics", func() {
		It("overwrites an existing mutation for the same key", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()

			rtx := NewFDBRecordContext(tx)
			key := []byte("same-key")

			rtx.AddVersionMutation(MutationTypeSetVersionstampedValue, key, []byte("first"))
			rtx.AddVersionMutation(MutationTypeSetVersionstampedKey, key, []byte("second"))

			// Should still be one mutation (overwritten), not two.
			Expect(rtx.HasVersionMutations()).To(BeTrue())
			rtx.RemoveVersionMutation(key)
			Expect(rtx.HasVersionMutations()).To(BeFalse(), "only one key should exist after overwrite")
		})
	})

	Describe("RemoveVersionMutation for non-existent key", func() {
		It("is a no-op and does not panic", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()

			rtx := NewFDBRecordContext(tx)
			// Remove from nil map.
			rtx.RemoveVersionMutation([]byte("ghost"))
			Expect(rtx.HasVersionMutations()).To(BeFalse())

			// Remove from populated map but non-existent key.
			rtx.AddVersionMutation(MutationTypeSetVersionstampedValue, []byte("real"), []byte("v"))
			rtx.RemoveVersionMutation([]byte("ghost"))
			Expect(rtx.HasVersionMutations()).To(BeTrue())
		})
	})

	Describe("RemoveLocalVersion for non-existent key", func() {
		It("is a no-op and does not panic", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()

			rtx := NewFDBRecordContext(tx)
			// Remove from nil map.
			rtx.RemoveLocalVersion([]byte("ghost"))

			// Remove from populated map but non-existent key.
			rtx.AddToLocalVersionCache([]byte("real"), 1)
			rtx.RemoveLocalVersion([]byte("ghost"))
			v, ok := rtx.GetLocalVersion([]byte("real"))
			Expect(ok).To(BeTrue())
			Expect(v).To(Equal(1))
		})
	})

	Describe("LocalVersionCache overwrite", func() {
		It("overwrites cached version for the same key", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()

			rtx := NewFDBRecordContext(tx)
			key := []byte("lv-key")
			rtx.AddToLocalVersionCache(key, 10)
			rtx.AddToLocalVersionCache(key, 20)

			v, ok := rtx.GetLocalVersion(key)
			Expect(ok).To(BeTrue())
			Expect(v).To(Equal(20))
		})
	})

	Describe("GetMetaDataVersionStamp", func() {
		It("returns nil when dirtyMetaDataVersionStamp is true before any Get", func() {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx.Cancel()

			rtx := NewFDBRecordContext(tx)
			rtx.SetMetaDataVersionStamp() // sets dirtyMetaDataVersionStamp = true

			stamp, err := rtx.GetMetaDataVersionStamp()
			Expect(err).NotTo(HaveOccurred())
			Expect(stamp).To(BeNil())
		})

		It("returns stamp after it was previously written and committed", func() {
			// First: write the metadata version stamp.
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.SetMetaDataVersionStamp()
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Second: read it back in a new transaction.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				stamp, err := rtx.GetMetaDataVersionStamp()
				Expect(err).NotTo(HaveOccurred())
				// The stamp should be non-nil and 10 bytes (versionstamp).
				Expect(stamp).NotTo(BeNil())
				Expect(len(stamp)).To(Equal(10))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

// spyTransactor wraps an fdb.Transactor and calls onCalled before delegating.
type spyTransactor struct {
	inner    fdb.Transactor
	onCalled func()
}

func (s *spyTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	s.onCalled()
	return s.inner.Transact(fn)
}

func (s *spyTransactor) ReadTransact(fn func(fdb.ReadTransaction) (any, error)) (any, error) {
	return s.inner.ReadTransact(fn)
}
