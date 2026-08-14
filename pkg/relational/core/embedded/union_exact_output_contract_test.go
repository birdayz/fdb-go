package embedded

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query"
	"fdb.dev/pkg/relational/core/query/logical"
)

const unionExactOutputDDL = `
	CREATE TABLE a (id BIGINT, v BIGINT, PRIMARY KEY (id))
	CREATE TABLE b (id BIGINT, w BIGINT, PRIMARY KEY (id))
	CREATE TABLE c (id BIGINT, s STRING, PRIMARY KEY (id))
`

func buildUnionLogical(t *testing.T, sql string) (logical.LogicalOperator, *recordlayer.RecordMetaData, error) {
	t.Helper()
	tmpl, err := buildSchemaTemplateFromDDL(unionExactOutputDDL)
	if err != nil {
		t.Fatalf("schema DDL: %v", err)
	}
	op, err := NewPlanVisitor(tmpl.Underlying()).VisitQuery(parseQuery(t, sql))
	return op, tmpl.Underlying(), err
}

func findTranslatedUnion(t *testing.T, root *expressions.Reference) *expressions.LogicalUnionExpression {
	t.Helper()
	seen := map[*expressions.Reference]bool{}
	var found *expressions.LogicalUnionExpression
	var walk func(*expressions.Reference)
	walk = func(ref *expressions.Reference) {
		if ref == nil || seen[ref] || found != nil {
			return
		}
		seen[ref] = true
		for _, member := range ref.AllMembers() {
			if union, ok := member.(*expressions.LogicalUnionExpression); ok {
				found = union
				return
			}
			for _, quantifier := range member.GetQuantifiers() {
				walk(quantifier.GetRangesOver())
			}
		}
	}
	walk(root)
	if found == nil {
		t.Fatal("translated plan has no LogicalUnionExpression")
	}
	return found
}

func assertUnionRow(t *testing.T, union *expressions.LogicalUnionExpression, names []string, types []values.Type) {
	t.Helper()
	quantifiers := union.GetQuantifiers()
	if len(quantifiers) != 2 {
		t.Fatalf("UNION has %d legs, want 2", len(quantifiers))
	}
	var first values.Type
	for leg, quantifier := range quantifiers {
		flowed, err := quantifier.GetFlowedObjectType()
		if err != nil {
			t.Fatalf("leg %d flowed type: %v", leg, err)
		}
		record, ok := flowed.(*values.RecordType)
		if !ok || len(record.Fields) != len(names) {
			t.Fatalf("leg %d row = %T %v, want record width %d", leg, flowed, flowed, len(names))
		}
		for i := range names {
			if record.Fields[i].Name != names[i] || !record.Fields[i].FieldType.Equals(types[i]) {
				t.Fatalf("leg %d slot %d = %s %s, want %s %s",
					leg, i, record.Fields[i].Name, record.Fields[i].FieldType, names[i], types[i])
			}
		}
		if leg == 0 {
			first = flowed
		} else if !first.Equals(flowed) {
			t.Fatalf("UNION rows disagree: first %s, leg %d %s", first, leg, flowed)
		}
	}
}

func TestUnionExactOutputContract_NormalizesNamesByOrdinal(t *testing.T) {
	t.Parallel()
	op, md, err := buildUnionLogical(t, `SELECT id, v FROM a UNION ALL SELECT id, w FROM b`)
	if err != nil {
		t.Fatalf("VisitQuery: %v", err)
	}
	ref, _, err := query.TranslateToCascadesWithError(op, md)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	union := findTranslatedUnion(t, ref)
	assertUnionRow(t, union, []string{"ID", "V"}, []values.Type{values.NullableLong, values.NullableLong})

	for leg, quantifier := range union.GetQuantifiers() {
		projection, ok := quantifier.GetRangesOver().Get().(*expressions.LogicalProjectionExpression)
		if !ok {
			t.Fatalf("leg %d = %T, want exact ordinal normalization projection", leg, quantifier.GetRangesOver().Get())
		}
		if got := projection.GetOutputNames(); len(got) != 2 || got[0] != "ID" || got[1] != "V" {
			t.Fatalf("leg %d output names = %v, want [ID V]", leg, got)
		}
	}
	if _, _, err := planWithOptions(t,
		`SELECT id, v FROM a UNION ALL SELECT id, w FROM b`, unionExactOutputDDL, nil); err != nil {
		t.Fatalf("physical planning: %v", err)
	}
}

func TestUnionExactOutputContract_WidensNotNullLiteral(t *testing.T) {
	t.Parallel()
	op, md, err := buildUnionLogical(t, `SELECT v FROM a UNION ALL SELECT 99 FROM b`)
	if err != nil {
		t.Fatalf("VisitQuery: %v", err)
	}
	ref, _, err := query.TranslateToCascadesWithError(op, md)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	union := findTranslatedUnion(t, ref)
	assertUnionRow(t, union, []string{"V"}, []values.Type{values.NullableLong})

	second := union.GetQuantifiers()[1].GetRangesOver().Get().(*expressions.LogicalProjectionExpression)
	projected := second.GetProjectedValues()
	if len(projected) != 1 {
		t.Fatalf("literal leg projected values = %v, want one", projected)
	}
	pick, ok := projected[0].(*values.PickValue)
	if !ok || !pick.Type().Equals(values.NullableLong) || len(pick.Alternatives) != 2 {
		t.Fatalf("literal widening = %T %v, want exact nullable LONG PickValue", projected[0], projected[0])
	}
	if _, ok := pick.Alternatives[1].(*values.NullValue); !ok {
		t.Fatalf("literal widening nullable alternative = %T, want *values.NullValue", pick.Alternatives[1])
	}
	if _, _, err := planWithOptions(t,
		`SELECT v FROM a UNION ALL SELECT 99 FROM b`, unionExactOutputDDL, nil); err != nil {
		t.Fatalf("physical planning: %v", err)
	}
}

func TestUnionExactOutputContract_IncompatibleTypesStayLoud(t *testing.T) {
	t.Parallel()
	op, md, err := buildUnionLogical(t, `SELECT 1 FROM a UNION ALL SELECT 'x' FROM c`)
	if err != nil {
		t.Fatalf("VisitQuery rejected before the translator contract: %v", err)
	}
	_, _, err = query.TranslateToCascadesWithError(op, md)
	var relationalErr *api.Error
	if !errors.As(err, &relationalErr) || relationalErr.Code != api.ErrCodeUnionIncompatibleColumns {
		t.Fatalf("incompatible UNION error = %v, want %s", err, api.ErrCodeUnionIncompatibleColumns)
	}
}
