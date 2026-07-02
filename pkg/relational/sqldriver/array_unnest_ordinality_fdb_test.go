package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/metadata"
)

// TestFDB_ArrayUnnestOrdinality is the RFC-142 (R5) end-to-end proof: correlated
// array UNNEST in the FROM list (`FROM t, t.arr AS x`) and its 4.12 ordinality
// companion (`AT ord`, 1-based INT), ported from Java's array-join-at.yamsql.
//
// SQL INSERT does not support array literals in this engine, so rows with array
// columns are written via the record-store API (dynamicpb repeated fields) and
// the unnest SQL is planned + executed through the full Cascades path. Each
// query asserts the plan shape (FlatMap over Explode, WITH ORDINALITY where
// applicable) AND the exact rows.
func TestFDB_ArrayUnnestOrdinality(t *testing.T) {
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

	// T1: int arrays (nullable + NOT NULL). T3: struct array.
	b := metadata.NewSchemaTemplateBuilder().SetName("ajt")
	b.AddTable("T1", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("ARR1", api.NewArrayType(api.NewIntegerType(false), true), 2),
		metadata.NewColumnSpec("ARR1_NN", api.NewArrayType(api.NewIntegerType(false), false), 3),
		metadata.NewColumnSpec("STRARR", api.NewArrayType(api.NewStringType(false), true), 4),
	}, []string{"ID"})
	// TCOLL has a REAL column named VAL — the unnest AS alias `VAL` must shadow
	// it (the review name-collision case).
	b.AddTable("TCOLL", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("ARR", api.NewArrayType(api.NewIntegerType(false), true), 2),
		metadata.NewColumnSpec("VAL", api.NewIntegerType(true), 3),
	}, []string{"ID"})
	// PA and PB BOTH carry an array column of the SAME name (ARR) with DIFFERENT
	// contents — the P2b non-rightmost-unnest case. `FROM PA, PB, PA.ARR AS X`
	// must explode PA.ARR (the classified source), NOT PB.ARR which the merged
	// row's BARE `arr` resolves to (last-leg-wins). RFC-142.
	b.AddTable("PA", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("ARR", api.NewArrayType(api.NewIntegerType(false), true), 2),
	}, []string{"ID"})
	b.AddTable("PB", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("ARR", api.NewArrayType(api.NewIntegerType(false), true), 2),
	}, []string{"ID"})
	// U has a SCALAR column V that collides with the unnest element binding `v`
	// in `FROM t, t.arr AS v, u` (the review P2 later-same-named-column case). The
	// later FROM item u must NOT overwrite the unnest's element binding.
	b.AddTable("U", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("V", api.NewIntegerType(true), 2),
	}, []string{"ID"})
	// D is a REAL table whose array column ARR collides with a DERIVED table aliased
	// `D` that projects a SCALAR `ARR`. `FROM (SELECT ID AS ARR FROM T1) AS D,
	// D.ARR AS V` must reject (derived-output unnest), NOT validate against this
	// real D's array metadata (the review P1 derived-alias-shadows-real-table case).
	b.AddTable("D", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("ARR", api.NewArrayType(api.NewIntegerType(false), true), 2),
	}, []string{"ID"})
	// DSC is a REAL table whose ARR column is a SCALAR. A CTE named `DSC` whose
	// OUTPUT column ARR is also a scalar SHADOWS this real table. `WITH DSC AS (…)
	// SELECT O FROM DSC, DSC.ARR AS V AT O` carries an AT alias, so the early
	// rejectAtOrdinalityOnTable pass runs FIRST — it must detect the CTE/derived
	// binding for segment 0 and SKIP the base-table AT-on-non-array check, leaving
	// the rejection to the translator's CTE-output UNSUPPORTED_QUERY. Without the
	// detection it would resolve segment 0 to this SHADOWED real DSC, see the scalar
	// ARR, and raise 42809 (WRONG_OBJECT_TYPE) — diverging from the translator's
	// intended code. (DSC's ARR is a scalar so the early base-table check WOULD fire
	// on revert, distinguishing the bug from the array-`D` case where it never did.)
	// RFC-142.
	b.AddTable("DSC", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("ARR", api.NewIntegerType(true), 2),
	}, []string{"ID"})
	// UV carries a scalar V matched by a CORRELATED EXISTS subquery against the
	// unnested ELEMENT: `... AS VAL WHERE EXISTS (SELECT 1 FROM UV WHERE UV.V =
	// VAL)`. Its V values are a PROPER SUBSET of T1.ARR1's elements (201, 203) so
	// the existential is non-trivially true for SOME elements and false for others
	// — the probe round 9 P2c case (the inner query must see the unnest binding VAL
	// in its outer scope, else translation fails). RFC-142.
	b.AddTable("UV", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("V", api.NewIntegerType(true), 2),
	}, []string{"ID"})
	// MA (array ARR + scalar C) and MB (scalar D) drive the MULTI-SOURCE-OUTER
	// unnest cases (`FROM MA, MB, MA.ARR AS X`): the unnest follows TWO prior
	// sources, so MA is NOT the rightmost FROM leg (MB is). MA.C is an outer-leg
	// scalar on the NON-rightmost source; MB.D is the rightmost leg's scalar. The
	// values are chosen so a wrong-key read is visibly different from the right key
	// (the merged row's bare keys vs the qualified MA.C / MB.D). RFC-142.
	b.AddTable("MA", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("ARR", api.NewArrayType(api.NewIntegerType(false), true), 2),
		metadata.NewColumnSpec("C", api.NewIntegerType(true), 3),
	}, []string{"ID"})
	b.AddTable("MB", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("D", api.NewIntegerType(true), 2),
	}, []string{"ID"})
	// JU is an explicit-JOIN target whose ID matches a PROPER SUBSET of T1's ids
	// (1 and 2, NOT 0 or 3). `FROM T1 INNER JOIN JU ON JU.ID = T1.ID, T1.ARR1 AS X`
	// must apply the ON predicate (keep only T1 ids 1 and 2 → 4 unnested elements),
	// NOT degrade to a CROSS join (which would also pair every other T1 row).
	// JU.K is a distinct scalar so the row carries a visible per-leg value. RFC-142.
	b.AddTable("JU", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewIntegerType(true), 2),
	}, []string{"ID"})
	// GD (group-duplicate) carries arrays whose ELEMENT VALUES RECUR across outer
	// rows in NON-CONTIGUOUS scan positions: id1.ARR={1,2}, id2.ARR={1,2}, so the
	// element-flow scan order is 1,2,1,2 — value 1 at positions 0 and 2, value 2 at
	// positions 1 and 3, never adjacent. GW (group-witness) carries a SCALAR V whose
	// value (999) is DISTINCT from every element. `SELECT V, COUNT(*) FROM GD,
	// GD.ARR AS V, GW GROUP BY V` groups by the SHADOWING unnest element V.V (round-15
	// qualification), but the streaming aggregate's REQUIRED pre-aggregate InMemorySort
	// must order by the SAME key — if it sorts by the BARE `V` (which mergeRows keys
	// last-leg-wins as GW.V=999, a constant → a NO-OP sort), the scan order stays
	// 1,2,1,2 and the streaming aggregate splits each value into TWO non-contiguous
	// groups → wrong counts (the streaming-aggregate twin of the in-memory
	// ORDER BY case below). With the fix the pre-aggregate sort carries the
	// qualified V.V, ordering 1,1,2,2 so each value is ONE group of count 2. RFC-142.
	// SARR is a STRING array whose element values distinguish an escaped LIKE
	// wildcard (`!_` = a literal underscore) from an unescaped one (`_` = any
	// single char): {"a_b","axy"}. `HAVING V LIKE 'a!_%' ESCAPE '!'` matches
	// ONLY "a_b" (literal underscore at position 2); without the escape `a_%`
	// matches BOTH (the `_` wildcard accepts the `x` in "axy"). Drives the
	// grouped-unnest HAVING-rebase escape-preservation check. RFC-142.
	b.AddTable("GD", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("ARR", api.NewArrayType(api.NewIntegerType(false), false), 2),
		metadata.NewColumnSpec("SARR", api.NewArrayType(api.NewStringType(false), false), 3),
	}, []string{"ID"})
	// GW carries BOTH a `V` and an `O` column so it shadows the BARE element key
	// (`V`) AND the BARE ordinal key (`O`) of the unnest in `FROM GD, GD.ARR AS V AT
	// O, GW` — mergeRows keys both last-leg-wins to GW's constants (V=999, O=888),
	// distinct from every element/ordinal, so a buggy bare-key pre-aggregate sort is a
	// NO-OP for BOTH the element and the ordinal grouping. The fix's qualified V.V /
	// V.O sort keys are immune. RFC-142.
	b.AddTable("GW", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("V", api.NewIntegerType(true), 2),
		metadata.NewColumnSpec("O", api.NewIntegerType(true), 3),
	}, []string{"ID"})
	// EXA and EXB drive the R31 P2a no-alias schema-qualified subquery source check.
	// `... WHERE EXISTS (SELECT 1 FROM EXA, s.EXB WHERE EXB.ID = EXA.ID)`: EXB is a
	// NO-ALIAS schema-qualified source whose bare table-name reference `EXB.ID` must
	// resolve to the scan EXB binds under. EXA={100,200} and EXB={200,300} OVERLAP on
	// 200, so a WORKING `EXB.ID = EXA.ID` finds the satisfying pair (200,200) → EXISTS
	// TRUE → every outer row kept. Pre-fix the scan binds under `S.EXB` while the
	// resolver (reading the normalized source) uses `EXB`, so `EXB.ID` reads NULL →
	// `NULL = EXA.ID` is false for every pair → EXISTS FALSE → ALL outer rows silently
	// DROPPED. The non-empty result is the revert sentinel. RFC-142.
	b.AddTable("EXA", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
	}, []string{"ID"})
	b.AddTable("EXB", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
	}, []string{"ID"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	md := tmpl.Underlying()
	desc := md.GetRecordType("T1").Descriptor
	collDesc := md.GetRecordType("TCOLL").Descriptor
	paDesc := md.GetRecordType("PA").Descriptor
	pbDesc := md.GetRecordType("PB").Descriptor
	uDesc := md.GetRecordType("U").Descriptor
	dDesc := md.GetRecordType("D").Descriptor
	uvDesc := md.GetRecordType("UV").Descriptor
	maDesc := md.GetRecordType("MA").Descriptor
	mbDesc := md.GetRecordType("MB").Descriptor
	juDesc := md.GetRecordType("JU").Descriptor
	gdDesc := md.GetRecordType("GD").Descriptor
	gwDesc := md.GetRecordType("GW").Descriptor
	exaDesc := md.GetRecordType("EXA").Descriptor
	exbDesc := md.GetRecordType("EXB").Descriptor

	setIntArr := func(m *dynamicpb.Message, d protoreflect.MessageDescriptor, name string, vals []int32) {
		fd := d.Fields().ByName(protoreflect.Name(name))
		list := m.NewField(fd).List()
		for _, v := range vals {
			list.Append(protoreflect.ValueOfInt32(v))
		}
		m.Set(fd, protoreflect.ValueOfList(list))
	}
	setStrArr := func(m *dynamicpb.Message, name string, vals []string) {
		fd := desc.Fields().ByName(protoreflect.Name(name))
		list := m.NewField(fd).List()
		for _, v := range vals {
			list.Append(protoreflect.ValueOfString(v))
		}
		m.Set(fd, protoreflect.ValueOfList(list))
	}
	setStrArrD := func(m *dynamicpb.Message, d protoreflect.MessageDescriptor, name string, vals []string) {
		fd := d.Fields().ByName(protoreflect.Name(name))
		list := m.NewField(fd).List()
		for _, v := range vals {
			list.Append(protoreflect.ValueOfString(v))
		}
		m.Set(fd, protoreflect.ValueOfList(list))
	}
	// rec builds a T1 record. arr1=nil leaves the array unset (NULL).
	rec := func(id int64, arr1 []int32, arr1nn []int32, strs []string) proto.Message {
		m := dynamicpb.NewMessage(desc)
		m.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		if arr1 != nil {
			setIntArr(m, desc, "ARR1", arr1)
		}
		setIntArr(m, desc, "ARR1_NN", arr1nn)
		if strs != nil {
			setStrArr(m, "STRARR", strs)
		}
		return m
	}
	collRec := func(id int64, arr []int32, val int32) proto.Message {
		m := dynamicpb.NewMessage(collDesc)
		m.Set(collDesc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		setIntArr(m, collDesc, "ARR", arr)
		m.Set(collDesc.Fields().ByName("VAL"), protoreflect.ValueOfInt32(val))
		return m
	}
	arrRec := func(d protoreflect.MessageDescriptor, id int64, arr []int32) proto.Message {
		m := dynamicpb.NewMessage(d)
		m.Set(d.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		setIntArr(m, d, "ARR", arr)
		return m
	}
	uRec := func(id int64, v int32) proto.Message {
		m := dynamicpb.NewMessage(uDesc)
		m.Set(uDesc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(uDesc.Fields().ByName("V"), protoreflect.ValueOfInt32(v))
		return m
	}
	uvRec := func(id int64, v int32) proto.Message {
		m := dynamicpb.NewMessage(uvDesc)
		m.Set(uvDesc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(uvDesc.Fields().ByName("V"), protoreflect.ValueOfInt32(v))
		return m
	}
	// maRec builds an MA record (array ARR + scalar C). mbRec builds an MB record
	// (scalar D). The multi-source unnest cases cross MA × MB then unnest MA.ARR.
	maRec := func(id int64, arr []int32, c int32) proto.Message {
		m := dynamicpb.NewMessage(maDesc)
		m.Set(maDesc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		setIntArr(m, maDesc, "ARR", arr)
		m.Set(maDesc.Fields().ByName("C"), protoreflect.ValueOfInt32(c))
		return m
	}
	mbRec := func(id int64, d int32) proto.Message {
		m := dynamicpb.NewMessage(mbDesc)
		m.Set(mbDesc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(mbDesc.Fields().ByName("D"), protoreflect.ValueOfInt32(d))
		return m
	}
	juRec := func(id int64, k int32) proto.Message {
		m := dynamicpb.NewMessage(juDesc)
		m.Set(juDesc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(juDesc.Fields().ByName("K"), protoreflect.ValueOfInt32(k))
		return m
	}
	// gdRec builds a GD record with BOTH the integer ARR and the string SARR.
	gdRec := func(id int64, arr []int32, sarr []string) proto.Message {
		m := dynamicpb.NewMessage(gdDesc)
		m.Set(gdDesc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		setIntArr(m, gdDesc, "ARR", arr)
		setStrArrD(m, gdDesc, "SARR", sarr)
		return m
	}
	gwRec := func(id int64, v, o int32) proto.Message {
		m := dynamicpb.NewMessage(gwDesc)
		m.Set(gwDesc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(gwDesc.Fields().ByName("V"), protoreflect.ValueOfInt32(v))
		m.Set(gwDesc.Fields().ByName("O"), protoreflect.ValueOfInt32(o))
		return m
	}
	// idRec builds a single-column (ID-only) record for EXA / EXB.
	idRec := func(d protoreflect.MessageDescriptor, id int64) proto.Message {
		m := dynamicpb.NewMessage(d)
		m.Set(d.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		return m
	}

	_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if sErr != nil {
			return nil, sErr
		}
		recs := []proto.Message{
			// id 0: empty arrays. id 1: single element. id 2: three elements.
			// id 3: NULL arr1, single arr1_nn.
			rec(0, []int32{}, []int32{}, []string{}),
			rec(1, []int32{101}, []int32{101}, []string{"a"}),
			rec(2, []int32{201, 202, 203}, []int32{201, 202, 203}, []string{"x", "y"}),
			rec(3, nil, []int32{301}, nil),
			// TCOLL: a real VAL=777 decoy the unnest alias VAL must shadow.
			collRec(1, []int32{101}, 777),
			collRec(2, []int32{201, 202, 203}, 777),
			// PA/PB: same-named ARR column, DIFFERENT contents — P2b. A single row
			// each so the cross-product is a clean 1×1.
			arrRec(paDesc, 1, []int32{10, 11}),
			arrRec(pbDesc, 1, []int32{90, 91, 92}),
			// U: a single row whose scalar V=999 is DISTINCT from every unnested
			// element value. `FROM t, t.arr AS v, u` SELECT v must return
			// the unnested element, never 999.
			uRec(1, 999),
			// D: the REAL table D (array column ARR={50,51}) that a DERIVED table
			// aliased `D` shadows in the P1 case. A genuine `FROM D, D.ARR AS X`
			// unnest of the real D must still work (the structural derived-table
			// detection fires only for a LogicalCTE-shadowed alias).
			arrRec(dDesc, 1, []int32{50, 51}),
			// UV: scalar V values that are a PROPER SUBSET of T1.ARR1's elements
			// {201, 203} (NOT 101, NOT 202) — so a correlated EXISTS over the unnest
			// ELEMENT (`WHERE UV.V = VAL`) is true for VAL∈{201,203} and false for
			// VAL∈{101,202}: a non-trivial subset that makes the test
			// revert-proof. RFC-142.
			uvRec(1, 201),
			uvRec(2, 203),
			// MA: TWO rows with DIFFERENT scalar C (11 and 5) so ORDER BY C is a
			// real sort, not a constant. MA1.ARR={10,11,12} contains MA1.C=11 so
			// `WHERE X = MA.C` keeps exactly one element (11); MA2.ARR={20,21} does
			// NOT contain MA2.C=5 so it contributes nothing — the equality genuinely
			// discriminates. MB: ONE row D=10 so the MA×MB cross is clean and
			// `WHERE X > MB.D` (>10) keeps {11,12} from MA1 and {20,21} from MA2.
			// RFC-142 multi-source-outer unnest.
			maRec(1, []int32{10, 11, 12}, 11),
			maRec(2, []int32{20, 21}, 5),
			mbRec(1, 10),
			// JU: rows matching a PROPER SUBSET of T1's ids (1 and 2, NOT 0/3).
			// The explicit `INNER JOIN JU ON JU.ID = T1.ID` before a comma unnest must
			// keep ONLY T1 ids 1 and 2 (their array elements), NOT cross-join every
			// T1 row. JU.K is a distinct per-leg scalar (1001 for id1, 2002 for id2).
			juRec(1, 1001),
			juRec(2, 2002),
			// GD: TWO rows whose arrays REPEAT the same element values across rows in
			// non-contiguous flow order — id1.ARR={1,2}, id2.ARR={1,2}. The element scan
			// order is 1,2,1,2: value 1 at flow positions 0,2 and value 2 at 1,3, never
			// adjacent. A no-op pre-aggregate sort (the round-19 bug) leaves this order, so
			// the streaming aggregate splits each value into two groups. The fix sorts by
			// the qualified element key (1,1,2,2) → one group per value, count 2.
			gdRec(1, []int32{1, 2}, []string{"a_b", "axy"}),
			gdRec(2, []int32{1, 2}, []string{"a_b", "axy"}),
			// GW: a single witness row whose scalars V=999, O=888 shadow the bare `V` and
			// `O` keys (the last-leg-wins constants a buggy bare-key sort would order by).
			gwRec(1, 999, 888),
			// EXA / EXB: OVERLAP on id 200 so `EXB.ID = EXA.ID` finds a satisfying pair
			// (the P2a no-alias schema-qualified subquery source check). EXB also has
			// 300 (no EXA match) so the predicate genuinely discriminates within EXB.
			idRec(exaDesc, 100), idRec(exaDesc, 200),
			idRec(exbDesc, 200), idRec(exbDesc, 300),
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

	// queryOrdered plans + executes a SELECT, returning the "k=v|k=v" row strings
	// in EXECUTION order (no post-sort) — used by ORDER BY assertions where the row
	// ordering itself is under test. `query` is the order-insensitive companion that
	// post-sorts for set comparisons.
	queryOrdered := func(t *testing.T, sql string) (string, []string) {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
		if perr != nil {
			t.Fatalf("plan %q: %v", sql, perr)
		}
		explain := plan.Explain()
		var out []string
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
				keys := make([]string, 0, len(m))
				for k := range m {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				parts := make([]string, 0, len(keys))
				for _, k := range keys {
					parts = append(parts, k+"="+unnestSprint(m[k]))
				}
				out = append(out, strings.Join(parts, "|"))
			}
			return nil, nil
		})
		if eerr != nil {
			t.Fatalf("exec %q: %v", sql, eerr)
		}
		return explain, out
	}

	// query plans + executes a SELECT, returning sorted "k=v|k=v" row strings.
	query := func(t *testing.T, sql string) (string, []string) {
		t.Helper()
		explain, out := queryOrdered(t, sql)
		sort.Strings(out)
		return explain, out
	}

	assertRows := func(t *testing.T, sql string, want []string) string {
		t.Helper()
		explain, got := query(t, sql)
		sort.Strings(want)
		if !unnestEqualStrs(got, want) {
			t.Fatalf("query %q\n got=%v\nwant=%v\nplan=%s", sql, got, want, explain)
		}
		return explain
	}

	// assertRowsOrdered checks the rows AND their exact order (for ORDER BY tests):
	// `want` is compared positionally against the execution-order output, NOT sorted.
	assertRowsOrdered := func(t *testing.T, sql string, want []string) string {
		t.Helper()
		explain, got := queryOrdered(t, sql)
		if !unnestEqualStrs(got, want) {
			t.Fatalf("ordered query %q\n got=%v\nwant=%v\nplan=%s", sql, got, want, explain)
		}
		return explain
	}

	// assertColumns pins the user-visible RESULT-SET COLUMN labels a query
	// advertises — the metadata the driver returns via rows.Columns(), derived from
	// the SAME production code (embedded.ResultColumnLabelsForPlan →
	// deriveColumnsFromPlan, the function the live Execute() path calls). This is a
	// distinct dimension from the per-row datum map the `query` helper inspects: the
	// datum map carries extra resolution-convenience keys (bare AND qualified
	// ALIAS.COL), so a dropped column shows up ONLY in the column metadata, not the
	// row datum. Used for `SELECT *` over a lateral unnest, where the element/ordinal
	// columns cannot be seeded through the SQL driver (no array literal). RFC-142.
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

	t.Run("base unnest nullable array", func(t *testing.T) {
		plan := assertRows(t, `SELECT "ID", "VAL" FROM T1, T1."ARR1" AS "VAL"`, []string{
			"ID=1|VAL=101", "ID=2|VAL=201", "ID=2|VAL=202", "ID=2|VAL=203",
		})
		// FlatMap over a (non-ordinal) Explode — NO WITH ORDINALITY.
		unnestMustContain(t, plan, "FlatMap")
		unnestMustContain(t, plan, "Explode")
		unnestMustNotContain(t, plan, "WITH ORDINALITY")
	})

	t.Run("with ordinality 1-based resets per row", func(t *testing.T) {
		plan := assertRows(t, `SELECT "ID", "VAL", "AT" FROM T1, T1."ARR1" AS "VAL" AT "AT"`, []string{
			"AT=1|ID=1|VAL=101", "AT=1|ID=2|VAL=201", "AT=2|ID=2|VAL=202", "AT=3|ID=2|VAL=203",
		})
		unnestMustContain(t, plan, "WITH ORDINALITY")
	})

	t.Run("ordinality on NOT NULL array includes id3", func(t *testing.T) {
		assertRows(t, `SELECT "ID", "VAL", "AT" FROM T1, T1."ARR1_NN" AS "VAL" AT "AT"`, []string{
			"AT=1|ID=1|VAL=101", "AT=1|ID=2|VAL=201", "AT=2|ID=2|VAL=202", "AT=3|ID=2|VAL=203",
			"AT=1|ID=3|VAL=301",
		})
	})

	t.Run("AT only no AS", func(t *testing.T) {
		plan := assertRows(t, `SELECT "ID", "AT" FROM T1, T1."ARR1" AT "AT"`, []string{
			"AT=1|ID=1", "AT=1|ID=2", "AT=2|ID=2", "AT=3|ID=2",
		})
		unnestMustContain(t, plan, "WITH ORDINALITY")
	})

	t.Run("AT only filter on ordinal equality", func(t *testing.T) {
		// P2a: AT-only (no AS), WHERE on the ordinal. The AT alias must drive the
		// inner correlation so the predicate pushes into the inner Explode filter.
		// `AT = 1` keeps ONLY the first element of each array — the original array
		// position, not the (single) filtered rank. Before the fix the predicate
		// was an unbound outer filter and returned wrong/empty rows.
		plan := assertRows(t, `SELECT "ID", "AT" FROM T1, T1."ARR1" AT "AT" WHERE "AT" = 1`, []string{
			"AT=1|ID=1", "AT=1|ID=2",
		})
		// The ordinal predicate pushes into the inner Explode.
		unnestMustContain(t, plan, "WITH ORDINALITY")
		unnestMustContain(t, plan, "PredicatesFilter")
	})

	t.Run("AT only filter on ordinal arithmetic", func(t *testing.T) {
		// P2a: AT-only, WHERE on a computed ordinal (`AT + 1 = 3` ⇒ ordinal 2).
		// Only id2's array has a 2nd element, so exactly one row survives.
		plan := assertRows(t, `SELECT "ID", "AT" FROM T1, T1."ARR1" AT "AT" WHERE "AT" + 1 = 3`, []string{
			"AT=2|ID=2",
		})
		unnestMustContain(t, plan, "WITH ORDINALITY")
		unnestMustContain(t, plan, "PredicatesFilter")
	})

	t.Run("filter on element preserves ordinal", func(t *testing.T) {
		// arr1_nn so id3 is present; element>201 keeps the ORIGINAL position as
		// the ordinal (202→2, 203→3, 301→1), not the filtered rank.
		plan := assertRows(t, `SELECT "ID", "V", "AT" FROM T1, T1."ARR1_NN" AS "V" AT "AT" WHERE "V" > 201`, []string{
			"AT=2|ID=2|V=202", "AT=3|ID=2|V=203", "AT=1|ID=3|V=301",
		})
		// The element filter pushes into the inner Explode.
		unnestMustContain(t, plan, "PredicatesFilter")
	})

	t.Run("filter on ordinal", func(t *testing.T) {
		assertRows(t, `SELECT "ID", "V", "AT" FROM T1, T1."ARR1_NN" AS "V" AT "AT" WHERE "AT" + 1 = 3`, []string{
			"AT=2|ID=2|V=202",
		})
	})

	t.Run("string array ordinality", func(t *testing.T) {
		assertRows(t, `SELECT "ID", "VAL", "AT" FROM T1, T1."STRARR" AS "VAL" AT "AT"`, []string{
			"AT=1|ID=1|VAL=a", "AT=1|ID=2|VAL=x", "AT=2|ID=2|VAL=y",
		})
	})

	t.Run("alias does not collide with real same-named column", func(t *testing.T) {
		// TCOLL has a REAL VAL column (777). `... AS "VAL"` must bind to the
		// unnest element (101/201/...), NOT the decoy outer VAL=777 — the exact
		// row assertion (no 777) proves the unnest binding shadows the column.
		assertRows(t, `SELECT "ID", "VAL" FROM TCOLL, TCOLL."ARR" AS "VAL"`, []string{
			"ID=1|VAL=101", "ID=2|VAL=201", "ID=2|VAL=202", "ID=2|VAL=203",
		})
	})

	// The ordinal being an INT usable in arithmetic is proven by the "filter on
	// ordinal" subtests above (`WHERE "AT" + 1 = 3`) AND the computed-projection
	// subtest below. The AT-ordinal COLUMN TYPE (non-null INT, not UNKNOWN) is
	// pinned at plan time by TestFDB_ArrayUnnestOrdinalityColumnType (RFC-142
	// round-21) — that guards the metadata dimension the row-value subtests don't.

	t.Run("computed projection over ordinal returns correct integers", func(t *testing.T) {
		// `SELECT id, ord + 1` over the AT ordinal. The ordinal must resolve to a
		// usable INT so the arithmetic evaluates: id1 has one element (ord 1 → 2),
		// id2 has three (ords 1,2,3 → 2,3,4). The raw-executor row map keys the
		// computed column by the projection's canonical explain (not the alias);
		// read the non-ID key per row. A regressed-to-UNKNOWN ordinal type does NOT
		// stop the value from computing (the runtime int is correct regardless), so
		// this subtest proves the VALUE while the plan-time type test proves the
		// TYPE — the two together fully pin the ordinal's INT-ness.
		_, perr := embedded.PlanRecordQueryWithMetadata(`SELECT "ID", "AT" + 1 FROM T1, T1."ARR1" AT "AT"`, md, nil)
		if perr != nil {
			t.Fatalf("plan computed ordinal: %v", perr)
		}
		_, rows := queryOrdered(t, `SELECT "ID", "AT" + 1 FROM T1, T1."ARR1" AT "AT"`)
		// queryOrdered renders each row as "k=v|k=v" with keys sorted; the ID key is
		// "ID=<n>", the computed key is whatever canonical name the executor used.
		// Pin the SET of (id, computed) pairs by extracting both numeric values.
		got := make([]string, 0, len(rows))
		for _, r := range rows {
			var idPart, compPart string
			for _, kv := range strings.Split(r, "|") {
				if strings.HasPrefix(kv, "ID=") {
					idPart = strings.TrimPrefix(kv, "ID=")
				} else if eq := strings.IndexByte(kv, '='); eq >= 0 {
					compPart = kv[eq+1:]
				}
			}
			got = append(got, idPart+":"+compPart)
		}
		sort.Strings(got)
		want := []string{"1:2", "2:2", "2:3", "2:4"}
		if !unnestEqualStrs(got, want) {
			t.Fatalf("computed ordinal\n got=%v\nwant=%v", got, want)
		}
	})

	t.Run("non-rightmost unnest explodes the classified source array", func(t *testing.T) {
		// P2b: `FROM PA, PB, PA.ARR AS X` — the unnest follows TWO prior sources,
		// so PA is NOT the rightmost FROM leg. The merged PA×PB row flows under
		// PB's alias and its BARE `arr` is PB.ARR (last-leg-wins). The array field
		// must be read QUALIFIED to the classified source (PA.ARR = {10,11}), NOT
		// the bare key (PB.ARR = {90,91,92}). Before the fix X was PB's elements.
		assertRows(t, `SELECT "X" FROM PA, PB, PA."ARR" AS "X"`, []string{
			"X=10", "X=11",
		})
	})

	t.Run("non-rightmost unnest with ordinality on classified source", func(t *testing.T) {
		// P2b under ordinality: the ordinal is PA.ARR's 1-based position (1,2),
		// proving both the array source AND the ordinal come from PA, not PB.
		assertRows(t, `SELECT "X", "O" FROM PA, PB, PA."ARR" AS "X" AT "O"`, []string{
			"O=1|X=10", "O=2|X=11",
		})
	})

	t.Run("non-rightmost unnest of the rightmost source still correct", func(t *testing.T) {
		// Control: unnest PB.ARR (the rightmost leg) — must be PB's elements,
		// proving the fix does not break the same-leg (bare-key) path.
		assertRows(t, `SELECT "X" FROM PA, PB, PB."ARR" AS "X"`, []string{
			"X=90", "X=91", "X=92",
		})
	})

	// --- MULTI-SOURCE-OUTER unnest: outer-leg WHERE rebase (P2a) + bare outer
	// column carry-through (P2b) ----------------------------------------------
	//
	// `FROM MA, MB, MA.arr AS X` — the unnest follows TWO prior sources, so the
	// unnested array's source MA is NOT the rightmost FROM leg (MB is). The
	// FlatMap binds the merged MA×MB outer row under MB's alias, so a WHERE on an
	// MA-leg column (MA.C) is correlated to QOV(MA) which the inner Explode does
	// NOT bind, and a bare projected/sorted outer column (C) is read off a key the
	// merged-row result value did not carry. Both must read the QUALIFIED MA.C key
	// (rebase) / the BARE C key (mergeRows-faithful result value). RFC-142.

	t.Run("P2a outer-leg WHERE on a non-rightmost source filters correctly", func(t *testing.T) {
		// `FROM MA, MB, MA.ARR AS X WHERE X = MA.C` — MA.C is an outer-leg column
		// of the NON-rightmost source MA. rewriteUnnestPredicate touches only the X
		// reference; MA.C stays correlated to QOV(MA). But the unnest FlatMap binds
		// the merged MA×MB row under MB's alias (the rightmost), so QOV(MA) is
		// UNBOUND inside the inner Explode's PredicatesFilter → `X = NULL` is false
		// for every element → ALL rows DROPPED (got=[]). The fix rebases the
		// outer-leg reference (MA.C) to the qualified `MA.C` key off the merged
		// outer QOV. MA1.C=11 and MA1.ARR={10,11,12} contains 11 → X=11 survives;
		// MA2.C=5 and MA2.ARR={20,21} contains no 5 → nothing. So exactly one row.
		// Before the fix: EMPTY. RFC-142.
		plan := assertRows(t, `SELECT "X" FROM MA, MB, MA."ARR" AS "X" WHERE "X" = MA."C"`, []string{
			"X=11",
		})
		// The element/outer-leg predicate pushes into the inner Explode filter.
		unnestMustContain(t, plan, "Explode")
		unnestMustContain(t, plan, "PredicatesFilter")
	})

	t.Run("P2a outer-leg WHERE carrying the element and outer id", func(t *testing.T) {
		// The same outer-leg-WHERE shape projecting the element X and the bare
		// outer column C alongside it: only X=11 (from MA1, where C=11) survives,
		// and C reads MA1.C=11. Pins that the outer-leg WHERE rebase composes with
		// the bare-outer-column projection (P2b) in one query. RFC-142.
		assertRows(t, `SELECT "C", "X" FROM MA, MB, MA."ARR" AS "X" WHERE "X" = MA."C"`, []string{
			"C=11|X=11",
		})
	})

	t.Run("P2a rightmost-leg WHERE still filters correctly (control)", func(t *testing.T) {
		// Control variant `WHERE X > MB.D` — MB.D is the RIGHTMOST leg's column, the
		// one the merged row flows under. It must NOT be rebased (it is read bare off
		// the merged QOV directly) and must keep filtering correctly. MB.D=10 →
		// `X > 10` keeps {11,12} from MA1.ARR and {20,21} from MA2.ARR. Proves the
		// outer-leg rebase excludes the flow leg, leaving the rightmost-leg WHERE
		// intact. RFC-142.
		assertRows(t, `SELECT "X" FROM MA, MB, MA."ARR" AS "X" WHERE "X" > MB."D"`, []string{
			"X=11", "X=12", "X=20", "X=21",
		})
	})

	t.Run("P2b bare outer column flows through a multi-source unnest", func(t *testing.T) {
		// `SELECT C, X FROM MA, MB, MA.ARR AS X` — C is a BARE outer column of the
		// non-rightmost source MA, X the element. legColumns(LogicalJoin) returns
		// only DOTTED keys (MA.C, MB.D, …); NewAnchoredJoinRecord propagates a
		// dotted leg column verbatim with NO bare form, so the FlatMap RETURN value
		// dropped the merged row's BARE keys → bare `C` read a MISSING key → NULL.
		// The runtime merged outer row carries bare keys (mergeRows), so the fix
		// emits the matching bare fields in the result value. MA1.C=11 (3 elements
		// {10,11,12}), MA2.C=5 (2 elements {20,21}). Before the fix C was NULL for
		// every row. RFC-142.
		plan := assertRows(t, `SELECT "C", "X" FROM MA, MB, MA."ARR" AS "X"`, []string{
			"C=11|X=10", "C=11|X=11", "C=11|X=12",
			"C=5|X=20", "C=5|X=21",
		})
		unnestMustContain(t, plan, "FlatMap")
		unnestMustContain(t, plan, "Explode")
	})

	t.Run("P2b bare outer column with ordinality on a multi-source unnest", func(t *testing.T) {
		// The bare-outer-column carry-through under WITH ORDINALITY: C (MA's bare
		// column) flows alongside the element X and the 1-based ordinal O (resetting
		// per outer row). Proves the bare key composes with the ordinality 2-field
		// record. MA1 (C=11): {10,11,12}→O 1,2,3; MA2 (C=5): {20,21}→O 1,2.
		plan := assertRows(t, `SELECT "C", "X", "O" FROM MA, MB, MA."ARR" AS "X" AT "O"`, []string{
			"C=11|O=1|X=10", "C=11|O=2|X=11", "C=11|O=3|X=12",
			"C=5|O=1|X=20", "C=5|O=2|X=21",
		})
		unnestMustContain(t, plan, "WITH ORDINALITY")
	})

	t.Run("non-ordinal WHERE on element keeps matching elements", func(t *testing.T) {
		// P1a (silent-wrong): a NON-ordinal unnest with a WHERE on the ELEMENT.
		// The Explode flows BARE SCALARS (not map/struct rows), so the element
		// alias VAL must resolve to the WHOLE QuantifiedObjectValue(VAL) — the
		// scalar itself — NOT FieldValue(QOV(VAL),"VAL") (which reads a named
		// subfield of a scalar = NULL, filtering every element out). arr1={101},
		// {201,202,203}; WHERE VAL > 201 keeps {202,203}. Before the fix: EMPTY.
		plan := assertRows(t, `SELECT "ID", "VAL" FROM T1, T1."ARR1" AS "VAL" WHERE "VAL" > 201`, []string{
			"ID=2|VAL=202", "ID=2|VAL=203",
		})
		// Non-ordinal Explode — no WITH ORDINALITY — and the element predicate is
		// pushed into the inner Explode filter (the scalar is bound under VAL).
		unnestMustNotContain(t, plan, "WITH ORDINALITY")
		unnestMustContain(t, plan, "PredicatesFilter")
		unnestMustContain(t, plan, "Explode")
	})

	t.Run("non-ordinal WHERE element exact equality", func(t *testing.T) {
		// P1a exact-match form: WHERE VAL = 202 keeps only that one element.
		// Before the fix the scalar-vs-FieldValue mismatch returned EMPTY.
		assertRows(t, `SELECT "ID", "VAL" FROM T1, T1."ARR1" AS "VAL" WHERE "VAL" = 202`, []string{
			"ID=2|VAL=202",
		})
	})

	t.Run("non-ordinal WHERE element on NOT NULL array includes id3", func(t *testing.T) {
		// Same P1a path over the NOT-NULL array so id3 (arr1_nn={301}) is present:
		// WHERE V >= 202 keeps {202,203,301}. Pins the scalar-element binding
		// across all three surviving outer rows.
		assertRows(t, `SELECT "ID", "V" FROM T1, T1."ARR1_NN" AS "V" WHERE "V" >= 202`, []string{
			"ID=2|V=202", "ID=2|V=203", "ID=3|V=301",
		})
	})

	t.Run("WITH ORDINALITY element filter still works (control)", func(t *testing.T) {
		// Control proving the P1a non-ordinal rewrite did not disturb the
		// ordinality path: the element filter under WITH ORDINALITY keeps the
		// ORIGINAL array position as the ordinal (202→2, 203→3), not a renumbered
		// rank.
		plan := assertRows(t, `SELECT "ID", "V", "AT" FROM T1, T1."ARR1" AS "V" AT "AT" WHERE "V" > 201`, []string{
			"AT=2|ID=2|V=202", "AT=3|ID=2|V=203",
		})
		unnestMustContain(t, plan, "WITH ORDINALITY")
		unnestMustContain(t, plan, "PredicatesFilter")
	})

	t.Run("aliasless unnest binds element by field name", func(t *testing.T) {
		// P1b (panic): `FROM t, t.arr` with NEITHER AS nor AT. Java's
		// visitAtomTableItem defaults the binding alias to the source name
		// (visitTableName) when AS is absent; RFC-142 defaults the element's
		// binding to the LAST SEGMENT (the array field name ARR1), so the
		// element is referenceable as ARR1 and no zero CorrelationIdentifier is
		// ever created. Before the fix this PANICKED in NewQuantifiedObjectValue.
		plan := assertRows(t, `SELECT "ARR1" FROM T1, T1."ARR1"`, []string{
			"ARR1=101", "ARR1=201", "ARR1=202", "ARR1=203",
		})
		unnestMustContain(t, plan, "FlatMap")
		unnestMustContain(t, plan, "Explode")
		unnestMustNotContain(t, plan, "WITH ORDINALITY")
	})

	t.Run("aliasless unnest with outer column still works", func(t *testing.T) {
		// P1b carrying the outer ID through alongside the field-name-bound
		// element. arr1={101},{201,202,203}; the empty (id0) and NULL (id3)
		// arrays contribute no rows.
		assertRows(t, `SELECT "ID", "ARR1" FROM T1, T1."ARR1"`, []string{
			"ARR1=101|ID=1", "ARR1=201|ID=2", "ARR1=202|ID=2", "ARR1=203|ID=2",
		})
	})

	t.Run("aliasless unnest WHERE on field-name element", func(t *testing.T) {
		// P1a + P1b together: the aliasless element (bound to the field name) is
		// filtered. The field-name reference resolves to the whole scalar QOV, so
		// WHERE ARR1 > 201 keeps {202,203}. Before either fix this panicked (P1b)
		// or returned empty (P1a).
		assertRows(t, `SELECT "ARR1" FROM T1, T1."ARR1" WHERE "ARR1" > 201`, []string{
			"ARR1=202", "ARR1=203",
		})
	})

	// --- probe round 7: later-same-named-column-after-unnest (P2) + derived-
	// alias-shadows-real-table (P1) silent-wrong shapes -----------------------

	t.Run("P2 later FROM item same-named column does not overwrite unnest", func(t *testing.T) {
		// probe round 7 P2 (silent-wrong): `FROM t, t.arr AS v, u` where U has a
		// SCALAR column V=999, DISTINCT from every unnested element. A bare
		// `SELECT v` resolves (via the unnest's Shadowing scope source) to the
		// unnest element binding. But the unnest is NOT the rightmost FROM leg —
		// U is — so the outer NestedLoopJoin's mergeRows OVERWRITES the bare `v`
		// key last-leg-wins with U.V=999. Before the fix `SELECT v` returned 999
		// (U's column) for every row. The fix qualifies the bare unnest reference
		// to the unnest correlation (`v.v`), which mergeRows preserves verbatim —
		// so it reads the UNNESTED element (101/201/202/203), never 999. RFC-142.
		plan := assertRows(t, `SELECT "V" FROM T1, T1."ARR1" AS "V", U`, []string{
			"V=101", "V=201", "V=202", "V=203",
		})
		// The bare unnest column is projected QUALIFIED to the unnest correlation
		// (V.V) so the later U.V cannot clobber it; still labeled V to the user.
		unnestMustContain(t, plan, "FlatMap")
		unnestMustContain(t, plan, "Explode")
	})

	t.Run("P2 unnest element with outer id alongside a later same-named column", func(t *testing.T) {
		// The same shape carrying the outer T1.ID through: each unnested element
		// (101/201/202/203) pairs with its outer ID, and U.V=999 never appears.
		assertRows(t, `SELECT T1."ID", "V" FROM T1, T1."ARR1" AS "V", U`, []string{
			"T1.ID=1|V=101", "T1.ID=2|V=201", "T1.ID=2|V=202", "T1.ID=2|V=203",
		})
	})

	t.Run("P2 explicitly-qualified later column still reads that column", func(t *testing.T) {
		// Control: the fix must NOT over-shadow — an EXPLICITLY qualified `U.V`
		// reference reads U's column (999), not the unnest element. Proves the
		// shadowing qualification only redirects a BARE `v`, the ambiguous one the
		// unnest binding owns; `U.V` is unambiguous and unaffected.
		assertRows(t, `SELECT "U"."V" FROM T1, T1."ARR1" AS "V", U`, []string{
			"U.V=999", "U.V=999", "U.V=999", "U.V=999",
		})
	})

	t.Run("later FROM source reusing the unnest AS alias is DuplicateAlias", func(t *testing.T) {
		// Silent-wrong hazard: a LATER comma source (`U AS V`) reuses the lateral
		// unnest's AS alias `V`. The translator's collision guard runs at unnest
		// lowering and only sees the LEFT (earlier) sources — it cannot see a later
		// source, which is the RIGHT child of an ancestor join. So the parent join was
		// planned with BOTH legs under alias V; the outer NestedLoopJoin's mergeRows
		// overwrites the unnest's bare/qualified V keys last-leg-wins with U's keys, and
		// `SELECT V` returned U.V=999 (DISTINCT from every unnested element 101/201/
		// 202/203) for every row instead of the element. Now rejected cleanly:
		// a duplicate range-variable name in the same FROM scope. RFC-142.
		assertRejected(t, md, `SELECT "V" FROM T1, T1."ARR1" AS "V", U AS "V"`, api.ErrCodeDuplicateAlias)
	})

	t.Run("later FROM source reusing the unnest AT alias is DuplicateAlias", func(t *testing.T) {
		// The AT (ordinal) alias participates in the same FROM-scope uniqueness rule:
		// `FROM T1, T1.arr AS V AT O, U AS O` reuses the AT alias `O` as a later table
		// alias. Same silent-wrong overwrite path (mergeRows clobbers the ordinal's
		// keys with U's) — rejected cleanly. RFC-142.
		assertRejected(t, md, `SELECT "V" FROM T1, T1."ARR1" AS "V" AT "O", U AS "O"`, api.ErrCodeDuplicateAlias)
	})

	t.Run("later FROM source with a DISTINCT alias still unnests correctly", func(t *testing.T) {
		// Control: a later source with a NON-colliding alias (`U AS W`) is unaffected —
		// the unnest plans and `SELECT V` reads the unnested elements, not U's column.
		// Proves the duplicate-alias rejection is specific to a true collision and does
		// not over-reject a benign later source. RFC-142.
		plan := assertRows(t, `SELECT "V" FROM T1, T1."ARR1" AS "V", U AS "W"`, []string{
			"V=101", "V=201", "V=202", "V=203",
		})
		unnestMustContain(t, plan, "FlatMap")
		unnestMustContain(t, plan, "Explode")
	})

	t.Run("later-source unnest-alias collision inside an EXISTS subquery is DuplicateAlias", func(t *testing.T) {
		// The collision lives in an EXISTS subquery's OWN FROM scope (`FROM T1, T1.arr
		// AS V, U AS V`). The duplicate-alias pass recurses into subquery plans (like the
		// AT-on-table pass), so the same clean rejection surfaces from the subquery, not
		// silent-wrong rows leaking from the inner FROM. RFC-142.
		assertRejected(t, md,
			`SELECT "ID" FROM T1 WHERE EXISTS (SELECT 1 FROM T1, T1."ARR1" AS "V", U AS "V")`,
			api.ErrCodeDuplicateAlias)
	})

	t.Run("P1 derived alias shadowing a real same-named table rejects cleanly", func(t *testing.T) {
		// probe round 7 P1 (silent-wrong): `FROM (SELECT ID AS ARR FROM T1) AS D,
		// D.ARR AS V` where a REAL table D ALSO exists with an ARRAY column ARR.
		// The derived table D's OUTPUT column ARR is the SCALAR `ID` renamed, NOT
		// an array. The derived table's LogicalCTE body is registered into the
		// translator's cteScope only when its leg is translated — AFTER the
		// metadata-validation guard — so the cteScope check missed it, and
		// findOuterScanTable resolved segment 0 (D) to the REAL table D, validating
		// ARR against ITS array metadata and exploding the derived row's SCALAR ARR
		// (one wrong scalar row per outer row). The structural detection
		// (outerSourceIsDerivedTable: a LogicalCTE leg in j.Left whose Name == D)
		// now rejects the derived-output unnest in ALL cases — even when a real
		// same-named table exists — never validating against the real table's
		// metadata. RFC-142.
		assertRejected(t, md, `SELECT "V" FROM (SELECT "ID" AS "ARR" FROM T1) AS "D", "D"."ARR" AS "V"`, api.ErrCodeUnsupportedQuery)
	})

	t.Run("P1 real table D unnest (no derived shadow) still works", func(t *testing.T) {
		// Control: the structural derived-table detection must NOT over-reject a
		// genuine unnest of the REAL table D's array column ARR. `FROM D, D.ARR AS
		// X` is a plain real-table unnest (no LogicalCTE leg named D), so it must
		// PLAN and explode the real D.ARR ({50,51}). Proves the P1 rejection is
		// specific to a derived/CTE-shadowed alias, not the real same-named table.
		plan := assertRows(t, `SELECT "X" FROM D, D."ARR" AS "X"`, []string{
			"X=50", "X=51",
		})
		unnestMustContain(t, plan, "FlatMap")
		unnestMustContain(t, plan, "Explode")
	})

	// --- Rejections (revert-proof on ErrCodeWrongObjectType / UNSUPPORTED) ---

	t.Run("AT on a real table is WRONG_OBJECT_TYPE", func(t *testing.T) {
		assertRejected(t, md, `SELECT "ID", "AT" FROM T1 AT "AT"`, api.ErrCodeWrongObjectType)
	})

	t.Run("AT on a non-array column is WRONG_OBJECT_TYPE", func(t *testing.T) {
		// TCOLL.VAL is a real, PRESENT scalar INT, not an array — AT (and indeed
		// any unnest) on it is invalid → WRONG_OBJECT_TYPE. (T1 has no scalar
		// column; using a table with a genuine present scalar keeps this test
		// honest — a present-non-array source, not a missing field.)
		assertRejected(t, md, `SELECT "ID" FROM TCOLL, TCOLL."VAL" AS "X" AT "AT"`, api.ErrCodeWrongObjectType)
	})

	t.Run("multiple unnests rejected cleanly", func(t *testing.T) {
		// Not yet supported (nested-FlatMap merged-row threading); rejected, never
		// silently wrong.
		assertRejected(t, md, `SELECT "ID", "V1", "V2" FROM T1, T1."ARR1" AS "V1", T1."ARR1_NN" AS "V2"`, api.ErrCodeUnsupportedQuery)
	})

	// --- probe round 4: classifier precision (P1 / P2a / P2b / P2c) ---------
	//
	// The lateral-unnest classifier must be PRECISE: a comma/JOIN source is a
	// lateral unnest IFF segment 0 names a VISIBLE in-scope FROM-source alias in
	// the CURRENT query's FROM scope AND the remaining segment(s) name an
	// array/list field on it. Everything else is a real table or the right clean
	// error — never silent-wrong, never a panic, never a mis-error.

	t.Run("P1 explicit unnest alias collides with outer alias", func(t *testing.T) {
		// `FROM T1 AS X, X.arr AS X` makes the unnest element alias X equal the
		// outer FlatMap correlation X. Binding both under one name lets the inner
		// element overwrite the outer row (silent-wrong). Must be a CLEAN
		// duplicate-alias rejection, NOT wrong rows.
		assertRejected(t, md, `SELECT "ID", "X" FROM T1 AS "X", "X"."ARR1" AS "X"`, api.ErrCodeDuplicateAlias)
	})

	t.Run("P1 aliasless unnest field-name collides with outer alias", func(t *testing.T) {
		// Aliasless `FROM T1 AS ARR1, ARR1.arr1`: the defaulted element alias is
		// the LAST segment (the array field name ARR1), which equals the outer
		// alias ARR1 → innerCorr == outerCorr. Same silent-wrong collision; must
		// be a clean duplicate-alias rejection.
		assertRejected(t, md, `SELECT "ID" FROM T1 AS "ARR1", "ARR1"."ARR1"`, api.ErrCodeDuplicateAlias)
	})

	t.Run("P2a schema-qualified comma source is a cross join not unnest", func(t *testing.T) {
		// `FROM PA, s.PB AS B` — `s.PB` is a SCHEMA-qualified table, NOT a
		// correlated array field of PA (`s` is the session schema, not an
		// in-scope source alias). It must PLAN as a normal cross join (B is a
		// real table) and return the cross product, NOT route through the unnest
		// path. PA(1)×PB(1) = one row; the executor flows both the aliased
		// (PID/BID) and qualified (PA.ID/B.ID) forms of each projected column.
		plan := assertRows(t, `SELECT PA."ID" AS "PID", "B"."ID" AS "BID" FROM PA, s."PB" AS "B"`, []string{
			"B.ID=1|BID=1|PA.ID=1|PID=1",
		})
		// A real cross join — NO Explode/FlatMap unnest machinery.
		unnestMustContain(t, plan, "NestedLoopJoin")
		unnestMustNotContain(t, plan, "Explode")
		unnestMustNotContain(t, plan, "FlatMap")
	})

	t.Run("P2a schema-qualified primary plus normal table still cross joins", func(t *testing.T) {
		// Control: the schema qualifier on the SECOND leg must not perturb the
		// row set vs the bare-name form. Same single cross-product row.
		assertRows(t, `SELECT PA."ID" AS "PID", "B"."ID" AS "BID" FROM PA, PB AS "B"`, []string{
			"B.ID=1|BID=1|PA.ID=1|PID=1",
		})
	})

	t.Run("R5b schema-qualified table where alias equals schema name is a cross join", func(t *testing.T) {
		// probe round 5 P2b (valid-query-fails): `FROM PA AS s, s.PB AS B` — the
		// prior source PA is aliased `s`, which ALSO equals the session schema
		// name. So `s.PB` is BOTH "field PB on source s" AND "schema-qualified
		// table PB". Java's generateAccess resolves TABLE first (tableExists:
		// qualifier == schema name AND table PB exists), so `s.PB` is the real
		// table PB — a normal cross join, NOT a correlated unnest of PA. Before the
		// fix the prior-alias match classified `s.PB` as an unnest and column
		// validation failed (the valid query was rejected). It must now plan
		// IDENTICALLY to the un-aliased `FROM PA, s.PB AS B` control below.
		plan := assertRows(t, `SELECT PA."ID" AS "PID", "B"."ID" AS "BID" FROM PA AS "s", "s"."PB" AS "B"`, []string{
			"B.ID=1|BID=1|PA.ID=1|PID=1",
		})
		// A real cross join — NO Explode/FlatMap unnest machinery.
		unnestMustContain(t, plan, "NestedLoopJoin")
		unnestMustNotContain(t, plan, "Explode")
		unnestMustNotContain(t, plan, "FlatMap")
	})

	t.Run("R5b AT on a schema-qualified table whose alias equals schema is WRONG_OBJECT_TYPE", func(t *testing.T) {
		// The alias-collision form still rejects AT cleanly: `FROM PA AS s, s.PB AT
		// "AT"` — even though `s.PB` is the real table PB, an AT clause is only
		// valid on a correlated array. Java rejects AT on a table with
		// WRONG_OBJECT_TYPE (the table branch asserts atAlias.isEmpty()); the AT
		// keeps this on the unnest path so the translator surfaces that code rather
		// than demoting to a (would-be) table cross join. RFC-142.
		assertRejected(t, md, `SELECT PA."ID" FROM PA AS "s", "s"."PB" AT "AT"`, api.ErrCodeWrongObjectType)
	})

	t.Run("R5a CTE-output unnest does not explode the base table array", func(t *testing.T) {
		// probe round 5 P2a (silent-wrong): `WITH T1 AS (SELECT ID AS ARR1 FROM T1)
		// SELECT V FROM T1, T1.ARR1 AS V` — the CTE alias T1 SHADOWS the real record
		// type T1. The CTE OUTPUT column ARR1 is the SCALAR `ID` renamed, NOT an
		// array; but the real base table T1 HAS an array column ARR1. Java validates
		// the unnest field against the in-scope source's OUTPUT type (the CTE's
		// projected columns), where ARR1 is a scalar → not unnestable. Before the
		// fix the translator validated ARR1 against the BASE-TABLE descriptor and
		// silently exploded the base table's ARR1 (wrong column, wrong rows). The
		// CTE-output element type is best-effort UnknownType in the current
		// architecture, so rather than validate against the wrong base-table
		// metadata, an unnest over a CTE/derived-table output is cleanly REJECTED —
		// never the silent base-table explode. RFC-142.
		assertRejected(t, md, `WITH "T1" AS (SELECT "ID" AS "ARR1" FROM T1) SELECT "V" FROM "T1", "T1"."ARR1" AS "V"`, api.ErrCodeUnsupportedQuery)
	})

	t.Run("R5a derived-table-output unnest is rejected not silent-wrong", func(t *testing.T) {
		// The same output-vs-base-table principle for a DERIVED table whose alias is
		// the visible source: `FROM (SELECT ID AS ARR1 FROM T1) AS d, d.ARR1 AS V`.
		// `d` is the visible derived source; its OUTPUT ARR1 is the scalar `ID`, not
		// an array. The unnest correlates to the derived output (not a base table),
		// whose element type is not recoverable here → clean UNSUPPORTED_QUERY,
		// never a silent explode. RFC-142.
		assertRejected(t, md, `SELECT "V" FROM (SELECT "ID" AS "ARR1" FROM T1) AS "d", "d"."ARR1" AS "V"`, api.ErrCodeUnsupportedQuery)
	})

	t.Run("R5a normal CTE and derived queries are unaffected (no over-rejection)", func(t *testing.T) {
		// Guard the fix did not over-reject: a normal CTE reference and a normal
		// derived table that are NOT lateral unnests must still plan and return the
		// correct rows. (T1 ids 1 and 2 carry non-empty arrays; ids 0/3 too — five
		// T1 rows: ids 0,1,2,3.)
		assertRows(t, `WITH "C" AS (SELECT "ID" FROM T1) SELECT "ID" FROM "C"`, []string{
			"ID=0", "ID=1", "ID=2", "ID=3",
		})
		assertRows(t, `SELECT "d"."ID" FROM (SELECT "ID" FROM T1) AS "d"`, []string{
			"ID=0", "ID=1", "ID=2", "ID=3",
		})
	})

	t.Run("R10 real table aliased with a CTE name shadows the CTE and unnests", func(t *testing.T) {
		// probe round 10 (OVER-rejection): `WITH X AS (SELECT ID AS ARR FROM T1)
		// SELECT V FROM D AS X, X.ARR AS V` — a CTE named X exists, but the FROM uses
		// `D AS X`, a REAL table D (array column ARR={50,51}) aliased X, which SHADOWS
		// the unused CTE. Segment 0 `X` resolves to the VISIBLE scan `D AS X`, so the
		// unnest of `X.ARR` is a valid lateral unnest over the real D.ARR. Before the
		// fix the rejection guard checked `outerSourceIsCTE(segment-0-NAME)`, which
		// returned true because a CTE named X is in the global WITH scope — wrongly
		// rejecting this valid query with UNSUPPORTED_QUERY. The fix ties the CTE
		// rejection to the ACTUAL source bound in j.Left (findOuterScanTable resolves
		// `X` → real table `D`; outerSourceIsCTE("D") is false), so the visible alias
		// shadows the same-named CTE exactly as Java's in-scope-alias resolution does.
		// Must PLAN (FlatMap over Explode) and return the unnested D.ARR elements.
		plan := assertRows(t, `WITH "X" AS (SELECT "ID" AS "ARR" FROM T1) SELECT "V" FROM D AS "X", "X"."ARR" AS "V"`, []string{
			"V=50", "V=51",
		})
		unnestMustContain(t, plan, "FlatMap")
		unnestMustContain(t, plan, "Explode")
	})

	t.Run("R10 CTE genuinely used as the unnest source is still rejected", func(t *testing.T) {
		// Control (round-5 boundary preserved): `WITH X AS (SELECT ID AS ARR FROM T1)
		// SELECT V FROM X, X.ARR AS V` — here X IS the CTE, used as the FROM source.
		// Its OUTPUT column ARR is the SCALAR `ID` renamed, not an array, and the CTE
		// output element type is not recoverable here (best-effort UnknownType), so
		// the unnest over a CTE output must still cleanly reject — NOT silently explode
		// a base table. findOuterScanTable resolves `X` → the CTE name `X` (the scan's
		// Table holds the CTE name), and outerSourceIsCTE("X") is true → rejected. This
		// is the SAME revert-proof boundary as the R5a CTE/derived-output cases; only
		// a real table shadowing the CTE name (the test above) is now allowed.
		assertRejected(t, md, `WITH "X" AS (SELECT "ID" AS "ARR" FROM T1) SELECT "V" FROM "X", "X"."ARR" AS "V"`, api.ErrCodeUnsupportedQuery)
	})

	t.Run("P2b dotted ref to a table hidden behind a derived table is not unnest", func(t *testing.T) {
		// `FROM (SELECT ... FROM T1) AS d, T1.arr AS x` — only `d` is a visible
		// FROM-source; `T1` is hidden inside the derived-table body and out of
		// scope. The classifier must NOT correlate an unnest against the hidden
		// T1 scan. It falls to the table path; `T1.arr` is then a schema-
		// qualified table whose qualifier (T1) is not the session schema → a
		// clean UndefinedDatabase error, NOT a silent unnest of the hidden scan.
		assertRejected(t, md, `SELECT "X" FROM (SELECT "ID" FROM T1) AS "d", T1."ARR1" AS "X"`, api.ErrCodeUndefinedDatabase)
	})

	t.Run("P2c scalar correlated field is WRONG_OBJECT_TYPE", func(t *testing.T) {
		// `FROM TCOLL, TCOLL.val AS x` where TCOLL.VAL is a real SCALAR INT — a
		// present but NON-array correlated field. Java's
		// generateCorrelatedFieldAccess asserts repeated type → WRONG_OBJECT_TYPE.
		// (Before the fix it fell to the table path and gave a generic failure.)
		assertRejected(t, md, `SELECT "ID" FROM TCOLL, TCOLL."VAL" AS "X"`, api.ErrCodeWrongObjectType)
	})

	t.Run("AT on a single-name non-source comma source is WRONG_OBJECT_TYPE", func(t *testing.T) {
		// `FROM T1, BOGUS AT "AT"` — a single-segment comma source carrying AT
		// where BOGUS is not a visible source. AT explicitly requests ordinality
		// (valid only on a correlated array), so the classifier must still route
		// it to the unnest path and reject it cleanly — NOT silently drop the AT
		// and treat BOGUS as a plain table scan. RFC-142.
		assertRejected(t, md, `SELECT "ID" FROM T1, "BOGUS" AT "AT"`, api.ErrCodeWrongObjectType)
	})

	t.Run("AT on a schema-qualified table is WRONG_OBJECT_TYPE", func(t *testing.T) {
		// `FROM PA, s.PB AT "AT"` — AT on a schema-qualified TABLE (not a
		// correlated array). Forced onto the unnest path by the AT alias; segment
		// 0 (s) is not a visible source, so it is not a correlated array → clean
		// WRONG_OBJECT_TYPE, matching Java's "AT clause requires an array column".
		assertRejected(t, md, `SELECT PA."ID" FROM PA, s."PB" AT "AT"`, api.ErrCodeWrongObjectType)
	})

	t.Run("P2c missing correlated field is a clean undefined-column error", func(t *testing.T) {
		// `FROM T1, T1.nope AS x` — segment 0 (T1) IS a visible source, but `nope`
		// is NOT a column of T1 (the field is MISSING). Mirroring Java's
		// resolveCorrelatedIdentifier failing the field lookup on a known source,
		// this is a clean UNDEFINED_COLUMN — distinct from the present-non-array
		// scalar case above (WRONG_OBJECT_TYPE). A genuinely-missing field is NOT
		// a wrong-object-type and NEVER a silent-wrong unnest.
		assertRejected(t, md, `SELECT "ID" FROM T1, T1."NOPE" AS "X"`, api.ErrCodeUndefinedColumn)
	})

	// --- probe round 6: two-FROM-scope unnest + AT-on-bare-source (P2a / P3) --

	t.Run("R6 P2a derived-table with its own unnest plus an outer unnest plans", func(t *testing.T) {
		// probe round 6 P2a (valid-query-fails): `FROM (SELECT V FROM PA, PA.ARR AS
		// V) AS d, PB, PB.ARR AS X` is TWO separate FROM scopes. The INNER unnest
		// (PA.ARR AS V) lives inside the derived table `d`'s OWN body; the OUTER
		// scope has exactly ONE unnest (PB.ARR AS X). The multiple-unnest guard
		// (containsLateralUnnest) must inspect only the VISIBLE sources of the
		// current FROM scope — the derived table contributes ONLY its alias `d`
		// (its Main leg), never its hidden body's unnest. Before the fix the
		// recursive walk descended into the derived body, counted d's inner
		// unnest, and wrongly rejected the query as "multiple lateral array
		// unnests in one FROM clause" (UNSUPPORTED_QUERY). It must PLAN and return
		// the correct rows: the derived `d` yields TWO rows (PA.ARR's two elements
		// 10,11) and participates as a cross-join leg; cross PB(id=7, one row);
		// then the OUTER unnest PB.ARR explodes to {90,91,92}, so X∈{90,91,92}
		// appears ONCE per d row (2 d rows × 3 elements = 6 rows). The qualified
		// outer column PB.ID flows through the outer FlatMap merged row alongside
		// the unnested element. RFC-142.
		plan := assertRows(t, `SELECT PB."ID", "X" FROM (SELECT "V" FROM PA, PA."ARR" AS "V") AS "d", PB, PB."ARR" AS "X"`, []string{
			"PB.ID=1|X=90", "PB.ID=1|X=90", "PB.ID=1|X=91",
			"PB.ID=1|X=91", "PB.ID=1|X=92", "PB.ID=1|X=92",
		})
		// The OUTER unnest (PB.ARR) is a FlatMap over an Explode, and the derived
		// table `d` carries its OWN FlatMap/Explode in the inner leg — two
		// SEPARATE scopes, not the rejected multi-unnest shape.
		unnestMustContain(t, plan, "FlatMap")
		unnestMustContain(t, plan, "Explode")
	})

	t.Run("R6 P2a derived-unnest scope select only the outer element", func(t *testing.T) {
		// Same two-scope shape projecting ONLY the outer unnest's element: X must
		// be PB.ARR's elements, each appearing once per derived-`d` row (the
		// derived table emits 2 rows from PA.ARR's two elements, so 2×3 = 6 rows).
		// Pins that the inner derived unnest does not bleed into the outer scope's
		// row-count or column set — and that the outer scope's single unnest is
		// the one that fires.
		assertRows(t, `SELECT "X" FROM (SELECT "V" FROM PA, PA."ARR" AS "V") AS "d", PB, PB."ARR" AS "X"`, []string{
			"X=90", "X=90", "X=91", "X=91", "X=92", "X=92",
		})
	})

	t.Run("R6 P2a with ordinality on the outer unnest of a two-scope FROM", func(t *testing.T) {
		// The two-scope shape under ORDINALITY: the OUTER unnest carries AT, so the
		// ordinal is PB.ARR's 1-based position (1,2,3), resetting per outer row.
		// Proves the outer scope's WITH-ORDINALITY unnest fires independently of
		// the derived table's own (non-ordinal) inner unnest. 2 derived rows × 3
		// PB.ARR elements = 6 rows; ordinal 1,2,3 each appears twice.
		plan := assertRows(t, `SELECT PB."ID", "X", "O" FROM (SELECT "V" FROM PA, PA."ARR" AS "V") AS "d", PB, PB."ARR" AS "X" AT "O"`, []string{
			"O=1|PB.ID=1|X=90", "O=1|PB.ID=1|X=90",
			"O=2|PB.ID=1|X=91", "O=2|PB.ID=1|X=91",
			"O=3|PB.ID=1|X=92", "O=3|PB.ID=1|X=92",
		})
		unnestMustContain(t, plan, "WITH ORDINALITY")
	})

	t.Run("R6 P3 AT on a bare source alias is WRONG_OBJECT_TYPE", func(t *testing.T) {
		// probe round 6 P3 (wrong error code): `FROM T1, T1 AT ord` — AT is present
		// but there are NO field segments (the source is the bare table/alias `T1`,
		// not `T1.field`). Segment 0 (T1) resolves to a visible scan, so it reaches
		// the known-source branch with an EMPTY field name. Before the fix that
		// branch reported UNDEFINED_COLUMN for the empty column name (42703). Since
		// this is AT-on-a-table/source (not on an array field), it must converge
		// with the other AT-on-table rejection paths → WRONG_OBJECT_TYPE (42809).
		// RFC-142.
		assertRejected(t, md, `SELECT "ID", "AT" FROM T1, T1 AT "AT"`, api.ErrCodeWrongObjectType)
	})

	t.Run("R6 P3 AT on a bare aliased source is WRONG_OBJECT_TYPE", func(t *testing.T) {
		// The aliased form `FROM T1 AS Y, Y AT ord` — `Y` is a visible source alias
		// with no field segment; AT on the bare source is still invalid. Pins the
		// fix across the alias-led shape, not just the table-name-led one.
		assertRejected(t, md, `SELECT "ID", "AT" FROM T1 AS "Y", "Y" AT "AT"`, api.ErrCodeWrongObjectType)
	})

	// --- AT on a SINGLE-segment table source: the WRONG_OBJECT_TYPE must NOT be
	// masked by a scope-level UNDEFINED_COLUMN ---------------------------------
	//
	// `FROM T1, U AT O` is a comma source `U` (a REAL distinct TABLE, not a field
	// of T1) carrying an AT ordinal alias. AT on a table is WRONG_OBJECT_TYPE
	// (42809). The bug: the scope-build virtual-unnest registration
	// (isLateralUnnestJoin) used to fire on the UNCONDITIONAL AT shortcut in
	// unnestCandidateShape, so it registered a VIRTUAL unnest source (correlation
	// `O`) instead of the REAL table `U`. A reference to U's own column (`U.ID`)
	// then failed to resolve with a MASKING UNDEFINED_COLUMN (42703) at scope
	// validation BEFORE the translator could raise the intended 42809. The fix:
	// the scope binding registers a virtual unnest ONLY for a GENUINE lateral
	// array (segment 0 a visible in-scope source), so a single-segment AT-on-table
	// source registers the REAL table `U` (its columns resolve), and the
	// translator's WRONG_OBJECT_TYPE is the surfaced error. RFC-142.

	t.Run("AT on a single-segment table referencing its column is WRONG_OBJECT_TYPE", func(t *testing.T) {
		// `SELECT U.ID FROM T1, U AT O` — U is a REAL table aliased nowhere; U.ID is
		// a genuine column of U. The scope must register the real U so U.ID resolves;
		// the translator then raises WRONG_OBJECT_TYPE for the AT on the table U.
		// Before the fix: a MASKING UNDEFINED_COLUMN (42703) on U.ID. Revert-proof on
		// the converged 42809.
		assertRejected(t, md, `SELECT "U"."ID" FROM T1, "U" AT "O"`, api.ErrCodeWrongObjectType)
	})

	t.Run("AT on a single-segment table referencing the AT alias is WRONG_OBJECT_TYPE", func(t *testing.T) {
		// `SELECT O FROM T1, U AT O` — referencing the AT alias O itself. AT on a
		// table is still invalid → WRONG_OBJECT_TYPE, never a 42703 on O. Pins the
		// AT-alias-projection axis the U.ID test does not cover.
		assertRejected(t, md, `SELECT "O" FROM T1, "U" AT "O"`, api.ErrCodeWrongObjectType)
	})

	t.Run("control: real array AT still unnests with virtual scope binding", func(t *testing.T) {
		// Control 1: `T1.ARR1 AS X AT O` is a GENUINE lateral array unnest (segment 0
		// T1 is the visible primary source, ARR1 an array field). It must STILL bind
		// the virtual unnest scope and PLAN (FlatMap over an Explode WITH ORDINALITY),
		// returning the unnested elements with 1-based ordinals — proving the fix did
		// not over-decline the real lateral-array case.
		plan := assertRows(t, `SELECT "ID", "X", "O" FROM T1, T1."ARR1" AS "X" AT "O"`, []string{
			"ID=1|O=1|X=101", "ID=2|O=1|X=201", "ID=2|O=2|X=202", "ID=2|O=3|X=203",
		})
		unnestMustContain(t, plan, "FlatMap")
		unnestMustContain(t, plan, "Explode")
		unnestMustContain(t, plan, "WITH ORDINALITY")
	})

	t.Run("control: AT on a non-array correlated field is WRONG_OBJECT_TYPE", func(t *testing.T) {
		// Control 2: `T1.ID AS X AT O` — segment 0 (T1) IS a visible source but ID is
		// a SCALAR field, not an array. Java's generateCorrelatedFieldAccess asserts
		// repeated type → WRONG_OBJECT_TYPE. Pins that a dotted AT on a present-scalar
		// correlated field still converges on 42809, alongside the table-AT case.
		assertRejected(t, md, `SELECT "ID" FROM T1, T1."ID" AS "X" AT "O"`, api.ErrCodeWrongObjectType)
	})

	t.Run("AT on a single-segment table inside an EXISTS subquery is WRONG_OBJECT_TYPE", func(t *testing.T) {
		// The backstop axis: an AT-on-a-table source buried inside an EXISTS
		// subquery's OWN FROM scope (`... WHERE EXISTS (SELECT 1 FROM UV, U AT O)`).
		// The early per-FROM-scope pass in VisitQuery runs before the subquery plan
		// is attached, so the harness-level rejectAtOrdinalityOnTable backstop must
		// reach it (recursing into subqueryPlans) and surface 42809 — not a masked
		// column error from the subquery's own scope resolution. RFC-142.
		assertRejected(t, md, `SELECT "ID" FROM T1 WHERE EXISTS (SELECT 1 FROM UV, "U" AT "O")`, api.ErrCodeWrongObjectType)
	})

	// --- AT-on-table inside a subquery whose OWN predicate references the real
	// table: the early rejection must run PRE-resolution in the subquery build path,
	// not only as the post-attach backstop -----------------------------------
	//
	// The post-attach backstop (cascades_generator.go) walks an already-ATTACHED
	// subquery tree. But when the subquery's OWN predicate (`WHERE U.ID = …`)
	// references the real table U — which the AT shortcut shadows with a virtual
	// unnest binding (correlation `O`) — resolving U.ID FAILS during subquery
	// construction (inside VisitQuery, before attach) with a MASKING UNDEFINED_COLUMN
	// (42703). The subquery plan is never attached, so the backstop never sees it and
	// the 42703 surfaces instead of the intended WRONG_OBJECT_TYPE (42809). The fix
	// runs the SAME early AT-on-table rejection (rejectAtOrdinalityOnTableWithCTEs) on
	// the subquery's built FROM tree BEFORE its WHERE/projection resolution, in the
	// catalog SELECT builder every subquery + DML + derived-table body flows through.
	// Revert-proof on the converged 42809 (before the fix: 42703). RFC-142.
	t.Run("AT-on-table in EXISTS subquery whose predicate references the table is WRONG_OBJECT_TYPE", func(t *testing.T) {
		// `... WHERE EXISTS (SELECT 1 FROM UV, U AT O WHERE U.ID = 1)` — the inner
		// predicate references U.ID. Before the fix the catalog SELECT builder resolved
		// U.ID against the virtual unnest binding (which shadows the real U) and failed
		// with 42703 BEFORE the post-attach backstop could convert the real AT-on-table
		// problem to 42809.
		assertRejected(t, md, `SELECT "ID" FROM T1 WHERE EXISTS (SELECT 1 FROM UV, "U" AT "O" WHERE "U"."ID" = 1)`, api.ErrCodeWrongObjectType)
	})

	t.Run("AT-on-table in a correlated EXISTS subquery is WRONG_OBJECT_TYPE", func(t *testing.T) {
		// The correlated variant: the inner predicate references BOTH the AT-table U
		// AND the outer query T1 (`U.ID = T1.ID`). The early rejection must fire on the
		// inner FROM tree before either correlation resolves — a correlated EXISTS would
		// otherwise route to buildCorrelatedExists only AFTER the catalog builder
		// returned 42703, masking the 42809. RFC-142.
		assertRejected(t, md, `SELECT "ID" FROM T1 WHERE EXISTS (SELECT 1 FROM UV, "U" AT "O" WHERE "U"."ID" = T1."ID")`, api.ErrCodeWrongObjectType)
	})

	t.Run("AT-on-table in a NOT EXISTS subquery with a predicate is WRONG_OBJECT_TYPE", func(t *testing.T) {
		// The NOT-EXISTS mirror, same masking class — the inner predicate `U.V = 5`
		// references the shadowed real table U. RFC-142.
		assertRejected(t, md, `SELECT "ID" FROM T1 WHERE NOT EXISTS (SELECT 1 FROM UV, "U" AT "O" WHERE "U"."V" = 5)`, api.ErrCodeWrongObjectType)
	})

	t.Run("AT-on-table in a scalar subquery whose predicate references the table is WRONG_OBJECT_TYPE", func(t *testing.T) {
		// The scalar-subquery path (BuildScalar → the same catalog SELECT builder). The
		// projection-position subquery's own WHERE references the shadowed U. RFC-142.
		assertRejected(t, md, `SELECT (SELECT MAX("U"."ID") FROM UV, "U" AT "O" WHERE "U"."ID" = 1) FROM T1`, api.ErrCodeWrongObjectType)
	})

	t.Run("control: genuine unnest inside a subquery whose predicate references the element still plans", func(t *testing.T) {
		// The discriminating control: swap the AT-on-table `U AT O` for a GENUINE
		// lateral array unnest `T.ARR1 AS V` whose inner predicate references the
		// ELEMENT binding V. The subquery's primary source is `T1 AS T`, so segment 0
		// `T` IS a visible in-scope FROM alias and ARR1 an array → the early rejection
		// must NOT fire — the subquery plans, proving the fix declines ONLY the real
		// AT-on-table case, never a genuine unnest. (Planning success is the assertion;
		// the unnest-in-subquery row behaviour is exercised end-to-end by the dedicated
		// P2b / R15 tests.) RFC-142.
		if _, perr := embedded.PlanRecordQueryWithMetadata(
			`SELECT "ID" FROM T1 WHERE EXISTS (SELECT 1 FROM T1 AS "T", "T"."ARR1" AS "V" WHERE "V" > 0)`, md, nil); perr != nil {
			t.Fatalf("genuine unnest inside a subquery should plan, got: %v", perr)
		}
	})

	t.Run("control: normal comma cross join (no AT) is unaffected", func(t *testing.T) {
		// Control 3: `FROM T1, U` (no AT, U a real table) is a plain comma CROSS JOIN.
		// The fix must leave it untouched — a NestedLoopJoin over T1 × U, NO Explode/
		// FlatMap unnest machinery, with both sources' columns resolving. T1 ids
		// {0,1,2,3} × the single U row (id=1) = 4 rows; U.V=999 carried per row. The
		// raw executor flows both the aliased (TID/UV) and qualified (T1.ID/U.V)
		// forms of each projected column (same as the P2a schema-qualified test).
		plan := assertRows(t, `SELECT T1."ID" AS "TID", "U"."V" AS "UV" FROM T1, "U"`, []string{
			"T1.ID=0|TID=0|U.V=999|UV=999", "T1.ID=1|TID=1|U.V=999|UV=999",
			"T1.ID=2|TID=2|U.V=999|UV=999", "T1.ID=3|TID=3|U.V=999|UV=999",
		})
		unnestMustContain(t, plan, "NestedLoopJoin")
		unnestMustNotContain(t, plan, "Explode")
		unnestMustNotContain(t, plan, "FlatMap")
	})

	// --- Explicit JOIN with a dotted array source must NOT lower as a lateral
	// unnest: only the COMMA-syntax `FROM t, t.arr AS x` correlated-field path is
	// a lateral array unnest in Java (generateCorrelatedFieldAccess). An explicit
	// `INNER JOIN t.arr AS x` is resolved as a table/derived source (the JOIN
	// visitor adds it as a normal operator), so a dotted `alias.field` JOIN source
	// falls to the table-resolution path and fails cleanly (`alias` is an unknown
	// database/schema qualifier), exactly as `extractJoinClause` treats every JOIN
	// source as never-lateral. Before the fix the lateral-unnest lowering ran for
	// EVERY fs.joins entry (onExpr alone cannot tell a no-ON inner join from a
	// comma source), so `SELECT V FROM T1 INNER JOIN T1.ARR1 AS V` silently planned
	// as FlatMap(Scan(T1), Explode(ARR1)) and RETURNED the unnested rows instead of
	// surfacing the table/join error. RFC-142 R5. ------------------------------
	t.Run("explicit INNER JOIN with a dotted array source is NOT a lateral unnest", func(t *testing.T) {
		// The bug shape: `FROM T1 INNER JOIN T1.ARR1 AS V` (explicit JOIN, no ON).
		// Pre-fix: planned as FlatMap over Explode → returned T1.ARR1's elements
		// {101, 201, 202, 203}. Post-fix: the dotted `T1.ARR1` JOIN source resolves
		// as a qualified table whose `T1` qualifier is not a known database →
		// ErrCodeUndefinedDatabase (the existing table-not-found path, unchanged).
		// NOT a silent FlatMap(Explode).
		assertRejected(t, md, `SELECT "V" FROM T1 INNER JOIN T1."ARR1" AS "V"`, api.ErrCodeUndefinedDatabase)
	})

	t.Run("control: comma source with the SAME dotted array still unnests", func(t *testing.T) {
		// The discriminating control: swap the explicit `INNER JOIN` for a COMMA —
		// `FROM T1, T1.ARR1 AS V` — and the SAME dotted array source IS a lateral
		// unnest, lowering to FlatMap over Explode and returning T1.ARR1's elements.
		// Proves the gate keys on the comma-vs-JOIN ORIGIN, not on the dotted-source
		// shape (which is identical between the two queries).
		plan := assertRows(t, `SELECT "ID", "V" FROM T1, T1."ARR1" AS "V"`, []string{
			"ID=1|V=101", "ID=2|V=201", "ID=2|V=202", "ID=2|V=203",
		})
		unnestMustContain(t, plan, "FlatMap")
		unnestMustContain(t, plan, "Explode")
	})

	t.Run("control: explicit LEFT JOIN with a dotted array source is NOT a lateral unnest", func(t *testing.T) {
		// The OUTER-join arm of extractJoinClause is the same never-lateral path: an
		// OUTER JOIN whose right source is a dotted `alias.field` resolves as a table.
		// (An OUTER JOIN always carries an ON clause per the grammar, so the unnest
		// classifier already excluded it via the onExpr guard pre-fix — this is a
		// breadth control over the OUTER arm, not the revert-proof sentinel. The INNER
		// JOIN test above — a no-ON join the onExpr guard cannot catch — is the
		// revert-proof one.) `T1` is an unknown database qualifier → clean rejection.
		assertRejected(t, md, `SELECT "V" FROM T1 LEFT JOIN T1."ARR1" AS "V" ON "V" = 1`, api.ErrCodeUndefinedDatabase)
	})

	t.Run("control: normal explicit INNER JOIN with ON is unaffected", func(t *testing.T) {
		// `FROM T1 INNER JOIN U ON U.ID = T1.ID` — a plain explicit JOIN over real
		// tables. The gate must leave it a real join (no Explode/FlatMap unnest):
		// T1 ids {0,1,2,3} INNER-joined with the single U row (id=1) on ID → one row.
		plan := assertRows(t, `SELECT T1."ID" AS "TID", "U"."V" AS "UV" FROM T1 INNER JOIN "U" ON "U"."ID" = T1."ID"`, []string{
			"T1.ID=1|TID=1|U.V=999|UV=999",
		})
		unnestMustNotContain(t, plan, "Explode")
	})

	// --- AT on a CTE/derived source whose alias ALSO names a REAL same-named
	// table: the early rejectAtOrdinalityOnTable pass must NOT raise 42809 keyed
	// on the SHADOWED base table; the translator's CTE-output rejection
	// (UNSUPPORTED_QUERY) must surface --------------------------------------
	//
	// `WITH DSC AS (SELECT ID AS ARR FROM E) SELECT O FROM DSC, DSC.ARR AS V AT O`
	// where a REAL table DSC exists with a SCALAR ARR. The AT alias makes the early
	// rejectAtOrdinalityOnTable pass run FIRST. Before the fix it resolved segment 0
	// (DSC) to the SHADOWED real table DSC, saw the scalar ARR (not a list), and
	// raised WRONG_OBJECT_TYPE (42809) — diverging from the translator, which
	// detects the in-scope CTE source (outerSourceIsDerivedTable) and rejects with
	// UNSUPPORTED_QUERY ("unnest over a CTE/derived-table output is not yet
	// supported"). The fix makes atOnNonArraySource detect the CTE/derived binding
	// for segment 0 BEFORE the md.GetRecordType lookup and skip the base-table AT
	// check, leaving the rejection to the translator's CTE-output path. Revert-proof
	// on the converged UNSUPPORTED_QUERY (42809 on revert). RFC-142.
	t.Run("AT over a CTE source shadowing a real scalar-ARR table is UNSUPPORTED_QUERY", func(t *testing.T) {
		assertRejected(t, md, `WITH "DSC" AS (SELECT "ID" AS "ARR" FROM T1) SELECT "O" FROM "DSC", "DSC"."ARR" AS "V" AT "O"`, api.ErrCodeUnsupportedQuery)
	})

	t.Run("control: AT on a real-table scalar field (no CTE) is still WRONG_OBJECT_TYPE", func(t *testing.T) {
		// The discriminating control: a GENUINE base-table AT on a non-array field
		// (no CTE shadow) must STILL raise 42809 — proving the fix narrowed the
		// CTE-shadow skip to derived sources only, not the whole base-table check.
		// `FROM T1, T1.ID AT O` — T1 is a real table, ID a scalar field, no CTE.
		assertRejected(t, md, `SELECT "ID" FROM T1, T1."ID" AT "O"`, api.ErrCodeWrongObjectType)
	})

	t.Run("control: genuine CTE-output unnest (no AT) is still UNSUPPORTED_QUERY", func(t *testing.T) {
		// The non-AT sibling of the bug query: `WITH DSC AS (…) SELECT V FROM DSC,
		// DSC.ARR AS V` (no AT). This never reaches rejectAtOrdinalityOnTable (no AT
		// alias), so it is the translator's outerSourceIsCTE rejection directly —
		// pinning that the genuine CTE-output unnest stays UNSUPPORTED_QUERY both
		// before and after the early-pass change.
		assertRejected(t, md, `WITH "DSC" AS (SELECT "ID" AS "ARR" FROM T1) SELECT "V" FROM "DSC", "DSC"."ARR" AS "V"`, api.ErrCodeUnsupportedQuery)
	})

	// --- probe round 8: ORDER BY over a shadowed unnest binding (P2a) +
	// WHERE EXISTS over a lateral unnest (P2b) ---------------------------------

	// orderedQuery plans + executes a SELECT and returns the rows in EXECUTION
	// order (NOT sorted) so an ORDER BY can be asserted. Each row is the same
	// "k=v|k=v" string shape `query` produces.
	orderedQuery := func(t *testing.T, sql string) (string, []string) {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
		if perr != nil {
			t.Fatalf("plan %q: %v", sql, perr)
		}
		explain := plan.Explain()
		var out []string
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
				keys := make([]string, 0, len(m))
				for k := range m {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				parts := make([]string, 0, len(keys))
				for _, k := range keys {
					parts = append(parts, k+"="+unnestSprint(m[k]))
				}
				out = append(out, strings.Join(parts, "|"))
			}
			return nil, nil
		})
		if eerr != nil {
			t.Fatalf("exec %q: %v", sql, eerr)
		}
		return explain, out
	}
	// assertOrderedRows checks rows in EXECUTION order (no sort on either side).
	assertOrderedRows := func(t *testing.T, sql string, want []string) string {
		t.Helper()
		explain, got := orderedQuery(t, sql)
		if !unnestEqualStrs(got, want) {
			t.Fatalf("query %q\n got=%v\nwant=%v\nplan=%s", sql, got, want, explain)
		}
		return explain
	}

	t.Run("P2b ORDER BY a bare outer column through a multi-source unnest desc", func(t *testing.T) {
		// `SELECT C, X FROM MA, MB, MA.ARR AS X ORDER BY C DESC` — C is a BARE outer
		// column of the non-rightmost source MA. The sort key reads C off the
		// FlatMap output; without the bare-key carry-through (P2b) C is a MISSING
		// key → every row ties on a constant NULL → a NO-OP sort (rows in insertion
		// order). With the fix C reads MA's value (MA1.C=11, MA2.C=5), so DESC
		// orders the 11-rows (X∈{10,11,12}) BEFORE the 5-rows (X∈{20,21}). A no-op
		// sort would leave insertion order (MA1 rows then MA2 rows = 11s then 5s by
		// luck for DESC, but ASC below is the discriminator). Asserted in execution
		// order. RFC-142.
		assertOrderedRows(t, `SELECT "C", "X" FROM MA, MB, MA."ARR" AS "X" ORDER BY "C" DESC`, []string{
			"C=11|X=10", "C=11|X=11", "C=11|X=12",
			"C=5|X=20", "C=5|X=21",
		})
	})

	t.Run("P2b ORDER BY a bare outer column through a multi-source unnest asc", func(t *testing.T) {
		// The ASC mirror: the 5-rows (MA2: X∈{20,21}) come BEFORE the 11-rows (MA1:
		// X∈{10,11,12}). A no-op sort would leave insertion order (MA1's 11-rows
		// FIRST), which is the REVERSE of this ASC expectation — so this asc case is
		// the revert-proof one (it fails on a constant/no-op sort key). RFC-142.
		assertOrderedRows(t, `SELECT "C", "X" FROM MA, MB, MA."ARR" AS "X" ORDER BY "C" ASC`, []string{
			"C=5|X=20", "C=5|X=21",
			"C=11|X=10", "C=11|X=11", "C=11|X=12",
		})
	})

	t.Run("P2a ORDER BY on a shadowed unnest element sorts by the element asc", func(t *testing.T) {
		// probe round 8 P2a (silent-wrong order): `FROM t, t.arr AS v, u` where U
		// has a SCALAR column V=999. A bare `ORDER BY v` resolves (via the unnest's
		// Shadowing scope source) to the unnest element binding, but the unnest is
		// NOT the rightmost FROM leg — U is — so the outer NestedLoopJoin's
		// mergeRows OVERWRITES the bare `v` sort key last-leg-wins with U.V=999. The
		// PROJECTION reads the protected qualified `v.v` (the round-7 P2 fix), but
		// the SORT KEY was emitted BARE, so every row tied on the constant 999 →
		// rows in the WRONG (insertion) order. The fix qualifies a bare sort key
		// that binds to the Shadowing unnest source to `v.v` — identical to the
		// projection path — so the sort reads the element. Elements 101,201,202,203
		// ascending. RFC-142.
		assertOrderedRows(t, `SELECT "V" FROM T1, T1."ARR1" AS "V", U ORDER BY "V" ASC`, []string{
			"V=101", "V=201", "V=202", "V=203",
		})
	})

	t.Run("P2a ORDER BY on a shadowed unnest element sorts by the element desc", func(t *testing.T) {
		// The DESC mirror: the element values descend 203,202,201,101. Before the
		// fix the bare sort key read U.V=999 for every row (a constant) so the sort
		// was a no-op and the rows came back in insertion order — a silent-wrong
		// order the asc/desc pair makes revert-proof (a constant key looks the same
		// asc and desc, so the desc assertion only passes when the sort actually
		// reads the element).
		assertOrderedRows(t, `SELECT "V" FROM T1, T1."ARR1" AS "V", U ORDER BY "V" DESC`, []string{
			"V=203", "V=202", "V=201", "V=101",
		})
	})

	t.Run("P2a ORDER BY shadowed element carrying the outer id desc", func(t *testing.T) {
		// The same shape carrying the outer T1.ID through, ORDER BY the element
		// DESC. Proves the qualified sort key composes with a projected outer
		// column: each element pairs with its outer ID and U.V=999 never leaks into
		// the ordering.
		assertOrderedRows(t, `SELECT T1."ID", "V" FROM T1, T1."ARR1" AS "V", U ORDER BY "V" DESC`, []string{
			"T1.ID=2|V=203", "T1.ID=2|V=202", "T1.ID=2|V=201", "T1.ID=1|V=101",
		})
	})

	// R26 P2a (silent-wrong column/order): the SHADOWED unnest projection +
	// ORDER BY rewrites the top-level PlanVisitor applies were MISSING from the
	// CATALOG SELECT builder (buildLogicalPlanForSelectWithCTECatalog_postBuild) —
	// the path SUBQUERIES / DML / derived-table SELECTs use, distinct from the
	// PlanVisitor path. `SELECT V FROM GD, GD.ARR AS V, GW ORDER BY V DESC` INSIDE an
	// EXISTS subquery is built through that catalog path. GW has a REAL scalar column
	// V=999, and the unnest is NOT the rightmost FROM leg (GW is) — so the bare `V`
	// key the projection AND the sort key emit is OVERWRITTEN last-leg-wins by
	// GW.V=999 in mergeRows; only the qualified `V.V` key (which dotted-key-preserving
	// mergeRows keeps verbatim) reads the unnest element. Pre-fix the catalog builder
	// emitted bare `V` for both the inner projection and the inner sort, so the
	// subquery projected/sorted GW.V (the wrong column) instead of the element.
	//
	// The bug is a PLAN-CONSTRUCTION divergence in the catalog builder, so the
	// revert-proof axis is the inner plan SHAPE rendered in EXPLAIN (the inner SELECT
	// plan of an EXISTS subquery is rendered inline). With the fix the inner is
	// `Project([V.V], InMemorySort([V.V DESC], …))` (qualified element); on revert it
	// is `Project([V], InMemorySort([V DESC], …))` (the bare, GW.V-clobbered key) —
	// the EXACT string difference asserted below, on BOTH the projection and the sort.
	// (A scalar-subquery-over-unnest's value is not yet pre-evaluated in execution —
	// a separate gap — so the plan-shape assertion is the faithful proof here, exactly
	// as the column-type tests assert on the planned Value.)
	t.Run("R26 P2a catalog-builder subquery qualifies the shadowed unnest projection AND sort key", func(t *testing.T) {
		plan, perr := embedded.PlanRecordQueryWithMetadata(
			`SELECT "ID" FROM U WHERE EXISTS (SELECT "V" FROM GD, GD."ARR" AS "V", GW ORDER BY "V" DESC)`, md, nil)
		if perr != nil {
			t.Fatalf("plan: %v", perr)
		}
		explain := plan.Explain()
		// The inner projection AND the inner sort must read the QUALIFIED V.V (the
		// unnest element), not the bare V that mergeRows clobbers with GW.V.
		unnestMustContain(t, explain, "Project([V.V]")
		unnestMustContain(t, explain, "InMemorySort([V.V DESC]")
		// Pre-fix the catalog builder emitted the bare key for both — assert it is gone
		// so the test fails on revert (a bare `Project([V],` / `InMemorySort([V DESC]`).
		unnestMustNotContain(t, explain, "Project([V],")
		unnestMustNotContain(t, explain, "InMemorySort([V DESC]")
	})

	t.Run("R26 P2a catalog-builder subquery qualifies the shadowed unnest sort key ASC", func(t *testing.T) {
		// The ASC mirror pins the sort-key half independent of DESC: the catalog
		// builder's sort key is the qualified V.V for either direction.
		plan, perr := embedded.PlanRecordQueryWithMetadata(
			`SELECT "ID" FROM U WHERE EXISTS (SELECT "V" FROM GD, GD."ARR" AS "V", GW ORDER BY "V" ASC)`, md, nil)
		if perr != nil {
			t.Fatalf("plan: %v", perr)
		}
		explain := plan.Explain()
		unnestMustContain(t, explain, "InMemorySort([V.V ASC]")
		unnestMustNotContain(t, explain, "InMemorySort([V ASC]")
	})

	t.Run("R26 P2a catalog-builder subquery qualifies the shadowed unnest bare projection no ORDER BY", func(t *testing.T) {
		// The projection half in isolation (no ORDER BY): the inner SELECT projects the
		// qualified V.V, not the GW.V-clobbered bare V. Pre-fix `Project([V],`. RFC-142.
		plan, perr := embedded.PlanRecordQueryWithMetadata(
			`SELECT "ID" FROM U WHERE EXISTS (SELECT "V" FROM GD, GD."ARR" AS "V", GW)`, md, nil)
		if perr != nil {
			t.Fatalf("plan: %v", perr)
		}
		explain := plan.Explain()
		unnestMustContain(t, explain, "Project([V.V]")
		unnestMustNotContain(t, explain, "Project([V],")
	})

	t.Run("P2b WHERE EXISTS over a lateral unnest returns matching elements", func(t *testing.T) {
		// probe round 8 P2b (translation failure): `SELECT v FROM t, t.arr AS v
		// WHERE EXISTS (...)` — translateFilter routes join+EXISTS to
		// translateJoinWithExists, which translated the join's right child via
		// translateRef. When that child is a LogicalUnnest, translateRef returns nil
		// → the whole query failed with a generic Cascades translation error. The
		// fix routes the EXISTS-join path through translateUnnestJoin (the same
		// lowering the non-EXISTS path uses) so the FlatMap(outer, Explode) is built
		// and the EXISTS semi-join applies on top.
		//
		// EXISTS (SELECT 1 FROM U WHERE U.ID = T1.ID): U has only id=1, so the
		// existential holds ONLY for T1.ID=1 → only id1's single element (101)
		// survives; id2's three elements are filtered out.
		plan := assertRows(t, `SELECT "VAL" FROM T1, T1."ARR1" AS "VAL" WHERE EXISTS (SELECT 1 FROM U WHERE "U"."ID" = T1."ID")`, []string{
			"VAL=101",
		})
		unnestMustContain(t, plan, "FlatMap")
		unnestMustContain(t, plan, "Explode")
	})

	t.Run("P2b NOT EXISTS over a lateral unnest returns the complement", func(t *testing.T) {
		// The NOT-EXISTS mirror: the existential is FALSE for every outer row whose
		// id is NOT in U (only id=1 is). id2's three elements (201,202,203) survive;
		// id1's element is filtered. Proves the semi-join is a real anti-join, not a
		// silent pass-through, on top of the unnest FlatMap.
		plan := assertRows(t, `SELECT "VAL" FROM T1, T1."ARR1" AS "VAL" WHERE NOT EXISTS (SELECT 1 FROM U WHERE "U"."ID" = T1."ID")`, []string{
			"VAL=201", "VAL=202", "VAL=203",
		})
		unnestMustContain(t, plan, "FlatMap")
		unnestMustContain(t, plan, "Explode")
	})

	t.Run("P2b WHERE EXISTS over a lateral unnest with ordinality", func(t *testing.T) {
		// The EXISTS-over-unnest composition under WITH ORDINALITY: the AT-bound
		// ordinal still flows (1-based per outer row) and the existential filters
		// outer rows by id. Only T1.ID=1 satisfies EXISTS, so only its single
		// element survives with ordinal 1. Proves the unnest element binding AND the
		// ordinal both compose with the existential semi-join.
		plan := assertRows(t, `SELECT "VAL", "AT" FROM T1, T1."ARR1" AS "VAL" AT "AT" WHERE EXISTS (SELECT 1 FROM U WHERE "U"."ID" = T1."ID")`, []string{
			"AT=1|VAL=101",
		})
		unnestMustContain(t, plan, "WITH ORDINALITY")
	})

	t.Run("P2b WHERE on the element AND EXISTS both apply", func(t *testing.T) {
		// The composition that exposed the predicate-routing trap: a WHERE filter on
		// the UNNEST ELEMENT (`VAL > 100`) AND a correlated EXISTS in the same WHERE.
		// The element predicate MUST push into the inner Explode (the unnest's own
		// filter), the EXISTS MUST remain the residual semi-join on top, and neither
		// may leak onto the outer scan (where the unnest column does not exist — a
		// naive flatten pushed `VAL > 100` onto Scan(T1) and returned ZERO rows).
		// Only id1 satisfies EXISTS and 101 > 100, so exactly one element survives.
		plan := assertRows(t, `SELECT "VAL" FROM T1, T1."ARR1" AS "VAL" WHERE "VAL" > 100 AND EXISTS (SELECT 1 FROM U WHERE "U"."ID" = T1."ID")`, []string{
			"VAL=101",
		})
		// The element filter is the inner Explode's PredicatesFilter; the EXISTS is
		// the outer FlatMap's FirstOrDefault semi-join.
		unnestMustContain(t, plan, "Explode")
		unnestMustContain(t, plan, "FirstOrDefault")
	})

	t.Run("P2b WHERE on the element AND NOT EXISTS both apply", func(t *testing.T) {
		// The anti-join mirror with an element filter: `VAL > 150 AND NOT EXISTS`.
		// id1 (101) is dropped by `> 150`; id2's elements (201,202,203) all exceed
		// 150 and id2 is NOT in U so NOT EXISTS holds — all three survive. Proves the
		// element filter and the negated semi-join compose without either clobbering
		// the other.
		assertRows(t, `SELECT "VAL" FROM T1, T1."ARR1" AS "VAL" WHERE "VAL" > 150 AND NOT EXISTS (SELECT 1 FROM U WHERE "U"."ID" = T1."ID")`, []string{
			"VAL=201", "VAL=202", "VAL=203",
		})
	})

	t.Run("P2b WHERE on the ordinal AND EXISTS both apply", func(t *testing.T) {
		// The ordinal-filter + EXISTS composition: `AT = 1 AND EXISTS`. Only id1
		// satisfies EXISTS, and its single element has ordinal 1, so it survives;
		// id2's elements are dropped by EXISTS. Proves the ordinal predicate pushes
		// into the WITH-ORDINALITY Explode alongside the existential semi-join.
		plan := assertRows(t, `SELECT "VAL", "AT" FROM T1, T1."ARR1" AS "VAL" AT "AT" WHERE "AT" = 1 AND EXISTS (SELECT 1 FROM U WHERE "U"."ID" = T1."ID")`, []string{
			"AT=1|VAL=101",
		})
		unnestMustContain(t, plan, "WITH ORDINALITY")
	})

	// --- probe round 9: computed ORDER BY over an unnest column (P2a) +
	// duplicate AS==AT alias (P2b) + correlated subquery over the unnest
	// binding (P2c) ------------------------------------------------------------

	t.Run("P2a computed ORDER BY over a shadowed unnest element sorts desc", func(t *testing.T) {
		// probe round 9 P2a (silent-wrong, no-op sort): a COMPUTED ORDER BY
		// expression (`V + 0`) over a lateral-unnest element. The round-8 P2a fix
		// qualified a BARE sort key (`ORDER BY V`) via qualifyShadowedSortKeys, but a
		// COMPUTED key flows through upgradeSortKeyValues, which built its resolver
		// via buildProjectionResolverWithCTEScopes — that resolver returns nil for an
		// unnest (it resolves the dotted source `T1.ARR1` as a TABLE and fails),
		// never registering the unnest's AS/AT virtual columns. So the sort key stayed
		// raw text (`InMemorySort(["V" + 0 DESC])`), the executor compared a
		// non-existent field, and the sort was a NO-OP — rows came back in insertion
		// order. The fix falls back to buildSelectScope (the single scope builder that
		// knows the unnest virtual source) so the computed expression resolves against
		// the unnest binding and sorts for real. `FROM t, t.arr AS V, U` puts the
		// unnest before a LATER same-named U.V=999, so a no-op sort (or a sort on the
		// clobbered bare `v`=999) is detectable: with the fix the elements descend
		// 203,202,201,101. RFC-142.
		assertOrderedRows(t, `SELECT "V" FROM T1, T1."ARR1" AS "V", U ORDER BY "V" + 0 DESC`, []string{
			"V=203", "V=202", "V=201", "V=101",
		})
	})

	t.Run("P2a computed ORDER BY over a shadowed unnest element sorts asc", func(t *testing.T) {
		// The ASC mirror of the computed sort: elements ascend 101,201,202,203. The
		// asc/desc pair is revert-proof — a no-op sort (insertion order
		// 101,201,202,203) would coincidentally PASS asc, so the DESC case above is the
		// load-bearing assertion; this asc case pins that the computed key is not
		// merely reversing a constant.
		assertOrderedRows(t, `SELECT "V" FROM T1, T1."ARR1" AS "V", U ORDER BY "V" + 0 ASC`, []string{
			"V=101", "V=201", "V=202", "V=203",
		})
	})

	t.Run("P2a computed ORDER BY carrying the outer id desc", func(t *testing.T) {
		// The computed-key form carrying the outer T1.ID through, ORDER BY `V * 1`
		// DESC. Proves the qualified computed sort key composes with a projected outer
		// column and the unnest element drives the order, never U.V=999.
		assertOrderedRows(t, `SELECT T1."ID", "V" FROM T1, T1."ARR1" AS "V", U ORDER BY "V" * 1 DESC`, []string{
			"T1.ID=2|V=203", "T1.ID=2|V=202", "T1.ID=2|V=201", "T1.ID=1|V=101",
		})
	})

	t.Run("P2b duplicate AS == AT alias is rejected cleanly", func(t *testing.T) {
		// probe round 9 P2b (silent-wrong, overwrite): `FROM t, t.arr AS X AT X` —
		// the AS element alias and the AT ordinal alias are IDENTICAL. The element
		// and the ordinal are appended under the SAME bare+qualified names in
		// buildUnnestResultValue; RecordConstructorValue.Evaluate stores fields in a
		// map, so the ordinal (appended last) silently OVERWRITES the element and
		// `SELECT X` returned the ordinal, not the unnested value. The fix rejects the
		// duplicate AS==AT alias cleanly (ErrCodeDuplicateAlias), consistent with the
		// existing unnest-alias-vs-outer-alias rejection. RFC-142.
		assertRejected(t, md, `SELECT "X" FROM T1, T1."ARR1" AS "X" AT "X"`, api.ErrCodeDuplicateAlias)
	})

	t.Run("P2c correlated EXISTS over the unnest element returns matching elements", func(t *testing.T) {
		// probe round 9 P2c (translation failure): `SELECT VAL FROM t, t.arr AS VAL
		// WHERE EXISTS (SELECT 1 FROM UV WHERE UV.V = VAL)` — the inner EXISTS query
		// correlates to the UNNEST element binding VAL. The unnest's virtual Shadowing
		// source was added only to the MAIN SELECT scope; the EXISTS planner built its
		// outerScopes (buildOuterScopeSources) from REAL table sources only, so the
		// inner query could not resolve VAL → a generic Cascades translation failure.
		// The fix registers the SAME unnest virtual source into the subquery
		// outerScopes so the correlated reference resolves. UV.V ∈ {201,203} is a
		// proper subset of T1.ARR1's elements, so EXISTS holds for VAL∈{201,203} only.
		// RFC-142.
		assertRows(t, `SELECT "VAL" FROM T1, T1."ARR1" AS "VAL" WHERE EXISTS (SELECT 1 FROM UV WHERE "UV"."V" = "VAL")`, []string{
			"VAL=201", "VAL=203",
		})
	})

	t.Run("P2c correlated NOT EXISTS over the unnest element returns the complement", func(t *testing.T) {
		// The anti-join mirror: NOT EXISTS holds for the elements UV does NOT contain
		// — VAL∈{101,202}. Proves the correlated reference to the unnest element
		// resolves in the negated semi-join too, and the result is a real complement,
		// not a silent pass-through. RFC-142.
		assertRows(t, `SELECT "VAL" FROM T1, T1."ARR1" AS "VAL" WHERE NOT EXISTS (SELECT 1 FROM UV WHERE "UV"."V" = "VAL")`, []string{
			"VAL=101", "VAL=202",
		})
	})

	t.Run("P2c correlated EXISTS over the unnest element carrying the outer id", func(t *testing.T) {
		// The EXISTS-over-element form carrying the outer T1.ID through: each surviving
		// element pairs with its outer ID. id1's element 101 is dropped (101∉UV.V);
		// id2's 201 and 203 survive (∈UV.V), 202 is dropped. Pins the outer column
		// flows alongside the element-correlated existential. RFC-142.
		assertRows(t, `SELECT T1."ID", "VAL" FROM T1, T1."ARR1" AS "VAL" WHERE EXISTS (SELECT 1 FROM UV WHERE "UV"."V" = "VAL")`, []string{
			"T1.ID=2|VAL=201", "T1.ID=2|VAL=203",
		})
	})

	t.Run("P2c correlated EXISTS over the unnest element with ordinality", func(t *testing.T) {
		// The EXISTS-over-element composition under WITH ORDINALITY: the AT-bound
		// ordinal still flows (the element's ORIGINAL 1-based array position, not the
		// filtered rank) and the existential filters by element membership. 201 is
		// id2's 1st element (AT=1), 203 is id2's 3rd (AT=3). Proves the element binding
		// AND the ordinal both compose with the element-correlated semi-join. RFC-142.
		plan := assertRows(t, `SELECT "VAL", "AT" FROM T1, T1."ARR1" AS "VAL" AT "AT" WHERE EXISTS (SELECT 1 FROM UV WHERE "UV"."V" = "VAL")`, []string{
			"AT=1|VAL=201", "AT=3|VAL=203",
		})
		unnestMustContain(t, plan, "WITH ORDINALITY")
	})

	// --- probe round 12: EXISTS over a lateral unnest correlating to the ORIGINAL
	// OUTER TABLE (P2a) ---------------------------------------------------------

	t.Run("R12 P2a EXISTS over unnest correlating to the OUTER TABLE returns matching elements", func(t *testing.T) {
		// probe round 12 P2a (silent-wrong, drops rows): `SELECT VAL FROM T1,
		// T1.ARR AS VAL WHERE EXISTS (SELECT 1 FROM U WHERE U.V > T1.ID)` — the EXISTS
		// subquery's residual correlation is to the ORIGINAL outer table T1 (via
		// T1.ID), NOT the unnest element/ordinal. buildCorrelatedExists resolved
		// T1.ID against the outer table source T1, so the residual carried
		// QOV(T1).ID. But the existential's outer is the UNNEST FlatMap, whose merged
		// output is bound under the unnest alias VAL, not T1 → QOV(T1) was UNBOUND at
		// execution → `U.V > NULL` was false for every row → ALL rows silently
		// dropped (got=[]). The fix rebases the residual's outer-table-leg reference
		// (T1.ID) to read the qualified T1.ID key off the unnest FlatMap's merged
		// binding, exactly as a real-JOIN+EXISTS does (rebaseOuterLegRefsToMerged).
		//
		// U has one row V=999. EXISTS (SELECT 1 FROM U WHERE U.V > T1.ID): 999>1 for
		// id1 and 999>2 for id2 → both hold → every element of id1 (101) and id2
		// (201,202,203) survives. id0/id3 have no/NULL array → 0 elements. Before the
		// fix this returned ZERO rows. RFC-142.
		plan := assertRows(t, `SELECT "VAL" FROM T1, T1."ARR1" AS "VAL" WHERE EXISTS (SELECT 1 FROM U WHERE "U"."V" > T1."ID")`, []string{
			"VAL=101", "VAL=201", "VAL=202", "VAL=203",
		})
		unnestMustContain(t, plan, "FlatMap")
		unnestMustContain(t, plan, "Explode")
	})

	t.Run("R12 P2a NOT EXISTS over unnest correlating to the OUTER TABLE is the complement", func(t *testing.T) {
		// The anti-join mirror. U.V=999, so `U.V > T1.ID` (999 > id) holds for EVERY
		// id, making NOT EXISTS FALSE for every outer row → ZERO elements survive.
		// Proves the outer-table-correlated residual drives the negated semi-join too
		// (a silent pass-through would wrongly return all elements). RFC-142.
		assertRows(t, `SELECT "VAL" FROM T1, T1."ARR1" AS "VAL" WHERE NOT EXISTS (SELECT 1 FROM U WHERE "U"."V" > T1."ID")`, nil)
	})

	t.Run("R12 P2a EXISTS over unnest correlating to the outer table that filters some rows", func(t *testing.T) {
		// A discriminating threshold: `U.V > T1.ID + 998` ⇒ 999 > id + 998 ⇒ id < 1.
		// Only id0 satisfies it, but id0's array is empty (0 elements). id1/id2 fail
		// the residual → their elements are dropped. So the result is EMPTY — the
		// outer-table residual genuinely filters, it is not a degenerate always-true.
		assertRows(t, `SELECT "VAL" FROM T1, T1."ARR1" AS "VAL" WHERE EXISTS (SELECT 1 FROM U WHERE "U"."V" > T1."ID" + 998)`, nil)
	})

	t.Run("R12 P2a EXISTS over unnest correlating to BOTH the outer table AND the element", func(t *testing.T) {
		// The combined correlation: the EXISTS subquery's inner predicate references
		// BOTH the original outer table (T1.ID — the rebased outer-leg residual) AND
		// the unnest ELEMENT (VAL — bound by the FlatMap, the round-9 P2c path), each
		// compared against the inner column U.V in the SAME existential WHERE:
		//   EXISTS (SELECT 1 FROM U WHERE U.V > VAL AND U.V > T1.ID)
		// U.V=999, so `999 > VAL` holds for every element (max 203) AND `999 > T1.ID`
		// holds for every id≥1 → every element of id1,id2 survives. This is
		// revert-proof on BOTH correlations: if the T1.ID residual broke (QOV(T1)
		// unbound) `999 > NULL` is false → all dropped; if the VAL element reference
		// broke `999 > <merged-row>` is NULL → all dropped. Only with BOTH bound do
		// the four elements survive. RFC-142.
		assertRows(t, `SELECT "VAL" FROM T1, T1."ARR1" AS "VAL" WHERE EXISTS (SELECT 1 FROM U WHERE "U"."V" > "VAL" AND "U"."V" > T1."ID")`, []string{
			"VAL=101", "VAL=201", "VAL=202", "VAL=203",
		})
	})

	t.Run("R12 P2a EXISTS over unnest correlating to BOTH outer table and element filters", func(t *testing.T) {
		// The discriminating BOTH-correlation form: the existential narrows the
		// element set. UV has rows (v=201),(v=203). `EXISTS (SELECT 1 FROM UV WHERE
		// UV.V = VAL AND UV.V > T1.ID)`: UV.V = VAL matches the element (201/203);
		// UV.V > T1.ID is 201/203 > id (always true for id1,id2). So only the
		// elements present in UV (201, 203) survive — 101 and 202 are dropped (∉UV.V).
		// Pins that the element equality AND the outer-table comparison BOTH filter in
		// the same residual: dropping the T1.ID rebase would still admit 201/203 (the
		// comparison degenerates), but dropping the element binding would drop
		// everything — the exact {201,203} set proves both correlations live. RFC-142.
		assertRows(t, `SELECT "VAL" FROM T1, T1."ARR1" AS "VAL" WHERE EXISTS (SELECT 1 FROM UV WHERE "UV"."V" = "VAL" AND "UV"."V" > T1."ID")`, []string{
			"VAL=201", "VAL=203",
		})
	})

	t.Run("R12 P2a EXISTS over unnest with ordinality correlating to the outer table", func(t *testing.T) {
		// The outer-table-correlated EXISTS under WITH ORDINALITY: the AT-bound
		// ordinal still flows (1-based per outer row) while the residual correlates
		// to T1.ID. Every id1/id2 element survives (999 > id always holds), each
		// carrying its original 1-based ordinal. Proves the outer-leg rebase composes
		// with the ordinality 2-field record. RFC-142.
		plan := assertRows(t, `SELECT "VAL", "AT" FROM T1, T1."ARR1" AS "VAL" AT "AT" WHERE EXISTS (SELECT 1 FROM U WHERE "U"."V" > T1."ID")`, []string{
			"AT=1|VAL=101", "AT=1|VAL=201", "AT=2|VAL=202", "AT=3|VAL=203",
		})
		unnestMustContain(t, plan, "WITH ORDINALITY")
	})

	// --- probe round 13: EXISTS over a lateral unnest that is NOT the rightmost
	// FROM item — the BURIED-UNNEST class. The round-12 fix handled the unnest as
	// the rightmost leg (routes through translateUnnestExistsFilter). When ANOTHER
	// comma source follows the unnest (`FROM T1, T1.arr AS V, U`), the TOP-LEVEL
	// join's right child is that source (U), so the filter routes to the GENERIC
	// join+EXISTS path. The outer of the existential FlatMap is then the inner-join
	// NestedLoopJoin(unnest-FlatMap, Scan(U)), whose MERGED row anchors the original
	// outer table T1's columns only as verbatim "T1.COL" keys (T1 is buried under
	// the unnest leg, whose row flows under the unnest alias V). The NLJ rule's
	// existential-residual rebase covered only the TWO top-level leg aliases
	// {V, U}, leaving the buried `QOV(T1).ID` residual unrebased → QOV(T1) UNBOUND
	// below the FlatMap → T1.ID evaluates NULL → every matching unnested row
	// silently DROPPED (got=[]). The root fix collects the COMPLETE outer-leg alias
	// set (every alias the merged row anchors columns for, derived from the anchored
	// result value's dotted field-name prefixes — {T1, V, U}) and feeds it to the
	// SAME rebase, so the buried T1.ID residual reads the merged row's verbatim
	// "T1.ID" key. RFC-142.

	t.Run("R13 EXISTS over a NON-rightmost unnest correlating to the buried outer table", func(t *testing.T) {
		// `FROM T1, T1.ARR1 AS V, U WHERE EXISTS (SELECT 1 FROM UV WHERE UV.ID =
		// T1.ID)` — the unnest is NOT the rightmost FROM leg (U is). The residual
		// correlates to the BURIED original table T1. UV ids are {1,2}; T1 ids with
		// elements are {1,2} — both satisfy EXISTS, so ALL four elements survive
		// (U has one row, a clean 1× cross). Before the fix: EMPTY (T1.ID unbound).
		// RFC-142.
		plan := assertRows(t, `SELECT "V" FROM T1, T1."ARR1" AS "V", U WHERE EXISTS (SELECT 1 FROM UV WHERE "UV"."ID" = T1."ID")`, []string{
			"V=101", "V=201", "V=202", "V=203",
		})
		// The buried unnest stays its own FlatMap-over-Explode; the merged-row
		// inner join feeds the existential FlatMap.
		unnestMustContain(t, plan, "FlatMap")
		unnestMustContain(t, plan, "Explode")
		unnestMustContain(t, plan, "NestedLoopJoin")
	})

	t.Run("R13 EXISTS over a NON-rightmost unnest that filters by the buried outer table", func(t *testing.T) {
		// A buried-outer-table residual that genuinely DISCRIMINATES: `UV.ID =
		// T1.ID + 1`. UV ids {1,2}; T1.ID+1 ∈ {2,3} for ids {1,2}. UV has id=2 → the
		// element from T1.ID=1 (101: needs UV.ID=2 → present → TRUE) and from
		// T1.ID=2 (201,202,203: needs UV.ID=3 → absent → FALSE). So only 101
		// survives — proving the rebased T1.ID is the REAL outer id, not a constant.
		// Before the fix: EMPTY. RFC-142.
		assertRows(t, `SELECT "V" FROM T1, T1."ARR1" AS "V", U WHERE EXISTS (SELECT 1 FROM UV WHERE "UV"."ID" = T1."ID" + 1)`, []string{
			"V=101",
		})
	})

	t.Run("R13 NOT EXISTS over a NON-rightmost unnest correlating to the buried outer table", func(t *testing.T) {
		// The anti-join mirror: T1 ids {1,2} both appear in UV, so EXISTS is TRUE for
		// every element → NOT EXISTS is FALSE for all → ZERO rows. Proves the rebase
		// composes with the negated semi-join (a pre-fix unbound T1.ID would have made
		// NOT EXISTS admit ALL rows — the opposite failure). RFC-142.
		assertRows(t, `SELECT "V" FROM T1, T1."ARR1" AS "V", U WHERE NOT EXISTS (SELECT 1 FROM UV WHERE "UV"."ID" = T1."ID")`, nil)
	})

	t.Run("R13 EXISTS over a NON-rightmost unnest correlating to the unnest ELEMENT", func(t *testing.T) {
		// The element-correlation variant over the buried unnest: the residual
		// correlates to the unnest ELEMENT VAL (DISTINCT from UV's column V so VAL is
		// unambiguously the outer correlation). UV.V ∈ {201,203}, so EXISTS holds for
		// VAL ∈ {201,203} only — 101 and 202 are dropped. The element binding (V's
		// merged-row key) must survive the buried-leg rebase too. RFC-142.
		assertRows(t, `SELECT "VAL" FROM T1, T1."ARR1" AS "VAL", U WHERE EXISTS (SELECT 1 FROM UV WHERE "UV"."V" = "VAL")`, []string{
			"VAL=201", "VAL=203",
		})
	})

	t.Run("R13 EXISTS over a NON-rightmost unnest correlating to BOTH buried table and element", func(t *testing.T) {
		// Both correlations in one residual: `UV.V = VAL AND UV.ID = T1.ID`. The
		// element VAL (merged-row key) AND the buried outer T1.ID must BOTH resolve.
		// VAL=203 is at T1.ID=2; UV row (id=2,v=203) → UV.V=203=VAL AND UV.ID=2=T1.ID
		// → TRUE. VAL=201 is at T1.ID=2; UV row (id=1,v=201) → UV.ID=1≠2 → FALSE. So
		// only 203. Proves the element key and the buried-table rebase compose.
		// RFC-142.
		assertRows(t, `SELECT "VAL" FROM T1, T1."ARR1" AS "VAL", U WHERE EXISTS (SELECT 1 FROM UV WHERE "UV"."V" = "VAL" AND "UV"."ID" = T1."ID")`, []string{
			"VAL=203",
		})
	})

	t.Run("R13 WHERE on the buried outer table through a NON-rightmost unnest", func(t *testing.T) {
		// The plain WHERE (no EXISTS) on the BURIED original table T1 through a
		// non-rightmost unnest: `WHERE T1.ID = 2` keeps id2's three elements. The
		// buried T1.ID reference must resolve against the merged outer row's verbatim
		// "T1.ID" key, exactly as the EXISTS residual does. RFC-142.
		assertRows(t, `SELECT "V" FROM T1, T1."ARR1" AS "V", U WHERE T1."ID" = 2`, []string{
			"V=201", "V=202", "V=203",
		})
	})

	t.Run("R13 EXISTS over a NON-rightmost unnest with ordinality correlating to buried table", func(t *testing.T) {
		// The buried-table-correlated EXISTS under WITH ORDINALITY: the AT-bound
		// 1-based ordinal still flows per outer row while the residual correlates to
		// the buried T1.ID. T1 ids {1,2} both in UV → every element survives, each
		// carrying its original 1-based ordinal. RFC-142.
		plan := assertRows(t, `SELECT "V", "AT" FROM T1, T1."ARR1" AS "V" AT "AT", U WHERE EXISTS (SELECT 1 FROM UV WHERE "UV"."ID" = T1."ID")`, []string{
			"AT=1|V=101", "AT=1|V=201", "AT=2|V=202", "AT=3|V=203",
		})
		unnestMustContain(t, plan, "WITH ORDINALITY")
	})

	t.Run("R13 control: a non-rightmost unnest cross join (no EXISTS, no WHERE) is unaffected", func(t *testing.T) {
		// Control: the SAME `FROM T1, T1.arr AS V, U` shape with NO EXISTS and NO
		// WHERE plans and returns the full cross product of each unnested element with
		// U's single row (U has one row, so 1× each). Proves the buried-alias rebase
		// only fires inside the join+EXISTS / WHERE path and never perturbs the plain
		// cross. RFC-142.
		assertRows(t, `SELECT "V" FROM T1, T1."ARR1" AS "V", U`, []string{
			"V=101", "V=201", "V=202", "V=203",
		})
	})

	// --- probe round 17: a BURIED (non-rightmost) lateral-unnest element/ordinal
	// WHERE conjunct COMBINED with EXISTS ------------------------------------------
	//
	// `FROM T1, T1.ARR1 AS V, U WHERE V > x AND EXISTS (…)` — the unnest is NOT the
	// rightmost FROM item (U is), so the TOP-LEVEL LogicalJoin's Right is U and the
	// unnest is BURIED in join.Left. The round-16 fix (pushBuriedUnnestPredicateDown)
	// folds `V > x` into the inner Explode — but it ran ONLY when no EXISTS was
	// present. With an EXISTS in the same WHERE, the EXISTS early-return fired FIRST
	// and routed the WHOLE filter through the generic join+EXISTS path
	// (translateJoinWithExists), where the buried-unnest conjunct (`V > x`) was
	// appended as a top-level join predicate. The Cascades planner then (for the
	// query shapes pinned below) pushes that predicate onto the unnest's OUTER scan
	// (`PredicatesFilter(Scan(T1), …)`) where QOV(V) is UNBOUND → `V > x` evaluates
	// NULL → every matching unnested row silently DROPPED (got=[]). The fix runs the
	// buried-unnest push BEFORE the EXISTS dispatch: the NON-EXISTS buried conjuncts
	// (V > x) are folded into the inner Explode (`PredicatesFilter(Explode …)`,
	// Java's `EXPLODE … | FILTER …`) FIRST, and the EXISTS subqueries + their
	// existential markers are preserved in the residual outer filter so the semi-join
	// still applies. After the fix BOTH the element/ordinal filter AND the EXISTS
	// hold, deterministically. The query forms below are chosen so the PRE-fix
	// planner reliably pushes the buried filter onto the outer scan (each fails
	// got=[] on revert — proven revert-proof). RFC-142.

	t.Run("R17 buried unnest element filter AND EXISTS both apply", func(t *testing.T) {
		// `FROM T1, T1.ARR1 AS V, U WHERE V > 150 AND EXISTS (SELECT 1 FROM UV WHERE
		// UV.ID = T1.ID)`. EXISTS holds for T1.ID ∈ {1,2} (UV ids {1,2}). `V > 150`
		// keeps {201,202,203} (101 dropped). id2's three elements all exceed 150 AND
		// id2 satisfies EXISTS → all three survive; id1's 101 is dropped by V>150.
		// Before the fix the pre-fix planner pushes `V > 150` onto the unnest's outer
		// Scan(T1) where V is unbound → EMPTY. RFC-142.
		plan := assertRows(t, `SELECT "V" FROM T1, T1."ARR1" AS "V", U WHERE "V" > 150 AND EXISTS (SELECT 1 FROM UV WHERE "UV"."ID" = T1."ID")`, []string{
			"V=201", "V=202", "V=203",
		})
		// The buried element filter is folded into the inner Explode's
		// PredicatesFilter; the EXISTS remains the outer semi-join.
		unnestMustContain(t, plan, "FlatMap")
		unnestMustContain(t, plan, "Explode")
		unnestMustContain(t, plan, "PredicatesFilter")
		unnestMustContain(t, plan, "NestedLoopJoin")
	})

	t.Run("R17 buried unnest element filter AND EXISTS carrying the buried outer id", func(t *testing.T) {
		// The same shape carrying the BURIED outer T1.ID through alongside the element.
		// Each surviving element (201,202,203 from id2) pairs with T1.ID=2, proving the
		// buried-table column resolves AND the element filter pushed. Before the fix:
		// EMPTY. RFC-142.
		assertRows(t, `SELECT T1."ID", "V" FROM T1, T1."ARR1" AS "V", U WHERE "V" > 150 AND EXISTS (SELECT 1 FROM UV WHERE "UV"."ID" = T1."ID")`, []string{
			"T1.ID=2|V=201", "T1.ID=2|V=202", "T1.ID=2|V=203",
		})
	})

	t.Run("R17 buried unnest element equality AND EXISTS", func(t *testing.T) {
		// An exact-equality buried element filter: `V = 202` keeps only that element
		// from id2; id2 ∈ UV → EXISTS holds; 101 (id1) and the other id2 elements are
		// dropped. So exactly one row survives. The pre-fix planner pushes `V = 202`
		// onto the outer Scan(T1) → EMPTY on revert. RFC-142.
		assertRows(t, `SELECT "V" FROM T1, T1."ARR1" AS "V", U WHERE "V" = 202 AND EXISTS (SELECT 1 FROM UV WHERE "UV"."ID" = T1."ID")`, []string{
			"V=202",
		})
	})

	t.Run("R17 buried unnest ordinal filter AND EXISTS both apply", func(t *testing.T) {
		// The ORDINAL-filter variant: `... AS V AT O, U WHERE O > 1 AND EXISTS (…)`.
		// EXISTS holds for T1.ID ∈ {1,2}. `O > 1` keeps elements at their ORIGINAL
		// 1-based position > 1: id1 has only O=1 (101, dropped); id2 has O=2 (202),
		// O=3 (203). Both kept rows are from id2 which satisfies EXISTS. The pre-fix
		// planner pushes `O > 1` onto the outer Scan(T1) where O is unbound → EMPTY on
		// revert. RFC-142.
		plan := assertRows(t, `SELECT "V", "O" FROM T1, T1."ARR1" AS "V" AT "O", U WHERE "O" > 1 AND EXISTS (SELECT 1 FROM UV WHERE "UV"."ID" = T1."ID")`, []string{
			"O=2|V=202", "O=3|V=203",
		})
		unnestMustContain(t, plan, "WITH ORDINALITY")
		unnestMustContain(t, plan, "PredicatesFilter")
	})

	t.Run("R17 buried unnest element filter AND NOT EXISTS both apply", func(t *testing.T) {
		// The NOT-EXISTS mirror with a buried element filter: `V > 150 AND NOT EXISTS
		// (SELECT 1 FROM UV WHERE UV.ID = T1.ID + 1)`. UV ids {1,2}; T1.ID+1 ∈ {2,3}
		// for ids {1,2}. For id1: UV.ID=2 present → EXISTS true → NOT EXISTS FALSE. For
		// id2: UV.ID=3 absent → EXISTS false → NOT EXISTS TRUE. So only id2 passes the
		// anti-join. `V > 150` over id2's {201,202,203} keeps all three. Both axes
		// discriminate. Before the fix the element filter on the unbound outer Scan(T1)
		// dropped every row → EMPTY (masking the NOT-EXISTS entirely). RFC-142.
		plan := assertRows(t, `SELECT "V" FROM T1, T1."ARR1" AS "V", U WHERE "V" > 150 AND NOT EXISTS (SELECT 1 FROM UV WHERE "UV"."ID" = T1."ID" + 1)`, []string{
			"V=201", "V=202", "V=203",
		})
		unnestMustContain(t, plan, "PredicatesFilter")
	})

	t.Run("R17 buried unnest ordinal filter AND NOT EXISTS both apply", func(t *testing.T) {
		// The ordinal + NOT-EXISTS composition: `O > 1 AND NOT EXISTS (… UV.ID =
		// T1.ID + 1)`. NOT EXISTS holds for id2 only (as above). `O > 1` over id2 keeps
		// O=2 (202) and O=3 (203). Proves the ordinal push composes with the negated
		// semi-join. The pre-fix planner pushes `O > 1` onto the outer Scan(T1) →
		// EMPTY on revert. RFC-142.
		plan := assertRows(t, `SELECT "V", "O" FROM T1, T1."ARR1" AS "V" AT "O", U WHERE "O" > 1 AND NOT EXISTS (SELECT 1 FROM UV WHERE "UV"."ID" = T1."ID" + 1)`, []string{
			"O=2|V=202", "O=3|V=203",
		})
		unnestMustContain(t, plan, "WITH ORDINALITY")
		unnestMustContain(t, plan, "PredicatesFilter")
	})

	t.Run("R17 control: buried unnest EXISTS with no element filter is unchanged", func(t *testing.T) {
		// Control: the SAME buried-unnest+EXISTS shape WITHOUT an element filter — the
		// round-13 path. EXISTS holds for T1.ID ∈ {1,2}, so ALL four elements survive.
		// Proves the round-17 pre-push leaves the no-element-filter EXISTS case (the
		// existing round-13 behavior) exactly as it was. RFC-142.
		assertRows(t, `SELECT "V" FROM T1, T1."ARR1" AS "V", U WHERE EXISTS (SELECT 1 FROM UV WHERE "UV"."ID" = T1."ID")`, []string{
			"V=101", "V=201", "V=202", "V=203",
		})
	})

	// --- probe round 11: schema-qualified table inside a SUBQUERY (P2) +
	// invalid array source after a prior unnest (P3) --------------------------

	t.Run("P2 schema-qualified table inside an EXISTS subquery is a cross join not unnest", func(t *testing.T) {
		// probe round 11 P2 (valid-query-fails): `SELECT ID FROM T1 WHERE EXISTS
		// (SELECT 1 FROM PA AS s, s.PB AS B ...)` with session schema `s`. Inside the
		// EXISTS subquery, the prior source PA is aliased `s`, which ALSO equals the
		// session schema name, so `s.PB` is BOTH "field PB on source s" AND the
		// schema-qualified TABLE PB. Java's generateAccess resolves the TABLE first
		// at EVERY FROM-source resolution point — including inside a subquery — so
		// `s.PB` is the real table PB, a normal cross join, never a correlated unnest
		// of the missing field PB on source s.
		//
		// The parser-side classifier has no metadata, so it tentatively emits a
		// LogicalUnnest for `s.PB`; the table-first demotion (demoteSchemaQualifiedUnnest)
		// rewrites it to a Scan once metadata is in scope. Before the fix that demotion
		// walked only the TOP-LEVEL operator tree and never descended into the EXISTS
		// subquery plan stored on LogicalFilter.ExistsSubqueries, so the surviving
		// LogicalUnnest mis-translated and the query failed with "column PB missing on
		// source s". Now the demotion reaches the subquery plan and the cross join plans.
		//
		// The EXISTS subquery is non-correlated and PA(1)×PB(1) yields one row, so
		// EXISTS is TRUE for every T1 row → all four T1 ids (0,1,2,3) survive.
		plan := assertRows(t, `SELECT "ID" FROM T1 WHERE EXISTS (SELECT 1 FROM PA AS "s", "s"."PB" AS "B")`, []string{
			"ID=0", "ID=1", "ID=2", "ID=3",
		})
		// The s.PB source inside the subquery is a real cross-join table — NO unnest
		// Explode anywhere. (The EXISTS itself lowers to a semi-join FlatMap wrapper
		// around the subquery; that FlatMap is the existential, NOT an unnest, so the
		// revert-proof marker is the ABSENCE of Explode plus the PRESENCE of the
		// subquery's NestedLoopJoin cross join of PA × PB.)
		unnestMustNotContain(t, plan, "Explode")
		unnestMustContain(t, plan, "NestedLoopJoin")
	})

	t.Run("P2 schema-qualified table inside an EXISTS subquery with a filter", func(t *testing.T) {
		// The same shape with a real WHERE inside the subquery, pinning that the
		// demoted cross-join table participates in the subquery's own predicate. The
		// subquery's `B.ID = 1` matches the single PB row, so EXISTS stays TRUE for
		// every T1 id. Proves the demotion-in-subquery resolves B as a real table
		// whose column `ID` is a scalar (a correlated-unnest mis-classification would
		// instead fail to resolve `B.ID` here). RFC-142.
		plan := assertRows(t, `SELECT "ID" FROM T1 WHERE EXISTS (SELECT 1 FROM PA AS "s", "s"."PB" AS "B" WHERE "B"."ID" = 1)`, []string{
			"ID=0", "ID=1", "ID=2", "ID=3",
		})
		// Same proof: the subquery's s.PB is a real table (cross join, B.ID filter
		// pushed into PB's scan), NOT an unnest Explode.
		unnestMustNotContain(t, plan, "Explode")
		unnestMustContain(t, plan, "NestedLoopJoin")
	})

	t.Run("P2 schema-qualified table inside an EXISTS subquery with no match drops all rows", func(t *testing.T) {
		// Control: the subquery is genuinely evaluated as a cross join, not silently
		// always-true. `B.ID = 9` matches NO PB row (PB has only id=1), so the EXISTS
		// is FALSE for every T1 row → the outer query returns ZERO rows. If the
		// subquery had failed to plan (the pre-fix behavior) this would error, not
		// return an empty result; if it were a degenerate always-true it would return
		// all rows. An empty result proves the cross join + filter executed. RFC-142.
		assertRows(t, `SELECT "ID" FROM T1 WHERE EXISTS (SELECT 1 FROM PA AS "s", "s"."PB" AS "B" WHERE "B"."ID" = 9)`, nil)
	})

	t.Run("P2 control: plain schema-qualified table inside an EXISTS subquery plans", func(t *testing.T) {
		// Sibling control (no alias collision): `… EXISTS (SELECT 1 FROM PA, s.PB AS B)`.
		// Here `s.PB` is NOT classified as an unnest at all (segment 0 `s` is the
		// session schema, not a visible alias) — it is a plain schema-qualified table
		// scan `Scan("S.PB")`. The schema-qualifier stripping (resolveQualifiedTableNames
		// + the scope-side resolveScopeTable) must reach the SUBQUERY plan too, else
		// the unresolved `S.PB` scan fails translation. Plans identically to the
		// alias-collision form: PA(1)×PB(1) makes EXISTS true for every T1 id. RFC-142.
		plan := assertRows(t, `SELECT "ID" FROM T1 WHERE EXISTS (SELECT 1 FROM PA, s."PB" AS "B")`, []string{
			"ID=0", "ID=1", "ID=2", "ID=3",
		})
		unnestMustNotContain(t, plan, "Explode")
		unnestMustContain(t, plan, "NestedLoopJoin")
	})

	t.Run("P3 AT-on-non-array after a prior unnest is WRONG_OBJECT_TYPE not multiple-unnest", func(t *testing.T) {
		// probe round 11 P3 (wrong error code): `FROM T1, T1.ARR1 AS V, U AT O` — the
		// FROM list has ONE real array unnest (T1.ARR1 AS V) followed by an AT on the
		// NON-ARRAY table U. The multiple-unnest guard (containsLateralUnnest(j.Left))
		// used to fire BEFORE the right-side array validation, so this wrongly reported
		// "multiple lateral array unnests" (UNSUPPORTED_QUERY) even though the second
		// source is not a valid array unnest at all. The guard now runs only AFTER the
		// right side is confirmed a valid array unnest, so the faithful AT-on-a-table
		// rejection (WRONG_OBJECT_TYPE, Java's "AT clause requires an array column")
		// fires first. RFC-142.
		assertRejected(t, md, `SELECT "ID", "V" FROM T1, T1."ARR1" AS "V", U AT "O"`, api.ErrCodeWrongObjectType)
	})

	t.Run("P3 scalar source after a prior unnest is WRONG_OBJECT_TYPE not multiple-unnest", func(t *testing.T) {
		// The scalar-field variant: `FROM T1, T1.ARR1 AS V, T1.ID AS X` — after the
		// real unnest T1.ARR1, the second dotted source T1.ID is a PRESENT SCALAR
		// (not an array). Java's generateCorrelatedFieldAccess asserts the field is
		// repeated → WRONG_OBJECT_TYPE. The array validation must fire BEFORE the
		// multiple-unnest guard, so this is WRONG_OBJECT_TYPE, not the (misleading)
		// multiple-unnest UNSUPPORTED_QUERY. RFC-142.
		assertRejected(t, md, `SELECT "ID", "V" FROM T1, T1."ARR1" AS "V", T1."ID" AS "X"`, api.ErrCodeWrongObjectType)
	})

	t.Run("P3 control: a genuine second array unnest is still UNSUPPORTED_QUERY", func(t *testing.T) {
		// Control: the multiple-unnest guard is preserved for an ACTUAL second array
		// unnest. `FROM T1, T1.ARR1 AS V, T1.ARR1_NN AS W` is two valid array unnests
		// in one FROM scope — chained multi-unnest, not yet supported (nested-FlatMap
		// merged-row threading). It must still reject with UNSUPPORTED_QUERY (the guard
		// fires AFTER the second source is confirmed a valid array unnest). Proves the
		// reorder did not weaken the multiple-unnest rejection — only let the
		// invalid-second-source cases reach the array-validation error first. RFC-142.
		assertRejected(t, md, `SELECT "ID", "V", "W" FROM T1, T1."ARR1" AS "V", T1."ARR1_NN" AS "W"`, api.ErrCodeUnsupportedQuery)
	})

	// --- probe round 12: schema-qualified table inside a subquery resolved
	// against a NON-DEFAULT session schema (P2b) -------------------------------

	// querySchema plans + executes a SELECT under a NON-DEFAULT session schema (the
	// real CONNECT-schema session path: NewPlanVisitorWithSchema + the
	// schema-qualified-table demotion threaded with the active schema), returning
	// the explain + sorted "k=v" rows. The default-schema query() helper above
	// always uses "s"; this one drives the threading under e.g. "main".
	querySchema := func(t *testing.T, sql, schemaName string) (string, []string) {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadataSchema(sql, md, schemaName, nil)
		if perr != nil {
			t.Fatalf("plan %q (schema %s): %v", sql, schemaName, perr)
		}
		explain := plan.Explain()
		var out []string
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
				keys := make([]string, 0, len(m))
				for k := range m {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				parts := make([]string, 0, len(keys))
				for _, k := range keys {
					parts = append(parts, k+"="+unnestSprint(m[k]))
				}
				out = append(out, strings.Join(parts, "|"))
			}
			return nil, nil
		})
		if eerr != nil {
			t.Fatalf("exec %q (schema %s): %v", sql, schemaName, eerr)
		}
		sort.Strings(out)
		return explain, out
	}

	assertRowsSchema := func(t *testing.T, sql, schemaName string, want []string) string {
		t.Helper()
		explain, got := querySchema(t, sql, schemaName)
		sort.Strings(want)
		if !unnestEqualStrs(got, want) {
			t.Fatalf("query %q (schema %s)\n got=%v\nwant=%v\nplan=%s", sql, schemaName, got, want, explain)
		}
		return explain
	}

	t.Run("R12 P2b schema-qualified table inside a subquery resolves against the ACTIVE schema", func(t *testing.T) {
		// probe round 12 P2b (misclassification in a non-default schema): the round-11
		// fix demoted a schema-qualified-table unnest INSIDE an EXISTS subquery, but
		// only against the HARDCODED default schema "s" — the subquery planner
		// (existsSubqueryPlanner) built the subquery plan through
		// buildLogicalPlanForQueryWithCTECatalog → buildLogicalPlanForSelectWithCTECatalog,
		// which fell back to defaultEmbeddedSchema. So in a session whose schema is
		// NOT "s" (here `main`), `main.PB` inside the subquery — segment 0 (`main`)
		// equals the ACTIVE schema, so it IS the schema-qualified table PB — was
		// demoted/normalized against "s" instead of `main` and stayed a correlated
		// LogicalUnnest → mis-translation ("column PB missing on source main" /
		// Cascades translation failed). The fix threads v.schemaName through the
		// subquery planner and its recursive catalog builders, so the demotion uses
		// the ACTIVE schema. `main.PB` plans as the real table PB (cross join), exactly
		// as the default-schema sibling (the round-11 `s` test). EXISTS is
		// non-correlated, PA(1)×PB(1) → one row → TRUE for every T1 id. RFC-142.
		plan := assertRowsSchema(t, `SELECT "ID" FROM T1 WHERE EXISTS (SELECT 1 FROM PA AS "main", "main"."PB" AS "B")`, "main", []string{
			"ID=0", "ID=1", "ID=2", "ID=3",
		})
		// The main.PB source inside the subquery is a real cross-join table — NO
		// unnest Explode. The EXISTS lowers to a semi-join FlatMap; the revert-proof
		// marker is the ABSENCE of Explode + the PRESENCE of the PA×PB NestedLoopJoin.
		unnestMustNotContain(t, plan, "Explode")
		unnestMustContain(t, plan, "NestedLoopJoin")
	})

	t.Run("R12 P2b schema-qualified table inside a subquery with a filter under the active schema", func(t *testing.T) {
		// The same shape with a real WHERE inside the subquery: `B.ID = 1` matches the
		// single PB row, so EXISTS stays TRUE for every T1 id. Proves the demoted
		// cross-join table B (resolved against the ACTIVE schema `main`) participates
		// in the subquery's own predicate — a correlated-unnest mis-classification
		// would instead fail to resolve `B.ID`. RFC-142.
		plan := assertRowsSchema(t, `SELECT "ID" FROM T1 WHERE EXISTS (SELECT 1 FROM PA AS "main", "main"."PB" AS "B" WHERE "B"."ID" = 1)`, "main", []string{
			"ID=0", "ID=1", "ID=2", "ID=3",
		})
		unnestMustNotContain(t, plan, "Explode")
		unnestMustContain(t, plan, "NestedLoopJoin")
	})

	t.Run("R12 P2b schema-qualified table inside a subquery with no match drops all rows under the active schema", func(t *testing.T) {
		// Control: the subquery is genuinely a cross join evaluated under `main`, not
		// silently always-true. `B.ID = 9` matches NO PB row (PB has only id=1), so
		// EXISTS is FALSE for every T1 row → ZERO rows. A pre-fix mis-classified
		// subquery would error (translation failure), not return an empty result; a
		// degenerate always-true would return all rows. RFC-142.
		assertRowsSchema(t, `SELECT "ID" FROM T1 WHERE EXISTS (SELECT 1 FROM PA AS "main", "main"."PB" AS "B" WHERE "B"."ID" = 9)`, "main", nil)
	})

	t.Run("R12 P2b control: plain schema-qualified table inside a subquery under the active schema", func(t *testing.T) {
		// The no-alias-collision sibling: `… EXISTS (SELECT 1 FROM PA, main.PB AS B)`.
		// `main.PB` is a plain schema-qualified table scan (segment 0 `main` is the
		// session schema, not a visible alias). The schema-qualifier stripping
		// (resolveQualifiedTableNames + normalizeSchemaQualifiedSelectSources) must
		// reach the SUBQUERY plan under the ACTIVE schema `main`, else the unresolved
		// `MAIN.PB` scan fails translation. Plans identically to the alias-collision
		// form. RFC-142.
		plan := assertRowsSchema(t, `SELECT "ID" FROM T1 WHERE EXISTS (SELECT 1 FROM PA, "main"."PB" AS "B")`, "main", []string{
			"ID=0", "ID=1", "ID=2", "ID=3",
		})
		unnestMustNotContain(t, plan, "Explode")
		unnestMustContain(t, plan, "NestedLoopJoin")
	})

	// --- probe round 15: a CORRELATED subquery whose OWN FROM clause has a lateral
	// array unnest correlating to the OUTER query. The unnest lives INSIDE the
	// subquery (`EXISTS (SELECT 1 FROM T1, T1.ARR1 AS VAL WHERE VAL = UV.V)`), and
	// the inner WHERE correlates to the outer (UV.V). The catalog-aware build of the
	// subquery hits an undefined column (the unresolvable outer ref) and falls back
	// to buildCorrelatedExists / buildCorrelatedScalar — which used to rebuild EVERY
	// inner FROM leg as a plain LogicalScan, so `T1.ARR1` was scanned as a TABLE name
	// → `table not found: T1.ARR1`. The fix routes each inner leg through the SAME
	// lateral-unnest classification the main FROM path uses (lateralUnnestCandidate +
	// the unnest virtual scope source), so the inner unnest lowers to FlatMap(Scan,
	// Explode) inside the correlated subquery and the residual correlates correctly.
	// RFC-142.

	t.Run("R15 correlated EXISTS with an inner unnest correlating to the outer", func(t *testing.T) {
		// `SELECT ID FROM UV WHERE EXISTS (SELECT 1 FROM T1, T1.ARR1 AS VAL WHERE
		// VAL = UV.V)` — the inner EXISTS unnests T1.ARR1 and equates each unnested
		// element to the OUTER UV.V. UV has rows (id1,V=201),(id2,V=203). T1.id2's
		// array is {201,202,203}, so BOTH 201 and 203 are present → EXISTS is TRUE for
		// both UV rows → ID∈{1,2}. Before the fix this errored `table not found:
		// T1.ARR1`. The inner unnest must lower to FlatMap-over-Explode INSIDE the
		// correlated subquery. RFC-142.
		plan := assertRows(t, `SELECT "ID" FROM UV WHERE EXISTS (SELECT 1 FROM T1, T1."ARR1" AS "VAL" WHERE "VAL" = "UV"."V")`, []string{
			"ID=1", "ID=2",
		})
		unnestMustContain(t, plan, "FlatMap")
		unnestMustContain(t, plan, "Explode")
	})

	t.Run("R15 correlated EXISTS with an inner unnest discriminates outer rows", func(t *testing.T) {
		// A discriminating residual so the existential is TRUE for one outer row and
		// FALSE for the other (not a degenerate always-true): `… WHERE VAL > UV.V`.
		// UV.id1 (V=201): some T1 element > 201? Yes (202,203 in id2) → TRUE. UV.id2
		// (V=203): some element > 203? No (max element is 203) → FALSE. So only ID=1
		// survives. Proves the inner unnest's elements genuinely feed the
		// outer-correlated comparison. RFC-142.
		assertRows(t, `SELECT "ID" FROM UV WHERE EXISTS (SELECT 1 FROM T1, T1."ARR1" AS "VAL" WHERE "VAL" > "UV"."V")`, []string{
			"ID=1",
		})
	})

	t.Run("R15 correlated NOT EXISTS with an inner unnest is the complement", func(t *testing.T) {
		// The anti-join mirror of the equality case: NOT EXISTS holds when NO unnested
		// element equals UV.V. Both UV.V values (201,203) ARE present in T1.id2's array,
		// so EXISTS is TRUE for both → NOT EXISTS is FALSE for both → ZERO rows. A
		// pre-fix silent pass-through (or the table-not-found error) would not produce
		// this exact empty complement. RFC-142.
		assertRows(t, `SELECT "ID" FROM UV WHERE NOT EXISTS (SELECT 1 FROM T1, T1."ARR1" AS "VAL" WHERE "VAL" = "UV"."V")`, nil)
	})

	t.Run("R15 correlated NOT EXISTS with an inner unnest discriminating residual", func(t *testing.T) {
		// The discriminating residual under NOT EXISTS: `VAL > UV.V`. EXISTS was TRUE
		// for UV.id1 (an element > 201 exists) and FALSE for UV.id2 (none > 203), so NOT
		// EXISTS is the complement → only ID=2. Pins that the inner-unnest residual
		// drives the negated semi-join, not just the positive one. RFC-142.
		assertRows(t, `SELECT "ID" FROM UV WHERE NOT EXISTS (SELECT 1 FROM T1, T1."ARR1" AS "VAL" WHERE "VAL" > "UV"."V")`, []string{
			"ID=2",
		})
	})

	t.Run("R15 correlated EXISTS with an inner unnest WITH ORDINALITY", func(t *testing.T) {
		// The inner unnest carries an AT ordinal binding alongside the
		// outer-correlated element equality: `FROM T1, T1.ARR1 AS VAL AT AT WHERE VAL =
		// UV.V`. The ordinal participates in the inner Explode (1-based per inner row)
		// while the element equality stays the outer-correlated residual; both UV rows
		// still match (201,203 present) → ID∈{1,2}. Proves the WITH-ORDINALITY inner
		// unnest composes with the outer correlation. RFC-142.
		plan := assertRows(t, `SELECT "ID" FROM UV WHERE EXISTS (SELECT 1 FROM T1, T1."ARR1" AS "VAL" AT "AT" WHERE "VAL" = "UV"."V" AND "AT" >= 1)`, []string{
			"ID=1", "ID=2",
		})
		unnestMustContain(t, plan, "WITH ORDINALITY")
	})

	t.Run("R15 correlated SCALAR subquery with an inner unnest in the projection", func(t *testing.T) {
		// A correlated SCALAR subquery (projection position) whose OWN FROM has a
		// lateral unnest correlated to the outer: `SELECT ID, (SELECT MAX(VAL) FROM T1,
		// T1.ARR1 AS VAL WHERE VAL < UV.V) AS M FROM UV`. The scalar aggregates the
		// unnested elements (across all T1 rows) that are < the outer UV.V. UV.id1
		// (V=201): elements < 201 = {101} → MAX=101. UV.id2 (V=203): elements < 203 =
		// {101,201,202} → MAX=202. Before the fix the inner `T1.ARR1` mis-scanned as a
		// table. The scalar path (buildCorrelatedScalar) reuses the SAME unnest
		// classification, lowering to StreamingAgg over FlatMap(Scan, Explode). RFC-142.
		//
		// The scalar value surfaces under BOTH the user alias `M` and the raw
		// aggregate column name `T1.MAX(VAL)` (the engine's existing dual-label for a
		// correlated scalar aggregate) — both carry the SAME correct value, which is
		// what the unnest fix proves; the dual label is pre-existing labeling behavior.
		plan := assertRows(t, `SELECT "ID", (SELECT MAX("VAL") FROM T1, T1."ARR1" AS "VAL" WHERE "VAL" < "UV"."V") AS "M" FROM UV`, []string{
			"ID=1|M=101|T1.MAX(VAL)=101", "ID=2|M=202|T1.MAX(VAL)=202",
		})
		unnestMustContain(t, plan, "Explode")
	})

	t.Run("R15 correlated EXISTS over inner unnest carrying the outer id", func(t *testing.T) {
		// Carry the outer column through the projection alongside the inner-unnest
		// existential: `SELECT ID, V FROM UV WHERE EXISTS (...)`. Each surviving outer
		// row keeps its own (ID, V); UV.id1→(1,201), UV.id2→(2,203). Pins that the
		// outer projection composes with the correlated inner-unnest semi-join and the
		// outer columns are not clobbered by the inner FROM. RFC-142.
		assertRows(t, `SELECT "ID", "V" FROM UV WHERE EXISTS (SELECT 1 FROM T1, T1."ARR1" AS "VAL" WHERE "VAL" = "UV"."V")`, []string{
			"ID=1|V=201", "ID=2|V=203",
		})
	})

	// --- probe round 25 P2a: a CORRELATED subquery whose OWN FROM has a
	// SCHEMA-QUALIFIED table source (`s.PB`, `s` the session schema). The outer
	// correlation (`B.ID = T1.ID`) makes the catalog-aware inner build fail with an
	// undefined column, so EXISTS/scalar fall back to buildCorrelatedExists /
	// buildCorrelatedScalar. Those fallbacks rebuild the inner FROM themselves and
	// (pre-fix) handed the raw `s.PB` straight to Analyzer.ResolveTable, which does
	// NOT strip a schema qualifier → `table not found: S.PB` → a 42703 rejection of
	// a VALID query. The normal SELECT path strips `s.PB`→`PB` via
	// normalizeSchemaQualifiedSelectSources; the fix runs the SAME normalization in
	// the correlated fallback before resolving the join sources, so `s.PB` is the
	// real cross-join table PB. Java's generateAccess resolves the table first at
	// every FROM-source point. RFC-142.

	t.Run("R25 P2a correlated EXISTS with a schema-qualified inner source plans the cross join", func(t *testing.T) {
		// `SELECT ID FROM T1 WHERE EXISTS (SELECT 1 FROM PA AS s, s.PB AS B WHERE
		// B.ID = T1.ID)`. The inner is PA×PB (a plain cross join, NOT a correlated
		// unnest — `s` is the schema, not a prior-source array field), with the
		// existential residual `B.ID = T1.ID`. PB has only id=1, so EXISTS is TRUE
		// only for T1.ID=1 → ID=1. Pre-fix: `table not found: S.PB` (42703). The
		// cross join must resolve.
		assertRows(t, `SELECT "ID" FROM T1 WHERE EXISTS (SELECT 1 FROM PA AS "s", "s"."PB" AS "B" WHERE "B"."ID" = T1."ID")`, []string{
			"ID=1",
		})
	})

	t.Run("R25 P2a correlated NOT EXISTS with a schema-qualified inner source is the complement", func(t *testing.T) {
		// The anti-join mirror: NOT EXISTS holds for every T1 row whose ID is NOT in
		// PB. PB has only id=1, so NOT EXISTS is TRUE for T1 ids 0, 2 and 3. Pins that
		// the schema-qualified cross join drives the NEGATED semi-join too (not just
		// the positive one), and is not a degenerate pass-through. RFC-142.
		assertRows(t, `SELECT "ID" FROM T1 WHERE NOT EXISTS (SELECT 1 FROM PA AS "s", "s"."PB" AS "B" WHERE "B"."ID" = T1."ID")`, []string{
			"ID=0", "ID=2", "ID=3",
		})
	})

	t.Run("R25 P2a correlated EXISTS with a schema-qualified primary inner source", func(t *testing.T) {
		// The schema qualifier on the PRIMARY inner source (`FROM s.PA AS A, PB AS B`)
		// is normalized by the same pass (normalizeSchemaQualifiedSelectSources strips
		// the primary source too). A.ID = T1.ID correlates to the outer; PA has only
		// id=1 → EXISTS true only for T1.ID=1. Proves both the primary and the join
		// leg are stripped, not just the comma leg. RFC-142.
		assertRows(t, `SELECT "ID" FROM T1 WHERE EXISTS (SELECT 1 FROM "s"."PA" AS "A", PB AS "B" WHERE "A"."ID" = T1."ID")`, []string{
			"ID=1",
		})
	})

	t.Run("R25 P2a correlated SCALAR subquery with a schema-qualified inner source", func(t *testing.T) {
		// The scalar-subquery fallback (buildCorrelatedScalar) runs the SAME
		// normalization: `SELECT ID, (SELECT MAX(B.ID) FROM PA AS s, s.PB AS B WHERE
		// B.ID = T1.ID) AS M FROM T1`. The inner cross join PA×PB keeps the PB rows
		// whose id = the outer T1.ID; only T1.ID=1 matches a PB row → MAX=1 for id1,
		// NULL for the rest (T1 ids 0,2,3). Pre-fix the inner `s.PB` mis-resolved
		// (table not found) and the scalar subquery was rejected. The scalar value
		// surfaces under BOTH the user alias `M` and the raw aggregate column name
		// `S.MAX(ID)` (the engine's existing dual-label for a correlated scalar
		// aggregate, keyed by the subquery's primary alias); both carry the same value
		// — the dual label is pre-existing behavior. RFC-142.
		assertRows(t, `SELECT "ID", (SELECT MAX("B"."ID") FROM PA AS "s", "s"."PB" AS "B" WHERE "B"."ID" = T1."ID") AS "M" FROM T1`, []string{
			"ID=0|M=<nil>|S.MAX(ID)=<nil>", "ID=1|M=1|S.MAX(ID)=1",
			"ID=2|M=<nil>|S.MAX(ID)=<nil>", "ID=3|M=<nil>|S.MAX(ID)=<nil>",
		})
	})

	// --- probe round 16: unnest × {non-rightmost-filter, GROUP BY, aggregate
	// ORDER BY} ---------------------------------------------------------------

	t.Run("R16 P1 WHERE on a BURIED (non-rightmost) unnest element filters elements", func(t *testing.T) {
		// probe round 16 P1 (silent-wrong, drops rows): `FROM T1, T1.ARR1 AS V, U
		// WHERE V > 201` — the lateral unnest `T1.ARR1 AS V` is NOT the rightmost FROM
		// item (U is), so the TOP-LEVEL LogicalJoin's Right is U (a scan) and the unnest
		// is BURIED in join.Left. translateFilter's unnest-element WHERE rewrite only
		// fired when join.Right was a LogicalUnnest, so for the buried unnest the rewrite
		// was SKIPPED: the `V > 201` reference stayed FieldValue{V, QOV(V)} pushed below
		// the unnest where V is UNBOUND → every row dropped (got=[]). The fix recurses
		// into join.Left to find the buried unnest leg and rewrites its element ref to
		// the bare scalar QOV so the filter binds and pushes into the inner Explode.
		// U has a single row, so the cross product does not change the element set:
		// T1.ARR1={101},{201,202,203}; V > 201 keeps {202,203}. Before the fix: EMPTY.
		plan := assertRows(t, `SELECT "V" FROM T1, T1."ARR1" AS "V", U WHERE "V" > 201`, []string{
			"V=202", "V=203",
		})
		// The element predicate pushes into the inner Explode filter, not onto the
		// outer scan (which would drop every row).
		unnestMustContain(t, plan, "FlatMap")
		unnestMustContain(t, plan, "Explode")
		unnestMustContain(t, plan, "PredicatesFilter")
	})

	t.Run("R16 P1 WHERE on a buried unnest element exact equality", func(t *testing.T) {
		// Exact-equality form of the buried-unnest element WHERE. WHERE V = 202 keeps
		// only that one element across the cross product with U. Before the fix: EMPTY.
		assertRows(t, `SELECT "V" FROM T1, T1."ARR1" AS "V", U WHERE "V" = 202`, []string{
			"V=202",
		})
	})

	t.Run("R16 P1 WHERE on a buried unnest element carrying the outer id", func(t *testing.T) {
		// The buried-unnest element WHERE composing with the outer T1.ID projection:
		// each surviving element pairs with its outer ID, and U.V=999 never appears.
		assertRows(t, `SELECT T1."ID", "V" FROM T1, T1."ARR1" AS "V", U WHERE "V" >= 202`, []string{
			"T1.ID=2|V=202", "T1.ID=2|V=203",
		})
	})

	t.Run("R16 P1 WHERE on a buried unnest ORDINAL filters by position", func(t *testing.T) {
		// The ordinal variant of the buried-unnest WHERE: `FROM T1, T1.ARR1 AS V AT O,
		// U WHERE O > 1` — the unnest (WITH ORDINALITY) is buried in join.Left, U is the
		// rightmost leg. The AT reference O must rewrite to the inner Explode's _1
		// ordinal field and push into the inner filter, keeping elements whose ORIGINAL
		// 1-based position is > 1. T1.ARR1={101} (only pos 1 → nothing) and
		// {201,202,203} (pos 2,3 → 202,203). Before the fix the ordinal ref was unbound
		// below the unnest → every row dropped.
		plan := assertRows(t, `SELECT "V", "O" FROM T1, T1."ARR1" AS "V" AT "O", U WHERE "O" > 1`, []string{
			"O=2|V=202", "O=3|V=203",
		})
		unnestMustContain(t, plan, "WITH ORDINALITY")
		unnestMustContain(t, plan, "PredicatesFilter")
	})

	t.Run("R16 P1 WHERE on a buried unnest ordinal arithmetic", func(t *testing.T) {
		// Computed buried ordinal: `AT O … WHERE O + 1 = 3` ⇒ ordinal 2. Only id2's
		// array has a 2nd element. Before the fix: EMPTY.
		assertRows(t, `SELECT "V", "O" FROM T1, T1."ARR1" AS "V" AT "O", U WHERE "O" + 1 = 3`, []string{
			"O=2|V=202",
		})
	})

	t.Run("R16 P1 control: WHERE on the rightmost unnest still filters", func(t *testing.T) {
		// Control: when the unnest IS the rightmost FROM item (`FROM T1, U, T1.ARR1 AS
		// V WHERE V > 201`), the direct (round-1) rewrite path handles it. Proves the
		// buried-unnest fix did not perturb the direct path. Same element set survives.
		assertRows(t, `SELECT "V" FROM T1, U, T1."ARR1" AS "V" WHERE "V" > 201`, []string{
			"V=202", "V=203",
		})
	})

	t.Run("R16 P2a GROUP BY on a buried unnest element groups by the element", func(t *testing.T) {
		// probe round 16 P2a (silent-wrong grouping): `SELECT V, COUNT(*) FROM T1,
		// T1.ARR1 AS V, U GROUP BY V` where U has its OWN column V=999. A simple column
		// group key does NOT populate groupByExprs, so `GROUP BY V` bypassed the
		// resolver and fell back to a bare FieldValue{V} — which mergeRows overwrites
		// last-leg-wins with U.V=999. So grouping collapsed every element into ONE group
		// (U.V=999) → a single row COUNT=4. The fix routes the simple group key through
		// ResolveColumnShadowingQualified (the same helper the projection/ORDER-BY paths
		// use), so the key resolves to the QUALIFIED V.V — the unnest element. U has one
		// row so each distinct element is its own group of count 1.
		// T1.ARR1 elements across all rows: 101,201,202,203 (id0 empty, id3 NULL).
		// The raw-executor row carries BOTH the canonical aggregate column `COUNT(*)`
		// and the user alias `N` (the engine's pre-existing dual-label for the raw
		// executor path) — both the same value; the SQL driver projects them to `N`.
		assertRows(t, `SELECT "V", COUNT(*) AS "N" FROM T1, T1."ARR1" AS "V", U GROUP BY "V"`, []string{
			"COUNT(*)=1|N=1|V=101", "COUNT(*)=1|N=1|V=201", "COUNT(*)=1|N=1|V=202", "COUNT(*)=1|N=1|V=203",
		})
	})

	t.Run("R16 P2a GROUP BY buried unnest element with duplicate elements counts per element", func(t *testing.T) {
		// The grouping is genuinely on the element, not a constant: T1.ARR1_NN over
		// ids 1,2,3 = {101},{201,202,203},{301}; cross with U (1 row). Every element is
		// distinct → each group is count 1. Add the NOT-NULL array so id3 contributes
		// 301 — proving the group key tracks the element across all outer rows.
		assertRows(t, `SELECT "V", COUNT(*) AS "N" FROM T1, T1."ARR1_NN" AS "V", U GROUP BY "V"`, []string{
			"COUNT(*)=1|N=1|V=101", "COUNT(*)=1|N=1|V=201", "COUNT(*)=1|N=1|V=202",
			"COUNT(*)=1|N=1|V=203", "COUNT(*)=1|N=1|V=301",
		})
	})

	t.Run("R16 P2a control: explicitly-qualified U.V GROUP BY groups by U's column", func(t *testing.T) {
		// Control: an EXPLICITLY qualified `U.V` group key is unambiguous and groups by
		// U's column (999), NOT the unnest element. Proves the shadowing qualification
		// only redirects the BARE `V` the unnest binding owns. Every (T1×ARR1×U) row has
		// U.V=999, so it is one group of count 4 (101,201,202,203).
		assertRows(t, `SELECT "U"."V", COUNT(*) AS "N" FROM T1, T1."ARR1" AS "V", U GROUP BY "U"."V"`, []string{
			"COUNT(*)=4|N=4|U.V=999",
		})
	})

	t.Run("R16 P2b aggregate ORDER BY on the group key sorts descending", func(t *testing.T) {
		// probe round 16 P2b (silent-wrong order): `SELECT V, COUNT(*) FROM T1, T1.ARR1
		// AS V GROUP BY V ORDER BY V DESC` — for a GROUPED query the ORDER BY sort is
		// ABOVE the aggregate output, but qualifyShadowedSortKeys resolved the sort key
		// against the PRE-aggregate FROM scope and rewrote it to V.V. The aggregate rows
		// expose the group key as BARE V (not V.V), so the sort compared nil keys → DESC
		// ignored. The fix skips the FROM-scope qualification for a post-aggregate
		// ORDER BY key that names a GROUP-BY output column, so the sort reads the bare
		// group output and orders for real. T1.ARR1 distinct elements: 101,201,202,203.
		assertRowsOrdered(t, `SELECT "V", COUNT(*) AS "N" FROM T1, T1."ARR1" AS "V" GROUP BY "V" ORDER BY "V" DESC`, []string{
			"COUNT(*)=1|N=1|V=203", "COUNT(*)=1|N=1|V=202", "COUNT(*)=1|N=1|V=201", "COUNT(*)=1|N=1|V=101",
		})
	})

	t.Run("R16 P2b aggregate ORDER BY on the group key sorts ascending", func(t *testing.T) {
		// The ASC companion: `ORDER BY V ASC` over the grouped unnest must order the
		// group keys ascending. A no-op sort (the bug) would coincide with insertion
		// order, so DESC above is the revert-proof and ASC pins the symmetric direction.
		assertRowsOrdered(t, `SELECT "V", COUNT(*) AS "N" FROM T1, T1."ARR1" AS "V" GROUP BY "V" ORDER BY "V" ASC`, []string{
			"COUNT(*)=1|N=1|V=101", "COUNT(*)=1|N=1|V=201", "COUNT(*)=1|N=1|V=202", "COUNT(*)=1|N=1|V=203",
		})
	})

	t.Run("R16 P2b non-grouped ORDER BY over an unnest still orders (round-7 path)", func(t *testing.T) {
		// The pre-aggregate (NON-grouped) ORDER BY over an unnest must STILL get the
		// round-7/round-8 V.V qualification (the unnest is below the sort, no aggregate):
		// `SELECT V FROM T1, T1.ARR1 AS V, U ORDER BY V DESC` — the bare sort key must
		// resolve to the unnest element V.V (else a later U.V clobbers it). Proves the
		// P2b fix narrowed the skip to GROUPED queries only and did not break the
		// non-grouped shadowing-sort path. Descending over the unnested elements.
		assertRowsOrdered(t, `SELECT "V" FROM T1, T1."ARR1" AS "V", U ORDER BY "V" DESC`, []string{
			"V=203", "V=202", "V=201", "V=101",
		})
	})

	t.Run("R16 P2b non-grouped ORDER BY over an unnest ascending", func(t *testing.T) {
		// ASC companion of the non-grouped shadowing-sort control.
		assertRowsOrdered(t, `SELECT "V" FROM T1, T1."ARR1" AS "V", U ORDER BY "V" ASC`, []string{
			"V=101", "V=201", "V=202", "V=203",
		})
	})

	// --- probe round 18: every scope/resolver/column-enumeration path must be
	// unnest-aware. Two silent-wrong shapes (P2a JOIN-ON dropped, P2b qualified
	// star not expanded) plus the convergence paths (CTE-scope WHERE builder,
	// projection-resolver) the audit made unnest-aware. RFC-142. -------------

	t.Run("R18 P2a explicit JOIN ON before a comma unnest applies the ON predicate", func(t *testing.T) {
		// `FROM T1 INNER JOIN JU ON JU.ID = T1.ID, T1.ARR1 AS X` — the LogicalUnnest
		// leg is in sq.joins, but upgradeJoinOnPredicates built its scope by resolving
		// EVERY join clause as a real table. The unnest entry (T1.ARR1, not a table)
		// made that scope build FAIL, so the earlier ON predicate (JU.ID = T1.ID) was
		// never attached → the T1/JU join degraded to a CROSS join before the unnest →
		// it returned every T1 row's elements paired with every JU row (silent-wrong).
		// The fix makes upgradeJoinOnPredicates skip the lateral-unnest leg and register
		// its virtual source via the shared isLateralUnnestJoin/unnestVirtualScopeSource
		// helpers, so the ON predicate resolves against the real legs and the T1/JU join
		// stays an INNER join. JU matches T1 ids 1,2 only → 4 elements ({101} from id1,
		// {201,202,203} from id2), each carrying its JU.K. A cross join would ALSO pair
		// JU id1 with T1 id2 and JU id2 with T1 id1 (8 rows). Assert exactly the inner
		// matches. Before the fix: the cross product (wrong K pairings, 8 rows).
		plan := assertRows(t, `SELECT T1."ID", JU."K", "X" FROM T1 INNER JOIN JU ON JU."ID" = T1."ID", T1."ARR1" AS "X"`, []string{
			"JU.K=1001|T1.ID=1|X=101",
			"JU.K=2002|T1.ID=2|X=201",
			"JU.K=2002|T1.ID=2|X=202",
			"JU.K=2002|T1.ID=2|X=203",
		})
		// The unnest still lowers to a FlatMap over an Explode; the explicit join's ON
		// equality is attached to the inner scan (not a cross join).
		unnestMustContain(t, plan, "FlatMap")
		unnestMustContain(t, plan, "Explode")
	})

	t.Run("R18 P2a explicit JOIN ON before a comma unnest with ordinality", func(t *testing.T) {
		// The same shape under WITH ORDINALITY: the ON predicate still applies (only
		// T1 ids 1,2), and the ordinal is each array's 1-based position resetting per
		// outer row. Proves the ON-predicate attach composes with the ordinality
		// 2-field record.
		plan := assertRows(t, `SELECT T1."ID", "X", "O" FROM T1 INNER JOIN JU ON JU."ID" = T1."ID", T1."ARR1" AS "X" AT "O"`, []string{
			"O=1|T1.ID=1|X=101",
			"O=1|T1.ID=2|X=201",
			"O=2|T1.ID=2|X=202",
			"O=3|T1.ID=2|X=203",
		})
		unnestMustContain(t, plan, "WITH ORDINALITY")
	})

	t.Run("R18 P2a explicit JOIN ON before a comma unnest with element WHERE", func(t *testing.T) {
		// The ON predicate AND a WHERE on the unnested element compose: only T1 ids 1,2
		// survive the inner join, then `WHERE X > 201` keeps {202,203} from id2 (101 and
		// 201 are filtered). The ON predicate must NOT be dropped (else id0/id2's other
		// rows would leak) and the element filter pushes into the inner Explode.
		assertRows(t, `SELECT T1."ID", "X" FROM T1 INNER JOIN JU ON JU."ID" = T1."ID", T1."ARR1" AS "X" WHERE "X" > 201`, []string{
			"T1.ID=2|X=202", "T1.ID=2|X=203",
		})
	})

	t.Run("R18 P2b qualified star over a non-ordinal unnest alias expands to the element only", func(t *testing.T) {
		// `SELECT V.* FROM T1, T1.ARR1 AS V` — the unnest alias V is in the resolver
		// scope, but expandQualifiedStars/expandProjQualifier only enumerated real
		// record types, so they could NOT expand `V.*` → the query was left as an
		// unqualified star → returned the ENTIRE FlatMap row (outer T1.ID + ARR1 array
		// included) instead of just the unnest source's columns. The fix expands a
		// qualified star over the unnest virtual source to its element column (the AS
		// alias V), via the shared column list. The expanded star projects the element
		// QUALIFIED to the unnest correlation (V.V) — the only column — so the raw row
		// map carries the single `V.V` key, with NO outer T1.ID / ARR1 array. Before the
		// fix: an unqualified star → the ENTIRE FlatMap row (outer columns leaked).
		plan := assertRows(t, `SELECT "V".* FROM T1, T1."ARR1" AS "V"`, []string{
			"V.V=101", "V.V=201", "V.V=202", "V.V=203",
		})
		unnestMustContain(t, plan, "FlatMap")
		unnestMustContain(t, plan, "Explode")
		unnestMustNotContain(t, plan, "WITH ORDINALITY")
	})

	t.Run("R18 P2b qualified star over an ordinality unnest expands to element plus ordinal", func(t *testing.T) {
		// `SELECT V.* FROM T1, T1.ARR1 AS V AT O` — the qualified star over an
		// ORDINALITY unnest expands to BOTH the element (AS) and the ordinal (AT)
		// columns, 1-based and resetting per outer row. Assert exactly those two
		// columns (no outer T1 columns). RFC-142.
		plan := assertRows(t, `SELECT "V".* FROM T1, T1."ARR1" AS "V" AT "O"`, []string{
			"V.O=1|V.V=101", "V.O=1|V.V=201", "V.O=2|V.V=202", "V.O=3|V.V=203",
		})
		unnestMustContain(t, plan, "WITH ORDINALITY")
	})

	t.Run("R18 P2b qualified star mixed with a named outer column", func(t *testing.T) {
		// `SELECT T1.ID, V.* FROM T1, T1.ARR1 AS V` — the qualified-star slot V.* is
		// expanded ALONGSIDE a named outer column (the mixed-star expandQualifiedStars
		// path, not the lone-qualifier path). The outer ID flows AND the element V is
		// the only star-expanded column. RFC-142.
		assertRows(t, `SELECT T1."ID", "V".* FROM T1, T1."ARR1" AS "V"`, []string{
			"T1.ID=1|V.V=101", "T1.ID=2|V.V=201", "T1.ID=2|V.V=202", "T1.ID=2|V.V=203",
		})
	})

	t.Run("qualified star over an ALIASLESS unnest default alias expands to the element", func(t *testing.T) {
		// `SELECT ARR1.* FROM T1, T1.ARR1` — NO `AS` was written, so the element
		// binding alias DEFAULTS to the array field name ARR1 (unnestAliases'
		// Java-faithful table-name fallback). Ordinary `SELECT ARR1` resolves
		// through that default alias, but the qualified-star VALIDATOR
		// (validateQualifiedStarSourcesFromClassification) whitelisted only the raw
		// join table/alias string (`T1.ARR1`), NOT the default element alias `ARR1`,
		// so `ARR1.*` was rejected 42F01 (unknown qualifier) BEFORE the unnest-aware
		// expansion could run. The fix adds the unnestAliases-derived default alias
		// to the whitelist, so `ARR1.*` is accepted and expanded to the element
		// column (ARR1.ARR1) — the SAME unnest virtual source the explicit-alias
		// `V.*` case (R18 P2b above) uses. Before the fix: 42F01. RFC-142.
		plan := assertRows(t, `SELECT "ARR1".* FROM T1, T1."ARR1"`, []string{
			"ARR1.ARR1=101", "ARR1.ARR1=201", "ARR1.ARR1=202", "ARR1.ARR1=203",
		})
		unnestMustContain(t, plan, "FlatMap")
		unnestMustContain(t, plan, "Explode")
		unnestMustNotContain(t, plan, "WITH ORDINALITY")
	})

	t.Run("qualified star over an ALIASLESS unnest with AT expands to element plus ordinal", func(t *testing.T) {
		// `SELECT ARR1.* FROM T1, T1.ARR1 AT O` — aliasless AS (default element
		// alias ARR1) WITH an explicit AT ordinal alias O. The ordinal column is
		// registered under the AS-default correlation (ARR1) by the shared
		// unnestVirtualScopeSource, so the default-alias star expands to BOTH the
		// element (ARR1.ARR1) AND the ordinal (ARR1.O), 1-based and resetting per
		// outer row — mirroring the explicit-alias `V.* AT O` → (V.V, V.O) shape.
		// Before the fix the aliasless `ARR1.*` was 42F01 before any expansion. The
		// AT alias O is a SEPARATE binding; the default-alias star carries the
		// ordinal because it lives under the AS correlation, not because O is the
		// qualifier. RFC-142.
		plan := assertRows(t, `SELECT "ARR1".* FROM T1, T1."ARR1" AT "O"`, []string{
			"ARR1.ARR1=101|ARR1.O=1", "ARR1.ARR1=201|ARR1.O=1",
			"ARR1.ARR1=202|ARR1.O=2", "ARR1.ARR1=203|ARR1.O=3",
		})
		unnestMustContain(t, plan, "WITH ORDINALITY")
	})

	t.Run("explicit-alias qualified star still works (control)", func(t *testing.T) {
		// Control: the explicit-alias case (`FROM T1, T1.ARR1 AS V` → `V.*`) the
		// round-17 expansion already handled must stay green — the validator
		// whitelist still accepts the explicit AS alias V. Pins that the
		// default-alias whitelist addition did not disturb the explicit-alias path.
		assertRows(t, `SELECT "V".* FROM T1, T1."ARR1" AS "V"`, []string{
			"V.V=101", "V.V=201", "V.V=202", "V.V=203",
		})
	})

	t.Run("genuinely-unknown qualified star still rejects 42F01 (control)", func(t *testing.T) {
		// Control: a qualifier that names NO source (real or unnest) must still be
		// rejected with 42F01 (ErrCodeUndefinedTable). `BOGUS.*` over a single table
		// FROM T1 has no source BOGUS — the whitelist widening only adds genuine
		// unnest default aliases, never a free-floating name. Pins that the fix did
		// not weaken the unknown-qualifier rejection. RFC-142.
		assertRejected(t, md, `SELECT "BOGUS".* FROM T1`, api.ErrCodeUndefinedTable)
	})

	t.Run("R18 convergence CTE-scope WHERE on an unnest element", func(t *testing.T) {
		// `WITH C AS (SELECT ID FROM U) SELECT VAL FROM T1, T1.ARR1 AS VAL WHERE VAL >
		// 201` — a CTE in scope makes the planner route the WHERE through the CTE-aware
		// predicate builder (buildWherePredicateForJoinsWithCTEScopes), the audit's
		// convergence target. It is now unnest-aware (its non-CTE twin already was), so
		// the element filter resolves and pushes into the inner Explode. Without the
		// fix it declined and the WHERE degraded; the element predicate is kept.
		plan := assertRows(t, `WITH "C" AS (SELECT "ID" FROM U) SELECT "VAL" FROM T1, T1."ARR1" AS "VAL" WHERE "VAL" > 201`, []string{
			"VAL=202", "VAL=203",
		})
		unnestMustContain(t, plan, "PredicatesFilter")
		unnestMustContain(t, plan, "Explode")
	})

	t.Run("R18 convergence CTE-scope WHERE on an unnest ordinal", func(t *testing.T) {
		// The ordinality companion of the CTE-scope WHERE path: `... AS VAL AT O WHERE
		// O = 1` keeps the first element of each array. Pins the CTE-aware predicate
		// builder threads the AT ordinal correlation into the inner Explode filter.
		plan := assertRows(t, `WITH "C" AS (SELECT "ID" FROM U) SELECT "ID", "VAL" FROM T1, T1."ARR1" AS "VAL" AT "O" WHERE "O" = 1`, []string{
			"ID=1|VAL=101", "ID=2|VAL=201",
		})
		unnestMustContain(t, plan, "WITH ORDINALITY")
		unnestMustContain(t, plan, "PredicatesFilter")
	})

	t.Run("R18 convergence GROUP BY over an unnest element counts per element", func(t *testing.T) {
		// `SELECT V, COUNT(*) FROM T1, T1.ARR1 AS V GROUP BY V` — the aggregate-operand
		// resolver (buildProjectionResolverWithCTEScopes) is now unnest-aware directly
		// (the audit made it register the virtual source rather than relying solely on
		// the buildSelectScope fallback). Every element is distinct here, so each group
		// has count 1. Pins that the GROUP-BY key resolves to the unnest element across
		// the projection-resolver path. RFC-142.
		assertRows(t, `SELECT "V", COUNT(*) FROM T1, T1."ARR1" AS "V" GROUP BY "V"`, []string{
			"COUNT(*)=1|V=101", "COUNT(*)=1|V=201", "COUNT(*)=1|V=202", "COUNT(*)=1|V=203",
		})
	})

	// --- probe round 19: the streaming-aggregate REQUIRED pre-aggregate sort must
	// carry the QUALIFIED group-key ValueExpr, NOT the bare field — the follow-on to
	// the round-15/16 GROUP-BY-shadowing fix. The round-16 fix made `GROUP BY V` over
	// a shadowing unnest alias resolve to the qualified key V.V, but
	// ImplementStreamingAggregationRule built its InMemorySort(FullScan) pre-aggregate
	// sort from `fv.Field` ONLY (the bare `V`). The aggregate cursor GROUPS by the
	// qualified V.V, but the inserted sort ordered by the merged row's BARE `V` key
	// (which mergeRows keys last-leg-wins as a LATER same-named column, e.g. GW.V=999,
	// a constant → a NO-OP sort). Sort and group key DISAGREE → contiguous array
	// elements split into multiple non-contiguous groups → duplicate/wrong counts. The
	// fix routes a qualified FieldValue group key (Child != nil) through the SortKey's
	// ValueExpr per-row path, exactly like ImplementInMemorySortRule (round-8 P2a), so
	// the pre-aggregate sort and the grouping use the SAME key. RFC-142. ----------

	t.Run("R19 GROUP BY buried shadowing unnest element with NON-CONTIGUOUS duplicates counts per element", func(t *testing.T) {
		// THE revert-proof: GD has duplicate element values that recur NON-CONTIGUOUSLY
		// across outer rows (id1.ARR={1,2}, id2.ARR={1,2} → element flow order 1,2,1,2),
		// crossed with GW (one row, V=999 shadows the bare key). `GROUP BY V` groups by
		// the unnest element V.V. With the bug the pre-aggregate sort keys off the bare
		// `V` = GW.V = 999 (constant) → a NO-OP sort → the streaming aggregate sees
		// 1,2,1,2 and EMITS A NEW GROUP whenever the key changes → value 1 splits into
		// TWO groups (and value 2 into two) with counts 1 each, instead of one group per
		// value with count 2. The fix sorts by the qualified V.V → order 1,1,2,2 → one
		// group per value, count 2. Assert exact (element, count): one row per element.
		assertRows(t, `SELECT "V", COUNT(*) AS "N" FROM GD, GD."ARR" AS "V", GW GROUP BY "V"`, []string{
			"COUNT(*)=2|N=2|V=1", "COUNT(*)=2|N=2|V=2",
		})
	})

	t.Run("R19 GROUP BY buried shadowing unnest ORDINAL with NON-CONTIGUOUS positions counts per ordinal", func(t *testing.T) {
		// The ordinality variant grouping by the ORDINAL (AT): `... AS V AT O, GW GROUP
		// BY O`. The ordinal resets per outer row (1,2,1,2 over GD's two 2-element
		// arrays), so the ordinal-1 rows are {GD1[0], GD2[0]} and the ordinal-2 rows are
		// {GD1[1], GD2[1]} — each ordinal value has count 2 but the rows carrying it are
		// NON-CONTIGUOUS in flow order (O scan order is 1,2,1,2). GW carries a column `O`
		// (=888) that SHADOWS the bare `O` key, so a buggy bare-`O` pre-aggregate sort is
		// a NO-OP (all rows key 888) → 1,2,1,2 stays → each ordinal splits into two
		// count-1 groups. The fix sorts by the qualified V.O (1,1,2,2) → one group per
		// ordinal, count 2. The unnest's group key V.O is qualified (a FieldValue with a
		// Child), so it exercises the SAME Child!=nil routing as the element key.
		assertRows(t, `SELECT "O", COUNT(*) AS "N" FROM GD, GD."ARR" AS "V" AT "O", GW GROUP BY "O"`, []string{
			"COUNT(*)=2|N=2|O=1", "COUNT(*)=2|N=2|O=2",
		})
	})

	t.Run("R19 control: GROUP BY unnest element with NON-CONTIGUOUS duplicates and NO shadowing later source", func(t *testing.T) {
		// Control: the SAME non-contiguous-duplicate array WITHOUT a later shadowing source
		// (`FROM GD, GD.ARR AS V GROUP BY V`, no GW). Here no later column shadows the bare
		// `V`, so even the bare-key path resolves to the element — but the qualified key
		// must STILL sort correctly (the unnest element binding is qualified V.V either
		// way). Proves the fix does not regress the plain single-unnest grouped case: each
		// value is one group of count 2.
		assertRows(t, `SELECT "V", COUNT(*) AS "N" FROM GD, GD."ARR" AS "V" GROUP BY "V"`, []string{
			"COUNT(*)=2|N=2|V=1", "COUNT(*)=2|N=2|V=2",
		})
	})

	// --- probe round 20: every POST-aggregate consumer that references a grouped
	// unnest key must read it under the aggregate OUTPUT name (the bare `V`), NOT
	// the qualified PRE-aggregate value `V.V`. The round-15/16 fix stores the
	// QUALIFIED FieldValue(QOV(V), V) in GroupKeyValues so grouping is on the
	// unnest ELEMENT (not a later same-named column); the aggregate cursor outputs
	// that key under aggKeyName = the BARE `V`. Round-18 already rebased the
	// post-aggregate ORDER BY (aggregateGroupKeyOutputName). Round-20 is the same
	// rebase for the post-aggregate PROJECTION (computed expressions) and HAVING.
	// Before the fix a computed projection / HAVING reference to `V` resolved to
	// the qualified `V.V` and read NULL from the bare-V aggregate rows. RFC-142. ---

	t.Run("R20 computed projection over a grouped unnest key reads the element", func(t *testing.T) {
		// THE projection revert-proof: `SELECT V + 1, COUNT(*) FROM GD, GD.ARR AS V
		// GROUP BY V`. The group key is the QUALIFIED V.V (unnest element); the
		// aggregate cursor outputs it under the BARE `V`. A computed projection
		// `V + 1` is NOT a bare/exact group-key reference, so it falls to the
		// resolver, which resolves `V` against the PRE-aggregate Shadowing source →
		// qualified FieldValue(QOV(V), V) → explain `V.V` → reads the MISSING `V.V`
		// key off the bare-V aggregate row → NULL computed column. The fix rebases
		// the post-aggregate `V.V` reference to the bare aggregate-output name `V`,
		// so `V + 1` reads the element + 1. GD.ARR over both rows = {1,2},{1,2} →
		// distinct elements 1,2 → computed 2,3, each count 2. Before the fix the
		// computed column was NULL. The raw-executor row also carries the canonical
		// expression key `(V + 1)`, the positional `_0` (executor.go's Java-compat
		// `_N` key for the computed slot-0 column), and the dual `COUNT(*)`/`N`
		// aggregate labels — all the same values; the SQL driver projects them to the
		// user alias VP.
		assertRows(t, `SELECT "V" + 1 AS "VP", COUNT(*) AS "N" FROM GD, GD."ARR" AS "V" GROUP BY "V"`, []string{
			"(V + 1)=2|COUNT(*)=2|N=2|VP=2|_0=2", "(V + 1)=3|COUNT(*)=2|N=2|VP=3|_0=3",
		})
	})

	t.Run("R20 computed projection over a SHADOWED grouped unnest key reads the element not the later column", func(t *testing.T) {
		// The shadowing variant: `FROM GD, GD.ARR AS V, GW GROUP BY V` where GW has
		// its OWN column V=999. The computed projection `V + 1` must read the unnest
		// ELEMENT (V.V), NOT GW.V (which mergeRows keys last-leg-wins into the bare
		// `V`). With the bug the qualified `V.V` reads NULL; a naive "use the bare V"
		// rebase WITHOUT the round-15 grouping would group by GW.V=999. The fix
		// groups by the element (round-16) AND rebases the projection to the bare
		// aggregate-output key — so V+1 is element+1, never 1000. Elements 1,2 →
		// computed 2,3, count 2 each. (Same full raw-executor key set as above:
		// `(V + 1)`, `_0`, `COUNT(*)`/`N`, `VP`.)
		assertRows(t, `SELECT "V" + 1 AS "VP", COUNT(*) AS "N" FROM GD, GD."ARR" AS "V", GW GROUP BY "V"`, []string{
			"(V + 1)=2|COUNT(*)=2|N=2|VP=2|_0=2", "(V + 1)=3|COUNT(*)=2|N=2|VP=3|_0=3",
		})
	})

	t.Run("R20 computed projection over a grouped unnest key on distinct elements", func(t *testing.T) {
		// Distinct-element variant over T1.ARR1 (101,201,202,203), each its own
		// group of count 1, so the computed `V + 1` is 102,202,203,204. Pins the
		// rebase across multiple groups and the bare element alongside the computed
		// column (the bare `V` projection still reads the element). Here `V` is a bare
		// FieldValue at slot 0 (no `_0`); the computed `V + 1` is at slot 1, so the
		// raw row carries the positional `_1` and the canonical `(V + 1)` alongside VP.
		assertRows(t, `SELECT "V", "V" + 1 AS "VP", COUNT(*) AS "N" FROM T1, T1."ARR1" AS "V" GROUP BY "V"`, []string{
			"(V + 1)=102|COUNT(*)=1|N=1|V=101|VP=102|_1=102",
			"(V + 1)=202|COUNT(*)=1|N=1|V=201|VP=202|_1=202",
			"(V + 1)=203|COUNT(*)=1|N=1|V=202|VP=203|_1=203",
			"(V + 1)=204|COUNT(*)=1|N=1|V=203|VP=204|_1=204",
		})
	})

	t.Run("R20 HAVING on a grouped unnest key filters groups by the element", func(t *testing.T) {
		// THE HAVING revert-proof: `... GROUP BY V HAVING V > 201`. HAVING is a
		// post-aggregate predicate; a reference to the grouped key `V` resolves
		// against the PRE-aggregate scope → qualified `V.V` → reads NULL off the
		// bare-V aggregate row → `NULL > 201` is false for EVERY group → all groups
		// dropped (got=[]). The fix rebases the HAVING `V.V` reference to the bare
		// aggregate-output `V`, so `V > 201` keeps the element groups 202,203 (101,
		// 201 filtered). T1.ARR1 distinct elements 101,201,202,203, each count 1.
		assertRows(t, `SELECT "V", COUNT(*) AS "N" FROM T1, T1."ARR1" AS "V" GROUP BY "V" HAVING "V" > 201`, []string{
			"COUNT(*)=1|N=1|V=202", "COUNT(*)=1|N=1|V=203",
		})
	})

	t.Run("R20 HAVING on a SHADOWED grouped unnest key filters by the element not the later column", func(t *testing.T) {
		// Shadowing HAVING: `FROM GD, GD.ARR AS V, GW GROUP BY V HAVING V > 1` where
		// GW.V=999 shadows the bare key. The HAVING `V` must filter on the unnest
		// ELEMENT (V.V), not GW.V (which would keep every group since 999 > 1). The
		// fix groups + filters by the element: elements 1,2 → `V > 1` keeps only the
		// element-2 group (count 2). With the bug HAVING read NULL → all dropped; a
		// bare-V-without-round-16 would group/filter on 999 (wrong group, all kept).
		assertRows(t, `SELECT "V", COUNT(*) AS "N" FROM GD, GD."ARR" AS "V", GW GROUP BY "V" HAVING "V" > 1`, []string{
			"COUNT(*)=2|N=2|V=2",
		})
	})

	t.Run("R20 HAVING COUNT(*) control still filters by the aggregate", func(t *testing.T) {
		// Control: a HAVING on the AGGREGATE (`COUNT(*) > 1`), not the group key,
		// must keep working — proving the group-key rebase did not disturb the
		// aggregate-reference HAVING path. GD.ARR={1,2},{1,2} → each element has
		// count 2, so `COUNT(*) > 1` keeps BOTH groups (1 and 2). With T1's distinct
		// elements every count is 1, so the same predicate there keeps none — here GD's
		// duplicates make it keep both, a non-trivial pass.
		assertRows(t, `SELECT "V", COUNT(*) AS "N" FROM GD, GD."ARR" AS "V" GROUP BY "V" HAVING COUNT(*) > 1`, []string{
			"COUNT(*)=2|N=2|V=1", "COUNT(*)=2|N=2|V=2",
		})
	})

	t.Run("R20 HAVING COUNT(*) control drops groups below the threshold", func(t *testing.T) {
		// The COUNT(*) HAVING genuinely discriminates: over T1.ARR1 every element is
		// distinct (count 1), so `HAVING COUNT(*) > 1` drops ALL groups (empty), the
		// complement of the GD case above. Pins that the aggregate HAVING is honoured
		// per group, not papered over.
		assertRows(t, `SELECT "V", COUNT(*) AS "N" FROM T1, T1."ARR1" AS "V" GROUP BY "V" HAVING COUNT(*) > 1`, nil)
	})

	t.Run("R20 HAVING on group key AND aggregate composes", func(t *testing.T) {
		// Both a group-key HAVING and an aggregate HAVING in one predicate:
		// `HAVING V > 0 AND COUNT(*) > 1` over GD (elements 1,2 each count 2). The
		// element filter `V > 0` keeps both, the `COUNT(*) > 1` keeps both → both
		// groups survive. Proves the rebased group-key reference composes with the
		// aggregate reference inside one AND predicate.
		assertRows(t, `SELECT "V", COUNT(*) AS "N" FROM GD, GD."ARR" AS "V" GROUP BY "V" HAVING "V" > 0 AND COUNT(*) > 1`, []string{
			"COUNT(*)=2|N=2|V=1", "COUNT(*)=2|N=2|V=2",
		})
	})

	t.Run("HAVING LIKE ESCAPE on a grouped unnest key preserves the escape rune", func(t *testing.T) {
		// THE escape-preservation revert-proof: `... GROUP BY V HAVING V LIKE
		// 'a!_%' ESCAPE '!' AND COUNT(*) > 0`. The `AND COUNT(*) > 0` references an
		// aggregate, so the HAVING STAYS ABOVE the GroupBy and flows through the
		// post-aggregate predicate rebase (rewriteAggregateRefsInPredicate +
		// rebaseHavingGroupKeyPredicate). When that rebase reconstructed a fresh
		// Comparison{Type, Operand} it DROPPED Comparison.Escape: `!_` lost its
		// escape and the pattern degraded to the unescaped `a_%`, whose `_`
		// wildcard matches ANY single char.
		//
		// GD.SARR distinct elements (each count 2): "a_b" (literal underscore at
		// position 2) and "axy". The escaped pattern matches ONLY "a_b"; the
		// unescaped degradation would ALSO match "axy" (the `_` wildcard accepts
		// the `x`). The fix copies the whole Comparison and replaces only the
		// rebased operand, preserving Escape — so exactly the "a_b" group survives.
		assertRows(t, `SELECT "V", COUNT(*) AS "N" FROM GD, GD."SARR" AS "V" GROUP BY "V" HAVING "V" LIKE 'a!_%' ESCAPE '!' AND COUNT(*) > 0`, []string{
			"COUNT(*)=2|N=2|V=a_b",
		})
	})

	t.Run("HAVING LIKE ESCAPE control: unescaped pattern matches both groups", func(t *testing.T) {
		// The complement of the escape test: the SAME pattern body WITHOUT ESCAPE
		// (`V LIKE 'a_%'`) treats `_` as a wildcard, so BOTH "a_b" and "axy" match.
		// Pins that the escaped vs unescaped patterns genuinely differ on this data
		// — without this control a buggy escape-dropping rebase would make the
		// escape test indistinguishable from the unescaped one.
		assertRows(t, `SELECT "V", COUNT(*) AS "N" FROM GD, GD."SARR" AS "V" GROUP BY "V" HAVING "V" LIKE 'a_%' AND COUNT(*) > 0`, []string{
			"COUNT(*)=2|N=2|V=a_b", "COUNT(*)=2|N=2|V=axy",
		})
	})

	t.Run("HAVING LIKE ESCAPE on a SHADOWED grouped unnest key preserves the escape", func(t *testing.T) {
		// Escape preservation crossed with the shadowing path (`FROM GD, GD.SARR AS
		// V, GW GROUP BY V HAVING V LIKE … ESCAPE … AND COUNT(*) > 0`): GW shadows
		// the BARE element key, so grouping is on the QUALIFIED V.V (round-15/16)
		// and the HAVING reference rebases through the SAME post-aggregate path.
		// Both the qualification AND the escape must survive: the group key is the
		// unnest element (not GW.V), and `a!_%` matches only "a_b". GW.V/O are
		// non-string scalars distinct from every element, never the grouped value.
		assertRows(t, `SELECT "V", COUNT(*) AS "N" FROM GD, GD."SARR" AS "V", GW GROUP BY "V" HAVING "V" LIKE 'a!_%' ESCAPE '!' AND COUNT(*) > 0`, []string{
			"COUNT(*)=2|N=2|V=a_b",
		})
	})

	t.Run("WHERE LIKE ESCAPE on an unnest element preserves the escape (push-down rebase)", func(t *testing.T) {
		// The WHERE twin: `FROM GD, GD.SARR AS V WHERE V LIKE 'a!_%' ESCAPE '!'`.
		// The element filter is rewritten (rewriteUnnestPredicate →
		// mapPredicateValues) and folded into the inner Explode's PredicatesFilter,
		// and the planner's RebasePredicate / replacePredicateValues passes run over
		// it. Each of those previously rebuilt the Comparison with a partial field
		// set; the escape must survive the whole pipeline. Only "a_b" matches the
		// escaped pattern; the rows are the per-(outer-row × element) flow (each of
		// the 2 GD rows contributes one "a_b").
		plan := assertRows(t, `SELECT "ID", "V" FROM GD, GD."SARR" AS "V" WHERE "V" LIKE 'a!_%' ESCAPE '!'`, []string{
			"ID=1|V=a_b", "ID=2|V=a_b",
		})
		unnestMustContain(t, plan, "PredicatesFilter")
	})

	// --- probe round 25 P2b: `SELECT *` over a lateral unnest must include the
	// unnested element (and, under AT, the ordinal) in the RESULT-SET COLUMN
	// metadata. The unnest lowers to FlatMap(outer, Explode) whose result value is a
	// source-anchored join record carrying the outer columns + element [+ ordinal];
	// but deriveColumnsFromFlatMap fell through to MERGING the outer scan's columns
	// with the inner Explode's — and the Explode has NO derivable record columns, so
	// the element V (and ordinal O) were DROPPED from the column set. `SELECT *` thus
	// advertised only the outer columns, omitting the unnest binding. The per-ROW
	// datum still carried V (RecordConstructorValue.Evaluate writes the bare key), so
	// the bug lived ONLY in the column metadata — assertColumns (rows.Columns()'s
	// derivation) is the revert-proof axis. The fix derives `SELECT *` columns from
	// the result value's bare fields, so the set matches the explicit `SELECT id, V
	// [, O]` projection. RFC-142.

	t.Run("R25 P2b SELECT star over an unnest includes the element column", func(t *testing.T) {
		// `SELECT * FROM T1, T1.ARR1 AS VAL` — the column set is T1's columns PLUS the
		// element VAL. Pre-fix: [ID ARR1 ARR1_NN STRARR] (no VAL). The element value
		// flows in the row datum too (proven by the explicit projection control). The
		// star column set must equal the outer columns followed by VAL.
		assertColumns(t, `SELECT * FROM T1, T1."ARR1" AS "VAL"`,
			[]string{"ID", "ARR1", "ARR1_NN", "STRARR", "VAL"})
		// The explicit `SELECT id, VAL` projection is the value control: VAL carries
		// the unnested element. `SELECT *` carries the SAME element value in its row
		// datum (the bug was metadata-only), so the explicit projection's rows pin the
		// values the star row also exposes under VAL.
		assertRows(t, `SELECT "ID", "VAL" FROM T1, T1."ARR1" AS "VAL"`, []string{
			"ID=1|VAL=101", "ID=2|VAL=201", "ID=2|VAL=202", "ID=2|VAL=203",
		})
	})

	t.Run("R25 P2b SELECT star over an unnest WITH ORDINALITY includes element and ordinal", func(t *testing.T) {
		// `SELECT * FROM T1, T1.ARR1 AS VAL AT AT` — the column set is T1's columns
		// PLUS the element VAL AND the ordinal AT. Pre-fix both VAL and AT were
		// dropped. The explicit `SELECT id, VAL, AT` is the value control (1-based AT
		// resetting per outer row).
		assertColumns(t, `SELECT * FROM T1, T1."ARR1" AS "VAL" AT "AT"`,
			[]string{"ID", "ARR1", "ARR1_NN", "STRARR", "VAL", "AT"})
		assertRows(t, `SELECT "ID", "VAL", "AT" FROM T1, T1."ARR1" AS "VAL" AT "AT"`, []string{
			"AT=1|ID=1|VAL=101", "AT=1|ID=2|VAL=201", "AT=2|ID=2|VAL=202", "AT=3|ID=2|VAL=203",
		})
	})

	t.Run("R25 P2b SELECT star over an aliasless unnest includes the field-name element", func(t *testing.T) {
		// The aliasless form (`FROM T1, T1.ARR1`, no AS) binds the element to the
		// array FIELD NAME (ARR1) — a same-named element shadows the outer ARR1
		// column. `SELECT *` must still expose that element column (the unnest's
		// binding wins the bare ARR1 key). The column set is the outer columns with
		// ARR1 present exactly once (the element binding). Pre-fix the element was
		// dropped entirely. RFC-142.
		assertColumns(t, `SELECT * FROM T1, T1."ARR1"`,
			[]string{"ID", "ARR1_NN", "STRARR", "ARR1"})
	})

	// --- R31 P2a: a NO-alias schema-qualified comma source `s.EXB` inside a
	// catalog-built SUBQUERY. The subquery's logical tree is built FIRST and its
	// LogicalScan aliased from the parser source (`S.EXB`); normalizeSchemaQualified
	// SelectSources then strips the parser's source alias to `EXB`. If normalize runs
	// AFTER the build, the built scan keeps alias `S.EXB` while the subquery SCOPE
	// (reading the normalized source) resolves `EXB.ID` to QOV(EXB) — so at execution
	// the scan binds under `S.EXB` and `EXB.ID` reads NULL, making `EXB.ID = EXA.ID`
	// false for every pair → EXISTS FALSE → every outer row silently DROPPED. The fix
	// runs normalize BEFORE the build so the scan carries the SAME `EXB` alias the
	// resolver uses and the predicate resolves. The session schema is the default `s`
	// (PlanRecordQueryWithMetadata), so `s.EXB` is the schema-qualified real table EXB.
	// EXA={100,200} and EXB={200,300} OVERLAP on 200, so a working `EXB.ID = EXA.ID`
	// finds the pair (200,200) → EXISTS TRUE → every T1 row kept. The non-empty result
	// is the revert sentinel (pre-fix: empty). RFC-142.
	t.Run("R31 P2a no-alias schema-qualified subquery bare table-name predicate reads the real column", func(t *testing.T) {
		// The REVERT SENTINEL: `EXB.ID = 200` — a bare table-name reference to the
		// no-alias schema-qualified source `s.EXB` compared to a constant EXB genuinely
		// has (200). With the fix the scan binds under the SAME `EXB` alias the resolver
		// uses → `EXB.ID` reads 200 → the existential holds → all four T1 rows. Pre-fix
		// the scan binds under `S.EXB` while the resolver uses `EXB` → `EXB.ID` reads
		// NULL → `NULL = 200` false → EXISTS FALSE → ALL outer rows silently DROPPED
		// (empty). The non-empty result fails on revert. The complement `EXB.ID = 999`
		// (no EXB row) → empty proves the predicate is not unconditionally satisfied.
		// The EXISTS subquery plans through the catalog SELECT builder where the bug
		// lived. RFC-142.
		assertRows(t, `SELECT "ID" FROM T1 WHERE EXISTS (SELECT 1 FROM EXA, "s"."EXB" WHERE "EXB"."ID" = 200)`, []string{
			"ID=0", "ID=1", "ID=2", "ID=3",
		})
		assertRows(t, `SELECT "ID" FROM T1 WHERE EXISTS (SELECT 1 FROM EXA, "s"."EXB" WHERE "EXB"."ID" = 999)`, nil)
	})

	t.Run("R31 P2a no-alias schema-qualified subquery cross-leg predicate (PB.ID = PA.ID shape)", func(t *testing.T) {
		// The prompt's exact `EXB.ID = EXA.ID` shape, with EXA={100,200}/EXB={200,300}
		// overlapping on 200 → the satisfying pair (200,200) → EXISTS TRUE → all four T1
		// rows. With the fix the scan binds the SAME `EXB` alias the resolver uses so
		// `EXB.ID` reads the real value and the predicate filters correctly. (The
		// constant-compare subtest above is the load-bearing revert sentinel; this
		// cross-leg form documents the prompt's canonical shape resolving correctly.)
		// RFC-142.
		assertRows(t, `SELECT "ID" FROM T1 WHERE EXISTS (SELECT 1 FROM EXA, "s"."EXB" WHERE "EXB"."ID" = "EXA"."ID")`, []string{
			"ID=0", "ID=1", "ID=2", "ID=3",
		})
		// Disjoint-id companion (`EXB.ID = EXA.ID + 1000` never matches) → empty, proving
		// the cross-leg predicate genuinely filters and is not unconditionally true.
		assertRows(t, `SELECT "ID" FROM T1 WHERE EXISTS (SELECT 1 FROM EXA, "s"."EXB" WHERE "EXB"."ID" = "EXA"."ID" + 1000)`, nil)
	})

	t.Run("R31 P2a schema-qualified subquery with an aliased sibling still correct (control)", func(t *testing.T) {
		// Control: the SAME constant-compare shape but with an ALIASED schema-qualified
		// source (`s.EXB AS B`) resolving the predicate through the explicit alias `B`.
		// The aliased path keeps j.alias = "B" (≠ tableName) so the scan binds `B` both
		// pre- and post-fix; this control proves the no-alias fix does not perturb the
		// already-correct aliased case. `B.ID = 200` → all four T1 rows. RFC-142.
		assertRows(t, `SELECT "ID" FROM T1 WHERE EXISTS (SELECT 1 FROM EXA, "s"."EXB" AS "B" WHERE "B"."ID" = 200)`, []string{
			"ID=0", "ID=1", "ID=2", "ID=3",
		})
	})
}

// TestFDB_ArrayUnnestOrdinalityColumnType is the RFC-142
// revert-proof guard for the AT-ordinal COLUMN TYPE metadata. The unnest WITH
// ORDINALITY ordinal is Java's `Type.primitiveType(INT, false)` — a 1-based,
// NON-NULL INT — and a query that PROJECTS or COMPUTES from the AT alias must
// report that INT type, NOT UNKNOWN.
//
// The bug: the AT virtual column was registered with the type string "INTEGER",
// but sqlTypeToCascadesType only recognized "INT" (mapping "INTEGER" → UNKNOWN).
// So the semantic resolver assigned values.TypeUnknown to the ordinal FieldValue,
// and that UNKNOWN flowed into the projected/computed column's type tag (and
// thence the result-set column metadata, via valueTypeName → ""). The runtime
// VALUE was a correct integer the whole time; only the TYPE was wrong — the
// dimension the row-comparison subtests never probed.
//
// The fix (both root sites): (1) the registration site uses the recognized
// non-null spelling "INT NOT NULL"; (2) sqlTypeToCascadesType recognizes
// "INTEGER" as a synonym for "INT" and "INT NOT NULL"/"INTEGER NOT NULL" → the
// values.NotNullInt singleton. This asserts on the planned VALUE TYPE — the exact
// bug dimension — and is reachable from the metadata-only planner harness (no
// Docker), so it guards even without a container.
func TestFDB_ArrayUnnestOrdinalityColumnType(t *testing.T) {
	t.Parallel()
	b := metadata.NewSchemaTemplateBuilder().SetName("ajt_coltype")
	b.AddTable("T1", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("ARR1", api.NewArrayType(api.NewIntegerType(false), true), 2),
		// STRARR drives the R26 P2b NON-ORDINAL element column-TYPE check: a STRING
		// array whose element column must report STRING, not the UnknownType→BIGINT
		// fallback the bare QuantifiedObjectValue element used to advertise.
		metadata.NewColumnSpec("STRARR", api.NewArrayType(api.NewStringType(false), true), 3),
	}, []string{"ID"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	md := tmpl.Underlying()

	// colTypesOf plans `sql` and returns the result-set column TYPE NAMES — the
	// no-FDB analog of the driver's column-type metadata (deriveColumnsFromPlan →
	// ColumnDef.TypeName, the SAME derivation the live Execute() path uses).
	colTypesOf := func(t *testing.T, sql string) []string {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
		if perr != nil {
			t.Fatalf("plan %q: %v", sql, perr)
		}
		return embedded.ResultColumnTypesForPlan(plan, md)
	}
	colLabelsOf := func(t *testing.T, sql string) []string {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
		if perr != nil {
			t.Fatalf("plan %q: %v", sql, perr)
		}
		return embedded.ResultColumnLabelsForPlan(plan, md)
	}

	// atProjType plans `sql` and returns the planned VALUE for the projection
	// whose user-facing label is `wantLabel` (the bare leaf of the projection's
	// FieldValue / the explicit alias). It walks down through the top Projection
	// plan's parallel projection/alias lists.
	atProjValue := func(t *testing.T, sql, wantField string) values.Value {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
		if perr != nil {
			t.Fatalf("plan %q: %v", sql, perr)
		}
		proj, ok := plan.(*plans.RecordQueryProjectionPlan)
		if !ok {
			t.Fatalf("plan %q: root is %T, want *RecordQueryProjectionPlan\n%s", sql, plan, plan.Explain())
		}
		projections := proj.GetProjections()
		for _, pv := range projections {
			fv, ok := pv.(*values.FieldValue)
			if ok && strings.EqualFold(fv.Field, wantField) {
				return pv
			}
		}
		// Fall back: if a single projection, return it (the AT-only / computed shapes).
		if len(projections) == 1 {
			return projections[0]
		}
		t.Fatalf("plan %q: no projection with field %q found\n%s", sql, wantField, plan.Explain())
		return nil
	}

	t.Run("AT projection type is non-null INT not UNKNOWN", func(t *testing.T) {
		// `SELECT id, V, ord` projecting the AT ordinal alias `AT`. The ordinal
		// projection's planned VALUE TYPE must be a NON-NULL INT — exactly Java's
		// Type.primitiveType(INT, false) — NOT UNKNOWN. Before the fix the resolver
		// assigned values.TypeUnknown (the "INTEGER"→UNKNOWN mapper miss), so this
		// asserts the precise bug dimension.
		v := atProjValue(t, `SELECT "ID", "VAL", "AT" FROM T1, T1."ARR1" AS "VAL" AT "AT"`, "AT")
		typ := v.Type()
		if typ == nil {
			t.Fatalf("AT projection has nil type")
		}
		if typ.Code() != values.TypeCodeInt {
			t.Fatalf("AT projection type code = %v (%s), want TypeCodeInt — UNKNOWN means the ordinal column type regressed", typ.Code(), typ.String())
		}
		if typ.IsNullable() {
			t.Fatalf("AT projection type = %s, want NON-NULL (Java: INT NOT NULL ordinal)", typ.String())
		}
		// Pin the exact singleton the translator's ordinal FieldValue uses, so the
		// resolver path and the translator path cannot drift.
		if !typ.Equals(values.NotNullInt) {
			t.Fatalf("AT projection type = %s, want %s (values.NotNullInt)", typ.String(), values.NotNullInt.String())
		}
	})

	t.Run("AT-only projection type is non-null INT", func(t *testing.T) {
		// AT-only form (no AS): `SELECT id, ord FROM t, t.arr AT ord`. The single
		// AT projection must still be the non-null INT.
		v := atProjValue(t, `SELECT "ID", "AT" FROM T1, T1."ARR1" AT "AT"`, "AT")
		if v.Type() == nil || v.Type().Code() != values.TypeCodeInt || v.Type().IsNullable() {
			t.Fatalf("AT-only ordinal type = %v, want non-null INT", v.Type())
		}
	})

	// R26 P2b (wrong metadata type): a NON-ordinality unnest's element column was
	// the bare QuantifiedObjectValue, whose type is UnknownType, so
	// deriveColumnsFromFlatMap → foldedColumnDef fell back to BIGINT. A STRING
	// array's element therefore advertised BIGINT even though every row is a string.
	// The fix types the element QOV to the array's elementType (matching the
	// WITH-ORDINALITY path, which already preserved it via NewOrdinalFieldValue), so
	// `SELECT *` reports the real element type. The SELECT-* path reaches
	// deriveColumnsFromFlatMap (no projection plan above the FlatMap), the exact site
	// of the bug. RFC-142.
	t.Run("R26 P2b SELECT star non-ordinal element over a STRING array reports STRING not BIGINT", func(t *testing.T) {
		labels := colLabelsOf(t, `SELECT * FROM T1, T1."STRARR" AS "VAL"`)
		types := colTypesOf(t, `SELECT * FROM T1, T1."STRARR" AS "VAL"`)
		// Columns: T1's ID(BIGINT), ARR1, STRARR, then the element VAL — typed STRING.
		wantLabels := []string{"ID", "ARR1", "STRARR", "VAL"}
		if fmt.Sprintf("%v", labels) != fmt.Sprintf("%v", wantLabels) {
			t.Fatalf("labels = %v, want %v", labels, wantLabels)
		}
		// The element column VAL is the LAST one; assert it is STRING (pre-fix BIGINT).
		valType := types[len(types)-1]
		if valType != "STRING" {
			t.Fatalf("element VAL type = %q, want STRING (BIGINT means the non-ordinal element type regressed); all types=%v", valType, types)
		}
	})

	t.Run("R26 P2b SELECT star non-ordinal element over an INT array reports INTEGER", func(t *testing.T) {
		// The INT-array control: the element of an INTEGER array reports INTEGER. This
		// distinguishes the fix from a hard-coded STRING — the element type tracks the
		// array's elementType. (BIGINT happened to coincide with the fallback for INT
		// arrays pre-fix; INTEGER is the correct element type and is the post-fix value.)
		types := colTypesOf(t, `SELECT * FROM T1, T1."ARR1" AS "VAL"`)
		valType := types[len(types)-1]
		if valType != "INTEGER" {
			t.Fatalf("INT-array element VAL type = %q, want INTEGER; all types=%v", valType, types)
		}
	})

	t.Run("R26 P2b SELECT star WITH ORDINALITY over a STRING array reports STRING element + INTEGER ordinal", func(t *testing.T) {
		// The WITH-ORDINALITY companion: the element VAL is STRING and the ordinal O is
		// INTEGER. The ordinality path already preserved elementType (via
		// NewOrdinalFieldValue); this pins that the non-ordinal fix did not break it and
		// that both paths now agree on the element type. RFC-142.
		labels := colLabelsOf(t, `SELECT * FROM T1, T1."STRARR" AS "VAL" AT "O"`)
		types := colTypesOf(t, `SELECT * FROM T1, T1."STRARR" AS "VAL" AT "O"`)
		wantLabels := []string{"ID", "ARR1", "STRARR", "VAL", "O"}
		if fmt.Sprintf("%v", labels) != fmt.Sprintf("%v", wantLabels) {
			t.Fatalf("labels = %v, want %v", labels, wantLabels)
		}
		// VAL is second-to-last (STRING), O is last (INTEGER, the 1-based ordinal).
		if got := types[len(types)-2]; got != "STRING" {
			t.Fatalf("ordinality element VAL type = %q, want STRING; all types=%v", got, types)
		}
		if got := types[len(types)-1]; got != "INTEGER" {
			t.Fatalf("ordinality ordinal O type = %q, want INTEGER; all types=%v", got, types)
		}
	})

	// R31 P2b (metadata nullability): the synthesized WITH-ORDINALITY ordinal column
	// is Java's Type.primitiveType(INT, false) — INT NOT NULL — but it has NO backing
	// proto descriptor field, so foldedColumnDef's colDesc-only path defaulted it to
	// ColumnNullable and the result-set metadata wrongly reported the ordinal as
	// nullable. The fix derives the column's nullability from the VALUE's own type
	// when no descriptor resolves, so a NOT-NULL synthesized value (the ordinal,
	// values.NotNullInt) reports ColumnNoNulls while a genuinely nullable synthesized
	// value (the array element, whose type the engine reads from the proto descriptor
	// as nullable) still reports ColumnNullable. Both the ordinal AND the element are
	// descriptor-less synthesized columns, so the SAME-query assertion below
	// distinguishes the Value-type-derived fix from BOTH the pre-fix blanket default
	// (every no-descriptor column ColumnNullable → ordinal wrongly nullable) AND a
	// naive blanket override (every no-descriptor column ColumnNoNulls → element
	// wrongly NOT NULL). Asserted via the SAME production derivation
	// (deriveColumnsFromPlan → ColumnDef.Nullable) the driver's
	// ResultSetMetaData.isNullable uses. RFC-142.
	colNullsOf := func(t *testing.T, sql string) (labels []string, nulls []int) {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
		if perr != nil {
			t.Fatalf("plan %q: %v", sql, perr)
		}
		return embedded.ResultColumnLabelsForPlan(plan, md),
			embedded.ResultColumnNullabilityForPlan(plan, md)
	}

	t.Run("R31 P2b ordinal reports NOT NULL while the element stays NULLABLE in the same query", func(t *testing.T) {
		// `SELECT * FROM T1, T1.ARR1 AS V AT O`. Columns: ID (BIGINT NOT NULL pk), ARR1
		// (array), V (the element), O (the 1-based ordinal). V and O are BOTH synthesized
		// values with no backing descriptor field. The ordinal O must report NOT NULL
		// (values.NotNullInt — Java's INT NOT NULL ordinal); the element V must report
		// NULLABLE (the engine types a scalar array element nullable). This single query
		// is the precise revert dimension: pre-fix BOTH defaulted to nullable (O wrong); a
		// naive blanket "no-descriptor → NOT NULL" would make BOTH not-null (V wrong);
		// only the Value-type-derived fix gives O=NOT NULL, V=NULLABLE. RFC-142.
		labels, nulls := colNullsOf(t, `SELECT * FROM T1, T1."ARR1" AS "V" AT "O"`)
		wantLabels := []string{"ID", "ARR1", "STRARR", "V", "O"}
		if fmt.Sprintf("%v", labels) != fmt.Sprintf("%v", wantLabels) {
			t.Fatalf("labels = %v, want %v", labels, wantLabels)
		}
		// O (last) must be NOT NULL — the precise revert dimension for the ordinal.
		if got := nulls[len(nulls)-1]; got != api.ColumnNoNulls {
			t.Fatalf("ordinal O nullability = %d, want %d (ColumnNoNulls); pre-fix the synthesized ordinal defaulted to ColumnNullable; all=%v",
				got, api.ColumnNoNulls, nulls)
		}
		// V (second-to-last) must stay NULLABLE — guards against a blanket NOT-NULL
		// override (which would make every descriptor-less column NOT NULL).
		if got := nulls[len(nulls)-2]; got != api.ColumnNullable {
			t.Fatalf("element V nullability = %d, want %d (ColumnNullable); a blanket no-descriptor→NOT-NULL fix would regress this; all=%v",
				got, api.ColumnNullable, nulls)
		}
		// The PK ID stays NOT NULL via its real descriptor (Required) — proves the
		// descriptor-backed path is untouched by the no-descriptor branch.
		if got := nulls[0]; got != api.ColumnNoNulls {
			t.Fatalf("pk ID nullability = %d, want %d (ColumnNoNulls); all=%v", got, api.ColumnNoNulls, nulls)
		}
	})

	t.Run("R31 P2b AT-only ordinal reports NOT NULL", func(t *testing.T) {
		// AT-only (no AS): `SELECT * FROM T1, T1.ARR1 AT O`. The single synthesized
		// ordinal column O must still report NOT NULL. RFC-142.
		_, nulls := colNullsOf(t, `SELECT * FROM T1, T1."ARR1" AT "O"`)
		if got := nulls[len(nulls)-1]; got != api.ColumnNoNulls {
			t.Fatalf("AT-only ordinal O nullability = %d, want %d (ColumnNoNulls); all=%v",
				got, api.ColumnNoNulls, nulls)
		}
	})
}

func assertRejected(t *testing.T, md *recordlayer.RecordMetaData, sql string, want api.ErrorCode) {
	t.Helper()
	_, err := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
	if err == nil {
		t.Fatalf("query %q: expected rejection %v, got nil", sql, want)
	}
	apiErr := asAPIError(err)
	if apiErr == nil || apiErr.Code != want {
		t.Fatalf("query %q: err = %v (%T), want code %v", sql, err, err, want)
	}
}

func unnestMustContain(t *testing.T, plan, substr string) {
	t.Helper()
	if !strings.Contains(plan, substr) {
		t.Fatalf("plan should contain %q:\n%s", substr, plan)
	}
}

func unnestMustNotContain(t *testing.T, plan, substr string) {
	t.Helper()
	if strings.Contains(plan, substr) {
		t.Fatalf("plan should NOT contain %q:\n%s", substr, plan)
	}
}

func unnestSprint(v any) string {
	switch x := v.(type) {
	case nil:
		return "<nil>"
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}

func unnestEqualStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestFDB_ArrayUnnestDMLNonDefaultSchema drives the REAL live DML path (planDML
// in cascades_generator.go, g.c.sess.Schema) under a NON-DEFAULT session schema
// `main`, pinning that a schema-qualified comma source inside a DML SELECT/WHERE
// classifies against the ACTIVE schema — not the hardcoded default `s`.
//
// The discriminating shape is `FROM PA AS main, main.PB AS B`: the prior source
// PA is aliased `main`, which equals the session schema name, so `main.PB` is
// BOTH "field PB on source main" AND "schema-qualified table PB". Java resolves
// the TABLE first (newUnnestTableResolver: qualifier == active schema AND PB
// exists). PA has NO column PB, so the pre-fix code — which planned the DML
// SELECT-source rebuild / WHERE-EXISTS subquery with the hardcoded default `s` —
// left `main.PB` a correlated LogicalUnnest (qualifier `main` != `s`), which
// resolveQualifiedTableNames cannot repair → the DML FAILS (`column PB missing`
// / translation failure). With the session schema threaded through the DML
// rebuild helpers, `main.PB` is the real cross-join table and the DML affects the
// correct rows. RFC-142.
func TestFDB_ArrayUnnestDMLNonDefaultSchema(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/ajt_dml_nds"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	// PA (the source aliased `main`, == schema name); PB (the real
	// schema-qualified table); DST/USRC (DML targets). All single-schema, plain
	// scalar columns so rows can be seeded via SQL VALUES.
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE ajt_dml_nds_tmpl"+
		" CREATE TABLE PA (id BIGINT NOT NULL, k BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE PB (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE DST (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE DST2 (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE USRC (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE ajt_dml_nds_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	// Session schema = `main`, the NON-default schema (default is `s`).
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// PA has ONE row (k=1) so the PA×PB cross product is exactly PB's rows.
	if _, err := db.ExecContext(ctx, "INSERT INTO PA VALUES (1, 1)"); err != nil {
		t.Fatalf("seed PA: %v", err)
	}
	// PB has three rows — the REAL table the schema-qualified source must read.
	if _, err := db.ExecContext(ctx, "INSERT INTO PB VALUES (10, 100), (20, 200), (30, 300)"); err != nil {
		t.Fatalf("seed PB: %v", err)
	}

	t.Run("INSERT...SELECT schema-qualified comma source", func(t *testing.T) {
		// `INSERT INTO DST SELECT B.id, B.v FROM PA AS main, main.PB AS B WHERE B.v >=
		// 200`. PA is aliased `main` (== session schema name), so main.PB is BOTH
		// "field PB on source main" AND the schema-qualified TABLE PB. The active
		// schema `main` makes the TABLE branch win: PA(1 row)×PB filtered to B.v>=200
		// → PB rows 20 and 30 insert into DST. The WHERE on B's own column proves B
		// resolves to the REAL table PB (its v column), not a correlated unnest.
		// Pre-fix main.PB stayed a LogicalUnnest over the non-existent PA.PB column
		// (classified against the hardcoded `s`) → the INSERT FAILS. RFC-142.
		res, err := db.ExecContext(ctx,
			`INSERT INTO DST SELECT "B"."ID", "B"."V" FROM PA AS "main", "main"."PB" AS "B" WHERE "B"."V" >= 200`)
		if err != nil {
			t.Fatalf("INSERT...SELECT (schema main): %v", err)
		}
		if n, _ := res.RowsAffected(); n != 2 {
			t.Fatalf("INSERT...SELECT RowsAffected = %d, want 2", n)
		}
		// Persisted rows are EXACTLY PB's v>=200 rows (NOT PA.PB unnest, NOT empty).
		got := map[int64]int64{}
		rows, qerr := db.QueryContext(ctx, "SELECT id, v FROM DST ORDER BY id")
		if qerr != nil {
			t.Fatalf("read back DST: %v", qerr)
		}
		for rows.Next() {
			var id, v int64
			if sErr := rows.Scan(&id, &v); sErr != nil {
				t.Fatalf("scan: %v", sErr)
			}
			got[id] = v
		}
		rows.Close()
		want := map[int64]int64{20: 200, 30: 300}
		if len(got) != len(want) || got[20] != 200 || got[30] != 300 {
			t.Fatalf("DST after INSERT...SELECT = %v, want %v", got, want)
		}
	})

	t.Run("DELETE WHERE EXISTS schema-qualified comma source", func(t *testing.T) {
		// The UPDATE/DELETE rebuild reaches a schema-qualified comma source only
		// through a WHERE-EXISTS subquery (the single-table DML FROM has no comma
		// source). Seed USRC, then `DELETE FROM USRC WHERE EXISTS (SELECT 1 FROM PA
		// AS main, main.PB AS B WHERE B.id = 10)`: the existential's subquery cross
		// joins PA×PB (B.id=10 matches one PB row) → EXISTS is TRUE → every USRC row
		// is deleted. Pre-fix the subquery's main.PB was classified against `s` →
		// LogicalUnnest over PA.PB → the DELETE FAILS. RFC-142.
		if _, err := db.ExecContext(ctx, "INSERT INTO USRC VALUES (1, 11), (2, 22)"); err != nil {
			t.Fatalf("seed USRC: %v", err)
		}
		res, err := db.ExecContext(ctx,
			`DELETE FROM USRC WHERE EXISTS (SELECT 1 FROM PA AS "main", "main"."PB" AS "B" WHERE "B"."ID" = 10)`)
		if err != nil {
			t.Fatalf("DELETE WHERE EXISTS (schema main): %v", err)
		}
		if n, _ := res.RowsAffected(); n != 2 {
			t.Fatalf("DELETE RowsAffected = %d, want 2", n)
		}
		var cnt int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM USRC").Scan(&cnt); err != nil {
			t.Fatalf("count USRC: %v", err)
		}
		if cnt != 0 {
			t.Fatalf("USRC count after DELETE = %d, want 0", cnt)
		}
	})

	t.Run("DELETE WHERE EXISTS schema-qualified source no match keeps rows", func(t *testing.T) {
		// Control: the subquery is a genuine cross join evaluated under `main`, not
		// silently always-true. `B.id = 99` matches NO PB row → EXISTS is FALSE →
		// the DELETE removes ZERO rows. A pre-fix mis-classified subquery would
		// FAIL (translation error), not return a clean zero-affected result. RFC-142.
		if _, err := db.ExecContext(ctx, "INSERT INTO USRC VALUES (3, 33)"); err != nil {
			t.Fatalf("seed USRC (control): %v", err)
		}
		res, err := db.ExecContext(ctx,
			`DELETE FROM USRC WHERE EXISTS (SELECT 1 FROM PA AS "main", "main"."PB" AS "B" WHERE "B"."ID" = 99)`)
		if err != nil {
			t.Fatalf("DELETE WHERE EXISTS no-match (schema main): %v", err)
		}
		if n, _ := res.RowsAffected(); n != 0 {
			t.Fatalf("DELETE no-match RowsAffected = %d, want 0", n)
		}
		var cnt int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM USRC").Scan(&cnt); err != nil {
			t.Fatalf("count USRC (control): %v", err)
		}
		if cnt != 1 {
			t.Fatalf("USRC count after no-match DELETE = %d, want 1", cnt)
		}
	})

	t.Run("DELETE WHERE EXISTS with AT-on-table subquery is WRONG_OBJECT_TYPE", func(t *testing.T) {
		// The DML axis of the masking bug: a DML WHERE-EXISTS subquery whose FROM has an
		// AT-on-a-TABLE source (`PB AT O`, PB a real table) AND whose OWN predicate
		// references that table (`PB.id = 10`). The DML WHERE-EXISTS rebuild
		// (upgradeDMLWhereWithCatalog) plans the subquery through the SAME catalog SELECT
		// builder (existsSubqueryPlanner.BuildExists). Without the early rejection the AT
		// shortcut registers a virtual unnest binding shadowing PB, `PB.id` fails to
		// resolve with a MASKING UNDEFINED_COLUMN (42703), and the DELETE is rejected on
		// the wrong code. With the fix the early AT-on-table rejection fires on the
		// inner FROM tree first → WRONG_OBJECT_TYPE (42809). Revert-proof on 42809.
		// RFC-142.
		_, err := db.ExecContext(ctx,
			`DELETE FROM USRC WHERE EXISTS (SELECT 1 FROM PA, "PB" AT "O" WHERE "PB"."ID" = 10)`)
		if err == nil {
			t.Fatalf("DELETE WHERE EXISTS AT-on-table: expected rejection, got nil")
		}
		requireSQLSTATE(t, err, api.ErrCodeWrongObjectType)
	})

	t.Run("INSERT...SELECT with AT-on-table in the SELECT body is WRONG_OBJECT_TYPE", func(t *testing.T) {
		// The INSERT…SELECT axis: the SELECT source body carries an AT-on-table comma
		// source (`PB AT O`) whose own WHERE references PB. The INSERT source rebuild
		// (buildLogicalPlanForInsertWithCatalog → buildLogicalPlanForSelectWithCatalog →
		// the same catalog SELECT builder) must surface 42809, not the masked 42703.
		// RFC-142.
		_, err := db.ExecContext(ctx,
			`INSERT INTO DST SELECT "PB"."ID", "PB"."V" FROM PA, "PB" AT "O" WHERE "PB"."ID" = 10`)
		if err == nil {
			t.Fatalf("INSERT...SELECT AT-on-table: expected rejection, got nil")
		}
		requireSQLSTATE(t, err, api.ErrCodeWrongObjectType)
	})

	t.Run("control: DELETE WHERE EXISTS with a genuine subquery affects rows", func(t *testing.T) {
		// The discriminating control: the SAME DELETE shape but with a genuine (non-AT)
		// subquery that matches — proving the early AT rejection declines ONLY the
		// AT-on-table case and a real DML WHERE-EXISTS still runs. Seed one USRC row,
		// then delete only THAT row via a per-row predicate ANDed with the genuine
		// non-correlated `EXISTS (SELECT 1 FROM PA, PB WHERE PB.id = 10)` (TRUE: one PB
		// row id=10). The `id = 707` clause isolates this subtest from any rows other
		// DELETE subtests left behind. RFC-142.
		if _, err := db.ExecContext(ctx, "INSERT INTO USRC VALUES (707, 77)"); err != nil {
			t.Fatalf("seed USRC (control): %v", err)
		}
		res, err := db.ExecContext(ctx,
			`DELETE FROM USRC WHERE "ID" = 707 AND EXISTS (SELECT 1 FROM PA, "PB" WHERE "PB"."ID" = 10)`)
		if err != nil {
			t.Fatalf("DELETE WHERE EXISTS genuine subquery: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Fatalf("DELETE genuine RowsAffected = %d, want 1", n)
		}
	})

	t.Run("INSERT...SELECT qualified-star schema-qualified comma source", func(t *testing.T) {
		// REVERT-PROOF for the segments-vs-tableName normalization bug. A qualified
		// star `SELECT B.*` (projQualifier set, projCols nil) forces the catalog
		// SELECT builder to REBUILD the logical plan (buildLogicalPlanForSelect, no
		// metadata in scope) AFTER normalizeSchemaQualifiedSelectSources has stripped
		// the schema qualifier off `main.PB` → `PB` on j.tableName. The rebuild's
		// lateral-unnest classifier reads j.segments, NOT j.tableName. If the strip
		// does not ALSO drop the leading schema segment, j.segments stays
		// ['main','PB'] — and since PA is aliased `main` (a VISIBLE FROM alias),
		// the metadata-less rebuild classifier sees segment 0 `main` as a source
		// alias and reclassifies the REAL table main.PB as Unnest(MAIN.PB AS B). The
		// INSERT then fails / reads the wrong source. With segments normalized in
		// lockstep (['main','PB'] → ['PB']), main.PB stays the real cross-join table:
		// PA(1 row)×PB → B.* yields PB's three rows (id, v) into DST2. RFC-142.
		res, err := db.ExecContext(ctx,
			`INSERT INTO DST2 SELECT "B".* FROM PA AS "main", "main"."PB" AS "B"`)
		if err != nil {
			t.Fatalf("INSERT...SELECT B.* (schema main): %v", err)
		}
		if n, _ := res.RowsAffected(); n != 3 {
			t.Fatalf("INSERT...SELECT B.* RowsAffected = %d, want 3", n)
		}
		// Persisted rows are EXACTLY PB's rows — NOT a PA.PB unnest (which has no
		// such column → 0 rows / failure), NOT PA's rows.
		got := map[int64]int64{}
		rows, qerr := db.QueryContext(ctx, "SELECT id, v FROM DST2 ORDER BY id")
		if qerr != nil {
			t.Fatalf("read back DST2: %v", qerr)
		}
		for rows.Next() {
			var id, v int64
			if sErr := rows.Scan(&id, &v); sErr != nil {
				t.Fatalf("scan: %v", sErr)
			}
			got[id] = v
		}
		rows.Close()
		if len(got) != 3 || got[10] != 100 || got[20] != 200 || got[30] != 300 {
			t.Fatalf("DST2 after INSERT...SELECT B.* = %v, want {10:100 20:200 30:300}", got)
		}
	})

	t.Run("SELECT qualified-star schema-qualified comma source", func(t *testing.T) {
		// The non-DML control: `SELECT B.* FROM PA AS main, main.PB AS B` under the
		// non-default session schema `main`. The top-level SELECT plans through the
		// PlanVisitor (which classifies with the REAL metadata resolver, so it never
		// hit the segments bug), but it is exercised end-to-end here to prove the
		// schema-qualified `main.PB` resolves to the real table PB — PA(1 row)×PB
		// yields exactly PB's three rows — NOT a correlated unnest of the
		// non-existent PA.PB (which would fail / drop rows). Scan dynamically: the
		// star expansion's column count is a separate qualified-star concern; this
		// control pins the SOURCE classification (real table, three rows of PB
		// data), not the projection width. RFC-142.
		rows, qerr := db.QueryContext(ctx,
			`SELECT "B".* FROM PA AS "main", "main"."PB" AS "B" ORDER BY "B"."ID"`)
		if qerr != nil {
			t.Fatalf("SELECT B.* (schema main): %v", qerr)
		}
		cols, cErr := rows.Columns()
		if cErr != nil {
			t.Fatalf("columns: %v", cErr)
		}
		var rowCount int
		seen := map[int64]int64{} // PB.id -> PB.v, read from whichever columns hold them
		for rows.Next() {
			dest := make([]any, len(cols))
			holders := make([]sql.NullInt64, len(cols))
			for i := range dest {
				dest[i] = &holders[i]
			}
			if sErr := rows.Scan(dest...); sErr != nil {
				t.Fatalf("scan: %v", sErr)
			}
			rowCount++
			// PB's (id, v) pair is present in each row regardless of star width.
			for i := 0; i+1 < len(holders); i++ {
				if holders[i].Valid && holders[i+1].Valid {
					if v, ok := map[int64]int64{10: 100, 20: 200, 30: 300}[holders[i].Int64]; ok && holders[i+1].Int64 == v {
						seen[holders[i].Int64] = holders[i+1].Int64
					}
				}
			}
		}
		rows.Close()
		if rowCount != 3 {
			t.Fatalf("SELECT B.* row count = %d, want 3 (PA×PB over real table PB)", rowCount)
		}
		if len(seen) != 3 || seen[10] != 100 || seen[20] != 200 || seen[30] != 300 {
			t.Fatalf("SELECT B.* PB rows = %v, want {10:100 20:200 30:300}", seen)
		}
	})
}

// TestFDB_ArrayUnnestDMLDuplicateAlias drives the REAL live DML path (planDML in
// cascades_generator.go) and pins the R31 P1 fix: an `INSERT INTO dst SELECT V
// FROM T1, T1.arr AS V, U AS V` — whose lateral-unnest AS alias `V` ALSO names a
// LATER comma source (`U AS V`) — must be REJECTED with ErrCodeDuplicateAlias, not
// silently INSERT wrong rows.
//
// The duplicate-alias guard (rejectDuplicateUnnestAlias) ran only in the SELECT
// path (planSelectCascades / the plan harness), NOT in planDML. So the DML's
// INSERT…SELECT body reached translation without the guard: the translator's
// bottom-up lowering of `T1, T1.arr AS V` cannot see the LATER `U AS V` (the right
// child of an ancestor join), so both legs were planned under alias V and the
// outer NestedLoopJoin's mergeRows OVERWRITES the unnest's V keys last-leg-wins
// with U.V — the INSERT wrote U.V instead of the unnested element (silent-wrong).
// The fix runs the SAME rejectDuplicateUnnestAlias pass on the DML logicalOp (it
// recurses into LogicalInsert.Source / LogicalUpdate.Input / LogicalDelete.Input),
// so the later-source collision is rejected cleanly BEFORE translation. RFC-142.
func TestFDB_ArrayUnnestDMLDuplicateAlias(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/ajt_dml_dupalias"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	// T1 carries an INTEGER ARRAY (the unnest source); U carries a scalar V whose
	// value (999) is distinct from every element; DST is the INSERT target. A
	// non-colliding sibling table W (scalar X) drives the control INSERT that must
	// still succeed.
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE ajt_dml_dupalias_tmpl"+
		" CREATE TABLE T1 (id BIGINT NOT NULL, arr INTEGER ARRAY, PRIMARY KEY (id))"+
		" CREATE TABLE U (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE W (id BIGINT NOT NULL, x BIGINT, PRIMARY KEY (id))"+
		" CREATE TABLE DST (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE ajt_dml_dupalias_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=s")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// One row in U (V=999) and W (X=7) so a successful INSERT…SELECT would have a
	// non-empty cross product; T1 has one row whose array is unset (NULL) — the
	// duplicate-alias rejection fires at PLAN time, independent of data.
	if _, err := db.ExecContext(ctx, "INSERT INTO U VALUES (1, 999)"); err != nil {
		t.Fatalf("seed U: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO W VALUES (1, 7)"); err != nil {
		t.Fatalf("seed W: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO T1 VALUES (1, NULL)"); err != nil {
		t.Fatalf("seed T1: %v", err)
	}

	t.Run("INSERT...SELECT later source reusing the unnest AS alias is DuplicateAlias", func(t *testing.T) {
		// `INSERT INTO DST SELECT V FROM T1, T1.arr AS V, U AS V`: the lateral-unnest
		// AS alias `V` collides with the LATER comma source `U AS V`. The DML path must
		// reject with ErrCodeDuplicateAlias. Pre-fix: NO rejection (planDML never ran
		// the guard), so the INSERT proceeded and U.V overwrote the unnest's V — silent-
		// wrong rows. Revert-proof on the rejection. RFC-142.
		_, err := db.ExecContext(ctx,
			`INSERT INTO DST SELECT "V" FROM T1, T1."ARR" AS "V", U AS "V"`)
		if err == nil {
			t.Fatalf("INSERT...SELECT duplicate unnest alias: expected rejection, got nil (the later U AS V silently overwrote the unnest V)")
		}
		requireSQLSTATE(t, err, api.ErrCodeDuplicateAlias)
		// DST must be untouched (the INSERT was rejected at plan time, never executed).
		var cnt int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM DST").Scan(&cnt); err != nil {
			t.Fatalf("count DST: %v", err)
		}
		if cnt != 0 {
			t.Fatalf("DST count after rejected INSERT = %d, want 0 (no wrong rows written)", cnt)
		}
	})

	t.Run("INSERT...SELECT AT alias colliding with a later source is DuplicateAlias", func(t *testing.T) {
		// The AT-alias variant: `... T1.arr AS E AT V, U AS V` — the unnest ORDINAL
		// alias `V` collides with the later `U AS V`. The same guard must reject it
		// (the AT binding participates in the same range-variable uniqueness). RFC-142.
		_, err := db.ExecContext(ctx,
			`INSERT INTO DST SELECT "E" FROM T1, T1."ARR" AS "E" AT "V", U AS "V"`)
		if err == nil {
			t.Fatalf("INSERT...SELECT duplicate AT alias: expected rejection, got nil")
		}
		requireSQLSTATE(t, err, api.ErrCodeDuplicateAlias)
	})

	t.Run("control: INSERT...SELECT unnest with a non-colliding later source succeeds", func(t *testing.T) {
		// The discriminating control: the SAME shape but the later comma source W does
		// NOT reuse the unnest alias V (it is `W`, with column X). The INSERT must
		// PLAN and run — proving the guard rejects ONLY the genuine alias collision, not
		// every unnest-with-later-source. T1's array is NULL so zero rows flow, but the
		// statement must SUCCEED (RowsAffected 0), not be rejected. RFC-142.
		res, err := db.ExecContext(ctx,
			`INSERT INTO DST SELECT "V" FROM T1, T1."ARR" AS "V", W`)
		if err != nil {
			t.Fatalf("control INSERT...SELECT non-colliding unnest: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 0 {
			t.Fatalf("control INSERT RowsAffected = %d, want 0 (T1.arr is NULL → no elements)", n)
		}
	})
}
