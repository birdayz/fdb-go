package recordlayer

import (
	"bytes"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// makeTestBytes creates a byte slice of the given size filled with a repeating pattern.
func makeTestBytes(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 251) // prime modulus avoids alignment artifacts
	}
	return data
}

var _ = Describe("SplitHelper", func() {
	// Each test gets its own subspace from specSubspace(), and within that
	// we use sub(RecordKey) as the recordSubspace — mirroring the real store layout.
	recordSub := func() subspace.Subspace {
		return specSubspace().Sub(RecordKey)
	}

	// ───────────────────────────────────────────────────────────────────
	// appendToTuple
	// ───────────────────────────────────────────────────────────────────

	Describe("appendToTuple", func() {
		It("appends to a single-element tuple", func() {
			base := tuple.Tuple{int64(42)}
			result := appendToTuple(base, 0)
			Expect(result).To(Equal(tuple.Tuple{int64(42), int64(0)}))
		})

		It("appends to a multi-element tuple", func() {
			base := tuple.Tuple{"hello", int64(1), int64(2)}
			result := appendToTuple(base, 99)
			Expect(result).To(Equal(tuple.Tuple{"hello", int64(1), int64(2), int64(99)}))
		})

		It("does not modify the original tuple (no aliasing)", func() {
			base := tuple.Tuple{int64(10), int64(20)}
			original := make(tuple.Tuple, len(base))
			copy(original, base)

			_ = appendToTuple(base, 30)

			// base must be unchanged
			Expect(base).To(Equal(original))
		})

		It("works with an empty base tuple", func() {
			base := tuple.Tuple{}
			result := appendToTuple(base, 7)
			Expect(result).To(Equal(tuple.Tuple{int64(7)}))
		})
	})

	// ───────────────────────────────────────────────────────────────────
	// saveWithSplit
	// ───────────────────────────────────────────────────────────────────

	Describe("saveWithSplit", func() {
		It("returns error for empty primary key", func() {
			rs := recordSub()
			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				si := &sizeInfo{}
				err := saveWithSplit(tx, rs, tuple.Tuple{}, []byte("data"), true, false, nil, si)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("primary key must not be empty"))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("saves a small record unsplit at suffix 0", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(1)}
			data := makeTestBytes(500)

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				si := &sizeInfo{}
				err := saveWithSplit(tx, rs, pk, data, true, false, nil, si)
				Expect(err).NotTo(HaveOccurred())

				Expect(si.IsSplit).To(BeFalse())
				Expect(si.KeyCount).To(Equal(1))
				Expect(si.ValueSize).To(Equal(500))

				// Verify the key is at suffix 0
				key := rs.Pack(appendToTuple(pk, unsplitRecord))
				val, err := tx.Get(fdb.Key(key)).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(Equal(data))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("does NOT split a record exactly at splitRecordSize", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(2)}
			data := makeTestBytes(splitRecordSize) // exactly 100_000 bytes

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				si := &sizeInfo{}
				err := saveWithSplit(tx, rs, pk, data, true, false, nil, si)
				Expect(err).NotTo(HaveOccurred())

				// <= splitRecordSize → unsplit
				Expect(si.IsSplit).To(BeFalse())
				Expect(si.KeyCount).To(Equal(1))
				Expect(si.ValueSize).To(Equal(splitRecordSize))

				// Verify at suffix 0
				val, err := tx.Get(fdb.Key(rs.Pack(appendToTuple(pk, unsplitRecord)))).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(len(val)).To(Equal(splitRecordSize))

				// No split chunk at suffix 1
				val, err = tx.Get(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord)))).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(BeNil())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("splits a record at splitRecordSize+1 into 2 chunks", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(3)}
			data := makeTestBytes(splitRecordSize + 1) // 100_001 bytes

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				si := &sizeInfo{}
				err := saveWithSplit(tx, rs, pk, data, true, false, nil, si)
				Expect(err).NotTo(HaveOccurred())

				Expect(si.IsSplit).To(BeTrue())
				Expect(si.KeyCount).To(Equal(2))
				Expect(si.ValueSize).To(Equal(splitRecordSize + 1))

				// Chunk 1 at suffix 1: full 100KB
				chunk1, err := tx.Get(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord)))).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(len(chunk1)).To(Equal(splitRecordSize))

				// Chunk 2 at suffix 2: 1 byte
				chunk2, err := tx.Get(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord+1)))).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(len(chunk2)).To(Equal(1))

				// No unsplit key at suffix 0
				unsplit, err := tx.Get(fdb.Key(rs.Pack(appendToTuple(pk, unsplitRecord)))).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(unsplit).To(BeNil())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("splits a record at 2*splitRecordSize into exactly 2 full chunks", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(4)}
			data := makeTestBytes(2 * splitRecordSize) // 200_000 bytes

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				si := &sizeInfo{}
				err := saveWithSplit(tx, rs, pk, data, true, false, nil, si)
				Expect(err).NotTo(HaveOccurred())

				Expect(si.IsSplit).To(BeTrue())
				Expect(si.KeyCount).To(Equal(2))
				Expect(si.ValueSize).To(Equal(2 * splitRecordSize))

				chunk1, err := tx.Get(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord)))).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(len(chunk1)).To(Equal(splitRecordSize))

				chunk2, err := tx.Get(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord+1)))).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(len(chunk2)).To(Equal(splitRecordSize))

				// No chunk 3
				chunk3, err := tx.Get(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord+2)))).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(chunk3).To(BeNil())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("splits 3*splitRecordSize+1 into 4 chunks (3 full + 1 partial)", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(5)}
			data := makeTestBytes(3*splitRecordSize + 1)

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				si := &sizeInfo{}
				err := saveWithSplit(tx, rs, pk, data, true, false, nil, si)
				Expect(err).NotTo(HaveOccurred())

				Expect(si.IsSplit).To(BeTrue())
				Expect(si.KeyCount).To(Equal(4))
				Expect(si.ValueSize).To(Equal(3*splitRecordSize + 1))

				for i := int64(0); i < 3; i++ {
					chunk, err := tx.Get(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord+i)))).Get()
					Expect(err).NotTo(HaveOccurred())
					Expect(len(chunk)).To(Equal(splitRecordSize))
				}

				// 4th chunk: 1 byte
				lastChunk, err := tx.Get(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord+3)))).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(len(lastChunk)).To(Equal(1))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns error when splitLongRecords=false and record exceeds limit", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(6)}
			data := makeTestBytes(splitRecordSize + 1)

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				si := &sizeInfo{}
				err := saveWithSplit(tx, rs, pk, data, false, false, nil, si)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("exceeds limit"))
				Expect(err.Error()).To(ContainSubstring("splitLongRecords is not enabled"))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("tracks sizeInfo.KeySize correctly for unsplit records", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(7)}
			data := makeTestBytes(42)

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				si := &sizeInfo{}
				err := saveWithSplit(tx, rs, pk, data, false, false, nil, si)
				Expect(err).NotTo(HaveOccurred())

				expectedKey := rs.Pack(appendToTuple(pk, unsplitRecord))
				Expect(si.KeySize).To(Equal(len(expectedKey)))
				Expect(si.KeyCount).To(Equal(1))
				Expect(si.ValueSize).To(Equal(42))
				Expect(si.IsSplit).To(BeFalse())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("tracks sizeInfo.KeySize correctly for split records", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(8)}
			data := makeTestBytes(splitRecordSize + 50)

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				si := &sizeInfo{}
				err := saveWithSplit(tx, rs, pk, data, true, false, nil, si)
				Expect(err).NotTo(HaveOccurred())

				Expect(si.KeyCount).To(Equal(2))
				Expect(si.IsSplit).To(BeTrue())

				// KeySize = sum of both packed key lengths
				key1 := rs.Pack(appendToTuple(pk, startSplitRecord))
				key2 := rs.Pack(appendToTuple(pk, startSplitRecord+1))
				Expect(si.KeySize).To(Equal(len(key1) + len(key2)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// ───────────────────────────────────────────────────────────────────
	// loadWithSplit
	// ───────────────────────────────────────────────────────────────────

	Describe("loadWithSplit", func() {
		It("returns nil for a non-existent record (split mode)", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(100)}

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				si := &sizeInfo{}
				data, err := loadWithSplit(tx, rs, pk, true, false, si)
				Expect(err).NotTo(HaveOccurred())
				Expect(data).To(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns nil for a non-existent record (non-split mode)", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(101)}

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				si := &sizeInfo{}
				data, err := loadWithSplit(tx, rs, pk, false, false, si)
				Expect(err).NotTo(HaveOccurred())
				Expect(data).To(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("loads an unsplit record at suffix 0", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(102)}
			original := makeTestBytes(500)

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				// Write raw unsplit data
				key := rs.Pack(appendToTuple(pk, unsplitRecord))
				tx.Set(fdb.Key(key), original)

				si := &sizeInfo{}
				loaded, err := loadWithSplit(tx, rs, pk, true, false, si)
				Expect(err).NotTo(HaveOccurred())
				Expect(loaded).To(Equal(original))

				Expect(si.IsSplit).To(BeFalse())
				Expect(si.KeyCount).To(Equal(1))
				Expect(si.KeySize).To(Equal(len(key)))
				Expect(si.ValueSize).To(Equal(500))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reassembles a split record from multiple chunks", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(103)}
			original := makeTestBytes(splitRecordSize*2 + 77)

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				// Write chunks manually
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord))),
					original[0:splitRecordSize])
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord+1))),
					original[splitRecordSize:2*splitRecordSize])
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord+2))),
					original[2*splitRecordSize:])

				si := &sizeInfo{}
				loaded, err := loadWithSplit(tx, rs, pk, true, false, si)
				Expect(err).NotTo(HaveOccurred())
				Expect(bytes.Equal(loaded, original)).To(BeTrue())

				Expect(si.IsSplit).To(BeTrue())
				Expect(si.KeyCount).To(Equal(3))
				Expect(si.ValueSize).To(Equal(len(original)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("does not find a split record when splitLongRecords=false", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(104)}

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				// Write only at split suffix (no unsplit key)
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord))),
					makeTestBytes(splitRecordSize))

				si := &sizeInfo{}
				loaded, err := loadWithSplit(tx, rs, pk, false, false, si)
				Expect(err).NotTo(HaveOccurred())
				Expect(loaded).To(BeNil())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("round-trips through saveWithSplit and loadWithSplit", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(105)}
			original := makeTestBytes(splitRecordSize*3 + 42)

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				saveSI := &sizeInfo{}
				err := saveWithSplit(tx, rs, pk, original, true, false, nil, saveSI)
				Expect(err).NotTo(HaveOccurred())
				Expect(saveSI.IsSplit).To(BeTrue())
				Expect(saveSI.KeyCount).To(Equal(4))

				loadSI := &sizeInfo{}
				loaded, err := loadWithSplit(tx, rs, pk, true, false, loadSI)
				Expect(err).NotTo(HaveOccurred())
				Expect(bytes.Equal(loaded, original)).To(BeTrue())

				Expect(loadSI.IsSplit).To(BeTrue())
				Expect(loadSI.KeyCount).To(Equal(4))
				Expect(loadSI.ValueSize).To(Equal(saveSI.ValueSize))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("skips version key at suffix -1 during split reassembly", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(106)}
			original := makeTestBytes(splitRecordSize + 10)

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				// Write split chunks
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord))),
					original[0:splitRecordSize])
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord+1))),
					original[splitRecordSize:])

				// Also write a version key at suffix -1 (should be ignored by loadWithSplit)
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, recordVersionSuffix))),
					[]byte("fake-version"))

				si := &sizeInfo{}
				loaded, err := loadWithSplit(tx, rs, pk, true, false, si)
				Expect(err).NotTo(HaveOccurred())
				Expect(bytes.Equal(loaded, original)).To(BeTrue())
				Expect(si.KeyCount).To(Equal(2)) // version key not counted

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// ───────────────────────────────────────────────────────────────────
	// deleteSplit
	// ───────────────────────────────────────────────────────────────────

	Describe("deleteSplit", func() {
		It("returns false for empty primary key", func() {
			rs := recordSub()
			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				result := deleteSplit(tx, rs, tuple.Tuple{}, true, false, &sizeInfo{})
				Expect(result).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns false for nil oldsizeInfo", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(200)}
			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				result := deleteSplit(tx, rs, pk, true, false, nil)
				Expect(result).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("deletes an unsplit record", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(201)}
			data := makeTestBytes(500)

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				// Write unsplit
				key := rs.Pack(appendToTuple(pk, unsplitRecord))
				tx.Set(fdb.Key(key), data)

				// Delete with IsSplit=false, splitLongRecords=false
				oldSI := &sizeInfo{KeyCount: 1, IsSplit: false}
				result := deleteSplit(tx, rs, pk, false, false, oldSI)
				Expect(result).To(BeTrue())

				// Verify gone
				val, err := tx.Get(fdb.Key(key)).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(BeNil())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("deletes a split record (all chunks cleared)", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(202)}

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				// Write 3 split chunks
				for i := int64(0); i < 3; i++ {
					tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord+i))),
						makeTestBytes(splitRecordSize))
				}

				oldSI := &sizeInfo{KeyCount: 3, IsSplit: true}
				result := deleteSplit(tx, rs, pk, true, false, oldSI)
				Expect(result).To(BeTrue())

				// All chunks should be gone
				for i := int64(0); i < 3; i++ {
					val, err := tx.Get(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord+i)))).Get()
					Expect(err).NotTo(HaveOccurred())
					Expect(val).To(BeNil())
				}

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("clears version key when VersionedInline is true", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(203)}

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				// Write unsplit record + version key
				unsplitKey := rs.Pack(appendToTuple(pk, unsplitRecord))
				versionKey := rs.Pack(appendToTuple(pk, recordVersionSuffix))
				tx.Set(fdb.Key(unsplitKey), []byte("record-data"))
				tx.Set(fdb.Key(versionKey), []byte("version-data"))

				oldSI := &sizeInfo{
					KeyCount:        1,
					IsSplit:         false,
					VersionedInline: true,
				}
				// splitLongRecords=false so it takes the unsplit path
				result := deleteSplit(tx, rs, pk, false, false, oldSI)
				Expect(result).To(BeTrue())

				// Both keys cleared
				val, err := tx.Get(fdb.Key(unsplitKey)).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(BeNil())

				val, err = tx.Get(fdb.Key(versionKey)).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(BeNil())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("does NOT clear version key when VersionedInline is false and splitLongRecords is false", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(204)}

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				unsplitKey := rs.Pack(appendToTuple(pk, unsplitRecord))
				versionKey := rs.Pack(appendToTuple(pk, recordVersionSuffix))
				tx.Set(fdb.Key(unsplitKey), []byte("record"))
				tx.Set(fdb.Key(versionKey), []byte("version"))

				oldSI := &sizeInfo{
					KeyCount:        1,
					IsSplit:         false,
					VersionedInline: false,
				}
				result := deleteSplit(tx, rs, pk, false, false, oldSI)
				Expect(result).To(BeTrue())

				// Record gone
				val, err := tx.Get(fdb.Key(unsplitKey)).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(BeNil())

				// Version key still there (not cleared)
				val, err = tx.Get(fdb.Key(versionKey)).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(val).NotTo(BeNil())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// ───────────────────────────────────────────────────────────────────
	// recordExistsWithSplit
	// ───────────────────────────────────────────────────────────────────

	Describe("recordExistsWithSplit", func() {
		It("detects an unsplit record", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(300)}

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, unsplitRecord))), []byte("data"))

				exists, err := recordExistsWithSplit(tx, rs, pk, true, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeTrue())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("detects a split record via first chunk", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(301)}

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				// Only split chunks, no unsplit key
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord))), []byte("chunk1"))
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord+1))), []byte("chunk2"))

				exists, err := recordExistsWithSplit(tx, rs, pk, true, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeTrue())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns false for a non-existent record", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(302)}

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				exists, err := recordExistsWithSplit(tx, rs, pk, true, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeFalse())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("only checks unsplit when splitLongRecords=false", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(303)}

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				// Write only at split suffix — unsplit check won't find it
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord))), []byte("chunk"))

				exists, err := recordExistsWithSplit(tx, rs, pk, false, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeFalse())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("detects unsplit when splitLongRecords=false", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(304)}

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, unsplitRecord))), []byte("data"))

				exists, err := recordExistsWithSplit(tx, rs, pk, false, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeTrue())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// ───────────────────────────────────────────────────────────────────
	// clearPreviousRecord
	// ───────────────────────────────────────────────────────────────────

	Describe("clearPreviousRecord", func() {
		It("is a no-op when oldsizeInfo is nil", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(400)}

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				// Write some data that should NOT be cleared
				key := rs.Pack(appendToTuple(pk, unsplitRecord))
				tx.Set(fdb.Key(key), []byte("keep-me"))

				clearPreviousRecord(tx, rs, pk, true, nil)

				val, err := tx.Get(fdb.Key(key)).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(Equal([]byte("keep-me")))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("range-clears when oldsizeInfo.IsSplit is true", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(401)}

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				// Write split chunks + version key
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, recordVersionSuffix))), []byte("ver"))
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord))), []byte("c1"))
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord+1))), []byte("c2"))
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord+2))), []byte("c3"))

				oldSI := &sizeInfo{IsSplit: true}
				clearPreviousRecord(tx, rs, pk, true, oldSI)

				// All keys should be gone
				for _, suffix := range []int64{recordVersionSuffix, startSplitRecord, startSplitRecord + 1, startSplitRecord + 2} {
					val, err := tx.Get(fdb.Key(rs.Pack(appendToTuple(pk, suffix)))).Get()
					Expect(err).NotTo(HaveOccurred())
					Expect(val).To(BeNil())
				}

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("range-clears when splitLongRecords is true even if oldsizeInfo.IsSplit is false", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(402)}

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, unsplitRecord))), []byte("data"))
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, recordVersionSuffix))), []byte("ver"))

				// splitLongRecords=true triggers range clear even with IsSplit=false
				oldSI := &sizeInfo{IsSplit: false}
				clearPreviousRecord(tx, rs, pk, true, oldSI)

				val, err := tx.Get(fdb.Key(rs.Pack(appendToTuple(pk, unsplitRecord)))).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(BeNil())

				val, err = tx.Get(fdb.Key(rs.Pack(appendToTuple(pk, recordVersionSuffix)))).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(BeNil())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("clears only suffix 0 when unsplit without version", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(403)}

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				unsplitKey := rs.Pack(appendToTuple(pk, unsplitRecord))
				versionKey := rs.Pack(appendToTuple(pk, recordVersionSuffix))
				tx.Set(fdb.Key(unsplitKey), []byte("record"))
				tx.Set(fdb.Key(versionKey), []byte("version"))

				// splitLongRecords=false, IsSplit=false, VersionedInline=false
				oldSI := &sizeInfo{IsSplit: false, VersionedInline: false}
				clearPreviousRecord(tx, rs, pk, false, oldSI)

				// Unsplit key cleared
				val, err := tx.Get(fdb.Key(unsplitKey)).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(BeNil())

				// Version key NOT cleared (VersionedInline is false)
				val, err = tx.Get(fdb.Key(versionKey)).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(Equal([]byte("version")))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("clears suffix 0 and -1 when unsplit with VersionedInline", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(404)}

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				unsplitKey := rs.Pack(appendToTuple(pk, unsplitRecord))
				versionKey := rs.Pack(appendToTuple(pk, recordVersionSuffix))
				tx.Set(fdb.Key(unsplitKey), []byte("record"))
				tx.Set(fdb.Key(versionKey), []byte("version"))

				oldSI := &sizeInfo{IsSplit: false, VersionedInline: true}
				clearPreviousRecord(tx, rs, pk, false, oldSI)

				// Both cleared
				val, err := tx.Get(fdb.Key(unsplitKey)).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(BeNil())

				val, err = tx.Get(fdb.Key(versionKey)).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(BeNil())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// ───────────────────────────────────────────────────────────────────
	// clearRecordKeyRange
	// ───────────────────────────────────────────────────────────────────

	Describe("clearRecordKeyRange", func() {
		It("is a no-op for empty primary key (safety guard)", func() {
			rs := recordSub()

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				// Write something at a known PK
				pk := tuple.Tuple{int64(500)}
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, unsplitRecord))), []byte("safe"))

				// Call with empty PK — must not nuke everything
				clearRecordKeyRange(tx, rs, tuple.Tuple{})

				val, err := tx.Get(fdb.Key(rs.Pack(appendToTuple(pk, unsplitRecord)))).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(Equal([]byte("safe")))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("clears all suffixes for a given primary key", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(501)}

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				// Write at multiple suffixes
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, recordVersionSuffix))), []byte("ver"))
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, unsplitRecord))), []byte("unsplit"))
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord))), []byte("s1"))
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord+1))), []byte("s2"))

				clearRecordKeyRange(tx, rs, pk)

				for _, suffix := range []int64{recordVersionSuffix, unsplitRecord, startSplitRecord, startSplitRecord + 1} {
					val, err := tx.Get(fdb.Key(rs.Pack(appendToTuple(pk, suffix)))).Get()
					Expect(err).NotTo(HaveOccurred())
					Expect(val).To(BeNil())
				}

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("does not affect other primary keys", func() {
			rs := recordSub()
			pk1 := tuple.Tuple{int64(502)}
			pk2 := tuple.Tuple{int64(503)}

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk1, unsplitRecord))), []byte("pk1"))
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk2, unsplitRecord))), []byte("pk2"))

				clearRecordKeyRange(tx, rs, pk1)

				// pk1 gone
				val, err := tx.Get(fdb.Key(rs.Pack(appendToTuple(pk1, unsplitRecord)))).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(BeNil())

				// pk2 still there
				val, err = tx.Get(fdb.Key(rs.Pack(appendToTuple(pk2, unsplitRecord)))).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(Equal([]byte("pk2")))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// ───────────────────────────────────────────────────────────────────
	// saveWithSplit + clearPreviousRecord integration
	// ───────────────────────────────────────────────────────────────────

	Describe("saveWithSplit overwrite (clearPreviousRecord integration)", func() {
		It("clears old split chunks when overwriting with a smaller record", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(600)}
			bigData := makeTestBytes(3 * splitRecordSize) // 3 chunks
			smallData := makeTestBytes(500)               // unsplit

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				// First save: 3 chunks
				saveSI := &sizeInfo{}
				err := saveWithSplit(tx, rs, pk, bigData, true, false, nil, saveSI)
				Expect(err).NotTo(HaveOccurred())
				Expect(saveSI.IsSplit).To(BeTrue())
				Expect(saveSI.KeyCount).To(Equal(3))

				// Second save: overwrite with small data, passing old sizeInfo
				overwriteSI := &sizeInfo{}
				err = saveWithSplit(tx, rs, pk, smallData, true, false, saveSI, overwriteSI)
				Expect(err).NotTo(HaveOccurred())
				Expect(overwriteSI.IsSplit).To(BeFalse())
				Expect(overwriteSI.KeyCount).To(Equal(1))

				// Verify old split chunks are gone
				for i := int64(0); i < 3; i++ {
					val, err := tx.Get(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord+i)))).Get()
					Expect(err).NotTo(HaveOccurred())
					Expect(val).To(BeNil())
				}

				// New unsplit data is there
				val, err := tx.Get(fdb.Key(rs.Pack(appendToTuple(pk, unsplitRecord)))).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(Equal(smallData))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("clears old unsplit when overwriting with a split record", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(601)}
			smallData := makeTestBytes(500)
			bigData := makeTestBytes(splitRecordSize + 50)

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				// First save: unsplit
				saveSI := &sizeInfo{}
				err := saveWithSplit(tx, rs, pk, smallData, true, false, nil, saveSI)
				Expect(err).NotTo(HaveOccurred())
				Expect(saveSI.IsSplit).To(BeFalse())

				// Second save: split, passing old sizeInfo
				overwriteSI := &sizeInfo{}
				err = saveWithSplit(tx, rs, pk, bigData, true, false, saveSI, overwriteSI)
				Expect(err).NotTo(HaveOccurred())
				Expect(overwriteSI.IsSplit).To(BeTrue())

				// Old unsplit key should be gone (range clear covers it)
				// But the new split is at suffix 1,2 — no data at suffix 0
				unsplitVal, err := tx.Get(fdb.Key(rs.Pack(appendToTuple(pk, unsplitRecord)))).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(unsplitVal).To(BeNil())

				// Load should work
				loadSI := &sizeInfo{}
				loaded, err := loadWithSplit(tx, rs, pk, true, false, loadSI)
				Expect(err).NotTo(HaveOccurred())
				Expect(bytes.Equal(loaded, bigData)).To(BeTrue())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
