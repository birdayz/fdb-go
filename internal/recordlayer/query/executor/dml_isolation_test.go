package executor

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// Java refuses a data-modification plan at SNAPSHOT isolation before it opens
// its child (QueryPlanUtils.enforceSerializable, called first in
// RecordQueryAbstractDataModificationPlan and RecordQueryDeletePlan.executePlan),
// dry run or not.
func TestDMLPlansRefuseSnapshotIsolationBeforeTheirChild(t *testing.T) {
	t.Parallel()
	scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, integrationOrderType(), false))
	for _, c := range []struct {
		plan plans.RecordQueryPlan
		name string
	}{
		{mustExecutorConstruct(plans.NewRecordQueryInsertPlan(scan, "Order", integrationOrderType())), "RecordQueryInsertPlan"},
		{mustExecutorConstruct(plans.NewRecordQueryUpdatePlan(scan, "Order", []expressions.UpdateTransform{})), "RecordQueryUpdatePlan"},
		{mustExecutorConstruct(plans.NewRecordQueryDeletePlan(scan, "Order")), "RecordQueryDeletePlan"},
	} {
		for _, dryRun := range []bool{false, true} {
			props := recordlayer.DefaultExecuteProperties().WithIsolationLevel(recordlayer.SnapshotIsolation).WithDryRun(dryRun)
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("%s dryRun=%t: opened its child (panic %v)", c.name, dryRun, r)
					}
				}()
				// No store: a plan that opened its child would fail differently.
				_, err = ExecutePlan(context.Background(), c.plan, nil, EmptyEvaluationContext(), nil, props)
			}()
			var argErr *recordlayer.RecordCoreArgumentError
			if !errors.As(err, &argErr) {
				t.Fatalf("%s dryRun=%t: err = %v (%T), want RecordCoreArgumentError", c.name, dryRun, err, err)
			}
			if argErr.Message != "Cannot execute plan at SNAPSHOT isolation level" || argErr.Plan != c.name {
				t.Fatalf("%s dryRun=%t: %+v", c.name, dryRun, argErr)
			}
		}
	}
}
