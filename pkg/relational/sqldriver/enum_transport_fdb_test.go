package sqldriver_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

// TestFDB_EnumTransport drives enum fields authored in protobuf metadata through
// the production SQL planner and a real Record Layer store. The fixture is kept
// independent of SQL DDL because CREATE TYPE AS ENUM is not part of Go's DDL
// surface: the declaration below is the schema authority and dynamicpb supplies
// the records exactly as a generated protobuf application would.
func TestFDB_EnumTransport(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}

	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	req := descriptorpb.FieldDescriptorProto_LABEL_REQUIRED.Enum()
	rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	i64 := descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()
	str := descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()
	enum := descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum()
	msg := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
	const pkg = "fdb.test.enumtransport"
	typeName := func(name string) *string { return proto.String("." + pkg + "." + name) }

	fdp := &descriptorpb.FileDescriptorProto{
		Name: proto.String("enum_transport_test.proto"), Package: proto.String(pkg), Syntax: proto.String("proto2"),
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: proto.String("WorkflowState"),
			Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("ALPHA"), Number: proto.Int32(30)},
				{Name: proto.String("ZULU"), Number: proto.Int32(10)},
				{Name: proto.String("MIDDLE"), Number: proto.Int32(20)},
				{Name: proto.String("CASH__1"), Number: proto.Int32(40)},
			},
		}, {
			Name: proto.String("AliasState"), Options: &descriptorpb.EnumOptions{AllowAlias: proto.Bool(true)},
			Value: []*descriptorpb.EnumValueDescriptorProto{{Name: proto.String("FIRST"), Number: proto.Int32(1)}, {Name: proto.String("SAME"), Number: proto.Int32(1)}},
		}},
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Envelope"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("STATE"), Number: proto.Int32(1), Label: opt, Type: str},
				{Name: proto.String("COLOR"), Number: proto.Int32(2), Label: opt, Type: enum, TypeName: typeName("WorkflowState")},
				{Name: proto.String("ESCAPED"), Number: proto.Int32(3), Label: opt, Type: enum, TypeName: typeName("WorkflowState")},
			}},
			{Name: proto.String("TASK"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("ID"), Number: proto.Int32(1), Label: req, Type: i64},
				{Name: proto.String("STATE"), Number: proto.Int32(2), Label: opt, Type: enum, TypeName: typeName("WorkflowState")},
				{Name: proto.String("ENVELOPE"), Number: proto.Int32(3), Label: opt, Type: msg, TypeName: typeName("Envelope")},
				{Name: proto.String("HISTORY"), Number: proto.Int32(4), Label: rep, Type: enum, TypeName: typeName("WorkflowState")},
				{Name: proto.String("COLOR"), Number: proto.Int32(5), Label: opt, Type: str},
				{Name: proto.String("ALIAS"), Number: proto.Int32(6), Label: opt, Type: enum, TypeName: typeName("AliasState")},
			}},
			{Name: proto.String("UnionDescriptor"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("_TASK"), Number: proto.Int32(1), Label: opt, Type: msg, TypeName: typeName("TASK")},
			}},
		},
	}
	file, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("build authored descriptor: %v", err)
	}
	mb := recordlayer.NewRecordMetaDataBuilder().SetRecords(file)
	mb.GetRecordType("TASK").SetPrimaryKey(recordlayer.Field("ID"))
	mb.AddIndex("TASK", recordlayer.NewIndex("STATE_IDX", recordlayer.Field("STATE")))
	md, err := mb.Build()
	if err != nil {
		t.Fatalf("build RecordMetaData: %v", err)
	}

	ctx := context.Background()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open FDB: %v", err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name(), t.TempDir()}.Pack())
	task := md.GetRecordType("TASK").Descriptor
	stateFD := task.Fields().ByName("STATE")
	envelopeFD := task.Fields().ByName("ENVELOPE")
	historyFD := task.Fields().ByName("HISTORY")
	enumValue := func(name string) protoreflect.EnumNumber {
		v := stateFD.Enum().Values().ByName(protoreflect.Name(name))
		if v == nil {
			t.Fatalf("authored fixture has no enum member %q", name)
		}
		return v.Number()
	}
	makeTask := func(id int64, state, nested string, history ...string) proto.Message {
		m := dynamicpb.NewMessage(task)
		m.Set(task.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		if state != "" {
			m.Set(stateFD, protoreflect.ValueOfEnum(enumValue(state)))
		}
		box := dynamicpb.NewMessage(envelopeFD.Message())
		box.Set(envelopeFD.Message().Fields().ByName("STATE"), protoreflect.ValueOfString(nested))
		box.Set(envelopeFD.Message().Fields().ByName("COLOR"), protoreflect.ValueOfEnum(enumValue(nested)))
		box.Set(envelopeFD.Message().Fields().ByName("ESCAPED"), protoreflect.ValueOfEnum(enumValue("CASH__1")))
		m.Set(task.Fields().ByName("COLOR"), protoreflect.ValueOfString(state))
		m.Set(task.Fields().ByName("ALIAS"), protoreflect.ValueOfEnum(1))
		m.Set(envelopeFD, protoreflect.ValueOfMessage(box))
		list := m.Mutable(historyFD).List()
		for _, member := range history {
			list.Append(protoreflect.ValueOfEnum(enumValue(member)))
		}
		return m
	}
	fixtures := []proto.Message{
		makeTask(1, "ALPHA", "ZULU", "ZULU", "ALPHA"),
		makeTask(2, "ZULU", "ALPHA", "MIDDLE"),
		makeTask(3, "MIDDLE", "MIDDLE"),
		makeTask(4, "", "ALPHA"),
	}
	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, openErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if openErr != nil {
			return nil, openErr
		}
		for _, fixture := range fixtures {
			if _, saveErr := store.SaveRecord(fixture); saveErr != nil {
				return nil, saveErr
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("save enum fixtures: %v", err)
	}

	type result struct {
		rows    []string
		explain string
	}
	run := func(sql string, params ...any) (result, error) {
		plan, planErr := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
		if planErr != nil {
			return result{}, planErr
		}
		out := result{explain: plan.Explain()}
		_, execErr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			out.rows = nil
			store, openErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if openErr != nil {
				return nil, openErr
			}
			cur, err := executor.ExecutePlan(ctx, plan, store, executor.EmptyEvaluationContext().WithParams(params), nil, recordlayer.DefaultExecuteProperties())
			if err != nil {
				return nil, err
			}
			defer cur.Close()
			rows, err := executor.CollectAll(ctx, cur)
			if err != nil {
				return nil, err
			}
			for _, row := range rows {
				out.rows = append(out.rows, positionalPipeSprint(row))
			}
			return nil, nil
		})
		return out, execErr
	}
	assertRows := func(name, sql string, params []any, want ...string) result {
		t.Helper()
		got, err := run(sql, params...)
		if err != nil {
			t.Fatalf("%s: %q: %v\nplan: %s", name, sql, err, got.explain)
		}
		if !reflect.DeepEqual(got.rows, want) {
			t.Fatalf("%s rows = %v, want %v\nSQL: %s\nplan: %s", name, got.rows, want, sql, got.explain)
		}
		t.Logf("ENUM-EXEC %s rows=%v", name, got.rows)
		return got
	}

	// The planner/executor boundary exposes protobuf enum numbers as int64,
	// independently of member spelling. These expected values come directly
	// from the authored fixture above, not from the implementation under test.
	assertRows("direct", `SELECT "ID", "STATE" FROM Task ORDER BY "ID"`, nil,
		"1|30", "2|10", "3|20", "4|<nil>")
	assertRows("derived", `SELECT D."STATE" FROM (SELECT "ID", "STATE" FROM Task) D ORDER BY D."ID"`, nil,
		"30", "10", "20", "<nil>")
	assertRows("chained_cte", `WITH A AS (SELECT "ID", "STATE" FROM Task), B AS (SELECT * FROM A) SELECT B."STATE" FROM B ORDER BY B."ID"`, nil,
		"30", "10", "20", "<nil>")
	assertRows("enum_and_nested_string_homonym", `SELECT "STATE", "ENVELOPE"."STATE" FROM Task WHERE "ID" = 1`, nil,
		"30|ZULU")
	assertRows("nested_enum_and_string_homonym", `WITH C AS (SELECT "ENVELOPE"."COLOR" AS N, "COLOR" FROM Task WHERE "ID" = 1) SELECT N, "COLOR" FROM C`, nil, "10|ALPHA")
	assertRows("escaped_member", `SELECT "ID" FROM Task WHERE "ENVELOPE"."ESCAPED" = 'CASH$' ORDER BY "ID"`, nil, "1", "2", "3", "4")
	assertRows("aliased_number_cte", `WITH C AS (SELECT "ID", "ALIAS" FROM Task) SELECT "ALIAS" FROM C WHERE "ID" = 1`, nil, "1")

	assertRows("union_derived", `SELECT C."STATE" FROM (SELECT "STATE" FROM Task WHERE "ID" = 1 UNION ALL SELECT "STATE" FROM Task WHERE "ID" = 2) AS C ORDER BY C."STATE"`, nil, "10", "30")
	// ENUM->STRING is Go's existing decimal-carrier cast, not a Java operator.
	assertRows("enum_string_cast", `SELECT CAST("STATE" AS STRING) FROM Task WHERE "ID" = 1`, nil, "30")
	assertRows("equals", `SELECT "ID" FROM Task WHERE "STATE" = 'ZULU' ORDER BY "ID"`, nil, "2")
	assertRows("not_equals", `SELECT "ID" FROM Task WHERE "STATE" <> 'ZULU' ORDER BY "ID"`, nil, "1", "3")
	assertRows("ordered_where", `SELECT "ID" FROM Task WHERE "STATE" < 'MIDDLE' ORDER BY "ID"`, nil, "2")
	assertRows("enum_order_is_declared_number_order", `SELECT "STATE" FROM Task WHERE "STATE" IS NOT NULL ORDER BY "STATE"`, nil,
		"10", "20", "30")
	assertRows("constant_in", `SELECT "ID" FROM Task WHERE "STATE" IN ('ALPHA', 'MIDDLE') ORDER BY "ID"`, nil, "1", "3")
	assertRows("runtime_in", `SELECT "ID" FROM Task WHERE "STATE" IN (?, ?) ORDER BY "ID"`, []any{"ZULU", "MIDDLE"}, "2", "3")
	assertRows("runtime_equal", `SELECT "ID" FROM Task WHERE "STATE" = ?`, []any{"ALPHA"}, "1")
	assertRows("runtime_reversed", `SELECT "ID" FROM Task WHERE ? = "STATE"`, []any{"ZULU"}, "2")
	assertRows("null", `SELECT "ID" FROM Task WHERE "STATE" IS NULL`, nil, "4")
	assertRows("repeated_enum_unnest", `SELECT "ID", "H" FROM Task, Task."HISTORY" AS "H" ORDER BY "ID", "H"`, nil,
		"1|10", "1|30", "2|20")

	indexed := assertRows("index_equality", `SELECT "ID" FROM Task WHERE "STATE" = 'ALPHA'`, nil, "1")
	if !strings.Contains(indexed.explain, "IndexScan(STATE_IDX") {
		t.Fatalf("index equality did not use STATE_IDX: %s", indexed.explain)
	}

	for _, sql := range []string{
		`SELECT "ID" FROM Task WHERE "STATE" = 'NOT_A_MEMBER'`,
		`SELECT "ID" FROM Task WHERE "STATE" IN ('ALPHA', 'NOT_A_MEMBER')`,
	} {
		got, err := run(sql)
		var invalid *values.InvalidEnumValueError
		if !errors.As(err, &invalid) || invalid.Value != "NOT_A_MEMBER" {
			t.Fatalf("invalid enum member: %s; error=%v rows=%v plan=%s", sql, err, got.rows, got.explain)
		}
	}

	// Assert metadata separately from row values: a projection and its derived
	// twin must retain the exact enum declaration and nullable bit. The nested
	// STATE homonym remains STRING, proving this is source-specific transport.
	metadataCases := []struct {
		name string
		sql  string
	}{
		{"direct", `SELECT "STATE" FROM Task`},
		{"derived", `SELECT D."STATE" FROM (SELECT "STATE" FROM Task) D`},
		{"chained_cte", `WITH A AS (SELECT "STATE" FROM Task), B AS (SELECT * FROM A) SELECT B."STATE" FROM B`},
		{"nested_enum_homonym", `SELECT D."COLOR" FROM (SELECT "ENVELOPE"."COLOR" FROM Task) D`},
	}
	wantMembers := []values.EnumValue{{Name: "ALPHA", Number: 30}, {Name: "ZULU", Number: 10}, {Name: "MIDDLE", Number: 20}, {Name: "CASH$", Number: 40}}
	for _, tc := range metadataCases {
		plan, err := embedded.PlanRecordQueryWithMetadata(tc.sql, md, nil)
		if err != nil {
			t.Fatalf("metadata %s plan: %v", tc.name, err)
		}
		defs := embedded.ResultColumnDefsForPlan(plan, md)
		if len(defs) != 1 || defs[0].TypeName != "OTHER" || defs[0].Nullable != api.ColumnNullable {
			t.Fatalf("metadata %s ColumnDef = %+v, want one nullable OTHER (Java's JDBC enum type name)", tc.name, defs)
		}
		resultType := plan.GetResultValue().Type()
		recordType, ok := resultType.(*values.RecordType)
		if !ok || len(recordType.Fields) != 1 {
			t.Fatalf("metadata %s result type = %T %v, want one-field record", tc.name, resultType, resultType)
		}
		gotEnum, ok := recordType.Fields[0].FieldType.(*values.EnumType)
		if !ok {
			t.Fatalf("metadata %s field type = %T %v, want ENUM", tc.name, recordType.Fields[0].FieldType, recordType.Fields[0].FieldType)
		}
		if gotEnum.EnumName != pkg+".WorkflowState" || !gotEnum.Nullable || !reflect.DeepEqual(gotEnum.Values, wantMembers) {
			t.Fatalf("metadata %s enum = name %q nullable %v members %v, want %q true %v", tc.name, gotEnum.EnumName, gotEnum.Nullable, gotEnum.Values, pkg+".WorkflowState", wantMembers)
		}
	}

	plan, err := embedded.PlanRecordQueryWithMetadata(`SELECT "ENVELOPE"."STATE" FROM Task`, md, nil)
	if err != nil {
		t.Fatalf("nested homonym metadata plan: %v", err)
	}
	defs := embedded.ResultColumnDefsForPlan(plan, md)
	if len(defs) != 1 || defs[0].TypeName != "STRING" {
		t.Fatalf("nested STATE metadata = %+v, want STRING rather than the top-level ENUM homonym", defs)
	}
}
