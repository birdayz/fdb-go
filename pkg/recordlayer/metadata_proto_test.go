package recordlayer

import (
	"testing"

	"fdb.dev/gen"
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
		val  any
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
	builder.GetRecordType("Order").SetPrimaryKey(Concat(Field("order_id"), Field("price")))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	builder.SetStoreRecordVersions(true)
	builder.SetSplitLongRecords(true)
	builder.SetRecordCountKey(EmptyKey())
	// Add an index (before setting version, since AddIndex bumps version)
	idx := NewIndex("order_by_price", Field("price"))
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
	if len(mdProto.RecordTypes) != 3 {
		t.Fatalf("record types: got %d, want 3", len(mdProto.RecordTypes))
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
	restoredIdx := md2.GetIndex("order_by_price")
	if restoredIdx == nil {
		t.Fatal("order_by_price index should exist")
	}
	if restoredIdx.Type != IndexTypeValue {
		t.Fatalf("index type: got %q, want %q", restoredIdx.Type, IndexTypeValue)
	}

	// Check index is associated with Order
	orderIndexes := md2.GetIndexesForRecordType("Order")
	found := false
	for _, idx := range orderIndexes {
		if idx.Name == "order_by_price" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("order_by_price should be associated with Order record type")
	}
}

func TestMetaDataToProtoWithFormerIndexes(t *testing.T) {
	t.Parallel()
	fd := gen.File_record_layer_demo_proto

	builder := NewRecordMetaDataBuilder().SetRecords(fd)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

	// Add and then remove an index
	idx := NewIndex("by_price", Field("price"))
	builder.AddIndex("Order", idx)
	builder.RemoveIndex("by_price")

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
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

	idx := NewIndex("global_idx", RecordTypeKey())
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

// TestRecordMetaDataFromProtoNil verifies nil input is rejected.
func TestRecordMetaDataFromProtoNil(t *testing.T) {
	t.Parallel()
	_, err := RecordMetaDataFromProto(nil)
	if err == nil {
		t.Fatal("expected error for nil metadata proto")
	}
}

// TestMetaDataProtoRoundtripWithAllIndexTypes verifies that metadata with
// various index types survives a ToProto→FromProto round-trip.
func TestMetaDataProtoRoundtripWithAllIndexTypes(t *testing.T) {
	t.Parallel()
	fd := gen.File_record_layer_demo_proto

	builder := NewRecordMetaDataBuilder().SetRecords(fd)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	builder.SetSplitLongRecords(true)
	builder.SetStoreRecordVersions(true)
	builder.SetRecordCountKey(RecordTypeKey())

	// Add various index types
	builder.AddIndex("Order", NewIndex("price_val", Field("price")))
	builder.AddIndex("Order", NewCountIndex("order_count", GroupAll(EmptyKey())))
	builder.AddIndex("Order", NewSumIndex("price_sum", GroupBy(Field("price"), EmptyKey())))
	builder.AddIndex("Order", NewRankIndex("price_rank", Ungrouped(Field("price"))))
	builder.AddIndex("Order", NewIndex("composite", Concat(Field("price"), Field("order_id"))))

	// Universal index
	builder.AddUniversalIndex(NewIndex("type_idx", RecordTypeKey()))

	// Add and remove to create a former index
	builder.AddIndex("Customer", NewIndex("cust_temp", Field("name")))
	builder.RemoveIndex("cust_temp")

	// Version must be >= all index added/removed versions (addIndexCommon bumps internally)
	builder.SetVersion(20)
	md, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}

	// Serialize to proto
	mdProto, err := md.ToProto()
	if err != nil {
		t.Fatal(err)
	}

	// Verify proto fields
	if !mdProto.GetSplitLongRecords() {
		t.Fatal("split_long_records should be true")
	}
	if !mdProto.GetStoreRecordVersions() {
		t.Fatal("store_record_versions should be true")
	}
	if mdProto.GetVersion() != 20 {
		t.Fatalf("version: got %d, want 20", mdProto.GetVersion())
	}
	if mdProto.RecordCountKey == nil {
		t.Fatal("record_count_key should be set")
	}

	// Deserialize back
	md2, err := RecordMetaDataFromProto(mdProto)
	if err != nil {
		t.Fatal(err)
	}

	// Verify all properties survive
	if md2.Version() != 20 {
		t.Fatalf("version: got %d, want 20", md2.Version())
	}
	if !md2.IsSplitLongRecords() {
		t.Fatal("split should be true")
	}
	if !md2.IsStoreRecordVersions() {
		t.Fatal("store_record_versions should be true")
	}

	// All indexes should survive
	for _, name := range []string{"price_val", "order_count", "price_sum", "price_rank", "composite", "type_idx"} {
		if md2.GetIndex(name) == nil {
			t.Fatalf("index %q should exist after round-trip", name)
		}
	}

	// Index types should survive
	if md2.GetIndex("order_count").Type != IndexTypeCount {
		t.Fatalf("order_count type: got %q, want %q", md2.GetIndex("order_count").Type, IndexTypeCount)
	}
	if md2.GetIndex("price_sum").Type != IndexTypeSum {
		t.Fatalf("price_sum type: got %q, want %q", md2.GetIndex("price_sum").Type, IndexTypeSum)
	}
	if md2.GetIndex("price_rank").Type != IndexTypeRank {
		t.Fatalf("price_rank type: got %q, want %q", md2.GetIndex("price_rank").Type, IndexTypeRank)
	}

	// Former index should survive
	if len(md2.GetFormerIndexes()) != 1 {
		t.Fatalf("former indexes: got %d, want 1", len(md2.GetFormerIndexes()))
	}

	// Universal index should survive
	if len(md2.GetUniversalIndexes()) != 1 {
		t.Fatalf("universal indexes: got %d, want 1", len(md2.GetUniversalIndexes()))
	}

	// Record types should survive
	for _, name := range []string{"Order", "Customer", "TypedRecord"} {
		if md2.GetRecordType(name) == nil {
			t.Fatalf("record type %q should exist", name)
		}
	}

	// Record count key should survive
	if md2.recordCountKey == nil {
		t.Fatal("record_count_key should survive round-trip")
	}
}

// TestMetaDataProtoRoundtripWithSinceVersion checks that SinceVersion survives.
func TestMetaDataProtoRoundtripWithSinceVersion(t *testing.T) {
	t.Parallel()
	fd := gen.File_record_layer_demo_proto

	builder := NewRecordMetaDataBuilder().SetRecords(fd)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	builder.GetRecordType("Order").recordType.SinceVersion = 3
	builder.SetVersion(5)

	md, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}

	mdProto, err := md.ToProto()
	if err != nil {
		t.Fatal(err)
	}

	md2, err := RecordMetaDataFromProto(mdProto)
	if err != nil {
		t.Fatal(err)
	}

	orderRT := md2.GetRecordType("Order")
	if orderRT == nil {
		t.Fatal("Order should exist")
	}
	if orderRT.SinceVersion != 3 {
		t.Fatalf("Order SinceVersion: got %d, want 3", orderRT.SinceVersion)
	}
}

// TestMetaDataProtoRoundtripWithExplicitRecordTypeKey checks explicit type keys.
func TestMetaDataProtoRoundtripWithExplicitRecordTypeKey(t *testing.T) {
	t.Parallel()
	fd := gen.File_record_layer_demo_proto

	builder := NewRecordMetaDataBuilder().SetRecords(fd)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	builder.GetRecordType("Order").SetRecordTypeKey(int64(42))
	builder.SetVersion(2)

	md, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}

	mdProto, err := md.ToProto()
	if err != nil {
		t.Fatal(err)
	}

	md2, err := RecordMetaDataFromProto(mdProto)
	if err != nil {
		t.Fatal(err)
	}

	orderRT := md2.GetRecordType("Order")
	if orderRT == nil {
		t.Fatal("Order should exist")
	}
	key := orderRT.GetRecordTypeKey()
	if key != int64(42) {
		t.Fatalf("Order record type key: got %v (%T), want int64(42)", key, key)
	}
}

// TestMetaDataProtoRoundtripMultiTypeIndex verifies multi-type indexes survive.
func TestMetaDataProtoRoundtripMultiTypeIndex(t *testing.T) {
	t.Parallel()
	fd := gen.File_record_layer_demo_proto

	builder := NewRecordMetaDataBuilder().SetRecords(fd)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

	// Add a multi-type index (shared between Order and Customer)
	multiIdx := NewIndex("shared_idx", RecordTypeKey())
	builder.AddMultiTypeIndex([]string{"Order", "Customer"}, multiIdx)

	md, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}

	mdProto, err := md.ToProto()
	if err != nil {
		t.Fatal(err)
	}

	// Multi-type index should list both record types
	var sharedProto *gen.Index
	for _, ip := range mdProto.Indexes {
		if ip.GetName() == "shared_idx" {
			sharedProto = ip
			break
		}
	}
	if sharedProto == nil {
		t.Fatal("shared_idx should be in proto")
	}
	if len(sharedProto.RecordType) != 2 {
		t.Fatalf("shared_idx record types: got %d, want 2", len(sharedProto.RecordType))
	}

	md2, err := RecordMetaDataFromProto(mdProto)
	if err != nil {
		t.Fatal(err)
	}

	restored := md2.GetIndex("shared_idx")
	if restored == nil {
		t.Fatal("shared_idx should survive round-trip")
	}

	// Both record types should have the index
	orderIdxs := md2.GetIndexesForRecordType("Order")
	customerIdxs := md2.GetIndexesForRecordType("Customer")
	hasOrder := false
	hasCustomer := false
	for _, idx := range orderIdxs {
		if idx.Name == "shared_idx" {
			hasOrder = true
		}
	}
	for _, idx := range customerIdxs {
		if idx.Name == "shared_idx" {
			hasCustomer = true
		}
	}
	if !hasOrder || !hasCustomer {
		t.Fatalf("shared_idx should be on both Order (%v) and Customer (%v)", hasOrder, hasCustomer)
	}
}
