package sqldriver_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
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
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

// buildChainedUnnestMetadata constructs a RecordMetaData with a THREE-level
// nested struct/array shape for the RFC-173 class-4 CHAINED lateral unnest
// (`FROM t, t.arr AS x, x.sub AS y`). The record T4 carries:
//
//	ID       int64 (pk)
//	SARR     repeated ELEM     — struct-array (the first unnest's array)
//	SCARR    repeated int32    — SCALAR-array (a chained-owner-is-scalar decline)
//
//	ELEM     { SUB repeated int32; K int64; SUBSTRUCT repeated ELEM2 }
//	ELEM2    { DEEP repeated int32; LEAF int64 }
//
// So a 2-chain (`T4.SARR AS x, x.SUB AS y`) unnests the struct-array element's
// own int-array SUB; a 3-chain (`… x.SUBSTRUCT AS y, y.DEEP AS z`) descends one
// struct level deeper — exercising chainedOwnerElementMessage's recursion. The
// SQL schema builder cannot express message/struct columns, so the proto is
// built dynamically (descriptorpb + protodesc.NewFile) exactly as the metadata
// builder does; records are genuine dynamicpb messages and the chained unnest
// runs the full Cascades path against real FDB.
func buildChainedUnnestMetadata(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("chained_unnest_test.proto"),
		Package: proto.String("fdb.test.chainedunnest"),
		Syntax:  proto.String("proto2"),
	}
	rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	i32 := descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum()
	i64 := descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()
	msg := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()

	elem2 := &descriptorpb.DescriptorProto{
		Name: proto.String("ELEM2"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{Name: proto.String("DEEP"), Number: proto.Int32(1), Label: rep, Type: i32},
			{Name: proto.String("LEAF"), Number: proto.Int32(2), Label: opt, Type: i64},
		},
	}
	elem := &descriptorpb.DescriptorProto{
		Name: proto.String("ELEM"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{Name: proto.String("SUB"), Number: proto.Int32(1), Label: rep, Type: i32},
			{Name: proto.String("K"), Number: proto.Int32(2), Label: opt, Type: i64},
			{
				Name: proto.String("SUBSTRUCT"), Number: proto.Int32(3), Label: rep, Type: msg,
				TypeName: proto.String(".fdb.test.chainedunnest.ELEM2"),
			},
		},
	}
	t4 := &descriptorpb.DescriptorProto{
		Name: proto.String("T4"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{Name: proto.String("ID"), Number: proto.Int32(1), Label: opt, Type: i64},
			{
				Name: proto.String("SARR"), Number: proto.Int32(2), Label: rep, Type: msg,
				TypeName: proto.String(".fdb.test.chainedunnest.ELEM"),
			},
			{Name: proto.String("SCARR"), Number: proto.Int32(3), Label: rep, Type: i32},
			// A TOP-LEVEL scalar deliberately NAMED "SUB" — the SAME bare name as
			// the ELEM element's sub-array field. The shadow-precedence pin
			// (condition 7) proves `x.SUB` reads the ELEMENT's SUB (via the
			// two-level accessor [X, SUB]), never this outer-row column.
			{Name: proto.String("SUB"), Number: proto.Int32(4), Label: opt, Type: i64},
		},
	}
	union := &descriptorpb.DescriptorProto{
		Name: proto.String("UnionDescriptor"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{
				Name: proto.String("_T4"), Number: proto.Int32(1), Label: opt, Type: msg,
				TypeName: proto.String(".fdb.test.chainedunnest.T4"),
			},
		},
	}
	fdp.MessageType = []*descriptorpb.DescriptorProto{elem2, elem, t4, union}

	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	mdBuilder := recordlayer.NewRecordMetaDataBuilder().SetRecords(fd)
	mdBuilder.SetSplitLongRecords(false)
	mdBuilder.SetStoreRecordVersions(false)
	mdBuilder.SetVersion(1)
	mdBuilder.SetRecordCountKey(recordlayer.RecordTypeKey())
	rt := mdBuilder.GetRecordType("T4")
	if rt == nil {
		t.Fatalf("record type T4 not found after SetRecords")
	}
	rt.SetPrimaryKey(recordlayer.Field("ID"))
	md, err := mdBuilder.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}
	return md
}

// TestFDB_RFC173ChainedUnnest is the RFC-173 class-4 (chained lateral unnest)
// e2e proof against real FDB. A chained unnest's OWNER is a PRIOR unnest's
// element, not a table — `FROM T4, T4.SARR AS x, x.SUB AS y` unnests the
// struct-array element x's own int-array SUB. Java lowers this as nested
// generateCorrelatedFieldAccess (an Explode-under-forEach per link); Go's
// residual composition mirrors that — translateRef recurses the left-deep join
// tree so the outer FlatMap is the first unnest and the inner Explode reads
// x.SUB off the merged element via a multi-accessor FieldValue rooted at the
// owner alias.
func TestFDB_RFC173ChainedUnnest(t *testing.T) {
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

	md := buildChainedUnnestMetadata(t)
	t4Desc := md.GetRecordType("T4").Descriptor
	sarrFD := t4Desc.Fields().ByName("SARR")
	elemDesc := sarrFD.Message()
	substructFD := elemDesc.Fields().ByName("SUBSTRUCT")
	elem2Desc := substructFD.Message()
	scarrFD := t4Desc.Fields().ByName("SCARR")

	// mkElem2 builds an ELEM2{DEEP:[…], LEAF:leaf} deepest-struct element.
	mkElem2 := func(leaf int64, deep ...int32) protoreflect.Value {
		m := dynamicpb.NewMessage(elem2Desc)
		m.Set(elem2Desc.Fields().ByName("LEAF"), protoreflect.ValueOfInt64(leaf))
		dl := m.NewField(elem2Desc.Fields().ByName("DEEP")).List()
		for _, d := range deep {
			dl.Append(protoreflect.ValueOfInt32(d))
		}
		m.Set(elem2Desc.Fields().ByName("DEEP"), protoreflect.ValueOfList(dl))
		return protoreflect.ValueOfMessage(m)
	}
	// mkElem builds an ELEM{SUB:[…], K:k, SUBSTRUCT:[…]} mid-struct element.
	mkElem := func(k int64, sub []int32, substruct ...protoreflect.Value) protoreflect.Value {
		m := dynamicpb.NewMessage(elemDesc)
		m.Set(elemDesc.Fields().ByName("K"), protoreflect.ValueOfInt64(k))
		sl := m.NewField(elemDesc.Fields().ByName("SUB")).List()
		for _, s := range sub {
			sl.Append(protoreflect.ValueOfInt32(s))
		}
		m.Set(elemDesc.Fields().ByName("SUB"), protoreflect.ValueOfList(sl))
		ssl := m.NewField(substructFD).List()
		for _, ss := range substruct {
			ssl.Append(ss)
		}
		m.Set(substructFD, protoreflect.ValueOfList(ssl))
		return protoreflect.ValueOfMessage(m)
	}
	mkT4 := func(id int64, scarr []int32, sarr ...protoreflect.Value) proto.Message {
		m := dynamicpb.NewMessage(t4Desc)
		m.Set(t4Desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		// The shadow SUB column: a fixed sentinel 999 on every row. A shadow bug
		// (reading the outer-row SUB instead of x.SUB) would surface 999s.
		m.Set(t4Desc.Fields().ByName("SUB"), protoreflect.ValueOfInt64(999))
		sl := m.NewField(sarrFD).List()
		for _, e := range sarr {
			sl.Append(e)
		}
		m.Set(sarrFD, protoreflect.ValueOfList(sl))
		cl := m.NewField(scarrFD).List()
		for _, c := range scarr {
			cl.Append(protoreflect.ValueOfInt32(c))
		}
		m.Set(scarrFD, protoreflect.ValueOfList(cl))
		return m
	}

	_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if sErr != nil {
			return nil, sErr
		}
		recs := []proto.Message{
			// ID=1: two SARR elements; first has SUB [100,200] + two SUBSTRUCT
			// (DEEP [11,12] and [13]); second has SUB [300] + no SUBSTRUCT.
			mkT4(1, []int32{1, 2, 3},
				mkElem(7, []int32{100, 200}, mkElem2(1, 11, 12), mkElem2(2, 13)),
				mkElem(8, []int32{300}),
			),
			// ID=2: one SARR element, SUB [400], SUBSTRUCT DEEP [20].
			mkT4(2, nil, mkElem(9, []int32{400}, mkElem2(5, 20))),
			// ID=3: one SARR element but EMPTY SUB and EMPTY SUBSTRUCT → the
			// chained inner unnest yields zero rows (NULL/empty owner element).
			mkT4(3, nil, mkElem(10, nil)),
			// ID=4: EMPTY SARR → the outer unnest yields zero rows.
			mkT4(4, nil),
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
				m, _ := r.Datum.(map[string]any)
				out = append(out, m)
			}
			return nil, nil
		})
		if eerr != nil {
			t.Fatalf("exec %q: %v", sql, eerr)
		}
		return explain, out
	}
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
	// planErr expects a planning error and returns its SQLSTATE code.
	planErr := func(t *testing.T, sql string) api.ErrorCode {
		t.Helper()
		_, perr := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
		if perr == nil {
			t.Fatalf("plan %q: expected error, got nil", sql)
		}
		var se *api.Error
		if errors.As(perr, &se) {
			return se.Code
		}
		t.Fatalf("plan %q: error %v is not an *api.Error", sql, perr)
		return ""
	}

	// ALIASLESS chained-unnest CTE under an enclosing ON (review-caught,
	// enclosed-CTE round 8): the ON-only derivation's leg walk recorded the
	// FLATTENED name ("T4.SARR") as the first unnest's binding, while the
	// scope exposes the EFFECTIVE alias (unnestAliases: last segment, SARR)
	// — so the next chain link ("SARR"."SUB") classified opaque and the CTE
	// spuriously 0AF00'd under ON. The walk now records the effective alias.
	// Real ON matches (T2.ID echoes VID) discriminate against an ON drop.
	t.Run("aliasless-chain CTE under ON derives and answers", func(t *testing.T) {
		const q = `WITH "V" AS (SELECT "ID" AS "VID", "Y" FROM T4, T4."SARR", "SARR"."SUB" AS "Y") SELECT "V"."Y", "T2"."ID" FROM "V" LEFT JOIN T4 AS "T2" ON "V"."VID" = "T2"."ID"`
		_, rows := queryRows(t, q)
		got := make([]string, 0, len(rows))
		for _, m := range rows {
			got = append(got, fmt.Sprintf("%v|%v", m["T2.ID"], m["V.Y"]))
		}
		sort.Strings(got)
		want := []string{"1|100", "1|200", "1|300", "2|400"}
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
			t.Fatalf("rows = %v, want %v", got, want)
		}
	})
	t.Run("2-chain unnests the struct element's own int-array", func(t *testing.T) {
		const q = `SELECT "ID", "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y"`
		assertColumns(t, q, []string{"ID", "Y"})
		explain, rows := queryRows(t, q)
		// Condition 6 — the gather-decline pin. A chained unnest lowers to NESTED
		// FlatMap-over-FlatMap (each link its own Explode-under-forEach), never a
		// FLAT gathered inner cluster. translateJoin routes j.Right=unnest to
		// translateUnnestJoin BEFORE the ordinal-seed gather, and
		// gatherInnerClusterLegs keeps an unnest-right join as ONE opaque leg
		// (never recurses into it). If the gather had absorbed the unnest legs
		// the plan would be a flat multi-leg select — assert the nested shape.
		if !strings.Contains(explain, "FlatMap(outer=FlatMap(") {
			t.Fatalf("2-chain must lower to NESTED FlatMaps (gather-decline); plan=%s", explain)
		}
		if strings.Count(explain, "Explode(") != 2 {
			t.Fatalf("2-chain must have exactly 2 Explode legs (one per link); plan=%s", explain)
		}
		got := make([]string, 0, len(rows))
		for _, m := range rows {
			got = append(got, fmt.Sprintf("%v|%v", m["ID"], m["Y"]))
		}
		sort.Strings(got)
		want := []string{"1|100", "1|200", "1|300", "2|400"}
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
			t.Fatalf("2-chain rows\n got=%v\nwant=%v", got, want)
		}
	})

	t.Run("3-chain descends one struct level deeper", func(t *testing.T) {
		const q = `SELECT "ID", "Z" FROM T4, T4."SARR" AS "X", "X"."SUBSTRUCT" AS "Y", "Y"."DEEP" AS "Z"`
		assertColumns(t, q, []string{"ID", "Z"})
		_, rows := queryRows(t, q)
		got := make([]string, 0, len(rows))
		for _, m := range rows {
			got = append(got, fmt.Sprintf("%v|%v", m["ID"], m["Z"]))
		}
		sort.Strings(got)
		want := []string{"1|11", "1|12", "1|13", "2|20"}
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
			t.Fatalf("3-chain rows\n got=%v\nwant=%v", got, want)
		}
	})

	t.Run("empty owner element and empty outer array yield zero rows", func(t *testing.T) {
		// ID=3 (empty SUB) and ID=4 (empty SARR) must contribute NO rows — the
		// chained inner unnest over an empty/NULL owner element is a no-op.
		_, rows := queryRows(t, `SELECT "ID", "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y"`)
		for _, m := range rows {
			if fmt.Sprintf("%v", m["ID"]) == "3" || fmt.Sprintf("%v", m["ID"]) == "4" {
				t.Fatalf("ID 3/4 (empty owner) leaked a row: %v", m)
			}
		}
	})

	t.Run("WITH ORDINALITY on the inner chained unnest is 1-based per owner element", func(t *testing.T) {
		const q = `SELECT "ID", "Y", "O" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" AT "O"`
		assertColumns(t, q, []string{"ID", "Y", "O"})
		_, rows := queryRows(t, q)
		got := make([]string, 0, len(rows))
		for _, m := range rows {
			got = append(got, fmt.Sprintf("%v|%v|%v", m["ID"], m["Y"], m["O"]))
		}
		sort.Strings(got)
		// id1 elem0 SUB[100,200] → O 1,2; id1 elem1 SUB[300] → O 1; id2 SUB[400] → O 1.
		want := []string{"1|100|1", "1|200|2", "1|300|1", "2|400|1"}
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
			t.Fatalf("WITH ORDINALITY chained\n got=%v\nwant=%v", got, want)
		}
	})

	t.Run("WITH ORDINALITY at BOTH chain levels binds independent ordinals", func(t *testing.T) {
		const q = `SELECT "ID", "Y", "OX", "OY" FROM T4, T4."SARR" AS "X" AT "OX", "X"."SUB" AS "Y" AT "OY"`
		assertColumns(t, q, []string{"ID", "Y", "OX", "OY"})
		_, rows := queryRows(t, q)
		got := make([]string, 0, len(rows))
		for _, m := range rows {
			got = append(got, fmt.Sprintf("%v|%v|%v|%v", m["ID"], m["Y"], m["OX"], m["OY"]))
		}
		sort.Strings(got)
		// id1: elem OX=1 (SUB 100,200 → OY 1,2); elem OX=2 (SUB 300 → OY 1).
		// id2: elem OX=1 (SUB 400 → OY 1).
		want := []string{"1|100|1|1", "1|200|1|2", "1|300|2|1", "2|400|1|1"}
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
			t.Fatalf("WITH ORDINALITY both levels\n got=%v\nwant=%v", got, want)
		}
	})

	t.Run("SELECT star exposes outer columns plus both chain element columns", func(t *testing.T) {
		// The outer T4 columns (ID, SARR, SCARR, SUB) plus the two chained element
		// bindings X (the whole ELEM struct) and Y (the SUB scalar), in FROM order.
		assertColumns(t, `SELECT * FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y"`,
			[]string{"ID", "SARR", "SCARR", "SUB", "X", "Y"})
	})

	t.Run("shadow precedence: x.SUB reads the element field, not the same-named outer column", func(t *testing.T) {
		// Condition 7 — shadow/precedence. T4 has a TOP-LEVEL scalar column also
		// named SUB (=999 on every row). The chained collection descends the
		// two-level accessor [X, SUB] rooted at the outer row, so `x.SUB` reads
		// the ELEMENT's SUB array — NEVER the outer-row SUB. A shadow bug
		// (collapsing to a bare [SUB] read) would surface 999s.
		_, rows := queryRows(t, `SELECT "ID", "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y"`)
		for _, m := range rows {
			if fmt.Sprintf("%v", m["Y"]) == "999" {
				t.Fatalf("x.SUB read the shadowing outer column T4.SUB (999) instead of the element field: %v", m)
			}
		}
		// And the element values are exactly the SUB arrays (not the outer 999).
		got := make([]string, 0, len(rows))
		for _, m := range rows {
			got = append(got, fmt.Sprintf("%v", m["Y"]))
		}
		sort.Strings(got)
		if want := "[100 200 300 400]"; fmt.Sprintf("%v", got) != want {
			t.Fatalf("shadow-precedence Y values\n got=%v\nwant=%v", got, want)
		}
	})

	t.Run("chain rooted at an AT ordinal alias declines loudly, never silent zero rows", func(t *testing.T) {
		// `t.arr AS x AT o, o.sub AS y` roots the second unnest on the AT ORDINAL
		// alias `o` (a scalar bigint), not the AS element `x`. `o.sub` is a field
		// access on a scalar — Java rejects it as UNDEFINED_COLUMN. Go must NOT
		// misclassify `o` as the chain owner and silently return zero rows:
		// FindOwnerUnnest matches only the AS element alias, and
		// IsUnnestOrdinalAlias surfaces the honest 42703 (never the generic 0AF00
		// fallback, and never a silent empty result).
		const q = `SELECT "Y" FROM T4, T4."SARR" AS "X" AT "O", "O"."SUB" AS "Y"`
		if code := planErr(t, q); code != api.ErrCodeUndefinedColumn {
			t.Fatalf("AT-ordinal chain owner: got SQLSTATE %s, want 42703 (UNDEFINED_COLUMN) — never a silent zero-row plan", code)
		}
		// And the legit sibling `x.SUB` (AS element) STILL chains — dropping the
		// AtAlias match must not break the real element-rooted chain.
		_, rows := queryRows(t, `SELECT "ID", "Y" FROM T4, T4."SARR" AS "X" AT "O", "X"."SUB" AS "Y"`)
		if len(rows) != 4 {
			t.Fatalf("legit x.SUB chain alongside AT o: got %d rows, want 4", len(rows))
		}
	})

	t.Run("chain rooted at a CTE owner declines loudly (UNSUPPORTED_QUERY)", func(t *testing.T) {
		// Condition 4 — a chained unnest whose chain bottoms at a CTE/derived
		// owner (not a real-table scan) declines LOUDLY. Here the first link
		// `c.SARR AS X` is a class-3 derived-owner unnest; the SECOND link
		// `X.SUB AS Y` is chained, but resolving X's element bottoms at the CTE
		// `c` (not a base-table scan), so chainedOwnerElementMessage declines and
		// the translator raises UNSUPPORTED_QUERY rather than resolve through the
		// CTE body (class-3×/class-4 composition is a separate follow-on).
		const q = `WITH "C" AS (SELECT * FROM T4) SELECT "Y" FROM "C", "C"."SARR" AS "X", "X"."SUB" AS "Y"`
		if code := planErr(t, q); code != api.ErrCodeUnsupportedQuery {
			t.Fatalf("CTE-rooted chain: got SQLSTATE %s, want 0AF00 (UNSUPPORTED_QUERY)", code)
		}
	})

	t.Run("chain rooted at a DERIVED-TABLE owner declines loudly (UNSUPPORTED_QUERY)", func(t *testing.T) {
		// Condition 4, derived-table primary form: `(SELECT * FROM T4) AS D` is a
		// derived owner; the chained `X.SUB AS Y` bottoms at D (not a base scan),
		// so chainedOwnerElementMessage's real-table branch (guarded by
		// outerSourceIsDerivedTable) declines — never resolving against a real
		// same-named table's descriptor (the P2a shadow the class-3 structural
		// guard closes). Loud 0AF00, never wrong-type metadata or wrong rows.
		const q = `SELECT "Y" FROM (SELECT * FROM T4) AS "D", "D"."SARR" AS "X", "X"."SUB" AS "Y"`
		if code := planErr(t, q); code != api.ErrCodeUnsupportedQuery {
			t.Fatalf("derived-owner chain: got SQLSTATE %s, want 0AF00 (UNSUPPORTED_QUERY)", code)
		}
	})

	t.Run("multi-segment chained sub-path (x.a.b) declines with its OWN cause", func(t *testing.T) {
		// A multi-HOP sub-path on the element (`x.SUBSTRUCT.DEEP AS y`, 3 segments)
		// is a Go reach gap Java DOES support — it must decline with the honest
		// "multi-segment" message, not be mislabeled a CTE/derived-root decline
		// (both are 0AF00; the message is the distinguishing signal).
		const q = `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUBSTRUCT"."DEEP" AS "Y"`
		_, perr := embedded.PlanRecordQueryWithMetadata(q, md, nil)
		if perr == nil {
			t.Fatalf("multi-segment sub-path: expected a loud decline, got nil")
		}
		var se *api.Error
		if !errors.As(perr, &se) || se.Code != api.ErrCodeUnsupportedQuery {
			t.Fatalf("multi-segment sub-path: got %v, want 0AF00 (UNSUPPORTED_QUERY)", perr)
		}
		if !strings.Contains(se.Message, "multi-segment") {
			t.Fatalf("multi-segment sub-path: message %q should name the real cause (multi-segment), not a CTE/derived-root decline", se.Message)
		}
	})

	t.Run("chained owner that is a SCALAR element → UNDEFINED_COLUMN", func(t *testing.T) {
		// SCARR is a scalar int-array; its element x is a scalar with no field
		// SUB — Java's lookupNestedField soft-misses → UNDEFINED_COLUMN (42703).
		if code := planErr(t, `SELECT "Y" FROM T4, T4."SCARR" AS "X", "X"."SUB" AS "Y"`); code != api.ErrCodeUndefinedColumn {
			t.Fatalf("scalar-element chained: got SQLSTATE %s, want 42703 (UNDEFINED_COLUMN)", code)
		}
	})

	t.Run("chained sub-field absent on the owner element → UNDEFINED_COLUMN", func(t *testing.T) {
		if code := planErr(t, `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."NOPE" AS "Y"`); code != api.ErrCodeUndefinedColumn {
			t.Fatalf("absent-sub chained: got SQLSTATE %s, want 42703 (UNDEFINED_COLUMN)", code)
		}
	})

	t.Run("chained sub-field present but NOT repeated → INVALID_COLUMN_REFERENCE", func(t *testing.T) {
		// x.K is a scalar int64 on the ELEM element (present but not repeated) —
		// Java's generateCorrelatedFieldAccess "repeated type" assert (42F10).
		if code := planErr(t, `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."K" AS "Y"`); code != api.ErrCodeInvalidColumnReference {
			t.Fatalf("nonarray-sub chained: got SQLSTATE %s, want 42F10 (INVALID_COLUMN_REFERENCE)", code)
		}
	})

	// RFC-173 c3 review fix (wrong value): the chained path bypassed translateUnnestJoin's
	// AS==AT overwrite guard. `... AS "Y" AT "Y"` appends the element and the ordinal
	// under the SAME name; the map-keyed result silently overwrites the element with the
	// ordinal, so `SELECT "Y"` returns the ordinal, not the unnested value. Java binds AS
	// and AT to distinct quantifier columns (a duplicate is a binding error), so Go must
	// reject cleanly. The guard now fires on the chained path too.
	t.Run("chained AS==AT dup alias rejects (element/ordinal overwrite)", func(t *testing.T) {
		const q = `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" AT "Y"`
		if code := planErr(t, q); code != api.ErrCodeDuplicateAlias {
			t.Fatalf("chained AS==AT dup: got SQLSTATE %s, want 42710 (DUPLICATE_ALIAS) — the ordinal must not silently overwrite the element", code)
		}
		// The distinct-alias sibling still chains and answers (the guard is scoped to
		// the collision, not the WITH-ORDINALITY chain itself).
		if _, rows := queryRows(t, `SELECT "ID", "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" AT "O"`); len(rows) == 0 {
			t.Fatal("distinct AS/AT chain returned zero rows — the dup guard must not block valid WITH ORDINALITY chains")
		}
	})
}
