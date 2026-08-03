package recordlayer

// The exhaustiveness gate behind TestQueryLeavesConsultIsolationLevel.
//
// The isolation test enumerates query leaves by hand, and a hand-written list
// is only as good as whoever last remembered to extend it. It missed one: the
// VECTOR leaf read through tx.Snapshot() unconditionally, so a SERIALIZABLE
// kNN scan took no conflict ranges and a dependent write could commit on a
// stale top-k with no 40001. The behavioural test could not have caught that,
// because the leaf it did not know about is exactly the leaf it did not run.
//
// So the list stops being hand-written where it matters: this gate derives the
// index-type universe FROM THE SOURCE (the IndexType* const block in index.go,
// parsed, not transcribed) and fails the build when a type appears that nobody
// has classified. Adding an index type is then a two-line decision — is its
// scan a query leaf, and if so which leaf entry covers it — rather than a
// silent omission that surfaces as a non-serializable commit years later.
//
// It is deliberately NOT a test that "every index type has a leaf entry": most
// types share the generic index-scan leaf, and several cannot be scanned from
// a plan at all. What it forbids is an UNDECLARED type.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// leafIsolationVerdict records, for one index type, how its scan reads decide
// snapshot vs serializable. Every IndexType* constant must have one.
type leafIsolationVerdict struct {
	// coveredBy names the subtest in TestQueryLeavesConsultIsolationLevel that
	// exercises this type's scan end to end, or is empty when the type is not
	// a query leaf.
	coveredBy string
	// reason explains a type with no leaf coverage. Required when coveredBy is
	// empty — "why can no plan reach this scan?" is the question that went
	// unasked about VECTOR.
	reason string
}

// leafIsolationDeclaration classifies every index type. A missing entry fails
// the gate; so does an entry that claims neither coverage nor a reason.
var leafIsolationDeclaration = map[string]leafIsolationVerdict{
	// The generic value-index access path: KeyValueCursor consults
	// ExecuteProperties.IsolationLevel directly (key_value_cursor.go).
	"IndexTypeValue":   {coveredBy: "index_scan"},
	"IndexTypeVersion": {coveredBy: "index_scan"},

	// Aggregate/atomic index types. Their maintainers keep no scannable entry
	// stream a plan reads through a leaf cursor: the planner answers from the
	// aggregate value, which is fetched by the aggregate machinery (its
	// snapshot reads are paired with explicit conflict keys — the sanctioned
	// SNAPSHOT + targeted conflict pattern, not a query leaf).
	"IndexTypeCount":          {reason: "aggregate value, not a scannable entry stream"},
	"IndexTypeCountNotNull":   {reason: "aggregate value, not a scannable entry stream"},
	"IndexTypeCountUpdates":   {reason: "aggregate value, not a scannable entry stream"},
	"IndexTypeSum":            {reason: "aggregate value, not a scannable entry stream"},
	"IndexTypeMaxEverLong":    {reason: "aggregate value, not a scannable entry stream"},
	"IndexTypeMinEverLong":    {reason: "aggregate value, not a scannable entry stream"},
	"IndexTypeMaxEverTuple":   {reason: "aggregate value, not a scannable entry stream"},
	"IndexTypeMinEverTuple":   {reason: "aggregate value, not a scannable entry stream"},
	"IndexTypeMaxEverVersion": {reason: "aggregate value, not a scannable entry stream"},

	// Scannable through their own scan types, all of which reach the same
	// keyValueCursor/index_scan machinery for the entry read and therefore the
	// same isolation consultation the index_scan leaf pins.
	"IndexTypeRank":                  {coveredBy: "index_scan"},
	"IndexTypePermutedMin":           {coveredBy: "index_scan"},
	"IndexTypePermutedMax":           {coveredBy: "index_scan"},
	"IndexTypeBitmapValue":           {coveredBy: "index_scan"},
	"IndexTypeText":                  {coveredBy: "index_scan"},
	"IndexTypeTimeWindowLeaderboard": {coveredBy: "index_scan"},
	"IndexTypeMultidimensional":      {coveredBy: "index_scan"},

	// The two vector families. Neither reaches a keyValueCursor — each walks
	// its own structure (HNSW graph / SPANN postings) — so each resolves the
	// read transaction itself through standardIndexMaintainer.readTx, and the
	// vector leaf exercises that end to end. This is the entry that was
	// missing when the leaf shipped snapshot-only.
	"IndexTypeVector":        {coveredBy: "vector_scan_by_distance"},
	"IndexTypeVectorSPFresh": {coveredBy: "vector_scan_by_distance"},
}

// TestQueryLeafIsolationDeclarationIsExhaustive fails when an index type is
// added without saying how its scan decides isolation.
func TestQueryLeafIsolationDeclarationIsExhaustive(t *testing.T) {
	t.Parallel()

	declared := indexTypeConstNames(t)
	if len(declared) < 15 {
		t.Fatalf("parsed only %d IndexType constants from index.go — the parse stopped "+
			"matching the source, so this gate would pass by finding nothing", len(declared))
	}

	for _, name := range declared {
		verdict, ok := leafIsolationDeclaration[name]
		if !ok {
			t.Errorf("index type %s is not classified in leafIsolationDeclaration.\n"+
				"Every index type must say how its SCAN decides snapshot vs serializable, "+
				"because a leaf that reads through tx.Snapshot() directly overrides the "+
				"statement's isolation silently — no error, no conflict range, and a "+
				"dependent write commits on a stale read. That is exactly how the VECTOR "+
				"leaf shipped non-serializable.\n"+
				"Add an entry: {coveredBy: \"<subtest in TestQueryLeavesConsultIsolationLevel>\"} "+
				"if a plan can scan it, or {reason: \"...\"} if no plan can reach it.", name)
			continue
		}
		if verdict.coveredBy == "" && verdict.reason == "" {
			t.Errorf("index type %s has an EMPTY classification: it must name either the "+
				"leaf subtest that covers it or the reason no plan can reach its scan", name)
		}
	}

	for name := range leafIsolationDeclaration {
		found := false
		for _, d := range declared {
			if d == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("leafIsolationDeclaration classifies %s, which is no longer an "+
				"IndexType constant — a stale entry hides the next real omission", name)
		}
	}
}

// indexTypeConstNames parses index.go and returns every IndexType* constant
// name. Parsed rather than transcribed: a transcribed list drifts silently,
// which is the failure this gate exists to prevent.
func indexTypeConstNames(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "index.go", indexGoSource, 0)
	if err != nil {
		t.Fatalf("parse index.go: %v", err)
	}
	var names []string
	ast.Inspect(file, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for _, ident := range vs.Names {
			if strings.HasPrefix(ident.Name, "IndexType") {
				names = append(names, ident.Name)
			}
		}
		return true
	})
	return names
}
