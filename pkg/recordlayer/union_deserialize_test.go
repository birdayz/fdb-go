package recordlayer

import (
	"errors"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
)

// TestDiscoverUnionIsJavasDeserializeUnion: a stored union message must hold
// exactly one known field and no unknown one, as Java's
// DynamicMessageRecordSerializer.deserializeUnion requires, and a field it
// repeats is merged, as protobuf parses it.
func TestDiscoverUnionIsJavasDeserializeUnion(t *testing.T) {
	t.Parallel()
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	md, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	store := &FDBRecordStore{metaData: md}
	orderRT, customerRT := md.GetRecordType("Order"), md.GetRecordType("Customer")
	field := func(num protowire.Number, m proto.Message) []byte {
		inner, err := proto.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return protowire.AppendBytes(protowire.AppendTag(nil, num, protowire.BytesType), inner)
	}
	order := field(orderRT.unionFieldNumber, &gen.Order{OrderId: proto.Int64(1)})
	customer := field(customerRT.unionFieldNumber, &gen.Customer{CustomerId: proto.Int64(2)})
	unknown := protowire.AppendVarint(protowire.AppendTag(nil, 999, protowire.VarintType), 7)

	rt, msg, _, err := store.discoverUnion(order)
	if err != nil || rt != orderRT || msg.(*gen.Order).GetOrderId() != 1 {
		t.Fatalf("one known field: %v, %v", rt, err)
	}
	merged := append(append([]byte(nil), order...), field(orderRT.unionFieldNumber, &gen.Order{Price: proto.Int32(5)})...)
	rt, msg, _, err = store.discoverUnion(merged)
	if err != nil || rt != orderRT || msg.(*gen.Order).GetOrderId() != 1 || msg.(*gen.Order).GetPrice() != 5 {
		t.Fatalf("one field twice, merged: %v, %v", msg, err)
	}
	for _, c := range []struct {
		name string
		data []byte
		want string
	}{
		{"an unknown field beside the record", append(append([]byte(nil), order...), unknown...), "because there are unknown fields"},
		{"an unknown field alone", unknown, "because there are unknown fields"},
		{"two record fields", append(append([]byte(nil), order...), customer...), "because there are extra known fields"},
		{"no field", nil, "because there are no fields"},
	} {
		_, _, _, err := store.discoverUnion(c.data)
		var se *RecordSerializationError
		if !errors.As(err, &se) || se.Message != "Could not deserialize union message "+c.want {
			t.Errorf("%s: %v, want \"Could not deserialize union message %s\"", c.name, err, c.want)
		}
	}
}
