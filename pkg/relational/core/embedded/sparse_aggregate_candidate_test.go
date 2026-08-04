package embedded

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	gen "fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// TestSparseAggregateIndexIsNotACandidate_ProgrammaticPredicate pins that a
// filtered aggregate index does not become a match candidate when its predicate
// is a programmatic Go closure rather than a serialized proto.
//
// Read this as a NEGATIVE result, deliberately kept. Deriving sparseness from
// the predicate proto alone would read a closure-predicated index as DENSE and
// give it an aggregate candidate that ignores the filter — an aggregate over
// only the rows the closure admitted, reported as the aggregate over the whole
// group. That is a wrong answer, and it does NOT reproduce today. Both this
// assertion and the one below hold with sparseness derived either way, so this
// test does not detect that change; the invariant test at the bottom of the file
// is what explains why, and what re-arms if the invariant goes.
//
// It is kept anyway because "a filtered aggregate index is not a candidate" is
// the property, and it should be pinned at the level a user can observe rather
// than only at the level where the current implementation happens to enforce it.
func TestSparseAggregateIndexIsNotACandidate_ProgrammaticPredicate(t *testing.T) {
	t.Parallel()

	const ddl = `
CREATE TABLE ai (pk BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (pk))
CREATE INDEX ai_sum_g AS SELECT SUM(v) FROM ai GROUP BY g
`
	const query = "SELECT g, SUM(v) FROM ai GROUP BY g"

	tmpl, err := buildSchemaTemplateFromDDL(ddl)
	if err != nil {
		t.Fatalf("schema DDL: %v", err)
	}
	md := tmpl.Underlying()

	// Baseline: while the index is genuinely dense it IS a candidate. Without
	// this the assertion below would pass on a query that never used the index.
	basePlan, err := PlanQueryWithMetadata(query, md, nil)
	if err != nil {
		t.Fatalf("baseline planning: %v", err)
	}
	if !strings.Contains(basePlan, "AI_SUM_G") {
		t.Fatalf("baseline: the dense aggregate index is not being used, so this test "+
			"cannot observe it being withdrawn.\n  plan: %s", basePlan)
	}

	// Now make it sparse the ONLY way that carries no proto: a Go closure.
	owner := md.GetAllIndexes()["AI_SUM_G"]
	if owner == nil {
		t.Fatalf("AI_SUM_G missing from metadata")
	}
	owner.Predicate = func(proto.Message) bool { return true }
	if owner.GetPredicateProto() != nil {
		t.Fatal("fixture carries a serialized predicate; it cannot cover the " +
			"proto-invisible case that motivated this test")
	}
	if !owner.HasFilteringPredicate() {
		t.Fatal("a programmatic predicate must read as filtering — it is opaque and " +
			"cannot be proved tautological; the fixture is not sparse")
	}

	plan, err := PlanQueryWithMetadata(query, md, nil)
	if err != nil {
		t.Fatalf("planning with the filtered index: %v", err)
	}
	if strings.Contains(plan, "AI_SUM_G") {
		t.Fatalf("a FILTERED aggregate index is still a match candidate.\n  plan: %s\n"+
			"Its entries cover only the rows the predicate admitted, so an aggregate "+
			"served from it is a sum over a subset reported as the sum over the whole "+
			"group. Sparseness must be asked of recordlayer.Index (which sees the "+
			"programmatic predicate too), not derived from the serialized proto alone.",
			plan)
	}
	if !strings.Contains(plan, "StreamingAgg") {
		t.Fatalf("withdrawing the index left no correct fallback.\n  plan: %s\n"+
			"Aggregating over the base records is the right answer here; trading a "+
			"wrong index answer for no plan at all is not the fix.", plan)
	}
}

// TestIndexPredicateRepresentationsArePublishedTogether pins the invariant that
// makes the wrong answer above unreachable from stored metadata, and names what
// re-arms it.
//
// An index's predicate has two representations — the serialized proto and the
// compiled Go evaluator. SetPredicateProto publishes BOTH, so an index rebuilt
// from stored metadata can never present the evaluator without the proto. That
// is the only reason a sparseness test written against the proto alone gives the
// same answer as one written against Index.HasFilteringPredicate, and nothing
// else states it.
//
// If this ever splits — a load path that installs the evaluator lazily, or one
// that keeps the proto without compiling it — then every proto-derived
// sparseness test in the planner silently starts disagreeing with the index's
// own answer, and a filtered aggregate index becomes a match candidate again.
func TestIndexPredicateRepresentationsArePublishedTogether(t *testing.T) {
	t.Parallel()

	idx := recordlayer.NewIndex("SUM_G", recordlayer.GroupBy(
		recordlayer.Field("v"), recordlayer.Field("g")))
	idx.Type = recordlayer.IndexTypeSum

	if idx.GetPredicateProto() != nil || idx.Predicate != nil {
		t.Fatal("a fresh index already carries a predicate; fixture is not clean")
	}

	tautology := &gen.Predicate{ConstantPredicate: &gen.ConstantPredicate{
		Value: gen.ConstantPredicate_TRUE.Enum(),
	}}
	if err := idx.SetPredicateProto(tautology); err != nil {
		t.Fatalf("SetPredicateProto: %v", err)
	}
	if idx.GetPredicateProto() == nil {
		t.Fatal("SetPredicateProto left no serialized predicate")
	}
	if idx.Predicate == nil {
		t.Fatal("SetPredicateProto published the proto WITHOUT the compiled evaluator. " +
			"The two representations are now separable, so a planner sparseness test " +
			"written against the proto no longer agrees with Index.HasFilteringPredicate " +
			"— re-check every such test, starting with buildMatchCandidates.")
	}

	// And the clearing direction: neither may outlive the other.
	if err := idx.SetPredicateProto(nil); err != nil {
		t.Fatalf("SetPredicateProto(nil): %v", err)
	}
	if idx.GetPredicateProto() != nil || idx.Predicate != nil {
		t.Fatal("clearing the predicate left one representation behind")
	}
}
