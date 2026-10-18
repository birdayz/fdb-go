package recordlayer

import (
	"bytes"
	"context"
	"errors"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
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
		It("commits versionstamped split keys in local order and measures offset bytes", func() {
			rs := recordSub()
			for _, dataLen := range []int{0, 1, splitRecordSize, splitRecordSize + 1, 2*splitRecordSize + 17} {
				ss := rs.Sub(dataLen)
				data := makeTestBytes(dataLen)
				_, err := sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
					for local := range 2 {
						pk := tuple.Tuple{int64(7), tuple.IncompleteVersionstamp(uint16(rtx.ClaimLocalVersion()))}
						si := &sizeInfo{KeyCount: 99, KeySize: 99, ValueSize: 99, IsSplit: true, VersionedInline: true}
						if err := saveWithSplit(rtx, rtx.Transaction(), ss, pk, data, true, false, nil, si); err != nil {
							return nil, err
						}
						chunks := max(1, (dataLen+splitRecordSize-1)/splitRecordSize)
						Expect(si.KeyCount).To(Equal(chunks))
						Expect(si.ValueSize).To(Equal(dataLen))
						Expect(si.IsSplit).To(Equal(dataLen > splitRecordSize))
						Expect(si.VersionedInline).To(BeFalse())
						var expectedKeySize int
						for i := range chunks {
							suffix := int64(0)
							if chunks > 1 {
								suffix = int64(i + 1)
							}
							packed, err := appendToTuple(pk, suffix).PackWithVersionstamp(ss.Bytes())
							Expect(err).NotTo(HaveOccurred())
							expectedKeySize += len(packed)
						}
						Expect(si.KeySize).To(Equal(expectedKeySize), "local=%d", local)
					}
					visible, err := rtx.Transaction().GetRange(ss, fdb.RangeOptions{}).GetSliceWithError()
					Expect(visible).To(BeEmpty())
					return nil, err
				})
				Expect(err).NotTo(HaveOccurred())
				_, err = sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
					rows, err := rtx.Transaction().GetRange(ss, fdb.RangeOptions{}).GetSliceWithError()
					if err != nil {
						return nil, err
					}
					chunks := max(1, (dataLen+splitRecordSize-1)/splitRecordSize)
					Expect(rows).To(HaveLen(2 * chunks))
					for local := range 2 {
						var joined []byte
						for i := range chunks {
							row := rows[local*chunks+i]
							key, err := ss.Unpack(row.Key)
							Expect(err).NotTo(HaveOccurred())
							Expect(key).To(HaveLen(3))
							Expect(key[0]).To(Equal(int64(7)))
							stamp, ok := key[1].(tuple.Versionstamp)
							Expect(ok).To(BeTrue())
							Expect(stamp.TransactionVersion).NotTo(Equal(tuple.IncompleteVersionstamp(0).TransactionVersion))
							Expect(stamp.UserVersion).To(Equal(uint16(local)))
							joined = append(joined, row.Value...)
						}
						Expect(bytes.Equal(joined, data)).To(BeTrue())
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			}
		})

		It("rejects incomplete keys without context and multiple incomplete stamps before mutation", func() {
			rs := recordSub()
			_, err := sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
				stamp := tuple.IncompleteVersionstamp(0)
				err := saveWithSplit(nil, rtx.Transaction(), rs, tuple.Tuple{stamp}, nil, true, false, nil, nil)
				var argument *RecordCoreArgumentError
				Expect(errors.As(err, &argument)).To(BeTrue())
				Expect(saveWithSplit(rtx, rtx.Transaction(), rs, tuple.Tuple{stamp, stamp}, nil, true, false, nil, nil)).NotTo(Succeed())
				Expect(rtx.HasVersionMutations()).To(BeFalse())
				rows, err := rtx.Transaction().GetRange(rs, fdb.RangeOptions{}).GetSliceWithError()
				Expect(rows).To(BeEmpty())
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reuses size metrics only after clearing the previous split", func() {
			rs := recordSub()
			_, err := sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
				pk := tuple.Tuple{int64(1)}
				si := &sizeInfo{}
				if err := saveWithSplit(rtx, rtx.Transaction(), rs, pk, makeTestBytes(splitRecordSize+1), true, false, nil, si); err != nil {
					return nil, err
				}
				if err := saveWithSplit(rtx, rtx.Transaction(), rs, pk, []byte("small"), true, false, si, si); err != nil {
					return nil, err
				}
				Expect(si.KeyCount).To(Equal(1))
				Expect(si.ValueSize).To(Equal(5))
				Expect(si.IsSplit).To(BeFalse())
				rows, err := rtx.Transaction().GetRange(rs, fdb.RangeOptions{}).GetSliceWithError()
				Expect(rows).To(HaveLen(1))
				Expect(rows[0].Value).To(Equal([]byte("small")))
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects repeated versionstamped chunk keys including empty values", func() {
			for _, dataLen := range []int{0, 1, splitRecordSize + 1} {
				rs := recordSub().Sub(dataLen)
				_, err := sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
					pk := tuple.Tuple{int64(0), tuple.IncompleteVersionstamp(0)}
					data := makeTestBytes(dataLen)
					if err := saveWithSplit(rtx, rtx.Transaction(), rs, pk, data, true, false, nil, nil); err != nil {
						return nil, err
					}
					return nil, saveWithSplit(rtx, rtx.Transaction(), rs, pk, data, true, false, nil, nil)
				})
				var internal *RecordCoreInternalError
				Expect(errors.As(err, &internal)).To(BeTrue())
				Expect(internal.Message).To(Equal("Key with version overwritten"))
			}
		})

		It("cancels buffered split keys on context clear before commit", func() {
			rs := recordSub()
			_, err := sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
				pk := tuple.Tuple{int64(0), tuple.IncompleteVersionstamp(0)}
				if err := saveWithSplit(rtx, rtx.Transaction(), rs, pk, makeTestBytes(splitRecordSize+1), true, false, nil, nil); err != nil {
					return nil, err
				}
				rtx.ClearRange(rs)
				Expect(rtx.HasVersionMutations()).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
				rows, err := rtx.Transaction().GetRange(rs, fdb.RangeOptions{}).GetSliceWithError()
				Expect(rows).To(BeEmpty())
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns error for empty primary key", func() {
			rs := recordSub()
			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				si := &sizeInfo{}
				err := saveWithSplit(nil, tx, rs, tuple.Tuple{}, []byte("data"), true, false, nil, si)
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
				err := saveWithSplit(nil, tx, rs, pk, data, true, false, nil, si)
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
				err := saveWithSplit(nil, tx, rs, pk, data, true, false, nil, si)
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
				err := saveWithSplit(nil, tx, rs, pk, data, true, false, nil, si)
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
				err := saveWithSplit(nil, tx, rs, pk, data, true, false, nil, si)
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
				err := saveWithSplit(nil, tx, rs, pk, data, true, false, nil, si)
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
				err := saveWithSplit(nil, tx, rs, pk, data, false, false, nil, si)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("Record is too long to be stored in a single value; consider split_long_records"))
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
				err := saveWithSplit(nil, tx, rs, pk, data, false, false, nil, si)
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
				err := saveWithSplit(nil, tx, rs, pk, data, true, false, nil, si)
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

		It("loads existing split data even when new records may not be split", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(104)}

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				// Write only at split suffix (no unsplit key)
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord))),
					makeTestBytes(splitRecordSize))

				si := &sizeInfo{}
				loaded, err := loadWithSplit(tx, rs, pk, false, false, si)
				Expect(err).NotTo(HaveOccurred())
				Expect(loaded).To(Equal(makeTestBytes(splitRecordSize)))
				Expect(si.IsSplit).To(BeTrue())

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
				err := saveWithSplit(nil, tx, rs, pk, original, true, false, nil, saveSI)
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

		It("counts the inline version without adding it to the reassembled record", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(106)}
			original := makeTestBytes(splitRecordSize + 10)

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				// Write split chunks
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord))),
					original[0:splitRecordSize])
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord+1))),
					original[splitRecordSize:])

				version, err := NewCompleteVersion(make([]byte, GlobalVersionBytes), 7)
				Expect(err).NotTo(HaveOccurred())
				packed, err := packVersion(version)
				Expect(err).NotTo(HaveOccurred())
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, recordVersionSuffix))), packed)

				si := &sizeInfo{}
				loaded, err := loadWithSplit(tx, rs, pk, true, false, si)
				Expect(err).NotTo(HaveOccurred())
				Expect(bytes.Equal(loaded, original)).To(BeTrue())
				Expect(si.KeyCount).To(Equal(3))
				Expect(si.VersionedInline).To(BeTrue())
				Expect(si.ValueSize).To(Equal(len(original) + VersionBytes + 1))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// ───────────────────────────────────────────────────────────────────
	// deleteSplit
	// ───────────────────────────────────────────────────────────────────

	// A previous record is cleared as Java's default unrolled delete does: a
	// whole-record write conflict range, then point clears, the inline version
	// first (SplitHelper.java:355-364, 702-715). Go formerly cleared the record
	// key before its version, so a concurrent load in the same transaction could
	// see the version without the record and fail with FoundSplitWithoutStartError
	// (the chaos TestIntraTransactionRace failure).
	Describe("previous record clears", func() {
		for _, path := range []string{"deleteSplit", "clearPreviousRecord"} {
			It(path+" clears the version first, under one record-wide conflict range", func() {
				rs := recordSub()
				pk := tuple.Tuple{int64(210)}
				_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
					tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, unsplitRecord))), []byte("record"))
					tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, recordVersionSuffix))), versionValue())
					rec := &clearRecorder{WritableTransaction: tx}
					// A load between the two clears sees the record without its
					// version, which is a valid record.
					var midErr error
					rec.onFirstClear = func() { _, midErr = loadWithSplit(tx, rs, pk, false, false, nil) }
					old := &sizeInfo{KeyCount: 2, VersionedInline: true}
					if path == "deleteSplit" {
						Expect(deleteSplit(rec, rs, pk, false, false, old)).To(BeTrue())
					} else {
						Expect(clearPreviousRecord(nil, rec, rs, pk, true, old)).To(Succeed())
					}
					Expect(midErr).NotTo(HaveOccurred(), "SPLIT_VERSION_ORPHANED: a load between the clears failed")
					Expect(rec.ranges).To(BeZero())
					begin, end := rs.Sub(pk...).FDBRangeKeys()
					Expect(rec.conflicts).To(Equal([]fdb.KeyRange{{Begin: begin, End: end}}))
					Expect(rec.cleared).To(Equal([]fdb.Key{
						fdb.Key(rs.Pack(appendToTuple(pk, recordVersionSuffix))),
						fdb.Key(rs.Pack(appendToTuple(pk, unsplitRecord))),
					}))
					for _, suffix := range []int64{unsplitRecord, recordVersionSuffix} {
						val, err := tx.Get(fdb.Key(rs.Pack(appendToTuple(pk, suffix)))).Get()
						Expect(err).NotTo(HaveOccurred())
						Expect(val).To(BeNil())
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})
		}

		It("clears every chunk of a split record, as its key count says", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(212)}
			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				rec := &clearRecorder{WritableTransaction: tx}
				Expect(deleteSplit(rec, rs, pk, true, false, &sizeInfo{KeyCount: 4, IsSplit: true, VersionedInline: true})).To(BeTrue())
				var want []fdb.Key
				for _, suffix := range []int64{recordVersionSuffix, startSplitRecord, startSplitRecord + 1, startSplitRecord + 2} {
					want = append(want, fdb.Key(rs.Pack(appendToTuple(pk, suffix))))
				}
				Expect(rec.cleared).To(Equal(want))
				Expect(rec.conflicts).To(HaveLen(1))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		// Java's addWriteConflictRange throws before any clear (SplitHelper.java:360).
		It("returns a conflict range failure without clearing", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(214)}
			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				failure := errors.New("key_outside_legal_range")
				rec := &clearRecorder{WritableTransaction: tx, conflictErr: failure}
				old := &sizeInfo{KeyCount: 2, VersionedInline: true}
				Expect(clearPreviousRecord(nil, rec, rs, pk, true, old)).To(MatchError(failure))
				deleted, delErr := deleteSplit(rec, rs, pk, false, false, old)
				Expect(delErr).To(MatchError(failure))
				Expect(deleted).To(BeFalse())
				Expect(rec.cleared).To(BeEmpty(), "SPLIT_CLEARED_WITHOUT_CONFLICT")
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		// Java skips the clear when the new value overwrites the only key of a
		// known, unsplit, unversioned record (SplitHelper.java:152).
		It("an ordinary unsplit overwrite neither clears nor adds a conflict range", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(215)}
			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, unsplitRecord))), []byte("old"))
				rec := &clearRecorder{WritableTransaction: tx}
				Expect(saveWithSplit(nil, rec, rs, pk, []byte("new"), false, false, &sizeInfo{KeyCount: 1}, nil)).To(Succeed())
				Expect(rec.cleared).To(BeEmpty(), "SPLIT_OVERWRITE_CLEARED")
				Expect(rec.conflicts).To(BeEmpty())
				val, err := tx.Get(fdb.Key(rs.Pack(appendToTuple(pk, unsplitRecord)))).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(Equal([]byte("new")))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		// An empty primary key would address the whole records subspace.
		It("clears nothing for an empty primary key", func() {
			rs := recordSub()
			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				rec := &clearRecorder{WritableTransaction: tx}
				Expect(clearPreviousRecord(nil, rec, rs, tuple.Tuple{}, true, &sizeInfo{KeyCount: 3, IsSplit: true, VersionedInline: true})).To(Succeed())
				Expect(rec.cleared).To(BeEmpty())
				Expect(rec.conflicts).To(BeEmpty())
				Expect(rec.ranges).To(BeZero())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		// The metadata store and pending-writes queue clear the key's whole range
		// (Java's five-argument saveWithSplit, SplitHelper.java:371-372).
		It("a whole-key clear range-clears even an unsplit previous value", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(213)}
			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, unsplitRecord))), []byte("value"))
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord+5))), []byte("stray"))
				rec := &clearRecorder{WritableTransaction: tx}
				Expect(clearPreviousRecord(nil, rec, rs, pk, false, nil)).To(Succeed())
				Expect(rec.ranges).To(Equal(1))
				Expect(rec.cleared).To(BeEmpty())
				val, err := tx.Get(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord+5)))).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		// The state Go's former clear order exposed: the record's key cleared,
		// its inline version still present. A load then fails exactly as the
		// chaos test did.
		It("a version key without its record fails to load", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(211)}
			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, recordVersionSuffix))), versionValue())
				_, err := loadWithSplit(tx, rs, pk, false, false, nil)
				var split *FoundSplitWithoutStartError
				Expect(errors.As(err, &split)).To(BeTrue(), "load = %v", err)
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("deleteSplit", func() {
		It("returns false for empty primary key", func() {
			rs := recordSub()
			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				result, delErr := deleteSplit(tx, rs, tuple.Tuple{}, true, false, &sizeInfo{})
				Expect(delErr).NotTo(HaveOccurred())
				Expect(result).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns false for nil oldsizeInfo", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(200)}
			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				result, delErr := deleteSplit(tx, rs, pk, true, false, nil)
				Expect(delErr).NotTo(HaveOccurred())
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
				result, delErr := deleteSplit(tx, rs, pk, false, false, oldSI)
				Expect(delErr).NotTo(HaveOccurred())
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
				result, delErr := deleteSplit(tx, rs, pk, true, false, oldSI)
				Expect(delErr).NotTo(HaveOccurred())
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
				result, delErr := deleteSplit(tx, rs, pk, false, false, oldSI)
				Expect(delErr).NotTo(HaveOccurred())
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
				result, delErr := deleteSplit(tx, rs, pk, false, false, oldSI)
				Expect(delErr).NotTo(HaveOccurred())
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

		It("checks any record suffix even when new records may not be split", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(303)}

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				// Write only at split suffix — unsplit check won't find it
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord))), []byte("chunk"))

				exists, err := recordExistsWithSplit(tx, rs, pk, false, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeTrue())

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

				Expect(clearPreviousRecord(nil, tx, rs, pk, true, nil)).To(Succeed())

				val, err := tx.Get(fdb.Key(key)).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(Equal([]byte("keep-me")))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("clears every key of a split record", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(401)}

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				// Write split chunks + version key
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, recordVersionSuffix))), []byte("ver"))
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord))), []byte("c1"))
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord+1))), []byte("c2"))
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, startSplitRecord+2))), []byte("c3"))

				oldSI := &sizeInfo{KeyCount: 4, IsSplit: true, VersionedInline: true}
				Expect(clearPreviousRecord(nil, tx, rs, pk, true, oldSI)).To(Succeed())

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

		It("clears only the unsplit key of an unsplit, unversioned record, as Java does", func() {
			rs := recordSub()
			pk := tuple.Tuple{int64(402)}

			_, err := sharedDB.db.Transact(func(tx fdb.WritableTransaction) (any, error) {
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, unsplitRecord))), []byte("data"))
				tx.Set(fdb.Key(rs.Pack(appendToTuple(pk, recordVersionSuffix))), []byte("ver"))

				// Java clears only UNSPLIT_RECORD when the previous record was
				// neither split nor versioned inline (SplitHelper.java:367-370),
				// whatever split_long_records says.
				Expect(clearPreviousRecord(nil, tx, rs, pk, true, &sizeInfo{IsSplit: false})).To(Succeed())

				val, err := tx.Get(fdb.Key(rs.Pack(appendToTuple(pk, unsplitRecord)))).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(BeNil())

				val, err = tx.Get(fdb.Key(rs.Pack(appendToTuple(pk, recordVersionSuffix)))).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(Equal([]byte("ver")))

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
				Expect(clearPreviousRecord(nil, tx, rs, pk, true, oldSI)).To(Succeed())

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
				Expect(clearPreviousRecord(nil, tx, rs, pk, true, oldSI)).To(Succeed())

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
				err := saveWithSplit(nil, tx, rs, pk, bigData, true, false, nil, saveSI)
				Expect(err).NotTo(HaveOccurred())
				Expect(saveSI.IsSplit).To(BeTrue())
				Expect(saveSI.KeyCount).To(Equal(3))

				// Second save: overwrite with small data, passing old sizeInfo
				overwriteSI := &sizeInfo{}
				err = saveWithSplit(nil, tx, rs, pk, smallData, true, false, saveSI, overwriteSI)
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
				err := saveWithSplit(nil, tx, rs, pk, smallData, true, false, nil, saveSI)
				Expect(err).NotTo(HaveOccurred())
				Expect(saveSI.IsSplit).To(BeFalse())

				// Second save: split, passing old sizeInfo
				overwriteSI := &sizeInfo{}
				err = saveWithSplit(nil, tx, rs, pk, bigData, true, false, saveSI, overwriteSI)
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

// versionValue is a stored inline record version (a packed complete versionstamp).
func versionValue() []byte {
	return tuple.Tuple{tuple.Versionstamp{TransactionVersion: [10]byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 0}, UserVersion: 1}}.Pack()
}

// clearRecorder records the clears and write conflict ranges made through it.
type clearRecorder struct {
	fdb.WritableTransaction
	cleared      []fdb.Key
	conflicts    []fdb.KeyRange
	ranges       int
	onFirstClear func()
	conflictErr  error // returned by AddWriteConflictRange instead of adding it
}

func (r *clearRecorder) clearKey(key fdb.Key) {
	r.cleared = append(r.cleared, key)
	if len(r.cleared) == 1 && r.onFirstClear != nil {
		r.onFirstClear()
	}
}

func (r *clearRecorder) Clear(key fdb.KeyConvertible) {
	r.WritableTransaction.Clear(key)
	r.clearKey(key.FDBKey())
}

func (r *clearRecorder) ClearBytes(key []byte) {
	r.WritableTransaction.ClearBytes(key)
	r.clearKey(fdb.Key(key))
}

func (r *clearRecorder) ClearRange(er fdb.ExactRange) {
	r.ranges++
	r.WritableTransaction.ClearRange(er)
}

func (r *clearRecorder) AddWriteConflictRange(er fdb.ExactRange) error {
	if r.conflictErr != nil {
		return r.conflictErr
	}
	begin, end := er.FDBRangeKeys()
	r.conflicts = append(r.conflicts, fdb.KeyRange{Begin: begin.FDBKey(), End: end.FDBKey()})
	return r.WritableTransaction.AddWriteConflictRange(er)
}
