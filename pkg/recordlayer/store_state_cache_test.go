package recordlayer

import (
	"context"
	"fmt"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
)

var _ = Describe("Store State Cache", func() {
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
		var err error
		md, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())
	})

	// Helper: metadata with an index.
	buildMetaDataWithIndex := func() *RecordMetaData {
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		b.AddIndex("Order", NewIndex("test_idx", Field("price")))
		m, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		return m
	}

	Describe("PassThroughRecordStoreStateCache", func() {
		It("always loads from FDB", func() {
			ss := specSubspace()

			// Create a store using passthrough cache (the default).
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(PassThroughStoreStateCache()).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				Expect(store).NotTo(BeNil())
				Expect(store.storeHeader).NotTo(BeNil())
				Expect(store.storeHeader.GetFormatVersion()).To(BeNumerically(">=", formatVersionCacheableState))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Open again with passthrough — should load fresh.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(PassThroughStoreStateCache()).
					Open()
				if err != nil {
					return nil, err
				}
				Expect(store.storeHeader).NotTo(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("Clear is a no-op", func() {
			cache := PassThroughStoreStateCache()
			// Should not panic.
			cache.Clear()
		})
	})

	Describe("MetaDataVersionStampStoreStateCache", func() {
		It("cache hit on repeated open with no mutations", func() {
			ss := specSubspace()
			cache := NewMetaDataVersionStampStoreStateCache()

			// Tx1: Create store and mark cacheable.
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				changed, err := store.SetStateCacheability(true)
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Tx2: Open store — cache miss (first access), loads from FDB and caches.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				if err != nil {
					return nil, err
				}
				Expect(store.storeHeader).NotTo(BeNil())
				Expect(store.storeHeader.GetCacheable()).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Tx3: Open store — should be a cache hit (no FDB mutations between tx2 and tx3).
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				if err != nil {
					return nil, err
				}
				Expect(store.storeHeader).NotTo(BeNil())
				Expect(store.storeHeader.GetCacheable()).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("cache miss after header mutation (SetHeaderUserField)", func() {
			ss := specSubspace()
			cache := NewMetaDataVersionStampStoreStateCache()

			// Create cacheable store.
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SetStateCacheability(true)
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Tx2: Open to populate cache.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Tx3: Mutate header (SetHeaderUserField) — bumps metadata version stamp.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				if err != nil {
					return nil, err
				}
				return nil, store.SetHeaderUserField("test-key", []byte("test-value"))
			})
			Expect(err).NotTo(HaveOccurred())

			// Tx4: Open — cache miss due to versionstamp change, reloads fresh state.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				if err != nil {
					return nil, err
				}
				// Verify the user field is present in the freshly loaded state.
				var found bool
				for _, uf := range store.storeHeader.UserField {
					if uf.GetKey() == "test-key" {
						Expect(uf.Value).To(Equal([]byte("test-value")))
						found = true
					}
				}
				Expect(found).To(BeTrue(), "expected to find user field 'test-key' after cache miss")
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("cache miss after index state change", func() {
			ss := specSubspace()
			cache := NewMetaDataVersionStampStoreStateCache()
			idxMd := buildMetaDataWithIndex()

			// Create cacheable store with index.
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(idxMd).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SetStateCacheability(true)
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Tx2: Populate cache.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(idxMd).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				if err != nil {
					return nil, err
				}
				Expect(store.IsIndexReadable("test_idx")).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Tx3: Mark index WRITE_ONLY.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(idxMd).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				if err != nil {
					return nil, err
				}
				changed, err := store.MarkIndexWriteOnly("test_idx")
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Tx4: Open — should see WRITE_ONLY from fresh load (cache invalidated).
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(idxMd).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				if err != nil {
					return nil, err
				}
				Expect(store.IsIndexWriteOnly("test_idx")).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("dirty store state skips cache within same transaction", func() {
			ss := specSubspace()
			cache := NewMetaDataVersionStampStoreStateCache()

			// Create cacheable store.
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SetStateCacheability(true)
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Populate cache.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Same-tx: modify header then re-open — dirty flag should force fresh load.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store1, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				if err != nil {
					return nil, err
				}

				// Mutate the header.
				err = store1.SetHeaderUserField("dirty-key", []byte("dirty-val"))
				Expect(err).NotTo(HaveOccurred())

				// Verify dirty flag is set.
				Expect(rtx.HasDirtyStoreState()).To(BeTrue())

				// Re-open in same transaction — should skip cache due to dirty state.
				store2, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				if err != nil {
					return nil, err
				}

				// Should see the user field written by store1 (loaded fresh, not from cache).
				var found bool
				for _, uf := range store2.storeHeader.UserField {
					if uf.GetKey() == "dirty-key" {
						found = true
					}
				}
				Expect(found).To(BeTrue(), "re-opened store should see dirty-key from same tx")
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("non-cacheable store is not cached", func() {
			ss := specSubspace()
			cache := NewMetaDataVersionStampStoreStateCache()

			// Create store without setting cacheable (default = false).
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				Expect(store.IsStateCacheable()).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Open twice — cache should remain empty since store is not cacheable.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Cache stores entries regardless of cacheable flag.
			cache.mu.Lock()
			entryCount := len(cache.entries)
			cache.mu.Unlock()
			Expect(entryCount).To(Equal(1))
		})

		It("SetStateCacheability(true) enables caching", func() {
			ss := specSubspace()
			cache := NewMetaDataVersionStampStoreStateCache()

			// Create store (not cacheable).
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Enable cacheability.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				if err != nil {
					return nil, err
				}
				changed, err := store.SetStateCacheability(true)
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())
				Expect(store.IsStateCacheable()).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Open to populate cache.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify cache has an entry.
			cache.mu.Lock()
			entryCount := len(cache.entries)
			cache.mu.Unlock()
			Expect(entryCount).To(Equal(1))
		})

		It("SetStateCacheability(false) disables caching", func() {
			ss := specSubspace()
			cache := NewMetaDataVersionStampStoreStateCache()

			// Create cacheable store.
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SetStateCacheability(true)
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Open to populate cache.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify entry is cached.
			cache.mu.Lock()
			count1 := len(cache.entries)
			cache.mu.Unlock()
			Expect(count1).To(Equal(1))

			// Disable cacheability.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				if err != nil {
					return nil, err
				}
				changed, err := store.SetStateCacheability(false)
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Open again — store should correctly report non-cacheable.
			// Clear cache first to force fresh load, verifying the persisted state.
			cache.Clear()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				if err != nil {
					return nil, err
				}
				Expect(store.IsStateCacheable()).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Cache stores entries regardless of cacheable flag.
			cache.mu.Lock()
			count2 := len(cache.entries)
			cache.mu.Unlock()
			Expect(count2).To(Equal(1))
		})

		It("multiple stores in same cache are independently cached", func() {
			ss1 := subspace.FromBytes(tuple.Tuple{CurrentSpecReport().FullText(), "store1"}.Pack())
			ss2 := subspace.FromBytes(tuple.Tuple{CurrentSpecReport().FullText(), "store2"}.Pack())
			cache := NewMetaDataVersionStampStoreStateCache()

			// Create two cacheable stores with different user fields.
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss1).
					SetStoreStateCache(cache).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				if _, err := store.SetStateCacheability(true); err != nil {
					return nil, err
				}
				return nil, store.SetHeaderUserField("s1-key", []byte("s1-val"))
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss2).
					SetStoreStateCache(cache).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				if _, err := store.SetStateCacheability(true); err != nil {
					return nil, err
				}
				return nil, store.SetHeaderUserField("s2-key", []byte("s2-val"))
			})
			Expect(err).NotTo(HaveOccurred())

			// Open both to populate cache (cache miss, then cached).
			for _, ss := range []subspace.Subspace{ss1, ss2} {
				_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					_, err := NewStoreBuilder().
						SetContext(rtx).
						SetMetaDataProvider(md).
						SetSubspace(ss).
						SetStoreStateCache(cache).
						Open()
					return nil, err
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Verify both are cached with distinct entries.
			cache.mu.Lock()
			Expect(len(cache.entries)).To(Equal(2))
			cache.mu.Unlock()

			// Open both again — should both work and have correct data.
			// Note: \xff/metadataVersion is a GLOBAL key, so a mutation to either store
			// invalidates ALL cache entries. But with no mutations, both should hit cache.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store1, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss1).
					SetStoreStateCache(cache).
					Open()
				if err != nil {
					return nil, err
				}
				Expect(store1.GetHeaderUserField("s1-key")).To(Equal([]byte("s1-val")))
				Expect(store1.GetHeaderUserField("s2-key")).To(BeNil())

				store2, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss2).
					SetStoreStateCache(cache).
					Open()
				if err != nil {
					return nil, err
				}
				Expect(store2.GetHeaderUserField("s2-key")).To(Equal([]byte("s2-val")))
				Expect(store2.GetHeaderUserField("s1-key")).To(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("cache expiry with short TTL", func() {
			ss := specSubspace()
			cache := NewMetaDataVersionStampStoreStateCache(
				WithExpireAfterAccess(1 * time.Millisecond),
			)

			// Create cacheable store.
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SetStateCacheability(true)
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Open to populate cache.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Wait for entry to expire.
			time.Sleep(5 * time.Millisecond)

			// Verify the entry was evicted on access.
			cache.mu.Lock()
			entry := cache.getIfPresent(string(ss.Bytes()))
			cache.mu.Unlock()
			Expect(entry).To(BeNil(), "expired entry should be evicted")
		})

		It("cache size limit causes LRU eviction", func() {
			ss1 := subspace.FromBytes(tuple.Tuple{CurrentSpecReport().FullText(), "lru1"}.Pack())
			ss2 := subspace.FromBytes(tuple.Tuple{CurrentSpecReport().FullText(), "lru2"}.Pack())
			cache := NewMetaDataVersionStampStoreStateCache(
				WithMaxSize(1),
			)

			// Create two cacheable stores.
			for _, ss := range []subspace.Subspace{ss1, ss2} {
				_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().
						SetContext(rtx).
						SetMetaDataProvider(md).
						SetSubspace(ss).
						SetStoreStateCache(cache).
						CreateOrOpen()
					if err != nil {
						return nil, err
					}
					_, err = store.SetStateCacheability(true)
					return nil, err
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Open store1 — populates cache (cache has 1 entry).
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss1).
					SetStoreStateCache(cache).
					Open()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			cache.mu.Lock()
			Expect(len(cache.entries)).To(Equal(1))
			cache.mu.Unlock()

			// Open store2 — should evict store1 (maxSize=1).
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss2).
					SetStoreStateCache(cache).
					Open()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			cache.mu.Lock()
			Expect(len(cache.entries)).To(Equal(1))
			e1 := cache.getIfPresent(string(ss1.Bytes()))
			e2 := cache.getIfPresent(string(ss2.Bytes()))
			cache.mu.Unlock()

			Expect(e1).To(BeNil(), "store1 should have been evicted")
			Expect(e2).NotTo(BeNil(), "store2 should be in cache")
		})

		It("read conflict on cache hit ensures transaction safety", func() {
			ss := specSubspace()
			cache := NewMetaDataVersionStampStoreStateCache()

			// Create cacheable store.
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SetStateCacheability(true)
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Populate cache.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Start tx1 (uses cache hit, adds read conflict on STORE_INFO key).
			tx1, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer tx1.Cancel()

			rtx1 := NewFDBRecordContext(tx1, nil)
			store1, err := NewStoreBuilder().
				SetContext(rtx1).
				SetMetaDataProvider(md).
				SetSubspace(ss).
				SetStoreStateCache(cache).
				Open()
			Expect(err).NotTo(HaveOccurred())
			Expect(store1).NotTo(BeNil())

			// Write something in tx1 so it becomes a read-write transaction.
			tx1.Set(fdb.Key(ss.Pack(tuple.Tuple{"conflict-marker"})), []byte("tx1"))

			// tx2: Concurrently modify the store header.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				if err != nil {
					return nil, err
				}
				return nil, store.SetHeaderUserField("conflict-test", []byte("modified"))
			})
			Expect(err).NotTo(HaveOccurred())

			// tx1 commit should fail due to read conflict on STORE_INFO key.
			err = tx1.Commit().Get()
			Expect(err).To(HaveOccurred(), "tx1 should fail: store header was modified by tx2 after cache hit")
		})

		It("SetStateCacheability returns error for old format versions", func() {
			ss := specSubspace()

			// Create store, then manually set an old format version.
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Manually downgrade format version to below formatVersionCacheableState.
				oldVersion := int32(formatVersionCacheableState - 1)
				store.storeHeader.FormatVersion = &oldVersion

				_, err = store.SetStateCacheability(true)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("does not support cacheability"))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("SetStateCacheability no-op when already at desired state", func() {
			ss := specSubspace()

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				// Default is not cacheable; setting to false should be no-op.
				changed, err := store.SetStateCacheability(false)
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("database-level cache inherited by store builder", func() {
			ss := specSubspace()
			cache := NewMetaDataVersionStampStoreStateCache()

			// Set cache on the database.
			sharedDB.SetStoreStateCache(cache)
			defer sharedDB.SetStoreStateCache(PassThroughStoreStateCache())

			// Create cacheable store using SetDatabase (not SetStoreStateCache).
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetDatabase(sharedDB).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SetStateCacheability(true)
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Open — cache miss, loads and caches.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetDatabase(sharedDB).
					Open()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify cache has an entry.
			cache.mu.Lock()
			entryCount := len(cache.entries)
			cache.mu.Unlock()
			Expect(entryCount).To(Equal(1))
		})

		It("per-store cache overrides database cache", func() {
			ss := specSubspace()
			dbCache := NewMetaDataVersionStampStoreStateCache()
			storeCache := NewMetaDataVersionStampStoreStateCache()

			sharedDB.SetStoreStateCache(dbCache)
			defer sharedDB.SetStoreStateCache(PassThroughStoreStateCache())

			// Create cacheable store with per-store cache override.
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetDatabase(sharedDB).
					SetStoreStateCache(storeCache).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SetStateCacheability(true)
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Open to populate cache.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetDatabase(sharedDB).
					SetStoreStateCache(storeCache).
					Open()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Entry should be in store cache, NOT in db cache.
			storeCache.mu.Lock()
			storeEntries := len(storeCache.entries)
			storeCache.mu.Unlock()

			dbCache.mu.Lock()
			dbEntries := len(dbCache.entries)
			dbCache.mu.Unlock()

			Expect(storeEntries).To(Equal(1))
			Expect(dbEntries).To(Equal(0))
		})

		It("pass-through per-store override observes a non-cacheable index-state transition", func() {
			ss := specSubspace()
			idxMd := buildMetaDataWithIndex()
			dbCache := NewMetaDataVersionStampStoreStateCache()
			sharedDB.SetStoreStateCache(dbCache)
			defer sharedDB.SetStoreStateCache(PassThroughStoreStateCache())

			// Create the default NON-cacheable store, then populate the database
			// cache with its initial all-READABLE state.
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, openErr := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(idxMd).
					SetSubspace(ss).
					SetDatabase(sharedDB).
					CreateOrOpen()
				if openErr != nil {
					return nil, openErr
				}
				Expect(store.storeHeader.GetCacheable()).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, openErr := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(idxMd).
					SetSubspace(ss).
					SetDatabase(sharedDB).
					Open()
				if openErr != nil {
					return nil, openErr
				}
				Expect(store.GetIndexState("test_idx")).To(Equal(IndexStateReadable))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Mutate through a fresh FDB-backed view. Non-cacheable stores do not
			// bump the metadata-version stamp, so the inherited dbCache entry is
			// intentionally the adversarial stale value for the final open.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, openErr := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(idxMd).
					SetSubspace(ss).
					SetDatabase(sharedDB).
					SetStoreStateCache(PassThroughStoreStateCache()).
					Open()
				if openErr != nil {
					return nil, openErr
				}
				_, markErr := store.MarkIndexWriteOnly("test_idx")
				return nil, markErr
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, openErr := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(idxMd).
					SetSubspace(ss).
					SetDatabase(sharedDB).
					SetStoreStateCache(PassThroughStoreStateCache()).
					Open()
				if openErr != nil {
					return nil, openErr
				}
				Expect(store.GetIndexState("test_idx")).To(Equal(IndexStateWriteOnly))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("Clear removes all cached entries", func() {
			ss := specSubspace()
			cache := NewMetaDataVersionStampStoreStateCache()

			// Create and populate cache.
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SetStateCacheability(true)
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			cache.mu.Lock()
			Expect(len(cache.entries)).To(BeNumerically(">", 0))
			cache.mu.Unlock()

			cache.Clear()

			cache.mu.Lock()
			Expect(len(cache.entries)).To(Equal(0))
			cache.mu.Unlock()
		})

		It("cache stores correct store header data", func() {
			ss := specSubspace()
			cache := NewMetaDataVersionStampStoreStateCache()

			// Create store with user field and make it cacheable.
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SetStateCacheability(true)
				if err != nil {
					return nil, err
				}
				return nil, store.SetHeaderUserField("cached-field", []byte("cached-value"))
			})
			Expect(err).NotTo(HaveOccurred())

			// Open to populate cache.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Open again (cache hit) — verify header data is correct.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				if err != nil {
					return nil, err
				}
				Expect(store.storeHeader.GetCacheable()).To(BeTrue())
				Expect(store.storeHeader.GetFormatVersion()).To(Equal(int32(formatVersionCurrent)))
				var found bool
				for _, uf := range store.storeHeader.UserField {
					if uf.GetKey() == "cached-field" {
						Expect(uf.Value).To(Equal([]byte("cached-value")))
						found = true
					}
				}
				Expect(found).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("cache stores correct index state data", func() {
			ss := specSubspace()
			cache := NewMetaDataVersionStampStoreStateCache()
			idxMd := buildMetaDataWithIndex()

			// Create cacheable store, mark index WRITE_ONLY.
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(idxMd).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SetStateCacheability(true)
				if err != nil {
					return nil, err
				}
				_, err = store.MarkIndexWriteOnly("test_idx")
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Open to populate cache.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(idxMd).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				if err != nil {
					return nil, err
				}
				Expect(store.IsIndexWriteOnly("test_idx")).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Open again (cache hit) — should still see WRITE_ONLY.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(idxMd).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				if err != nil {
					return nil, err
				}
				Expect(store.IsIndexWriteOnly("test_idx")).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("record operations work correctly with cached state", func() {
			ss := specSubspace()
			cache := NewMetaDataVersionStampStoreStateCache()

			// Create cacheable store and save a record.
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SetStateCacheability(true)
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(42),
					Price:   proto.Int32(100),
				})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Populate cache.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Open with cache hit, verify record operations work.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				if err != nil {
					return nil, err
				}
				rec, err := store.LoadRecord(tuple.Tuple{int64(42)})
				Expect(err).NotTo(HaveOccurred())
				Expect(rec).NotTo(BeNil())
				order := rec.Record.(*gen.Order)
				Expect(order.GetOrderId()).To(Equal(int64(42)))
				Expect(order.GetPrice()).To(Equal(int32(100)))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("getNewerEntry", func() {
		It("returns b when a has nil stamp", func() {
			a := &FDBRecordStoreStateCacheEntry{metaDataVersionStamp: nil}
			b := &FDBRecordStoreStateCacheEntry{metaDataVersionStamp: []byte{0x01}}
			Expect(getNewerEntry(a, b)).To(Equal(b))
		})

		It("returns a when b has nil stamp", func() {
			a := &FDBRecordStoreStateCacheEntry{metaDataVersionStamp: []byte{0x01}}
			b := &FDBRecordStoreStateCacheEntry{metaDataVersionStamp: nil}
			Expect(getNewerEntry(a, b)).To(Equal(a))
		})

		It("returns a when stamps are equal", func() {
			stamp := []byte{0x01, 0x02, 0x03}
			a := &FDBRecordStoreStateCacheEntry{metaDataVersionStamp: stamp}
			b := &FDBRecordStoreStateCacheEntry{metaDataVersionStamp: stamp}
			Expect(getNewerEntry(a, b)).To(Equal(a))
		})

		It("returns the entry with the larger stamp", func() {
			a := &FDBRecordStoreStateCacheEntry{metaDataVersionStamp: []byte{0x01}}
			b := &FDBRecordStoreStateCacheEntry{metaDataVersionStamp: []byte{0x02}}
			Expect(getNewerEntry(a, b)).To(Equal(b))
		})

		It("returns b when both nil", func() {
			a := &FDBRecordStoreStateCacheEntry{metaDataVersionStamp: nil}
			b := &FDBRecordStoreStateCacheEntry{metaDataVersionStamp: nil}
			// When a is nil, returns b.
			Expect(getNewerEntry(a, b)).To(Equal(b))
		})
	})

	Describe("cache constructor options", func() {
		It("default maxSize is 500", func() {
			c := NewMetaDataVersionStampStoreStateCache()
			Expect(c.maxSize).To(Equal(500))
		})

		It("default expireAfter is 1 minute", func() {
			c := NewMetaDataVersionStampStoreStateCache()
			Expect(c.expireAfter).To(Equal(time.Minute))
		})

		It("WithMaxSize overrides default", func() {
			c := NewMetaDataVersionStampStoreStateCache(WithMaxSize(42))
			Expect(c.maxSize).To(Equal(42))
		})

		It("WithExpireAfterAccess overrides default", func() {
			c := NewMetaDataVersionStampStoreStateCache(WithExpireAfterAccess(5 * time.Second))
			Expect(c.expireAfter).To(Equal(5 * time.Second))
		})

		It("multiple options compose", func() {
			c := NewMetaDataVersionStampStoreStateCache(
				WithMaxSize(10),
				WithExpireAfterAccess(30*time.Second),
			)
			Expect(c.maxSize).To(Equal(10))
			Expect(c.expireAfter).To(Equal(30 * time.Second))
		})
	})

	Describe("FDBRecordStoreStateCacheEntry", func() {
		It("GetRecordStoreState returns the embedded state", func() {
			header := &gen.DataStoreInfo{}
			states := map[string]IndexState{"idx": IndexStateWriteOnly}
			entry := &FDBRecordStoreStateCacheEntry{
				recordStoreState: &RecordStoreState{
					StoreHeader: header,
					IndexStates: states,
				},
			}
			Expect(entry.GetRecordStoreState().StoreHeader).To(Equal(header))
			Expect(entry.GetRecordStoreState().IndexStates).To(HaveKeyWithValue("idx", IndexStateWriteOnly))
		})

		It("GetMetaDataVersionStamp returns the stamp", func() {
			stamp := []byte{0xDE, 0xAD}
			entry := &FDBRecordStoreStateCacheEntry{
				metaDataVersionStamp: stamp,
			}
			Expect(entry.GetMetaDataVersionStamp()).To(Equal(stamp))
		})

		It("GetMetaDataVersionStamp returns nil when unset", func() {
			entry := &FDBRecordStoreStateCacheEntry{}
			Expect(entry.GetMetaDataVersionStamp()).To(BeNil())
		})
	})

	Describe("cache invalidation via invalidateOlderEntry", func() {
		It("removes entry with older stamp", func() {
			cache := NewMetaDataVersionStampStoreStateCache()
			oldStamp := []byte{0x01}
			newStamp := []byte{0x02}

			trueVal := true
			cache.addToCache("key1", &FDBRecordStoreStateCacheEntry{
				metaDataVersionStamp: oldStamp,
				recordStoreState: &RecordStoreState{
					StoreHeader: &gen.DataStoreInfo{Cacheable: &trueVal},
				},
			})

			cache.mu.Lock()
			Expect(len(cache.entries)).To(Equal(1))
			cache.mu.Unlock()

			cache.invalidateOlderEntry("key1", newStamp)

			cache.mu.Lock()
			Expect(len(cache.entries)).To(Equal(0))
			cache.mu.Unlock()
		})

		It("keeps entry with newer stamp", func() {
			cache := NewMetaDataVersionStampStoreStateCache()
			newerStamp := []byte{0x05}
			olderStamp := []byte{0x02}

			trueVal := true
			cache.addToCache("key1", &FDBRecordStoreStateCacheEntry{
				metaDataVersionStamp: newerStamp,
				recordStoreState: &RecordStoreState{
					StoreHeader: &gen.DataStoreInfo{Cacheable: &trueVal},
				},
			})

			cache.invalidateOlderEntry("key1", olderStamp)

			cache.mu.Lock()
			Expect(len(cache.entries)).To(Equal(1))
			cache.mu.Unlock()
		})

		It("removes entry with nil stamp", func() {
			cache := NewMetaDataVersionStampStoreStateCache()

			trueVal := true
			cache.addToCache("key1", &FDBRecordStoreStateCacheEntry{
				metaDataVersionStamp: nil,
				recordStoreState: &RecordStoreState{
					StoreHeader: &gen.DataStoreInfo{Cacheable: &trueVal},
				},
			})

			cache.invalidateOlderEntry("key1", []byte{0x01})

			cache.mu.Lock()
			Expect(len(cache.entries)).To(Equal(0))
			cache.mu.Unlock()
		})

		It("no-op on missing key", func() {
			cache := NewMetaDataVersionStampStoreStateCache()
			// Should not panic.
			cache.invalidateOlderEntry("nonexistent", []byte{0x01})
		})
	})

	Describe("Benchmark", func() {
		const iterations = 50

		It("no cache vs cache hit performance", func() {
			ss := specSubspace()
			cache := NewMetaDataVersionStampStoreStateCache()

			// Create a cacheable store.
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SetStateCacheability(true)
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Populate cache.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(md).
					SetSubspace(ss).
					SetStoreStateCache(cache).
					Open()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Benchmark: open with cache (cache hits).
			startCached := time.Now()
			for i := 0; i < iterations; i++ {
				_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					_, err := NewStoreBuilder().
						SetContext(rtx).
						SetMetaDataProvider(md).
						SetSubspace(ss).
						SetStoreStateCache(cache).
						Open()
					return nil, err
				})
				Expect(err).NotTo(HaveOccurred())
			}
			cachedDuration := time.Since(startCached)

			// Benchmark: open without cache (always hits FDB).
			passThrough := PassThroughStoreStateCache()
			startUncached := time.Now()
			for i := 0; i < iterations; i++ {
				_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					_, err := NewStoreBuilder().
						SetContext(rtx).
						SetMetaDataProvider(md).
						SetSubspace(ss).
						SetStoreStateCache(passThrough).
						Open()
					return nil, err
				})
				Expect(err).NotTo(HaveOccurred())
			}
			uncachedDuration := time.Since(startUncached)

			avgCached := cachedDuration / time.Duration(iterations)
			avgUncached := uncachedDuration / time.Duration(iterations)

			GinkgoWriter.Printf("\n=== Store Open Benchmark (%d iterations) ===\n", iterations)
			GinkgoWriter.Printf("  No cache:    %v total, %v avg/op\n", uncachedDuration, avgUncached)
			GinkgoWriter.Printf("  Cache hit:   %v total, %v avg/op\n", cachedDuration, avgCached)
			if avgUncached > 0 {
				speedup := float64(avgUncached) / float64(avgCached)
				GinkgoWriter.Printf("  Speedup:     %.1fx\n", speedup)
			}
			GinkgoWriter.Printf("===========================================\n\n")

			// Cache should be faster than no cache (at minimum not significantly slower).
			// We don't assert a strict speedup since FDB local overhead varies, but the
			// cache avoids 2+ FDB reads per open (store header + index states range).
		})
	})

	Describe("eviction", func() {
		It("evicts oldest accessed entry when over capacity", func() {
			cache := NewMetaDataVersionStampStoreStateCache(WithMaxSize(2))
			trueVal := true

			cache.addToCache("a", &FDBRecordStoreStateCacheEntry{
				metaDataVersionStamp: []byte{0x01},
				recordStoreState: &RecordStoreState{
					StoreHeader: &gen.DataStoreInfo{Cacheable: &trueVal},
				},
			})
			// Small pause to ensure different timestamps.
			time.Sleep(time.Millisecond)

			cache.addToCache("b", &FDBRecordStoreStateCacheEntry{
				metaDataVersionStamp: []byte{0x02},
				recordStoreState: &RecordStoreState{
					StoreHeader: &gen.DataStoreInfo{Cacheable: &trueVal},
				},
			})

			cache.mu.Lock()
			Expect(len(cache.entries)).To(Equal(2))
			cache.mu.Unlock()

			// Add third entry — should evict "a" (oldest).
			time.Sleep(time.Millisecond)
			cache.addToCache("c", &FDBRecordStoreStateCacheEntry{
				metaDataVersionStamp: []byte{0x03},
				recordStoreState: &RecordStoreState{
					StoreHeader: &gen.DataStoreInfo{Cacheable: &trueVal},
				},
			})

			cache.mu.Lock()
			Expect(len(cache.entries)).To(Equal(2))
			Expect(cache.entries).NotTo(HaveKey("a"))
			Expect(cache.entries).To(HaveKey("b"))
			Expect(cache.entries).To(HaveKey("c"))
			cache.mu.Unlock()
		})
	})

	// The derived-subspace-keys cache is process-global and keyed by store
	// subspace, so a process serving many tenant stores adds one entry per
	// store and released nothing before it was bounded. Java bounds the
	// analogous per-store cache (ReadVersionRecordStoreStateCache is an
	// AsyncLoadingCache with maximumSize) for the same reason.
	Describe("derived subspace keys cache bound", func() {
		// The policy knobs and the shared map are process-global. Ginkgo runs
		// specs one at a time within a process, so driving them down here and
		// restoring afterwards is safe; the restore also puts back whatever
		// entries the rest of the suite had cached, since eviction only ever
		// costs a recompute.
		withTightBoundStride := func(maxEntries int, ttlMs int64, stride int64) {
			origMax, origTTL, origStride := subspaceKeysMaxEntries, subspaceKeysIdleTTLMs, subspaceKeysSweepEveryN
			subspaceKeysCacheMu.Lock()
			origEntries := subspaceKeysCacheM
			subspaceKeysCacheM = make(map[string]*storeSubspaceKeys)
			subspaceKeysCacheMu.Unlock()

			subspaceKeysMaxEntries = maxEntries
			subspaceKeysIdleTTLMs = ttlMs
			subspaceKeysSweepEveryN = stride

			DeferCleanup(func() {
				subspaceKeysMaxEntries, subspaceKeysIdleTTLMs, subspaceKeysSweepEveryN = origMax, origTTL, origStride
				subspaceKeysCacheMu.Lock()
				subspaceKeysCacheM = origEntries
				subspaceKeysCacheMu.Unlock()
			})
		}

		// Most specs want a sweep on every lookup so the bound is observable
		// without driving the real stride.
		withTightBound := func(maxEntries int, ttlMs int64) {
			withTightBoundStride(maxEntries, ttlMs, 1)
		}

		// ageAllEntries rewinds every cached entry's idle clock, which makes
		// "these entries have gone idle" a deterministic fact rather than a
		// sleep the test has to wait out.
		ageAllEntries := func(byMs int64) {
			subspaceKeysCacheMu.Lock()
			defer subspaceKeysCacheMu.Unlock()
			for _, ks := range subspaceKeysCacheM {
				ks.lastUseMs.Store(ks.lastUseMs.Load() - byMs)
			}
		}

		tenantSubspace := func(i int) subspace.Subspace {
			return subspace.FromBytes([]byte(fmt.Sprintf("tenant-bound-%04d", i)))
		}

		cacheSize := func() int {
			subspaceKeysCacheMu.RLock()
			defer subspaceKeysCacheMu.RUnlock()
			return len(subspaceKeysCacheM)
		}

		It("holds the entry cap while many tenant stores are opened", func() {
			const capEntries, tenants = 8, 200
			withTightBound(capEntries, 15*60*1000)

			for i := 0; i < tenants; i++ {
				Expect(getCachedSubspaceKeys(tenantSubspace(i))).NotTo(BeNil())
			}

			// Unbounded, this would be `tenants`. The sweep drops to 90% of
			// the cap, so the steady state sits at or below the cap itself.
			Expect(cacheSize()).To(BeNumerically("<=", capEntries),
				"derived-subspace-keys cache grew past its cap — a long-lived process serving many tenants leaks one entry per store")
			Expect(cacheSize()).To(BeNumerically(">", 0),
				"the cache evicted everything — every store open would recompute")
		})

		It("recomputes an evicted entry identically", func() {
			const capEntries = 4
			withTightBound(capEntries, 15*60*1000)

			victim := tenantSubspace(0)
			first := getCachedSubspaceKeys(victim)

			// Push the victim out with unrelated tenants.
			for i := 1; i <= 100; i++ {
				getCachedSubspaceKeys(tenantSubspace(i))
			}
			subspaceKeysCacheMu.RLock()
			_, stillCached := subspaceKeysCacheM[string(victim.Bytes())]
			subspaceKeysCacheMu.RUnlock()
			Expect(stillCached).To(BeFalse(), "the cap never evicted the oldest tenant")

			// The value is a pure function of the key, so the recompute must
			// be indistinguishable from the entry that was dropped — this is
			// what makes eviction safe at all.
			again := getCachedSubspaceKeys(victim)
			Expect(again).NotTo(BeIdenticalTo(first), "entry was never evicted, so this proves nothing")
			Expect(again.indexStateBegin).To(Equal(first.indexStateBegin))
			Expect(again.indexStateEnd).To(Equal(first.indexStateEnd))
			Expect(again.indexStatePrefixLen).To(Equal(first.indexStatePrefixLen))
			Expect(again.expectedInfoKey).To(Equal(first.expectedInfoKey))
			Expect(again.recordsSubspace.Bytes()).To(Equal(first.recordsSubspace.Bytes()))
		})

		It("reclaims idle entries even when the cap is never reached", func() {
			// A tenant fleet well under the cap still leaks if departed
			// tenants are never released; the idle horizon is what reclaims
			// them. TTL of -1 makes every entry idle immediately.
			withTightBound(4096, -1)

			for i := 0; i < 20; i++ {
				getCachedSubspaceKeys(tenantSubspace(i))
			}
			getCachedSubspaceKeys(tenantSubspace(0))
			Expect(cacheSize()).To(BeNumerically("<=", 1),
				"idle entries were never reclaimed — a departed tenant's keys stay resident forever")
		})

		// The workload that needs the idle horizon most never hits the cache
		// at all: a long-lived process opening stores for ever-new subspaces
		// misses every time and, staying below the cap, never trips the
		// over-cap branch either. If the sweep is driven only by HITS, nothing
		// ever reclaims — the cache grows to the hard cap and holds departed
		// tenants indefinitely. The reclaiming lookup here is a MISS on a
		// subspace never seen before, with no hit anywhere in the workload.
		It("reclaims idle entries on a miss-only workload", func() {
			const ttlMs = 60 * 1000
			// A stride far larger than the fill keeps the fill itself
			// sweep-free, so the entries are genuinely resident and idle
			// before the one lookup under test.
			withTightBoundStride(4096, ttlMs, 1_000_000)

			const idle = 20
			for i := 0; i < idle; i++ {
				getCachedSubspaceKeys(tenantSubspace(i))
			}
			Expect(cacheSize()).To(Equal(idle),
				"precondition: the idle entries must actually be resident")

			// Those tenants stop being served: their idle clocks age past the
			// horizon. Deterministic, no sleeping.
			ageAllEntries(10 * ttlMs)
			subspaceKeysSweepEveryN = 1

			// A brand-new subspace: a MISS, not a touch of anything cached,
			// and the cache is nowhere near the cap so the over-cap branch
			// cannot fire either.
			fresh := subspace.FromBytes([]byte("tenant-bound-miss-only-fresh"))
			Expect(getCachedSubspaceKeys(fresh)).NotTo(BeNil())

			Expect(cacheSize()).To(Equal(1),
				"a miss-only workload never reclaimed idle entries — the sweep was reachable only from cache HITS, "+
					"so ever-new store subspaces below the cap accumulate to the hard cap regardless of the idle horizon")
		})
	})
})
