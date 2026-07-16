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
)

// TestFDB_NullSupplyBarrier pins the null-supplying pushdown BARRIER: a WHERE
// on a null-supplying leg's column keeps post-join (null-excluding) semantics
// — `FROM A LEFT JOIN B ON … WHERE B.col = x` excludes the padded rows (NULL =
// x is never true). (OBSERVED at introduction, not asserted here: the plans
// keep the conjunct as a PredicatesFilter ABOVE the LEFT
// FlatMap+DefaultOnEmpty / the FULL NLJ — this test pins ROWS only.) A
// pushdown that moved the conjunct INTO the null-supplying leg pre-join would
// re-null-extend the filtered leg and the padded rows would REAPPEAR. This is
// a semantic floor that any future change to predicate placement — however
// the row is represented internally — must keep holding.
func TestFDB_NullSupplyBarrier(t *testing.T) {
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
	t4Desc := md.GetRecordType("T4").Descriptor
	sarrFD := t4Desc.Fields().ByName("SARR")
	mkT4 := func(id, sub int64) proto.Message {
		m := dynamicpb.NewMessage(t4Desc)
		m.Set(t4Desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(t4Desc.Fields().ByName("SUB"), protoreflect.ValueOfInt64(sub))
		m.Set(sarrFD, protoreflect.ValueOfList(m.NewField(sarrFD).List()))
		return m
	}
	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if sErr != nil {
			return nil, sErr
		}
		// Store holds IDs 1,2,3,11 — ALL are A-side rows for the self-join
		// (11 doubles as the B-side match). ON A.ID+10=B.ID → only A=1 matches
		// (B=11); A=2,3,11 are B-NULL rows. WHERE B.SUB = 50: B(11).SUB=50 →
		// post-join answer = ONLY the matched pair. A buggy pre-join push over
		// LEFT would filter B first and still null-pad A=2,3,11 → 4 rows.
		for _, r := range []proto.Message{mkT4(1, 5), mkT4(2, 5), mkT4(3, 5), mkT4(11, 50)} {
			if _, e := store.SaveRecord(r); e != nil {
				return nil, e
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	run := func(name, q string, exp []string) {
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
		sort.Strings(exp)
		if fmt.Sprintf("%v", out) != fmt.Sprintf("%v", exp) {
			t.Fatalf("%s: rows = %v, want %v (padded rows reappearing = the barrier broke)\n  sql: %s", name, out, exp, q)
		}
	}

	// LEFT: exactly ONE row (A=1,B=11). Null-padded A=2,3,11 excluded by B.SUB=50.
	run("left_where_nullsupplying",
		`SELECT "A"."ID", "B"."ID" FROM T4 AS "A" LEFT JOIN T4 AS "B" ON "A"."ID" + 10 = "B"."ID" WHERE "B"."SUB" = 50`,
		[]string{"map[A.ID:1 B.ID:11]"})
	// FULL: same expectation (B-null and A-null rows all excluded).
	run("full_where_nullsupplying",
		`SELECT "A"."ID", "B"."ID" FROM T4 AS "A" FULL OUTER JOIN T4 AS "B" ON "A"."ID" + 10 = "B"."ID" WHERE "B"."SUB" = 50`,
		[]string{"map[A.ID:1 B.ID:11]"})
	// Control: unfiltered LEFT — A={1,2,3,11}: A=1→B=11; A=2,3,11 null-padded.
	run("left_unfiltered",
		`SELECT "A"."ID", "B"."ID" FROM T4 AS "A" LEFT JOIN T4 AS "B" ON "A"."ID" + 10 = "B"."ID"`,
		[]string{"map[A.ID:1 B.ID:11]", "map[A.ID:11 B.ID:<nil>]", "map[A.ID:2 B.ID:<nil>]", "map[A.ID:3 B.ID:<nil>]"})
	// IS NULL variant — the sharpest discriminator: ONLY the null-padded rows.
	run("left_where_is_null",
		`SELECT "A"."ID" FROM T4 AS "A" LEFT JOIN T4 AS "B" ON "A"."ID" + 10 = "B"."ID" WHERE "B"."ID" IS NULL`,
		[]string{"map[ID:11]", "map[ID:2]", "map[ID:3]"})
}
