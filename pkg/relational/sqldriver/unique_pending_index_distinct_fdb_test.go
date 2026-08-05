package sqldriver_test

// READABLE_UNIQUE_PENDING is the state a UNIQUE index lands in when its build
// completes and uniqueness violations exist: the index is fully populated and
// scannable, but its `unique` flag is a LIE about the data
// (IndexState.java:59-66 — "safe to consider READABLE for queries as long as
// uniqueness is not assumed").
//
// The planner assumes it. distinctEliminatedByUniqueKey elides a DISTINCT when
// the projected columns cover a unique index's columns, and a MatchCandidate
// carries no index state — so the proof holds over a store where the index's
// own uniqueness is known-violated. Java is safe here for one reason only, and
// it is not a check at the uniqueness site: MetaDataPlanContext.java:96-103
// filters candidate indexes through RecordStoreState::isReadable, which is
// exact equality with READABLE (RecordStoreState.java:172-174) and therefore
// excludes READABLE_UNIQUE_PENDING. Every Java path that trusts
// MatchCandidate.isUnique() rests on that one filter.
//
// The consequence is not a failed scan. A plan that never touches the pending
// index — a base-record scan — passes the executor's per-leaf state backstop
// untouched, and the elided DISTINCT emits the duplicate rows verbatim.

import (
	"context"
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
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/metadata"
)

func TestFDB_UniquePendingIndexDoesNotEliminateDistinct(t *testing.T) {
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

	cols := []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewIntegerType(false), 1),
		metadata.NewColumnSpec("C1", api.NewIntegerType(false), 2),
		metadata.NewColumnSpec("OTHER", api.NewIntegerType(false), 3),
	}

	// mdPlain has no secondary index: the duplicate C1 pair is written under it,
	// exactly as it would be by an application running before the UNIQUE index
	// was declared.
	plainTmpl, err := metadata.NewSchemaTemplateBuilder().SetName("uqpend_plain").
		AddTable("T", cols, []string{"ID"}).Build()
	if err != nil {
		t.Fatalf("build plain schema: %v", err)
	}
	mdPlain := plainTmpl.Underlying()

	uqBuilder := metadata.NewSchemaTemplateBuilder().SetName("uqpend")
	uqBuilder.AddTable("T", cols, []string{"ID"})
	uqBuilder.AddIndex("T", "T_C1_UNIQ", []string{"C1"}, true)
	uqTmpl, err := uqBuilder.Build()
	if err != nil {
		t.Fatalf("build unique schema: %v", err)
	}
	md := uqTmpl.Underlying()
	if idx := md.GetIndex("T_C1_UNIQ"); idx == nil || !idx.IsUnique() {
		t.Fatal("T_C1_UNIQ is missing or not unique — the rest of this test would assert nothing")
	}
	desc := md.GetRecordType("T").Descriptor

	rec := func(id, c1, other int32) proto.Message {
		m := dynamicpb.NewMessage(desc)
		m.Set(desc.Fields().ByName(protoreflect.Name("ID")), protoreflect.ValueOfInt32(id))
		m.Set(desc.Fields().ByName(protoreflect.Name("C1")), protoreflect.ValueOfInt32(c1))
		m.Set(desc.Fields().ByName(protoreflect.Name("OTHER")), protoreflect.ValueOfInt32(other))
		return m
	}

	// Two records share C1 = 7. A third has a distinct C1 so a plan that
	// wrongly returns the raw scan is distinguishable from one that returns a
	// single collapsed row.
	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(mdPlain).SetSubspace(ks).CreateOrOpen()
		if sErr != nil {
			return nil, sErr
		}
		for _, r := range []proto.Message{rec(1, 7, 100), rec(2, 7, 100), rec(3, 9, 100)} {
			if _, e := store.SaveRecord(r); e != nil {
				return nil, e
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("seed duplicate C1 rows: %v", err)
	}

	// Getting the index into READABLE_UNIQUE_PENDING takes the ONLY route Java
	// offers, and it is deliberately not the inline one. An inline rebuild marks
	// readable with allowUniquePending=FALSE (FDBRecordStore.java:4602 →
	// :3767-3768) and THROWS on the C1 = 7 duplicate; READABLE_UNIQUE_PENDING is
	// reachable only through IndexingBase.java:324, gated on
	// IndexingPolicy.shouldAllowUniquePendingState (OnlineIndexer.java:1117),
	// which defaults FALSE (:1220).
	//
	// So: let reconciliation leave the index DISABLED — with no record-count key
	// the count fallback reports an unbounded store for any non-empty one (Java's
	// getRecordCountForRebuildIndexes, FDBRecordStore.java:4862-4884) — then run
	// an OnlineIndexer that opts in.
	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		_, sErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		return nil, sErr
	}); err != nil {
		t.Fatalf("reconcile metadata with the UNIQUE index declared: %v", err)
	}
	indexer, iErr := recordlayer.NewOnlineIndexerBuilder().
		SetDatabase(db).SetMetaData(md).SetIndex(md.GetIndex("T_C1_UNIQ")).
		SetSubspace(ks).SetAllowUniquePendingState(true).Build()
	if iErr != nil {
		t.Fatalf("build online indexer: %v", iErr)
	}
	if _, bErr := indexer.BuildIndex(ctx); bErr != nil {
		t.Fatalf("build unique index over violating data: %v", bErr)
	}
	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
		if sErr != nil {
			return nil, sErr
		}
		state := store.GetIndexState("T_C1_UNIQ")
		if state != recordlayer.IndexStateReadableUniquePending {
			t.Errorf("T_C1_UNIQ state = %v, want READABLE_UNIQUE_PENDING — "+
				"without that state this test exercises nothing", state)
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("inspect index state: %v", err)
	}

	// The predicate on OTHER cannot be served by an index on C1 alone, so the
	// access path is a base-record scan: the executor's per-leaf index-state
	// backstop is never consulted, and only the DISTINCT decision is in play.
	const q = "SELECT DISTINCT C1 FROM T WHERE OTHER > 0"
	plan, err := embedded.PlanRecordQueryWithMetadata(q, md, nil)
	if err != nil {
		t.Fatalf("plan %q: %v", q, err)
	}
	explain := plan.Explain()

	var got []string
	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
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
			got = append(got, positionalNamedPipeSprint(r))
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("exec %q: %v\nplan:\n%s", q, err, explain)
	}

	sort.Strings(got)
	if len(got) != 2 {
		t.Fatalf("SELECT DISTINCT over a READABLE_UNIQUE_PENDING unique index returned "+
			"%d rows %v, want 2 distinct values.\nplan:\n%s\n"+
			"— the index's `unique` flag describes an intent the data violates; a "+
			"DISTINCT elided on its authority emits the duplicate verbatim",
			len(got), got, explain)
	}
	if strings.Join(got, " ") == "" {
		t.Fatalf("empty rows: %v", got)
	}
}
