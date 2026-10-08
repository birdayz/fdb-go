package sqldriver_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
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

func TestFDB_ComputedRecordArrayAggContinuation(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	fdb.MustAPIVersion(730)
	ctx := context.Background()
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	require.NoError(t, err)
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	b := metadata.NewSchemaTemplateBuilder().SetName("computed_array_agg")
	b.AddTable("T", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(false), 2),
		metadata.NewColumnSpec("V", api.NewLongType(true), 3),
	}, []string{"ID"})
	tmpl, err := b.Build()
	require.NoError(t, err)
	md := tmpl.Underlying()
	desc := md.GetRecordType("T").Descriptor

	_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		for i := int64(1); i <= 3; i++ {
			msg := dynamicpb.NewMessage(desc)
			msg.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(i))
			msg.Set(desc.Fields().ByName("K"), protoreflect.ValueOfInt64(4-i))
			if i != 3 {
				msg.Set(desc.Fields().ByName("V"), protoreflect.ValueOfInt64(i*10))
			}
			if _, err := store.SaveRecord(msg); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	require.NoError(t, err)
	plan, err := embedded.PlanRecordQueryWithMetadata("SELECT ARRAY_AGG((id,v)) FROM t", md, nil)
	require.NoError(t, err)
	require.Contains(t, plan.Explain(), "StreamingAgg")
	require.NoError(t, plans.FinalizePlan(plan))
	var continuation []byte
	var got [][]any
	done := false
	pages := 0
	for !done && pages < 8 {
		var page [][]any
		var next []byte
		_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			page = nil
			store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			cursor, err := executor.ExecutePlan(ctx, plan, store, executor.EmptyEvaluationContext(), continuation, recordlayer.DefaultExecuteProperties().WithScannedRecordsLimit(1))
			if err != nil {
				return nil, err
			}
			defer cursor.Close()
			for {
				result, err := cursor.OnNext(ctx)
				if err != nil {
					return nil, err
				}
				if result.HasNext() {
					page = append(page, result.GetValue().Positional.Slots)
					continue
				}
				cont := result.GetContinuation()
				done = cont == nil || cont.IsEnd()
				if !done {
					next, err = cont.ToBytes()
				}
				return nil, err
			}
		})
		require.NoError(t, err, "resume page %d", pages)
		got = append(got, page...)
		continuation = next
		pages++
	}
	require.True(t, done, "pagination must terminate")
	require.Greater(t, pages, 1, "must resume a partial aggregate")
	require.Len(t, got, 1)
	require.Len(t, got[0], 1)
	elements := got[0][0].([]any)
	require.Len(t, elements, 3)
	for i, element := range elements {
		message := element.(proto.Message).ProtoReflect()
		fields := message.Descriptor().Fields()
		require.Equal(t, int64(i+1), message.Get(fields.Get(0)).Int())
		if i < 2 {
			require.Equal(t, int64((i+1)*10), message.Get(fields.Get(1)).Int())
		} else {
			require.False(t, message.Has(fields.Get(1)))
		}
	}
}
