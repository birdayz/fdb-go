package sqldriver_test

import (
	"context"
	"fmt"
	"sort"
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
	"fdb.dev/pkg/relational/core/embedded"
)

// buildStructArrayMetadata constructs a RecordMetaData for a record type TS that
// carries a STRUCT-ARRAY column (a `repeated SItem` message field) — the shape
// the metadata.NewSchemaTemplateBuilder() SQL schema path cannot express
// (datatypeToProtoFieldType has no message/struct case). The proto is built
// dynamically exactly as metadata.Builder.buildFileDescriptor does (descriptorpb
// + protodesc.NewFile), so the record type, its nested struct element, and the
// UnionDescriptor are all real proto descriptors; records are written as genuine
// dynamicpb messages and the unnest SQL runs the full Cascades path. RFC-173 W4c.
func buildStructArrayMetadata(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("struct_unnest_test.proto"),
		Package: proto.String("fdb.test.structunnest"),
		Syntax:  proto.String("proto2"),
	}
	// SItem: the STRUCT element of the array (two scalar fields).
	sitem := &descriptorpb.DescriptorProto{
		Name: proto.String("SItem"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{
				Name: proto.String("SKU"), Number: proto.Int32(1),
				Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:  descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			},
			{
				Name: proto.String("QTY"), Number: proto.Int32(2),
				Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:  descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
			},
		},
	}
	// TS: the record type. ITEMS is the repeated STRUCT (message) column.
	ts := &descriptorpb.DescriptorProto{
		Name: proto.String("TS"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{
				Name: proto.String("ID"), Number: proto.Int32(1),
				Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:  descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
			},
			{
				Name: proto.String("ITEMS"), Number: proto.Int32(2),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: proto.String(".fdb.test.structunnest.SItem"),
			},
			{
				Name: proto.String("NAME"), Number: proto.Int32(3),
				Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:  descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			},
		},
	}
	union := &descriptorpb.DescriptorProto{
		Name: proto.String("UnionDescriptor"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{
				Name: proto.String("_TS"), Number: proto.Int32(1),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: proto.String(".fdb.test.structunnest.TS"),
			},
		},
	}
	fdp.MessageType = []*descriptorpb.DescriptorProto{sitem, ts, union}

	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	mdBuilder := recordlayer.NewRecordMetaDataBuilder().SetRecords(fd)
	mdBuilder.SetSplitLongRecords(false)
	mdBuilder.SetStoreRecordVersions(false)
	mdBuilder.SetVersion(1)
	mdBuilder.SetRecordCountKey(recordlayer.RecordTypeKey())
	rt := mdBuilder.GetRecordType("TS")
	if rt == nil {
		t.Fatalf("record type TS not found after SetRecords")
	}
	rt.SetPrimaryKey(recordlayer.Field("ID"))
	md, err := mdBuilder.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}
	return md
}

// structPair renders a struct-array element datum (a *dynamicpb.Message SItem)
// as "SKU:QTY" — proving the WHOLE struct flowed (both fields), not a flattened
// scalar. A non-message datum renders with its Go type so a regression that
// changes the element representation is visible in the failure.
func structPair(v any) string {
	m, ok := v.(proto.Message)
	if !ok {
		return fmt.Sprintf("<not-a-struct %T:%v>", v, v)
	}
	refl := m.ProtoReflect()
	desc := refl.Descriptor()
	sku := ""
	if fd := desc.Fields().ByName("SKU"); fd != nil && refl.Has(fd) {
		sku = refl.Get(fd).String()
	}
	var qty int64
	if fd := desc.Fields().ByName("QTY"); fd != nil && refl.Has(fd) {
		qty = refl.Get(fd).Int()
	}
	return fmt.Sprintf("%s:%d", sku, qty)
}

// TestFDB_ArrayUnnestStruct is the RFC-173 W4c STRUCT-ARRAY (array-of-rows)
// lateral-unnest e2e proof. It pins the design's WHOLE-OBJECT ruling for a
// STRUCT-array element (`FROM t, t.structArr AS x`): Go keeps a whole-object
// binding — deliberately NOT porting Java's field-flattening. The
// element flows as ONE struct column (`SELECT x` / `SELECT *`), and WITH
// ORDINALITY binds the whole struct element alongside the 1-based ordinal.
//
// The struct element is UnknownType at the seed (unnestArrayElementType returns
// UnknownType for a message element — "the runtime flows the raw element"), so
// it is a bare QOV over a non-record type: a RawLeg that binds the whole flowed
// element raw at the executor (ordinalJoinBirth.RawLegs), never adapted/flattened
// to a positional row.
//
// NOTE on `SELECT x.field`: composite field extraction on the whole-struct
// element (`x.SKU`) is NOT pinned here. It is unimplemented ENGINE-WIDE — nested
// struct-field access fails identically for a regular (non-array) struct column
// (`SELECT hdr.SKU`), and the whole-object `SELECT x` works precisely because the
// raw element is not field-readable (the executor returns the whole bound object
// for a non-map binding). Making `x.field` extract a field while `x` still binds
// the whole struct is a query-engine design change (composite field extraction +
// whole-object/field-access disambiguation), not a W4c test gap. Pinning it as a
// passing assertion would require that feature; asserting the current
// non-resolution would pin a limitation as if desired. Neither is done here.
func TestFDB_ArrayUnnestStruct(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())

	md := buildStructArrayMetadata(t)
	tsDesc := md.GetRecordType("TS").Descriptor
	itemsFD := tsDesc.Fields().ByName("ITEMS")
	sitemDesc := itemsFD.Message()

	mkItem := func(sku string, qty int64) protoreflect.Value {
		im := dynamicpb.NewMessage(sitemDesc)
		im.Set(sitemDesc.Fields().ByName("SKU"), protoreflect.ValueOfString(sku))
		im.Set(sitemDesc.Fields().ByName("QTY"), protoreflect.ValueOfInt64(qty))
		return protoreflect.ValueOfMessage(im)
	}
	mkTS := func(id int64, name string, items ...protoreflect.Value) proto.Message {
		m := dynamicpb.NewMessage(tsDesc)
		m.Set(tsDesc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(tsDesc.Fields().ByName("NAME"), protoreflect.ValueOfString(name))
		list := m.NewField(itemsFD).List()
		for _, it := range items {
			list.Append(it)
		}
		m.Set(itemsFD, protoreflect.ValueOfList(list))
		return m
	}

	_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if sErr != nil {
			return nil, sErr
		}
		recs := []proto.Message{
			mkTS(1, "n1", mkItem("a", 10), mkItem("b", 20)),
			mkTS(2, "n2", mkItem("c", 30)),
			mkTS(3, "n3"), // empty ITEMS → NULL array → no unnest rows
		}
		for _, r := range recs {
			if _, e := store.SaveRecord(r); e != nil {
				return nil, e
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// queryRows plans + executes a SELECT and returns the per-row datum maps in
	// execution order.
	queryRows := func(t *testing.T, sql string) (string, []map[string]any) {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
		if perr != nil {
			t.Fatalf("plan %q: %v", sql, perr)
		}
		explain := plan.Explain()
		var out []map[string]any
		_, eerr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, sErr := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if sErr != nil {
				return nil, sErr
			}
			cursor, cErr := executor.ExecutePlan(ctx, plan, store,
				executor.EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
			if cErr != nil {
				return nil, cErr
			}
			defer cursor.Close()
			rows, rErr := executor.CollectAll(ctx, cursor)
			if rErr != nil {
				return nil, rErr
			}
			for _, r := range rows {
				m, _ := executor.RowValue(r).(map[string]any)
				out = append(out, m)
			}
			return nil, nil
		})
		if eerr != nil {
			t.Fatalf("exec %q: %v", sql, eerr)
		}
		return explain, out
	}

	// assertColumns pins the user-visible RESULT-SET COLUMN labels (the metadata
	// the driver returns via rows.Columns(), from the same production
	// ResultColumnLabelsForPlan the live path uses). This is the load-bearing
	// pin for "the struct element is ONE column, NOT flattened": a flattening
	// bug would expand X into SKU/QTY here.
	assertColumns := func(t *testing.T, sql string, want []string) {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
		if perr != nil {
			t.Fatalf("plan %q: %v", sql, perr)
		}
		got := embedded.ResultColumnLabelsForPlan(plan, md)
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
			t.Fatalf("columns %q\n got=%v\nwant=%v\nplan=%s", sql, got, want, plan.Explain())
		}
	}

	t.Run("SELECT x binds the whole struct element as one column", func(t *testing.T) {
		// (c) Whole-object: the element is ONE struct column X, never flattened.
		assertColumns(t, `SELECT "X" FROM TS, TS."ITEMS" AS "X"`, []string{"X"})
		_, rows := queryRows(t, `SELECT "X" FROM TS, TS."ITEMS" AS "X"`)
		got := make([]string, 0, len(rows))
		for _, m := range rows {
			// The element datum is the WHOLE struct (SKU+QTY together) — both
			// fields present in a single column value.
			got = append(got, structPair(m["X"]))
		}
		sort.Strings(got)
		want := []string{"a:10", "b:20", "c:30"}
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
			t.Fatalf("SELECT X struct values\n got=%v\nwant=%v", got, want)
		}
	})

	t.Run("SELECT id, x pairs id with the whole struct element", func(t *testing.T) {
		assertColumns(t, `SELECT "ID", "X" FROM TS, TS."ITEMS" AS "X"`, []string{"ID", "X"})
		_, rows := queryRows(t, `SELECT "ID", "X" FROM TS, TS."ITEMS" AS "X"`)
		got := make([]string, 0, len(rows))
		for _, m := range rows {
			got = append(got, fmt.Sprintf("%v|%s", m["ID"], structPair(m["X"])))
		}
		sort.Strings(got)
		want := []string{"1|a:10", "1|b:20", "2|c:30"}
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
			t.Fatalf("SELECT ID,X\n got=%v\nwant=%v", got, want)
		}
	})

	t.Run("SELECT star keeps the element as one struct column not flattened", func(t *testing.T) {
		// (c) The KEY pin: `SELECT *` over a struct-array unnest exposes the
		// element as ONE column X — NOT the flattened struct subfields (no SKU /
		// QTY columns). The outer table's own columns (ID, ITEMS, NAME) plus the
		// element X, in FROM order.
		assertColumns(t, `SELECT * FROM TS, TS."ITEMS" AS "X"`,
			[]string{"ID", "ITEMS", "NAME", "X"})
	})

	t.Run("WITH ORDINALITY binds whole struct element plus 1-based ordinal", func(t *testing.T) {
		// The struct element (whole) flows alongside the 1-based ordinal O, which
		// resets per outer row. id1 has two elements (O 1,2); id2 one (O 1).
		assertColumns(t, `SELECT "ID", "X", "O" FROM TS, TS."ITEMS" AS "X" AT "O"`,
			[]string{"ID", "X", "O"})
		_, rows := queryRows(t, `SELECT "ID", "X", "O" FROM TS, TS."ITEMS" AS "X" AT "O"`)
		got := make([]string, 0, len(rows))
		for _, m := range rows {
			got = append(got, fmt.Sprintf("%v|%s|%v", m["ID"], structPair(m["X"]), m["O"]))
		}
		sort.Strings(got)
		want := []string{"1|a:10|1", "1|b:20|2", "2|c:30|1"}
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
			t.Fatalf("WITH ORDINALITY struct\n got=%v\nwant=%v", got, want)
		}
	})

	t.Run("WITH ORDINALITY star keeps one struct column and the ordinal", func(t *testing.T) {
		// `SELECT *` under ordinality: the element X stays ONE struct column and O
		// is the added ordinal column — neither flattens the struct.
		assertColumns(t, `SELECT * FROM TS, TS."ITEMS" AS "X" AT "O"`,
			[]string{"ID", "ITEMS", "NAME", "X", "O"})
	})
}
