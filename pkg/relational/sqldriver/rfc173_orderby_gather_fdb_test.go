package sqldriver_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// TestFDB_RFC173S4_OrderByGather is the discriminating-ORDER regression cert for the
// ORDER-BY-over-gathered-seed fix — the test that SHOULD have caught the pre-existing
// silent-wrong-order bug the fake-green `order_by_bare_twin_resolves` (asserting the row
// SET on single-value K, never the ORDER) let ship. A sort key that names a LEG COLUMN
// of a gathered multi-source lateral unnest must POSITIONALLY BAKE to its flat slot (the
// SAME shared authority the GROUP BY path uses: translateSort → gatheredSeedBakeContext
// → bakeGatheredGroupValue). Pre-fix the leg-column sort key evaluated to a dead constant
// over the merged positional row, so InMemorySort sorted on nothing and DESC == ASC.
//
// EVERY case pins the EXACT executor-ordered rows (not a set) with DISTINCT sort values
// (100/300/NULL) so DESC ≠ ASC is load-bearing: a mis-bind that leaves the output
// element-ordered fails both directions. Coverage: dup / non-dup / 3-leg (was a LOUD
// `ordinal -1`) / element-key / box class-1 FULL-NULL (buried + NULL-padded), ASC and
// DESC, plus a single-source control proving the gather's NULL sort position is IDENTICAL
// to the non-gather path (nulls smallest: ASC first, DESC last).
//
//	P(PID,K,ARR): (1,100,[7,8]) (2,300,[9]) (3,NULL,[11])  -- bare-twin owner, distinct K
//	Q(QID,K):     (1,200)                                   -- dup partner (K)
//	R(RID,RK):    (1,400)                                   -- non-dup partner
//	S(SID,SK):    (1,100) (2,300)                           -- box leg, SK sort col (buried)
//	XT(XID,XARR): (1,[7,8]) (2,[9]) (5,[55])                -- box leg, owns array; join S.SID=XT.XID
//	Y(YID,YK):    (1,500)                                   -- plain leg
func TestFDB_RFC173S4_OrderByGather(t *testing.T) {
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

	b := metadata.NewSchemaTemplateBuilder().SetName("obgather")
	arr := func() api.DataType { return api.NewArrayType(api.NewIntegerType(false), true) }
	b.AddTable("P", []metadata.ColumnSpec{
		metadata.NewColumnSpec("PID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
		metadata.NewColumnSpec("ARR", arr(), 3),
	}, []string{"PID"})
	b.AddTable("Q", []metadata.ColumnSpec{
		metadata.NewColumnSpec("QID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
	}, []string{"QID"})
	b.AddTable("R", []metadata.ColumnSpec{
		metadata.NewColumnSpec("RID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("RK", api.NewLongType(true), 2),
	}, []string{"RID"})
	b.AddTable("S", []metadata.ColumnSpec{
		metadata.NewColumnSpec("SID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("SK", api.NewLongType(true), 2),
	}, []string{"SID"})
	b.AddTable("XT", []metadata.ColumnSpec{
		metadata.NewColumnSpec("XID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("XARR", arr(), 2),
	}, []string{"XID"})
	b.AddTable("Y", []metadata.ColumnSpec{
		metadata.NewColumnSpec("YID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("YK", api.NewLongType(true), 2),
	}, []string{"YID"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	md := tmpl.Underlying()

	longOrNull := func(m *dynamicpb.Message, d protoreflect.MessageDescriptor, name string, v int64, null bool) {
		if !null {
			m.Set(d.Fields().ByName(protoreflect.Name(name)), protoreflect.ValueOfInt64(v))
		}
	}
	mkArr := func(table, idField, arrField string, id int64, vals ...int32) proto.Message {
		d := md.GetRecordType(table).Descriptor
		m := dynamicpb.NewMessage(d)
		m.Set(d.Fields().ByName(protoreflect.Name(idField)), protoreflect.ValueOfInt64(id))
		fd := d.Fields().ByName(protoreflect.Name(arrField))
		l := m.NewField(fd).List()
		for _, v := range vals {
			l.Append(protoreflect.ValueOfInt32(v))
		}
		m.Set(fd, protoreflect.ValueOfList(l))
		return m
	}
	mkP := func(pid, k int64, kNull bool, vals ...int32) proto.Message {
		d := md.GetRecordType("P").Descriptor
		m := dynamicpb.NewMessage(d)
		m.Set(d.Fields().ByName("PID"), protoreflect.ValueOfInt64(pid))
		longOrNull(m, d, "K", k, kNull)
		fd := d.Fields().ByName("ARR")
		l := m.NewField(fd).List()
		for _, v := range vals {
			l.Append(protoreflect.ValueOfInt32(v))
		}
		m.Set(fd, protoreflect.ValueOfList(l))
		return m
	}
	mk2 := func(table, f1, f2 string, v1, v2 int64) proto.Message {
		d := md.GetRecordType(table).Descriptor
		m := dynamicpb.NewMessage(d)
		m.Set(d.Fields().ByName(protoreflect.Name(f1)), protoreflect.ValueOfInt64(v1))
		m.Set(d.Fields().ByName(protoreflect.Name(f2)), protoreflect.ValueOfInt64(v2))
		return m
	}

	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Create()
		if sErr != nil {
			return nil, sErr
		}
		recs := []proto.Message{
			mkP(1, 100, false, 7, 8), mkP(2, 300, false, 9), mkP(3, 0, true, 11),
			mk2("Q", "QID", "K", 1, 200),
			mk2("R", "RID", "RK", 1, 400),
			mk2("S", "SID", "SK", 1, 100), mk2("S", "SID", "SK", 2, 300),
			mkArr("XT", "XID", "XARR", 1, 7, 8), mkArr("XT", "XID", "XARR", 2, 9), mkArr("XT", "XID", "XARR", 5, 55),
			mk2("Y", "YID", "YK", 1, 500),
		}
		for _, r := range recs {
			if _, e := store.SaveRecord(r); e != nil {
				return nil, e
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	runOrdered := func(t *testing.T, q string) (string, []string) {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadata(q, md, nil)
		if perr != nil {
			t.Fatalf("plan %q: %v", q, perr)
		}
		explain := plan.Explain()
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
				out = append(out, fmt.Sprintf("%v", r.Datum))
			}
			return nil, nil
		})
		if eerr != nil {
			t.Fatalf("exec %q: %v", q, eerr)
		}
		return explain, out
	}
	// wantOrdered asserts the EXACT executor-ordered rows. bakedKey (e.g. ".K#" / ".SK#")
	// pins the fix MECHANISM: the leg-column sort key rendered as a baked ORDINAL over the
	// seed. On revert the sort key is the unbaked bare name (no "#"), so the ordinal marker
	// is absent AND the rows regress — either fails the test.
	wantOrdered := func(t *testing.T, q string, want []string, bakedKey string) {
		t.Helper()
		explain, got := runOrdered(t, q)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("ordered rows = %v, want %v\n  sql: %s\n  plan: %s", got, want, q, explain)
		}
		if bakedKey != "" && !strings.Contains(explain, bakedKey) {
			t.Fatalf("sort key not baked (missing %q — a revert to the unbaked name)\n  sql: %s\n  plan: %s", bakedKey, q, explain)
		}
	}

	// The discriminating ordered results: DESC and ASC differ, so a dead-key mis-bind
	// (which leaves the element-ordered [7,8,9,11] output for BOTH directions) fails.
	descK := []string{"map[EL:9 P.K:300]", "map[EL:7 P.K:100]", "map[EL:8 P.K:100]", "map[EL:11 P.K:<nil>]"}
	ascK := []string{"map[EL:11 P.K:<nil>]", "map[EL:7 P.K:100]", "map[EL:8 P.K:100]", "map[EL:9 P.K:300]"}

	// 1. dup (2-leg bare-twin): P,Q share K. DESC reorders to 300 first; NULL last.
	t.Run("dup_desc", func(t *testing.T) {
		wantOrdered(t, `SELECT P."K", "EL" FROM P, Q, P."ARR" AS "EL" ORDER BY P."K" DESC, "EL"`, descK, ".K#")
	})
	t.Run("dup_asc", func(t *testing.T) {
		wantOrdered(t, `SELECT P."K", "EL" FROM P, Q, P."ARR" AS "EL" ORDER BY P."K" ASC, "EL"`, ascK, ".K#")
	})
	// 1b. COMPOUND sort key `P.K + Q.K` (the sort-site sibling of the condition-1
	// SUM(A.K+B.K) recursion pin): the key is an arithmetic over TWO leg columns, so the
	// bake must RECURSE (values.Replace) and bake each FieldValue LEAF to its own slot. A
	// non-recursing bake leaves the arithmetic unmatched → dead key → element order; and
	// if EITHER leaf fails to bake it reads NULL → NULL sum for every row → also element
	// order. P.K+Q.K = P.K+200 (Q.K=200): {300,500,NULL}, monotonic in P.K so the display
	// order matches the P.K sort while proving BOTH leaves baked.
	t.Run("compound_two_leg_desc", func(t *testing.T) {
		wantOrdered(t, `SELECT P."K", "EL" FROM P, Q, P."ARR" AS "EL" ORDER BY P."K" + Q."K" DESC, "EL"`, descK, ".K#")
	})
	t.Run("compound_two_leg_asc", func(t *testing.T) {
		wantOrdered(t, `SELECT P."K", "EL" FROM P, Q, P."ARR" AS "EL" ORDER BY P."K" + Q."K" ASC, "EL"`, ascK, ".K#")
	})
	// 1c. MULTI-KEY with a LEG-COLUMN secondary (each key bakes to its OWN slot, not just
	// the first): Q.K (constant 200) primary ties every row, so the
	// SECONDARY leg column P.K DESC determines the order (EL breaks the P.K=100 tie). If
	// the secondary key didn't bake, the order would collapse to the EL tertiary (element
	// order); it doesn't — P.K DESC drives it.
	t.Run("multikey_leg_secondary", func(t *testing.T) {
		wantOrdered(t, `SELECT P."K", "EL" FROM P, Q, P."ARR" AS "EL" ORDER BY Q."K" ASC, P."K" DESC, "EL" ASC`, descK, ".K#")
	})
	// 2. non-dup (2-leg): P,R — the gather-general axis (bug is NOT dup-specific).
	t.Run("nondup_desc", func(t *testing.T) {
		wantOrdered(t, `SELECT P."K", "EL" FROM P, R, P."ARR" AS "EL" ORDER BY P."K" DESC, "EL"`, descK, ".K#")
	})
	t.Run("nondup_asc", func(t *testing.T) {
		wantOrdered(t, `SELECT P."K", "EL" FROM P, R, P."ARR" AS "EL" ORDER BY P."K" ASC, "EL"`, ascK, ".K#")
	})
	// 3. 3-leg (P,Q,R + P.ARR): pre-fix this ERRORED loud (`field "P.K" not resolvable,
	// ordinal -1`) — the same bug, different resolution arm. The one bake fixes both.
	t.Run("threeleg_desc", func(t *testing.T) {
		wantOrdered(t, `SELECT P."K", "EL" FROM P, Q, R, P."ARR" AS "EL" ORDER BY P."K" DESC, "EL"`, descK, ".K#")
	})
	t.Run("threeleg_asc", func(t *testing.T) {
		wantOrdered(t, `SELECT P."K", "EL" FROM P, Q, R, P."ARR" AS "EL" ORDER BY P."K" ASC, "EL"`, ascK, ".K#")
	})
	// 4. element-key (ORDER BY the array element): already sorted pre-fix; now baked
	// positionally via the SAME authority (element-first) — must stay correct.
	t.Run("element_desc", func(t *testing.T) {
		wantOrdered(t, `SELECT P."K", "EL" FROM P, Q, P."ARR" AS "EL" ORDER BY "EL" DESC`,
			[]string{"map[EL:11 P.K:<nil>]", "map[EL:9 P.K:300]", "map[EL:8 P.K:100]", "map[EL:7 P.K:100]"}, ".EL#")
	})
	// 5. box class-1 FULL-NULL ((S FULL XT), Y, XT.XARR): S.SK is BURIED in the box and
	// NULL-padded on the X-only row. The buried box column sorts, and the padded row's
	// S.SK=NULL sorts in the SAME position as a real NULL (DESC last, ASC first).
	t.Run("box_fullnull_desc", func(t *testing.T) {
		wantOrdered(t, `SELECT S."SK", "EL" FROM S FULL OUTER JOIN XT ON S."SID" = XT."XID", Y, XT."XARR" AS "EL" ORDER BY S."SK" DESC, "EL"`,
			[]string{"map[EL:9 S.SK:300]", "map[EL:7 S.SK:100]", "map[EL:8 S.SK:100]", "map[EL:55 S.SK:<nil>]"}, ".SK#")
	})
	t.Run("box_fullnull_asc", func(t *testing.T) {
		wantOrdered(t, `SELECT S."SK", "EL" FROM S FULL OUTER JOIN XT ON S."SID" = XT."XID", Y, XT."XARR" AS "EL" ORDER BY S."SK" ASC, "EL"`,
			[]string{"map[EL:55 S.SK:<nil>]", "map[EL:7 S.SK:100]", "map[EL:8 S.SK:100]", "map[EL:9 S.SK:300]"}, ".SK#")
	})
	// 6. CONTROL — single-source (no gather, no bake): the NULL-ordering REFERENCE. The
	// gather cases above sort NULL to the IDENTICAL position (nulls smallest), proving the
	// bake matches the non-gather nulls ordering. No bakedKey — the single-source sort key
	// is not baked (its W4c binary path resolves the leg column directly).
	t.Run("control_singlesource_desc", func(t *testing.T) {
		wantOrdered(t, `SELECT P."K", "EL" FROM P, P."ARR" AS "EL" ORDER BY P."K" DESC, "EL"`, descK, "")
	})
	t.Run("control_singlesource_asc", func(t *testing.T) {
		wantOrdered(t, `SELECT P."K", "EL" FROM P, P."ARR" AS "EL" ORDER BY P."K" ASC, "EL"`, ascK, "")
	})
}
