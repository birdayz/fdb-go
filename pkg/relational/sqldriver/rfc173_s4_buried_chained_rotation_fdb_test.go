package sqldriver_test

import (
	"context"
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
	"fdb.dev/pkg/relational/core/embedded"
	rquery "fdb.dev/pkg/relational/core/query"
)

// The BURIED-CHAINED-SPINE rotation (RFC-173 S4, item B): a chained lateral
// unnest followed by TRAILING plain legs (`FROM t, t.arr AS x, x.sub AS y, z`)
// rotates to the box-bottom chained form (`FROM t, z, t.arr AS x, x.sub AS y`)
// so the WHOLE cluster ordinalizes via the chained seed — before the rotation
// the spine was a buried LEG of the trailing join, which name-modeled the
// parent (the last un-enclosed P4 producer caller for a WORKING plan).
//
// Two tests share the data:
//   - TestFDB_RFC173S4_BuriedChainedRotation pins the ROWS (hand-computed from
//     the data below — the name-model ground truth; the buried-straddle rows
//     were additionally verified against the pre-rotation name-model plan,
//     which answers identically).
//   - TestRFC173BuriedChainedRotationCensus (serial, planning-only) pins the
//     PRODUCER RETIREMENT: every rotated shape fires 0 P4/P5. RED-on-revert:
//     without the rotation each fires 1 P4 (the trailing join's name-model
//     anchored seed).
//
// Data (SARR elements as {SUB…; SUBSTRUCT DEEP…}; top-level SUB is the shadow
// scalar):
//
//	T4(1)  sub=999 SARR=[{SUB[1,7]; SUBSTRUCT [DEEP[11,12]],[DEEP[13]]}]
//	T4(2)  sub=20  SARR=[{SUB[9];   SUBSTRUCT [DEEP[20]]}]
//	T4(11) sub=999 SARR=[{SUB[5];   SUBSTRUCT [DEEP[11]]}]
//
// The trailing leg T4C scans the same table (IDs 1, 2, 11) — deliberately
// SAME-TYPED, so a mis-window between the two identically-shaped legs (T4 vs
// T4C) is observable (the leg-projection subtest reads both IDs).
func TestFDB_RFC173S4_BuriedChainedRotation(t *testing.T) {
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
	if err := saveBuriedChainedRotationRows(ctx, db, ks, md); err != nil {
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
				out = append(out, fmt.Sprintf("%v", executor.RowValue(r)))
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

	// Y values per T4 row: T4(1) {1,7}; T4(2) {9}; T4(11) {5}. T4C has 3 rows.
	buried := `FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y", T4 AS "T4C"`

	// UNFILTERED: every (T4, Y) pair × 3 T4C rows = 12.
	want("unfiltered", `SELECT "Y" `+buried, []string{
		"map[Y:1]", "map[Y:1]", "map[Y:1]", "map[Y:7]", "map[Y:7]", "map[Y:7]",
		"map[Y:9]", "map[Y:9]", "map[Y:9]", "map[Y:5]", "map[Y:5]", "map[Y:5]",
	})

	// OUTER⋈ELEMENT straddle: T4.ID = Y matches only T4(1)'s Y=1, × 3 T4C rows.
	want("straddle", `SELECT "Y" `+buried+` WHERE T4."ID" = "Y"`,
		[]string{"map[Y:1]", "map[Y:1]", "map[Y:1]"})

	// TRAILING-leg-only conjunct: T4C.ID = 2 keeps one T4C row → each (T4, Y) once.
	want("trailing_only", `SELECT "Y" `+buried+` WHERE "T4C"."ID" = 2`,
		[]string{"map[Y:1]", "map[Y:7]", "map[Y:9]", "map[Y:5]"})

	// MIXED: straddle AND trailing conjunct.
	want("mixed", `SELECT "Y" `+buried+` WHERE T4."ID" = "Y" AND "T4C"."ID" = 2`,
		[]string{"map[Y:1]"})

	// CROSS-LEG straddle: T4C.ID = Y — only Y=1 has a matching T4C row (ID 1).
	want("crossleg_straddle", `SELECT "Y" `+buried+` WHERE "T4C"."ID" = "Y"`,
		[]string{"map[Y:1]"})

	// ELEMENT-only filter: Y > 4 → {7,9,5} × 3.
	want("element_filter", `SELECT "Y" `+buried+` WHERE "Y" > 4`, []string{
		"map[Y:7]", "map[Y:7]", "map[Y:7]",
		"map[Y:9]", "map[Y:9]", "map[Y:9]",
		"map[Y:5]", "map[Y:5]", "map[Y:5]",
	})

	// LEG PROJECTION over the two IDENTICALLY-TYPED legs — the dup-window
	// discriminator: a mis-window between T4 and T4C would swap the IDs.
	want("dup_leg_projection", `SELECT T4."ID", "T4C"."ID", "Y" `+buried+` WHERE "Y" = 7`, []string{
		"map[T4.ID:1 T4C.ID:1 Y:7]", "map[T4.ID:1 T4C.ID:2 Y:7]", "map[T4.ID:1 T4C.ID:11 Y:7]",
	})

	// THREE-link buried chain.
	want("threelink_buried",
		`SELECT "Z" FROM T4, T4."SARR" AS "X", "X"."SUBSTRUCT" AS "Y2", "Y2"."DEEP" AS "Z", T4 AS "T4C" WHERE "T4C"."ID" = 2`,
		[]string{"map[Z:11]", "map[Z:11]", "map[Z:12]", "map[Z:13]", "map[Z:20]"})

	// FORK buried chain: W's owner is X (two links back). Per T4: T4(1) has 2
	// SUBSTRUCT elements × SUB {1,7}; T4(2) 1 × {9}; T4(11) 1 × {5}.
	want("fork_buried",
		`SELECT "W" FROM T4, T4."SARR" AS "X", "X"."SUBSTRUCT" AS "Y2", "X"."SUB" AS "W", T4 AS "T4C" WHERE "T4C"."ID" = 2`,
		[]string{"map[W:1]", "map[W:1]", "map[W:7]", "map[W:7]", "map[W:9]", "map[W:5]"})

	// AT ordinality on the first link, buried: every row has ONE SARR element.
	want("at_buried",
		`SELECT "P", "Y" FROM T4, T4."SARR" AS "X" AT "P", "X"."SUB" AS "Y", T4 AS "T4C" WHERE "T4C"."ID" = 2`,
		[]string{"map[P:1 Y:1]", "map[P:1 Y:7]", "map[P:1 Y:9]", "map[P:1 Y:5]"})

	// LEFT-box spine bottom + trailing leg: A(1)→B(2); A(2)/A(11) null-supplied.
	want("leftbox_bottom_trailing",
		`SELECT "A"."ID", "B"."ID", "Y" FROM T4 AS "A" LEFT JOIN T4 AS "B" ON "A"."ID" + 1 = "B"."ID", "A"."SARR" AS "X", "X"."SUB" AS "Y", T4 AS "T4C" WHERE "T4C"."ID" = 2`,
		[]string{
			"map[A.ID:1 B.ID:2 Y:1]", "map[A.ID:1 B.ID:2 Y:7]",
			"map[A.ID:2 B.ID:<nil> Y:9]", "map[A.ID:11 B.ID:<nil> Y:5]",
		})

	// SELECT * over the buried chain: STRANDED before the rotation ("best
	// expression is not a physical plan"); now plans and executes. Column order
	// follows the ROTATED form (trailing legs before the elements — the same
	// element-last convention the single-link rotation established), pinned via
	// the label list; the row COUNT pins execution.
	t.Run("star_now_plans", func(t *testing.T) {
		q := `SELECT * ` + buried
		plan, perr := embedded.PlanRecordQueryWithMetadata(q, md, nil)
		if perr != nil {
			t.Fatalf("SELECT * over the buried chain must plan post-rotation: %v", perr)
		}
		labels := fmt.Sprintf("%v", embedded.ResultColumnLabelsForPlan(plan, md))
		if labels != "[ID SARR SCARR SUB ID SARR SCARR SUB X Y]" {
			t.Fatalf("SELECT * labels = %s (rotated element-last layout expected)", labels)
		}
		if got := run("star_rows", q); len(got) != 12 {
			t.Fatalf("SELECT * rows = %d, want 12", len(got))
		}
	})
}

// TestRFC173BuriedChainedRotationCensus pins the PRODUCER RETIREMENT of the
// buried-chained class: each shape plans AND fires 0 P4/P5 name-model
// producers. RED-ON-REVERT (of rotateBuriedChainedSpine + its dispatch): the
// shapes still plan — the trailing join name-models around the buried spine —
// but each fires 1 P4, tripping the count. Serial (process-global observer).
func TestRFC173BuriedChainedRotationCensus(t *testing.T) { //nolint:paralleltest // process-global observer, must be serial
	md := buildChainedUnnestMetadata(t)
	buried := `FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y", T4 AS "T4C"`
	queries := []struct{ name, sql string }{
		{"unfiltered", `SELECT "Y" ` + buried},
		{"straddle", `SELECT "Y" ` + buried + ` WHERE T4."ID" = "Y"`},
		{"trailing_only", `SELECT "Y" ` + buried + ` WHERE "T4C"."ID" = 2`},
		{"mixed", `SELECT "Y" ` + buried + ` WHERE T4."ID" = "Y" AND "T4C"."ID" = 2`},
		{"crossleg_straddle", `SELECT "Y" ` + buried + ` WHERE "T4C"."ID" = "Y"`},
		{"element_filter", `SELECT "Y" ` + buried + ` WHERE "Y" > 4`},
		{"threelink_buried", `SELECT "Z" FROM T4, T4."SARR" AS "X", "X"."SUBSTRUCT" AS "Y2", "Y2"."DEEP" AS "Z", T4 AS "T4C"`},
		{"fork_buried", `SELECT "W" FROM T4, T4."SARR" AS "X", "X"."SUBSTRUCT" AS "Y2", "X"."SUB" AS "W", T4 AS "T4C"`},
		{"at_buried", `SELECT "P", "Y" FROM T4, T4."SARR" AS "X" AT "P", "X"."SUB" AS "Y", T4 AS "T4C"`},
		{"leftbox_bottom_trailing", `SELECT "A"."ID", "B"."ID", "Y" FROM T4 AS "A" LEFT JOIN T4 AS "B" ON "A"."ID" + 1 = "B"."ID", "A"."SARR" AS "X", "X"."SUB" AS "Y", T4 AS "T4C"`},
		{"two_trailing_legs", `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y", T4 AS "T4C", T4 AS "T4D"`},
	}
	for _, tc := range queries {
		n := 0
		rquery.SetProducerCensusObserver(func(rquery.ProducerCensusRecord) { n++ })
		_, err := embedded.PlanRecordQueryWithMetadata(tc.sql, md, nil)
		rquery.SetProducerCensusObserver(nil)
		if err != nil {
			t.Errorf("%s: plan error (should rotate + ordinalize cleanly): %v\n  sql: %s", tc.name, err, tc.sql)
			continue
		}
		if n != 0 {
			t.Errorf("%s: fired %d P4/P5 producer(s), want 0 (the buried-chained rotation regressed "+
				"to the name model)\n  sql: %s", tc.name, n, tc.sql)
		}
	}
}

// saveBuriedChainedRotationRows writes the shared T4 fixture rows.
func saveBuriedChainedRotationRows(ctx context.Context, db *recordlayer.FDBDatabase, ks subspace.Subspace, md *recordlayer.RecordMetaData) error {
	t4Desc := md.GetRecordType("T4").Descriptor
	sarrFD := t4Desc.Fields().ByName("SARR")
	elemDesc := sarrFD.Message()
	substructFD := elemDesc.Fields().ByName("SUBSTRUCT")
	elem2Desc := substructFD.Message()

	mkElem2 := func(deep ...int32) protoreflect.Value {
		m := dynamicpb.NewMessage(elem2Desc)
		m.Set(elem2Desc.Fields().ByName("LEAF"), protoreflect.ValueOfInt64(0))
		dl := m.NewField(elem2Desc.Fields().ByName("DEEP")).List()
		for _, d := range deep {
			dl.Append(protoreflect.ValueOfInt32(d))
		}
		m.Set(elem2Desc.Fields().ByName("DEEP"), protoreflect.ValueOfList(dl))
		return protoreflect.ValueOfMessage(m)
	}
	mkElem := func(sub []int32, substruct ...protoreflect.Value) protoreflect.Value {
		m := dynamicpb.NewMessage(elemDesc)
		m.Set(elemDesc.Fields().ByName("K"), protoreflect.ValueOfInt64(0))
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
	mkT4 := func(id, sub int64, sarr ...protoreflect.Value) proto.Message {
		m := dynamicpb.NewMessage(t4Desc)
		m.Set(t4Desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(t4Desc.Fields().ByName("SUB"), protoreflect.ValueOfInt64(sub))
		sl := m.NewField(sarrFD).List()
		for _, e := range sarr {
			sl.Append(e)
		}
		m.Set(sarrFD, protoreflect.ValueOfList(sl))
		return m
	}

	_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if sErr != nil {
			return nil, sErr
		}
		for _, r := range []proto.Message{
			mkT4(1, 999, mkElem([]int32{1, 7}, mkElem2(11, 12), mkElem2(13))),
			mkT4(2, 20, mkElem([]int32{9}, mkElem2(20))),
			mkT4(11, 999, mkElem([]int32{5}, mkElem2(11))),
		} {
			if _, e := store.SaveRecord(r); e != nil {
				return nil, e
			}
		}
		return nil, nil
	})
	return err
}
