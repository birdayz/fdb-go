package sqldriver_test

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/relational/core/embedded"
)

// TestFDB_StructMultiSegmentUnnest is the end-to-end certificate the former
// TestStructColumnIsRejectedAtDDL sentinel demanded: with CREATE TYPE AS
// STRUCT declarable (RFC-204 Phase 1), a MULTI-SEGMENT lateral unnest
// (`FROM w, w.nested.vals AS x`) is reachable from SQL, and the
// multi-segment collection bake (unnestBakedRootCollection's suffix arm)
// must flow real rows through translateUnnestJoin — descending the struct
// column AND the NullableArrayWrapper around its nullable array field.
//
// Rows are written through the record-layer API (dynamicpb): struct DML is
// Phase 2 and fails closed, so SQL INSERT cannot seed this shape yet.
func TestFDB_StructMultiSegmentUnnest(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	t.Parallel()
	ctx := context.Background()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatal(err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())

	const ddl = `CREATE TABLE w (wid BIGINT, nested nested_s, PRIMARY KEY (wid))
		CREATE TYPE AS STRUCT nested_s (vals BIGINT ARRAY, label STRING)`
	tmpl, err := embedded.BuildSchemaTemplateFromDDL(ddl)
	if err != nil {
		t.Fatalf("struct DDL must build: %v", err)
	}
	md := tmpl.Underlying()

	wDesc := md.GetRecordType("W").Descriptor
	nestedFD := wDesc.Fields().ByName("NESTED")
	nestedDesc := nestedFD.Message()
	valsFD := nestedDesc.Fields().ByName("VALS")

	makeRow := func(wid int64, label string, vals []int64) *dynamicpb.Message {
		m := dynamicpb.NewMessage(wDesc)
		m.Set(wDesc.Fields().ByName("WID"), protoreflect.ValueOfInt64(wid))
		n := dynamicpb.NewMessage(nestedDesc)
		n.Set(nestedDesc.Fields().ByName("LABEL"), protoreflect.ValueOfString(label))
		// VALS is a NULLABLE array — stored through the NullableArrayWrapper.
		wrapper := dynamicpb.NewMessage(valsFD.Message())
		list := wrapper.Mutable(valsFD.Message().Fields().Get(0)).List()
		for _, v := range vals {
			list.Append(protoreflect.ValueOfInt64(v))
		}
		n.Set(valsFD, protoreflect.ValueOfMessage(wrapper))
		m.Set(nestedFD, protoreflect.ValueOfMessage(n))
		return m
	}

	_, err = db.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		store, serr := recordlayer.NewStoreBuilder().SetContext(rctx).SetMetaDataProvider(md).SetSubspace(ks).Create()
		if serr != nil {
			return nil, serr
		}
		for _, row := range []*dynamicpb.Message{
			makeRow(1, "a", []int64{7, 8}),
			makeRow(2, "b", []int64{9}),
			makeRow(3, "c", nil), // empty (present, zero-length) array
		} {
			if _, werr := store.SaveRecord(row); werr != nil {
				return nil, werr
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed rows: %v", err)
	}

	plan, subs, perr := embedded.PlanRecordQueryWithSubqueries(
		`SELECT x FROM w, w.nested.vals AS x ORDER BY x`, md, nil)
	if perr != nil {
		t.Fatalf("multi-segment unnest must plan: %v", perr)
	}
	var got []string
	_, eerr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
		if sErr != nil {
			return nil, sErr
		}
		evalCtx, bindErr := prebindScalarSubqueries(ctx, store, subs)
		if bindErr != nil {
			return nil, bindErr
		}
		cur, cErr := executor.ExecutePlan(ctx, plan, store, evalCtx, nil, recordlayer.DefaultExecuteProperties())
		if cErr != nil {
			return nil, cErr
		}
		defer cur.Close()
		rows, rErr := executor.CollectAll(ctx, cur)
		if rErr != nil {
			return nil, rErr
		}
		for _, r := range rows {
			got = append(got, positionalNamedPipeSprint(r))
		}
		return nil, nil
	})
	if eerr != nil {
		t.Fatalf("multi-segment unnest must execute: %v", eerr)
	}
	// One row per element of each row's nested.vals; the empty array
	// contributes none (unnest of [] is empty, distinct from NULL).
	want := []string{"X=7", "X=8", "X=9"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("rows = %v, want %v (plan: %s)", got, want, plan.Explain())
	}
}
