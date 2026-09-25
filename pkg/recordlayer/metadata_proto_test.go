package recordlayer

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
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
			got, err := valueFromProto(p)
			if err != nil {
				t.Fatal(err)
			}
			// Types may differ (int→int64, float32→float32)
			if got == nil && tt.val != nil {
				t.Fatalf("got nil, want %v", tt.val)
			}
		})
	}
}

// A Value with two fields set is Java's RecordCoreException "More than one value
// encoded in value" (LiteralKeyExpression.fromProtoValue), wherever a literal is
// read: a key expression's value, a record type's explicit key, and the
// record_type_key option SetRecords reads.
func TestValueFromProto_MoreThanOneValue(t *testing.T) {
	t.Parallel()
	two := &gen.Value{LongValue: proto.Int64(1), IntValue: proto.Int32(1)}
	wantRCE := func(t *testing.T, err error) {
		t.Helper()
		var rce *RecordCoreError
		if !errors.As(err, &rce) || rce.Message != "More than one value encoded in value" {
			t.Fatalf("err = %T %v, want RecordCoreError \"More than one value encoded in value\"", err, err)
		}
	}
	_, err := valueFromProto(two)
	wantRCE(t, err)
	_, err = KeyExpressionFromProto(&gen.KeyExpression{Value: two})
	wantRCE(t, err)
	if v, err := valueFromProto(&gen.Value{}); v != nil || err != nil {
		t.Fatalf("an empty Value is (%v, %v), want (nil, nil)", v, err)
	}
	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	built, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	md, err := built.ToProto()
	if err != nil {
		t.Fatal(err)
	}
	md.RecordTypes[0].ExplicitKey = two
	_, err = RecordMetaDataFromProto(md)
	wantRCE(t, err)

	// The record_type_key option of a message, which SetRecords reads.
	opts := &descriptorpb.MessageOptions{}
	proto.SetExtension(opts, gen.E_Record, &gen.RecordTypeOptions{RecordTypeKey: two})
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name: proto.String("two_values.proto"), Package: proto.String("twovalues"), Syntax: proto.String("proto2"),
		Dependency: []string{"record_metadata_options.proto"},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name:    proto.String("T"),
			Field:   []*descriptorpb.FieldDescriptorProto{{Name: proto.String("id"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()}},
			Options: opts,
		}, {
			Name:  proto.String("RecordTypeUnion"),
			Field: []*descriptorpb.FieldDescriptorProto{{Name: proto.String("_T"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(".twovalues.T")}},
		}},
	}, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatal(err)
	}
	b := NewRecordMetaDataBuilder().SetRecords(fd)
	b.GetRecordType("T").SetPrimaryKey(Field("id"))
	_, err = b.Build()
	wantRCE(t, err)
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

// TestStoredIndexSubspaceKeyIsReadAsJavaReadsIt pins the loader's reading of a
// stored index's subspace key against Java's Index(proto) (Index.java:80-97,
// :221-225): a present key must pack exactly one non-null item, an absent key
// is the index's name. Go used to fall back to the name for an empty key, a key
// of two items and a null item, loading metadata Java refuses and maintaining
// the index under a subspace Java never reads. The conformance spec "WS-J stored
// index protos read as Java reads them" asks the JVM for the same protos.
func TestStoredIndexSubspaceKeyIsReadAsJavaReadsIt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name         string
		key          []byte
		want         any // the loaded key, when it loads
		repeatOption bool
		noRoot       bool
		errType      string
		errMsg       string
	}{
		{name: "absent: the index's name", key: nil, want: "SK_IDX"},
		{name: "a single string item", key: tuple.Tuple{"elsewhere"}.Pack(), want: "elsewhere"},
		{name: "a single integer item", key: tuple.Tuple{int64(7)}.Pack(), want: int64(7)},
		{name: "present and empty", key: []byte{}, errType: "RecordCoreError", errMsg: "subspace key must encode a single item tuple"},
		{name: "two items", key: tuple.Tuple{"a", "b"}.Pack(), errType: "RecordCoreError", errMsg: "subspace key must encode a single item tuple"},
		{name: "a null item", key: tuple.Tuple{nil}.Pack(), errType: "RecordCoreArgumentError", errMsg: "Index subspace key cannot be null"},
		// Java builds the options with the type, before the root and the key
		// (Index.java:198-221), so a repeated option wins over an empty key.
		{name: "a repeated option beside an empty key", key: []byte{}, repeatOption: true, errType: "DuplicateIndexOptionError", errMsg: "Multiple entries with same key: a=2 and a=1"},
		// The root is read before the key (Index.java:205), and an absent root
		// is refused (KeyExpression.java:404-405), where Go used to skip it.
		{name: "an absent root beside an empty key", key: []byte{}, noRoot: true, errType: "root", errMsg: "Exactly one root must be specified for an index"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			built, err := b.Build()
			if err != nil {
				t.Fatal(err)
			}
			md, err := built.ToProto()
			if err != nil {
				t.Fatal(err)
			}
			md.Version = proto.Int32(2)
			idx := &gen.Index{
				Name: proto.String("SK_IDX"), RecordType: []string{"Order"},
				RootExpression: Field("price").ToKeyExpression(), Type: proto.String("value"),
				AddedVersion: proto.Int32(1), LastModifiedVersion: proto.Int32(1), SubspaceKey: c.key,
			}
			if c.repeatOption {
				idx.Options = []*gen.Index_Option{{Key: proto.String("a"), Value: proto.String("1")}, {Key: proto.String("a"), Value: proto.String("2")}}
			}
			if c.noRoot {
				idx.RootExpression = nil
			}
			md.Indexes = append(md.Indexes, idx)
			// Load the marshalled bytes, as a stored meta-data is loaded: the
			// presence of an empty key is what the unmarshaller keeps.
			stored, err := proto.Marshal(md)
			if err != nil {
				t.Fatal(err)
			}
			var decoded gen.MetaData
			if err := proto.Unmarshal(stored, &decoded); err != nil {
				t.Fatal(err)
			}
			loaded, err := RecordMetaDataFromProto(&decoded)
			if c.errMsg == "" {
				if err != nil {
					t.Fatalf("load: %v", err)
				}
				if got := loaded.GetIndex("SK_IDX").SubspaceTupleKey(); got != c.want {
					t.Fatalf("subspace key = %#v, want %#v", got, c.want)
				}
				return
			}
			var core *RecordCoreError
			var pde *MetaDataProtoDeserializationError
			var rootErr *KeyExpressionDeserializationError
			var arg *RecordCoreArgumentError
			var dup *DuplicateIndexOptionError
			switch {
			case c.errType == "root" && errors.As(err, &pde) && errors.As(err, &rootErr) && strings.HasPrefix(rootErr.Message, c.errMsg) &&
				errors.As(err, &core) && core.Message == rootErr.Message:
				// The root's refusal, Java's DeserializationException (the only
				// RecordCoreError in the chain) under its
				// MetaDataProtoDeserializationException, before the empty key is
				// read.
			case c.errType == "DuplicateIndexOptionError" && errors.As(err, &dup):
				if dup.Error() != c.errMsg {
					t.Fatalf("message = %q, want %q", dup.Error(), c.errMsg)
				}
			case c.errType == "RecordCoreError" && errors.As(err, &core):
				if core.Message != c.errMsg {
					t.Fatalf("message = %q, want %q", core.Message, c.errMsg)
				}
			case c.errType == "RecordCoreArgumentError" && errors.As(err, &arg):
				if arg.Message != c.errMsg || arg.IndexName != "SK_IDX" {
					t.Fatalf("error = %+v, want %q naming SK_IDX", arg, c.errMsg)
				}
			default:
				t.Fatalf("load = %v (%T), want %s %q", err, err, c.errType, c.errMsg)
			}
		})
	}
}

// TestReusedIndexMessageKeepsAnEmptySubspaceKey pins why a loader must be handed
// a FRESH Index message. vtproto's ResetVT keeps a non-nil SubspaceKey[:0], so a
// reused (pooled) message decoded from bytes without a subspace key reads the
// key as present and empty, and the loader refuses it as Java refuses an empty
// key. Nothing in this repository pools MetaData, Index or FormerIndex messages
// (ws-j-design.md, the carry rule's stored side); this is what a loader handed
// one would do: refuse loudly, never read another index's key.
func TestReusedIndexMessageKeepsAnEmptySubspaceKey(t *testing.T) {
	t.Parallel()
	withoutKey, err := proto.Marshal(&gen.Index{
		Name: proto.String("SK_IDX"), RecordType: []string{"Order"},
		RootExpression: Field("price").ToKeyExpression(), Type: proto.String("value"),
		AddedVersion: proto.Int32(1), LastModifiedVersion: proto.Int32(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	var fresh gen.Index
	if err := fresh.UnmarshalVT(withoutKey); err != nil {
		t.Fatal(err)
	}
	if fresh.SubspaceKey != nil {
		t.Fatalf("a fresh message read an absent key as %#v", fresh.SubspaceKey)
	}

	reused := &gen.Index{SubspaceKey: tuple.Tuple{"elsewhere"}.Pack()}
	reused.ResetVT()
	if err := reused.UnmarshalVT(withoutKey); err != nil {
		t.Fatal(err)
	}
	if reused.SubspaceKey == nil || len(reused.SubspaceKey) != 0 {
		t.Fatalf("a reused message's key = %#v; ResetVT no longer keeps an empty non-nil key, "+
			"and the loader contract this test pins can be restated", reused.SubspaceKey)
	}
	if _, err := indexFromProto(reused); err == nil {
		t.Fatal("the loader read a reused message's empty key as a key")
	} else {
		var core *RecordCoreError
		if !errors.As(err, &core) || core.Message != "subspace key must encode a single item tuple" {
			t.Fatalf("loading a reused message: %v, want the empty-key refusal", err)
		}
	}
}
