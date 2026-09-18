package sqldriver_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

// A STAR-BODIED CTE/derived-table body used as an OPAQUE ordinal JOIN LEG
// (derivedBodyStarOrdinalLeg): a projection-less single-link unnest body
// (`WITH S AS (SELECT * FROM t, t.arr AS x) …`) referenced as a JOIN LEG
// gates ordinal — the parent seeds positionally over the body's own seed
// labels, rather than resolving the parent and the enclosed body by name.
// This file pins the star-body shape family's rows.
//
// Data (SCARR is the scalar array; T4(11) has an EMPTY SCARR so it vanishes
// from every unnest; the top-level SUB scalar is the shadow column):
//
//	T4(1)  SUB=999 SCARR=[100,200]
//	T4(2)  SUB=20  SCARR=[300]
//	T4(11) SUB=999 SCARR=[]
//
// CC scans the same table (IDs 1, 2, 11).
func TestFDB_StarBodyCTEJoinLeg(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatal(err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())

	md := buildChainedUnnestMetadata(t)
	if err := saveStarCTERows(ctx, db, ks, md); err != nil {
		t.Fatal(err)
	}

	run := func(name, q string) []string {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadata(q, md, nil)
		if perr != nil {
			t.Fatalf("%s: plan error: %v\n  sql: %s", name, perr, q)
		}
		var out []string
		_, eerr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if sErr != nil {
				return nil, sErr
			}
			cur, cErr := executor.ExecutePlan(ctx, plan, store, executor.EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
			if cErr != nil {
				return nil, cErr
			}
			defer cur.Close()
			rows, rErr := executor.CollectAll(ctx, cur)
			if rErr != nil {
				return nil, rErr
			}
			for _, r := range rows {
				out = append(out, positionalNamedPipeSprint(r))
			}
			return nil, nil
		})
		if eerr != nil {
			t.Fatalf("%s: exec error: %v\n  sql: %s", name, eerr, q)
		}
		sort.Strings(out)
		return out
	}
	want := func(name, q string, exp []string) {
		t.Helper()
		got := run(name, q)
		sort.Strings(exp)
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", exp) {
			t.Errorf("%s: rows = %v, want %v\n  sql: %s", name, got, exp, q)
		}
	}
	ambiguous := func(name, q string) {
		t.Helper()
		_, err := embedded.PlanRecordQueryWithMetadata(q, md, nil)
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeAmbiguousColumn {
			t.Errorf("%s: error = %v, want 42702\n  sql: %s", name, err, q)
		}
	}

	const starCTE = `WITH "S" AS (SELECT * FROM T4, T4."SCARR" AS "X") `

	// Qualified reads over the enclosed star leg. S rows:
	// (1,100),(1,200),(2,300) — T4(11)'s empty SCARR contributes nothing —
	// × 3 CC rows.
	want("enclosed_qualified", starCTE+`SELECT "S"."ID", "S"."X" FROM "S", T4 AS "CC"`, []string{
		"ID=1|X=100", "ID=1|X=100", "ID=1|X=100",
		"ID=1|X=200", "ID=1|X=200", "ID=1|X=200",
		"ID=2|X=300", "ID=2|X=300", "ID=2|X=300",
	})

	// A BARE unique-name read over the enclosed leg.
	want("enclosed_bare_x", starCTE+`SELECT "X" FROM "S", T4 AS "CC"`, []string{
		"X=100", "X=100", "X=100",
		"X=200", "X=200", "X=200",
		"X=300", "X=300", "X=300",
	})

	// WITH ORDINALITY in the body: (X,O) = (100,1),(200,2),(300,1), × 3 CC rows.
	want("at_body", `WITH "S" AS (SELECT * FROM T4, T4."SCARR" AS "X" AT "O") SELECT "S"."X", "S"."O" FROM "S", T4 AS "CC"`, []string{
		"X=100|O=1", "X=100|O=1", "X=100|O=1",
		"X=200|O=2", "X=200|O=2", "X=200|O=2",
		"X=300|O=1", "X=300|O=1", "X=300|O=1",
	})

	// A body WHERE (plain filter above the unnest join): S = (1,200),(2,300).
	want("where_body", `WITH "S" AS (SELECT * FROM T4, T4."SCARR" AS "X" WHERE "X" > 150) SELECT "S"."ID", "S"."X" FROM "S", T4 AS "CC"`, []string{
		"ID=1|X=200", "ID=1|X=200", "ID=1|X=200",
		"ID=2|X=300", "ID=2|X=300", "ID=2|X=300",
	})

	// The DERIVED-TABLE twin (LogicalCTE directly in leg position). Its runtime
	// row's output field names are the bare column names. The WITH form retains
	// its authored S.ID/S.X source identity only while logical alternatives are
	// interned; its final SQL projection publishes the same bare [ID X] schema.
	want("derived_twin", `SELECT "S"."ID", "S"."X" FROM (SELECT * FROM T4, T4."SCARR" AS "X") AS "S", T4 AS "CC"`, []string{
		"ID=1|X=100", "ID=1|X=100", "ID=1|X=100",
		"ID=1|X=200", "ID=1|X=200", "ID=1|X=200",
		"ID=2|X=300", "ID=2|X=300", "ID=2|X=300",
	})
	t.Run("driver_labels_bare_both_forms", func(t *testing.T) {
		for _, q := range []string{
			`SELECT "S"."ID", "S"."X" FROM (SELECT * FROM T4, T4."SCARR" AS "X") AS "S", T4 AS "CC"`,
			starCTE + `SELECT "S"."ID", "S"."X" FROM "S", T4 AS "CC"`,
		} {
			plan, perr := embedded.PlanRecordQueryWithMetadata(q, md, nil)
			if perr != nil {
				t.Fatalf("plan %q: %v", q, perr)
			}
			if got := fmt.Sprintf("%v", embedded.ResultColumnLabelsForPlan(plan, md)); got != "[ID X]" {
				t.Fatalf("driver labels = %s, want [ID X] (the name model's labels)\n  sql: %s", got, q)
			}
		}
	})

	// Record/scalar/ordinal element publication preserves every visible output.
	// When an element or AT alias repeats the outer table's SUB column, a
	// reference through the CTE/derived boundary is ambiguous exactly as Java's
	// per-attribute lookup reports; no shadow-preference may silently choose one.
	ambiguous("colliding_label", `WITH "S2" AS (SELECT * FROM T4, T4."SCARR" AS "SUB") SELECT "S2"."SUB" FROM "S2", T4 AS "CC"`)
	ambiguous("colliding_derived_twin", `SELECT "S2"."SUB" FROM (SELECT * FROM T4, T4."SCARR" AS "SUB") AS "S2", T4 AS "CC"`)
	ambiguous("colliding_at_alias", `WITH "S2" AS (SELECT * FROM T4, T4."SCARR" AS "E" AT "SUB") SELECT "S2"."SUB" FROM "S2", T4 AS "CC"`)
	ambiguous("chained_colliding_label", `WITH "S" AS (SELECT * FROM T4, T4."SARR" AS "X", "X"."SUB" AS "SUB") SELECT "S"."SUB" FROM "S", T4 AS "CC"`)
	ambiguous("colliding_no_parent_join", `WITH "S2" AS (SELECT * FROM T4, T4."SCARR" AS "SUB") SELECT "S2"."SUB" FROM "S2"`)

	// A unique-label read over the same bodies still answers.
	want("colliding_derived_twin_unique", `SELECT "S2"."ID" FROM (SELECT * FROM T4, T4."SCARR" AS "SUB") AS "S2", T4 AS "CC"`, []string{
		"ID=1", "ID=1", "ID=1",
		"ID=1", "ID=1", "ID=1",
		"ID=2", "ID=2", "ID=2",
	})
	want("colliding_unique_read", `WITH "S2" AS (SELECT * FROM T4, T4."SCARR" AS "SUB") SELECT "S2"."ID" FROM "S2", T4 AS "CC"`, []string{
		"ID=1", "ID=1", "ID=1",
		"ID=1", "ID=1", "ID=1",
		"ID=2", "ID=2", "ID=2",
	})

	// Parent WHERE predicates now retain the CTE leg's exact output schema.
	// Pin both a predicate owned solely by the sibling leg and a cross-leg
	// predicate: the latter must address S.ID and CC.ID through different
	// ordinal windows even though their bare leaf names collide.
	want("parent_where_sibling", starCTE+`SELECT "S"."X" FROM "S", T4 AS "CC" WHERE "CC"."ID" = 2`, []string{
		"X=100", "X=200", "X=300",
	})
	want("parent_where_cross_leg", starCTE+`SELECT "S"."X", "CC"."ID" FROM "S", T4 AS "CC" WHERE "S"."ID" = "CC"."ID" AND "S"."X" > 150`, []string{
		"X=200|ID=1", "X=300|ID=2",
	})
}

// TestFDB_ChainedStarBodyCTE pins the CHAINED star body's rows: a
// projection-less multi-link unnest body
// (`WITH S AS (SELECT * FROM t, t.sarr AS x, x.sub AS y) …`) referenced as a
// JOIN LEG gates ordinal through the SAME factored gate its fresh translation
// runs (chainedUnnestOrdinalGate via derivedBodyStarOrdinalLeg), resolving the
// parent positionally rather than by name.
//
// SARR fixture: T4(1)=[{SUB:[1,2]},{SUB:[3]}], T4(2)=[{SUB:[4]}], T4(11) empty.
// Chained rows (ID,Y): (1,1),(1,2),(1,3),(2,4) — × 3 CC rows for the parent join.
func TestFDB_ChainedStarBodyCTE(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatal(err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())

	md := buildChainedUnnestMetadata(t)
	if err := saveStarCTERows(ctx, db, ks, md); err != nil {
		t.Fatal(err)
	}

	run := func(name, q string) []string {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadata(q, md, nil)
		if perr != nil {
			t.Fatalf("%s: plan error: %v\n  sql: %s", name, perr, q)
		}
		var out []string
		_, eerr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if sErr != nil {
				return nil, sErr
			}
			cur, cErr := executor.ExecutePlan(ctx, plan, store, executor.EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
			if cErr != nil {
				return nil, cErr
			}
			defer cur.Close()
			rows, rErr := executor.CollectAll(ctx, cur)
			if rErr != nil {
				return nil, rErr
			}
			for _, r := range rows {
				out = append(out, positionalNamedPipeSprint(r))
			}
			return nil, nil
		})
		if eerr != nil {
			t.Fatalf("%s: exec error: %v\n  sql: %s", name, eerr, q)
		}
		sort.Strings(out)
		return out
	}
	want := func(name, q string, exp []string) {
		t.Helper()
		got := run(name, q)
		sort.Strings(exp)
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", exp) {
			t.Errorf("%s: rows = %v, want %v\n  sql: %s", name, got, exp, q)
		}
	}
	times3 := func(rows ...string) []string {
		var out []string
		for _, r := range rows {
			out = append(out, r, r, r)
		}
		return out
	}

	const chainedCTE = `WITH "S" AS (SELECT * FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y") `

	want("qualified_y", chainedCTE+`SELECT "S"."Y" FROM "S", T4 AS "CC"`,
		times3("Y=1", "Y=2", "Y=3", "Y=4"))

	want("qualified_id", chainedCTE+`SELECT "S"."ID" FROM "S", T4 AS "CC"`,
		times3("ID=1", "ID=1", "ID=1", "ID=2"))

	want("id_and_y", chainedCTE+`SELECT "S"."ID", "S"."Y" FROM "S", T4 AS "CC"`,
		times3("ID=1|Y=1", "ID=1|Y=2", "ID=1|Y=3", "ID=2|Y=4"))

	// A BARE unique-name read over the enclosed chained leg.
	want("bare_y", chainedCTE+`SELECT "Y" FROM "S", T4 AS "CC"`,
		times3("Y=1", "Y=2", "Y=3", "Y=4"))

	// WITH ORDINALITY on the chained link: (Y,O) = (1,1),(2,2) within elem{1,2};
	// (3,1) within elem{3}; (4,1) within elem{4}.
	want("at_chained_link", `WITH "S" AS (SELECT * FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" AT "O") SELECT "S"."Y", "S"."O" FROM "S", T4 AS "CC"`,
		times3("Y=1|O=1", "Y=2|O=2", "Y=3|O=1", "Y=4|O=1"))

	// A body WHERE (plain filter above the chained spine): Y > 2 keeps {3,4}.
	want("where_body", `WITH "S" AS (SELECT * FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE "Y" > 2) SELECT "S"."Y" FROM "S", T4 AS "CC"`,
		times3("Y=3", "Y=4"))

	// The DERIVED-TABLE twin (LogicalCTE directly in leg position) — bare
	// internal labels, same values (the single-link star class's twin rule).
	want("derived_twin", `SELECT "S"."Y" FROM (SELECT * FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y") AS "S", T4 AS "CC"`,
		times3("Y=1", "Y=2", "Y=3", "Y=4"))
}

// TestStarBodyCTEPlanSweep pins both halves of the star-CTE class: unique-label
// shapes plan, while colliding visible labels are rejected as 42702. There is no
// name-keyed fallback for either decision. Planning-only.
func TestStarBodyCTEPlanSweep(t *testing.T) {
	t.Parallel()
	md := buildChainedUnnestMetadata(t)
	count := func(sql string) (int, error) {
		_, err := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
		return 0, err
	}

	zeroed := []struct{ name, sql string }{
		{"enclosed_qualified", `WITH "S" AS (SELECT * FROM T4, T4."SCARR" AS "X") SELECT "S"."ID", "S"."X" FROM "S", T4 AS "CC"`},
		{"at_body", `WITH "S" AS (SELECT * FROM T4, T4."SCARR" AS "X" AT "O") SELECT "S"."X", "S"."O" FROM "S", T4 AS "CC"`},
		{"where_body", `WITH "S" AS (SELECT * FROM T4, T4."SCARR" AS "X" WHERE "X" > 150) SELECT "S"."ID" FROM "S", T4 AS "CC"`},
		{"derived_twin", `SELECT "S"."ID" FROM (SELECT * FROM T4, T4."SCARR" AS "X") AS "S", T4 AS "CC"`},
		{"struct_array_body", `WITH "S" AS (SELECT * FROM T4, T4."SARR" AS "X") SELECT "S"."ID" FROM "S", T4 AS "CC"`},
		// The CHAINED star body (multi-link spine) — admitted through the
		// factored chainedUnnestOrdinalGate (the chained body itself ordinalizes
		// fresh, and the parent reads it positionally too).
		{"chained_star", `WITH "S" AS (SELECT * FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y") SELECT "S"."Y" FROM "S", T4 AS "CC"`},
		{"chained_star_at", `WITH "S" AS (SELECT * FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" AT "O") SELECT "S"."Y", "S"."O" FROM "S", T4 AS "CC"`},
		{"chained_star_where", `WITH "S" AS (SELECT * FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE "Y" > 2) SELECT "S"."Y" FROM "S", T4 AS "CC"`},
		{"chained_star_derived_twin", `SELECT "S"."Y" FROM (SELECT * FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y") AS "S", T4 AS "CC"`},
		// The COLLIDING-label bodies — every visible duplicate is retained, so
		// a reference is ambiguous rather than shadow-preferred.
		{"colliding_label", `WITH "S2" AS (SELECT * FROM T4, T4."SCARR" AS "SUB") SELECT "S2"."SUB" FROM "S2", T4 AS "CC"`},
		{"chained_colliding_label", `WITH "S" AS (SELECT * FROM T4, T4."SARR" AS "X", "X"."SUB" AS "SUB") SELECT "S"."SUB" FROM "S", T4 AS "CC"`},
		{"colliding_at_alias", `WITH "S2" AS (SELECT * FROM T4, T4."SCARR" AS "E" AT "SUB") SELECT "S2"."SUB" FROM "S2", T4 AS "CC"`},
		{"colliding_derived_twin", `SELECT "S2"."SUB" FROM (SELECT * FROM T4, T4."SCARR" AS "SUB") AS "S2", T4 AS "CC"`},
	}
	ambiguousCases := map[string]bool{
		"colliding_label": true, "chained_colliding_label": true,
		"colliding_at_alias": true, "colliding_derived_twin": true,
	}
	for _, tc := range zeroed {
		_, err := count(tc.sql)
		if ambiguousCases[tc.name] {
			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeAmbiguousColumn {
				t.Errorf("%s: error = %v, want 42702\n  sql: %s", tc.name, err, tc.sql)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: plan error (should ordinalize cleanly): %v\n  sql: %s", tc.name, err, tc.sql)
		}
	}
}

// saveStarCTERows writes the shared T4 fixture rows for the star-CTE tests.
// SCARR carries the scalar-unnest values; SARR carries struct elements
// ({SUB:[…]}) for the CHAINED star shapes (T4(1)=[{1,2},{3}], T4(2)=[{4}],
// T4(11) empty — it vanishes from every unnest).
func saveStarCTERows(ctx context.Context, db *recordlayer.FDBDatabase, ks subspace.Subspace, md *recordlayer.RecordMetaData) error {
	t4Desc := md.GetRecordType("T4").Descriptor
	scarrFD := t4Desc.Fields().ByName("SCARR")
	sarrFD := t4Desc.Fields().ByName("SARR")
	elemDesc := arrayElementMessageDescriptor(sarrFD)
	mkElem := func(sub ...int32) protoreflect.Value {
		m := dynamicpb.NewMessage(elemDesc)
		vals := make([]protoreflect.Value, 0, len(sub))
		for _, s := range sub {
			vals = append(vals, protoreflect.ValueOfInt32(s))
		}
		setArrayField(m, elemDesc.Fields().ByName("SUB"), vals...)
		return protoreflect.ValueOfMessage(m)
	}
	mkT4 := func(id, sub int64, scarr []int32, sarr ...protoreflect.Value) proto.Message {
		m := dynamicpb.NewMessage(t4Desc)
		m.Set(t4Desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(t4Desc.Fields().ByName("SUB"), protoreflect.ValueOfInt64(sub))
		vals := make([]protoreflect.Value, 0, len(scarr))
		for _, s := range scarr {
			vals = append(vals, protoreflect.ValueOfInt32(s))
		}
		setArrayField(m, scarrFD, vals...)
		setArrayField(m, sarrFD, sarr...)
		return m
	}
	_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if sErr != nil {
			return nil, sErr
		}
		for _, r := range []proto.Message{
			mkT4(1, 999, []int32{100, 200}, mkElem(1, 2), mkElem(3)),
			mkT4(2, 20, []int32{300}, mkElem(4)),
			mkT4(11, 999, nil),
		} {
			if _, e := store.SaveRecord(r); e != nil {
				return nil, e
			}
		}
		return nil, nil
	})
	return err
}
