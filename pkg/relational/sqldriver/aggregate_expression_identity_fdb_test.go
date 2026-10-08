package sqldriver_test

import (
	"context"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"

	"github.com/stretchr/testify/require"
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

func TestFDB_AggregateIndexExpressionIdentity(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	fdb.MustAPIVersion(730)
	for _, tc := range []struct {
		name    string
		root    recordlayer.KeyExpression
		indexed bool
	}{
		{"field_control", recordlayer.GroupBy(recordlayer.Field("V"), recordlayer.Field("G")), true},
		{"computed_operand", recordlayer.GroupBy(recordlayer.FunctionExpr("add", recordlayer.Concat(recordlayer.Field("V"), recordlayer.Literal(int64(1)))), recordlayer.Field("G")), false},
		{"computed_group", recordlayer.GroupBy(recordlayer.Field("V"), recordlayer.FunctionExpr("add", recordlayer.Concat(recordlayer.Field("G"), recordlayer.Literal(int64(1))))), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			rawDB, err := fdb.OpenDatabase(testkit.ClusterFile())
			require.NoError(t, err)
			db := recordlayer.NewFDBDatabase(rawDB)
			ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
			b := metadata.NewSchemaTemplateBuilder().SetName("agg_expression_identity")
			b.AddTable("T", []metadata.ColumnSpec{
				metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
				metadata.NewColumnSpec("G", api.NewLongType(false), 2),
				metadata.NewColumnSpec("V", api.NewLongType(false), 3),
			}, []string{"ID"})
			b.AddGeneratedIndex("T", "MX", tc.root, recordlayer.IndexTypePermutedMax, false, map[string]string{recordlayer.IndexOptionPermutedSize: "0"}, nil)
			tmpl, err := b.Build()
			require.NoError(t, err)
			md := tmpl.Underlying()
			desc := md.GetRecordType("T").Descriptor
			_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := int64(1); i <= 2; i++ {
					msg := dynamicpb.NewMessage(desc)
					msg.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(i))
					msg.Set(desc.Fields().ByName("G"), protoreflect.ValueOfInt64(7))
					msg.Set(desc.Fields().ByName("V"), protoreflect.ValueOfInt64(10*i))
					if _, err := store.SaveRecord(msg); err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			require.NoError(t, err)
			plan, err := embedded.PlanRecordQueryWithMetadata("SELECT g, MAX(v) FROM t GROUP BY g", md, nil)
			require.NoError(t, err)
			t.Logf("plan: %s", plan.Explain())
			require.Equal(t, tc.indexed, strings.Contains(plan.Explain(), "AggregateIndex"), plan.Explain())
			_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				cursor, err := executor.ExecutePlan(ctx, plan, store, executor.EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
				if err != nil {
					return nil, err
				}
				defer cursor.Close()
				rows, err := executor.CollectAll(ctx, cursor)
				if err != nil {
					return nil, err
				}
				require.Len(t, rows, 1)
				require.NotNil(t, rows[0].Positional)
				require.Equal(t, []any{int64(7), int64(20)}, rows[0].Positional.Slots)
				return nil, nil
			})
			require.NoError(t, err)
		})
	}
}
