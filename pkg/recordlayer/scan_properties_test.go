package recordlayer

import (
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("IsolationLevel", func() {
	It("Snapshot IsSnapshot returns true", func() {
		Expect(IsolationLevelSnapshot.IsSnapshot()).To(BeTrue())
	})

	It("Serializable IsSnapshot returns false", func() {
		Expect(IsolationLevelSerializable.IsSnapshot()).To(BeFalse())
	})

	It("Snapshot String returns Snapshot", func() {
		Expect(IsolationLevelSnapshot.String()).To(Equal("Snapshot"))
	})

	It("Serializable String returns Serializable", func() {
		Expect(IsolationLevelSerializable.String()).To(Equal("Serializable"))
	})

	It("unknown value String returns Unknown", func() {
		Expect(IsolationLevel(99).String()).To(Equal("Unknown"))
	})

	It("legacy aliases match canonical constants", func() {
		Expect(SnapshotIsolation).To(Equal(IsolationLevelSnapshot))
		Expect(SerializableIsolation).To(Equal(IsolationLevelSerializable))
	})
})

var _ = Describe("CursorStreamingMode", func() {
	DescribeTable("ToFDB maps to correct FDB streaming mode",
		func(mode CursorStreamingMode, expected fdb.StreamingMode) {
			Expect(mode.ToFDB()).To(Equal(expected))
		},
		Entry("Small", StreamingModeSmall, fdb.StreamingModeSmall),
		Entry("Medium", StreamingModeMedium, fdb.StreamingModeMedium),
		Entry("Large", StreamingModeLarge, fdb.StreamingModeLarge),
		Entry("Serial", StreamingModeSerial, fdb.StreamingModeSerial),
		Entry("WantAll", StreamingModeWantAll, fdb.StreamingModeWantAll),
		Entry("Iterator", StreamingModeIterator, fdb.StreamingModeIterator),
		Entry("unknown defaults to Iterator", CursorStreamingMode(99), fdb.StreamingModeIterator),
	)
})

var _ = Describe("ExecuteProperties", func() {
	It("DefaultExecuteProperties has correct defaults", func() {
		p := DefaultExecuteProperties()
		Expect(p.IsolationLevel).To(Equal(IsolationLevelSerializable))
		Expect(p.ReturnedRowLimit).To(Equal(0))
		Expect(p.ScannedRecordsLimit).To(Equal(0))
		Expect(p.ScannedBytesLimit).To(Equal(int64(0)))
		Expect(p.TimeLimit).To(Equal(time.Duration(0)))
		Expect(p.DefaultCursorStreamingMode).To(Equal(StreamingModeIterator))
		Expect(p.FailOnScanLimitReached).To(BeFalse())
		Expect(p.Skip).To(Equal(0))
	})

	It("WithReturnedRowLimit returns immutable copy", func() {
		orig := DefaultExecuteProperties()
		updated := orig.WithReturnedRowLimit(42)
		Expect(updated.ReturnedRowLimit).To(Equal(42))
		Expect(orig.ReturnedRowLimit).To(Equal(0))
	})

	It("WithTimeLimit returns immutable copy", func() {
		orig := DefaultExecuteProperties()
		updated := orig.WithTimeLimit(5 * time.Second)
		Expect(updated.TimeLimit).To(Equal(5 * time.Second))
		Expect(orig.TimeLimit).To(Equal(time.Duration(0)))
	})

	It("WithIsolationLevel returns immutable copy", func() {
		orig := DefaultExecuteProperties()
		updated := orig.WithIsolationLevel(IsolationLevelSnapshot)
		Expect(updated.IsolationLevel).To(Equal(IsolationLevelSnapshot))
		Expect(orig.IsolationLevel).To(Equal(IsolationLevelSerializable))
	})

	It("WithScannedRecordsLimit returns immutable copy", func() {
		orig := DefaultExecuteProperties()
		updated := orig.WithScannedRecordsLimit(1000)
		Expect(updated.ScannedRecordsLimit).To(Equal(1000))
		Expect(orig.ScannedRecordsLimit).To(Equal(0))
	})

	It("WithScannedBytesLimit returns immutable copy", func() {
		orig := DefaultExecuteProperties()
		updated := orig.WithScannedBytesLimit(1024 * 1024)
		Expect(updated.ScannedBytesLimit).To(Equal(int64(1024 * 1024)))
		Expect(orig.ScannedBytesLimit).To(Equal(int64(0)))
	})

	It("WithSkip returns immutable copy", func() {
		orig := DefaultExecuteProperties()
		updated := orig.WithSkip(10)
		Expect(updated.Skip).To(Equal(10))
		Expect(orig.Skip).To(Equal(0))
	})

	It("ClearRowAndTimeLimits clears row limit and time limit, preserves others", func() {
		p := DefaultExecuteProperties().
			WithReturnedRowLimit(50).
			WithTimeLimit(3 * time.Second).
			WithScannedRecordsLimit(200).
			WithSkip(5)
		cleared := p.ClearRowAndTimeLimits()
		Expect(cleared.ReturnedRowLimit).To(Equal(0))
		Expect(cleared.TimeLimit).To(Equal(time.Duration(0)))
		Expect(cleared.ScannedRecordsLimit).To(Equal(200))
		Expect(cleared.Skip).To(Equal(5))
		// original unchanged
		Expect(p.ReturnedRowLimit).To(Equal(50))
		Expect(p.TimeLimit).To(Equal(3 * time.Second))
	})

	It("ClearSkipAndLimit clears skip and row limit, preserves others", func() {
		p := DefaultExecuteProperties().
			WithReturnedRowLimit(100).
			WithSkip(20).
			WithTimeLimit(2 * time.Second).
			WithScannedRecordsLimit(500)
		cleared := p.ClearSkipAndLimit()
		Expect(cleared.Skip).To(Equal(0))
		Expect(cleared.ReturnedRowLimit).To(Equal(0))
		Expect(cleared.TimeLimit).To(Equal(2 * time.Second))
		Expect(cleared.ScannedRecordsLimit).To(Equal(500))
		// original unchanged
		Expect(p.Skip).To(Equal(20))
		Expect(p.ReturnedRowLimit).To(Equal(100))
	})

	It("chaining preserves all fields", func() {
		p := DefaultExecuteProperties().
			WithReturnedRowLimit(10).
			WithSkip(5).
			WithIsolationLevel(IsolationLevelSnapshot).
			WithTimeLimit(1 * time.Second)
		Expect(p.ReturnedRowLimit).To(Equal(10))
		Expect(p.Skip).To(Equal(5))
		Expect(p.IsolationLevel).To(Equal(IsolationLevelSnapshot))
		Expect(p.TimeLimit).To(Equal(1 * time.Second))
	})
})

var _ = Describe("ScanProperties", func() {
	It("ForwardScan is not reverse and uses iterator mode", func() {
		s := ForwardScan()
		Expect(s.IsReverse()).To(BeFalse())
		Expect(s.CursorStreamingMode).To(Equal(StreamingModeIterator))
	})

	It("ForwardScan has default execute properties", func() {
		s := ForwardScan()
		Expect(s.GetExecuteProperties()).To(Equal(DefaultExecuteProperties()))
	})

	It("ReverseScan is reverse and uses iterator mode", func() {
		s := ReverseScan()
		Expect(s.IsReverse()).To(BeTrue())
		Expect(s.CursorStreamingMode).To(Equal(StreamingModeIterator))
	})

	It("ReverseScan has default execute properties", func() {
		s := ReverseScan()
		Expect(s.GetExecuteProperties()).To(Equal(DefaultExecuteProperties()))
	})

	It("NewScanProperties is forward and inherits streaming mode from execute props", func() {
		ep := DefaultExecuteProperties()
		ep.DefaultCursorStreamingMode = StreamingModeWantAll
		s := NewScanProperties(ep)
		Expect(s.IsReverse()).To(BeFalse())
		Expect(s.CursorStreamingMode).To(Equal(StreamingModeWantAll))
		Expect(s.GetExecuteProperties()).To(Equal(ep))
	})

	It("WithReverse returns immutable copy", func() {
		orig := ForwardScan()
		rev := orig.WithReverse(true)
		Expect(rev.IsReverse()).To(BeTrue())
		Expect(orig.IsReverse()).To(BeFalse())
	})

	It("WithStreamingMode returns immutable copy", func() {
		orig := ForwardScan()
		updated := orig.WithStreamingMode(StreamingModeSerial)
		Expect(updated.CursorStreamingMode).To(Equal(StreamingModeSerial))
		Expect(orig.CursorStreamingMode).To(Equal(StreamingModeIterator))
	})

	It("WithExecuteProperties returns immutable copy", func() {
		orig := ForwardScan()
		ep := DefaultExecuteProperties().WithReturnedRowLimit(99)
		updated := orig.WithExecuteProperties(ep)
		Expect(updated.GetExecuteProperties().ReturnedRowLimit).To(Equal(99))
		Expect(orig.GetExecuteProperties().ReturnedRowLimit).To(Equal(0))
	})

	It("two ForwardScan calls return independent copies", func() {
		a := ForwardScan()
		b := ForwardScan()
		// Mutate a via With* — b must be unaffected
		mutated := a.WithReverse(true).WithStreamingMode(StreamingModeLarge)
		Expect(mutated.IsReverse()).To(BeTrue())
		Expect(b.IsReverse()).To(BeFalse())
		Expect(b.CursorStreamingMode).To(Equal(StreamingModeIterator))
	})

	It("IsReverse accessor matches Reverse field", func() {
		s := ScanProperties{Reverse: true}
		Expect(s.IsReverse()).To(BeTrue())
		s.Reverse = false
		Expect(s.IsReverse()).To(BeFalse())
	})

	It("GetExecuteProperties accessor returns the embedded execute properties", func() {
		ep := DefaultExecuteProperties().WithReturnedRowLimit(7).WithSkip(3)
		s := NewScanProperties(ep)
		Expect(s.GetExecuteProperties().ReturnedRowLimit).To(Equal(7))
		Expect(s.GetExecuteProperties().Skip).To(Equal(3))
	})
})
