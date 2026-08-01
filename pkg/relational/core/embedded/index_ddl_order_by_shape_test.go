package embedded

import (
	"testing"

	"fdb.dev/pkg/relational/core/parser"
	"fdb.dev/pkg/relational/core/query/logical"
)

// The ORDER-BY-outside-the-projection question CANNOT be answered from the
// logical plan's SHAPE. This file is the measurement that says so.
//
// Java answers it with a set difference over expression VALUES
// (LogicalOperator.java:389-390, `orderByExpressions.difference(output, …)`) and
// converts a non-empty remainder into a select wrapped ABOVE the sort, which
// DdlVisitor.java:217 then rejects as INVALID_COLUMN_REFERENCE. Reading that as
// "the sort must be the top operator" and porting the POSITION rather than the
// set difference looks equivalent and is not — twice over:
//
//  1. In Go, projection-above-sort is the pinned NORMAL layering for every
//     ordered query (VisitSimpleTable builds the sort at step 4 and the
//     SELECT-list projection at step 5; TestSortNeverSitsOverAProjection pins
//     that the projection may never sink below the sort). A positional rule
//     therefore rejects the ordinary shape — the overwhelming majority of the
//     corpus's ordered index definitions.
//
//  2. Go validates ORDER BY against the SOURCE scope, not the projection, so it
//     ACCEPTS `ORDER BY` on a non-projected column — the very input Java
//     rejects. The accepted-by-both shape and the must-be-rejected shape come
//     out of the builder structurally IDENTICAL, so no amount of walking the
//     tree can separate them.
//
// Both facts are asserted below. Together they are the reason RFC-202 D3
// specifies a value-identity subset test between the sort keys and the
// projection's values, and not a position test.

// indexSelectShapes are the ORDER BY index-definition bodies from the vendored
// corpus's orderby.yamsql plus the two shapes that make the point sharpest: a
// covering split and an ORDER BY over a column that is not projected at all.
var indexSelectShapes = []struct {
	name string
	sql  string
	// projected is false when the ORDER BY names a column outside the SELECT
	// list — the shape Java rejects and Go accepts.
	projected bool
}{
	{"order_by_prefix", "SELECT b, c FROM t1 ORDER BY b", true},
	{"order_by_suffix_covering", "SELECT b, c FROM t1 ORDER BY c", true},
	{"order_by_full_permutation", "SELECT b, c FROM t1 ORDER BY c, b", true},
	{"order_by_reversal", "SELECT d, c, b, a FROM t3 ORDER BY a, b, c, d", true},
	{"order_by_desc", "SELECT b, c FROM t1 ORDER BY c, b DESC", true},
	{"order_by_single", "SELECT b FROM t1 ORDER BY b", true},
	{"order_by_unprojected", "SELECT b FROM t1 ORDER BY c", false},
	{"order_by_unprojected_multi", "SELECT b, c FROM t1 ORDER BY a", false},
}

const indexShapeDDL = `
	CREATE TABLE t1(a bigint, b bigint, c bigint, primary key(a))
	CREATE TABLE t3(a bigint, b bigint, c bigint, d bigint, p bigint, primary key(p))
`

// TestIndexDefinitionOrderByIsNotStructurallyDiscriminable is the refutation.
//
// It asserts, for every shape, that the plan is Project-over-Sort — including
// for the two shapes whose ORDER BY leaves the projection. A discriminator that
// asks "is the sort the top operator?" answers NO for all of them, which means
// it both rejects legal definitions and fails to reject the illegal ones.
func TestIndexDefinitionOrderByIsNotStructurallyDiscriminable(t *testing.T) {
	t.Parallel()

	tmpl, err := buildSchemaTemplateFromDDL(indexShapeDDL)
	if err != nil {
		t.Fatalf("schema DDL: %v", err)
	}

	for _, shape := range indexSelectShapes {
		shape := shape
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()
			root, err := parser.Parse(shape.sql)
			if err != nil {
				t.Fatalf("parse %q: %v", shape.sql, err)
			}
			sel := root.Statements().AllStatement()[0].SelectStatement()
			if sel == nil {
				t.Fatalf("not a SELECT: %q", shape.sql)
			}
			op, err := NewPlanVisitor(tmpl.Underlying()).VisitQuery(sel.Query())
			if err != nil {
				t.Fatalf("build %q: %v", shape.sql, err)
			}

			// Every shape — projected or not — must reach a plan at all. Go
			// accepting the unprojected ORDER BY is itself half the finding: a
			// build error there would have made the position test viable.
			if op == nil {
				t.Fatalf("no plan for %q", shape.sql)
			}

			if _, isSort := op.(*logical.LogicalSort); isSort {
				t.Errorf("top operator is a Sort for %q\n%s\n\n"+
					"Go builds the SELECT-list projection ABOVE the sort "+
					"(VisitSimpleTable step 4 then step 5). A Sort on top means that "+
					"layering changed, and RFC-202 D3's argument — that the sort's "+
					"POSITION carries no information about whether ORDER BY left the "+
					"projection — has to be re-derived.", shape.sql, op.Explain(""))
				return
			}
			proj, isProj := op.(*logical.LogicalProject)
			if !isProj {
				t.Fatalf("top operator is %T, expected a Project, for %q\n%s",
					op, shape.sql, op.Explain(""))
			}
			if _, belowIsSort := proj.Input.(*logical.LogicalSort); !belowIsSort {
				t.Errorf("no Sort directly below the Project for %q\n%s",
					shape.sql, proj.Input.Explain(""))
			}
		})
	}
}

// TestIndexDefinitionOrderByUnprojectedIsAccepted pins the second half: Go's
// ORDER BY resolution reaches the SOURCE scope, so a column absent from the
// SELECT list still resolves.
//
// This is a NEGATIVE result and it is load-bearing. Java rejects this input
// (IndexTest.java:710-716, INVALID_COLUMN_REFERENCE "not present in the
// projection list") and the cross-engine plan corpus records the same
// limitation on the query side. RFC-202 D3 must therefore ADD the rejection in
// the index-definition path rather than inherit it from the builder — and if
// this test ever starts failing because the builder began rejecting the shape
// on its own, that decision is re-armed and D3's set-difference test may be
// redundant.
func TestIndexDefinitionOrderByUnprojectedIsAccepted(t *testing.T) {
	t.Parallel()

	tmpl, err := buildSchemaTemplateFromDDL(indexShapeDDL)
	if err != nil {
		t.Fatalf("schema DDL: %v", err)
	}
	for _, shape := range indexSelectShapes {
		if shape.projected {
			continue
		}
		shape := shape
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()
			root, err := parser.Parse(shape.sql)
			if err != nil {
				t.Fatalf("parse %q: %v", shape.sql, err)
			}
			sel := root.Statements().AllStatement()[0].SelectStatement()
			op, err := NewPlanVisitor(tmpl.Underlying()).VisitQuery(sel.Query())
			if err != nil {
				t.Fatalf("Go rejected %q with %v — it used to accept it. Java "+
					"rejects this shape with INVALID_COLUMN_REFERENCE; if the Go "+
					"builder now rejects it too, RFC-202 D3's explicit "+
					"ORDER-BY-subset check may no longer be the only thing standing "+
					"between this input and a silently different index.",
					shape.sql, err)
			}
			if op == nil {
				t.Fatalf("no plan for %q", shape.sql)
			}
			// And the accepted shape is structurally the same as the legal one:
			// a Project over a Sort. That equality is the refutation.
			proj, isProj := op.(*logical.LogicalProject)
			if !isProj {
				t.Fatalf("top operator is %T, expected Project, for %q", op, shape.sql)
			}
			if _, belowIsSort := proj.Input.(*logical.LogicalSort); !belowIsSort {
				t.Errorf("the must-reject shape %q is NO LONGER structurally identical "+
					"to the legal one (%T below the Project, not a Sort). A positional "+
					"discriminator may have become viable; re-read RFC-202 D3.",
					shape.sql, proj.Input)
			}
		})
	}
}
