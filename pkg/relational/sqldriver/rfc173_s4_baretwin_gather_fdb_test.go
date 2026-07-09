package sqldriver_test

import (
	"context"
	"fmt"
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

// TestFDB_RFC173S4_BareTwinGather is the certificate for RFC-173 S4 Slice 2a: a
// multi-source lateral unnest whose two legs SHARE a bare column name (the
// name-ambiguous bare-twin, `FROM A, B, A.ARR AS X` with A,B both carrying K) now
// GATHERS via the RAW seed instead of declining to name-model — narrowing :1151's
// bare-twin residency for the CROSS-LEG case.
//
// It gathers via the RAW seed, NOT a positional wrap: each plain leg is its own
// quantifier (Scan(A), Scan(B)) in the flat select, so a QUALIFIED `A.K`/`B.K` read
// routes to its own scan — in the SELECT list, an outer WHERE, and a cross-leg
// predicate alike. An earlier wrap attempt was BOTH unnecessary (the raw seed
// already disambiguates qualified reads) and wrong (its bare pass-through key made
// `WHERE B.K=200` resolve to first-match A.K=100 and drop every row) — the
// where_on_* subtest pins those cases as regressions.
//
// A=(1,100,[7,8]); B=(1,200); C=(1,55). A,B share K; C has a distinct M.
func TestFDB_RFC173S4_BareTwinGather(t *testing.T) {
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

	b := metadata.NewSchemaTemplateBuilder().SetName("baretwin2a")
	b.AddTable("A", []metadata.ColumnSpec{
		metadata.NewColumnSpec("AID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
		metadata.NewColumnSpec("ARR", api.NewArrayType(api.NewIntegerType(false), true), 3),
	}, []string{"AID"})
	b.AddTable("B", []metadata.ColumnSpec{
		metadata.NewColumnSpec("BID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
	}, []string{"BID"})
	b.AddTable("C", []metadata.ColumnSpec{
		metadata.NewColumnSpec("CID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("M", api.NewLongType(true), 2),
	}, []string{"CID"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	md := tmpl.Underlying()

	aDesc := md.GetRecordType("A").Descriptor
	a := dynamicpb.NewMessage(aDesc)
	a.Set(aDesc.Fields().ByName("AID"), protoreflect.ValueOfInt64(1))
	a.Set(aDesc.Fields().ByName("K"), protoreflect.ValueOfInt64(100))
	arrFd := aDesc.Fields().ByName("ARR")
	arr := a.NewField(arrFd).List()
	arr.Append(protoreflect.ValueOfInt32(7))
	arr.Append(protoreflect.ValueOfInt32(8))
	a.Set(arrFd, protoreflect.ValueOfList(arr))

	bDesc := md.GetRecordType("B").Descriptor
	bm := dynamicpb.NewMessage(bDesc)
	bm.Set(bDesc.Fields().ByName("BID"), protoreflect.ValueOfInt64(1))
	bm.Set(bDesc.Fields().ByName("K"), protoreflect.ValueOfInt64(200))

	cDesc := md.GetRecordType("C").Descriptor
	cm := dynamicpb.NewMessage(cDesc)
	cm.Set(cDesc.Fields().ByName("CID"), protoreflect.ValueOfInt64(1))
	cm.Set(cDesc.Fields().ByName("M"), protoreflect.ValueOfInt64(55))

	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Create()
		if sErr != nil {
			return nil, sErr
		}
		for _, r := range []proto.Message{a, bm, cm} {
			if _, e := store.SaveRecord(r); e != nil {
				return nil, e
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	run := func(t *testing.T, q string) (string, []string) {
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
		sort.Strings(out)
		return explain, out
	}
	wantRows := func(t *testing.T, q string, want []string) string {
		t.Helper()
		explain, got := run(t, q)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("rows = %v, want %v\n  sql: %s\n  plan: %s", got, want, q, explain)
		}
		return explain
	}

	// The QUALIFIED bare-twin `SELECT A.K, B.K, X` — dup name K resolves by QUANTIFIER
	// (A.K→Scan(A), B.K→Scan(B)), not last-leg-wins. A×B×unnest = 2 rows. It gathers
	// via the RAW seed — a SINGLE user projection, NO positional wrap.
	t.Run("qualified_bare_twin_resolves_by_quantifier", func(t *testing.T) {
		explain := wantRows(t, `SELECT A."K", B."K", "X" FROM A, B, A."ARR" AS "X"`,
			[]string{"map[A.K:100 B.K:200 X:7]", "map[A.K:100 B.K:200 X:8]"})
		// RAW seed: one user projection, no nested positional wrap.
		if strings.Count(explain, "Project(") != 1 {
			t.Fatalf("bare-twin must gather via the raw seed (single Project, no wrap); plan=%s", explain)
		}
	})

	// The BARE element X resolves through the raw seed even though the leg columns are
	// dup-named (the element quantifier is distinct from A and B).
	t.Run("bare_element_resolves", func(t *testing.T) {
		wantRows(t, `SELECT "X" FROM A, B, A."ARR" AS "X"`, []string{"map[X:7]", "map[X:8]"})
	})

	// A WHERE on a QUALIFIED bare-twin column resolves against its own quantifier — on
	// the FIRST leg (A.K) AND on a LATER leg (B.K), and in a cross-leg predicate. This
	// pins the regression a positional wrap introduced: the wrap's bare pass-through
	// key made `WHERE B.K=200` resolve to first-match (A.K=100) and drop every row.
	t.Run("where_on_qualified_bare_twin_resolves", func(t *testing.T) {
		wantRows(t, `SELECT "X" FROM A, B, A."ARR" AS "X" WHERE A."K" = 100`, []string{"map[X:7]", "map[X:8]"})
		wantRows(t, `SELECT "X" FROM A, B, A."ARR" AS "X" WHERE A."K" = 999`, nil)
		// The wrap-regression cases: a filter on the SECOND leg's dup column, and a
		// cross-leg dup predicate. Both must route B.K to Scan(B) (=200), not A.K.
		wantRows(t, `SELECT "X" FROM A, B, A."ARR" AS "X" WHERE B."K" = 200`, []string{"map[X:7]", "map[X:8]"})
		wantRows(t, `SELECT "X" FROM A, B, A."ARR" AS "X" WHERE B."K" = 999`, nil)
		wantRows(t, `SELECT "X" FROM A, B, A."ARR" AS "X" WHERE A."K" <> B."K"`, []string{"map[X:7]", "map[X:8]"})
	})

	// GROUPED bare-twin: DECLINES to name-model (the deferred grouped-bare-twin
	// increment). The grouped path is stuck between the wrap (name-blind to qualifiers
	// — collapses the dup to first-match) and the raw seed (a GROUP BY over it groups
	// under NULL — the shadow-key defect), so a non-box cross-leg dup under aggregate
	// declines; name-model resolves the qualified group key. COUNT over the 2 unnest rows.
	t.Run("grouped_bare_twin", func(t *testing.T) {
		wantRows(t, `SELECT A."K", COUNT(*) FROM A, B, A."ARR" AS "X" GROUP BY A."K"`,
			[]string{"map[A.K:100 COUNT(*):2]"})
	})

	// GROUPED bare-twin with a cross-leg WHERE — the outer-filter-under-aggregate axis.
	// Before Slice 2a a cross-leg dup declined BEFORE the aggregate split, so the group-by
	// wrap never saw a bare-twin; the wrap approach (and the raw-seed pivot's first cut)
	// let a grouped
	// bare-twin reach the wrap, whose bare pass-through key mis-bound an outer filter (the
	// outer-filter first-match wrong-rows class) — `WHERE B.K=200` collapsed to A.K=100 → []).
	// The fix DECLINES the grouped bare-twin to name-model, which resolves qualified reads
	// on BOTH legs: B.K=200 matches → A.K=100 groups the 2 unnest rows; B.K=999 drops all.
	// This pins that the grouped path does NOT ship the NAK'd wrong rows.
	t.Run("grouped_bare_twin_cross_leg_where", func(t *testing.T) {
		wantRows(t, `SELECT A."K", COUNT(*) FROM A, B, A."ARR" AS "X" WHERE B."K" = 200 GROUP BY A."K"`,
			[]string{"map[A.K:100 COUNT(*):2]"})
		wantRows(t, `SELECT A."K", COUNT(*) FROM A, B, A."ARR" AS "X" WHERE B."K" = 999 GROUP BY A."K"`, nil)
	})

	// COMPOUND aggregate operand on a DUP column: SUM(A.K + B.K) over the grouped
	// bare-twin must bake BOTH nested leaves — A.K→A's slot, B.K→B's slot. The operand
	// bake RECURSES the value tree (a top-level-only bake would leave B.K reading
	// first-match A.K → silent wrong rows). A.K=100, B.K=200: each of the 2 unnest rows
	// is 100+200=300 → SUM=600 (a top-level-only bug would give 100+100=200 → 400).
	t.Run("grouped_compound_operand_on_dup_column", func(t *testing.T) {
		wantRows(t, `SELECT A."K", SUM(A."K" + B."K") AS "TOT" FROM A, B, A."ARR" AS "X" GROUP BY A."K"`,
			[]string{`map[A.K:100 SUM(A."K" + B."K"):600 TOT:600]`})
	})

	// MID-LIST element grouped bare-twin (`FROM A, A.arr AS X, B` — the unnest is NOT
	// trailing; B follows it). The element splits the leg run, so the leg windows come
	// from the element-ANYWHERE layout authority (OrdinalSeedLegWindows walks leading
	// legs, steps over the element's slot, windows trailing legs). A.K and B.K (the dup)
	// each route to their own window: `GROUP BY A.K` reads A's K (=100); `WHERE B.K=200`
	// reads B's K. A(1 row) × {7,8} × B(1 row) = 2 rows → COUNT=2. Pins that a mid-list
	// cross-leg dup WORKS (not the fail-open decline — the authority windows it).
	t.Run("mid_list_element_grouped_bare_twin", func(t *testing.T) {
		wantRows(t, `SELECT A."K", COUNT(*) FROM A, A."ARR" AS "X", B GROUP BY A."K"`,
			[]string{"map[A.K:100 COUNT(*):2]"})
		wantRows(t, `SELECT A."K", COUNT(*) FROM A, A."ARR" AS "X", B WHERE B."K" = 200 GROUP BY A."K"`,
			[]string{"map[A.K:100 COUNT(*):2]"})
		wantRows(t, `SELECT A."K", COUNT(*) FROM A, A."ARR" AS "X", B WHERE B."K" = 999 GROUP BY A."K"`, nil)
	})

	// ORDER BY on a bare-twin dup column resolves each qualifier to its own quantifier.
	// This asserts RESOLUTION only (the row set), NOT the sort ORDER — A has a single K
	// value here (100), so it cannot discriminate direction. That was the fake-green trap:
	// a set-only assertion on single-value K read as "ORDER BY covered" while the sort key
	// silently evaluated to a dead constant (DESC == ASC). The DISCRIMINATING ORDER
	// dimension — distinct sort values so DESC ≠ ASC, across dup/non-dup/3-leg/element/box-
	// FULL-NULL/compound/multi-key, with NULL ordering matched to single-source — lives in
	// TestFDB_RFC173S4_OrderByGather. Keep this as the bare-twin A/B/C-schema resolution pin.
	t.Run("order_by_bare_twin_resolves", func(t *testing.T) {
		wantRows(t, `SELECT "X" FROM A, B, A."ARR" AS "X" ORDER BY A."K", "X"`,
			[]string{"map[X:7]", "map[X:8]"})
		wantRows(t, `SELECT "X" FROM A, B, A."ARR" AS "X" ORDER BY B."K", "X"`,
			[]string{"map[X:7]", "map[X:8]"})
	})

	// A BARE ambiguous reference errors 42702 at semantic analysis — BEFORE the
	// translator, so no last-leg-wins-vs-first-match divergence reaches the wrap.
	t.Run("bare_ambiguous_reference_errors_42702", func(t *testing.T) {
		_, err := embedded.PlanRecordQueryWithMetadata(`SELECT "K" FROM A, B, A."ARR" AS "X"`, md, nil)
		if err == nil {
			t.Fatalf("bare ambiguous K must error at plan time")
		}
		requireSQLSTATE(t, err, api.ErrCodeAmbiguousColumn)
	})

	// NON-AMBIGUOUS gather (A,C — distinct names) keeps its RAW seed: no wrap, plan
	// byte-identical to today (a single user projection, no nested positional Project).
	t.Run("non_ambiguous_gather_is_byte_identical_raw_seed", func(t *testing.T) {
		explain := wantRows(t, `SELECT A."K", C."M", "X" FROM A, C, A."ARR" AS "X"`,
			[]string{"map[A.K:100 C.M:55 X:7]", "map[A.K:100 C.M:55 X:8]"})
		if strings.Count(explain, "Project(") != 1 {
			t.Fatalf("non-ambiguous gather must keep the raw seed (single Project, no wrap); plan=%s", explain)
		}
	})

	// WITHIN-BOX buried dup (`(A FULL OUTER B), A.ARR AS X` — A,B share K in ONE box
	// leg's concat) is OUT of Slice 2a scope: it DECLINES to name-model (a positional
	// disambiguation of a buried box dup is a later increment) and still answers.
	t.Run("within_box_buried_dup_declines_to_name_model", func(t *testing.T) {
		wantRows(t, `SELECT "X" FROM A FULL OUTER JOIN B ON A."AID" = B."BID", A."ARR" AS "X"`,
			[]string{"map[X:7]", "map[X:8]"})
	})

	// GROUPED over a NAME-MODEL FALLBACK: the within-box dup declines the gather to
	// name-model, but the fallback is STILL a SelectExpression with an Explode
	// quantifier — its RC is ANCHORED and emits NO positional row. The group-by MUST
	// NOT positionally bake `GROUP BY X` over it (a baked ordinal on the Datum map
	// would error); the `!rc.AnchoredJoin` gate keeps it name-model. A FULL B matches
	// one row; A.arr={7,8} → GROUP BY X yields 7,8 each COUNT 1. Regression pin for the
	// anchored-fallback bake gate.
	t.Run("grouped_over_name_model_fallback_does_not_bake", func(t *testing.T) {
		wantRows(t, `SELECT "X", COUNT(*) FROM A FULL OUTER JOIN B ON A."AID" = B."BID", A."ARR" AS "X" GROUP BY "X"`,
			[]string{"map[COUNT(*):1 X:7]", "map[COUNT(*):1 X:8]"})
	})

	// BOX-INVOLVED CROSS-LEG dup (RFC-173 S4 class-2 lift): a merge-opaque box `(A LEFT C)`
	// BURIES A.K, and a SEPARATE scan leg B also carries K → the dup crosses legs with one
	// leg a box. Unlike a within-box dup, the two K's live in DIFFERENT legs, so each keeps
	// its own [Offset,Width) window — A.K via the box's buried-leaf sub-window
	// (finalizeSeedWindows), B.K via its scan run — and a qualified read routes by SLOT, not
	// first-match. It GATHERS the raw seed and returns correct rows (no first-match wrong
	// rows). A LEFT C matches (AID=1=CID=1), A.arr={7,8} × B(1) → X∈{7,8}. The richer
	// discriminating pins (FULL-NULL grouped, cross-leg-buried-predicate, box-dup ORDER BY)
	// live in TestFDB_RFC173S4_BoxDupClass2. A WITHIN-box dup (two same-named columns in ONE
	// box) remains its own declining increment — sentineled by
	// TestRFC173W5_Gathered_DeclineBoundary case (b2-within).
	t.Run("box_involved_cross_leg_dup_gathers_correct_rows", func(t *testing.T) {
		wantRows(t, `SELECT "X" FROM A LEFT OUTER JOIN C ON A."AID" = C."CID", B, A."ARR" AS "X"`,
			[]string{"map[X:7]", "map[X:8]"})
	})
}
