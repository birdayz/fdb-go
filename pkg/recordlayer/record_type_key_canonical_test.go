package recordlayer

import (
	"errors"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// A record type key is read by four subsystems that each ask it a different
// question: duplicate-key validation asks whether two keys are the same, proto
// serialization asks whether the value is representable, DeleteRecordsWhere's
// prefix comparison asks whether a caller's value selects this type, and the
// tuple packer asks for its bytes. Canonicalizing at the setter — Java's
// placement, RecordTypeIndexesBuilder.setRecordTypeKey — is what keeps those
// four answers consistent. These tests pin each answer against the shape that
// was wrong when the canonical form lived on the read path instead.

// keyMetaData builds metadata with record-type-prefixed primary keys and the
// given explicit record type keys, returning the Build error unchanged so the
// caller can assert on it.
func keyMetaData(keys map[string]any) (*RecordMetaData, error) {
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
	b.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("id")))
	for name, key := range keys {
		b.GetRecordType(name).SetRecordTypeKey(key)
	}
	return b.Build()
}

// TestRecordTypeKey_EqualEncodingsAreDuplicates pins that two record types
// whose keys ENCODE identically are rejected at Build, whatever Go types the
// caller spelled them with.
//
// int64(7) and uint(7) are different values, so a seen-set keyed on the value
// accepted both — and the tuple encoder then wrote the same bytes for each.
// With record-type-prefixed primary keys that puts two record types in one key
// space: saving an Order at id 1 overwrites the Customer at id 1, silently,
// with no error at any layer.
func TestRecordTypeKey_EqualEncodingsAreDuplicates(t *testing.T) {
	t.Parallel()

	// The premise the whole test rests on: these two spellings are one key on
	// the wire. If this ever stops holding, the duplicate rule below is
	// testing something else.
	signed := tuple.Tuple{int64(7)}.Pack()
	unsigned := tuple.Tuple{uint(7)}.Pack()
	if string(signed) != string(unsigned) {
		t.Fatalf("precondition: int64(7) and uint(7) must encode identically, got %x and %x", signed, unsigned)
	}

	for _, tc := range []struct {
		name       string
		first      any
		second     any
		duplicates bool
	}{
		{"int64 vs uint", int64(7), uint(7), true},
		{"int64 vs uint64", int64(7), uint64(7), true},
		{"int vs int32", 7, int32(7), true},
		{"int64 vs uint8", int64(7), uint8(7), true},
		// Distinct keys must still be accepted — a duplicate rule that
		// rejects everything would pass the assertion above for free.
		{"different integers", int64(7), int64(8), false},
		// A string and the same bytes are NOT duplicates: the tuple encoding
		// gives them different type codes, so they occupy different key
		// spaces. Folding bytes into a string to make them comparable would
		// reject this valid metadata.
		{"string vs same bytes", "k", []byte("k"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := keyMetaData(map[string]any{"Order": tc.first, "Customer": tc.second})
			if tc.duplicates {
				var mdErr *MetaDataError
				if !errors.As(err, &mdErr) {
					t.Fatalf("record type keys %v (%T) and %v (%T) encode to identical bytes but Build accepted them — "+
						"two record types share one record-type-prefixed key space and a save silently overwrites the other type's record; got err=%v",
						tc.first, tc.first, tc.second, tc.second, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("record type keys %v (%T) and %v (%T) encode differently but Build rejected them: %v",
					tc.first, tc.first, tc.second, tc.second, err)
			}
		})
	}
}

// TestRecordTypeKey_NonEncodableRejectedAtBuild pins that a key the tuple
// encoder cannot write is refused by Build with a typed error.
//
// These values are comparable, so they passed validation, and they are not
// among the encoder's cases, so the first save PANICKED with "unencodable
// element" — a panic in library code, from metadata the builder accepted
// without complaint. Java refuses the same class of value at the setter
// (MetaDataException "Only primitive types are allowed as record type key").
func TestRecordTypeKey_NonEncodableRejectedAtBuild(t *testing.T) {
	t.Parallel()

	type namedInt int

	for _, tc := range []struct {
		name string
		key  any
	}{
		{"named integer type", namedInt(7)},
		{"complex", complex64(complex(1, 2))},
		{"struct", struct{ A int }{1}},
		{"slice of int", []int{1, 2}},
		{"map", map[string]int{"a": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := keyMetaData(map[string]any{"Order": tc.key})
			var typeErr *RecordTypeKeyTypeError
			if !errors.As(err, &typeErr) {
				t.Fatalf("Build accepted record type key %v (%T) that the tuple encoder cannot write — "+
					"it panics at pack time on the save path instead; got err=%v", tc.key, tc.key, err)
			}
		})
	}

	// The accepted set must not have been narrowed into uselessness. The
	// integers are chosen above the derived record type keys of the other
	// types in this schema, so a rejection here is about the TYPE, never an
	// accidental duplicate.
	for _, key := range []any{
		int8(101), int16(102), int32(103), int64(104), 105,
		uint8(106), uint16(107), uint32(108), uint(109), uint64(110),
		"k", []byte("k"), true, float32(1.5), float64(2.5),
	} {
		md, err := keyMetaData(map[string]any{"Order": key})
		if err != nil {
			t.Fatalf("Build rejected encodable record type key %v (%T): %v", key, key, err)
		}
		// And what it stored must actually pack.
		got := md.GetRecordType("Order").GetRecordTypeKey()
		if _, ok := encodableKeyBytes(got); !ok {
			t.Fatalf("record type key %v (%T) was stored as %v (%T), which the tuple encoder cannot write",
				key, key, got, got)
		}
	}
}

// TestRecordTypeKey_NarrowIntegersRoundTripThroughProto pins that a key the
// save path accepts also survives metadata export.
//
// int8/int16/uint32 worked on the save path (they were widened when the key
// was evaluated) but ToProto sent the RAW stored value to valueToProto, which
// has no case for them — so exporting or persisting otherwise-working metadata
// failed. Storing the canonical value is what makes the two paths agree.
func TestRecordTypeKey_NarrowIntegersRoundTripThroughProto(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		key  any
		want any
	}{
		{"int8", int8(42), int64(42)},
		{"int16", int16(42), int64(42)},
		{"int32", int32(42), int64(42)},
		{"int", 42, int64(42)},
		{"uint8", uint8(42), int64(42)},
		{"uint16", uint16(42), int64(42)},
		{"uint32", uint32(42), int64(42)},
		{"uint", uint(42), int64(42)},
		{"uint64", uint64(42), int64(42)},
		{"string", "forty-two", "forty-two"},
		{"bool", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			md, err := keyMetaData(map[string]any{"Order": tc.key})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if got := md.GetRecordType("Order").GetRecordTypeKey(); got != tc.want {
				t.Fatalf("stored record type key = %v (%T), want %v (%T)", got, got, tc.want, tc.want)
			}

			mdProto, err := md.ToProto()
			if err != nil {
				t.Fatalf("ToProto rejected metadata whose record type key the save path accepts: %v", err)
			}
			back, err := RecordMetaDataFromProto(mdProto)
			if err != nil {
				t.Fatalf("RecordMetaDataFromProto: %v", err)
			}
			if got := back.GetRecordType("Order").GetRecordTypeKey(); got != tc.want {
				t.Fatalf("record type key after proto round-trip = %v (%T), want %v (%T)", got, got, tc.want, tc.want)
			}
		})
	}

	// Bytes keys round-trip too, compared by value since []byte is not
	// comparable.
	md, err := keyMetaData(map[string]any{"Order": []byte{0x01, 0x02}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	mdProto, err := md.ToProto()
	if err != nil {
		t.Fatalf("ToProto: %v", err)
	}
	back, err := RecordMetaDataFromProto(mdProto)
	if err != nil {
		t.Fatalf("RecordMetaDataFromProto: %v", err)
	}
	got, ok := back.GetRecordType("Order").GetRecordTypeKey().([]byte)
	if !ok || string(got) != "\x01\x02" {
		t.Fatalf("bytes record type key after proto round-trip = %v (%T), want [1 2]", got, got)
	}
}

// TestRecordTypeKey_SetterCopiesBytes pins that the metadata does not alias the
// caller's slice: a key that changed under the metadata would move every
// record of that type to a different key space without a single write.
// Java's setter copies for the same reason (ByteString.copyFrom).
func TestRecordTypeKey_SetterCopiesBytes(t *testing.T) {
	t.Parallel()

	raw := []byte{0x01, 0x02}
	md, err := keyMetaData(map[string]any{"Order": raw})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	raw[0] = 0xFF

	got, ok := md.GetRecordType("Order").GetRecordTypeKey().([]byte)
	if !ok || string(got) != "\x01\x02" {
		t.Fatalf("record type key aliased the caller's slice: now %v", got)
	}
}

// TestRecordTypeKeyEquals_BytesKey pins that comparing a prefix value against a
// bytes record type key answers, rather than panicking.
//
// The comparison was `prefixVal == typeKey` on two interfaces, which panics
// ("comparing uncomparable type []uint8") as soon as either side holds a byte
// slice — reachable from an ordinary DeleteRecordsWhere on metadata with an
// explicit bytes key.
func TestRecordTypeKeyEquals_BytesKey(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		prefix any
		key    any
		want   bool
	}{
		{"equal bytes", []byte{0x01, 0x02}, []byte{0x01, 0x02}, true},
		{"different bytes", []byte{0x01, 0x02}, []byte{0x01, 0x03}, false},
		{"bytes prefix vs int key", []byte{0x01}, int64(1), false},
		{"int prefix vs bytes key", int64(1), []byte{0x01}, false},
		// A tuple-decoded prefix is int64; a key may be spelled int. Their
		// encodings agree, so they must match.
		{"int64 prefix vs int key", int64(3), 3, true},
		{"int64 prefix vs uint key", int64(3), uint(3), true},
		{"string prefix vs same bytes key", "k", []byte("k"), false},
		{"unencodable prefix", struct{ A int }{1}, int64(1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := recordTypeKeyEquals(tc.prefix, tc.key); got != tc.want {
				t.Fatalf("recordTypeKeyEquals(%v (%T), %v (%T)) = %v, want %v",
					tc.prefix, tc.prefix, tc.key, tc.key, got, tc.want)
			}
		})
	}
}

// TestRecordTypeKey_BytesKeyDeleteWhereSelectsTheType pins the reachable path
// end to end at the metadata level: with a bytes record type key, resolving
// which record types a DeleteRecordsWhere prefix covers must select exactly
// the matching type — the call that panicked.
func TestRecordTypeKey_BytesKeyDeleteWhereSelectsTheType(t *testing.T) {
	t.Parallel()

	md, err := keyMetaData(map[string]any{
		"Order":    []byte{0x0A},
		"Customer": []byte{0x0B},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	store := &FDBRecordStore{metaData: md}
	names := store.findMatchingRecordTypes(tuple.Tuple{[]byte{0x0A}})
	if len(names) != 1 || names[0] != "Order" {
		t.Fatalf("prefix []byte{0x0A} selected %v, want exactly [Order] — "+
			"a bytes record type key must be selectable by DeleteRecordsWhere, not panic on comparison", names)
	}

	if other := store.findMatchingRecordTypes(tuple.Tuple{[]byte{0x0B}}); len(other) != 1 || other[0] != "Customer" {
		t.Fatalf("prefix []byte{0x0B} selected %v, want exactly [Customer]", other)
	}
	if none := store.findMatchingRecordTypes(tuple.Tuple{[]byte{0x0C}}); len(none) != 0 {
		t.Fatalf("prefix []byte{0x0C} matches no record type key but selected %v", none)
	}
}

// TestRecordTypeKey_CanonicalKeyPacksIdenticalBytes pins the property that
// makes canonicalization safe at all: it must never change the bytes a key
// encodes to. If it did, canonicalizing would silently move existing records.
//
// The reference is an ENCODABLE spelling of the same value, because half the
// accepted types cannot be packed as they arrive — tuple.Tuple{int8(1)}.Pack()
// panics, which is precisely why the canonical form has to widen them. So the
// claim is: canonicalizing preserves the value's encoding, and for the types
// the encoder already handles it is a no-op on the bytes.
func TestRecordTypeKey_CanonicalKeyPacksIdenticalBytes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		key any
		// reference is a spelling of the same value the tuple encoder
		// accepts directly; the canonical form must pack to the same bytes.
		reference any
	}{
		{int8(1), int64(1)},
		{int16(2), int64(2)},
		{int32(3), int64(3)},
		{int64(4), int64(4)},
		{5, 5},
		{uint8(6), int64(6)},
		{uint16(7), int64(7)},
		{uint32(8), int64(8)},
		{uint(9), uint(9)},
		{uint64(10), uint64(10)},
		{int64(-1), int64(-1)},
		{-2, -2},
		{int8(-3), int64(-3)},
		{uint64(1) << 63, uint64(1) << 63}, // above max int64: must stay unsigned
		{"k", "k"},
		{[]byte("k"), []byte("k")},
		{true, true},
		{false, false},
		{float32(1.5), float32(1.5)},
		{float64(2.5), float64(2.5)},
	} {
		canonical, err := canonicalRecordTypeKey(tc.key)
		if err != nil {
			t.Fatalf("canonicalRecordTypeKey(%v (%T)): %v", tc.key, tc.key, err)
		}
		want := tuple.Tuple{tc.reference}.Pack()
		got, ok := encodableKeyBytes(canonical)
		if !ok {
			t.Fatalf("canonicalizing %v (%T) produced %v (%T), which the tuple encoder cannot write",
				tc.key, tc.key, canonical, canonical)
		}
		if string(got) != string(want) {
			t.Fatalf("canonicalizing %v (%T) to %v (%T) changed its key bytes: %x, want %x — "+
				"existing records of that type would move to a different key space",
				tc.key, tc.key, canonical, canonical, got, want)
		}
	}
}

// TestRecordTypeKey_HugeUnsignedStaysUnsigned pins the one integer that must
// NOT be widened: a uint64 above max int64 has no int64 representation, so
// folding it would either wrap to a negative key or lose the value.
func TestRecordTypeKey_HugeUnsignedStaysUnsigned(t *testing.T) {
	t.Parallel()

	huge := uint64(1) << 63
	canonical, err := canonicalRecordTypeKey(huge)
	if err != nil {
		t.Fatalf("canonicalRecordTypeKey(%d): %v", huge, err)
	}
	got, ok := canonical.(uint64)
	if !ok || got != huge {
		t.Fatalf("canonicalRecordTypeKey(%d) = %v (%T), want the value unchanged as uint64", huge, canonical, canonical)
	}

	// And it is still a usable key end to end.
	md, err := keyMetaData(map[string]any{"Order": huge})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	order := &gen.Order{OrderId: proto.Int64(1)}
	stored := &FDBStoredRecord[proto.Message]{RecordType: md.GetRecordType("Order"), Record: order}
	pk, err := md.GetRecordType("Order").PrimaryKey.Evaluate(stored, order)
	if err != nil {
		t.Fatalf("primary key evaluation: %v", err)
	}
	if len(pk) != 1 || len(pk[0]) != 2 || pk[0][0] != any(huge) {
		t.Fatalf("primary key = %v, want the uint64 key followed by the order id", pk)
	}
}
