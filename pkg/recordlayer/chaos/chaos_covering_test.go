package chaos

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// buildCoveringIndexMetadata creates metadata with a covering index (KeyWithValue).
// price in FDB key, flower.type in FDB value.
func buildCoveringIndexMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.AddIndex("Order", recordlayer.NewIndex("covering_price_flower",
		recordlayer.KeyWithValue(
			recordlayer.Concat(recordlayer.Field("price"), recordlayer.Nest("flower", recordlayer.Field("type"))),
			1)))
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build covering index metadata: " + err.Error())
	}
	return md
}

// buildMultiKeyCoveringMetadata creates metadata with a multi-key-column covering index.
// [price, order_id] in FDB key, [flower.type] in FDB value.
func buildMultiKeyCoveringMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.AddIndex("Order", recordlayer.NewIndex("covering_multi",
		recordlayer.KeyWithValue(
			recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("order_id"), recordlayer.Nest("flower", recordlayer.Field("type"))),
			2)))
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build multi-key covering metadata: " + err.Error())
	}
	return md
}

// --- Basic verification (no faults) ---

func TestCoveringIndexBasicVerify(t *testing.T) {
	t.Parallel()
	md := buildCoveringIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
		Flower:  &gen.Flower{Type: proto.String("Rose"), Color: gen.Color_RED.Enum()},
	})
	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(2),
		Price:   proto.Int32(200),
		Flower:  &gen.Flower{Type: proto.String("Tulip"), Color: gen.Color_BLUE.Enum()},
	})
	s.Verify()
}

func TestCoveringIndexOverwrite(t *testing.T) {
	t.Parallel()
	md := buildCoveringIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
		Flower:  &gen.Flower{Type: proto.String("Rose")},
	})
	s.Verify()

	// Update: change both key column (price) and value column (flower.type).
	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(200),
		Flower:  &gen.Flower{Type: proto.String("Tulip")},
	})
	s.Verify()

	// Update: change only value column (flower.type), key column (price) stays.
	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(200),
		Flower:  &gen.Flower{Type: proto.String("Lily")},
	})
	s.Verify()
}

func TestCoveringIndexDelete(t *testing.T) {
	t.Parallel()
	md := buildCoveringIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
		Flower:  &gen.Flower{Type: proto.String("Rose")},
	})
	s.DeleteRecord(tuple.Tuple{int64(1)})
	s.Verify()
}

func TestCoveringIndexDeleteAll(t *testing.T) {
	t.Parallel()
	md := buildCoveringIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
		Flower:  &gen.Flower{Type: proto.String("Rose")},
	})
	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(2),
		Price:   proto.Int32(200),
		Flower:  &gen.Flower{Type: proto.String("Tulip")},
	})
	s.DeleteAllRecords()
	s.Verify()
}

// --- Commit-unknown fault injection ---

func TestCoveringIndexCommitUnknownInsert(t *testing.T) {
	t.Parallel()
	md := buildCoveringIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
		Flower:  &gen.Flower{Type: proto.String("Rose")},
	})
	s.Verify()
}

func TestCoveringIndexCommitUnknownOverwrite(t *testing.T) {
	t.Parallel()
	md := buildCoveringIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	// First save (clean).
	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
		Flower:  &gen.Flower{Type: proto.String("Rose")},
	})

	// Overwrite with commit-unknown.
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(200),
		Flower:  &gen.Flower{Type: proto.String("Tulip")},
	})
	s.Verify()
}

func TestCoveringIndexCommitUnknownDelete(t *testing.T) {
	t.Parallel()
	md := buildCoveringIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
		Flower:  &gen.Flower{Type: proto.String("Rose")},
	})

	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(1)})
	s.Verify()
}

func TestCoveringIndexCommitUnknownDeleteAll(t *testing.T) {
	t.Parallel()
	md := buildCoveringIndexMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
		Flower:  &gen.Flower{Type: proto.String("Rose")},
	})
	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(2),
		Price:   proto.Int32(200),
		Flower:  &gen.Flower{Type: proto.String("Tulip")},
	})

	s.InjectOnce(FaultCommitUnknown)
	s.DeleteAllRecords()
	s.Verify()
}

// --- Multi-key covering index ---

func TestMultiKeyCoveringBasicVerify(t *testing.T) {
	t.Parallel()
	md := buildMultiKeyCoveringMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
		Flower:  &gen.Flower{Type: proto.String("Rose")},
	})
	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(2),
		Price:   proto.Int32(200),
		Flower:  &gen.Flower{Type: proto.String("Tulip")},
	})
	s.Verify()
}

func TestMultiKeyCoveringCommitUnknown(t *testing.T) {
	t.Parallel()
	md := buildMultiKeyCoveringMetadata()
	s := NewScenario(t, testRealDB, md)

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(100),
		Flower:  &gen.Flower{Type: proto.String("Rose")},
	})
	s.Verify()

	// Overwrite with fault.
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Order{
		OrderId: proto.Int64(1),
		Price:   proto.Int32(300),
		Flower:  &gen.Flower{Type: proto.String("Daisy")},
	})
	s.Verify()
}

// --- Random stress ---

func TestCoveringIndexRandomRetryHeavy(t *testing.T) {
	t.Parallel()
	RunRandom(t, testRealDB, buildCoveringIndexMetadata(), RandomConfig{
		Seed:   20020,
		NumOps: 200,
		MaxPKs: 20,
		Faults: FaultsRetryHeavy,
	})
}

func TestCoveringIndexRandomAll(t *testing.T) {
	t.Parallel()
	RunRandom(t, testRealDB, buildCoveringIndexMetadata(), RandomConfig{
		Seed:   21021,
		NumOps: 200,
		MaxPKs: 20,
		Faults: FaultsAll,
	})
}
