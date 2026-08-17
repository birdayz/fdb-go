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
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// TestFDB_ProjectedExistsEnclosureLift is the certificate + forward sentinel
// for removing translateProjectOverExistsFilter's forced `inInnerCluster =
// true` on its f.Input.
//
// The removal is certified as follows: the old forcing was NEVER protective
// and, where it becomes observable, flipped the WRONG way. For an ENCLOSED
// multi-source lateral-unnest derived body with a conjunct spanning the
// unnest element and a second source (`FROM A, A.ARR AS X, B WHERE X =
// B.BK`), the old forcing SUPPRESSED translateFilter's enclosed-gather
// rotation (its `!t.inInnerCluster` guard) → the spanning conjunct would land
// on an unbound element → silent 0 rows; without the forcing the rotation
// FIRES → the body takes the gathered path that the DIRECT
// (non-derived-wrapped) form already answers correctly.
//
// Two invariants, forever:
//   - CONTROL: the DIRECT enclosed-unnest form (routes through translateFilter,
//     never through the removed forcing) answers the gathered row [X=7] and must
//     never regress. This is the "correct answer" the derived-wrapped form must
//     eventually match.
//   - FORWARD SENTINEL: the derived-wrapped projected-EXISTS form declines TODAY
//     (0AF00 — the existential fold's generic path over a LogicalCTE carrier does
//     not flatten the buried cluster; a separate, unrelated reach gap). Removing
//     the forcing makes this shape byte-identical (0AF00) with or without the
//     enclosure, so removing it is safe today. When that reach gap later closes
//     and this shape starts returning rows, THIS ASSERTION TRIPS — forcing that
//     author to confirm the rows are the gathered [X=7, EXISTS=true] (matching
//     CONTROL), never the suppressed silent-0. The sentinel is the guard against
//     silently shipping the wrong-direction treatment.
func TestFDB_ProjectedExistsEnclosureLift(t *testing.T) {
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

	b := metadata.NewSchemaTemplateBuilder().SetName("cl3234")
	b.AddTable("A", []metadata.ColumnSpec{
		metadata.NewColumnSpec("AID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
		metadata.NewColumnSpec("ARR", api.NewArrayType(api.NewIntegerType(false), true), 3),
	}, []string{"AID"})
	b.AddTable("B", []metadata.ColumnSpec{
		metadata.NewColumnSpec("BID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("BK", api.NewLongType(true), 2),
	}, []string{"BID"})
	b.AddTable("C", []metadata.ColumnSpec{
		metadata.NewColumnSpec("CID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("CK", api.NewLongType(true), 2),
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
	setArrayField(a, aDesc.Fields().ByName("ARR"),
		protoreflect.ValueOfInt32(7), protoreflect.ValueOfInt32(8))

	bDesc := md.GetRecordType("B").Descriptor
	bm := dynamicpb.NewMessage(bDesc)
	bm.Set(bDesc.Fields().ByName("BID"), protoreflect.ValueOfInt64(1))
	bm.Set(bDesc.Fields().ByName("BK"), protoreflect.ValueOfInt64(7))

	cDesc := md.GetRecordType("C").Descriptor
	cm := dynamicpb.NewMessage(cDesc)
	cm.Set(cDesc.Fields().ByName("CID"), protoreflect.ValueOfInt64(1))
	cm.Set(cDesc.Fields().ByName("CK"), protoreflect.ValueOfInt64(100))

	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Create()
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

	run := func(sqlText string) ([]string, error) {
		plan, perr := embedded.PlanRecordQueryWithMetadata(sqlText, md, nil)
		if perr != nil {
			return nil, perr
		}
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
				out = append(out, positionalNamedPipeSprint(r))
			}
			return nil, nil
		})
		if eerr != nil {
			return nil, eerr
		}
		sort.Strings(out)
		return out, nil
	}

	// CONTROL — the DIRECT enclosed-unnest form. Gathered path answers [X=7]
	// (A.ARR={7,8} × B.BK=7, WHERE X=B.BK → X=7). Must never regress; this is the
	// correct answer the derived-wrapped form must eventually match.
	t.Run("control_direct_unnest_gathers", func(t *testing.T) {
		rows, err := run(`SELECT "X" FROM A, A."ARR" AS "X", B WHERE "X" = B."BK"`)
		if err != nil {
			t.Fatalf("direct enclosed-unnest must plan+run (gathered path): %v", err)
		}
		if len(rows) != 1 || rows[0] != "X=7" {
			t.Fatalf("direct enclosed-unnest rows = %v, want [map[X:7]] (the gathered, spanning-conjunct-correct row)", rows)
		}
	})

	// THE SENTINEL TRIPPED, AND THIS IS ITS ROWS ASSERTION. The derived-wrapped
	// projected-EXISTS form used to decline 0AF00 — the derived-table schema
	// derivation could not describe a body whose FROM list carries a lateral
	// unnest leg, so the outer reference to `d` had nothing to resolve against.
	// It plans now, and what it must answer is the CONTROL's gathered row with a
	// TRUE beside it: A.K=100 matches C.CK=100, so the EXISTS holds on the one
	// row the gather produces.
	//
	// The row COUNT and the EXISTS value are the assertion, not just "it plans".
	// The failure this sentinel was posted against is a SILENT ZERO — the
	// enclosure forcing this shape used to carry would have suppressed the gather
	// entirely — and a zero-row answer is indistinguishable from a working query
	// over empty data unless the count is pinned.
	//
	// The second column's LABEL is not pinned. An unaliased projected EXISTS is
	// named by the machinery, and that spelling is not what this test is about;
	// its VALUE is.
	t.Run("derived_wrapped_projexists_gathers_like_the_control", func(t *testing.T) {
		rows, err := run(`SELECT d."X", EXISTS (SELECT 1 FROM C WHERE C."CK" = d."K") FROM ` +
			`(SELECT A."K", "X" FROM A, A."ARR" AS "X", B WHERE "X" = B."BK") d`)
		if err != nil {
			t.Fatalf("derived-wrapped projected-EXISTS-over-unnest must plan+run: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("derived-wrapped projected-EXISTS rows = %v, want exactly 1 — the "+
				"gathered row the control produces. Zero rows is the SILENT-0 this "+
				"sentinel was posted against: the gather suppressed rather than lifted.", rows)
		}
		parts := strings.Split(rows[0], "|")
		if len(parts) != 2 {
			t.Fatalf("row %q is not the 2-column (X, EXISTS) projection", rows[0])
		}
		if parts[0] != "X=7" {
			t.Fatalf("first column = %q, want X=7 — the same gathered row "+
				"control_direct_unnest_gathers pins (A.ARR={7,8} x B.BK=7, WHERE X=B.BK).",
				parts[0])
		}
		if !strings.HasSuffix(parts[1], "=true") {
			t.Fatalf("projected EXISTS = %q, want a TRUE: C.CK=100 matches the derived "+
				"row's A.K=100, so the correlated EXISTS holds.", parts[1])
		}
	})
}
