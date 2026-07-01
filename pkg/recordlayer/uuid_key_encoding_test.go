package recordlayer

import (
	"bytes"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// Pins the WIRE format of a UUID index/PK key element (RFC-162, item 1021): a
// tuple_fields.UUID message must encode byte-identically to Java's
// TupleFieldsHelper.fromProto → java.util.UUID → Tuple, i.e. UUID_CODE(0x30) +
// most_significant_bits(8B big-endian) + least_significant_bits(8B big-endian).
// This is the wire contract — Go and Java apps share index entries.
func TestUUIDMessageToTuple_JavaWireFormat(t *testing.T) {
	t.Parallel()
	// "550e8400-e29b-41d4-a716-446655440000":
	//   msb = 0x550e8400e29b41d4, lsb = 0xa716446655440000.
	// (lsb exceeds int64 max, so reinterpret its bits via a uint64 variable — a
	// constant int64(...) conversion would be range-checked and overflow.)
	msbBits := uint64(0x550e8400e29b41d4)
	lsbBits := uint64(0xa716446655440000)
	msg := (&gen.UUID{
		MostSignificantBits:  proto.Int64(int64(msbBits)),
		LeastSignificantBits: proto.Int64(int64(lsbBits)),
	}).ProtoReflect()

	got := uuidMessageToTuple(msg)
	want := tuple.UUID{
		0x55, 0x0e, 0x84, 0x00, 0xe2, 0x9b, 0x41, 0xd4, // msb, big-endian
		0xa7, 0x16, 0x44, 0x66, 0x55, 0x44, 0x00, 0x00, // lsb, big-endian
	}
	if got != want {
		t.Fatalf("uuidMessageToTuple = %x, want %x (msb||lsb big-endian)", got, want)
	}

	// Packed into a tuple, the element is 0x30 + the 16 bytes — exactly Java's
	// TupleUtil.encode(UUID): add(UUID_CODE).add(msb).add(lsb).
	packed := tuple.Tuple{got}.Pack()
	wantPacked := append([]byte{0x30}, want[:]...)
	if !bytes.Equal(packed, wantPacked) {
		t.Fatalf("packed UUID tuple = %x, want %x (0x30 + msb||lsb BE)", packed, wantPacked)
	}

	// Zero UUID round-trips to all-zero bytes (no sign-extension surprises).
	zero := uuidMessageToTuple((&gen.UUID{
		MostSignificantBits:  proto.Int64(0),
		LeastSignificantBits: proto.Int64(0),
	}).ProtoReflect())
	if zero != (tuple.UUID{}) {
		t.Fatalf("zero UUID = %x, want all-zero", zero)
	}
}
