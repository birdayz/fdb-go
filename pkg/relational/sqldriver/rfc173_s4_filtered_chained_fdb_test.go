package sqldriver_test

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/relational/core/embedded"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// TestFDB_RFC173S4_FilteredChained is the certificate for the RFC-173 S4 filtered-chained
// lift: a chained lateral unnest under an ancestor WHERE (`FROM t, t.SARR AS x, x.SUB AS y
// WHERE <pred>`) now ORDINALIZES instead of declining to the name-model residual
// (buildUnnestResultValue → NewAnchoredJoinRecord). The coarse "any filter suppresses the
// chained ordinal seed" decline is retired; predicate placement is handled PER CONJUNCT by
// the ⊆-outerLegs pushable-to-scan rebase gate (rebaseChainedOuterLegPredicate):
//   - an OUTER-column-only conjunct (correlated-to ⊆ {t}) is pushed to Scan(t) as a SARG,
//     leaving its ref lazy (QOV(t));
//   - a conjunct referencing a correlation bound INSIDE the chain (the first-link element x
//     or the second-link element y) keeps the rebase (QOV(X)."t.col") and lands at the
//     FlatMap/Explode level whose row is the merged [t-cols ++ x] row.
//
// Discriminating data: the outer scalar T4.SUB=777 SHADOWS the element's SUB array — a
// mis-bind (reading the outer-row SUB instead of the element's SUB) would surface 777s. The
// straddling detector T4(3) with y∈{3,9} makes T4.ID=3 match element y=3 (a full-row filter
// resolves it; a single level does not). Element K values discriminate a would-be x-column
// conjunct (unreachable today — see the scalar-sibling resolution gap).
//
//	T4(10): SARR=[elem K=100 SUB[1,2], elem K=200 SUB[3]]  → chained y ∈ {1,2,3}
//	T4(20): SARR=[elem K=100 SUB[4,5,6]]                   → chained y ∈ {4,5,6}
//	T4(3):  SARR=[elem K=300 SUB[3,9]]                     → chained y ∈ {3,9}
//
// The scalar-subquery-over-chained residual (`WHERE t.id = (SELECT …)`) is rejected LOUDLY
// (0A000) — pinned here as the sentinel for the silent-wrong bug HEAD shipped (name-model
// answered `[]` instead of Java's rows). It rides the wedgeGate POSITIONAL bake, a different
// path than this slice's per-conjunct rebase; making it return Java's rows is booked as its
// own slice. This 0A000 pin also guards the retirement: if the coarse name-model decline
// returned, this shape would regress to silent-`[]` and the pin would fail.
func TestFDB_RFC173S4_FilteredChained(t *testing.T) {
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
	elemDesc := sarrFD.Message()

	mkElem := func(k int64, sub ...int32) protoreflect.Value {
		m := dynamicpb.NewMessage(elemDesc)
		m.Set(elemDesc.Fields().ByName("K"), protoreflect.ValueOfInt64(k))
		sl := m.NewField(elemDesc.Fields().ByName("SUB")).List()
		for _, s := range sub {
			sl.Append(protoreflect.ValueOfInt32(s))
		}
		m.Set(elemDesc.Fields().ByName("SUB"), protoreflect.ValueOfList(sl))
		return protoreflect.ValueOfMessage(m)
	}
	mkT4 := func(id int64, sarr ...protoreflect.Value) proto.Message {
		m := dynamicpb.NewMessage(t4Desc)
		m.Set(t4Desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(t4Desc.Fields().ByName("SUB"), protoreflect.ValueOfInt64(777)) // outer shadow of the element SUB
		sl := m.NewField(sarrFD).List()
		for _, e := range sarr {
			sl.Append(e)
		}
		m.Set(sarrFD, protoreflect.ValueOfList(sl))
		return m
	}

	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if sErr != nil {
			return nil, sErr
		}
		recs := []proto.Message{
			mkT4(10, mkElem(100, 1, 2), mkElem(200, 3)),
			mkT4(20, mkElem(100, 4, 5, 6)),
			mkT4(3, mkElem(300, 3, 9)),
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

	run := func(t *testing.T, q string) (string, []string, error) {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadata(q, md, nil)
		if perr != nil {
			return "", nil, perr
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
		return explain, out, eerr
	}
	// wantSet asserts the sorted row SET. planContains (optional) pins a plan-placement
	// fragment — the end-to-end proof that PushFilterBelowJoinRule landed the conjunct where
	// the ⊆-outerLegs gate predicted.
	wantSet := func(t *testing.T, q string, want []string, planContains string) {
		t.Helper()
		explain, got, eerr := run(t, q)
		if eerr != nil {
			t.Fatalf("exec %q: %v", q, eerr)
		}
		sort.Strings(got)
		if !slices.Equal(got, want) {
			t.Fatalf("rows = %v, want %v\n  sql: %s\n  plan: %s", got, want, q, explain)
		}
		if planContains != "" && !strings.Contains(explain, planContains) {
			t.Fatalf("plan missing %q (placement regression)\n  sql: %s\n  plan: %s", planContains, q, explain)
		}
	}

	// Unfiltered control (already ordinalized pre-slice): all chained y.
	t.Run("unfiltered_control", func(t *testing.T) {
		t.Parallel()
		wantSet(t, `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y"`,
			[]string{"map[Y:1]", "map[Y:2]", "map[Y:3]", "map[Y:3]", "map[Y:4]", "map[Y:5]", "map[Y:6]", "map[Y:9]"}, "")
	})

	// OUTER-column-only equality → pushed to Scan(T4) as a SARG (the ⊆-outerLegs gate leaves
	// it lazy; PushFilterBelowJoinRule pushes it to the base scan). Explain must show the SARG.
	t.Run("outer_col_eq_sargs_scan", func(t *testing.T) {
		t.Parallel()
		wantSet(t, `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE T4."ID" = 10`,
			[]string{"map[Y:1]", "map[Y:2]", "map[Y:3]"}, "Scan(T4, [=])")
		wantSet(t, `SELECT T4."ID", "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE T4."ID" = 20`,
			[]string{"map[T4.ID:20 Y:4]", "map[T4.ID:20 Y:5]", "map[T4.ID:20 Y:6]"}, "Scan(T4, [=])")
	})

	// STRADDLING (`T4.ID = y`): references the inner element y → keeps the rebase → the whole
	// predicate lands as a PredicatesFilter on the INNER Explode (QOV(X)."T4.ID" + y in scope).
	// T4(3): y∈{3,9}, ID=3=y=3 → one row. NO single level except the inner filter resolves it.
	t.Run("straddling_inner_filter", func(t *testing.T) {
		t.Parallel()
		wantSet(t, `SELECT T4."ID", "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE T4."ID" = "Y"`,
			[]string{"map[T4.ID:3 Y:3]"}, "inner=PredicatesFilter")
	})

	// INNER-element-only filter (`y > 2`) → PredicatesFilter on the inner Explode.
	t.Run("inner_element_filter", func(t *testing.T) {
		t.Parallel()
		wantSet(t, `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE "Y" > 2`,
			[]string{"map[Y:3]", "map[Y:3]", "map[Y:4]", "map[Y:5]", "map[Y:6]", "map[Y:9]"}, "inner=PredicatesFilter")
	})

	// SEPARATE conjuncts (`T4.ID=10 AND y>2`): the AND splits — T4.ID=10 to the outer level
	// (T4 in scope), y>2 to the inner Explode. T4(10) y∈{1,2,3} ∩ >2 = {3}.
	t.Run("separate_conjuncts_split", func(t *testing.T) {
		t.Parallel()
		wantSet(t, `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE T4."ID" = 10 AND "Y" > 2`,
			[]string{"map[Y:3]"}, "")
	})

	// The full spread of NON-subquery outer-column filter shapes — each must ordinalize with
	// correct rows (the coarse decline was needlessly routing ALL of these to name-model).
	t.Run("non_subquery_filter_shapes", func(t *testing.T) {
		t.Parallel()
		// IN-list (not subquery).
		wantSet(t, `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE T4."ID" IN (10, 3)`,
			[]string{"map[Y:1]", "map[Y:2]", "map[Y:3]", "map[Y:3]", "map[Y:9]"}, "")
		// BETWEEN.
		wantSet(t, `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE T4."ID" BETWEEN 5 AND 25`,
			[]string{"map[Y:1]", "map[Y:2]", "map[Y:3]", "map[Y:4]", "map[Y:5]", "map[Y:6]"}, "")
		// Arithmetic on the outer column.
		wantSet(t, `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE T4."ID" + 1 = 11`,
			[]string{"map[Y:1]", "map[Y:2]", "map[Y:3]"}, "")
		// IS NOT NULL (all rows).
		wantSet(t, `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE T4."ID" IS NOT NULL`,
			[]string{"map[Y:1]", "map[Y:2]", "map[Y:3]", "map[Y:3]", "map[Y:4]", "map[Y:5]", "map[Y:6]", "map[Y:9]"}, "")
		// NOT of an outer comparison.
		wantSet(t, `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE NOT (T4."ID" = 10)`,
			[]string{"map[Y:3]", "map[Y:4]", "map[Y:5]", "map[Y:6]", "map[Y:9]"}, "")
		// NOT(AND) has NO OrPredicate node → ORDINALIZES (predicateContainsOr = false). Safe
		// because NormalizePredicatesRule does NOT De Morgan it: DeMorganRule is absent from the
		// default rules and normalizeCNF treats a NotPredicate as an OPAQUE leaf, so the mixed
		// NOT(ID=10 AND y>2) conjunct is name-key-rebased WHOLE and resolves at the inner Explode
		// — no pure-outer clause is ever extracted to strand. T4(10) y≤2 → {1,2}; T4(20),T4(3)
		// (ID≠10) → all → {4,5,6}∪{3,9}. → {1,2,3,4,5,6,9}.
		wantSet(t, `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE NOT (T4."ID" = 10 AND "Y" > 2)`,
			[]string{"map[Y:1]", "map[Y:2]", "map[Y:3]", "map[Y:4]", "map[Y:5]", "map[Y:6]", "map[Y:9]"}, "")
		// Three-conjunct AND: T4.ID=10 AND y>1 AND y<3 → {2}.
		wantSet(t, `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE T4."ID" = 10 AND "Y" > 1 AND "Y" < 3`,
			[]string{"map[Y:2]"}, "")
	})

	// An OR in a filter over a chained unnest DECLINES to the name-model residual and returns
	// CORRECT rows (chainedUnderOrFilter). Why decline, not ordinalize: NormalizePredicatesRule
	// distributes the OR to CNF and PredicatePushDownRule pushes the extracted PURE-OUTER clause
	// (e.g. `ID=10 OR ID=3` from `(ID=10 AND y>2) OR (ID=3 AND y<5)`) to the first-link ordinal
	// FlatMap, where the name-keyed rebase resolves ordinal -1 → a malformed plan (a correctness
	// regression the first cut, which ordinalized ORs, shipped). The name-model qualified name
	// keys resolve the pushed clause; ordinalizing the OR needs the positional-bake path (booked).
	// Covers the nested-outer-conjunct shapes that survive CNF as a pushable outer clause AND flat ORs.
	t.Run("or_over_chained_declines_correct_rows", func(t *testing.T) {
		t.Parallel()
		// Flat OR mixing outer + inner: T4(10)→{1,2,3} (ID=10) ∪ y=5 from T4(20) → {1,2,3,5}.
		wantSet(t, `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE T4."ID" = 10 OR "Y" = 5`,
			[]string{"map[Y:1]", "map[Y:2]", "map[Y:3]", "map[Y:5]"}, "")
		// Flat OR of two outer conjuncts → {1,2,3,4,5,6}.
		wantSet(t, `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE T4."ID" = 10 OR T4."ID" = 20`,
			[]string{"map[Y:1]", "map[Y:2]", "map[Y:3]", "map[Y:4]", "map[Y:5]", "map[Y:6]"}, "")
		// NESTED-outer OR (the regressing shape): (ID=10 AND y>2)→T4(10) y=3; (ID=3 AND y<5)→T4(3)
		// y=3 (y=9 fails <5). → {3,3}.
		wantSet(t, `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE (T4."ID" = 10 AND "Y" > 2) OR (T4."ID" = 3 AND "Y" < 5)`,
			[]string{"map[Y:3]", "map[Y:3]"}, "")
		// (ID=10 AND y>2)→{3}; (ID=20)→all T4(20) {4,5,6}. → {3,4,5,6}.
		wantSet(t, `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE (T4."ID" = 10 AND "Y" > 2) OR (T4."ID" = 20)`,
			[]string{"map[Y:3]", "map[Y:4]", "map[Y:5]", "map[Y:6]"}, "")
		// (ID=10 AND y=1)→{1}; (ID=3 AND y=9)→{9}. → {1,9}.
		wantSet(t, `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE (T4."ID" = 10 AND "Y" = 1) OR (T4."ID" = 3 AND "Y" = 9)`,
			[]string{"map[Y:1]", "map[Y:9]"}, "")
		// Straddling INSIDE a disjunct (`t.id = y` OR outer): T4(3) id=3=y=3 → {3}; (ID=20)→{4,5,6}.
		wantSet(t, `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE (T4."ID" = "Y") OR (T4."ID" = 20)`,
			[]string{"map[Y:3]", "map[Y:4]", "map[Y:5]", "map[Y:6]"}, "")
		// NOT(OR) CONTAINS an OrPredicate node → predicateContainsOr catches it → DECLINES. This
		// is CONSERVATIVE over-decline (a NotPredicate is opaque to normalizeCNF, so this OR would
		// not actually distribute a stranding pure-outer clause), but the discriminator is
		// coarse-within-OR by design — correct-or-loud, and name-model answers correctly.
		// NOT(ID=10 OR ID=3) = ID∉{10,3} → T4(20) → {4,5,6}.
		wantSet(t, `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE NOT (T4."ID" = 10 OR T4."ID" = 3)`,
			[]string{"map[Y:4]", "map[Y:5]", "map[Y:6]"}, "")
	})

	// SCALAR-SUBQUERY residual → clean 0A000 (the silent-wrong-`[]` sentinel). Java answers
	// {4,5,6}; making it work is booked as its own slice. This must be LOUD, never `[]`.
	t.Run("scalar_subquery_loud_0A000", func(t *testing.T) {
		t.Parallel()
		q := `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y" WHERE T4."ID" = (SELECT MAX("ID") FROM T4 AS "T4B")`
		assert0A000(t, q, run)
	})

	// BURIED variant — the chained unnest is NOT the filter-input's direct rightmost source
	// (a trailing self-join T4C follows it). The gate's spine WALK must still catch it: HEAD +
	// the direct-only gate both shipped silent-`[]` here; the widened detector makes it LOUD.
	t.Run("scalar_subquery_buried_loud_0A000", func(t *testing.T) {
		t.Parallel()
		q := `SELECT "Y" FROM T4, T4."SARR" AS "X", "X"."SUB" AS "Y", T4 AS "T4C" WHERE T4."ID" = (SELECT MAX("ID") FROM T4 AS "T4B")`
		assert0A000(t, q, run)
	})
}

// assert0A000 asserts the query rejects LOUDLY with 0A000, never a silent row set.
func assert0A000(t *testing.T, q string, run func(*testing.T, string) (string, []string, error)) {
	t.Helper()
	_, _, eerr := run(t, q)
	if eerr == nil {
		t.Fatalf("must reject LOUDLY (0A000), not silently return rows\n  sql: %s", q)
	}
	if !strings.Contains(eerr.Error(), "0A000") {
		t.Fatalf("want 0A000 (feature-not-supported), got %v\n  sql: %s", eerr, q)
	}
}
