package sqldriver_test

import (
	"context"
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
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/metadata"
)

// TestFDB_CorrelatedPrimaryUnnestUsesFanOutIndex is the real-store cardinality
// certificate for a correlated array EXISTS satisfied by a FAN_OUT index. A
// FAN_OUT root is duplicate-producing in general, and this fixture includes
// multiple distinct matching elements for one record; the optimizer must both choose that index and
// retain the existential one-row-per-outer-row contract with
// UnorderedPrimaryKeyDistinct. The one-row paging pass also exercises the
// distinct seen-set continuation through serialization and a fresh FDB
// transaction for every page.
func TestFDB_CorrelatedPrimaryUnnestUsesFanOutIndex(t *testing.T) {
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

	b := metadata.NewSchemaTemplateBuilder().SetName("fanout_exists_fdb")
	b.AddTable("T", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("TAGS", api.NewArrayType(api.NewLongType(false), true), 2),
	}, []string{"ID"})
	b.AddFanOutIndex("T", "T_TAGS", "TAGS")
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	md := tmpl.Underlying()
	desc := md.GetRecordType("T").Descriptor

	record := func(id int64, tags ...int64) proto.Message {
		m := dynamicpb.NewMessage(desc)
		m.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		tagsField := desc.Fields().ByName("TAGS")
		list := m.NewField(tagsField).List()
		for _, tag := range tags {
			list.Append(protoreflect.ValueOfInt64(tag))
		}
		m.Set(tagsField, protoreflect.ValueOfList(list))
		return m
	}

	_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, createErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(ks).
			CreateOrOpen()
		if createErr != nil {
			return nil, createErr
		}
		for _, rec := range []proto.Message{
			record(1),          // empty: no fanout key
			record(2, 9),       // one matching key
			record(3, 7, 8, 9), // two distinct matching index keys
			record(4, 8, 9),    // two distinct matching index keys
			record(5, 7),       // non-matching key
		} {
			if _, saveErr := store.SaveRecord(rec); saveErr != nil {
				return nil, saveErr
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// A fanout index is not a base-record COUNT source: it omits empty arrays
	// and emits one entry per distinct element key. Pin both the plan choice and
	// the real-store answer so count-only covering shortcuts cannot silently
	// count index entries.
	countPlan, err := embedded.PlanRecordQueryWithMetadata(
		`SELECT COUNT(*) AS N FROM T`,
		md,
		properties.FixedStatistics{Cardinality: 1_000_000},
	)
	if err != nil {
		t.Fatalf("plan count: %v", err)
	}
	if countExplain := countPlan.Explain(); strings.Contains(countExplain, "IndexScan(T_TAGS") {
		t.Fatalf("base-record COUNT must not use duplicate-producing fanout index: %s", countExplain)
	}
	rawCount, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, openErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(ks).
			Open()
		if openErr != nil {
			return nil, openErr
		}
		cursor, executeErr := executor.ExecutePlan(
			ctx,
			countPlan,
			store,
			executor.EmptyEvaluationContext(),
			nil,
			recordlayer.DefaultExecuteProperties(),
		)
		if executeErr != nil {
			return nil, executeErr
		}
		defer cursor.Close()
		return executor.CollectAll(ctx, cursor)
	})
	if err != nil {
		t.Fatalf("execute count: %v", err)
	}
	countResults, ok := rawCount.([]executor.QueryResult)
	if !ok || len(countResults) != 1 {
		t.Fatalf("count results = %T %#v, want one row", rawCount, rawCount)
	}
	countRow, ok := executor.RowValue(countResults[0]).(map[string]any)
	if !ok || len(countRow) != 1 {
		t.Fatalf("count row = %T %#v, want one projected value", executor.RowValue(countResults[0]), executor.RowValue(countResults[0]))
	}
	var count int64
	for _, value := range countRow {
		switch typed := value.(type) {
		case int64:
			count = typed
		case int32:
			count = int64(typed)
		case int:
			count = int64(typed)
		default:
			t.Fatalf("COUNT value = %T (%v), want integer", value, value)
		}
	}
	if count != 5 {
		t.Fatalf("COUNT(*) = %d, want 5 base records", count)
	}

	const sql = `SELECT R.ID FROM T AS R WHERE EXISTS (SELECT E FROM R.TAGS AS E WHERE E >= 8)`
	plan, err := embedded.PlanRecordQueryWithMetadata(
		sql,
		md,
		properties.FixedStatistics{Cardinality: 1_000_000},
	)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	explain := plan.Explain()
	for _, required := range []string{"IndexScan(T_TAGS", "UnorderedPrimaryKeyDistinct"} {
		if !strings.Contains(explain, required) {
			t.Fatalf("EXPLAIN must contain %q (fanout optimization/cardinality repair must fire): %s", required, explain)
		}
	}
	for _, fallback := range []string{"Scan(T)", "Explode"} {
		if strings.Contains(explain, fallback) {
			t.Fatalf("EXPLAIN must not contain fallback %q: %s", fallback, explain)
		}
	}

	type pageResult struct {
		ids          []int64
		continuation []byte
		done         bool
	}
	rowID := func(result executor.QueryResult) (int64, error) {
		row, ok := executor.RowValue(result).(map[string]any)
		if !ok {
			return 0, fmt.Errorf("projected row has type %T, want map[string]any", executor.RowValue(result))
		}
		switch id := row["ID"].(type) {
		case int64:
			return id, nil
		case int32:
			return int64(id), nil
		case int:
			return int64(id), nil
		default:
			return 0, fmt.Errorf("projected ID has type %T (%v), want integer", row["ID"], row["ID"])
		}
	}
	runPage := func(
		continuation []byte,
		rowLimit int,
		state *recordlayer.ExecuteState,
	) (pageResult, error) {
		raw, runErr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, openErr := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(md).
				SetSubspace(ks).
				Open()
			if openErr != nil {
				return nil, openErr
			}
			props := recordlayer.DefaultExecuteProperties()
			props.State = state
			if rowLimit > 0 {
				props = props.WithReturnedRowLimit(rowLimit)
			}
			cursor, executeErr := executor.ExecutePlan(
				ctx,
				plan,
				store,
				executor.EmptyEvaluationContext(),
				continuation,
				props,
			)
			if executeErr != nil {
				return nil, executeErr
			}
			defer cursor.Close()

			var result pageResult
			for {
				next, nextErr := cursor.OnNext(ctx)
				if nextErr != nil {
					return nil, nextErr
				}
				if next.HasNext() {
					id, idErr := rowID(next.GetValue())
					if idErr != nil {
						return nil, idErr
					}
					result.ids = append(result.ids, id)
					continue
				}
				if next.GetNoNextReason().IsSourceExhausted() {
					result.done = true
					return result, nil
				}
				if next.GetNoNextReason() != recordlayer.ReturnLimitReached {
					return nil, fmt.Errorf("page stopped for %v, want ReturnLimitReached", next.GetNoNextReason())
				}
				nextContinuation := next.GetContinuation()
				if nextContinuation == nil || nextContinuation.IsEnd() {
					return nil, fmt.Errorf("row-limited page stopped without a live continuation")
				}
				encoded, encodeErr := nextContinuation.ToBytes()
				if encodeErr != nil {
					return nil, fmt.Errorf("serialize continuation: %w", encodeErr)
				}
				if len(encoded) == 0 {
					return nil, fmt.Errorf("row-limited page returned an empty live continuation")
				}
				result.continuation = append([]byte(nil), encoded...)
				return result, nil
			}
		})
		if runErr != nil {
			return pageResult{}, runErr
		}
		result, ok := raw.(pageResult)
		if !ok {
			return pageResult{}, fmt.Errorf("page result has type %T", raw)
		}
		return result, nil
	}
	assertIDs := func(label string, got []int64) {
		t.Helper()
		sorted := append([]int64(nil), got...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		want := []int64{2, 3, 4}
		if len(sorted) != len(want) {
			t.Fatalf("%s IDs = %v, want %v exactly once; plan: %s", label, sorted, want, explain)
		}
		for i := range want {
			if sorted[i] != want[i] {
				t.Fatalf("%s IDs = %v, want %v exactly once; plan: %s", label, sorted, want, explain)
			}
		}
	}

	unpaged, err := runPage(nil, 0, recordlayer.NewExecuteState(0))
	if err != nil {
		t.Fatalf("execute unpaged: %v", err)
	}
	if !unpaged.done || len(unpaged.continuation) != 0 {
		t.Fatalf("unpaged execution did not exhaust: done=%v continuation=%x", unpaged.done, unpaged.continuation)
	}
	assertIDs("unpaged", unpaged.ids)

	const maxPages = 16
	var (
		paged        []int64
		continuation []byte
		done         bool
	)
	pagedState := recordlayer.NewExecuteState(0)
	for page := 1; page <= maxPages; page++ {
		result, pageErr := runPage(continuation, 1, pagedState)
		if pageErr != nil {
			t.Fatalf("execute page %d: %v", page, pageErr)
		}
		if len(result.ids) > 1 {
			t.Fatalf("page %d returned %d rows with returned-row-limit 1: %v", page, len(result.ids), result.ids)
		}
		paged = append(paged, result.ids...)
		if result.done {
			done = true
			break
		}
		continuation = result.continuation
	}
	if !done {
		t.Fatalf("paged execution did not terminate within %d pages (IDs so far: %v)", maxPages, paged)
	}
	assertIDs("paged", paged)
}
