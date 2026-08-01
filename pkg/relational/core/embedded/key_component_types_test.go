package embedded

import (
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
)

func requireEmbeddedPhysicalTypeCodes(t *testing.T, got []values.Type, want ...values.TypeCode) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("type count = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] == nil || got[i].Code() != want[i] {
			t.Fatalf("type[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestPhysicalKeyComponentTypesFollowExpressionTopology(t *testing.T) {
	t.Parallel()

	descriptor := gen.File_record_layer_demo_proto.Messages().ByName("TypedRecord")
	recordType := &recordlayer.RecordType{
		Name:            "TypedRecord",
		Descriptor:      descriptor,
		RecordTypeIndex: 3,
	}
	expression := recordlayer.Concat(
		recordlayer.Field("val_float"),
		recordlayer.Field("val_double"),
		recordlayer.Literal(float32(0)),
		recordlayer.Literal(float64(0)),
		recordlayer.Literal(int32(1)),
		recordlayer.RecordTypeKey(),
	)
	requireEmbeddedPhysicalTypeCodes(
		t,
		physicalKeyComponentTypes(expression, []*recordlayer.RecordType{recordType}),
		values.TypeCodeFloat,
		values.TypeCodeDouble,
		values.TypeCodeFloat,
		values.TypeCodeDouble,
		values.TypeCodeInt,
		values.TypeCodeLong,
	)

	keyWithValue := recordlayer.KeyWithValue(
		recordlayer.Concat(recordlayer.Field("val_float"), recordlayer.Field("val_double")),
		1,
	)
	requireEmbeddedPhysicalTypeCodes(
		t,
		physicalKeyComponentTypes(keyWithValue, []*recordlayer.RecordType{recordType}),
		values.TypeCodeFloat,
	)
}

func TestPhysicalKeyComponentTypesHandleNestingAndCardinality(t *testing.T) {
	t.Parallel()

	order := &recordlayer.RecordType{
		Name:            "Order",
		Descriptor:      gen.File_record_layer_demo_proto.Messages().ByName("Order"),
		RecordTypeIndex: 1,
	}
	nested := recordlayer.Nest("flower", recordlayer.Field("color"))
	requireEmbeddedPhysicalTypeCodes(
		t,
		physicalKeyComponentTypes(nested, []*recordlayer.RecordType{order}),
		values.TypeCodeLong,
	)
	cardinality := recordlayer.CardinalityExpr(recordlayer.FieldConcatenate("tags"))
	requireEmbeddedPhysicalTypeCodes(
		t,
		physicalKeyComponentTypes(cardinality, []*recordlayer.RecordType{order}),
		values.TypeCodeInt,
	)
}

func TestPhysicalKeyComponentTypesDisagreeToUnknown(t *testing.T) {
	t.Parallel()

	label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	floatKind := descriptorpb.FieldDescriptorProto_TYPE_FLOAT
	doubleKind := descriptorpb.FieldDescriptorProto_TYPE_DOUBLE
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    proto.String("physical_key_types_test.proto"),
		Package: proto.String("physicalkeytest"),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("FloatRecord"), Field: []*descriptorpb.FieldDescriptorProto{{
				Name: proto.String("key"), Number: proto.Int32(1), Label: &label, Type: &floatKind,
			}}},
			{Name: proto.String("DoubleRecord"), Field: []*descriptorpb.FieldDescriptorProto{{
				Name: proto.String("key"), Number: proto.Int32(1), Label: &label, Type: &doubleKind,
			}}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build descriptor: %v", err)
	}
	recordTypes := []*recordlayer.RecordType{
		{Name: "FloatRecord", Descriptor: file.Messages().ByName("FloatRecord"), RecordTypeIndex: 1},
		{Name: "DoubleRecord", Descriptor: file.Messages().ByName("DoubleRecord"), RecordTypeIndex: 2},
	}
	requireEmbeddedPhysicalTypeCodes(
		t,
		physicalKeyComponentTypes(recordlayer.Field("key"), recordTypes),
		values.TypeCodeUnknown,
	)
}

func TestPrimaryCandidateTypesExcludeExecutorRecordTypePrefix(t *testing.T) {
	t.Parallel()

	recordType := &recordlayer.RecordType{
		Name:            "TypedRecord",
		Descriptor:      gen.File_record_layer_demo_proto.Messages().ByName("TypedRecord"),
		RecordTypeIndex: 3,
		PrimaryKey: recordlayer.Concat(
			recordlayer.RecordTypeKey(),
			recordlayer.Field("val_float"),
		),
	}
	names, types := primaryCandidateKeyComponents(recordType)
	if len(names) != 1 || names[0] != "val_float" {
		t.Fatalf("primary candidate names = %q, want [val_float]", names)
	}
	requireEmbeddedPhysicalTypeCodes(t, types, values.TypeCodeFloat)

	// An implicit component after the separately supplied type prefix cannot be
	// represented by the flat primary-candidate Value list. Decline the whole
	// candidate rather than shift the following DOUBLE field onto that slot.
	recordType.PrimaryKey = recordlayer.Concat(
		recordlayer.RecordTypeKey(),
		recordlayer.Literal(float32(0)),
		recordlayer.Field("val_double"),
	)
	names, types = primaryCandidateKeyComponents(recordType)
	if names != nil || types != nil {
		t.Fatalf("unsafe primary candidate = names %q, types %v; want declined", names, types)
	}

	recordType.PrimaryKey = recordlayer.Concat(
		recordlayer.Field("id"), recordlayer.Field("val_double"),
	)
	names, types = primaryCandidateKeyComponents(recordType)
	if len(names) != 2 || names[0] != "id" || names[1] != "val_double" {
		t.Fatalf("flat primary candidate names = %q, want [id val_double]", names)
	}
	requireEmbeddedPhysicalTypeCodes(t, types, values.TypeCodeLong, values.TypeCodeDouble)

	for name, primaryKey := range map[string]recordlayer.KeyExpression{
		"record type key only": recordlayer.RecordTypeKey(),
		"literal suffix": recordlayer.Concat(
			recordlayer.Field("id"), recordlayer.Literal(int64(7)),
		),
		"nested": recordlayer.Nest("flower", recordlayer.Field("color")),
		"function": recordlayer.CardinalityExpr(
			recordlayer.FieldConcatenate("tags"),
		),
		"version": recordlayer.Concat(
			recordlayer.VersionKey(), recordlayer.Field("id"),
		),
		"mid record type key": recordlayer.Concat(
			recordlayer.Field("id"), recordlayer.RecordTypeKey(),
		),
	} {
		name, primaryKey := name, primaryKey
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			candidate := *recordType
			candidate.PrimaryKey = primaryKey
			candidateNames, candidateTypes := primaryCandidateKeyComponents(&candidate)
			if candidateNames != nil || candidateTypes != nil {
				t.Fatalf("unsafe primary topology exposed candidate names=%v types=%v",
					candidateNames, candidateTypes)
			}
		})
	}
}

func TestCoveredPrimaryKeyColumnsRequireCoordinateSafeTail(t *testing.T) {
	t.Parallel()

	recordType := &recordlayer.RecordType{
		Name:       "TypedRecord",
		Descriptor: gen.File_record_layer_demo_proto.Messages().ByName("TypedRecord"),
	}
	for _, test := range []struct {
		name       string
		primaryKey recordlayer.KeyExpression
		want       []string
		wantRTK    bool
	}{
		{
			name: "flat scalar",
			primaryKey: recordlayer.Concat(
				recordlayer.Field("id"), recordlayer.Field("val_double"),
			),
			want: []string{"id", "val_double"},
		},
		{
			name: "single leading record type key",
			primaryKey: recordlayer.Concat(
				recordlayer.RecordTypeKey(), recordlayer.Field("id"),
			),
			want:    []string{"id"},
			wantRTK: true,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			candidate := *recordType
			candidate.PrimaryKey = test.primaryKey
			columns, leadingRTK, ok := coveredPrimaryKeyColumns(&candidate)
			if !ok || leadingRTK != test.wantRTK || len(columns) != len(test.want) {
				t.Fatalf("covered PK = %v, leadingRTK=%v, ok=%v; want %v, %v, true",
					columns, leadingRTK, ok, test.want, test.wantRTK)
			}
			for i := range test.want {
				if columns[i] != test.want[i] {
					t.Fatalf("covered PK[%d] = %q, want %q", i, columns[i], test.want[i])
				}
			}
		})
	}

	for name, primaryKey := range map[string]recordlayer.KeyExpression{
		"record type key only": recordlayer.RecordTypeKey(),
		"literal suffix": recordlayer.Concat(
			recordlayer.Field("id"), recordlayer.Literal(int64(7)),
		),
		"literal prefix": recordlayer.Concat(
			recordlayer.Literal(int64(7)), recordlayer.Field("id"),
		),
		"hidden middle": recordlayer.Concat(
			recordlayer.RecordTypeKey(), recordlayer.Field("id"),
			recordlayer.Literal(int64(7)), recordlayer.Field("val_double"),
		),
		"record type suffix": recordlayer.Concat(
			recordlayer.Field("id"), recordlayer.RecordTypeKey(),
		),
		"nested field": recordlayer.Nest("flower", recordlayer.Field("color")),
		"function": recordlayer.CardinalityExpr(
			recordlayer.FieldConcatenate("tags"),
		),
		"version": recordlayer.Concat(
			recordlayer.VersionKey(), recordlayer.Field("id"),
		),
	} {
		name, primaryKey := name, primaryKey
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			candidate := *recordType
			candidate.PrimaryKey = primaryKey
			if columns, leadingRTK, ok := coveredPrimaryKeyColumns(&candidate); ok ||
				columns != nil || leadingRTK {
				t.Fatalf("unsafe PK exposed coverage tail %v (leadingRTK=%v)", columns, leadingRTK)
			}
		})
	}
}

func metadataIndexDefForOrderPrimaryKey(
	t *testing.T,
	primaryKey recordlayer.KeyExpression,
) *metadataIndexDef {
	return metadataIndexDefForOrderPrimaryKeyAndIndex(
		t, primaryKey, recordlayer.Field("price"), "order_price",
	)
}

func metadataIndexDefForOrderPrimaryKeyAndIndex(
	t *testing.T,
	primaryKey recordlayer.KeyExpression,
	indexKey recordlayer.KeyExpression,
	indexName string,
) *metadataIndexDef {
	t.Helper()
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(primaryKey)
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	index := recordlayer.NewIndex(indexName, indexKey)
	builder.AddIndex("Order", index)
	metadata, err := builder.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}
	return &metadataIndexDef{idx: metadata.GetIndex(index.Name), md: metadata}
}

func TestMetadataIndexDefPrimaryKeyCoverageAndOrderingAuthority(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		primaryKey recordlayer.KeyExpression
	}{
		{name: "flat", primaryKey: recordlayer.Field("order_id")},
		{
			name: "constant record type prefix",
			primaryKey: recordlayer.Concat(
				recordlayer.RecordTypeKey(), recordlayer.Field("order_id"),
			),
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			definition := metadataIndexDefForOrderPrimaryKey(t, test.primaryKey)
			columns := definition.IndexPrimaryKeyColumns()
			if len(columns) != 1 || columns[0] != "order_id" {
				t.Fatalf("PK coverage columns = %v, want [order_id]", columns)
			}
			requireEmbeddedPhysicalTypeCodes(
				t, definition.IndexPrimaryKeyComponentTypes(), values.TypeCodeLong,
			)
			primaryColumns := (&metadataPlanContext{md: definition.md}).GetPrimaryKeyColumns("Order")
			if len(primaryColumns) != 1 || primaryColumns[0] != "order_id" {
				t.Fatalf("primary scan columns = %v, want [order_id]", primaryColumns)
			}
		})
	}
}

func TestMetadataIndexDefRejectsUnsafePrimaryKeyCoverage(t *testing.T) {
	t.Parallel()

	definition := metadataIndexDefForOrderPrimaryKey(t, recordlayer.Concat(
		recordlayer.Field("order_id"), recordlayer.Literal(int64(7)),
	))
	if columns := definition.IndexPrimaryKeyColumns(); columns != nil {
		t.Fatalf("unsafe (ID,literal) PK exposed coverage columns %v", columns)
	}
	if types := definition.IndexPrimaryKeyComponentTypes(); types != nil {
		t.Fatalf("unsafe (ID,literal) PK exposed ordering types %v", types)
	}
	if columns := (&metadataPlanContext{md: definition.md}).GetPrimaryKeyColumns("Order"); columns != nil {
		t.Fatalf("unsafe primary scan metadata exposed columns %v", columns)
	}

	context := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{definition})
	candidates := context.GetMatchCandidates()
	if len(candidates) != 1 {
		t.Fatalf("candidate count = %d, want 1", len(candidates))
	}
	candidate, ok := candidates[0].(*cascades.ValueIndexScanMatchCandidate)
	if !ok {
		t.Fatalf("candidate type = %T, want value index", candidates[0])
	}
	if pk := candidate.GetPKColumnNames(); len(pk) != 0 {
		t.Fatalf("unsafe candidate retained PK coverage names %v", pk)
	}
	if _, pushable := candidate.PushValueThroughFetch(
		&values.FieldValue{Field: "ORDER_ID", Typ: values.NotNullLong},
		values.UniqueCorrelationIdentifier(), values.UniqueCorrelationIdentifier(),
	); pushable {
		t.Fatal("SELECT ORDER_ID was incorrectly coverable from (ID,literal) PK")
	}
}

func TestUnsafePrimaryKeySuffixCannotOverwriteCoveredIndexColumn(t *testing.T) {
	t.Parallel()

	definition := metadataIndexDefForOrderPrimaryKeyAndIndex(
		t,
		recordlayer.Concat(
			recordlayer.Field("order_id"), recordlayer.Literal(int64(7)),
		),
		recordlayer.Field("order_id"),
		"order_id_covering",
	)
	context := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{definition})
	candidates := context.GetMatchCandidates()
	if len(candidates) != 1 {
		t.Fatalf("candidate count = %d, want 1", len(candidates))
	}
	candidate := candidates[0].(*cascades.ValueIndexScanMatchCandidate)
	orderDomain := values.OrdinalDomainOfType(candidate.GetBaseType())
	if _, pushable := candidate.PushValueThroughFetch(
		values.NewFieldValueWithResolvedOrdinalInDomain(
			"ORDER_ID", 0, values.NotNullLong, orderDomain,
		),
		values.UniqueCorrelationIdentifier(), values.UniqueCorrelationIdentifier(),
	); !pushable {
		t.Fatal("index root ORDER_ID should remain directly coverable")
	}
	fetch, ok := candidate.ToScanPlan(nil, false).(*plans.RecordQueryFetchFromPartialRecordPlan)
	if !ok {
		t.Fatalf("scan plan = %T, want fetch over index", candidate.ToScanPlan(nil, false))
	}
	indexPlan, ok := fetch.GetInner().(*plans.RecordQueryIndexPlan)
	if !ok {
		t.Fatalf("fetch inner = %T, want index plan", fetch.GetInner())
	}
	if pk := indexPlan.GetPKColumnNames(); len(pk) != 0 {
		t.Fatalf("unsafe PK coverage names reached physical plan: %v", pk)
	}
}

func TestMetadataIndexDefSharedRecordTypePrefixStopsVisibleOrdering(t *testing.T) {
	t.Parallel()

	label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	longKind := descriptorpb.FieldDescriptorProto_TYPE_INT64
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    proto.String("shared_pk_suffix_test.proto"),
		Package: proto.String("sharedpksuffixtest"),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("First"), Field: []*descriptorpb.FieldDescriptorProto{{
				Name: proto.String("id"), Number: proto.Int32(1), Label: &label, Type: &longKind,
			}}},
			{Name: proto.String("Second"), Field: []*descriptorpb.FieldDescriptorProto{{
				Name: proto.String("id"), Number: proto.Int32(1), Label: &label, Type: &longKind,
			}}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build shared descriptor: %v", err)
	}
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(file)
	builder.GetRecordType("First").SetPrimaryKey(recordlayer.Concat(
		recordlayer.RecordTypeKey(), recordlayer.Field("id"),
	))
	builder.GetRecordType("Second").SetPrimaryKey(recordlayer.Concat(
		recordlayer.RecordTypeKey(), recordlayer.Field("id"),
	))
	index := recordlayer.NewIndex("shared_by_type", recordlayer.Field("id"))
	builder.AddMultiTypeIndex([]string{"First", "Second"}, index)
	metadata, err := builder.Build()
	if err != nil {
		t.Fatalf("build shared metadata: %v", err)
	}
	definition := &metadataIndexDef{idx: metadata.GetIndex(index.Name), md: metadata}
	columns := definition.IndexPrimaryKeyColumns()
	if len(columns) != 1 || columns[0] != "id" {
		t.Fatalf("shared PK coverage columns = %v, want [id]", columns)
	}
	requireEmbeddedPhysicalTypeCodes(
		t, definition.IndexPrimaryKeyComponentTypes(), values.TypeCodeUnknown,
	)
}

func TestMetadataIndexDefSharedPrimaryKeyWidthDisagreementIsUnknown(t *testing.T) {
	t.Parallel()

	label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	floatKind := descriptorpb.FieldDescriptorProto_TYPE_FLOAT
	doubleKind := descriptorpb.FieldDescriptorProto_TYPE_DOUBLE
	longKind := descriptorpb.FieldDescriptorProto_TYPE_INT64
	message := func(name string, idType *descriptorpb.FieldDescriptorProto_Type) *descriptorpb.DescriptorProto {
		return &descriptorpb.DescriptorProto{
			Name: proto.String(name),
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("id"), Number: proto.Int32(1), Label: &label, Type: idType},
				{Name: proto.String("k"), Number: proto.Int32(2), Label: &label, Type: &longKind},
			},
		}
	}
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:        proto.String("shared_pk_width_test.proto"),
		Package:     proto.String("sharedpkwidthtest"),
		Syntax:      proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{message("FloatRecord", &floatKind), message("DoubleRecord", &doubleKind)},
	}, nil)
	if err != nil {
		t.Fatalf("build shared-width descriptor: %v", err)
	}
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(file)
	builder.GetRecordType("FloatRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.GetRecordType("DoubleRecord").SetPrimaryKey(recordlayer.Field("id"))
	index := recordlayer.NewIndex("shared_k", recordlayer.Field("k"))
	builder.AddMultiTypeIndex([]string{"FloatRecord", "DoubleRecord"}, index)
	metadata, buildErr := builder.Build()
	if buildErr != nil {
		t.Fatalf("build shared-width metadata: %v", buildErr)
	}
	definition := &metadataIndexDef{idx: metadata.GetIndex(index.Name), md: metadata}
	if columns := definition.IndexPrimaryKeyColumns(); len(columns) != 1 || columns[0] != "id" {
		t.Fatalf("shared-width PK columns = %v, want [id]", columns)
	}
	requireEmbeddedPhysicalTypeCodes(
		t, definition.IndexPrimaryKeyComponentTypes(), values.TypeCodeUnknown,
	)
}

func TestMetadataIndexDefPrimaryKeyTypesStayAlignedThroughMiddleTrim(t *testing.T) {
	t.Parallel()

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Concat(
		recordlayer.Field("id"),
		recordlayer.Field("val_float"),
		recordlayer.Field("val_double"),
	))
	index := recordlayer.NewIndex("typed_float", recordlayer.Field("val_float"))
	builder.AddIndex("TypedRecord", index)
	metadata, err := builder.Build()
	if err != nil {
		t.Fatalf("build middle-trim metadata: %v", err)
	}
	definition := &metadataIndexDef{idx: metadata.GetIndex(index.Name), md: metadata}
	columns := definition.IndexPrimaryKeyColumns()
	if len(columns) != 3 || columns[0] != "id" ||
		columns[1] != "val_float" || columns[2] != "val_double" {
		t.Fatalf("PK columns = %v, want [id val_float val_double]", columns)
	}
	types := definition.IndexPrimaryKeyComponentTypes()
	requireEmbeddedPhysicalTypeCodes(
		t, types, values.TypeCodeLong, values.TypeCodeFloat, values.TypeCodeDouble,
	)
	ordered := plans.PhysicallyOrderedTrimmedPKSuffix(
		definition.IndexColumnNames(), columns, types,
	)
	if len(ordered) != 1 || ordered[0] != "id" {
		t.Fatalf("middle-trim ordered suffix = %v, want [id] before DOUBLE barrier", ordered)
	}
}
