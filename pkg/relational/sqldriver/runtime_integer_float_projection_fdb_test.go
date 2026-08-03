package sqldriver_test

import (
	"context"
	"errors"
	"math"
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
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/metadata"
)

// The exported SQL planner preserves `?` as an Unknown-typed ParameterValue,
// unlike database/sql's current text-substitution path. That real API shape
// must not pack an arbitrary runtime float directly against a LONG primary key.
func TestFDB_RuntimeFloatAgainstLongPrimaryKeyProjection(t *testing.T) {
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
	ks := subspace.FromBytes(tuple.Tuple{t.Name(), "runtime-long-float-projection"}.Pack())

	builder := metadata.NewSchemaTemplateBuilder().SetName("runtime_long_float_projection")
	builder.AddTable("T", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
	}, []string{"ID"})
	template, err := builder.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	md := template.Underlying()
	desc := md.GetRecordType("T").Descriptor
	makeRecord := func(id int64) proto.Message {
		message := dynamicpb.NewMessage(desc)
		message.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		return message
	}
	const twoTo53 = int64(1) << 53
	_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, openErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if openErr != nil {
			return nil, openErr
		}
		for _, id := range []int64{1, 2, 3, twoTo53, twoTo53 + 1} {
			if _, saveErr := store.SaveRecord(makeRecord(id)); saveErr != nil {
				return nil, saveErr
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	plan := func(sql string) plans.RecordQueryPlan {
		t.Helper()
		planned, planErr := embedded.PlanRecordQueryWithMetadata(sql, md, nil)
		if planErr != nil {
			t.Fatalf("plan %q: %v", sql, planErr)
		}
		if !strings.Contains(planned.Explain(), "Scan(T") {
			t.Fatalf("plan %q = %s, want physical primary scan", sql, planned.Explain())
		}
		return planned
	}
	run := func(planned plans.RecordQueryPlan, parameter any) (int, error) {
		var count int
		_, runErr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, openErr := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if openErr != nil {
				return nil, openErr
			}
			cursor, execErr := executor.ExecutePlan(
				ctx, planned, store,
				executor.EmptyEvaluationContext().WithParams([]any{parameter}),
				nil, recordlayer.DefaultExecuteProperties(),
			)
			if execErr != nil {
				return nil, execErr
			}
			defer func() { _ = cursor.Close() }()
			results, collectErr := executor.CollectAll(ctx, cursor)
			if collectErr != nil {
				return nil, collectErr
			}
			count = len(results)
			return nil, nil
		})
		return count, runErr
	}

	equality := plan("SELECT ID FROM T WHERE ID = ?")
	if count, runErr := run(equality, twoTo53+1); runErr != nil || count != 1 {
		t.Fatalf("ordinary integer parameter = count %d, err %v; want 1, nil", count, runErr)
	}
	_, runErr := run(equality, float64(twoTo53))
	var plural *executor.UnsupportedPhysicalNumericProjectionError
	if !errors.As(runErr, &plural) ||
		plural.EquivalenceClassLow != twoTo53 || plural.EquivalenceClassHigh != twoTo53+1 {
		t.Fatalf("precision-cliff equality error = %T(%v), want plural-class projection error", runErr, runErr)
	}

	greaterOrEqual := plan("SELECT ID FROM T WHERE ID >= ?")
	if count, runErr := run(greaterOrEqual, 1.5); runErr != nil || count != 4 {
		t.Fatalf("ID >= 1.5 = count %d, err %v; want 4, nil", count, runErr)
	}
	lessOrEqual := plan("SELECT ID FROM T WHERE ID <= ?")
	if count, runErr := run(lessOrEqual, 1.5); runErr != nil || count != 1 {
		t.Fatalf("ID <= 1.5 = count %d, err %v; want 1, nil", count, runErr)
	}

	_, runErr = run(equality, "9007199254740992")
	var incompatible *executor.IncompatiblePhysicalComparandError
	if !errors.As(runErr, &incompatible) {
		t.Fatalf("string-bound LONG equality error = %T(%v), want IncompatiblePhysicalComparandError", runErr, runErr)
	}

	// cmpAny's existing total-order policy places NaN above every integer.
	// The inverse projector follows that policy exactly instead of relying on
	// cross-type tuple ordering.
	lessThan := plan("SELECT ID FROM T WHERE ID < ?")
	if count, runErr := run(lessThan, math.NaN()); runErr != nil || count != 5 {
		t.Fatalf("ID < NaN = count %d, err %v; want 5, nil", count, runErr)
	}
}
