package recordlayer

import (
	"bytes"
	"context"
	"time"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Index Scan Unit Tests", func() {
	Describe("TupleRangeAllOf", func() {
		It("returns TupleRangeAll when prefix is nil", func() {
			r := TupleRangeAllOf(nil)
			Expect(r.LowEndpoint).To(Equal(EndpointTypeTreeStart))
			Expect(r.HighEndpoint).To(Equal(EndpointTypeTreeEnd))
			Expect(r.Low).To(BeNil())
			Expect(r.High).To(BeNil())
		})

		It("returns inclusive range when prefix is non-nil", func() {
			r := TupleRangeAllOf(tuple.Tuple{"alice"})
			Expect(r.LowEndpoint).To(Equal(EndpointTypeRangeInclusive))
			Expect(r.HighEndpoint).To(Equal(EndpointTypeRangeInclusive))
			Expect(r.Low).To(Equal(tuple.Tuple{"alice"}))
			Expect(r.High).To(Equal(tuple.Tuple{"alice"}))
		})

		It("handles empty tuple (not nil) as a valid prefix", func() {
			r := TupleRangeAllOf(tuple.Tuple{})
			Expect(r.LowEndpoint).To(Equal(EndpointTypeRangeInclusive))
			Expect(r.HighEndpoint).To(Equal(EndpointTypeRangeInclusive))
			Expect(r.Low).To(HaveLen(0))
		})

		It("handles multi-element prefix", func() {
			r := TupleRangeAllOf(tuple.Tuple{"group1", int64(42)})
			Expect(r.Low).To(Equal(tuple.Tuple{"group1", int64(42)}))
			Expect(r.High).To(Equal(tuple.Tuple{"group1", int64(42)}))
		})
	})

	Describe("TupleRangeBetween", func() {
		It("returns inclusive low, exclusive high", func() {
			r := TupleRangeBetween(tuple.Tuple{int64(10)}, tuple.Tuple{int64(50)})
			Expect(r.LowEndpoint).To(Equal(EndpointTypeRangeInclusive))
			Expect(r.HighEndpoint).To(Equal(EndpointTypeRangeExclusive))
			Expect(r.Low).To(Equal(tuple.Tuple{int64(10)}))
			Expect(r.High).To(Equal(tuple.Tuple{int64(50)}))
		})

		It("handles string keys", func() {
			r := TupleRangeBetween(tuple.Tuple{"aaa"}, tuple.Tuple{"zzz"})
			Expect(r.Low).To(Equal(tuple.Tuple{"aaa"}))
			Expect(r.High).To(Equal(tuple.Tuple{"zzz"}))
		})

		It("handles nil bounds", func() {
			r := TupleRangeBetween(nil, nil)
			Expect(r.Low).To(BeNil())
			Expect(r.High).To(BeNil())
			Expect(r.LowEndpoint).To(Equal(EndpointTypeRangeInclusive))
			Expect(r.HighEndpoint).To(Equal(EndpointTypeRangeExclusive))
		})
	})

	Describe("TupleRangeBetweenInclusive", func() {
		It("returns both-inclusive endpoints", func() {
			r := TupleRangeBetweenInclusive(tuple.Tuple{int64(1)}, tuple.Tuple{int64(99)})
			Expect(r.LowEndpoint).To(Equal(EndpointTypeRangeInclusive))
			Expect(r.HighEndpoint).To(Equal(EndpointTypeRangeInclusive))
			Expect(r.Low).To(Equal(tuple.Tuple{int64(1)}))
			Expect(r.High).To(Equal(tuple.Tuple{int64(99)}))
		})

		It("handles same low and high", func() {
			r := TupleRangeBetweenInclusive(tuple.Tuple{"x"}, tuple.Tuple{"x"})
			Expect(r.Low).To(Equal(tuple.Tuple{"x"}))
			Expect(r.High).To(Equal(tuple.Tuple{"x"}))
		})
	})

	Describe("TupleRangePrefixString", func() {
		It("creates a prefix-string range for a simple token", func() {
			r := TupleRangePrefixString("hello")
			Expect(r.LowEndpoint).To(Equal(EndpointTypePrefixString))
			Expect(r.HighEndpoint).To(Equal(EndpointTypePrefixString))
			Expect(r.Low).To(Equal(tuple.Tuple{"hello"}))
			Expect(r.High).To(Equal(tuple.Tuple{"hello"}))
		})

		It("handles empty string", func() {
			r := TupleRangePrefixString("")
			Expect(r.Low).To(Equal(tuple.Tuple{""}))
			Expect(r.High).To(Equal(tuple.Tuple{""}))
			Expect(r.LowEndpoint).To(Equal(EndpointTypePrefixString))
		})

		It("handles string with special characters", func() {
			r := TupleRangePrefixString("foo\x00bar")
			Expect(r.Low).To(Equal(tuple.Tuple{"foo\x00bar"}))
		})
	})

	Describe("TupleRange.Prepend", func() {
		It("prepends prefix to both low and high", func() {
			r := TupleRangeBetween(tuple.Tuple{int64(10)}, tuple.Tuple{int64(50)})
			prepended := r.Prepend(tuple.Tuple{"group"})
			Expect(prepended.Low).To(Equal(tuple.Tuple{"group", int64(10)}))
			Expect(prepended.High).To(Equal(tuple.Tuple{"group", int64(50)}))
			Expect(prepended.LowEndpoint).To(Equal(EndpointTypeRangeInclusive))
			Expect(prepended.HighEndpoint).To(Equal(EndpointTypeRangeExclusive))
		})

		It("handles nil low/high by returning just the prefix", func() {
			r := TupleRangeAll // Low and High are nil
			prepended := r.Prepend(tuple.Tuple{"pfx"})
			Expect(prepended.Low).To(Equal(tuple.Tuple{"pfx"}))
			Expect(prepended.High).To(Equal(tuple.Tuple{"pfx"}))
			Expect(prepended.LowEndpoint).To(Equal(EndpointTypeTreeStart))
			Expect(prepended.HighEndpoint).To(Equal(EndpointTypeTreeEnd))
		})

		It("preserves endpoint types", func() {
			r := TupleRangePrefixString("token")
			prepended := r.Prepend(tuple.Tuple{int64(7)})
			Expect(prepended.LowEndpoint).To(Equal(EndpointTypePrefixString))
			Expect(prepended.HighEndpoint).To(Equal(EndpointTypePrefixString))
			Expect(prepended.Low).To(Equal(tuple.Tuple{int64(7), "token"}))
			Expect(prepended.High).To(Equal(tuple.Tuple{int64(7), "token"}))
		})

		It("handles multi-element prefix", func() {
			r := TupleRangeAllOf(tuple.Tuple{"val"})
			prepended := r.Prepend(tuple.Tuple{"a", "b"})
			Expect(prepended.Low).To(Equal(tuple.Tuple{"a", "b", "val"}))
			Expect(prepended.High).To(Equal(tuple.Tuple{"a", "b", "val"}))
		})
	})

	Describe("TupleRange.ToFDBRange with PrefixString endpoints", func() {
		It("PrefixString low strips trailing null byte", func() {
			ss := subspace.Sub("test")
			r := TupleRange{
				Low:          tuple.Tuple{"hello"},
				High:         tuple.Tuple{"hello"},
				LowEndpoint:  EndpointTypePrefixString,
				HighEndpoint: EndpointTypeTreeEnd,
			}
			kr := r.ToFDBRange(ss)

			// The packed form of ("hello") ends with a null terminator.
			// PrefixString low strips it.
			packed := ss.Pack(tuple.Tuple{"hello"})
			expectedBegin := packed[:len(packed)-1]
			Expect([]byte(kr.Begin.(fdb.Key))).To(Equal([]byte(expectedBegin)))
		})

		It("PrefixString high strips trailing null and increments", func() {
			ss := subspace.Sub("test")
			r := TupleRange{
				Low:          tuple.Tuple{"hello"},
				High:         tuple.Tuple{"hello"},
				LowEndpoint:  EndpointTypeTreeStart,
				HighEndpoint: EndpointTypePrefixString,
			}
			kr := r.ToFDBRange(ss)

			// The packed string ends with \x00, strip it, then strinc:
			// remove trailing 0xFF bytes, increment last byte.
			packed := ss.Pack(tuple.Tuple{"hello"})
			stripped := packed[:len(packed)-1]
			// "hello" doesn't end in 0xFF, so just increment last byte.
			expected := make([]byte, len(stripped))
			copy(expected, stripped)
			expected[len(expected)-1]++
			Expect([]byte(kr.End.(fdb.Key))).To(Equal(expected))
		})

		It("PrefixString both endpoints (full prefix scan)", func() {
			ss := subspace.Sub("test")
			r := TupleRangePrefixString("abc")
			kr := r.ToFDBRange(ss)

			packed := ss.Pack(tuple.Tuple{"abc"})
			expectedBegin := packed[:len(packed)-1]
			stripped := packed[:len(packed)-1]
			expectedEnd := make([]byte, len(stripped))
			copy(expectedEnd, stripped)
			expectedEnd[len(expectedEnd)-1]++

			Expect([]byte(kr.Begin.(fdb.Key))).To(Equal([]byte(expectedBegin)))
			Expect([]byte(kr.End.(fdb.Key))).To(Equal(expectedEnd))
			// begin < end
			Expect(bytes.Compare(kr.Begin.(fdb.Key), kr.End.(fdb.Key))).To(BeNumerically("<", 0))
		})

		It("PrefixString high with trailing 0xFF bytes strips them", func() {
			ss := subspace.Sub("test")
			// Use a raw string that, when tuple-packed, produces trailing 0xFF before the null terminator.
			// The string "\xff" packs as: 0x02 0xFF 0xFF 0x00 (type byte, escaped 0xFF, null terminator).
			// After stripping the null: 0x02 0xFF 0xFF
			// Then strip trailing 0xFF bytes: leaves 0x02
			// Then increment: 0x03
			r := TupleRange{
				Low:          tuple.Tuple{"\xff"},
				High:         tuple.Tuple{"\xff"},
				LowEndpoint:  EndpointTypeTreeStart,
				HighEndpoint: EndpointTypePrefixString,
			}
			kr := r.ToFDBRange(ss)

			packed := ss.Pack(tuple.Tuple{"\xff"})
			stripped := packed[:len(packed)-1]
			// Remove trailing 0xFF bytes
			newLen := len(stripped)
			for newLen >= 1 && stripped[newLen-1] == 0xFF {
				newLen--
			}
			Expect(newLen).To(BeNumerically(">", 0))
			expected := make([]byte, newLen)
			copy(expected, stripped[:newLen])
			expected[newLen-1]++
			Expect([]byte(kr.End.(fdb.Key))).To(Equal(expected))
		})

		It("default low endpoint falls back to subspace key", func() {
			ss := subspace.Sub("test")
			r := TupleRange{
				Low:          tuple.Tuple{"x"},
				High:         tuple.Tuple{"x"},
				LowEndpoint:  EndpointType(99), // unknown type → default
				HighEndpoint: EndpointTypeTreeEnd,
			}
			kr := r.ToFDBRange(ss)
			Expect([]byte(kr.Begin.(fdb.Key))).To(Equal([]byte(ss.FDBKey())))
		})

		It("default high endpoint falls back to subspace end", func() {
			ss := subspace.Sub("test")
			r := TupleRange{
				Low:          tuple.Tuple{"x"},
				High:         tuple.Tuple{"x"},
				LowEndpoint:  EndpointTypeTreeStart,
				HighEndpoint: EndpointType(99), // unknown type → default
			}
			kr := r.ToFDBRange(ss)
			_, expectedEnd := ss.FDBRangeKeys()
			Expect([]byte(kr.End.(fdb.Key))).To(Equal([]byte(expectedEnd.FDBKey())))
		})
	})

	Describe("IndexEntry", func() {
		Describe("PrimaryKey", func() {
			It("returns empty tuple when Index is nil", func() {
				entry := &IndexEntry{Key: tuple.Tuple{"a", int64(1)}}
				Expect(entry.PrimaryKey()).To(Equal(tuple.Tuple{}))
			})

			It("extracts PK from tail of key (no dedup)", func() {
				idx := NewIndex("test_idx", Field("price"))
				entry := &IndexEntry{
					Index: idx,
					Key:   tuple.Tuple{int64(100), int64(42)}, // [indexedValue, pk]
				}
				pk := entry.PrimaryKey()
				Expect(pk).To(Equal(tuple.Tuple{int64(42)}))
			})

			It("caches the primary key on second call", func() {
				idx := NewIndex("test_idx", Field("price"))
				entry := &IndexEntry{
					Index: idx,
					Key:   tuple.Tuple{int64(100), int64(42)},
				}
				pk1 := entry.PrimaryKey()
				pk2 := entry.PrimaryKey()
				Expect(pk1).To(Equal(pk2))
			})

			It("returns empty tuple when key has no PK portion", func() {
				idx := NewIndex("test_idx", Field("price"))
				entry := &IndexEntry{
					Index: idx,
					Key:   tuple.Tuple{int64(100)}, // only index value, no PK
				}
				pk := entry.PrimaryKey()
				Expect(pk).To(Equal(tuple.Tuple{}))
			})

			It("extracts PK from composite index", func() {
				idx := NewIndex("test_idx", Concat(Field("a"), Field("b")))
				entry := &IndexEntry{
					Index: idx,
					Key:   tuple.Tuple{"x", "y", int64(7)}, // [a, b, pk]
				}
				pk := entry.PrimaryKey()
				Expect(pk).To(Equal(tuple.Tuple{int64(7)}))
			})
		})

		Describe("IndexValues", func() {
			It("returns empty tuple when Index is nil", func() {
				entry := &IndexEntry{Key: tuple.Tuple{"a", int64(1)}}
				Expect(entry.IndexValues()).To(Equal(tuple.Tuple{}))
			})

			It("returns first colSize elements", func() {
				idx := NewIndex("test_idx", Concat(Field("a"), Field("b")))
				entry := &IndexEntry{
					Index: idx,
					Key:   tuple.Tuple{"x", "y", int64(7)}, // [a, b, pk]
				}
				Expect(entry.IndexValues()).To(Equal(tuple.Tuple{"x", "y"}))
			})

			It("returns full key when key is shorter than colSize", func() {
				idx := NewIndex("test_idx", Concat(Field("a"), Field("b"), Field("c")))
				entry := &IndexEntry{
					Index: idx,
					Key:   tuple.Tuple{"x"}, // shorter than colSize (3)
				}
				Expect(entry.IndexValues()).To(Equal(tuple.Tuple{"x"}))
			})

			It("returns single element for single-field index", func() {
				idx := NewIndex("test_idx", Field("price"))
				entry := &IndexEntry{
					Index: idx,
					Key:   tuple.Tuple{int64(100), int64(42)},
				}
				Expect(entry.IndexValues()).To(Equal(tuple.Tuple{int64(100)}))
			})
		})
	})

	Describe("wrapContinuation and unwrapContinuation", func() {
		It("round-trips a suffix through the TO_NEW proto wrapper", func() {
			inner := []byte{0x01, 0x02, 0x03, 0x04}
			wrapped, err := wrapContinuation(inner)
			Expect(err).NotTo(HaveOccurred())
			unwrapped := unwrapContinuation(wrapped)
			Expect(unwrapped).To(Equal(inner))
		})

		It("handles nil input", func() {
			wrapped, err := wrapContinuation(nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(wrapped).To(BeNil())
			unwrapped := unwrapContinuation(nil)
			Expect(unwrapped).To(BeNil())
		})

		It("handles empty input", func() {
			wrapped, err := wrapContinuation([]byte{})
			Expect(err).NotTo(HaveOccurred())
			unwrapped := unwrapContinuation(wrapped)
			Expect(unwrapped).To(HaveLen(0))
		})

		It("unwraps proto-wrapped continuation with magic number", func() {
			inner := []byte{0xAA, 0xBB, 0xCC}
			magic := int64(6_773_487_359_078_157_740)
			msg := &gen.KeyValueCursorContinuation{
				InnerContinuation: inner,
				MagicNumber:       &magic,
			}
			data, err := msg.MarshalVT()
			Expect(err).NotTo(HaveOccurred())

			unwrapped := unwrapContinuation(data)
			Expect(unwrapped).To(Equal(inner))
		})

		It("treats proto without magic number as raw bytes", func() {
			// If the magic number doesn't match, fall back to raw.
			wrongMagic := int64(12345)
			msg := &gen.KeyValueCursorContinuation{
				InnerContinuation: []byte{0x01},
				MagicNumber:       &wrongMagic,
			}
			data, err := msg.MarshalVT()
			Expect(err).NotTo(HaveOccurred())

			// Should return the original bytes since magic doesn't match.
			unwrapped := unwrapContinuation(data)
			Expect(unwrapped).To(Equal(data))
		})

		It("treats proto without magic field as raw bytes", func() {
			msg := &gen.KeyValueCursorContinuation{
				InnerContinuation: []byte{0x01, 0x02},
				// no MagicNumber set
			}
			data, err := msg.MarshalVT()
			Expect(err).NotTo(HaveOccurred())

			unwrapped := unwrapContinuation(data)
			Expect(unwrapped).To(Equal(data))
		})

		It("treats garbage bytes as raw continuation", func() {
			garbage := []byte{0xFF, 0xFE, 0xFD, 0xFC, 0xFB}
			unwrapped := unwrapContinuation(garbage)
			Expect(unwrapped).To(Equal(garbage))
		})
	})

	Describe("indexCursor.limitContinuation", func() {
		It("returns StartContinuation when no lastCont", func() {
			c := &indexCursor{}
			cont := c.limitContinuation()
			Expect(cont.IsEnd()).To(BeFalse())
			Expect(cont.ToBytes()).To(BeNil())
		})

		It("returns BytesContinuation when lastCont is set", func() {
			c := &indexCursor{lastCont: []byte{0x01, 0x02}}
			cont := c.limitContinuation()
			Expect(cont.ToBytes()).To(Equal([]byte{0x01, 0x02}))
		})
	})

	Describe("indexCursor.makeContinuation", func() {
		It("extracts key suffix past prefix length", func() {
			ss := subspace.Sub("idx")
			c := &indexCursor{
				indexSubspace: ss,
				prefixLength:  len(ss.FDBKey()),
			}
			fullKey := ss.Pack(tuple.Tuple{int64(42)})
			cont, err := c.makeContinuation(fullKey)
			Expect(err).NotTo(HaveOccurred())
			// TO_NEW proto-wrapped (Java 4.11.1.0 default): the packed tuple suffix
			// (without subspace prefix) round-trips back through the dual-reader.
			suffix := []byte(fullKey[len(ss.FDBKey()):])
			Expect(unwrapContinuation(cont)).To(Equal(suffix))
		})

		It("uses full key when key is shorter than prefix", func() {
			ss := subspace.Sub("idx")
			c := &indexCursor{
				indexSubspace: ss,
				prefixLength:  len(ss.FDBKey()),
			}
			shortKey := fdb.Key{0x01}
			cont, err := c.makeContinuation(shortKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(unwrapContinuation(cont)).To(Equal([]byte(shortKey)))
		})
	})

	Describe("indexCursor.unpackKeyValue", func() {
		It("unpacks a key-value pair into an IndexEntry", func() {
			ss := subspace.Sub("idx")
			idx := NewIndex("test_idx", Field("price"))
			c := &indexCursor{
				index:         idx,
				indexSubspace: ss,
			}
			key := ss.Pack(tuple.Tuple{int64(100), int64(42)})
			kv := fdb.KeyValue{Key: key, Value: tuple.Tuple{}.Pack()}
			entry, err := c.unpackKeyValue(kv)
			Expect(err).NotTo(HaveOccurred())
			Expect(entry.Key).To(Equal(tuple.Tuple{int64(100), int64(42)}))
			Expect(entry.Value).To(HaveLen(0))
			Expect(entry.Index).To(Equal(idx))
		})

		It("unpacks non-empty value", func() {
			ss := subspace.Sub("idx")
			idx := NewIndex("test_idx", Field("price"))
			c := &indexCursor{
				index:         idx,
				indexSubspace: ss,
			}
			key := ss.Pack(tuple.Tuple{int64(100)})
			val := tuple.Tuple{"extra", int64(7)}.Pack()
			kv := fdb.KeyValue{Key: key, Value: val}
			entry, err := c.unpackKeyValue(kv)
			Expect(err).NotTo(HaveOccurred())
			Expect(entry.Key).To(Equal(tuple.Tuple{int64(100)}))
			Expect(entry.Value).To(Equal(tuple.Tuple{"extra", int64(7)}))
		})

		It("returns error when key is shorter than subspace prefix", func() {
			ss := subspace.Sub("idx")
			idx := NewIndex("test_idx", Field("price"))
			c := &indexCursor{
				index:         idx,
				indexSubspace: ss,
			}
			kv := fdb.KeyValue{Key: fdb.Key{0x01}, Value: nil}
			_, err := c.unpackKeyValue(kv)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("index key shorter than subspace prefix"))
		})

		It("returns error when key suffix is invalid tuple", func() {
			ss := subspace.Sub("idx")
			idx := NewIndex("test_idx", Field("price"))
			c := &indexCursor{
				index:         idx,
				indexSubspace: ss,
			}
			// Create a key with valid prefix but invalid tuple bytes after it.
			badKey := append([]byte(nil), ss.FDBKey()...)
			badKey = append(badKey, 0xFF, 0xFE, 0xFD) // invalid tuple encoding
			kv := fdb.KeyValue{Key: badKey, Value: nil}
			_, err := c.unpackKeyValue(kv)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unpack index key"))
		})

		It("returns error when value is invalid tuple", func() {
			ss := subspace.Sub("idx")
			idx := NewIndex("test_idx", Field("price"))
			c := &indexCursor{
				index:         idx,
				indexSubspace: ss,
			}
			key := ss.Pack(tuple.Tuple{int64(100)})
			kv := fdb.KeyValue{Key: key, Value: []byte{0xFF, 0xFE, 0xFD}} // invalid tuple
			_, err := c.unpackKeyValue(kv)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unpack index value"))
		})

		It("handles empty value bytes", func() {
			ss := subspace.Sub("idx")
			idx := NewIndex("test_idx", Field("price"))
			c := &indexCursor{
				index:         idx,
				indexSubspace: ss,
			}
			key := ss.Pack(tuple.Tuple{int64(100)})
			kv := fdb.KeyValue{Key: key, Value: nil}
			entry, err := c.unpackKeyValue(kv)
			Expect(err).NotTo(HaveOccurred())
			Expect(entry.Value).To(BeNil())
		})
	})

	Describe("indexCursor.Close", func() {
		It("marks cursor as closed", func() {
			c := &indexCursor{}
			Expect(c.closed).To(BeFalse())
			err := c.Close()
			Expect(err).NotTo(HaveOccurred())
			Expect(c.closed).To(BeTrue())
		})
	})

	Describe("indexCursor.OnNext context cancellation (RFC-106a)", func() {
		// The statement deadline (a Go-only read-path extension) reaches the
		// secondary-index scan via the ctx passed to OnNext. Before the fix the
		// index cursor ignored ctx (signature was OnNext(_ context.Context)), so
		// a cancelled/expired statement kept draining to the per-page time limit.
		// The check sits before initIterator, so a zero cursor with a cancelled
		// ctx returns the ctx error without touching FDB. Revert-proof: drop the
		// check and OnNext falls through to initIterator, nil-derefing c.store.
		It("returns the ctx error before touching the iterator", func() {
			c := &indexCursor{} // not closed; iterator nil
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := c.OnNext(ctx)
			Expect(err).To(Equal(context.Canceled))
		})

		It("propagates a deadline-exceeded ctx (→ 54F01 statement timeout)", func() {
			c := &indexCursor{}
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
			defer cancel()
			_, err := c.OnNext(ctx)
			Expect(err).To(Equal(context.DeadlineExceeded))
		})
	})

	// The remaining specialized leaf cursors (count/aggregate, text, bitmap,
	// vector) also ignored ctx before RFC-106a, so a statement deadline could not
	// bound their scans. Each now checks ctx.Err() at the top of OnNext. A zero
	// cursor + cancelled ctx exercises that check with no FDB; revert-proof: drop
	// the check and OnNext either nil-derefs its (nil) iterator/tx or returns
	// SourceExhausted instead of the ctx error.
	Describe("specialized leaf cursors honor ctx cancellation (RFC-106a)", func() {
		It("countKVCursor.OnNext returns the ctx error", func() {
			c := &countKVCursor{}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := c.OnNext(ctx)
			Expect(err).To(Equal(context.Canceled))
		})
		It("textCursor.OnNext returns the ctx error", func() {
			c := &textCursor{}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := c.OnNext(ctx)
			Expect(err).To(Equal(context.Canceled))
		})
		It("bitmapKVCursor.OnNext returns the ctx error", func() {
			c := &bitmapKVCursor{}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := c.OnNext(ctx)
			Expect(err).To(Equal(context.Canceled))
		})
		It("vectorSearchCursor.OnNext returns the ctx error", func() {
			c := &vectorSearchCursor{}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := c.OnNext(ctx)
			Expect(err).To(Equal(context.Canceled))
		})
		It("rtreeScanCursor.OnNext returns the ctx error", func() {
			c := &rtreeScanCursor{}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := c.OnNext(ctx)
			Expect(err).To(Equal(context.Canceled))
		})
		It("prefixSkipScanCursor.OnNext returns the ctx error", func() {
			c := &prefixSkipScanCursor{}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := c.OnNext(ctx)
			Expect(err).To(Equal(context.Canceled))
		})
	})

	Describe("newIndexCursor", func() {
		It("initializes with correct defaults", func() {
			ss := subspace.Sub("idx")
			idx := NewIndex("test_idx", Field("price"))
			r := TupleRangeAll
			props := ForwardScan()
			cont := []byte{0x01}

			var tx fdb.WritableTransaction
			c := newIndexCursor(idx, ss, tx, r, cont, props)
			Expect(c.index).To(Equal(idx))
			Expect(c.indexSubspace).To(Equal(ss))
			Expect(c.tupleRange).To(Equal(r))
			Expect(c.continuation).To(Equal(cont))
			Expect(c.scanProps).To(Equal(props))
			Expect(c.prefixLength).To(Equal(len(ss.FDBKey())))
			Expect(c.closed).To(BeFalse())
			Expect(c.recordsRead).To(Equal(0))
			Expect(c.bytesScanned).To(Equal(int64(0)))
		})
	})

	Describe("ChainedCursor", func() {
		It("returns exhausted after Close", func() {
			counter := 0
			cursor := Chained[int](
				func(prev *int) (*int, error) {
					counter++
					v := counter
					return &v, nil
				},
				nil, nil, nil,
			)

			// First call should return a value.
			ctx := context.Background()
			r1, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r1.HasNext()).To(BeTrue())
			Expect(r1.GetValue()).To(Equal(1))

			// Close the cursor.
			Expect(cursor.Close()).NotTo(HaveOccurred())

			// After Close, OnNext should return no-next with SourceExhausted.
			r2, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r2.HasNext()).To(BeFalse())
			Expect(r2.GetNoNextReason()).To(Equal(SourceExhausted))
		})
	})
})
