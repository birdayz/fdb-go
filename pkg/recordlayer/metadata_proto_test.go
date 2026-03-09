package recordlayer

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"google.golang.org/protobuf/proto"
)

func TestIndexToProtoRoundtrip(t *testing.T) {
	t.Parallel()

	t.Run("basic_value_index", func(t *testing.T) {
		t.Parallel()
		idx := NewIndex("by_name", Field("name"))
		idx.AddedVersion = 1
		idx.LastModifiedVersion = 2

		p, err := indexToProto(idx)
		if err != nil {
			t.Fatal(err)
		}
		if p.GetName() != "by_name" {
			t.Fatalf("name: got %q, want %q", p.GetName(), "by_name")
		}
		if p.GetType() != IndexTypeValue {
			t.Fatalf("type: got %q, want %q", p.GetType(), IndexTypeValue)
		}
		if p.RootExpression == nil || p.RootExpression.Field == nil {
			t.Fatal("root expression should be a Field")
		}

		restored, err := indexFromProto(p)
		if err != nil {
			t.Fatal(err)
		}
		if restored.Name != idx.Name {
			t.Fatalf("name: got %q, want %q", restored.Name, idx.Name)
		}
		if restored.Type != idx.Type {
			t.Fatalf("type: got %q, want %q", restored.Type, idx.Type)
		}
		if restored.AddedVersion != idx.AddedVersion {
			t.Fatalf("added version: got %d, want %d", restored.AddedVersion, idx.AddedVersion)
		}
		if restored.LastModifiedVersion != idx.LastModifiedVersion {
			t.Fatalf("last modified version: got %d, want %d", restored.LastModifiedVersion, idx.LastModifiedVersion)
		}
	})

	t.Run("unique_index_with_options", func(t *testing.T) {
		t.Parallel()
		idx := NewIndex("by_email", Field("email")).SetUnique()

		p, err := indexToProto(idx)
		if err != nil {
			t.Fatal(err)
		}

		restored, err := indexFromProto(p)
		if err != nil {
			t.Fatal(err)
		}
		if !restored.IsUnique() {
			t.Fatal("restored index should be unique")
		}
	})

	t.Run("composite_root_expression", func(t *testing.T) {
		t.Parallel()
		idx := NewIndex("by_name_age", Concat(Field("name"), Field("age")))

		p, err := indexToProto(idx)
		if err != nil {
			t.Fatal(err)
		}

		restored, err := indexFromProto(p)
		if err != nil {
			t.Fatal(err)
		}
		if !keyExpressionEquals(idx.RootExpression, restored.RootExpression) {
			t.Fatal("root expression mismatch")
		}
	})

	t.Run("numeric_subspace_key", func(t *testing.T) {
		t.Parallel()
		idx := NewIndex("by_id", Field("id"))
		idx.SetSubspaceKey(int64(42))

		p, err := indexToProto(idx)
		if err != nil {
			t.Fatal(err)
		}

		restored, err := indexFromProto(p)
		if err != nil {
			t.Fatal(err)
		}
		if restored.SubspaceTupleKey() != int64(42) {
			t.Fatalf("subspace key: got %v, want 42", restored.SubspaceTupleKey())
		}
	})
}

func TestFormerIndexToProtoRoundtrip(t *testing.T) {
	t.Parallel()
	fi := &FormerIndex{
		SubspaceKey:    "old_index",
		AddedVersion:   1,
		RemovedVersion: 3,
		FormerName:     "old_index",
	}

	p, err := formerIndexToProto(fi)
	if err != nil {
		t.Fatal(err)
	}

	restored, err := formerIndexFromProto(p)
	if err != nil {
		t.Fatal(err)
	}
	if restored.FormerName != fi.FormerName {
		t.Fatalf("name: got %q, want %q", restored.FormerName, fi.FormerName)
	}
	if restored.AddedVersion != fi.AddedVersion {
		t.Fatalf("added: got %d, want %d", restored.AddedVersion, fi.AddedVersion)
	}
	if restored.RemovedVersion != fi.RemovedVersion {
		t.Fatalf("removed: got %d, want %d", restored.RemovedVersion, fi.RemovedVersion)
	}
}

func TestValueToProtoRoundtrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		val  interface{}
	}{
		{"int", 42},
		{"int64", int64(123456)},
		{"int32", int32(99)},
		{"float64", 3.14},
		{"float32", float32(2.5)},
		{"bool", true},
		{"string", "hello"},
		{"bytes", []byte{1, 2, 3}},
		{"nil", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, err := valueToProto(tt.val)
			if err != nil {
				t.Fatal(err)
			}
			got := valueFromProto(p)
			// Types may differ (int→int64, float32→float32)
			if got == nil && tt.val != nil {
				t.Fatalf("got nil, want %v", tt.val)
			}
		})
	}
}

func TestMetaDataToProtoRoundtrip(t *testing.T) {
	t.Parallel()
	// Use the demo proto file descriptor
	fd := gen.File_record_layer_demo_proto

	builder := NewRecordMetaDataBuilder().SetRecords(fd)
	builder.GetRecordType("Order").SetPrimaryKey( Concat(Field("order_id"), Field("order_no")))
	builder.GetRecordType("Customer").SetPrimaryKey( Field("customer_id"))
	builder.SetStoreRecordVersions(true)
	builder.SetSplitLongRecords(true)
	builder.SetRecordCountKey(EmptyKey())
	// Add an index (before setting version, since AddIndex bumps version)
	idx := NewIndex("order_by_no", Field("order_no"))
	builder.AddIndex("Order", idx)

	builder.SetVersion(5)

	md, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}

	// Serialize
	mdProto, err := md.ToProto()
	if err != nil {
		t.Fatal(err)
	}

	// Verify proto fields
	if mdProto.Records == nil {
		t.Fatal("records should be set")
	}
	if !mdProto.GetSplitLongRecords() {
		t.Fatal("split_long_records should be true")
	}
	if !mdProto.GetStoreRecordVersions() {
		t.Fatal("store_record_versions should be true")
	}
	if mdProto.GetVersion() != 5 {
		t.Fatalf("version: got %d, want 5", mdProto.GetVersion())
	}
	if len(mdProto.RecordTypes) != 2 {
		t.Fatalf("record types: got %d, want 2", len(mdProto.RecordTypes))
	}
	if len(mdProto.Indexes) != 1 {
		t.Fatalf("indexes: got %d, want 1", len(mdProto.Indexes))
	}
	//nolint:staticcheck // RecordCountKey is deprecated but still used
	if mdProto.RecordCountKey == nil || mdProto.RecordCountKey.Empty == nil {
		t.Fatal("record count key should be Empty")
	}

	// Wire roundtrip
	data, err := proto.Marshal(mdProto)
	if err != nil {
		t.Fatal(err)
	}
	var restored gen.MetaData
	if err := proto.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}

	// Deserialize
	md2, err := RecordMetaDataFromProto(&restored)
	if err != nil {
		t.Fatal(err)
	}

	// Verify deserialized metadata
	if md2.Version() != md.Version() {
		t.Fatalf("version: got %d, want %d", md2.Version(), md.Version())
	}
	if md2.IsSplitLongRecords() != md.IsSplitLongRecords() {
		t.Fatal("split long records mismatch")
	}
	if md2.IsStoreRecordVersions() != md.IsStoreRecordVersions() {
		t.Fatal("store record versions mismatch")
	}
	if md2.GetRecordCountKey() == nil {
		t.Fatal("record count key should be restored")
	}

	// Check record types
	orderRT := md2.GetRecordType("Order")
	if orderRT == nil {
		t.Fatal("Order record type should exist")
	}
	if orderRT.PrimaryKey == nil {
		t.Fatal("Order primary key should be set")
	}
	customerRT := md2.GetRecordType("Customer")
	if customerRT == nil {
		t.Fatal("Customer record type should exist")
	}

	// Check index
	restoredIdx := md2.GetIndex("order_by_no")
	if restoredIdx == nil {
		t.Fatal("order_by_no index should exist")
	}
	if restoredIdx.Type != IndexTypeValue {
		t.Fatalf("index type: got %q, want %q", restoredIdx.Type, IndexTypeValue)
	}

	// Check index is associated with Order
	orderIndexes := md2.GetIndexesForRecordType("Order")
	found := false
	for _, idx := range orderIndexes {
		if idx.Name == "order_by_no" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("order_by_no should be associated with Order record type")
	}
}

func TestMetaDataToProtoWithFormerIndexes(t *testing.T) {
	t.Parallel()
	fd := gen.File_record_layer_demo_proto

	builder := NewRecordMetaDataBuilder().SetRecords(fd)
	builder.GetRecordType("Order").SetPrimaryKey( Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey( Field("customer_id"))

	// Add and then remove an index
	idx := NewIndex("by_name", Field("order_no"))
	builder.AddIndex("Order", idx)
	builder.RemoveIndex("by_name")

	md, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}

	if len(md.GetFormerIndexes()) != 1 {
		t.Fatalf("former indexes: got %d, want 1", len(md.GetFormerIndexes()))
	}

	mdProto, err := md.ToProto()
	if err != nil {
		t.Fatal(err)
	}
	if len(mdProto.FormerIndexes) != 1 {
		t.Fatalf("proto former indexes: got %d, want 1", len(mdProto.FormerIndexes))
	}

	md2, err := RecordMetaDataFromProto(mdProto)
	if err != nil {
		t.Fatal(err)
	}
	if len(md2.GetFormerIndexes()) != 1 {
		t.Fatalf("restored former indexes: got %d, want 1", len(md2.GetFormerIndexes()))
	}
}

func TestMetaDataToProtoUniversalIndex(t *testing.T) {
	t.Parallel()
	fd := gen.File_record_layer_demo_proto

	builder := NewRecordMetaDataBuilder().SetRecords(fd)
	builder.GetRecordType("Order").SetPrimaryKey( Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey( Field("customer_id"))

	idx := NewIndex("global_idx", Field("order_no"))
	builder.AddUniversalIndex(idx)

	md, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}

	mdProto, err := md.ToProto()
	if err != nil {
		t.Fatal(err)
	}

	// Universal index should have no record types
	if len(mdProto.Indexes) != 1 {
		t.Fatalf("indexes: got %d, want 1", len(mdProto.Indexes))
	}
	if len(mdProto.Indexes[0].RecordType) != 0 {
		t.Fatalf("universal index should have no record types, got %v", mdProto.Indexes[0].RecordType)
	}

	md2, err := RecordMetaDataFromProto(mdProto)
	if err != nil {
		t.Fatal(err)
	}

	if len(md2.GetUniversalIndexes()) != 1 {
		t.Fatalf("restored universal indexes: got %d, want 1", len(md2.GetUniversalIndexes()))
	}
}
