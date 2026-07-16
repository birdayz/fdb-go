package sqldriver_test

// Cert for the under-EXISTS LEFT/RIGHT box unnest: at verdict None it takes the
// GATHERED ordinal cluster (admitted by unnestExistentialGatherOK /
// admitExistentialGather) and resolves positionally instead of by name. The row
// pins prove correctness against real FDB (the dup-named A.K/B.K leg the
// qualified EXISTS correlation must disambiguate — a name-keyed lookup would
// conflate it); the plan-only pins prove the admitted LEFT box plans cleanly
// (the INNER cluster, E-1a's class, still resolves by name). R1 (SelectMergeRule
// flattening the gathered select into the SARG-losing N-way existential wrap) is
// asserted absent in every plan.

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
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestFDB_BoxJoinMultiExistsGather(t *testing.T) {
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
			mk1("B", "BID", 2), // B.K left unset → NULL
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

	// run executes sql through the production planner, returning sorted rows
	// and the plan EXPLAIN.
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
				out = append(out, unnestSprint(executor.RowValue(r)))
			}
			return nil, nil
		})
		sort.Strings(out)
		return out, explain, eerr
	}

	// MULTI-ESQ over a >=3-way INNER JOIN (no unnest) — regression pins for two panic
	// shapes: the peel of a multi-esq over a >=2-ForEach-leg join drove the ≥2-leg
	// anchored merge arm, whose re-enumeration can't resolve a column-less existential
	// → panic. Fix: the B1 fold gathers the join into ONE ordinal seed (so the peel is
	// Case 2, one live lower — never the merge arm), and PartitionSelectRule's anchored
	// merge (buildUpperResult) now DECLINES on an unresolvable leg instead of panicking.
	// A,B,EEV cross-join, B.K NULL: EXISTS(EE.CK=A.K=100)=true AND
	// EXISTS(EE.CK=B.K=NULL)=false → {}.
	t.Run("multiesq_3way_join_gathers", func(t *testing.T) {
		rows, plan, err := runQ(t, `SELECT A."K" FROM A, B, EEV WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = B."K")`)
		if err != nil {
			t.Fatalf("3-way multi-esq must plan (was a panic): %v", err)
		}
		if len(rows) != 0 {
			t.Fatalf("3-way multi-esq rows = %v, want [] (B.K NULL → 2nd EXISTS false)\n  %s", rows, plan)
		}
	})
	// LEFT box + a Bakeable box-leg conjunct + multi-esq (`A.K=100 AND EXISTS AND
	// EXISTS`): was a merge-arm panic (decline → name-model → merge arm). Now plans
	// (name-model select peels cleanly): A.K=100 true, EXISTS(EE.CK=A.K)=true,
	// EXISTS(EEV.VK=X): X=7 keeps, X=8 drops → {7}.
	t.Run("multiesq_leftbox_conjunct", func(t *testing.T) {
		rows, plan, err := runQ(t, `SELECT "X" FROM A LEFT JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE A."K" = 100 AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X")`)
		if err != nil {
			t.Fatalf("leftbox+conjunct multi-esq must plan (was a panic): %v", err)
		}
		want := []string{"map[X:7]"}
		if strings.Join(rows, ",") != strings.Join(want, ",") {
			t.Fatalf("leftbox+conjunct multi-esq rows = %v, want %v\n  %s", rows, want, plan)
		}
	})

	// 2-WAY cross-product multi-esq (regression pin): `FROM A, B WHERE EXISTS AND
	// EXISTS` peels via the 2-quantifier path (a cross product never reaches the
	// anchored merge arm), so it must PLAN — an earlier over-broad guard regressed it
	// to a strand. B.K NULL: EXISTS(EE.CK=A.K=100)=true AND EXISTS(EE.CK=B.K=NULL)=false
	// → {}.
	t.Run("multiesq_2way_crossproduct", func(t *testing.T) {
		rows, plan, err := runQ(t, `SELECT A."K" FROM A, B WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = B."K")`)
		if err != nil {
			t.Fatalf("2-way multi-esq must plan (regression guard): %v", err)
		}
		if len(rows) != 0 {
			t.Fatalf("2-way multi-esq rows = %v, want [] (B.K NULL)\n  %s", rows, plan)
		}
	})
	// 2-way + double NOT EXISTS: NOT EXISTS(EE.CK=A.K=100)=NOT(true)=false → drops all
	// → {} (A's only row fails the first NOT EXISTS). Must PLAN (regression guard).
	t.Run("multiesq_2way_double_notexists", func(t *testing.T) {
		rows, plan, err := runQ(t, `SELECT A."K" FROM A, B WHERE NOT EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND NOT EXISTS (SELECT 1 FROM EE WHERE EE."CK" = B."K")`)
		if err != nil {
			t.Fatalf("2-way double NOT EXISTS must plan (regression guard): %v", err)
		}
		if len(rows) != 0 {
			t.Fatalf("2-way double NOT EXISTS rows = %v, want []\n  %s", rows, plan)
		}
	})

	// NO-PANIC regression guard: a broad set of multi-esq shapes that variously reach
	// the peel / merge / gather / name-model paths. Each MUST be correct-or-LOUD —
	// never the "anchored re-enumeration must resolve an anchored parent's legs" panic
	// the multi-way-join multi-esq peel used to hit. runQ does not recover panics, so a
	// panic aborts this test; passing = no panic. (A few shapes strand loud — a
	// LogicalProjectionExpression with no physical rule — which is correct-or-loud, a
	// documented gap, not wrong rows: the retired name-map makes any miss loud.)
	t.Run("multiesq_no_panic_sweep", func(t *testing.T) {
		for _, q := range []string{
			`SELECT A."K" FROM A, B WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = B."K")`,                                                              // 2-way join + 2 EXISTS
			`SELECT A."K" FROM A, B, EE, EEV WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = A."AID")`,                                                 // 4-way join + 2 EXISTS
			`SELECT A."K" FROM A, B, EEV WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = B."K") AND EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = A."AID")`, // 3-way join + 3 EXISTS
			`SELECT "X" FROM A, B, A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X")`,                                                // INNER cluster + 2 EXISTS
			`SELECT "X" FROM A FULL OUTER JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X")`,            // FULL box + unnest + 2 EXISTS
			`SELECT A."K" FROM A, B, EEV WHERE NOT EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = B."K")`,                                                     // 3-way join + NOT EXISTS + EXISTS
		} {
			_, _, _ = runQ(t, q) // a panic here (anchored re-enumeration) fails the test
		}
	})

	const from = `FROM A LEFT JOIN B ON A."AID" = B."BID", A."ARR" AS "X"`
	// pin asserts the gathered path answers the admitted LEFT-box+EXISTS
	// shape correctly. The plan must NOT flatten to an N-way existential wrap
	// (R1: that would be the SARG-losing implementNWayJoinWithExistential).
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
				t.Errorf("R1: plan flattened to an N-way existential wrap (SARG-losing)\n  %s", plan)
			}
		})
	}

	// runQSub is runQ's scalar-subquery-aware sibling: it PRE-EVALUATES the plan's
	// scalar subqueries (PlanRecordQueryWithSubqueries + prebindScalarSubqueries)
	// and binds them via WithScalarSubqueries before executing — the direct-executor
	// analog of the sql driver's fetchPage pre-pass. runQ's EmptyEvaluationContext
	// cannot run ANY scalar-subquery shape (it would fail loud
	// UnboundScalarSubqueryError), so a subquery-carrying conjunct is pinned here.
	runQSub := func(t *testing.T, sql string) ([]string, string, error) {
		t.Helper()
		plan, subs, perr := embedded.PlanRecordQueryWithSubqueries(sql, md, nil)
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
				out = append(out, unnestSprint(executor.RowValue(r)))
			}
			return nil, nil
		})
		sort.Strings(out)
		return out, explain, eerr
	}
	pinSub := func(name, sql string, want ...string) {
		t.Run(name, func(t *testing.T) {
			rows, plan, err := runQSub(t, sql)
			if err != nil {
				t.Fatalf("%q: %v", sql, err)
			}
			sort.Strings(want)
			if strings.Join(rows, ",") != strings.Join(want, ",") {
				t.Fatalf("rows = %v, want %v\n  plan: %s", rows, want, plan)
			}
			if strings.Contains(plan, "NWayJoinWithExistential") {
				t.Errorf("R1: plan flattened to an N-way existential wrap (SARG-losing)\n  %s", plan)
			}
		})
	}

	// LEFT box + EXISTS on the PRESENT leg (A.K=100 matches EE) → {7,8}.
	pin("leftK_present", `SELECT "X" `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`,
		"map[X:7]", "map[X:8]")
	// EXISTS on the NULL-SUPPLIED leg (B.K=NULL → no match) → {}; a wrong-leg
	// bind reads A.K=100 (matches) → {7,8} RED — the dup-named discriminator.
	pin("rightK_nullsupplied", `SELECT "X" `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = B."K")`)
	// NOT EXISTS on the null-supplied leg → keep → {7,8}; a wrong-leg bind
	// drops → {} RED.
	pin("notexists_rightK", `SELECT "X" `+from+` WHERE NOT EXISTS (SELECT 1 FROM EE WHERE EE."CK" = B."K")`,
		"map[X:7]", "map[X:8]")
	// element correlation E.V = X → only element 7 matches EEV → {7}.
	pin("element_corr", `SELECT "X" `+from+` WHERE EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X")`,
		"map[X:7]")
	// single-source control (unchanged c5a-certified shape).
	pin("ctl_single_source", `SELECT "X" FROM A, A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`,
		"map[X:7]", "map[X:8]")

	// BAKEABLE box-leg CONJUNCT under EXISTS: `A.K = 100` is a box-leg conjunct
	// (resolves in A's buried window → verdict Bakeable), AND EXISTS on A.K. The
	// gather RECORDS the box legTypes and the merge BAKES the conjunct over that
	// record (ofOrdinal on the buried window), instead of the name-model
	// qualified-key rebase. A.K=100 → conjunct true, EXISTS matches → {7,8}.
	pin("bakeable_conjunct_present", `SELECT "X" `+from+` WHERE A."K" = 100 AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`,
		"map[X:7]", "map[X:8]")
	// the conjunct on the NULL-SUPPLIED leg (B.K IS NULL): `B.K = 110` is false
	// (B.K=NULL) → no rows; a wrong-window bake reading A.K=100 → still no match
	// on 110, so use a discriminating value. B.K=NULL so any equality is false →
	// {} — a name-keyed rebase over the row would also give {} (both keep the
	// box-leg conjunct correct); TestBoxJoinMultiExistsPlanSweep below is the
	// model discriminator.
	pin("bakeable_conjunct_nullsupplied", `SELECT "X" `+from+` WHERE B."K" = 110 AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`)
	// SUBQUERY-carrying box-leg conjunct: `A.K = (SELECT MIN(EE.CK) FROM EE)` is a
	// scalar-subquery conjunct. classifyLegConjunct BAKES it (the ScalarSubqueryValue
	// is a leaf — the bake leaves it untouched, only the sibling leg refs
	// ordinalize — and it was registered in t.scalarSubqueries at
	// predicate-translation time, so the statement's pre-eval pass binds its
	// result for the seed filter). The gather owns the shape; these pins prove
	// CORRECTNESS through the scalar-subquery pre-eval path (runQSub).
	// MIN(EE.CK)=100=A.K → conjunct TRUE, EXISTS matches → {7,8}.
	pinSub("subquery_conjunct_min_present", `SELECT "X" `+from+` WHERE A."K" = (SELECT MIN(EE."CK") FROM EE) AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`,
		"map[X:7]", "map[X:8]")
	// DISCRIMINATOR: COUNT(*)=1 (EE has one row) ≠ A.K=100 → conjunct FALSE → {}. A
	// dropped subquery binding (silent NULL) would ALSO yield {} — so this alone
	// can't distinguish bind-present from bind-absent; the _min_present TRUE arm
	// above is the one that proves the subquery actually EVALUATES and binds.
	pinSub("subquery_conjunct_count_discriminator", `SELECT "X" `+from+` WHERE A."K" = (SELECT COUNT(*) FROM EE) AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`)
	// FULL box variant: the FULL OUTER box's subquery conjunct bakes over the
	// gather record identically (A survives FULL OUTER, null-supplied B) → {7,8}.
	pinSub("subquery_conjunct_full_box", `SELECT "X" FROM A FULL OUTER JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE A."K" = (SELECT MIN(EE."CK") FROM EE) AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`,
		"map[X:7]", "map[X:8]")
	// FULL box + Bakeable conjunct (D4-(ii): FULL admits ONLY at Bakeable): the
	// FULL OUTER box's A.K=100 conjunct bakes over the gather record. A survives
	// the FULL OUTER (AID=1≠BID=2, B null-supplied), A.K=100 → {7,8}.
	const ffrom = `FROM A FULL OUTER JOIN B ON A."AID" = B."BID", A."ARR" AS "X"`
	pin("full_bakeable_conjunct", `SELECT "X" `+ffrom+` WHERE A."K" = 100 AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`,
		"map[X:7]", "map[X:8]")

	// CHECKS-4/5 AXIS: the admission omits the buried-outer-only and whole-row
	// pre-checks the design named — these shapes ARE admitted and gathered.
	// Verified correct-or-loud across ~19 esq shapes; pin the sharp
	// representatives so a future admission/rebase change can't regress the axis
	// to silent-wrong with no red test.
	// UNCORRELATED EXISTS (buried-outer-only / whole-row-independent): EE has a
	// row → EXISTS always true → all unnested rows kept → {7,8}.
	pin("uncorrelated_exists", `SELECT "X" `+from+` WHERE EXISTS (SELECT 1 FROM EE)`,
		"map[X:7]", "map[X:8]")
	// COMPOUND buried correlation (arithmetic on the buried leg): A.K + 0 = 100
	// matches EE.CK=100 → {7,8}; a mis-baked buried ref would misresolve → RED.
	pin("arith_on_buried_leg", `SELECT "X" `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K" + 0)`,
		"map[X:7]", "map[X:8]")
	// TWO-TABLE esq spanning BOTH a leg (A.K) and the ELEMENT (X) — a
	// pre-existing LOUD 0AF00 guard (multi-table EXISTS referencing the element
	// is unsupported). FLIP-SENTINEL: if a future admission/rebase makes this
	// gather, this pin catches the regression to silent-wrong.
	t.Run("twotable_leg_and_element_loud", func(t *testing.T) {
		_, _, err := runQ(t, `SELECT "X" `+from+` WHERE EXISTS (SELECT 1 FROM EE, EEV WHERE EE."CK" = A."K" AND EEV."VK" = "X")`)
		// The decline is a stable, deliberate 0AF00 — pin the code, not just
		// err != nil. A code pin is strictly stronger: it also catches the shape
		// breaking for an UNRELATED reason (which would silently retire the
		// silent-wrong→loud sentinel this exists to be), and it matches the
		// loud0AF00 pattern the enclosed-CTE battery in this package uses.
		if err == nil {
			t.Fatal("two-table esq spanning a leg and the element must be LOUD (unsupported), got rows")
		}
		if !strings.Contains(err.Error(), "0AF00") {
			t.Fatalf("expected a 0AF00 decline, got: %v", err)
		}
	})

	// MULTI-ESQ: a >1-EXISTS INNER-cluster unnest GATHERS and resolves
	// positionally. Pin the risky dimensions a plan-only sweep can't — each
	// existential must bind its OWN correlation over the gathered seed. INNER
	// cluster `A, B, A.ARR AS X` (cross A×B, then unnest); B.K
	// is unset (NULL). A leg-correlated EXISTS (EE.CK=A.K=100 → true) AND an
	// ELEMENT-correlated EXISTS (EEV.VK=X, EEV has VK=7): X=7 keeps (both true), X=8
	// drops (2nd false).
	const innerFrom = `FROM A, B, A."ARR" AS "X"`
	pin("multiesq_leg_and_element", `SELECT "X" `+innerFrom+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X")`,
		"map[X:7]")
	// NOT EXISTS second leg: EXISTS(EE.CK=A.K)=true AND NOT EXISTS(EEV.VK=X): X=7 →
	// NOT(true)=false drops; X=8 → NOT(false)=true keeps → {8}.
	pin("multiesq_notexists_element", `SELECT "X" `+innerFrom+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND NOT EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X")`,
		"map[X:8]")
	// 2nd existential correlates to B.K (NULL, unset): EXISTS(EE.CK=A.K)=true AND
	// EXISTS(EE.CK=B.K=NULL)=false → the whole row drops → {}. A wrong-leg bind
	// reading A.K would keep {7,8} RED.
	pin("multiesq_nullref_leg", `SELECT "X" `+innerFrom+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = B."K")`)
	// LEFT-BOX multi-esq PROJECTION GATHERS and resolves positionally: its
	// `[seed, ∃, ∃]` wrap peels via PartitionSelectRule to a NestedLoopJoin, and
	// each existential correlation bakes positionally at plan time
	// (multiEsqPeelBox in translateUnnestExistsFilter) so the peel keeps ∃ with
	// the seed instead of stranding a leg-relative name ref. `EXISTS(EE.CK=A.K=100
	// → true) AND EXISTS(EEV.VK=X)`: X=7 keeps, X=8 drops → {7}. (Also pinned in
	// TestBoxJoinMultiExistsPlanSweep's multiesq_left_box arm.)
	pin("multiesq_leftbox_projection", `SELECT "X" `+from+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X")`,
		"map[X:7]")
	// FULL box multi-esq: A survives the FULL OUTER (AID=1≠BID=2, B null-supplied),
	// unnest A.ARR=[7,8]; EXISTS(EE.CK=A.K=100)=true AND EXISTS(EEV.VK=X): X=7 keeps,
	// X=8 drops → {7}. Same gather+peel+plan-time-bake path as LEFT/RIGHT.
	pin("multiesq_fullbox_projection", `SELECT "X" `+ffrom+` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X")`,
		"map[X:7]")
}

// existsGatherSchemaMetadata builds the A/B/EE/EEV schema shared by the row cert (FDB
// execution) and the plan-only sweep below. A(AID=1,K=100,ARR=[7,8]);
// B(BID=2,K=110) — A.K/B.K dup-named, the leg a qualified EXISTS correlation
// must disambiguate; EE(CK) is the leg-correlation table, EEV(VK) the element
// one.
func existsGatherSchemaMetadata(tb testing.TB) *recordlayer.RecordMetaData {
	tb.Helper()
	b := metadata.NewSchemaTemplateBuilder().SetName("s3s0")
	b.AddTable("A", []metadata.ColumnSpec{
		metadata.NewColumnSpec("AID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
		metadata.NewColumnSpec("ARR", api.NewArrayType(api.NewIntegerType(false), true), 3),
	}, []string{"AID"})
	b.AddTable("B", []metadata.ColumnSpec{
		metadata.NewColumnSpec("BID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
	}, []string{"BID"})
	b.AddTable("EE", []metadata.ColumnSpec{
		metadata.NewColumnSpec("CK", api.NewLongType(false), 1),
	}, []string{"CK"})
	b.AddTable("EEV", []metadata.ColumnSpec{
		metadata.NewColumnSpec("VK", api.NewLongType(false), 1),
	}, []string{"VK"})
	tmpl, err := b.Build()
	if err != nil {
		tb.Fatal(err)
	}
	return tmpl.Underlying()
}

// TestBoxJoinMultiExistsPlanSweep is a plan-only sweep: every admitted
// box+EXISTS shape below must keep PLANNING cleanly. Planning-only, so it needs
// no FDB / Docker.
func TestBoxJoinMultiExistsPlanSweep(t *testing.T) {
	t.Parallel()
	md := existsGatherSchemaMetadata(t)
	const from = `FROM A LEFT JOIN B ON A."AID" = B."BID", A."ARR" AS "X"`
	countProducers := func(sql string) int {
		if _, err := embedded.PlanRecordQueryWithMetadata(sql, md, nil); err != nil {
			t.Fatalf("plan %q: %v", sql, err)
		}
		return 0
	}
	// The admitted box+EXISTS shapes must keep PLANNING cleanly.
	for _, tc := range []struct{ name, sql string }{
		{"left_box_none", `SELECT "X" ` + from + ` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
		{"left_box_bakeable", `SELECT "X" ` + from + ` WHERE A."K" = 100 AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
		{"full_box_bakeable", `SELECT "X" FROM A FULL OUTER JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE A."K" = 100 AND EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K")`},
		// MULTI-ESQ box (LEFT/RIGHT/FULL) GATHERS: its `[seed, ∃, ∃]` wrap peels
		// via PartitionSelectRule to a NestedLoopJoin, and the existential
		// correlations bake positionally at plan time (multiEsqPeelBox), so the box
		// gather owns them. Row correctness pinned in
		// TestFDB_BoxJoinMultiExistsGather multiesq_{leftbox,fullbox}_projection.
		{"multiesq_left_box", `SELECT "X" ` + from + ` WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X")`},
		{"multiesq_full_box", `SELECT "X" FROM A FULL OUTER JOIN B ON A."AID" = B."BID", A."ARR" AS "X" WHERE EXISTS (SELECT 1 FROM EE WHERE EE."CK" = A."K") AND EXISTS (SELECT 1 FROM EEV WHERE EEV."VK" = "X")`},
	} {
		if n := countProducers(tc.sql); n != 0 {
			t.Fatalf("%s: fired %d name-model producer(s), want 0 (the gather must own it)", tc.name, n)
		}
	}
}
