package sqldriver_test

import (
	"context"
	"fmt"
	"slices"
	"sort"
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

// TestFDB_RFC173S4_BoxDupBuriedElementPredicate certifies the buried-element predicate
// fix: a WHERE conjunct referencing a BURIED box column AND the unnest element (`WHERE
// A.K = X`, A buried in `(A FULL C)`, X the element of C.arr) over a MULTI-LEG box gather
// (box + a sibling scan + unnest) must POSITIONALLY BAKE the buried A.K to the box
// quantifier's buried-leaf slot before the conjunct reaches the Explode filter. Pre-fix
// it was left lazy — the buried alias A has no quantifier of its own (the box's quantifier
// holds its slot) — so QOV(A) was unbound → NULL = X → ALL rows silently dropped.
//
// This is the third consumer of the qualifier-honoring bake (after GROUP BY / ORDER BY):
// bakeGatedJoinPredicates skipped a single-leg predicate as pushdown-safe, but a
// single-leg-BURIED predicate has no per-leg select to push into. The fix bakes it via
// the SAME machinery (predicateRefsBuriedLeg lifts the skip). It was a pre-existing
// gather-general bug — the NON-DUP box+sibling gather shipped it too; the box-dup class-2
// lift merely widened the affected set.
//
// Every pin DISCRIMINATES: A.K=7 matches the element value 7, so the correct answer is a
// single row {A.K:7, X:7}; a mis-resolution (unbound buried A.K) yields [].
//
//	A(AID,K):   (1,7) (2,300)              -- buried dup column; A.K=7 matches element 7
//	C(CID,ARR): (1,[7,8]) (2,[9]) (5,[55]) -- owns array; join A.AID=C.CID
//	B(BID,K):   (1,200)                    -- scan sibling; B.K=200 dups A.K (cross-leg)
//	E(EID,EV):  (1,999)                    -- NON-dup sibling (isolates: bug is not dup-specific)
//
// Box (A FULL C): A.K=7/ARR[7,8]; A.K=300/ARR[9]; A.K=NULL(padded)/ARR[55].
func TestFDB_RFC173S4_BoxDupBuriedElementPredicate(t *testing.T) {
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

	b := metadata.NewSchemaTemplateBuilder().SetName("bdbe")
	b.AddTable("A", []metadata.ColumnSpec{
		metadata.NewColumnSpec("AID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
	}, []string{"AID"})
	b.AddTable("C", []metadata.ColumnSpec{
		metadata.NewColumnSpec("CID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("ARR", api.NewArrayType(api.NewIntegerType(false), true), 2),
	}, []string{"CID"})
	b.AddTable("B", []metadata.ColumnSpec{
		metadata.NewColumnSpec("BID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
	}, []string{"BID"})
	b.AddTable("E", []metadata.ColumnSpec{
		metadata.NewColumnSpec("EID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("EV", api.NewLongType(true), 2),
	}, []string{"EID"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	md := tmpl.Underlying()

	mk2 := func(table, f1, f2 string, v1, v2 int64) proto.Message {
		d := md.GetRecordType(table).Descriptor
		m := dynamicpb.NewMessage(d)
		m.Set(d.Fields().ByName(protoreflect.Name(f1)), protoreflect.ValueOfInt64(v1))
		m.Set(d.Fields().ByName(protoreflect.Name(f2)), protoreflect.ValueOfInt64(v2))
		return m
	}
	mkC := func(cid int64, vals ...int32) proto.Message {
		d := md.GetRecordType("C").Descriptor
		m := dynamicpb.NewMessage(d)
		m.Set(d.Fields().ByName("CID"), protoreflect.ValueOfInt64(cid))
		fd := d.Fields().ByName("ARR")
		l := m.NewField(fd).List()
		for _, v := range vals {
			l.Append(protoreflect.ValueOfInt32(v))
		}
		m.Set(fd, protoreflect.ValueOfList(l))
		return m
	}

	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Create()
		if sErr != nil {
			return nil, sErr
		}
		recs := []proto.Message{
			mk2("A", "AID", "K", 1, 7), mk2("A", "AID", "K", 2, 300),
			mkC(1, 7, 8), mkC(2, 9), mkC(5, 55),
			mk2("B", "BID", "K", 1, 200),
			mk2("E", "EID", "EV", 1, 999),
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

	wantSet := func(t *testing.T, q string, want []string) {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadata(q, md, nil)
		if perr != nil {
			t.Fatalf("plan %q: %v", q, perr)
		}
		explain := plan.Explain()
		var got []string
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
				got = append(got, fmt.Sprintf("%v", r.Datum))
			}
			return nil, nil
		})
		if eerr != nil {
			t.Fatalf("exec %q: %v", q, eerr)
		}
		sort.Strings(got)
		if !slices.Equal(got, want) {
			t.Fatalf("rows = %v, want %v\n  sql: %s\n  plan: %s", got, want, q, explain)
		}
	}

	one := []string{"map[A.K:7 X:7]"} // the discriminating single row (mis-resolution → nil)

	// PLACEMENT AXIS (c) — buried-vs-ELEMENT, the gated path the fix wires. Box-DUP
	// (A.K dups scan B.K) AND NON-DUP (sibling E, no dup) BOTH resolve — proving the
	// general fix, not muted for one instance (the non-dup case flipped [] → correct).
	t.Run("buried_element_box_dup", func(t *testing.T) {
		wantSet(t, `SELECT A."K", "X" FROM A FULL OUTER JOIN C ON A."AID" = C."CID", B, C."ARR" AS "X" WHERE A."K" = "X"`, one)
	})
	t.Run("buried_element_non_dup", func(t *testing.T) {
		wantSet(t, `SELECT A."K", "X" FROM A FULL OUTER JOIN C ON A."AID" = C."CID", E, C."ARR" AS "X" WHERE A."K" = "X"`, one)
	})
	// ENCLOSED entry point (unnest NOT last — B follows it): routes through the enclosed
	// merge arm, a SECOND dispatch path that builds its OWN buried windows.
	t.Run("buried_element_enclosed", func(t *testing.T) {
		wantSet(t, `SELECT A."K", "X" FROM A FULL OUTER JOIN C ON A."AID" = C."CID", C."ARR" AS "X", B WHERE A."K" = "X"`, one)
	})
	// COMPOUND buried-element (`A.K + 0 = X`): the arithmetic recurses and bakes the
	// buried A.K leaf — a non-recursing bake leaves it lazy → [].
	t.Run("buried_element_compound", func(t *testing.T) {
		wantSet(t, `SELECT A."K", "X" FROM A FULL OUTER JOIN C ON A."AID" = C."CID", B, C."ARR" AS "X" WHERE A."K" + 0 = "X"`, one)
	})

	// PLACEMENT AXIS (a) — buried-vs-LITERAL pushes INTO the box (not the Explode filter);
	// must stay correct (not regressed). A.K=7 keeps both its elements.
	t.Run("placement_a_buried_literal_preserved", func(t *testing.T) {
		wantSet(t, `SELECT A."K", "X" FROM A FULL OUTER JOIN C ON A."AID" = C."CID", B, C."ARR" AS "X" WHERE A."K" = 7`,
			[]string{"map[A.K:7 X:7]", "map[A.K:7 X:8]"})
	})
	// PLACEMENT AXIS (b) — buried-vs-SCAN realizes as a JOIN predicate; must stay correct.
	// A.K∈{7,300,NULL} <> B.K=200: keeps 7 & 300, drops NULL (NULL<>200=NULL).
	t.Run("placement_b_buried_scan_preserved", func(t *testing.T) {
		wantSet(t, `SELECT A."K", "X" FROM A FULL OUTER JOIN C ON A."AID" = C."CID", B, C."ARR" AS "X" WHERE A."K" <> B."K"`,
			[]string{"map[A.K:300 X:9]", "map[A.K:7 X:7]", "map[A.K:7 X:8]"})
	})

	// FULL-NULL both ways: `A.K = X` DROPS the padded A.K=NULL row (NULL=55 → NULL,
	// three-valued); `A.K IS NULL` KEEPS it — proving the padded buried A.K resolves
	// through the gather (a spurious match or unresolved read would diverge).
	t.Run("full_null_element_drops", func(t *testing.T) {
		// The padded A.K=NULL/X=55 row is NOT in the `A.K=X` result (only {7,7}).
		wantSet(t, `SELECT A."K", "X" FROM A FULL OUTER JOIN C ON A."AID" = C."CID", B, C."ARR" AS "X" WHERE A."K" = "X"`, one)
	})
	t.Run("full_null_is_null_keeps", func(t *testing.T) {
		wantSet(t, `SELECT A."K", "X" FROM A FULL OUTER JOIN C ON A."AID" = C."CID", B, C."ARR" AS "X" WHERE A."K" IS NULL`,
			[]string{"map[A.K:<nil> X:55]"})
	})

	// NO-REGRESSION controls: box-only (no sibling → name-model path, bake not called) and
	// plain multi-source (sibling has its own quantifier) both already resolved and stay so.
	t.Run("box_only_control", func(t *testing.T) {
		wantSet(t, `SELECT A."K", "X" FROM A FULL OUTER JOIN C ON A."AID" = C."CID", C."ARR" AS "X" WHERE A."K" = "X"`, one)
	})
	t.Run("plain_sibling_control", func(t *testing.T) {
		wantSet(t, `SELECT A."K", "X" FROM A, C, C."ARR" AS "X" WHERE A."K" = "X"`, one)
	})

	// A buried-element ON conjunct is UNREACHABLE via supported SQL: the element requires a
	// comma-lateral unnest, and a JOIN whose ON could see it must mix with that comma —
	// rejected 0A000 ("JOIN clauses on comma-separated FROM sources are not supported").
	// The WHERE source is the reachable predicate dimension; pinned above.
	t.Run("buried_element_on_conjunct_unreachable", func(t *testing.T) {
		_, perr := embedded.PlanRecordQueryWithMetadata(
			`SELECT A."K", "X" FROM A FULL OUTER JOIN C ON A."AID" = C."CID", C."ARR" AS "X" JOIN B ON A."K" = "X"`, md, nil,
		)
		if perr == nil {
			t.Fatalf("a buried-element ON on a comma-lateral unnest is expected to be 0A000-unsupported; if it now plans, pin its rows")
		}
		requireSQLSTATE(t, perr, "0A000")
	})
}
