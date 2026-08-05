package embedded

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/recordlayer"
)

// A UNIQUE index whose filtering predicate is a Go CLOSURE — set through
// Index.SetPredicate rather than SetPredicateProto — must not be admitted as a
// DISTINCT-elision proof, and must not become a value-scan candidate at all.
//
// RFC-210 §5.1 clause 4 refuses a SPARSE unique index because its UNIQUE
// declaration constrains only the rows its predicate ADMITS and says nothing
// about the rows it excludes, which may hold arbitrarily many duplicates of an
// admitted value. The clause is implemented as `predicateProto != nil`.
//
// That reads sparseness off the SERIALIZED representation, and the two
// representations are not the same question:
//
//	SetPredicateProto  — publishes BOTH the proto and the evaluator closure
//	SetPredicate       — publishes ONLY the closure, and NILS the proto
//
// The index maintainers gate entry creation on the CLOSURE
// (index_maintainer.go: `m.index.Predicate == nil || m.index.Predicate(...)`),
// so a closure-only index is every bit as sparse on disk as a proto one. Only
// the planner's view of it differs, and it differs in the unsafe direction: nil
// proto reads as "unfiltered".
//
// This is not a Java parity question — Java has no programmatic predicate API
// at all; its Index.getPredicate() is parsed from the metadata proto and cannot
// exist without it. The closure is a Go-only extension, so the obligation is
// entirely on Go to fail closed.
//
// The generator already asks the RIGHT question one screen above this, and says
// why: HasFilteringPredicate covers both representations and treats a closure as
// unprovable-therefore-filtering. The defect is that the answer is computed and
// then DISCARDED — only the proto is threaded onto the candidate, so everything
// downstream that asks "is this index complete?" is answered from the
// representation rather than from the fact.
func TestClosureSparseUniqueIndex_IsNeverAnEliminationProof(t *testing.T) {
	t.Parallel()

	const ddl = "CREATE TABLE CS (ID BIGINT, EMAIL STRING, KEEP BIGINT, PRIMARY KEY (ID))\n" +
		"CREATE UNIQUE INDEX CS_EMAIL ON CS(EMAIL)"

	tmpl, err := buildSchemaTemplateFromDDL(ddl)
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	md := tmpl.Underlying()

	idx := md.GetIndex("CS_EMAIL")
	if idx == nil {
		t.Fatal("CS_EMAIL was not built")
	}
	// Precondition: as declared, this is a FULL unique index and therefore a
	// legitimate proof. Without this the assertions below could pass because
	// nothing was ever provable on this shape.
	if !idx.IsUnique() {
		t.Fatal("precondition: CS_EMAIL must be unique")
	}
	if idx.GetPredicateProto() != nil {
		t.Fatal("precondition: CS_EMAIL must start with no stored predicate")
	}
	full, err := PlanRecordQueryAssertingAllIndexesReadable(
		"SELECT DISTINCT EMAIL FROM CS", md, nil)
	if err != nil {
		t.Fatalf("plan the control: %v", err)
	}
	controlPlan := full.Explain()
	if !strings.Contains(controlPlan, "CS_EMAIL") {
		t.Fatalf("the CONTROL drew no proof from the full unique index: %s\n"+
			"Every assertion below claims the CLOSURE case is refused "+
			"specifically, and that claim is vacuous if nothing is provable here.",
			controlPlan)
	}

	// Now make it sparse the closure way. Nothing else changes: same key, same
	// UNIQUE bit, same record type. The only difference is that the planner can
	// no longer SEE the filter.
	idx.SetPredicate(func(msg proto.Message) bool { return true })

	// The metadata's own answer is already correct — this is the fact the
	// candidate boundary has to stop discarding.
	if !idx.HasFilteringPredicate() {
		t.Fatal("Index.HasFilteringPredicate reported a closure-only predicate as " +
			"NON-filtering. A Go closure is opaque and cannot be proved " +
			"tautological, so it must fail closed; if this flipped, every " +
			"completeness decision downstream silently widened")
	}
	if idx.GetPredicateProto() != nil {
		t.Fatal("SetPredicate left a proto behind, so this fixture is exercising " +
			"the proto path and not the closure path it exists for")
	}

	sparsePlan, err := PlanRecordQueryAssertingAllIndexesReadable(
		"SELECT DISTINCT EMAIL FROM CS", md, nil)
	if err != nil {
		// A hard refusal is an acceptable outcome; what is not acceptable is a
		// plan that silently trusts the index.
		return
	}
	explained := sparsePlan.Explain()
	t.Logf("EXPLAIN closure-sparse => %s", explained)

	if !strings.Contains(explained, "Distinct(") {
		t.Fatalf("the DISTINCT was ELIDED over a CLOSURE-sparse unique index: %s\n"+
			"The index holds one entry per ADMITTED record and says nothing about "+
			"the records its closure rejects, which may duplicate an admitted "+
			"EMAIL. Eliding on that proof emits them, and the filtered-index "+
			"execution guard cannot catch it because the proving index is never "+
			"scanned — the plan is a base-table scan wearing a proof stamp.",
			explained)
	}
	if strings.Contains(explained, "narrowed-by") {
		t.Fatalf("the DISTINCT was NARROWED by a CLOSURE-sparse unique index: %s\n"+
			"Narrowing retains only exempt (NULL/NaN) keys and passes every other "+
			"row straight through, so the excluded duplicates reach the output.",
			explained)
	}
	if strings.Contains(explained, "distinct-by") {
		t.Fatalf("a closure-sparse unique index was recorded as a proof "+
			"dependency: %s", explained)
	}
	// Beyond clause 4: an index whose filter the planner cannot see must not be
	// served as though it held every record. That is the same failure the
	// stored-predicate path guards by attaching a candidate-side predicate, and
	// a closure has none to attach.
	if strings.Contains(explained, "IndexScan(CS_EMAIL") {
		t.Fatalf("a CLOSURE-sparse index was scanned as if it were complete: %s\n"+
			"Its entries cover only the records the closure admitted, so serving "+
			"the query from it drops every excluded record from the answer.",
			explained)
	}
}

// The same fact stated where the admission decision is made, so the pin does not
// depend on a particular query planning a particular way.
func TestClosureSparseUniqueIndex_MetadataReportsFiltering(t *testing.T) {
	t.Parallel()

	idx := recordlayer.NewIndex("CLOSURE_U", recordlayer.Field("EMAIL")).SetUnique()
	if idx.HasPredicate() || idx.HasFilteringPredicate() {
		t.Fatal("a freshly built index reports a predicate")
	}
	idx.SetPredicate(func(msg proto.Message) bool { return true })
	if !idx.HasPredicate() {
		t.Fatal("HasPredicate missed a closure predicate")
	}
	if !idx.HasFilteringPredicate() {
		t.Fatal("HasFilteringPredicate missed a closure predicate. A closure " +
			"cannot be proved tautological, so it must count as filtering; " +
			"reading it as non-filtering is what lets a sparse index be treated " +
			"as complete")
	}
	if idx.GetPredicateProto() != nil {
		t.Fatal("SetPredicate published a proto")
	}
}
