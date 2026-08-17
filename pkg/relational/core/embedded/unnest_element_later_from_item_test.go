package embedded

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
	"fdb.dev/pkg/relational/core/query/logical"
)

func unnestFrontendMetadata(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	b := metadata.NewSchemaTemplateBuilder().SetName("unnest_frontend")
	b.AddTable("T1", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("ARR1", api.NewArrayType(api.NewLongType(false), true), 2),
	}, []string{"ID"})
	b.AddTable("U", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("V", api.NewLongType(true), 2),
	}, []string{"ID"})
	b.AddTable("PA", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
	}, []string{"ID"})
	b.AddTable("PB", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
	}, []string{"ID"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	return tmpl.Underlying()
}

func TestDuplicateUnnestAliasInsideExistsIsRejectedBeforeTranslation(t *testing.T) {
	t.Parallel()
	md := unnestFrontendMetadata(t)
	root, err := parseQueryFromSelect(t,
		`SELECT "ID" FROM T1 WHERE EXISTS (SELECT 1 FROM T1, T1."ARR1" AS "V", U AS "V")`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, err = NewPlanVisitorWithSchema(md, "s").VisitQuery(root)
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeDuplicateAlias {
		t.Fatalf("VisitQuery error = %v, want %s before subquery attachment/translation", err, api.ErrCodeDuplicateAlias)
	}
}

func TestQualifiedStarOverScalarUnnestKeepsExactWholeObjectValue(t *testing.T) {
	t.Parallel()
	md := unnestFrontendMetadata(t)
	root, err := parseQueryFromSelect(t, `SELECT "V".* FROM T1, T1."ARR1" AS "V"`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	op, err := NewPlanVisitorWithSchema(md, "s").VisitQuery(root)
	if err != nil {
		t.Fatalf("VisitQuery: %v", err)
	}
	proj := findProjection(op)
	if proj == nil || len(proj.ProjectedValues) != 1 || proj.ProjectedValues[0] == nil {
		t.Fatalf("qualified-star projection = %#v, want one resolved exact value", proj)
	}
	qov, ok := values.AsQuantifiedObjectValue(proj.ProjectedValues[0])
	if !ok {
		t.Fatalf("qualified-star value = %T %v, want whole scalar QOV", proj.ProjectedValues[0], proj.ProjectedValues[0])
	}
	if qov.Correlation() != values.NamedCorrelationIdentifier("V") ||
		qov.FlowedType().Code() != values.TypeCodeLong || qov.FlowedType().IsNullable() {
		t.Fatalf("qualified-star QOV = %s:%s, want V:LONG NOT NULL", qov.Correlation(), qov.FlowedType())
	}
}

func TestSchemaAliasCollisionRetainsOnlyTheAuthoredTableQualifier(t *testing.T) {
	t.Parallel()
	md := unnestFrontendMetadata(t)
	root, err := parseQueryFromSelect(t,
		`SELECT PA."ID" AS "PID", "B"."ID" AS "BID" FROM PA AS "s", "s"."PB" AS "B"`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	op, err := NewPlanVisitorWithSchema(md, "s").VisitQuery(root)
	if err != nil {
		t.Fatalf("VisitQuery: %v", err)
	}
	proj := findProjection(op)
	if proj == nil || len(proj.ProjectedValues) != 2 {
		t.Fatalf("projection = %#v, want two exact slots", proj)
	}
	for i, want := range []values.CorrelationIdentifier{
		values.NamedCorrelationIdentifier("S"), values.NamedCorrelationIdentifier("B"),
	} {
		fv, ok := values.AsFieldValue(proj.ProjectedValues[i])
		if !ok {
			t.Fatalf("slot %d = %T %v, want FieldValue", i, proj.ProjectedValues[i], proj.ProjectedValues[i])
		}
		qov, ok := values.AsQuantifiedObjectValue(fv.ChildValue())
		if !ok || qov.Correlation() != want {
			t.Fatalf("slot %d root = %v, want QOV(%s)", i, fv.ChildValue(), want)
		}
		if got := fv.Path().Ordinals(); len(got) != 1 || got[0] != 0 {
			t.Fatalf("slot %d path = %v, want [0]", i, got)
		}
	}

	negative, err := parseQueryFromSelect(t, `SELECT PA."ID" FROM PA AS "x", PB AS "B"`)
	if err != nil {
		t.Fatalf("parse negative: %v", err)
	}
	if _, err := NewPlanVisitorWithSchema(md, "s").VisitQuery(negative); err == nil {
		t.Fatal("ordinary alias X unexpectedly retained the hidden PA table qualifier")
	}
}

// TestUnnestElementCarriesExactScalarWithALaterFromItem is the logical-tree
// counterpart of TestUnnestElementQuantifierCarriesExactScalar with ONE more
// FROM item. The extra item changes which resolver mint emits the element's
// quantified object:
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
// RFC-232 no longer permits UNKNOWN quantified objects. The virtual unnest
// table is lookup metadata, while QOV(X) flows the exact array element itself:
// LONG NOT NULL for this fixture, never RECORD<X ...>. The logical tree is the
// last surface on which this resolver mint is still directly observable.
//
// Do not "strengthen" this into a physical-plan check. The rewrite that erases
// the type is incidental — it is not a guard, nothing pins it, and a shape
// where it does not fire is a shape with silently wrong leg windows.
func TestUnnestElementCarriesExactScalarWithALaterFromItem(t *testing.T) {
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
	seen := map[string][]values.Type{}
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
					if qov, isQ := values.AsQuantifiedObjectValue(n); isQ {
						seen[qov.Correlation().Name()] = append(seen[qov.Correlation().Name()], qov.FlowedType())
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
		if ty != nil && ty.Code() == values.TypeCodeRecord {
			typedA = true
		}
	}
	if !typedA {
		t.Fatalf("correlation A's projected quantifier object states no ROW (%v). The "+
			"resolver's correlated mint carries the source's declared row so a "+
			"leg-correlated read can state its own identity; if that regressed, the "+
			"assertion below is vacuous.\n  seen: %v", aTypes, seen)
	}

	// THE BOUNDARY. The unnest element's quantifier must state the exact scalar
	// element — here, where ResolveColumnShadowingQualified is the mint.
	xTypes, sawX := seen["X"]
	if !sawX {
		t.Fatalf("no projected quantifier object for the unnest binding X; the virtual "+
			"scope source likely still exposes UNKNOWN and exact QOV construction declined. "+
			"The bare `\"X\"` projection must reach ResolveColumnShadowingQualified.\n  seen: %v", seen)
	}
	for _, ty := range xTypes {
		if ty == nil || ty.Code() != values.TypeCodeLong || ty.IsNullable() {
			t.Fatalf("the unnest ELEMENT quantifier X states %v, want exact LONG NOT NULL. "+
				"This shape resolves bare `\"X\"` through ResolveColumnShadowingQualified; "+
				"the virtual one-column lookup table must not become the flowed type, and "+
				"UNKNOWN is no longer an admissible QOV.\n  seen: %v", ty, seen)
		}
	}
}
