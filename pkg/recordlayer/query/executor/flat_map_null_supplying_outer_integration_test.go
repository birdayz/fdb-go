package executor

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestIntegration_FlatMapNullSupplyingOuter executes, over real FDB scans, the
// shape ImplementNestedLoopJoinRule lowers a null-on-empty OUTER to (Java's
// planPartitionToPhysical wraps either leg): the outer is DefaultOnEmpty over
// its scan, the inner is correlated to the outer's row and typed against the
// wrapped outer's own carrier (the rule wraps before it normalizes the inner),
// and the FlatMap states the outer's presence. An empty outer must supply ONE
// NULL row that the inner reads — here `outer.customer_id IS NULL` holds for
// it, so every order comes back beside a NULL customer, the whole outer row
// NULL too — while a non-empty outer supplies its own rows, for which the
// predicate is false. A projection above reads the FlatMap's rows through the
// plan's own layout.
//
// What this does NOT observe: the cursor's presence configuration for the
// outer (flat_map_cursor.go). With it removed every arm here stays green,
// because the row DefaultOnEmpty supplies is itself a NULL record, so an
// absent row and a present row read alike. It is kept as the twin of the
// null-supplying inner's, so the build's layout states what the plan's does.
func TestIntegration_FlatMapNullSupplyingOuter(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		customers []*gen.Customer
		want      []string
	}{
		{"empty outer supplies one NULL row", nil, []string{"C=<nil>|O=18301", "C=<nil>|O=18302"}},
		{"non-empty outer supplies its rows", []*gen.Customer{{CustomerId: proto.Int64(7)}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := setupStore(t)
			insertOrders(t, store,
				&gen.Order{OrderId: proto.Int64(18301)},
				&gen.Order{OrderId: proto.Int64(18302)},
			)
			if len(tc.customers) > 0 {
				if _, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					s, err := recordlayer.NewStoreBuilder().SetContext(rtx).
						SetMetaDataProvider(store.GetMetaData()).SetSubspace(testSubspace(t)).Open()
					if err != nil {
						return nil, err
					}
					for _, c := range tc.customers {
						if _, err := s.SaveRecord(c); err != nil {
							return nil, err
						}
					}
					return nil, nil
				}); err != nil {
					t.Fatalf("insert customers: %v", err)
				}
			}

			outerAlias := values.NamedCorrelationIdentifier("E")
			innerAlias := values.NamedCorrelationIdentifier("F")
			customers := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Customer"}, integrationCustomerType(), false))
			outer := mustExecutorConstruct(plans.NewRecordQueryDefaultOnEmptyPlan(
				customers, values.NewNullValue(integrationCustomerType())))
			outerLayout, err := outer.ProvidedOutputLayout()
			if err != nil {
				t.Fatalf("outer layout: %v", err)
			}
			outerRow := mustTestQOV(t, outerAlias, values.PhysicalCarrierType(outerLayout))
			orders := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
			inner := mustExecutorConstruct(plans.NewRecordQueryFilterPlan(
				[]predicates.QueryPredicate{predicates.NewComparisonPredicate(
					mustTestFieldOrdinal(t, outerRow, 0),
					predicates.Comparison{Type: predicates.ComparisonIsNull},
				)},
				orders,
			))
			result := values.NewRecordConstructorValue(
				values.RecordConstructorField{Name: "C", Value: mustTestFieldOrdinal(t, mustTestQOV(t, outerAlias, integrationCustomerType()), 0)},
				values.RecordConstructorField{Name: "O", Value: mustTestFieldOrdinal(t, mustTestQOV(t, innerAlias, integrationOrderType()), 0)},
				values.RecordConstructorField{Name: "R", Value: mustTestQOV(t, outerAlias, integrationCustomerType())},
			)
			plan := mustExecutorConstruct(plans.NewRecordQueryFlatMapPlanFromQuantifiersWithNullSupplying(
				expressions.NamedPhysicalQuantifier(outerAlias, expressions.FinalOf(outer)),
				expressions.NamedPhysicalQuantifier(innerAlias, expressions.FinalOf(inner)),
				outerAlias, innerAlias, result, false, true, false))
			if !plan.NullSupplyingOuter() {
				t.Fatal("setup: the FlatMap does not state a null-supplying outer")
			}

			var got []string
			if _, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				s, err := recordlayer.NewStoreBuilder().SetContext(rtx).
					SetMetaDataProvider(store.GetMetaData()).SetSubspace(testSubspace(t)).Open()
				if err != nil {
					return nil, err
				}
				row := mustExecutorConstruct(plan.ProvidedOutputLayout()).Carrier()
				above := mustExecutorConstruct(newProjectionMapOverForTest([]values.Value{
					mustTestFieldOrdinal(t, row, 0),
					mustTestFieldOrdinal(t, row, 1),
					mustTestFieldOrdinal(t, row, 2),
				}, plan))
				cursor, err := ExecutePlan(ctx, above, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
				if err != nil {
					return nil, err
				}
				defer cursor.Close()
				rows, err := CollectAll(ctx, cursor)
				if err != nil {
					return nil, err
				}
				got = nil
				for _, row := range rows {
					c, cok := row.Positional.Get(0)
					o, ook := row.Positional.Get(1)
					r, rok := row.Positional.Get(2)
					if !cok || !ook || !rok {
						return nil, fmt.Errorf("row %v lacks its three slots", row)
					}
					got = append(got, fmt.Sprintf("C=%v|O=%v", c, o))
					if c == nil && r != nil {
						return nil, fmt.Errorf("the NULL outer's whole row reads %#v, want NULL", r)
					}
				}
				return nil, nil
			}); err != nil {
				t.Fatalf("execute: %v", err)
			}
			sort.Strings(got)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("rows = %v, want %v", got, tc.want)
			}
		})
	}
}
