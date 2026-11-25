package executor

import (
	"context"
	"slices"
	"testing"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"google.golang.org/protobuf/proto"
)

// TestIntegration_ScanPlan_PrefixStringResume pages a STARTS_WITH primary-key scan
// two rows at a time. Its range is PREFIX_STRING on both ends, so the continuation
// prefix stops before the string terminator (KeyValueCursorBase.java:318-320); the
// executor must hand the cursor the range's own endpoints, not CONTINUATION.
func TestIntegration_ScanPlan_PrefixStringResume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ks := testSubspace(t)
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("name"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	md, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"aa", "ab", "aba", "abb", "abc", "abd", "abz", "ac", "b"}
	_, err = testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		if err := s.DeleteAllRecords(); err != nil {
			return nil, err
		}
		for i, n := range names {
			if _, err := s.SaveRecord(&demopb.Customer{CustomerId: proto.Int64(int64(i)), Name: proto.String(n)}); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"ab", "aba", "abb", "abc", "abd", "abz"}
	for _, reverse := range []bool{false, true} {
		comps := predicates.EmptyComparisonRange().
			Merge(&predicates.Comparison{Type: predicates.ComparisonStartsWith, Operand: values.LiteralValue("ab")})
		if !comps.Complete() {
			t.Fatal("merge STARTS_WITH failed")
		}
		scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Customer"}, integrationCustomerType(), reverse)).
			WithScanComparisons([]*predicates.ComparisonRange{comps.Range}).
			WithKeyComponentTypes([]values.Type{values.NullableString})

		var got []string
		var cont []byte
		for page := 0; ; page++ {
			if page > len(want) {
				t.Fatalf("reverse=%v: still paging after %d pages, got %v", reverse, page, got)
			}
			var done bool
			_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				s, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				cur, err := ExecutePlan(ctx, scan, s, EmptyEvaluationContext(), cont,
					recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(2))
				if err != nil {
					return nil, err
				}
				defer cur.Close()
				for {
					res, err := cur.OnNext(ctx)
					if err != nil {
						return nil, err
					}
					if !res.HasNext() {
						done = res.GetNoNextReason().IsSourceExhausted()
						cont, err = res.GetContinuation().ToBytes()
						return nil, err
					}
					pk := res.GetValue().PrimaryKey
					if len(pk) != 1 {
						t.Fatalf("primary key %v", pk)
					}
					got = append(got, pk[0].(string))
				}
			})
			if err != nil {
				t.Fatalf("reverse=%v page %d: %v", reverse, page, err)
			}
			if done {
				break
			}
		}
		wantOrder := slices.Clone(want)
		if reverse {
			slices.Reverse(wantOrder)
		}
		if !slices.Equal(got, wantOrder) {
			t.Errorf("reverse=%v: paged %v, want %v", reverse, got, wantOrder)
		}
	}
}
