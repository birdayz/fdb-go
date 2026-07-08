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
// GATHERS via the positional wrap (each leg's columns re-exposed as ALIAS.COL by
// its [Start,Width) window) instead of declining to name-model — narrowing :1151's
// bare-twin residency for the CROSS-LEG case.
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

	// The QUALIFIED bare-twin `SELECT A.K, B.K, X` — dup name K resolves by leg window
	// (A.K→its slot, B.K→its slot), not last-leg-wins. A×B×unnest = 2 rows.
	t.Run("qualified_bare_twin_resolves_by_window", func(t *testing.T) {
		explain := wantRows(t, `SELECT A."K", B."K", "X" FROM A, B, A."ARR" AS "X"`,
			[]string{"map[A.K:100 B.K:200 X:7]", "map[A.K:100 B.K:200 X:8]"})
		// The positional WRAP fired (an inner projection binding the dup columns by slot).
		if strings.Count(explain, "Project(") < 2 {
			t.Fatalf("bare-twin must gather via the positional wrap (nested Project); plan=%s", explain)
		}
	})

	// The BARE element X still resolves through the wrap (emitLeafKeys ADDS ALIAS.COL,
	// keeps the bare pass-through) even though the leg columns are dup-named.
	t.Run("bare_element_survives_the_wrap", func(t *testing.T) {
		wantRows(t, `SELECT "X" FROM A, B, A."ARR" AS "X"`, []string{"map[X:7]", "map[X:8]"})
	})

	// A WHERE on a QUALIFIED bare-twin column resolves against the wrap (the gathered
	// path is NOT chainedUnnestUnderFilter-suppressed — that flag reads only in the
	// chained path). A.K=100 matches; A.K=999 drops all.
	t.Run("where_on_qualified_bare_twin_resolves", func(t *testing.T) {
		wantRows(t, `SELECT "X" FROM A, B, A."ARR" AS "X" WHERE A."K" = 100`, []string{"map[X:7]", "map[X:8]"})
		wantRows(t, `SELECT "X" FROM A, B, A."ARR" AS "X" WHERE A."K" = 999`, nil)
	})

	// GROUPED bare-twin: GROUP BY A.K reads A's K slot; COUNT over the 2 unnest rows.
	t.Run("grouped_bare_twin", func(t *testing.T) {
		wantRows(t, `SELECT A."K", COUNT(*) FROM A, B, A."ARR" AS "X" GROUP BY A."K"`,
			[]string{"map[A.K:100 COUNT(*):2]"})
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
}
