package recordlayer

import (
	"bytes"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// TestComputeRecordsRange pins the typed-records build-range preset bounds (RFC-139).
// The byte-exact assertions are the wire-critical part: Java's
// TupleRange.betweenInclusive(low, high).toRange() yields begin = low.Pack() VERBATIM and
// end = high.Pack()+0xff (RANGE_INCLUSIVE high appends one 0xff, NOT strinc). A strinc impl
// would write different range-set bytes — a wire divergence even though both cover the same
// real PKs for integer record-type keys.
func TestComputeRecordsRange(t *testing.T) {
	t.Parallel()

	rtPrefixedPK := Concat(RecordTypeKey(), Field("id"))
	mdTyped := func() *RecordMetaData {
		return &RecordMetaData{
			recordTypes: map[string]*RecordType{
				"A": {Name: "A", PrimaryKey: rtPrefixedPK, explicitRecordTypeKey: int64(1)},
				"B": {Name: "B", PrimaryKey: rtPrefixedPK, explicitRecordTypeKey: int64(2)},
			},
		}
	}

	t.Run("byte-exact bounds for a typed subset (begin verbatim, end +0xff)", func(t *testing.T) {
		t.Parallel()
		oi := &OnlineIndexer{metaData: mdTyped(), recordTypes: []string{"A", "B"}}
		begin, end, ok := oi.computeRecordsRange()
		if !ok {
			t.Fatal("expected ok=true for a typed subset")
		}
		wantBegin := tuple.Tuple{int64(1)}.Pack()
		wantEnd := append(tuple.Tuple{int64(2)}.Pack(), 0xff)
		if !bytes.Equal(begin, wantBegin) {
			t.Errorf("begin = %x, want %x (low.Pack() verbatim, NOT strinc)", begin, wantBegin)
		}
		if !bytes.Equal(end, wantEnd) {
			t.Errorf("end = %x, want %x (high.Pack()+0xff, NOT strinc)", end, wantEnd)
		}
		if end[len(end)-1] != 0xff {
			t.Errorf("end must terminate with 0xff (RANGE_INCLUSIVE high), got %02x", end[len(end)-1])
		}
	})

	t.Run("single type uses that type key for both bounds", func(t *testing.T) {
		t.Parallel()
		oi := &OnlineIndexer{metaData: mdTyped(), recordTypes: []string{"A"}}
		begin, end, ok := oi.computeRecordsRange()
		if !ok {
			t.Fatal("expected ok=true")
		}
		if !bytes.Equal(begin, tuple.Tuple{int64(1)}.Pack()) {
			t.Errorf("begin = %x", begin)
		}
		if !bytes.Equal(end, append(tuple.Tuple{int64(1)}.Pack(), 0xff)) {
			t.Errorf("end = %x", end)
		}
	})

	t.Run("give up (ok=false) when a type lacks the record-type prefix", func(t *testing.T) {
		t.Parallel()
		md := mdTyped()
		md.recordTypes["B"].PrimaryKey = Field("id") // no record-type prefix
		oi := &OnlineIndexer{metaData: md, recordTypes: []string{"A", "B"}}
		if _, _, ok := oi.computeRecordsRange(); ok {
			t.Error("expected ok=false when a type lacks the record-type prefix")
		}
	})

	t.Run("give up (ok=false) when no indexed types resolve", func(t *testing.T) {
		t.Parallel()
		oi := &OnlineIndexer{metaData: mdTyped(), recordTypes: []string{"NoSuchType"}}
		if _, _, ok := oi.computeRecordsRange(); ok {
			t.Error("expected ok=false when no indexed types resolve")
		}
	})

	t.Run("give up (ok=false) for a non-integer explicit record-type key", func(t *testing.T) {
		t.Parallel()
		// Go's RecordTypeKeyExpression encodes only integer record-type keys; a string/bytes
		// key is stored under the message type name, so bounds from GetRecordTypeKey() would
		// not match the records — the preset must give up rather than skip them.
		md := mdTyped()
		md.recordTypes["B"].explicitRecordTypeKey = "string-key"
		oi := &OnlineIndexer{metaData: md, recordTypes: []string{"A", "B"}}
		if _, _, ok := oi.computeRecordsRange(); ok {
			t.Error("expected ok=false for a non-integer record-type key")
		}
	})

	t.Run("int32 record-type key is normalized to int64 (no panic, matches placement)", func(t *testing.T) {
		t.Parallel()
		// SetRecordTypeKey(int32(...)) is valid; the tuple encoder panics on a raw int32, and
		// bindTypeKeys stores int64, so the preset must normalize to int64 — matching where
		// records are actually stored.
		md := mdTyped()
		md.recordTypes["A"].explicitRecordTypeKey = int32(1)
		md.recordTypes["B"].explicitRecordTypeKey = int32(2)
		oi := &OnlineIndexer{metaData: md, recordTypes: []string{"A", "B"}}
		begin, end, ok := oi.computeRecordsRange()
		if !ok {
			t.Fatal("expected ok=true for int32 keys")
		}
		if !bytes.Equal(begin, tuple.Tuple{int64(1)}.Pack()) {
			t.Errorf("begin = %x, want int64(1) pack %x", begin, tuple.Tuple{int64(1)}.Pack())
		}
		if !bytes.Equal(end, append(tuple.Tuple{int64(2)}.Pack(), 0xff)) {
			t.Errorf("end = %x, want int64(2) pack +0xff", end)
		}
	})
}

// TestRecordTypePrimaryKeyPredicates pins the two RecordType predicates the preset needs.
func TestRecordTypePrimaryKeyPredicates(t *testing.T) {
	t.Parallel()

	withPrefix := &RecordType{PrimaryKey: Concat(RecordTypeKey(), Field("id"))}
	if !withPrefix.PrimaryKeyHasRecordTypePrefix() {
		t.Error("Concat(RecordTypeKey(), ...) should have a record-type prefix")
	}
	if withPrefix.IsSynthetic() {
		t.Error("Go has no synthetic record types; IsSynthetic must be false")
	}

	rtkOnly := &RecordType{PrimaryKey: RecordTypeKey()}
	if !rtkOnly.PrimaryKeyHasRecordTypePrefix() {
		t.Error("a bare RecordTypeKey() PK should count as having the prefix")
	}

	noPrefix := &RecordType{PrimaryKey: Field("id")}
	if noPrefix.PrimaryKeyHasRecordTypePrefix() {
		t.Error("Field(\"id\") should NOT have a record-type prefix")
	}
}

// TestUnpackRangeEndBoundary pins how the build loop consumes range-set end boundaries:
// a normal packed tuple is EXCLUSIVE; a tuple+0xff (the typed-records preset's
// RANGE_INCLUSIVE-high bound, not itself a valid tuple) is INCLUSIVE of the stripped tuple.
func TestUnpackRangeEndBoundary(t *testing.T) {
	t.Parallel()

	normal := tuple.Tuple{int64(5)}.Pack()
	if tup, ep, err := unpackRangeEndBoundary(normal); err != nil || ep != EndpointTypeRangeExclusive || !bytes.Equal(tup.Pack(), normal) {
		t.Errorf("normal tuple: got (%x, %v, %v), want EXCLUSIVE of %x", safePack(tup), ep, err, normal)
	}

	high := tuple.Tuple{int64(5)}.Pack()
	bound := append(append([]byte(nil), high...), 0xff)
	if tup, ep, err := unpackRangeEndBoundary(bound); err != nil || ep != EndpointTypeRangeInclusive || !bytes.Equal(tup.Pack(), high) {
		t.Errorf("tuple+0xff: got (%x, %v, %v), want INCLUSIVE of %x", safePack(tup), ep, err, high)
	}

	// int 255 packs to 0x15 0xff — a VALID tuple that ends in 0xff. It must be treated as
	// that key (EXCLUSIVE), NOT as a +0xff bound: the whole-tuple unpack is tried first.
	i255 := tuple.Tuple{int64(255)}.Pack()
	if tup, ep, err := unpackRangeEndBoundary(i255); err != nil || ep != EndpointTypeRangeExclusive || !bytes.Equal(tup.Pack(), i255) {
		t.Errorf("int255 (0x..ff): got (%x, %v, %v), want EXCLUSIVE of %x", safePack(tup), ep, err, i255)
	}
}

func safePack(t tuple.Tuple) []byte {
	if t == nil {
		return nil
	}
	return t.Pack()
}
