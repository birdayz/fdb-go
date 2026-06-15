package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("rankedSet (skip-list)", func() {
	ctx := context.Background()

	// runInTx is a helper that runs a function in a single FDB transaction.
	runInTx := func(fn func(tx fdb.WritableTransaction)) {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			fn(rtx.Transaction())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	}

	Describe("newRankedSet and config", func() {
		It("defaults NLevels to 6 when zero", func() {
			sub := specSubspace().Sub("cfg-zero")
			rs := newRankedSet(sub, rankedSetConfig{})
			Expect(rs.config.NLevels).To(Equal(rankedSetDefaultLevels))
			Expect(rs.config.HashFunction).NotTo(BeNil())
		})

		It("defaults NLevels to 6 when negative", func() {
			sub := specSubspace().Sub("cfg-neg")
			rs := newRankedSet(sub, rankedSetConfig{NLevels: -5})
			Expect(rs.config.NLevels).To(Equal(rankedSetDefaultLevels))
		})

		It("clamps NLevels to max 8", func() {
			sub := specSubspace().Sub("cfg-big")
			rs := newRankedSet(sub, rankedSetConfig{NLevels: 100})
			Expect(rs.config.NLevels).To(Equal(rankedSetMaxLevels))
		})

		It("respects explicit NLevels within bounds", func() {
			sub := specSubspace().Sub("cfg-explicit")
			rs := newRankedSet(sub, rankedSetConfig{NLevels: 3})
			Expect(rs.config.NLevels).To(Equal(3))
		})

		It("uses jdkArrayHash by default", func() {
			sub := specSubspace().Sub("cfg-hash")
			rs := newRankedSet(sub, rankedSetConfig{})
			// Verify hash function is set (compare known value)
			Expect(rs.config.HashFunction([]byte("test"))).To(Equal(jdkArrayHash([]byte("test"))))
		})

		It("uses custom hash function when provided", func() {
			sub := specSubspace().Sub("cfg-crc")
			rs := newRankedSet(sub, rankedSetConfig{HashFunction: crcHash})
			Expect(rs.config.HashFunction([]byte("test"))).To(Equal(crcHash([]byte("test"))))
		})
	})

	Describe("Init and InitNeeded", func() {
		It("reports init needed on fresh subspace", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("init-needed")
				rs := newRankedSet(sub, defaultRankedSetConfig)

				needed, err := rs.InitNeeded(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(needed).To(BeTrue())
			})
		})

		It("reports init not needed after Init", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("init-done")
				rs := newRankedSet(sub, defaultRankedSetConfig)

				Expect(rs.Init(tx)).To(Succeed())

				needed, err := rs.InitNeeded(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(needed).To(BeFalse())
			})
		})

		It("Init is idempotent", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("init-idempotent")
				rs := newRankedSet(sub, defaultRankedSetConfig)

				Expect(rs.Init(tx)).To(Succeed())

				// Add something
				_, err := rs.Add(tx, []byte("x"))
				Expect(err).NotTo(HaveOccurred())

				// Init again should not destroy data
				Expect(rs.Init(tx)).To(Succeed())

				has, err := rs.Contains(tx, []byte("x"))
				Expect(err).NotTo(HaveOccurred())
				Expect(has).To(BeTrue())

				size, err := rs.Size(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(Equal(int64(1)))
			})
		})

		It("initializes sentinel entries at all levels", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("init-sentinels")
				nLevels := 4
				rs := newRankedSet(sub, rankedSetConfig{NLevels: nLevels})

				Expect(rs.Init(tx)).To(Succeed())

				// Verify sentinel at each level
				for level := 0; level < nLevels; level++ {
					k := fdb.Key(sub.Pack(tuple.Tuple{int64(level), []byte{}}))
					v, err := tx.Get(k).Get()
					Expect(err).NotTo(HaveOccurred())
					Expect(v).NotTo(BeNil(), "sentinel missing at level %d", level)
					val, err2 := rsDecodeLong(v)
					Expect(err2).NotTo(HaveOccurred())
					Expect(val).To(Equal(int64(0)), "sentinel count at level %d", level)
				}
			})
		})
	})

	Describe("Add", func() {
		It("rejects empty key", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("add-empty")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				_, err := rs.Add(tx, []byte{})
				Expect(err).To(HaveOccurred())

				var emptyKeyErr *rankedSetEmptyKeyError
				Expect(errors.As(err, &emptyKeyErr)).To(BeTrue())
			})
		})

		It("rejects nil key", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("add-nil")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				_, err := rs.Add(tx, nil)
				Expect(err).To(HaveOccurred())

				var emptyKeyErr *rankedSetEmptyKeyError
				Expect(errors.As(err, &emptyKeyErr)).To(BeTrue())
			})
		})

		It("returns true on first add, false on duplicate without CountDuplicates", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("add-dup-no-count")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				added, err := rs.Add(tx, []byte("key"))
				Expect(err).NotTo(HaveOccurred())
				Expect(added).To(BeTrue())

				added, err = rs.Add(tx, []byte("key"))
				Expect(err).NotTo(HaveOccurred())
				Expect(added).To(BeFalse())

				// Size should be 1 (not 2)
				size, err := rs.Size(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(Equal(int64(1)))
			})
		})

		It("handles a single byte key", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("add-single-byte")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				added, err := rs.Add(tx, []byte{0x01})
				Expect(err).NotTo(HaveOccurred())
				Expect(added).To(BeTrue())

				has, err := rs.Contains(tx, []byte{0x01})
				Expect(err).NotTo(HaveOccurred())
				Expect(has).To(BeTrue())
			})
		})

		It("handles long keys", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("add-long-key")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				// 500-byte key (well within FDB limits)
				longKey := make([]byte, 500)
				for i := range longKey {
					longKey[i] = byte(i % 256)
				}

				added, err := rs.Add(tx, longKey)
				Expect(err).NotTo(HaveOccurred())
				Expect(added).To(BeTrue())

				has, err := rs.Contains(tx, longKey)
				Expect(err).NotTo(HaveOccurred())
				Expect(has).To(BeTrue())

				rank, err := rs.Rank(tx, longKey, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(*rank).To(Equal(int64(0)))
			})
		})

		It("adds elements in reverse sorted order and maintains correct ranks", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("add-reverse")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				// Insert z, y, x, ..., a (reverse order)
				keys := []string{"z", "y", "x", "w", "v", "u", "t", "s", "r", "q"}
				for _, k := range keys {
					_, err := rs.Add(tx, []byte(k))
					Expect(err).NotTo(HaveOccurred())
				}

				size, err := rs.Size(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(Equal(int64(len(keys))))

				// Sorted order: q, r, s, t, u, v, w, x, y, z
				sorted := make([]string, len(keys))
				copy(sorted, keys)
				sort.Strings(sorted)

				for i, k := range sorted {
					nth, err := rs.GetNth(tx, int64(i))
					Expect(err).NotTo(HaveOccurred())
					Expect(nth).To(Equal([]byte(k)), "GetNth(%d) should be %q", i, k)
				}
			})
		})

		It("adds binary keys and maintains byte ordering", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("add-binary")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				// Keys with high byte values (0x80+) to exercise signed/unsigned handling
				keys := [][]byte{
					{0xFF},
					{0x00, 0x01},
					{0x80},
					{0x01},
					{0x7F},
				}
				for _, k := range keys {
					_, err := rs.Add(tx, k)
					Expect(err).NotTo(HaveOccurred())
				}

				// Verify ordering: FDB uses lexicographic byte order
				sorted := make([][]byte, len(keys))
				copy(sorted, keys)
				sort.Slice(sorted, func(i, j int) bool {
					return bytes.Compare(sorted[i], sorted[j]) < 0
				})

				for i, expected := range sorted {
					nth, err := rs.GetNth(tx, int64(i))
					Expect(err).NotTo(HaveOccurred())
					Expect(nth).To(Equal(expected), "GetNth(%d) mismatch", i)
				}
			})
		})
	})

	Describe("Remove", func() {
		It("rejects empty key", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("rm-empty")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				_, err := rs.Remove(tx, []byte{})
				var emptyKeyErr *rankedSetEmptyKeyError
				Expect(errors.As(err, &emptyKeyErr)).To(BeTrue())
			})
		})

		It("returns false for removing from empty set", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("rm-empty-set")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				removed, err := rs.Remove(tx, []byte("nope"))
				Expect(err).NotTo(HaveOccurred())
				Expect(removed).To(BeFalse())
			})
		})

		It("removes the only element leaving an empty set", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("rm-last")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				_, err := rs.Add(tx, []byte("only"))
				Expect(err).NotTo(HaveOccurred())

				removed, err := rs.Remove(tx, []byte("only"))
				Expect(err).NotTo(HaveOccurred())
				Expect(removed).To(BeTrue())

				size, err := rs.Size(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(Equal(int64(0)))

				has, err := rs.Contains(tx, []byte("only"))
				Expect(err).NotTo(HaveOccurred())
				Expect(has).To(BeFalse())

				// GetNth on empty set
				nth, err := rs.GetNth(tx, 0)
				Expect(err).NotTo(HaveOccurred())
				Expect(nth).To(BeNil())
			})
		})

		It("removes first element and shifts ranks", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("rm-first")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				for _, k := range []string{"a", "b", "c"} {
					_, err := rs.Add(tx, []byte(k))
					Expect(err).NotTo(HaveOccurred())
				}

				removed, err := rs.Remove(tx, []byte("a"))
				Expect(err).NotTo(HaveOccurred())
				Expect(removed).To(BeTrue())

				// b is now rank 0, c is rank 1
				nth, err := rs.GetNth(tx, 0)
				Expect(err).NotTo(HaveOccurred())
				Expect(nth).To(Equal([]byte("b")))

				nth, err = rs.GetNth(tx, 1)
				Expect(err).NotTo(HaveOccurred())
				Expect(nth).To(Equal([]byte("c")))

				rank, err := rs.Rank(tx, []byte("b"), false)
				Expect(err).NotTo(HaveOccurred())
				Expect(*rank).To(Equal(int64(0)))
			})
		})

		It("removes last element and preserves others", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("rm-last-of-three")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				for _, k := range []string{"a", "b", "c"} {
					_, err := rs.Add(tx, []byte(k))
					Expect(err).NotTo(HaveOccurred())
				}

				removed, err := rs.Remove(tx, []byte("c"))
				Expect(err).NotTo(HaveOccurred())
				Expect(removed).To(BeTrue())

				size, err := rs.Size(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(Equal(int64(2)))

				nth, err := rs.GetNth(tx, 1)
				Expect(err).NotTo(HaveOccurred())
				Expect(nth).To(Equal([]byte("b")))

				// Out of bounds now
				nth, err = rs.GetNth(tx, 2)
				Expect(err).NotTo(HaveOccurred())
				Expect(nth).To(BeNil())
			})
		})

		It("removes all elements one by one", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("rm-all")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				keys := []string{"a", "b", "c", "d", "e"}
				for _, k := range keys {
					_, err := rs.Add(tx, []byte(k))
					Expect(err).NotTo(HaveOccurred())
				}

				for i, k := range keys {
					removed, err := rs.Remove(tx, []byte(k))
					Expect(err).NotTo(HaveOccurred())
					Expect(removed).To(BeTrue(), "should remove %q", k)

					size, err := rs.Size(tx)
					Expect(err).NotTo(HaveOccurred())
					Expect(size).To(Equal(int64(len(keys)-i-1)), "size after removing %q", k)
				}

				size, err := rs.Size(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(Equal(int64(0)))
			})
		})

		It("can re-add after remove", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("rm-readd")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				_, err := rs.Add(tx, []byte("key"))
				Expect(err).NotTo(HaveOccurred())

				_, err = rs.Remove(tx, []byte("key"))
				Expect(err).NotTo(HaveOccurred())

				added, err := rs.Add(tx, []byte("key"))
				Expect(err).NotTo(HaveOccurred())
				Expect(added).To(BeTrue())

				has, err := rs.Contains(tx, []byte("key"))
				Expect(err).NotTo(HaveOccurred())
				Expect(has).To(BeTrue())

				size, err := rs.Size(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(Equal(int64(1)))
			})
		})
	})

	Describe("CountDuplicates", func() {
		newDupSet := func(tx fdb.WritableTransaction, name string) *rankedSet {
			sub := specSubspace().Sub(name)
			rs := newRankedSet(sub, rankedSetConfig{
				NLevels:         rankedSetDefaultLevels,
				CountDuplicates: true,
			})
			Expect(rs.Init(tx)).To(Succeed())
			return rs
		}

		It("increments count on duplicate add", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				rs := newDupSet(tx, "dup-inc")

				for i := 0; i < 5; i++ {
					added, err := rs.Add(tx, []byte("same"))
					Expect(err).NotTo(HaveOccurred())
					Expect(added).To(BeTrue())
				}

				count, err := rs.Count(tx, []byte("same"))
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(5)))

				size, err := rs.Size(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(Equal(int64(5)))
			})
		})

		It("decrements count on remove, only fully removes at zero", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				rs := newDupSet(tx, "dup-dec")

				// Add 3 copies
				for i := 0; i < 3; i++ {
					_, err := rs.Add(tx, []byte("x"))
					Expect(err).NotTo(HaveOccurred())
				}

				// Remove 1: count should be 2
				removed, err := rs.Remove(tx, []byte("x"))
				Expect(err).NotTo(HaveOccurred())
				Expect(removed).To(BeTrue())

				count, err := rs.Count(tx, []byte("x"))
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(2)))

				has, err := rs.Contains(tx, []byte("x"))
				Expect(err).NotTo(HaveOccurred())
				Expect(has).To(BeTrue())

				// Remove 1 more: count = 1
				_, err = rs.Remove(tx, []byte("x"))
				Expect(err).NotTo(HaveOccurred())

				count, err = rs.Count(tx, []byte("x"))
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(1)))

				// Remove last copy
				removed, err = rs.Remove(tx, []byte("x"))
				Expect(err).NotTo(HaveOccurred())
				Expect(removed).To(BeTrue())

				has, err = rs.Contains(tx, []byte("x"))
				Expect(err).NotTo(HaveOccurred())
				Expect(has).To(BeFalse())

				count, err = rs.Count(tx, []byte("x"))
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(0)))

				size, err := rs.Size(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(Equal(int64(0)))
			})
		})

		It("handles mixed duplicates and unique keys", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				rs := newDupSet(tx, "dup-mixed")

				// "a" x3, "b" x1, "c" x2
				for i := 0; i < 3; i++ {
					_, err := rs.Add(tx, []byte("a"))
					Expect(err).NotTo(HaveOccurred())
				}
				_, err := rs.Add(tx, []byte("b"))
				Expect(err).NotTo(HaveOccurred())
				for i := 0; i < 2; i++ {
					_, err = rs.Add(tx, []byte("c"))
					Expect(err).NotTo(HaveOccurred())
				}

				// Total size = 6
				size, err := rs.Size(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(Equal(int64(6)))

				// Ranks: a(0), a(1), a(2), b(3), c(4), c(5)
				rank, err := rs.Rank(tx, []byte("b"), false)
				Expect(err).NotTo(HaveOccurred())
				Expect(*rank).To(Equal(int64(3)))

				rank, err = rs.Rank(tx, []byte("c"), false)
				Expect(err).NotTo(HaveOccurred())
				Expect(*rank).To(Equal(int64(4)))

				// GetNth
				nth, err := rs.GetNth(tx, 3)
				Expect(err).NotTo(HaveOccurred())
				Expect(nth).To(Equal([]byte("b")))

				nth, err = rs.GetNth(tx, 0)
				Expect(err).NotTo(HaveOccurred())
				Expect(nth).To(Equal([]byte("a")))

				nth, err = rs.GetNth(tx, 2)
				Expect(err).NotTo(HaveOccurred())
				Expect(nth).To(Equal([]byte("a")))

				nth, err = rs.GetNth(tx, 4)
				Expect(err).NotTo(HaveOccurred())
				Expect(nth).To(Equal([]byte("c")))
			})
		})

		It("remove returns false after all duplicates exhausted", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				rs := newDupSet(tx, "dup-exhaust")

				_, err := rs.Add(tx, []byte("k"))
				Expect(err).NotTo(HaveOccurred())

				removed, err := rs.Remove(tx, []byte("k"))
				Expect(err).NotTo(HaveOccurred())
				Expect(removed).To(BeTrue())

				// Already gone
				removed, err = rs.Remove(tx, []byte("k"))
				Expect(err).NotTo(HaveOccurred())
				Expect(removed).To(BeFalse())
			})
		})
	})

	Describe("Rank", func() {
		It("rejects empty key", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("rank-empty")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				_, err := rs.Rank(tx, []byte{}, false)
				var emptyKeyErr *rankedSetEmptyKeyError
				Expect(errors.As(err, &emptyKeyErr)).To(BeTrue())
			})
		})

		It("returns insertion-point rank for non-existent key with nullIfMissing=false", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("rank-insertion")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				for _, k := range []string{"b", "d", "f"} {
					_, err := rs.Add(tx, []byte(k))
					Expect(err).NotTo(HaveOccurred())
				}

				// "a" would be at rank 0 (before "b")
				rank, err := rs.Rank(tx, []byte("a"), false)
				Expect(err).NotTo(HaveOccurred())
				Expect(*rank).To(Equal(int64(0)))

				// "c" would be at rank 1 (after "b", before "d")
				rank, err = rs.Rank(tx, []byte("c"), false)
				Expect(err).NotTo(HaveOccurred())
				Expect(*rank).To(Equal(int64(1)))

				// "e" would be at rank 2 (after "d", before "f")
				rank, err = rs.Rank(tx, []byte("e"), false)
				Expect(err).NotTo(HaveOccurred())
				Expect(*rank).To(Equal(int64(2)))

				// "g" would be at rank 3 (after "f")
				rank, err = rs.Rank(tx, []byte("g"), false)
				Expect(err).NotTo(HaveOccurred())
				Expect(*rank).To(Equal(int64(3)))
			})
		})

		It("returns nil for non-existent key with nullIfMissing=true", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("rank-null")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				_, err := rs.Add(tx, []byte("a"))
				Expect(err).NotTo(HaveOccurred())

				rank, err := rs.Rank(tx, []byte("missing"), true)
				Expect(err).NotTo(HaveOccurred())
				Expect(rank).To(BeNil())
			})
		})

		It("returns correct rank for existing key with nullIfMissing=true", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("rank-exists-null")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				for _, k := range []string{"a", "b", "c"} {
					_, err := rs.Add(tx, []byte(k))
					Expect(err).NotTo(HaveOccurred())
				}

				rank, err := rs.Rank(tx, []byte("b"), true)
				Expect(err).NotTo(HaveOccurred())
				Expect(rank).NotTo(BeNil())
				Expect(*rank).To(Equal(int64(1)))
			})
		})

		It("rank is consistent with GetNth for many elements", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("rank-consistency")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				n := 100
				for i := range n {
					key := tuple.Tuple{int64(i)}.Pack()
					_, err := rs.Add(tx, key)
					Expect(err).NotTo(HaveOccurred())
				}

				for i := range n {
					key := tuple.Tuple{int64(i)}.Pack()
					rank, err := rs.Rank(tx, key, false)
					Expect(err).NotTo(HaveOccurred())
					Expect(*rank).To(Equal(int64(i)), "Rank(%d)", i)

					nth, err := rs.GetNth(tx, int64(i))
					Expect(err).NotTo(HaveOccurred())
					Expect(nth).To(Equal(key), "GetNth(%d)", i)
				}
			})
		})
	})

	Describe("GetNth", func() {
		It("returns nil for negative rank", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("nth-neg")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				_, err := rs.Add(tx, []byte("a"))
				Expect(err).NotTo(HaveOccurred())

				nth, err := rs.GetNth(tx, -1)
				Expect(err).NotTo(HaveOccurred())
				Expect(nth).To(BeNil())

				nth, err = rs.GetNth(tx, -100)
				Expect(err).NotTo(HaveOccurred())
				Expect(nth).To(BeNil())
			})
		})

		It("returns nil for rank beyond size", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("nth-oob")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				_, err := rs.Add(tx, []byte("only"))
				Expect(err).NotTo(HaveOccurred())

				nth, err := rs.GetNth(tx, 1)
				Expect(err).NotTo(HaveOccurred())
				Expect(nth).To(BeNil())

				nth, err = rs.GetNth(tx, 999)
				Expect(err).NotTo(HaveOccurred())
				Expect(nth).To(BeNil())
			})
		})

		It("returns nil on empty set", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("nth-empty")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				nth, err := rs.GetNth(tx, 0)
				Expect(err).NotTo(HaveOccurred())
				Expect(nth).To(BeNil())
			})
		})

		It("correct after interleaved adds and removes", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("nth-interleave")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				// Add a, b, c, d, e
				for _, k := range []string{"a", "b", "c", "d", "e"} {
					_, err := rs.Add(tx, []byte(k))
					Expect(err).NotTo(HaveOccurred())
				}

				// Remove b, d → remaining: a, c, e
				_, err := rs.Remove(tx, []byte("b"))
				Expect(err).NotTo(HaveOccurred())
				_, err = rs.Remove(tx, []byte("d"))
				Expect(err).NotTo(HaveOccurred())

				nth, err := rs.GetNth(tx, 0)
				Expect(err).NotTo(HaveOccurred())
				Expect(nth).To(Equal([]byte("a")))

				nth, err = rs.GetNth(tx, 1)
				Expect(err).NotTo(HaveOccurred())
				Expect(nth).To(Equal([]byte("c")))

				nth, err = rs.GetNth(tx, 2)
				Expect(err).NotTo(HaveOccurred())
				Expect(nth).To(Equal([]byte("e")))

				// Add f, then remove a → remaining: c, e, f
				_, err = rs.Add(tx, []byte("f"))
				Expect(err).NotTo(HaveOccurred())
				_, err = rs.Remove(tx, []byte("a"))
				Expect(err).NotTo(HaveOccurred())

				nth, err = rs.GetNth(tx, 0)
				Expect(err).NotTo(HaveOccurred())
				Expect(nth).To(Equal([]byte("c")))

				nth, err = rs.GetNth(tx, 2)
				Expect(err).NotTo(HaveOccurred())
				Expect(nth).To(Equal([]byte("f")))
			})
		})
	})

	Describe("Contains", func() {
		It("rejects empty key", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("contains-empty")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				_, err := rs.Contains(tx, []byte{})
				var emptyKeyErr *rankedSetEmptyKeyError
				Expect(errors.As(err, &emptyKeyErr)).To(BeTrue())
			})
		})

		It("returns false for absent key, true for present", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("contains-basic")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				has, err := rs.Contains(tx, []byte("nope"))
				Expect(err).NotTo(HaveOccurred())
				Expect(has).To(BeFalse())

				_, err = rs.Add(tx, []byte("yes"))
				Expect(err).NotTo(HaveOccurred())

				has, err = rs.Contains(tx, []byte("yes"))
				Expect(err).NotTo(HaveOccurred())
				Expect(has).To(BeTrue())
			})
		})

		It("returns false after key is removed", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("contains-after-rm")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				_, err := rs.Add(tx, []byte("gone"))
				Expect(err).NotTo(HaveOccurred())

				_, err = rs.Remove(tx, []byte("gone"))
				Expect(err).NotTo(HaveOccurred())

				has, err := rs.Contains(tx, []byte("gone"))
				Expect(err).NotTo(HaveOccurred())
				Expect(has).To(BeFalse())
			})
		})
	})

	Describe("Count", func() {
		It("rejects empty key", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("count-empty")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				_, err := rs.Count(tx, []byte{})
				var emptyKeyErr *rankedSetEmptyKeyError
				Expect(errors.As(err, &emptyKeyErr)).To(BeTrue())
			})
		})

		It("returns 0 for absent key", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("count-absent")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				c, err := rs.Count(tx, []byte("nope"))
				Expect(err).NotTo(HaveOccurred())
				Expect(c).To(Equal(int64(0)))
			})
		})

		It("returns 1 for present key without CountDuplicates", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("count-nodup")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				_, err := rs.Add(tx, []byte("k"))
				Expect(err).NotTo(HaveOccurred())

				// Add again (no effect)
				_, err = rs.Add(tx, []byte("k"))
				Expect(err).NotTo(HaveOccurred())

				c, err := rs.Count(tx, []byte("k"))
				Expect(err).NotTo(HaveOccurred())
				Expect(c).To(Equal(int64(1)))
			})
		})

		It("returns duplicate count with CountDuplicates", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("count-dup")
				rs := newRankedSet(sub, rankedSetConfig{
					NLevels:         rankedSetDefaultLevels,
					CountDuplicates: true,
				})
				Expect(rs.Init(tx)).To(Succeed())

				for i := 0; i < 7; i++ {
					_, err := rs.Add(tx, []byte("k"))
					Expect(err).NotTo(HaveOccurred())
				}

				c, err := rs.Count(tx, []byte("k"))
				Expect(err).NotTo(HaveOccurred())
				Expect(c).To(Equal(int64(7)))
			})
		})
	})

	Describe("Size", func() {
		It("returns 0 for empty set", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("size-empty")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				size, err := rs.Size(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(Equal(int64(0)))
			})
		})

		It("accounts for duplicates when CountDuplicates is on", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("size-dup")
				rs := newRankedSet(sub, rankedSetConfig{
					NLevels:         rankedSetDefaultLevels,
					CountDuplicates: true,
				})
				Expect(rs.Init(tx)).To(Succeed())

				// 3x "a", 2x "b"
				for i := 0; i < 3; i++ {
					_, err := rs.Add(tx, []byte("a"))
					Expect(err).NotTo(HaveOccurred())
				}
				for i := 0; i < 2; i++ {
					_, err := rs.Add(tx, []byte("b"))
					Expect(err).NotTo(HaveOccurred())
				}

				size, err := rs.Size(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(Equal(int64(5)))
			})
		})

		It("correct after add-remove-add cycle", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("size-cycle")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				for _, k := range []string{"a", "b", "c"} {
					_, err := rs.Add(tx, []byte(k))
					Expect(err).NotTo(HaveOccurred())
				}

				size, err := rs.Size(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(Equal(int64(3)))

				_, err = rs.Remove(tx, []byte("b"))
				Expect(err).NotTo(HaveOccurred())

				size, err = rs.Size(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(Equal(int64(2)))

				_, err = rs.Add(tx, []byte("d"))
				Expect(err).NotTo(HaveOccurred())

				size, err = rs.Size(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(Equal(int64(3)))
			})
		})
	})

	Describe("Clear", func() {
		It("resets everything and allows re-use", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("clear-reuse")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				for _, k := range []string{"a", "b", "c"} {
					_, err := rs.Add(tx, []byte(k))
					Expect(err).NotTo(HaveOccurred())
				}

				Expect(rs.Clear(tx)).To(Succeed())

				size, err := rs.Size(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(Equal(int64(0)))

				// InitNeeded should be false (Clear re-inits)
				needed, err := rs.InitNeeded(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(needed).To(BeFalse())

				// Can add elements again
				added, err := rs.Add(tx, []byte("new"))
				Expect(err).NotTo(HaveOccurred())
				Expect(added).To(BeTrue())

				size, err = rs.Size(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(Equal(int64(1)))
			})
		})
	})

	Describe("Hash functions", func() {
		It("jdkArrayHash matches known Java values", func() {
			// Java: Arrays.hashCode(new byte[]{}) = 1
			Expect(jdkArrayHash([]byte{})).To(Equal(int32(1)))

			// Java: Arrays.hashCode(new byte[]{0}) = 31
			Expect(jdkArrayHash([]byte{0})).To(Equal(int32(31)))

			// Java: Arrays.hashCode(new byte[]{1}) = 32
			Expect(jdkArrayHash([]byte{1})).To(Equal(int32(32)))

			// Java: Arrays.hashCode(new byte[]{1, 2}) = 31*32 + 2 = 994
			Expect(jdkArrayHash([]byte{1, 2})).To(Equal(int32(994)))

			// Java: Arrays.hashCode(new byte[]{(byte)0xFF}) = 31*1 + (-1) = 30
			// 0xFF as signed byte = -1
			Expect(jdkArrayHash([]byte{0xFF})).To(Equal(int32(30)))
		})

		It("crcHash produces consistent results", func() {
			// Same input -> same output
			h1 := crcHash([]byte("test"))
			h2 := crcHash([]byte("test"))
			Expect(h1).To(Equal(h2))

			// Different inputs -> different outputs (not guaranteed but very likely)
			h3 := crcHash([]byte("other"))
			Expect(h3).NotTo(Equal(h1))
		})

		It("JDK and CRC hashes produce different level distributions", func() {
			// Just verify both hash functions produce valid level decisions
			// by running the add path with each
			runInTx(func(tx fdb.WritableTransaction) {
				subJDK := specSubspace().Sub("hash-jdk")
				rsJDK := newRankedSet(subJDK, rankedSetConfig{
					HashFunction: jdkArrayHash,
					NLevels:      rankedSetDefaultLevels,
				})
				Expect(rsJDK.Init(tx)).To(Succeed())

				subCRC := specSubspace().Sub("hash-crc")
				rsCRC := newRankedSet(subCRC, rankedSetConfig{
					HashFunction: crcHash,
					NLevels:      rankedSetDefaultLevels,
				})
				Expect(rsCRC.Init(tx)).To(Succeed())

				for i := range 30 {
					key := []byte(fmt.Sprintf("key-%03d", i))
					_, err := rsJDK.Add(tx, key)
					Expect(err).NotTo(HaveOccurred())
					_, err = rsCRC.Add(tx, key)
					Expect(err).NotTo(HaveOccurred())
				}

				// Both should have 30 elements, and rank/getNth consistency
				for _, rs := range []*rankedSet{rsJDK, rsCRC} {
					size, err := rs.Size(tx)
					Expect(err).NotTo(HaveOccurred())
					Expect(size).To(Equal(int64(30)))

					for i := range 30 {
						nth, err := rs.GetNth(tx, int64(i))
						Expect(err).NotTo(HaveOccurred())
						Expect(nth).NotTo(BeNil(), "GetNth(%d)", i)

						rank, err := rs.Rank(tx, nth, false)
						Expect(err).NotTo(HaveOccurred())
						Expect(*rank).To(Equal(int64(i)), "Rank round-trip at index %d", i)
					}
				}
			})
		})
	})

	Describe("NLevels variations", func() {
		It("works with minimum NLevels=2", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("levels-2")
				rs := newRankedSet(sub, rankedSetConfig{NLevels: 2})
				Expect(rs.Init(tx)).To(Succeed())

				for i := range 20 {
					key := tuple.Tuple{int64(i)}.Pack()
					_, err := rs.Add(tx, key)
					Expect(err).NotTo(HaveOccurred())
				}

				size, err := rs.Size(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(Equal(int64(20)))

				// Verify rank/getNth consistency
				for i := range 20 {
					expected := tuple.Tuple{int64(i)}.Pack()
					nth, err := rs.GetNth(tx, int64(i))
					Expect(err).NotTo(HaveOccurred())
					Expect(nth).To(Equal(expected), "GetNth(%d) with 2 levels", i)
				}
			})
		})

		It("works with maximum NLevels=8", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("levels-8")
				rs := newRankedSet(sub, rankedSetConfig{NLevels: 8})
				Expect(rs.Init(tx)).To(Succeed())

				for i := range 20 {
					key := tuple.Tuple{int64(i)}.Pack()
					_, err := rs.Add(tx, key)
					Expect(err).NotTo(HaveOccurred())
				}

				size, err := rs.Size(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(Equal(int64(20)))

				for i := range 20 {
					expected := tuple.Tuple{int64(i)}.Pack()
					nth, err := rs.GetNth(tx, int64(i))
					Expect(err).NotTo(HaveOccurred())
					Expect(nth).To(Equal(expected), "GetNth(%d) with 8 levels", i)
				}
			})
		})
	})

	Describe("rsEncodeLong and rsDecodeLong", func() {
		It("round-trips zero", func() {
			encoded := rsEncodeLong(0)
			Expect(encoded).To(HaveLen(8))
			val, err := rsDecodeLong(encoded)
			Expect(err).NotTo(HaveOccurred())
			Expect(val).To(Equal(int64(0)))
		})

		It("round-trips positive values", func() {
			for _, v := range []int64{1, 127, 128, 255, 256, 65535, 1<<31 - 1, 1<<63 - 1} {
				encoded := rsEncodeLong(v)
				val, err := rsDecodeLong(encoded)
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(Equal(v), "round-trip of %d", v)
			}
		})

		It("round-trips negative values", func() {
			for _, v := range []int64{-1, -128, -256, -1 << 31, -1 << 63} {
				encoded := rsEncodeLong(v)
				val, err := rsDecodeLong(encoded)
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(Equal(v), "round-trip of %d", v)
			}
		})

		It("returns 0 for nil input", func() {
			val, err := rsDecodeLong(nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(val).To(Equal(int64(0)))
		})

		It("returns error for short non-nil input", func() {
			_, err := rsDecodeLong([]byte{1})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("corrupted count value"))

			_, err = rsDecodeLong([]byte{1, 2, 3, 4, 5, 6, 7})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("expected 8 bytes, got 7"))
		})

		It("is little-endian (matches Java encodeLong)", func() {
			// 1 as little-endian int64
			encoded := rsEncodeLong(1)
			Expect(encoded).To(Equal([]byte{1, 0, 0, 0, 0, 0, 0, 0}))

			// 256 as little-endian int64
			encoded = rsEncodeLong(256)
			Expect(encoded).To(Equal([]byte{0, 1, 0, 0, 0, 0, 0, 0}))
		})
	})

	Describe("stress: add-remove-verify cycle", func() {
		It("maintains consistency through 200 operations", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("stress")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				// Track what we expect to be in the set
				present := make(map[string]bool)

				// Phase 1: add 50 elements
				for i := range 50 {
					key := fmt.Sprintf("key-%04d", i)
					_, err := rs.Add(tx, []byte(key))
					Expect(err).NotTo(HaveOccurred())
					present[key] = true
				}

				// Phase 2: remove every other element
				for i := 0; i < 50; i += 2 {
					key := fmt.Sprintf("key-%04d", i)
					removed, err := rs.Remove(tx, []byte(key))
					Expect(err).NotTo(HaveOccurred())
					Expect(removed).To(BeTrue())
					delete(present, key)
				}

				// Phase 3: add 25 more
				for i := 50; i < 75; i++ {
					key := fmt.Sprintf("key-%04d", i)
					_, err := rs.Add(tx, []byte(key))
					Expect(err).NotTo(HaveOccurred())
					present[key] = true
				}

				// Verify size
				size, err := rs.Size(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(Equal(int64(len(present))))

				// Build sorted list of expected keys
				expected := make([]string, 0, len(present))
				for k := range present {
					expected = append(expected, k)
				}
				sort.Strings(expected)

				// Verify every rank
				for i, k := range expected {
					nth, err := rs.GetNth(tx, int64(i))
					Expect(err).NotTo(HaveOccurred())
					Expect(nth).To(Equal([]byte(k)), "GetNth(%d)", i)

					rank, err := rs.Rank(tx, []byte(k), false)
					Expect(err).NotTo(HaveOccurred())
					Expect(*rank).To(Equal(int64(i)), "Rank(%q)", k)
				}

				// Verify contains
				for _, k := range expected {
					has, err := rs.Contains(tx, []byte(k))
					Expect(err).NotTo(HaveOccurred())
					Expect(has).To(BeTrue(), "Contains(%q)", k)
				}

				// Verify removed keys are not present
				for i := 0; i < 50; i += 2 {
					key := fmt.Sprintf("key-%04d", i)
					has, err := rs.Contains(tx, []byte(key))
					Expect(err).NotTo(HaveOccurred())
					Expect(has).To(BeFalse(), "should not contain removed %q", key)
				}
			})
		})
	})

	Describe("stress: CountDuplicates with many operations", func() {
		It("maintains correct size and ranks through dup adds and removes", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("stress-dup")
				rs := newRankedSet(sub, rankedSetConfig{
					NLevels:         rankedSetDefaultLevels,
					CountDuplicates: true,
				})
				Expect(rs.Init(tx)).To(Succeed())

				// Add "a" x10, "b" x5, "c" x3
				for i := 0; i < 10; i++ {
					_, err := rs.Add(tx, []byte("a"))
					Expect(err).NotTo(HaveOccurred())
				}
				for i := 0; i < 5; i++ {
					_, err := rs.Add(tx, []byte("b"))
					Expect(err).NotTo(HaveOccurred())
				}
				for i := 0; i < 3; i++ {
					_, err := rs.Add(tx, []byte("c"))
					Expect(err).NotTo(HaveOccurred())
				}

				// Size = 18
				size, err := rs.Size(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(Equal(int64(18)))

				// Rank of "b" = 10 (10 "a"s before it)
				rank, err := rs.Rank(tx, []byte("b"), false)
				Expect(err).NotTo(HaveOccurred())
				Expect(*rank).To(Equal(int64(10)))

				// GetNth(9) = "a" (last "a"), GetNth(10) = "b" (first "b")
				nth, err := rs.GetNth(tx, 9)
				Expect(err).NotTo(HaveOccurred())
				Expect(nth).To(Equal([]byte("a")))

				nth, err = rs.GetNth(tx, 10)
				Expect(err).NotTo(HaveOccurred())
				Expect(nth).To(Equal([]byte("b")))

				// Remove 7 "a"s
				for i := 0; i < 7; i++ {
					_, err := rs.Remove(tx, []byte("a"))
					Expect(err).NotTo(HaveOccurred())
				}

				// Now: a=3, b=5, c=3 → size=11
				size, err = rs.Size(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(Equal(int64(11)))

				count, err := rs.Count(tx, []byte("a"))
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(3)))

				// Rank of "b" = 3
				rank, err = rs.Rank(tx, []byte("b"), false)
				Expect(err).NotTo(HaveOccurred())
				Expect(*rank).To(Equal(int64(3)))
			})
		})
	})

	Describe("cross-transaction persistence", func() {
		It("data persists across separate transactions", func() {
			ks := specSubspace()

			// Transaction 1: add elements
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				sub := ks.Sub("persist")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				tx := rtx.Transaction()
				Expect(rs.Init(tx)).To(Succeed())

				for _, k := range []string{"x", "y", "z"} {
					_, err := rs.Add(tx, []byte(k))
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Transaction 2: read back
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				sub := ks.Sub("persist")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				tx := rtx.Transaction()

				needed, err := rs.InitNeeded(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(needed).To(BeFalse())

				size, err := rs.Size(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(Equal(int64(3)))

				nth, err := rs.GetNth(tx, 0)
				Expect(err).NotTo(HaveOccurred())
				Expect(nth).To(Equal([]byte("x")))

				nth, err = rs.GetNth(tx, 2)
				Expect(err).NotTo(HaveOccurred())
				Expect(nth).To(Equal([]byte("z")))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Transaction 3: remove and verify
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				sub := ks.Sub("persist")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				tx := rtx.Transaction()

				removed, err := rs.Remove(tx, []byte("y"))
				Expect(err).NotTo(HaveOccurred())
				Expect(removed).To(BeTrue())

				size, err := rs.Size(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(Equal(int64(2)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("tuple-packed keys (real index usage)", func() {
		It("handles negative, zero, and large int64 tuple keys", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("tuple-int")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				values := []int64{-1000, -1, 0, 1, 42, 999, 1 << 50}
				for _, v := range values {
					key := tuple.Tuple{v}.Pack()
					_, err := rs.Add(tx, key)
					Expect(err).NotTo(HaveOccurred())
				}

				size, err := rs.Size(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(Equal(int64(len(values))))

				// Verify sorted order matches tuple byte order
				sort.Slice(values, func(i, j int) bool {
					ki := tuple.Tuple{values[i]}.Pack()
					kj := tuple.Tuple{values[j]}.Pack()
					return bytes.Compare(ki, kj) < 0
				})

				for i, v := range values {
					expected := tuple.Tuple{v}.Pack()
					nth, err := rs.GetNth(tx, int64(i))
					Expect(err).NotTo(HaveOccurred())
					Expect(nth).To(Equal(expected), "GetNth(%d) for value %d", i, v)
				}
			})
		})

		It("handles string tuple keys", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("tuple-str")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				strings := []string{"banana", "apple", "cherry", "date"}
				for _, s := range strings {
					key := tuple.Tuple{s}.Pack()
					_, err := rs.Add(tx, key)
					Expect(err).NotTo(HaveOccurred())
				}

				size, err := rs.Size(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(Equal(int64(4)))

				// Tuple byte order for strings is lexicographic
				sort.Strings(strings)
				for i, s := range strings {
					expected := tuple.Tuple{s}.Pack()
					nth, err := rs.GetNth(tx, int64(i))
					Expect(err).NotTo(HaveOccurred())
					Expect(nth).To(Equal(expected), "GetNth(%d) for %q", i, s)
				}
			})
		})

		It("handles composite tuple keys (group + score)", func() {
			runInTx(func(tx fdb.WritableTransaction) {
				sub := specSubspace().Sub("tuple-composite")
				rs := newRankedSet(sub, defaultRankedSetConfig)
				Expect(rs.Init(tx)).To(Succeed())

				type entry struct {
					group string
					score int64
				}
				entries := []entry{
					{"groupA", 300},
					{"groupA", 100},
					{"groupA", 200},
					{"groupB", 50},
				}
				for _, e := range entries {
					key := tuple.Tuple{e.group, e.score}.Pack()
					_, err := rs.Add(tx, key)
					Expect(err).NotTo(HaveOccurred())
				}

				size, err := rs.Size(tx)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).To(Equal(int64(4)))

				// Verify ordering: groupA sorts before groupB,
				// then by score within group
				sort.Slice(entries, func(i, j int) bool {
					ki := tuple.Tuple{entries[i].group, entries[i].score}.Pack()
					kj := tuple.Tuple{entries[j].group, entries[j].score}.Pack()
					return bytes.Compare(ki, kj) < 0
				})

				for i, e := range entries {
					expected := tuple.Tuple{e.group, e.score}.Pack()
					nth, err := rs.GetNth(tx, int64(i))
					Expect(err).NotTo(HaveOccurred())
					Expect(nth).To(Equal(expected), "GetNth(%d) for %v/%d", i, e.group, e.score)
				}
			})
		})
	})
})
