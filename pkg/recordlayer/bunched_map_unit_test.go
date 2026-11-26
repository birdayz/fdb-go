package recordlayer

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestStoreTimerBunchedMapAdapter(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	g.Expect(NewBunchedMap(5)).NotTo(BeNil())
	g.Expect(NewInstrumentedBunchedMap(5, nil)).NotTo(BeNil())

	timer := NewStoreTimer()
	g.Expect(NewInstrumentedBunchedMap(10, timer)).NotTo(BeNil())

	adapter := storeTimerBunchedMapAdapter{timer: timer}
	adapter.OnWrite(2, 3, -1)
	g.Expect(timer.GetCount(CountSaveIndexKey)).To(Equal(int64(1)))
	g.Expect(timer.GetCount(CountSaveIndexKeyBytes)).To(Equal(int64(2)))
	g.Expect(timer.GetCount(CountSaveIndexValueBytes)).To(Equal(int64(3)))
	g.Expect(timer.GetCount(CountDeleteIndexValueBytes)).To(Equal(int64(0)))

	adapter.OnWrite(2, 3, 4)
	g.Expect(timer.GetCount(CountSaveIndexKey)).To(Equal(int64(2)))
	g.Expect(timer.GetCount(CountDeleteIndexValueBytes)).To(Equal(int64(4)))

	adapter.OnDelete(3, -1)
	g.Expect(timer.GetCount(CountDeleteIndexKey)).To(Equal(int64(1)))
	g.Expect(timer.GetCount(CountDeleteIndexKeyBytes)).To(Equal(int64(3)))
	g.Expect(timer.GetCount(CountDeleteIndexValueBytes)).To(Equal(int64(4)))

	adapter.OnDelete(2, 5)
	g.Expect(timer.GetCount(CountDeleteIndexKey)).To(Equal(int64(2)))
	g.Expect(timer.GetCount(CountDeleteIndexValueBytes)).To(Equal(int64(9)))

	adapter.OnRangeRead([]fdb.KeyValue{
		{Key: []byte{0x01, 0x02}, Value: []byte{0x03}},
		{Key: []byte{0x04, 0x05, 0x06}, Value: []byte{0x07, 0x08}},
	})
	g.Expect(timer.GetCount(CountLoadIndexKey)).To(Equal(int64(2)))
	g.Expect(timer.GetCount(CountLoadIndexKeyBytes)).To(Equal(int64(5)))
	g.Expect(timer.GetCount(CountLoadIndexValueBytes)).To(Equal(int64(3)))
}

var _ = Describe("textSubspaceSplitter public API", func() {
	It("SubspaceOf extracts grouping columns", func() {
		indexSS := subspace.FromBytes(tuple.Tuple{"idx"}.Pack())
		splitter := newTextSubspaceSplitter(indexSS, 2)

		fullKey := indexSS.Pack(tuple.Tuple{"group1", "token", "pk_part"})
		ss, err := splitter.SubspaceOf(fullKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(ss.Bytes()).To(Equal(indexSS.Sub("group1", "token").Bytes()))
	})

	It("SubspaceOf returns error for short key", func() {
		indexSS := subspace.FromBytes(tuple.Tuple{"idx"}.Pack())
		splitter := newTextSubspaceSplitter(indexSS, 3)

		shortKey := indexSS.Pack(tuple.Tuple{"group1", "token"})
		_, err := splitter.SubspaceOf(shortKey)
		var serErr *BunchedSerializationError
		Expect(err).To(BeAssignableToTypeOf(serErr))
	})

	It("SubspaceTag returns grouping tuple", func() {
		indexSS := subspace.FromBytes(tuple.Tuple{"idx"}.Pack())
		splitter := newTextSubspaceSplitter(indexSS, 2)

		tag, err := splitter.SubspaceTag(indexSS.Sub("group1", "token"))
		Expect(err).NotTo(HaveOccurred())
		Expect(tag).To(Equal(tuple.Tuple{"group1", "token"}))
	})
})

func TestBunchedMapExceptionError(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	e := &BunchedMapException{Message: "signpost mismatch"}
	g.Expect(e.Error()).To(ContainSubstring("signpost mismatch"))
	g.Expect(e.Error()).To(ContainSubstring("bunched map error"))
}

func TestBunchedSerializationError(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	e := &BunchedSerializationError{Message: "corrupt prefix", Data: []byte{0xDE, 0xAD}}
	g.Expect(e.Error()).To(ContainSubstring("corrupt prefix"))
	g.Expect(e.Error()).To(ContainSubstring("data len=2"))

	e2 := &BunchedSerializationError{Message: "short"}
	g.Expect(e2.Error()).NotTo(ContainSubstring("data len"))
}
