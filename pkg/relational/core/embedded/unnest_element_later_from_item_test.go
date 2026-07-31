package embedded

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
	"fdb.dev/pkg/relational/core/query/logical"
)

// TestUnnestElementStaysUntypedWithALaterFromItem is
// TestUnnestElementQuantifierStaysUntyped's shape with ONE more FROM item, and
// the extra item changes which resolver mint emits the element's quantifier
// object:
//
//	FROM A, C, C."ARR" AS "X"          -> ResolveIdentifier's correlated arm
//	FROM A, C, C."ARR" AS "X", U       -> ResolveColumnShadowingQualified
//
// The second helper exists precisely because a LATER FROM item's mergeRows
// clobbers the bare `X` key last-leg-wins, so the bare `SELECT "X"` must read
// the qualified `X.X` key instead (RFC-142). It is a different function, and a
// flowed-type guard written only in the first one does not reach it — which was
// the state this test was written against.
//
// # Why this asserts on the LOGICAL tree, not the physical plan
//
// MEASURED on this shape, with the guard absent:
// ResolveColumnShadowingQualified returned `QOV(X) : RECORD<X UNKNOWN NULL>`
// and the Cascades translator received it in the projection's ProjectedValues —
// but the WINNING PHYSICAL plan carried `QOV(X) : UNKNOWN`, because a planner
// rewrite happens to re-mint that read untyped on this shape. So a
// physical-plan assertion here passes with the defect fully present: it cannot
// express it, which is not coverage. The logical tree is the last surface on
// which the resolver's own answer is still the answer, so that is where the
// assertion goes.
//
// Do not "strengthen" this into a physical-plan check. The rewrite that erases
// the type is incidental — it is not a guard, nothing pins it, and a shape
// where it does not fire is a shape with silently wrong leg windows.
func TestUnnestElementStaysUntypedWithALaterFromItem(t *testing.T) {
	t.Parallel()

	b := metadata.NewSchemaTemplateBuilder().SetName("unnest_later_from_item")
	b.AddTable("A", []metadata.ColumnSpec{
		metadata.NewColumnSpec("AID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("K", api.NewLongType(true), 2),
	}, []string{"AID"})
	b.AddTable("C", []metadata.ColumnSpec{
		metadata.NewColumnSpec("CID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("ARR", api.NewArrayType(api.NewLongType(false), true), 2),
	}, []string{"CID"})
	b.AddTable("U", []metadata.ColumnSpec{
		metadata.NewColumnSpec("UID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("X", api.NewLongType(true), 2),
	}, []string{"UID"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}

	root, perr := parseQueryFromSelect(t,
		`SELECT A."K", "X" FROM A, C, C."ARR" AS "X", U WHERE A."K" = U."UID"`)
	if perr != nil {
		t.Fatalf("parse: %v", perr)
	}
	op, berr := buildLogicalPlanForQueryWithCatalog(root, tmpl.Underlying())
	if berr != nil {
		t.Fatalf("build logical plan: %v", berr)
	}

	// Every quantifier object the RESOLVER put on a projected value, by
	// correlation. Projected values are the resolver's verbatim output at this
	// stage — nothing has rewritten them yet.
	seen := map[string][]string{}
	var visit func(logical.LogicalOperator)
	visit = func(node logical.LogicalOperator) {
		if node == nil {
			return
		}
		if proj, isProj := node.(*logical.LogicalProject); isProj {
			for _, v := range proj.ProjectedValues {
				if v == nil {
					continue
				}
				values.WalkValue(v, func(n values.Value) bool {
					if qov, isQ := n.(*values.QuantifiedObjectValue); isQ {
						seen[qov.Correlation.Name()] = append(seen[qov.Correlation.Name()], fmt.Sprint(qov.Typ))
					}
					return true
				})
			}
		}
		if filter, isFilter := node.(*logical.LogicalFilter); isFilter {
			for _, exists := range filter.ExistsSubqueries {
				visit(exists.Plan)
			}
		}
		for _, child := range node.Children() {
			visit(child)
		}
	}
	visit(op)

	// POSITIVE CONTROL. The REAL table's projected read must state its row.
	// Without it the boundary assertion below holds because nothing is typed at
	// all — and the flowed-type narrowing this test guards would then be
	// indistinguishable from having deleted the typing outright.
	aTypes, sawA := seen["A"]
	if !sawA {
		t.Fatalf("no projected quantifier object for correlation A; the shape changed "+
			"and this test no longer probes what it was written for.\n  seen: %v", seen)
	}
	typedA := false
	for _, ty := range aTypes {
		if isRowType(ty) {
			typedA = true
		}
	}
	if !typedA {
		t.Fatalf("correlation A's projected quantifier object states no ROW (%v). The "+
			"resolver's correlated mint carries the source's declared row so a "+
			"leg-correlated read can state its own identity; if that regressed, the "+
			"assertion below is vacuous.\n  seen: %v", aTypes, seen)
	}

	// THE BOUNDARY. The unnest element's quantifier must NOT state a row —
	// here, where ResolveColumnShadowingQualified is the mint.
	xTypes, sawX := seen["X"]
	if !sawX {
		t.Fatalf("no projected quantifier object for the unnest binding X; the bare "+
			"`\"X\"` projection is no longer resolved QOV-correlated, so this test no "+
			"longer reaches ResolveColumnShadowingQualified.\n  seen: %v", seen)
	}
	for _, ty := range xTypes {
		if isRowType(ty) {
			t.Fatalf("the unnest ELEMENT quantifier X states a ROW (%s) on the "+
				"LATER-FROM-ITEM shape.\n\n"+
				"  This shape resolves the bare `\"X\"` through "+
				"ResolveColumnShadowingQualified, not through ResolveIdentifier's "+
				"correlated arm. A flowed-type guard applied at only one of those mints "+
				"leaves the other stating a row, and the two are reached by SQL that "+
				"differs by one FROM item.\n\n"+
				"  values.IsMixedSeedElementType discriminates the element from a join "+
				"leg by exactly this record-ness, so a row here makes the element read "+
				"as a leg: LOUD \"multi-leg row cannot serve a source-relative ordinal\" "+
				"on some shapes, SILENTLY MISSING ROWS on others.\n\n"+
				"  Fix expr.flowedTypeFor — the single authority all three mints route "+
				"through.\n  seen: %v", ty, seen)
		}
	}
}

// isRowType reports whether a rendered values.Type is a record. The rendering
// is compared rather than the Go type because the type travels here as the
// string the plan prints, which is the same surface
// TestUnnestElementQuantifierStaysUntyped reads.
func isRowType(rendered string) bool {
	return len(rendered) >= 6 && rendered[:6] == "RECORD"
}
