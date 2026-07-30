package sqldriver_test

import (
	"context"
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

func TestFDB_UnnestExistsGather(t *testing.T) {
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
	md := existsGatherSchemaMetadata(t)

	mkA := func(aid, k int64, vals ...int32) proto.Message {
		d := md.GetRecordType("A").Descriptor
		m := dynamicpb.NewMessage(d)
		m.Set(d.Fields().ByName("AID"), protoreflect.ValueOfInt64(aid))
		m.Set(d.Fields().ByName("K"), protoreflect.ValueOfInt64(k))
		fd := d.Fields().ByName("ARR")
		l := m.NewField(fd).List()
		for _, v := range vals {
			l.Append(protoreflect.ValueOfInt32(v))
		}
		m.Set(fd, protoreflect.ValueOfList(l))
		return m
	}
	mk1 := func(table, f string, v int64) proto.Message {
		d := md.GetRecordType(table).Descriptor
		m := dynamicpb.NewMessage(d)
		m.Set(d.Fields().ByName(protoreflect.Name(f)), protoreflect.ValueOfInt64(v))
		return m
	}

	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Create()
		if sErr != nil {
			return nil, sErr
		}
		for _, r := range []proto.Message{
			mkA(1, 100, 7, 8),
			mk1("B", "BID", 2),
			mk1("EE", "CK", 100),
			mk1("EEV", "VK", 7),
		} {
			if _, e := store.SaveRecord(r); e != nil {
				return nil, e
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	runQ := func(t *testing.T, sql string) ([]string, string, error) {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
		if perr != nil {
			return nil, "", perr
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
				// POSITIONAL, in slot order -- the map rendering this replaced printed the
				// row by NAME, so permuting (Fields, Slots) together was invisible.
				out = append(out, positionalPipeSprint(r))
			}
			return nil, nil
		})
		sort.Strings(out)
		return out, explain, eerr
	}

	const from = `FROM A, B, A."ARR" AS "X"`
	// pin: the inner cluster under EXISTS ordinalizes and answers correctly. The
	// plan must NOT flatten to the SARG-losing N-way existential wrap
	// (NWayJoinWithExistential), and the dup-named A.K/B.K legs must be
	// DISAMBIGUATED by the alias-aware per-leg bake — a name-based or
	// alias-count-blind bind conflates them (RED rows).
	pin := func(name, sql string, want ...string) {
		t.Run(name, func(t *testing.T) {
			rows, plan, err := runQ(t, sql)
			if err != nil {
				t.Fatalf("%q: %v", sql, err)
			}
			sort.Strings(want)
			if strings.Join(rows, ",") != strings.Join(want, ",") {
				t.Fatalf("rows = %v, want %v\n  plan: %s", rows, want, plan)
			}
			if strings.Contains(plan, "NWayJoinWithExistential") {
				t.Errorf("R1: plan flattened to the SARG-losing N-way existential wrap\n  %s", plan)
			}
		})
	}

	// EXISTS on the PRESENT leg (A.K=100 matches EE) → {7,8}. A wrong-leg bind
	// reading B.K=NULL would give {} (RED).
	pin("faceB_AK", `SELECT "X" `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`, "7", "8")
	// EXISTS on the NULL leg (B.K=NULL → no match) → {}. A wrong-leg bind reading
	// A.K=100 (matches) would give {7,8} (RED) — the dup-named discriminator.
	pin("faceB_BK", `SELECT "X" `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = B."K")`)
	// NOT EXISTS on the NULL leg → keep → {7,8}; a wrong-leg bind (A.K matches)
	// would drop → {} (RED).
	pin("faceB_notBK", `SELECT "X" `+from+` WHERE NOT EXISTS (SELECT 1 FROM EE WHERE EE."CK" = B."K")`, "7", "8")
	// ELEMENT correlation EEV.VK = X → only element 7 matches EEV(VK=7) → {7}. An
	// unbaked element ref (mis-resolved over the NLJ layout) gives {} (RED).
	pin("faceB_element", `SELECT "X" `+from+` WHERE EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X")`, "7")
	// a WHERE conjunct alongside the EXISTS, both on the present leg → {7,8}.
	pin("faceB_conj_and_exists", `SELECT "X" `+from+` WHERE A."AID" = 1 AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`, "7", "8")

	// AGGREGATE over the INNER-under-EXISTS gather ORDINALIZES: removing the
	// underAggregate gather-decline and peeling the EXISTS wrapper in
	// gatheredSeedBakeContext (the semi-join FlatMap's outer quantifier is the
	// raw gathered seed) bakes COUNT(X)'s element operand to its seed slot. The
	// NLJ's leg-window drop is immaterial — COUNT(X) reads the ELEMENT, not a
	// leg column. COUNT(X) over elements {7,8} of the surviving A.K=100 row = 2
	// (the accompanying census test pins that this shape plans cleanly).
	pin("agg_count_over_gather", `SELECT COUNT("X") `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`, "2")
	// LEG-COLUMN aggregate under the same gather (the correct-or-loud discriminator the
	// element-only COUNT can't be): SUM(A.K) reads a LEG column, which the existential-
	// wrapper NLJ drops from its windowed layout — so this proves the aggregate bake
	// resolves leg columns too, not just the element. A.K=100 over the 2 surviving
	// element rows {7,8} → SUM = 200. A collapse (leg-window drop unhandled) → NULL/0 RED.
	pin("agg_sum_legcol_over_gather", `SELECT SUM(A."K") `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`, "200")
	// GROUP BY the ELEMENT under the gather (another explicit collapse concern): the
	// grouped key bakes to the element slot the same way. X∈{7,8}, each its own group of 1.
	pin("agg_groupby_element_over_gather", `SELECT "X", COUNT(*) `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") GROUP BY "X"`, "7|1", "8|1")
	// RETAINED outer-only ELEMENT predicate: `EXISTS(… WHERE X = 7)` has no
	// inner-table ref, so buildCorrelatedExists keeps it in esq.Plan (the buried
	// channel), which must ALSO bake the element — JoinPredicate-only baking
	// would leave X unbound → 0 rows. X=7 true, EE non-empty → {7}.
	pin("retained_element_pred", `SELECT "X" `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE "X" = 7)`, "7")
	// ELEMENT aliased the SAME as an inner column (`A.ARR AS CK`, inner `EE.CK`):
	// the element bake must NOT rewrite the inner-table `EE.CK` to the element
	// slot — a nil-windows bake would mis-bake it into a malformed plan. Only
	// merged-corr/bare refs bake; EE.CK resolves in ∃. A.K=100 matches → {7,8}.
	pin("element_alias_shadows_inner", `SELECT "CK" FROM A, B, A."ARR" AS "CK" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`, "7", "8")

	// WRAPPED aggregate-over-EXISTS: a CTE/derived alias or a DISTINCT sits
	// between the aggregate and the EXISTS gather. The seed sits under those
	// IDENTITY wrappers (SELECT-*, DISTINCT) that preserve its positional row,
	// so gatheredSeedBakeContext walks the outer-quantifier chain (identity
	// wrappers only — never a row-reshaping GROUP BY) to reach it, and the
	// group key bakes to the seed's leg/element window. It ordinalizes
	// correctly with no dependency on name-based resolution as a fallback. A
	// one-level peel / wrapper-blind bake used to collapse these to AID=NULL
	// or one NULL group; the recursive identity-wrapper walk plus
	// bare-leg-column resolution fixes them. Java-parity: Java plans GROUP BY
	// over a multi-source-FROM derived table (GroupByQueryTests.java:699); CTE
	// ≡ derived table.
	//   CTE passthrough, GROUP BY a BARE leg column (D.AID → seed A-leg window).
	pin("agg_cte_groupby_leg", `WITH D AS (SELECT * `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")) SELECT D."AID", COUNT(*) FROM D GROUP BY D."AID"`, "1|2")
	//   DISTINCT between the aggregate and the EXISTS, GROUP BY the element.
	pin("agg_cte_distinct_groupby_element", `WITH D AS (SELECT DISTINCT * `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")) SELECT D."X", COUNT(*) FROM D GROUP BY D."X"`, "7|1", "8|1")
	//   DEEP DISTINCT chain (≥6): DISTINCT is non-merging, so N chained CTEs stay N wrappers
	//   deep. The identity walk is UNBOUNDED (visited-set over the finite plan tree), so the
	//   seed is reached at any depth — a fixed depth bound would silently exhaust and skip the
	//   bake → NULL past 6 levels deep. Correct: single A.K=100 group, 2 rows.
	pin("agg_cte_deep6_distinct_groupby_leg",
		`WITH D0 AS (SELECT DISTINCT * `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")), `+
			`D1 AS (SELECT DISTINCT * FROM D0), D2 AS (SELECT DISTINCT * FROM D1), D3 AS (SELECT DISTINCT * FROM D2), `+
			`D4 AS (SELECT DISTINCT * FROM D3), D5 AS (SELECT DISTINCT * FROM D4) `+
			`SELECT D5."AID", COUNT(*) FROM D5 GROUP BY D5."AID"`, "1|2")
	// PROJECTING-CTE-aggregate class: the name-based fallback reads each group
	// key by NAME over the projection's output columns, so the outcome splits
	// on whether the key RESOLVES there:
	//   BARE projection — output column name == the D-stripped key ("AID"→"AID") → resolves →
	//   correct rows (kept; refusing it would regress a Java-supported query that already works).
	pin("agg_cte_projecting_single_bare", `WITH D AS (SELECT "AID" `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")) SELECT D."AID", COUNT(*) FROM D GROUP BY D."AID"`, "1|2")
	pin("agg_cte_projecting_bare_reorder", `WITH D AS (SELECT "BID", "AID" `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")) SELECT D."AID", COUNT(*) FROM D GROUP BY D."AID"`, "1|2")
	//   QUALIFIED projection — ORDINALIZED: the body's qualified reads bake
	//   leg-addressed (bakeDottedRefsToLegQOV) and the group key bakes against
	//   the projected output layout, so the query ANSWERS with Java's rows
	//   (GroupByQueryTests:699). This used to be a LOUD refusal because
	//   name-based resolution mis-named the output column ("A.AID" ≠ key
	//   "AID" → silent NULL); the loud pin is superseded by the row assertion
	//   here — the stronger check.
	pin("agg_cte_qualreorder", `WITH D AS (SELECT B."BID", A."AID", A."K" `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")) SELECT D."AID", COUNT(*) FROM D GROUP BY D."AID"`, "1|2")
	// A LAYOUT-PRESERVING wrapper (ORDER BY, LIMIT) between the aggregate and the gather does
	// NOT reshape columns — findWindowedSeed walks it, so an IDENTITY SELECT-* under a sort/limit
	// still ORDINALIZES correctly. (A bare `SELECT * … ORDER BY` used to aggregate
	// silently to NULL when the walk stopped at the sort.)
	pin("agg_cte_star_orderby_limit", `WITH D AS (SELECT * `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") ORDER BY A."AID" LIMIT 100) SELECT D."AID", COUNT(*) FROM D GROUP BY D."AID"`, "1|2")
	// The qualified projection ORDINALIZES through those same wrappers (+ UNION ALL) too —
	// the former loud floor is superseded by row assertions (see agg_cte_qualreorder).
	pin("agg_cte_qual_orderby", `WITH D AS (SELECT A."AID" `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") ORDER BY A."AID" LIMIT 100) SELECT D."AID", COUNT(*) FROM D GROUP BY D."AID"`, "1|2")
	pin("agg_cte_qual_union", `WITH D AS (SELECT A."AID" `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") UNION ALL SELECT A."AID" `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")) SELECT D."AID", COUNT(*) FROM D GROUP BY D."AID"`, "1|4")

	// MULTI-EXISTS-under-aggregate now PLANS: PartitionSelectRule peels a
	// sibling multi-EXISTS `EXISTS(A) AND EXISTS(B)` into nested 2-quantifier
	// existential selects the NLJ rule implements — previously this shape
	// failed loud. COUNT("X") over the LEFT box A⋈B, A.ARR={7,8}: EXISTS(EE.CK=A.K=100)
	// is true; EXISTS(EEV.VK=X) is true only for X=7 (EEV has VK=7) → the multi-EXISTS
	// keeps X=7, drops X=8 → COUNT=1.
	pin("agg_multiexists_counts", `SELECT COUNT("X") `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X")`,
		"1")

	// Additional coverage dimensions the core disambiguation pins above don't
	// exercise:
	// (4) UNCORRELATED verdict-None EXISTS (whole-row-independent) → must ANSWER.
	pin("uncorrelated_exists", `SELECT "X" `+from+` WHERE EXISTS (SELECT 1 FROM EE)`, "7", "8")
	// (5) ARITHMETIC on a buried leg (compound leg ref through the ordinal bake).
	pin("arith_on_buried_leg", `SELECT "X" `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K" + 0)`, "7", "8")
	// the multi-table-element EXISTS stays LOUD 0AF00 (the same guard that fires
	// on the INNER path too — a forward sentinel: if a future change makes it
	// gather, this catches a silent-wrong regression instead).
	t.Run("twotable_leg_and_element_loud", func(t *testing.T) {
		_, _, err := runQ(t, `SELECT "X" `+from+` WHERE EXISTS (SELECT 1 FROM EE, EEV WHERE EE."CK" = A."K" AND EEV."VK" = "X")`)
		if err == nil || !strings.Contains(err.Error(), "0AF00") {
			t.Fatalf("two-table leg+element EXISTS must be LOUD 0AF00, got: %v", err)
		}
	})

	// A BAKEABLE leg conjunct alongside the EXISTS (`… A.K=100 AND EXISTS(…)`)
	// ORDINALIZES: the conjunct bakes IN-SELECT over the re-derivable
	// gatedJoinLegTypes; the correlation bakes onto the output QOV — both over
	// the SAME seed layout.
	// LOAD-BEARING: the conjunct's A.K slot and the correlation's A.K slot must be
	// the SAME. A.K=100 → conjunct true, EE.CK=100 matches A.K → EXISTS true → {7,8}.
	pin("e1b_conj_and_corr_same_leg", `SELECT "X" `+from+` WHERE A."K" = 100 AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`, "7", "8")
	// the conjunct genuinely filters (reads A.K, not a wrong slot): A.K=100 ≠ 999 → {}.
	pin("e1b_conj_false", `SELECT "X" `+from+` WHERE A."K" = 999 AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`)
	// dup-named discriminator: the conjunct is on B.K (NULL), the correlation on
	// A.K (100) — DIFFERENT slots. B.K IS NULL → conjunct true; A.K=100 → EXISTS
	// true → {7,8}. A wrong-slot bake reading A.K for the B.K conjunct → A.K IS
	// NULL false → {} RED.
	pin("e1b_dup_conj_BK_corr_AK", `SELECT "X" `+from+` WHERE B."K" IS NULL AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`, "7", "8")
	// a conjunct on the ELEMENT (not a leg): X = 7 → only element 7 → {7}.
	pin("e1b_element_conjunct", `SELECT "X" `+from+` WHERE "X" = 7 AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`, "7")
	// An UNBAKEABLE conjunct (subquery-carrying) declines GRACEFULLY to
	// name-based resolution — the plan SUCCEEDS (not a strand/loud error).
	// Plan-only: executing it needs scalar-subquery pre-binding
	// (WithScalarSubqueries), which is orthogonal to this test.
	t.Run("e1b_unbakeable_conj_subquery", func(t *testing.T) {
		if _, err := embedded.PlanRecordQueryWithMetadata(`SELECT "X" `+from+` WHERE A."K" = (SELECT MIN(EE."CK") FROM EE) AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`, md, nil); err != nil {
			t.Fatalf("unbakeable subquery conjunct must plan (graceful decline to name-based resolution), got: %v", err)
		}
	})

	// ENCLOSURE REGRESSION: when the leg-conjunct-plus-EXISTS filter above is
	// translated BENEATH an enclosing parent that resolves by name (this
	// cluster used as a CTE join leg), translateUnnestJoin SKIPS the gather
	// (prevEnclosure) and returns an ANCHORED binary seed — but the merge
	// branch keyed only on `gatheredHere` (admission) baked the conjunct
	// IN-SELECT over that anchored seed anyway, leaving A.K unbound in the
	// Explode filter → 0 rows. The branch must ALSO verify the seed is the
	// WINDOWED ordinal one (the gather actually ran), like the plain gather's
	// seedWindowed check / the box arm's hasRecord. A.K=100 > X∈{7,8} → both
	// true; EXISTS EE.CK=100 → true; the CTE = {7,8}, crossed with EEV →
	// {D.X:7, D.X:8}. Projection `SELECT D."X"` keys the Datum as the
	// qualified D.X.
	pin("e1b_enclosed_cte_leg_conj", `WITH D AS (SELECT "X" `+from+` WHERE A."K" > "X" AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")) SELECT D."X" FROM D, EEV`, "7", "8")
}

// TestUnnestExistsGatherCensus pins that the inner cluster under EXISTS keeps
// PLANNING cleanly (the ordinal gather owns it, with no name-based fallback
// to fall back to, so a regression is a LOUD plan error). Planning-only.
func TestUnnestExistsGatherCensus(t *testing.T) {
	t.Parallel()
	md := existsGatherSchemaMetadata(t)
	const from = `FROM A, B, A."ARR" AS "X"`
	countProducers := func(sql string) int {
		if _, err := embedded.PlanRecordQueryWithMetadata(sql, md, nil); err != nil {
			t.Fatalf("plan %q: %v", sql, err)
		}
		return 0 // plan-success probe: there is no per-producer firing count to check any more
	}
	for _, tc := range []struct{ name, sql string }{
		{"inner_AK", `SELECT "X" ` + from + ` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
		{"inner_BK", `SELECT "X" ` + from + ` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = B."K")`},
		{"inner_element", `SELECT "X" ` + from + ` WHERE EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X")`},
		// AGGREGATE-under-EXISTS ordinalizes too (the peeled-wrapper element
		// bake) — this used to decline before the aggregate.
		{"agg_count", `SELECT COUNT("X") ` + from + ` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
	} {
		if n := countProducers(tc.sql); n != 0 {
			t.Fatalf("%s: fired %d name-based fallback(s), want 0 (the gather must own it)", tc.name, n)
		}
	}
}
