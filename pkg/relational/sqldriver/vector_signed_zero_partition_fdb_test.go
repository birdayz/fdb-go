package sqldriver_test

// A vector partition equality is a physical partition selector. Logical zero
// therefore opens both the -0 and +0 HNSW graphs, preserving top-K per graph
// and a branch-tagged continuation.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/metadata"
)

func TestFDB_VectorSignedZeroPartitions(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	for _, width := range []string{"DOUBLE", "FLOAT"} {
		t.Run(width, func(t *testing.T) {
			runVectorSignedZeroPartition(t, width)
		})
	}
}

func runVectorSignedZeroPartition(t *testing.T, width string) {
	t.Helper()
	ctx := context.Background()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name(), width}.Pack())

	var partType api.DataType
	if width == "FLOAT" {
		partType = api.NewFloatType(false)
	} else {
		partType = api.NewDoubleType(false)
	}
	b := metadata.NewSchemaTemplateBuilder().SetName("signed_zero_vector_" + width)
	b.AddTable("DOCS", []metadata.ColumnSpec{
		metadata.NewColumnSpec("PART", partType, 1),
		metadata.NewColumnSpec("ID", api.NewLongType(false), 2),
		metadata.NewColumnSpec("EMBEDDING", api.NewVectorType(64, 3, true), 3),
	}, []string{"PART", "ID"})
	b.AddVectorIndex("DOCS", "VEC_IDX", "EMBEDDING", []string{"PART"},
		map[string]string{recordlayer.IndexOptionVectorMetric: "EUCLIDEAN_METRIC"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	md := tmpl.Underlying()
	desc := md.GetRecordType("DOCS").Descriptor

	makeRec := func(part float64, id int64, vec []float64) proto.Message {
		m := dynamicpb.NewMessage(desc)
		partField := desc.Fields().ByName("PART")
		if width == "FLOAT" {
			m.Set(partField, protoreflect.ValueOfFloat32(float32(part)))
		} else {
			m.Set(partField, protoreflect.ValueOfFloat64(part))
		}
		m.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(desc.Fields().ByName("EMBEDDING"),
			protoreflect.ValueOfBytes(recordlayer.SerializeVector(vec)))
		return m
	}
	_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, openErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if openErr != nil {
			return nil, openErr
		}
		// Each physical zero partition has two candidates. With top-1, ids 1
		// and 3 are independently nearest; a single-sign probe returns only one.
		for _, rec := range []proto.Message{
			makeRec(math.Copysign(0, -1), 1, []float64{1, 0, 0}),
			makeRec(math.Copysign(0, -1), 2, []float64{0, 1, 0}),
			makeRec(0.0, 3, []float64{0.9, 0.1, 0}),
			makeRec(0.0, 4, []float64{-1, 0, 0}),
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

	query := `SELECT id FROM docs WHERE part = 0
		QUALIFY ROW_NUMBER() OVER (PARTITION BY part
			ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0])) <= 1`
	plan, err := embedded.PlanRecordQueryWithMetadata(query, md, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	explain := plan.Explain()
	if !strings.Contains(explain, "VectorIndexScan") || !strings.Contains(explain, "prefix=[=]") {
		t.Fatalf("plan = %s\nwant an exact signed-zero vector partition probe", explain)
	}

	type vectorPage struct {
		ids          []int64
		continuation []byte
		done         bool
	}
	runPage := func(continuation []byte, returnedRowLimit int) vectorPage {
		t.Helper()
		var page vectorPage
		_, runErr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, openErr := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if openErr != nil {
				return nil, openErr
			}
			props := recordlayer.DefaultExecuteProperties()
			if returnedRowLimit > 0 {
				props = props.WithReturnedRowLimit(returnedRowLimit)
			}
			cursor, execErr := executor.ExecutePlan(ctx, plan, store,
				executor.EmptyEvaluationContext(), continuation, props)
			if execErr != nil {
				return nil, execErr
			}
			defer func() { _ = cursor.Close() }()
			for {
				next, nextErr := cursor.OnNext(ctx)
				if nextErr != nil {
					return nil, nextErr
				}
				if next.HasNext() {
					row, ok := executor.RowValue(next.GetValue()).(map[string]any)
					if !ok {
						return nil, fmt.Errorf("vector row is %T, want map[string]any", executor.RowValue(next.GetValue()))
					}
					id, ok := row["ID"].(int64)
					if !ok {
						return nil, fmt.Errorf("vector ID is %T, want int64", row["ID"])
					}
					page.ids = append(page.ids, id)
					continue
				}
				cont := next.GetContinuation()
				if cont == nil || cont.IsEnd() {
					page.done = true
					return nil, nil
				}
				encoded, encodeErr := cont.ToBytes()
				if encodeErr != nil {
					return nil, encodeErr
				}
				if len(encoded) == 0 {
					return nil, fmt.Errorf("live vector continuation encoded empty")
				}
				page.continuation = append([]byte(nil), encoded...)
				return nil, nil
			}
		})
		if runErr != nil {
			t.Fatalf("execute page: %v", runErr)
		}
		return page
	}

	unpaged := runPage(nil, 0)
	if !unpaged.done || !reflect.DeepEqual(unpaged.ids, []int64{1, 3}) {
		t.Fatalf("unpaged %s signed-zero vector result = ids %v done=%v, want [1 3] done",
			width, unpaged.ids, unpaged.done)
	}

	var paged []int64
	var continuation []byte
	pagedDone := false
	for pageNumber := 0; pageNumber < 10; pageNumber++ {
		page := runPage(continuation, 1)
		paged = append(paged, page.ids...)
		if page.done {
			pagedDone = true
			break
		}
		if len(page.ids) == 0 && reflect.DeepEqual(page.continuation, continuation) {
			t.Fatalf("page %d made no progress", pageNumber)
		}
		continuation = page.continuation
	}
	if !pagedDone {
		t.Fatal("paged signed-zero vector scan hit the 10-page progress cap before SourceExhausted")
	}
	if !reflect.DeepEqual(paged, unpaged.ids) {
		t.Fatalf("paged %s signed-zero vector result = %v, want unpaged %v; branch "+
			"continuation dropped, duplicated, or reordered a physical partition",
			width, paged, unpaged.ids)
	}

	// A continuation belongs to the evaluated physical invocation, not merely
	// to the prepared plan. In particular, rebinding a dynamic rank cap from a
	// positive value to an empty adjusted cap must reject the old live token.
	// Silently returning EMPTY would conceal a stale resume and disagree with
	// every non-empty range-set fingerprint mismatch.
	rankCases := []struct {
		name       string
		operator   string
		initialK   int64
		emptyK     int64
		explainPin string
	}{
		{
			name:       "rank less than or equal",
			operator:   "<=",
			initialK:   2,
			emptyK:     0,
			explainPin: "rank<=?1",
		},
		{
			name:       "rank less than",
			operator:   "<",
			initialK:   2,
			emptyK:     1,
			explainPin: "rank<?1",
		},
	}
	for _, rankCase := range rankCases {
		rankCase := rankCase
		t.Run("dynamic continuation "+rankCase.name, func(t *testing.T) {
			dynamicQuery := fmt.Sprintf(`SELECT id FROM docs WHERE part = 0
				QUALIFY ROW_NUMBER() OVER (PARTITION BY part
					ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0])) %s ?`,
				rankCase.operator)
			preparedPlan, planErr := embedded.PlanRecordQueryWithMetadata(dynamicQuery, md, nil)
			if planErr != nil {
				t.Fatalf("plan dynamic rank: %v", planErr)
			}
			explain := preparedPlan.Explain()
			if !strings.Contains(explain, "VectorIndexScan") ||
				!strings.Contains(explain, "prefix=[=]") ||
				!strings.Contains(explain, rankCase.explainPin) {
				t.Fatalf("dynamic rank plan = %s\nwant signed-zero vector partition access with %s",
					explain, rankCase.explainPin)
			}

			var liveContinuation []byte
			_, firstErr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, openErr := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if openErr != nil {
					return nil, openErr
				}
				props := recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(1)
				cursor, execErr := executor.ExecutePlan(
					ctx,
					preparedPlan,
					store,
					executor.EmptyEvaluationContext().WithParams([]any{rankCase.initialK}),
					nil,
					props,
				)
				if execErr != nil {
					return nil, execErr
				}
				defer func() { _ = cursor.Close() }()

				first, nextErr := cursor.OnNext(ctx)
				if nextErr != nil {
					return nil, nextErr
				}
				if !first.HasNext() {
					return nil, fmt.Errorf("positive dynamic K=%d produced no first row (reason=%v)",
						rankCase.initialK, first.GetNoNextReason())
				}
				stopped, nextErr := cursor.OnNext(ctx)
				if nextErr != nil {
					return nil, nextErr
				}
				if stopped.HasNext() || stopped.GetNoNextReason() != recordlayer.ReturnLimitReached {
					return nil, fmt.Errorf("one-row page stopped with hasNext=%v reason=%v, want ReturnLimitReached",
						stopped.HasNext(), stopped.GetNoNextReason())
				}
				continuation := stopped.GetContinuation()
				if continuation == nil || continuation.IsEnd() {
					return nil, fmt.Errorf("positive dynamic K=%d returned no live continuation", rankCase.initialK)
				}
				encoded, encodeErr := continuation.ToBytes()
				if encodeErr != nil {
					return nil, encodeErr
				}
				if len(encoded) == 0 {
					return nil, fmt.Errorf("positive dynamic K=%d encoded an empty continuation", rankCase.initialK)
				}
				liveContinuation = bytes.Clone(encoded)
				return nil, nil
			})
			if firstErr != nil {
				t.Fatalf("obtain live continuation: %v", firstErr)
			}

			// Pin that the test is resuming the range-set wrapper around a live
			// vector child, rather than an end marker or a generic LIMIT envelope.
			var rangeSetContinuation gen.ScanRangeSetContinuation
			if parseErr := rangeSetContinuation.UnmarshalVT(liveContinuation); parseErr != nil {
				t.Fatalf("live continuation is not a range-set token: %v", parseErr)
			}
			if len(rangeSetContinuation.Fingerprint) == 0 ||
				len(rangeSetContinuation.Choices) != 1 ||
				rangeSetContinuation.ChildStarted == nil ||
				!rangeSetContinuation.GetChildStarted() ||
				len(rangeSetContinuation.InnerContinuation) == 0 {
				t.Fatalf("range-set continuation = %+v, want one started signed-zero branch with a live vector child",
					&rangeSetContinuation)
			}

			var resumeErr error
			_, transactionErr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, openErr := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if openErr != nil {
					return nil, openErr
				}
				cursor, execErr := executor.ExecutePlan(
					ctx,
					preparedPlan,
					store,
					executor.EmptyEvaluationContext().WithParams([]any{rankCase.emptyK}),
					liveContinuation,
					recordlayer.DefaultExecuteProperties(),
				)
				resumeErr = execErr
				if cursor != nil {
					_ = cursor.Close()
				}
				return nil, nil
			})
			if transactionErr != nil {
				t.Fatalf("resume transaction: %v", transactionErr)
			}
			var continuationErr *recordlayer.ContinuationParseError
			if !errors.As(resumeErr, &continuationErr) {
				t.Fatalf("resume %s with K=%d error = %T %v, want *recordlayer.ContinuationParseError",
					rankCase.operator, rankCase.emptyK, resumeErr, resumeErr)
			}
			if !bytes.Equal(continuationErr.RawBytes, liveContinuation) {
				t.Fatalf("ContinuationParseError raw bytes = %x, want live token %x",
					continuationErr.RawBytes, liveContinuation)
			}
			if !strings.Contains(continuationErr.Error(), "cannot resume an empty adjusted rank") {
				t.Fatalf("ContinuationParseError = %v, want empty-adjusted-rank context", continuationErr)
			}
		})
	}
}
