package embedded

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
)

// depsMetaData is a real RecordMetaData, not a stand-in: the dependency check
// asks it for each index's lastModifiedVersion, so a fake would be testing the
// fake.
func depsMetaData(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	tmpl, err := buildSchemaTemplateFromDDL(ordersSchema)
	if err != nil {
		t.Fatalf("schema DDL: %v", err)
	}
	return tmpl.Underlying()
}

func depsOn(t *testing.T, md *recordlayer.RecordMetaData, names ...string) planIndexDependencies {
	t.Helper()
	out := planIndexDependencies{}
	for _, name := range names {
		idx := md.GetIndex(name)
		if idx == nil {
			t.Fatalf("schema declares no index %s", name)
		}
		out = append(out, predicates.UsedIndex{
			Name:                name,
			LastModifiedVersion: idx.LastModifiedVersion,
		})
	}
	return out
}

func allReadable(md *recordlayer.RecordMetaData) map[string]recordlayer.IndexState {
	states := map[string]recordlayer.IndexState{}
	for name := range md.GetAllIndexes() {
		states[name] = recordlayer.IndexStateReadable
	}
	return states
}

// The check is SCOPED: an index the plan does not depend on may change state
// freely. Signing the store's whole index-state snapshot instead meant an
// unrelated index build failed every in-flight statement in the database with a
// retryable error — an outage, not a safety measure.
func TestValidatePlanIndexDependenciesIgnoresUnrelatedIndexes(t *testing.T) {
	t.Parallel()
	md := depsMetaData(t)
	states := allReadable(md)
	states["IDX_STATUS"] = recordlayer.IndexStateWriteOnly
	states["IDX_AMOUNT"] = recordlayer.IndexStateDisabled

	if err := validatePlanIndexDependencies(depsOn(t, md, "IDX_CUSTOMER"), md, states); err != nil {
		t.Fatalf("unrelated index states invalidated the plan: %v", err)
	}
	if err := validatePlanIndexDependencies(nil, md, states); err != nil {
		t.Fatalf("a plan with no index dependencies was invalidated: %v", err)
	}
}

// The check is DIRECTIONAL. Only a dependency CEASING to be readable
// invalidates. An index excluded at planning that has since become readable
// leaves the plan correct and merely less optimal — Java's
// RecordStoreState.compatibleWith reaches the same answer structurally, by
// iterating only the current state's non-readable exceptions.
func TestValidatePlanIndexDependenciesIsDirectional(t *testing.T) {
	t.Parallel()
	md := depsMetaData(t)
	// IDX_STATUS was non-readable at planning, so it is not a dependency; it is
	// readable now. Nothing to invalidate.
	if err := validatePlanIndexDependencies(
		depsOn(t, md, "IDX_CUSTOMER"), md, allReadable(md),
	); err != nil {
		t.Fatalf("an index becoming readable invalidated a plan that never used it: %v", err)
	}
	// The other direction must still fire, or the assertion above is satisfied
	// by a check that never fires at all.
	states := allReadable(md)
	states["IDX_CUSTOMER"] = recordlayer.IndexStateWriteOnly
	err := validatePlanIndexDependencies(depsOn(t, md, "IDX_CUSTOMER"), md, states)
	if err == nil {
		t.Fatal("a dependency going WRITE_ONLY did not invalidate the plan")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeSerializationFailure {
		t.Fatalf("stale-plan error = %T %v, want SQLSTATE %s", err, err, api.ErrCodeSerializationFailure)
	}
}

// READABLE_UNIQUE_PENDING is SCANNABLE, so a check written against
// scannability would let it through. Java asks isReadable — exact equality with
// READABLE — because a unique-pending index has not proven the uniqueness a
// plan may have assumed from it.
func TestValidatePlanIndexDependenciesRejectsUniquePending(t *testing.T) {
	t.Parallel()
	md := depsMetaData(t)
	states := allReadable(md)
	states["IDX_CUSTOMER"] = recordlayer.IndexStateReadableUniquePending
	if err := validatePlanIndexDependencies(
		depsOn(t, md, "IDX_CUSTOMER"), md, states,
	); err == nil {
		t.Fatal("READABLE_UNIQUE_PENDING was accepted for a plan that depends on the index")
	}
}

// An index absent from the store's state map is one the store's metadata no
// longer names — Java's !recordMetaData.hasIndex leg.
func TestValidatePlanIndexDependenciesRejectsDroppedIndex(t *testing.T) {
	t.Parallel()
	md := depsMetaData(t)
	states := allReadable(md)
	delete(states, "IDX_CUSTOMER")
	if err := validatePlanIndexDependencies(
		depsOn(t, md, "IDX_CUSTOMER"), md, states,
	); err == nil {
		t.Fatal("a dependency the store no longer names was accepted")
	}
}

// A dependency recreated under the same name with a different definition is a
// different index. Execution opens the store with the CONNECTION's metadata,
// which can be newer than the plan's, so the name alone does not identify it.
func TestValidatePlanIndexDependenciesRejectsRedefinedIndex(t *testing.T) {
	t.Parallel()
	md := depsMetaData(t)
	deps := depsOn(t, md, "IDX_CUSTOMER")
	deps[0].LastModifiedVersion++
	if err := validatePlanIndexDependencies(deps, md, allReadable(md)); err == nil {
		t.Fatal("a redefined index was accepted under its old identity")
	}
}

// A scalar subquery is planned as its OWN plan, hanging off the statement rather
// than as a child of the main plan, so a walk from the main plan's root cannot
// reach it. Its leaves scan indexes exactly as the main plan's do.
//
// This is the shape where a walk-based collector looks complete and is not: the
// subquery's index would be an unguarded dependency, and it fails in the
// direction that matters — the plan keeps running against an index the store has
// taken away.
func TestCollectPlanIndexDependenciesIncludesScalarSubqueryIndexes(t *testing.T) {
	t.Parallel()
	md := depsMetaData(t)
	const sql = "SELECT id FROM orders WHERE amount = " +
		"(SELECT max(amount) FROM orders WHERE status = 'shipped')"
	plan, subs, err := PlanRecordQueryWithSubqueries(sql, md, nil)
	if err != nil {
		t.Fatalf("plan %q: %v", sql, err)
	}
	if len(subs) == 0 {
		t.Fatalf("the query planned no scalar subquery, so this test cannot "+
			"observe what it exists to observe: %s", plan.Explain())
	}

	subOnly := map[string]struct{}{}
	collectDependedOnIndexNames(subs[0].Plan, subOnly)
	if len(subOnly) == 0 {
		t.Fatalf("the scalar subquery scanned no index, so it contributes no "+
			"dependency to lose: %s", subs[0].Plan.Explain())
	}

	deps := collectPlanIndexDependencies(md, plan, subs)
	got := map[string]struct{}{}
	for _, dep := range deps {
		got[dep.Name] = struct{}{}
	}
	for name := range subOnly {
		if _, ok := got[name]; !ok {
			t.Fatalf("index %s is scanned by a scalar subquery but is not a "+
				"dependency of the statement: deps=%v", name, deps)
		}
	}
}

// The DISTINCT-elision proof is the one dependency kind whose plan names the
// index NOWHERE — the elision's whole point is that the winning access path is
// the base-record scan. A scan-only collector looks complete on it and is not,
// and it fails in the direction that matters: the statement keeps running and
// returns DUPLICATE ROWS rather than an error.
//
// This is what replaced collectPlanIndexDependencies' `proofOnly []string`
// parameter. That seam had no producer at any of its three call sites nor in any
// test, so it read as coverage while collecting nothing; the stamp is a producer
// and this is the test of the walk that reads it.
func TestCollectPlanIndexDependenciesIncludesDistinctProofStamp(t *testing.T) {
	t.Parallel()
	md := depsMetaData(t)

	plan, err := PlanRecordQueryWithMetadata("SELECT id FROM orders", md, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	stampable, ok := plan.(plans.DistinctProofStampable)
	if !ok {
		t.Fatalf("the planned root %T cannot carry a proof stamp, so this test "+
			"cannot observe what it exists to observe: %s", plan, plan.Explain())
	}

	unstamped := collectPlanIndexDependencies(md, plan, nil)
	for _, dep := range unstamped {
		if dep.Name == "IDX_CUSTOMER" {
			t.Fatalf("IDX_CUSTOMER is already a dependency of the UNSTAMPED plan, "+
				"so the stamp arm below would pass with the walk never reading it: %v",
				unstamped)
		}
	}

	stamped := stampable.WithDistinctProofIndexName("IDX_CUSTOMER")
	deps := collectPlanIndexDependencies(md, stamped, nil)
	found := false
	for _, dep := range deps {
		if dep.Name != "IDX_CUSTOMER" {
			continue
		}
		found = true
		// The stamp records the NAME only; the version is resolved here, from
		// the same planning metadata that supplies it for a scanned index. Two
		// authorities for one field is how they drift apart.
		if want := md.GetIndex("IDX_CUSTOMER").LastModifiedVersion; dep.LastModifiedVersion != want {
			t.Fatalf("proof dependency carries lastModifiedVersion %d, metadata says %d",
				dep.LastModifiedVersion, want)
		}
	}
	if !found {
		t.Fatalf("a plan stamped with a DISTINCT-elision proof over IDX_CUSTOMER "+
			"does not depend on it: deps=%v plan=%s", deps, stamped.Explain())
	}
}
