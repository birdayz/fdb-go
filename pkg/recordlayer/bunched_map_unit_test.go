package recordlayer

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// ===========================================================================
// joinBytes
// ===========================================================================

func TestJoinBytesEmpty(t *testing.T) {
	t.Parallel()
	result := joinBytes()
	Expect(result).To(BeEmpty())
}

func TestJoinBytesSingle(t *testing.T) {
	t.Parallel()
	result := joinBytes([]byte{0x01, 0x02})
	Expect(result).To(Equal([]byte{0x01, 0x02}))
}

func TestJoinBytesMultiple(t *testing.T) {
	t.Parallel()
	result := joinBytes([]byte{0x01}, []byte{0x02, 0x03}, []byte{0x04})
	Expect(result).To(Equal([]byte{0x01, 0x02, 0x03, 0x04}))
}

func TestJoinBytesWithNilSlice(t *testing.T) {
	t.Parallel()
	result := joinBytes([]byte{0x01}, nil, []byte{0x02})
	Expect(result).To(Equal([]byte{0x01, 0x02}))
}

func TestJoinBytesAllNil(t *testing.T) {
	t.Parallel()
	result := joinBytes(nil, nil)
	Expect(result).To(BeEmpty())
}

func TestJoinBytesAllEmpty(t *testing.T) {
	t.Parallel()
	result := joinBytes([]byte{}, []byte{}, []byte{})
	Expect(result).To(BeEmpty())
}

// ===========================================================================
// tupleEqual
// ===========================================================================

func TestTupleEqualSame(t *testing.T) {
	t.Parallel()
	a := tuple.Tuple{int64(42), "hello"}
	b := tuple.Tuple{int64(42), "hello"}
	Expect(tupleEqual(a, b)).To(BeTrue())
}

func TestTupleEqualDifferent(t *testing.T) {
	t.Parallel()
	a := tuple.Tuple{int64(42), "hello"}
	b := tuple.Tuple{int64(42), "world"}
	Expect(tupleEqual(a, b)).To(BeFalse())
}

func TestTupleEqualEmpty(t *testing.T) {
	t.Parallel()
	a := tuple.Tuple{}
	b := tuple.Tuple{}
	Expect(tupleEqual(a, b)).To(BeTrue())
}

func TestTupleEqualDifferentLengths(t *testing.T) {
	t.Parallel()
	a := tuple.Tuple{int64(1)}
	b := tuple.Tuple{int64(1), int64(2)}
	Expect(tupleEqual(a, b)).To(BeFalse())
}

func TestTupleEqualNilVsEmpty(t *testing.T) {
	t.Parallel()
	var a tuple.Tuple
	b := tuple.Tuple{}
	// Both pack to empty bytes.
	Expect(tupleEqual(a, b)).To(BeTrue())
}

// ===========================================================================
// positionListsEqual
// ===========================================================================

func TestPositionListsEqualSame(t *testing.T) {
	t.Parallel()
	Expect(positionListsEqual([]int{1, 2, 3}, []int{1, 2, 3})).To(BeTrue())
}

func TestPositionListsEqualDifferent(t *testing.T) {
	t.Parallel()
	Expect(positionListsEqual([]int{1, 2, 3}, []int{1, 2, 4})).To(BeFalse())
}

func TestPositionListsEqualDifferentLengths(t *testing.T) {
	t.Parallel()
	Expect(positionListsEqual([]int{1, 2}, []int{1, 2, 3})).To(BeFalse())
}

func TestPositionListsEqualBothEmpty(t *testing.T) {
	t.Parallel()
	Expect(positionListsEqual([]int{}, []int{})).To(BeTrue())
}

func TestPositionListsEqualBothNil(t *testing.T) {
	t.Parallel()
	Expect(positionListsEqual(nil, nil)).To(BeTrue())
}

func TestPositionListsEqualNilVsEmpty(t *testing.T) {
	t.Parallel()
	// nil and empty have equal length (0), so loop body never executes.
	Expect(positionListsEqual(nil, []int{})).To(BeTrue())
}

func TestPositionListsEqualSingleElement(t *testing.T) {
	t.Parallel()
	Expect(positionListsEqual([]int{42}, []int{42})).To(BeTrue())
	Expect(positionListsEqual([]int{42}, []int{43})).To(BeFalse())
}

// ===========================================================================
// boolToInt
// ===========================================================================

func TestBoolToIntTrue(t *testing.T) {
	t.Parallel()
	Expect(boolToInt(true)).To(Equal(1))
}

func TestBoolToIntFalse(t *testing.T) {
	t.Parallel()
	Expect(boolToInt(false)).To(Equal(0))
}

// ===========================================================================
// tupleToTupleElements
// ===========================================================================

func TestTupleToTupleElementsEmpty(t *testing.T) {
	t.Parallel()
	result := tupleToTupleElements(tuple.Tuple{})
	Expect(result).To(BeEmpty())
}

func TestTupleToTupleElementsSingle(t *testing.T) {
	t.Parallel()
	result := tupleToTupleElements(tuple.Tuple{int64(42)})
	Expect(result).To(HaveLen(1))
	Expect(result[0]).To(Equal(int64(42)))
}

func TestTupleToTupleElementsMixed(t *testing.T) {
	t.Parallel()
	result := tupleToTupleElements(tuple.Tuple{int64(1), "hello", []byte{0xFF}})
	Expect(result).To(HaveLen(3))
	Expect(result[0]).To(Equal(int64(1)))
	Expect(result[1]).To(Equal("hello"))
	Expect(result[2]).To(Equal([]byte{0xFF}))
}

// ===========================================================================
// NewBunchedMap / NewInstrumentedBunchedMap
// ===========================================================================

func TestNewBunchedMap(t *testing.T) {
	t.Parallel()
	bm := NewBunchedMap(5)
	Expect(bm).NotTo(BeNil())
	Expect(bm.bunchSize).To(Equal(5))
	Expect(bm.serializer).NotTo(BeNil())
	Expect(bm.timer).To(BeNil())
}

func TestNewInstrumentedBunchedMap(t *testing.T) {
	t.Parallel()
	timer := NewStoreTimer()
	bm := NewInstrumentedBunchedMap(10, timer)
	Expect(bm).NotTo(BeNil())
	Expect(bm.bunchSize).To(Equal(10))
	Expect(bm.serializer).NotTo(BeNil())
	Expect(bm.timer).To(Equal(timer))
}

// ===========================================================================
// instrumentWrite
// ===========================================================================

func TestInstrumentWriteNilTimer(t *testing.T) {
	t.Parallel()
	bm := NewBunchedMap(5)
	// Must not panic.
	bm.instrumentWrite([]byte{0x01, 0x02}, []byte{0x03, 0x04, 0x05}, nil)
}

func TestInstrumentWriteWithTimer(t *testing.T) {
	t.Parallel()
	timer := NewStoreTimer()
	bm := NewInstrumentedBunchedMap(5, timer)

	key := []byte{0x01, 0x02}
	value := []byte{0x03, 0x04, 0x05}
	bm.instrumentWrite(key, value, nil)

	Expect(timer.GetCount(CountSaveIndexKey)).To(Equal(int64(1)))
	Expect(timer.GetCount(CountSaveIndexKeyBytes)).To(Equal(int64(2)))
	Expect(timer.GetCount(CountSaveIndexValueBytes)).To(Equal(int64(3)))
	Expect(timer.GetCount(CountDeleteIndexValueBytes)).To(Equal(int64(0)))
}

func TestInstrumentWriteWithOldValue(t *testing.T) {
	t.Parallel()
	timer := NewStoreTimer()
	bm := NewInstrumentedBunchedMap(5, timer)

	key := []byte{0x01, 0x02}
	value := []byte{0x03, 0x04, 0x05}
	oldValue := []byte{0x10, 0x20, 0x30, 0x40}
	bm.instrumentWrite(key, value, oldValue)

	Expect(timer.GetCount(CountSaveIndexKey)).To(Equal(int64(1)))
	Expect(timer.GetCount(CountSaveIndexKeyBytes)).To(Equal(int64(2)))
	Expect(timer.GetCount(CountSaveIndexValueBytes)).To(Equal(int64(3)))
	Expect(timer.GetCount(CountDeleteIndexValueBytes)).To(Equal(int64(4)))
}

func TestInstrumentWriteMultipleCalls(t *testing.T) {
	t.Parallel()
	timer := NewStoreTimer()
	bm := NewInstrumentedBunchedMap(5, timer)

	bm.instrumentWrite([]byte{0x01}, []byte{0x02}, nil)
	bm.instrumentWrite([]byte{0x03, 0x04}, []byte{0x05, 0x06, 0x07}, []byte{0x08})

	Expect(timer.GetCount(CountSaveIndexKey)).To(Equal(int64(2)))
	Expect(timer.GetCount(CountSaveIndexKeyBytes)).To(Equal(int64(3)))   // 1 + 2
	Expect(timer.GetCount(CountSaveIndexValueBytes)).To(Equal(int64(4))) // 1 + 3
	Expect(timer.GetCount(CountDeleteIndexValueBytes)).To(Equal(int64(1)))
}

// ===========================================================================
// instrumentDelete
// ===========================================================================

func TestInstrumentDeleteNilTimer(t *testing.T) {
	t.Parallel()
	bm := NewBunchedMap(5)
	// Must not panic.
	bm.instrumentDelete([]byte{0x01}, []byte{0x02})
}

func TestInstrumentDeleteWithTimer(t *testing.T) {
	t.Parallel()
	timer := NewStoreTimer()
	bm := NewInstrumentedBunchedMap(5, timer)

	key := []byte{0x01, 0x02, 0x03}
	bm.instrumentDelete(key, nil)

	Expect(timer.GetCount(CountDeleteIndexKey)).To(Equal(int64(1)))
	Expect(timer.GetCount(CountDeleteIndexKeyBytes)).To(Equal(int64(3)))
	Expect(timer.GetCount(CountDeleteIndexValueBytes)).To(Equal(int64(0)))
}

func TestInstrumentDeleteWithOldValue(t *testing.T) {
	t.Parallel()
	timer := NewStoreTimer()
	bm := NewInstrumentedBunchedMap(5, timer)

	key := []byte{0x01, 0x02}
	oldValue := []byte{0x10, 0x20, 0x30, 0x40, 0x50}
	bm.instrumentDelete(key, oldValue)

	Expect(timer.GetCount(CountDeleteIndexKey)).To(Equal(int64(1)))
	Expect(timer.GetCount(CountDeleteIndexKeyBytes)).To(Equal(int64(2)))
	Expect(timer.GetCount(CountDeleteIndexValueBytes)).To(Equal(int64(5)))
}

// ===========================================================================
// instrumentRangeRead
// ===========================================================================

func TestInstrumentRangeReadNilTimer(t *testing.T) {
	t.Parallel()
	bm := NewBunchedMap(5)
	// Must not panic.
	bm.instrumentRangeRead([]fdb.KeyValue{
		{Key: []byte{0x01}, Value: []byte{0x02}},
	})
}

func TestInstrumentRangeReadWithTimer(t *testing.T) {
	t.Parallel()
	timer := NewStoreTimer()
	bm := NewInstrumentedBunchedMap(5, timer)

	kvs := []fdb.KeyValue{
		{Key: []byte{0x01, 0x02}, Value: []byte{0x03}},
		{Key: []byte{0x04, 0x05, 0x06}, Value: []byte{0x07, 0x08}},
	}
	bm.instrumentRangeRead(kvs)

	Expect(timer.GetCount(CountLoadIndexKey)).To(Equal(int64(2)))
	Expect(timer.GetCount(CountLoadIndexKeyBytes)).To(Equal(int64(5)))   // 2 + 3
	Expect(timer.GetCount(CountLoadIndexValueBytes)).To(Equal(int64(3))) // 1 + 2
}

func TestInstrumentRangeReadEmpty(t *testing.T) {
	t.Parallel()
	timer := NewStoreTimer()
	bm := NewInstrumentedBunchedMap(5, timer)
	bm.instrumentRangeRead(nil)

	Expect(timer.GetCount(CountLoadIndexKey)).To(Equal(int64(0)))
}

// ===========================================================================
// TextSubspaceSplitter
// ===========================================================================

var _ = Describe("TextSubspaceSplitter", func() {
	It("SubspaceOf extracts grouping columns", func() {
		indexSS := subspace.FromBytes(tuple.Tuple{"idx"}.Pack())
		splitter := NewTextSubspaceSplitter(indexSS, 2)

		// Build a key: indexSS + ("group1", "token", "pk_part")
		fullKey := indexSS.Pack(tuple.Tuple{"group1", "token", "pk_part"})

		ss, err := splitter.SubspaceOf(fullKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(ss).NotTo(BeNil())

		// The subspace should be indexSS + ("group1", "token")
		expected := indexSS.Sub("group1", "token")
		Expect(ss.Bytes()).To(Equal(expected.Bytes()))
	})

	It("SubspaceOf returns error for short key", func() {
		indexSS := subspace.FromBytes(tuple.Tuple{"idx"}.Pack())
		splitter := NewTextSubspaceSplitter(indexSS, 3)

		// Build a key with only 2 tuple elements (need 3).
		shortKey := indexSS.Pack(tuple.Tuple{"group1", "token"})

		_, err := splitter.SubspaceOf(shortKey)
		Expect(err).To(HaveOccurred())
		var serErr *BunchedSerializationError
		Expect(err).To(BeAssignableToTypeOf(serErr))
	})

	It("SubspaceTag returns grouping tuple", func() {
		indexSS := subspace.FromBytes(tuple.Tuple{"idx"}.Pack())
		splitter := NewTextSubspaceSplitter(indexSS, 2)

		// Build a subspace that is indexSS + ("group1", "token")
		ss := indexSS.Sub("group1", "token")

		tag, err := splitter.SubspaceTag(ss)
		Expect(err).NotTo(HaveOccurred())
		Expect(tag).To(Equal(tuple.Tuple{"group1", "token"}))
	})

	It("SubspaceOf with single grouping column", func() {
		indexSS := subspace.FromBytes(tuple.Tuple{"idx"}.Pack())
		splitter := NewTextSubspaceSplitter(indexSS, 1)

		fullKey := indexSS.Pack(tuple.Tuple{"token", int64(42)})

		ss, err := splitter.SubspaceOf(fullKey)
		Expect(err).NotTo(HaveOccurred())

		expected := indexSS.Sub("token")
		Expect(ss.Bytes()).To(Equal(expected.Bytes()))
	})

	It("SubspaceOf with integer grouping columns", func() {
		indexSS := subspace.FromBytes(tuple.Tuple{"idx"}.Pack())
		splitter := NewTextSubspaceSplitter(indexSS, 2)

		fullKey := indexSS.Pack(tuple.Tuple{int64(1), "token", "pk"})

		ss, err := splitter.SubspaceOf(fullKey)
		Expect(err).NotTo(HaveOccurred())

		expected := indexSS.Sub(int64(1), "token")
		Expect(ss.Bytes()).To(Equal(expected.Bytes()))
	})
})

// ===========================================================================
// BunchedMapMultiIterator — initial state, cancel, err
// ===========================================================================

var _ = Describe("BunchedMapMultiIterator state", func() {
	It("Cancel stops iteration", func() {
		it := &BunchedMapMultiIterator{
			serializer: TextIndexBunchedSerializerInstance(),
		}
		it.Cancel()
		Expect(it.done).To(BeTrue())
	})

	It("Err returns nil by default", func() {
		it := &BunchedMapMultiIterator{
			serializer: TextIndexBunchedSerializerInstance(),
		}
		Expect(it.Err()).To(BeNil())
	})

	It("Err returns sticky error", func() {
		it := &BunchedMapMultiIterator{
			serializer: TextIndexBunchedSerializerInstance(),
			iterErr:    &BunchedMapException{Message: "test error"},
		}
		Expect(it.Err()).To(MatchError(ContainSubstring("test error")))
	})

	It("GetContinuation returns nil when no iteration has occurred", func() {
		it := &BunchedMapMultiIterator{
			serializer: TextIndexBunchedSerializerInstance(),
		}
		Expect(it.GetContinuation()).To(BeNil())
	})

	It("GetContinuation returns nil when done and not stopped by limit", func() {
		it := &BunchedMapMultiIterator{
			serializer:            TextIndexBunchedSerializerInstance(),
			lastKey:               tuple.Tuple{"foo"},
			currentSubspaceKey:    []byte{0x01},
			currentSubspaceSuffix: []byte{0x01},
			done:                  true,
			limit:                 0, // no limit
			returned:              5,
		}
		Expect(it.GetContinuation()).To(BeNil())
	})

	It("GetContinuation returns token when stopped by limit", func() {
		s := TextIndexBunchedSerializerInstance()
		lastKey := tuple.Tuple{"mytoken"}
		suffix := []byte{0xAB, 0xCD}
		it := &BunchedMapMultiIterator{
			serializer:            s,
			lastKey:               lastKey,
			currentSubspaceKey:    []byte{0x01, 0xAB, 0xCD},
			currentSubspaceSuffix: suffix,
			done:                  true,
			limit:                 5,
			returned:              5, // returned == limit => stopped by limit
		}
		cont := it.GetContinuation()
		Expect(cont).NotTo(BeNil())
		// Continuation = subspaceSuffix + serializedKey
		expected := append(append([]byte{}, suffix...), s.SerializeKey(lastKey)...)
		Expect(cont).To(Equal(expected))
	})

	It("HasNext returns false when done", func() {
		it := &BunchedMapMultiIterator{
			serializer: TextIndexBunchedSerializerInstance(),
			done:       true,
		}
		Expect(it.HasNext()).To(BeFalse())
	})

	It("Next returns nil when no more entries", func() {
		it := &BunchedMapMultiIterator{
			serializer: TextIndexBunchedSerializerInstance(),
			done:       true,
		}
		Expect(it.Next()).To(BeNil())
	})
})

// ===========================================================================
// BunchedMapIterator — initial state, err, continuation
// ===========================================================================

var _ = Describe("BunchedMapIterator state", func() {
	It("Err returns nil by default", func() {
		it := &BunchedMapIterator{
			serializer: TextIndexBunchedSerializerInstance(),
		}
		Expect(it.Err()).To(BeNil())
	})

	It("Err returns sticky error", func() {
		it := &BunchedMapIterator{
			serializer: TextIndexBunchedSerializerInstance(),
			iterErr:    &BunchedSerializationError{Message: "bad data"},
		}
		Expect(it.Err()).To(MatchError(ContainSubstring("bad data")))
	})

	It("GetContinuation returns nil when no iteration has occurred", func() {
		it := &BunchedMapIterator{
			serializer: TextIndexBunchedSerializerInstance(),
		}
		Expect(it.GetContinuation()).To(BeNil())
	})

	It("GetContinuation returns nil when done and not stopped by limit", func() {
		it := &BunchedMapIterator{
			serializer: TextIndexBunchedSerializerInstance(),
			lastKey:    tuple.Tuple{"foo"},
			done:       true,
			limit:      0,
			returned:   3,
		}
		Expect(it.GetContinuation()).To(BeNil())
	})

	It("GetContinuation returns nil when done with limit not reached", func() {
		it := &BunchedMapIterator{
			serializer: TextIndexBunchedSerializerInstance(),
			lastKey:    tuple.Tuple{"foo"},
			done:       true,
			limit:      10,
			returned:   3, // returned < limit => exhausted
		}
		Expect(it.GetContinuation()).To(BeNil())
	})

	It("GetContinuation returns token when stopped by limit", func() {
		s := TextIndexBunchedSerializerInstance()
		lastKey := tuple.Tuple{"bar"}
		it := &BunchedMapIterator{
			serializer: s,
			lastKey:    lastKey,
			done:       true,
			limit:      5,
			returned:   5, // returned == limit => stopped by limit
		}
		cont := it.GetContinuation()
		Expect(cont).NotTo(BeNil())
		Expect(cont).To(Equal(s.SerializeKey(lastKey)))
	})

	It("GetContinuation returns token when not done", func() {
		s := TextIndexBunchedSerializerInstance()
		lastKey := tuple.Tuple{int64(99)}
		it := &BunchedMapIterator{
			serializer: s,
			lastKey:    lastKey,
			done:       false,
			limit:      10,
			returned:   3,
		}
		cont := it.GetContinuation()
		Expect(cont).NotTo(BeNil())
		Expect(cont).To(Equal(s.SerializeKey(lastKey)))
	})

	It("HasNext returns false when done", func() {
		it := &BunchedMapIterator{
			serializer: TextIndexBunchedSerializerInstance(),
			done:       true,
		}
		Expect(it.HasNext()).To(BeFalse())
	})

	It("Next returns nil when done", func() {
		it := &BunchedMapIterator{
			serializer: TextIndexBunchedSerializerInstance(),
			done:       true,
		}
		Expect(it.Next()).To(BeNil())
	})

	It("Next returns cached entry and advances counters", func() {
		entry := bunchedEntry{
			Key:   tuple.Tuple{"cached"},
			Value: []int{1, 2, 3},
		}
		it := &BunchedMapIterator{
			serializer: TextIndexBunchedSerializerInstance(),
			nextEntry:  &entry,
			limit:      0, // unlimited
		}
		result := it.Next()
		Expect(result).NotTo(BeNil())
		Expect(result.Key).To(Equal(tuple.Tuple{"cached"}))
		Expect(result.Value).To(Equal([]int{1, 2, 3}))
		Expect(it.lastKey).To(Equal(tuple.Tuple{"cached"}))
		Expect(it.returned).To(Equal(1))
		Expect(it.nextEntry).To(BeNil()) // consumed
	})

	It("Next sets done when limit reached", func() {
		entry := bunchedEntry{
			Key:   tuple.Tuple{"last"},
			Value: []int{5},
		}
		it := &BunchedMapIterator{
			serializer: TextIndexBunchedSerializerInstance(),
			nextEntry:  &entry,
			limit:      1,
			returned:   0,
		}
		result := it.Next()
		Expect(result).NotTo(BeNil())
		Expect(it.returned).To(Equal(1))
		Expect(it.done).To(BeTrue())
	})
})

// ===========================================================================
// BunchedMapMultiIterator — Next advances counters and enforces limit
// ===========================================================================

var _ = Describe("BunchedMapMultiIterator Next", func() {
	It("Next returns cached entry and advances counters", func() {
		entry := &BunchedMapScanEntry{
			Subspace:    subspace.FromBytes([]byte{0x01}),
			SubspaceTag: tuple.Tuple{"tag"},
			Key:         tuple.Tuple{"cached"},
			Value:       []int{10, 20},
		}
		it := &BunchedMapMultiIterator{
			serializer: TextIndexBunchedSerializerInstance(),
			nextEntry:  entry,
			limit:      0,
		}
		result := it.Next()
		Expect(result).NotTo(BeNil())
		Expect(result.Key).To(Equal(tuple.Tuple{"cached"}))
		Expect(it.lastKey).To(Equal(tuple.Tuple{"cached"}))
		Expect(it.returned).To(Equal(1))
		Expect(it.nextEntry).To(BeNil())
	})

	It("Next sets done when limit reached", func() {
		entry := &BunchedMapScanEntry{
			Key:   tuple.Tuple{"last"},
			Value: []int{5},
		}
		it := &BunchedMapMultiIterator{
			serializer: TextIndexBunchedSerializerInstance(),
			nextEntry:  entry,
			limit:      1,
			returned:   0,
		}
		result := it.Next()
		Expect(result).NotTo(BeNil())
		Expect(it.returned).To(Equal(1))
		Expect(it.done).To(BeTrue())
	})

	It("Next returns nil when done", func() {
		it := &BunchedMapMultiIterator{
			serializer: TextIndexBunchedSerializerInstance(),
			done:       true,
		}
		Expect(it.Next()).To(BeNil())
	})
})

// ===========================================================================
// BunchedMapException / BunchedSerializationError — error messages
// ===========================================================================

func TestBunchedMapExceptionError(t *testing.T) {
	t.Parallel()
	e := &BunchedMapException{Message: "signpost mismatch"}
	Expect(e.Error()).To(ContainSubstring("signpost mismatch"))
	Expect(e.Error()).To(ContainSubstring("bunched map error"))
}

func TestBunchedSerializationErrorWithData(t *testing.T) {
	t.Parallel()
	e := &BunchedSerializationError{
		Message: "corrupt prefix",
		Data:    []byte{0xDE, 0xAD},
	}
	Expect(e.Error()).To(ContainSubstring("corrupt prefix"))
	Expect(e.Error()).To(ContainSubstring("data len=2"))
}

func TestBunchedSerializationErrorWithoutData(t *testing.T) {
	t.Parallel()
	e := &BunchedSerializationError{
		Message: "no data available",
	}
	Expect(e.Error()).To(ContainSubstring("no data available"))
	Expect(e.Error()).NotTo(ContainSubstring("data len="))
}

// ===========================================================================
// zeroArray constant
// ===========================================================================

func TestZeroArrayValue(t *testing.T) {
	t.Parallel()
	Expect(zeroArray).To(Equal([]byte{0x00}))
}

// ===========================================================================
// bunchedMapMaxValueSize constant
// ===========================================================================

func TestBunchedMapMaxValueSize(t *testing.T) {
	t.Parallel()
	Expect(bunchedMapMaxValueSize).To(Equal(10_000))
}
