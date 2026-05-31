package embedded

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
)

// TestQualify_BuildsDistanceRankPredicate drives the full SQL parse →
// QUALIFY predicate path and verifies the vector K-NN ROW_NUMBER() OVER(...)
// <= K filter lowers to a DistanceRank comparison over the distance-specialized
// row-number value (Java's transformComparisonMaybe shape).
func TestQualify_BuildsDistanceRankPredicate(t *testing.T) {
	t.Parallel()
	tmpl, err := buildSchemaTemplateFromDDL(`CREATE TABLE docs (
			zone string, doc_id string, embedding vector(3, half),
			PRIMARY KEY (zone, doc_id))
		CREATE VECTOR INDEX doc_idx USING HNSW ON docs(embedding)
			PARTITION BY (zone) OPTIONS (METRIC = EUCLIDEAN_METRIC)`)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	md := tmpl.Underlying()

	sql := `SELECT doc_id FROM docs WHERE zone = 'z1'
		QUALIFY ROW_NUMBER() OVER (
			PARTITION BY zone
			ORDER BY euclidean_distance(embedding, embedding)
			OPTIONS ef_search = 64
		) <= 3`
	root, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sel := root.Statements().AllStatement()[0].SelectStatement()
	sq, err := extractSelectParts(sel)
	if err != nil {
		t.Fatalf("extractSelectParts: %v", err)
	}
	if sq.qualifyExpr == nil {
		t.Fatal("qualifyExpr was not captured from the QUALIFY clause")
	}

	pred, ok := buildQualifyPredicate(md, sq, nil)
	if !ok {
		t.Fatal("buildQualifyPredicate returned ok=false")
	}
	cp, ok := pred.(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("qualify predicate is %T, want *ComparisonPredicate", pred)
	}
	if _, ok := cp.Operand.(*values.EuclideanDistanceRowNumberValue); !ok {
		t.Errorf("LHS is %T, want *EuclideanDistanceRowNumberValue", cp.Operand)
	}
	if cp.Comparison.Type != predicates.ComparisonDistanceRankLessThanOrEq {
		t.Errorf("comparison type = %v, want DistanceRankLessThanOrEq", cp.Comparison.Type)
	}
	if cp.Comparison.QueryVector == nil {
		t.Error("DistanceRank comparison missing query vector")
	}
	if cp.Comparison.EfSearch == nil || *cp.Comparison.EfSearch != 64 {
		t.Errorf("ef_search = %v, want 64", cp.Comparison.EfSearch)
	}
	// The k (top-K) comparand is carried on the DistanceRank comparison; its
	// concrete scalar value is exercised end-to-end in the FDB test (9.4).
	if cp.Comparison.Operand == nil {
		t.Error("DistanceRank comparison missing k (top-K comparand)")
	}
}

// TestQualify_InvertedComparison verifies K >= ROW_NUMBER() OVER(...) lowers to
// the same DistanceRank predicate as ROW_NUMBER() OVER(...) <= K (Java tries
// both argument orderings).
func TestQualify_InvertedComparison(t *testing.T) {
	t.Parallel()
	tmpl, err := buildSchemaTemplateFromDDL(`CREATE TABLE docs (
			zone string, doc_id string, embedding vector(3, half),
			PRIMARY KEY (zone, doc_id))
		CREATE VECTOR INDEX doc_idx USING HNSW ON docs(embedding)
			PARTITION BY (zone) OPTIONS (METRIC = EUCLIDEAN_METRIC)`)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	md := tmpl.Underlying()

	sql := `SELECT doc_id FROM docs WHERE zone = 'z1'
		QUALIFY 3 >= ROW_NUMBER() OVER (PARTITION BY zone ORDER BY euclidean_distance(embedding, embedding))`
	root, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sel := root.Statements().AllStatement()[0].SelectStatement()
	sq, err := extractSelectParts(sel)
	if err != nil {
		t.Fatalf("extractSelectParts: %v", err)
	}
	pred, ok := buildQualifyPredicate(md, sq, nil)
	if !ok {
		t.Fatal("buildQualifyPredicate returned ok=false for inverted comparison")
	}
	cp, ok := pred.(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("qualify predicate is %T, want *ComparisonPredicate", pred)
	}
	if cp.Comparison.Type != predicates.ComparisonDistanceRankLessThanOrEq {
		t.Errorf("comparison type = %v, want DistanceRankLessThanOrEq (3 >= RN ≡ RN <= 3)", cp.Comparison.Type)
	}
	if _, ok := cp.Operand.(*values.EuclideanDistanceRowNumberValue); !ok {
		t.Errorf("LHS is %T, want *EuclideanDistanceRowNumberValue", cp.Operand)
	}
}

// TestAttachOrSynthesizeFilter covers Finding 1 (Torvalds): a QUALIFY predicate
// must not be dropped when there is no WHERE (hence no existing LogicalFilter).
func TestAttachOrSynthesizeFilter(t *testing.T) {
	t.Parallel()
	pred := predicates.NewValuePredicate(values.LiteralValue(true))

	// No filter on the spine → synthesize one above the bare scan.
	scan := logical.NewScan("DOCS", "DOCS")
	got := attachOrSynthesizeFilter(scan, pred)
	f, ok := got.(*logical.LogicalFilter)
	if !ok {
		t.Fatalf("bare scan: result is %T, want *LogicalFilter", got)
	}
	if f.Input != scan || f.Predicate != pred {
		t.Error("bare scan: synthesized filter not wrapping the scan with the predicate")
	}

	// Project(Scan) with no filter → insert filter ABOVE the scan, under the
	// projection: Project(Filter(Scan)).
	scan2 := logical.NewScan("DOCS", "DOCS")
	proj := &logical.LogicalProject{Input: scan2}
	got2 := attachOrSynthesizeFilter(proj, pred)
	if got2 != proj {
		t.Fatalf("project: root changed to %T, want the project unchanged", got2)
	}
	filt, ok := proj.Input.(*logical.LogicalFilter)
	if !ok {
		t.Fatalf("project: input is %T, want a synthesized *LogicalFilter", proj.Input)
	}
	if filt.Input != scan2 || filt.Predicate != pred {
		t.Error("project: synthesized filter not placed directly above the scan")
	}
}
