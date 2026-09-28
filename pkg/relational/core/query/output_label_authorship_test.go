package query

import (
	"slices"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// Authorship rides the label walk with the names: a projection slot is
// authored only by its recorded SQLNameAuthored bit — never by its Aliases
// entry, which the builder also fills with an un-aliased reference's
// inherited name — a CTE column list and an unnest's AS/AT aliases are
// authored, and a scan's descriptor names are not. The flag survives the
// wrappers and a CTE reference.
func TestOutputLabelAuthorshipFollowsTheLabelWalk(t *testing.T) {
	t.Parallel()
	value, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("A"), values.NotNullLong)
	if err != nil {
		t.Fatal(err)
	}
	proj := &logical.LogicalProject{
		Input:           &logical.LogicalScan{Table: "T"},
		Projections:     []string{"ID", "KEEP", "N"},
		Aliases:         []string{"k", "keep", "N"},
		ProjectedValues: []values.Value{value, value, value},
		SQLNameAuthored: []bool{true, false},
	}
	labels, err := ExactLogicalOutputLabelsAuthored(proj, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []OutputLabel{{Name: "k", Authored: true}, {Name: "keep"}, {Name: "N"}}
	if !slices.Equal(labels, want) {
		t.Fatalf("projection labels = %v, want %v (a short SQLNameAuthored reads as not authored)", labels, want)
	}
	if names := OutputLabelNames(labels); !slices.Equal(names, []string{"k", "keep", "N"}) {
		t.Fatalf("names = %v", names)
	}
	if wrapped, err := ExactLogicalOutputLabelsAuthored(&logical.LogicalFilter{Input: proj}, nil, nil); err != nil || !slices.Equal(wrapped, want) {
		t.Fatalf("a filter changed the provenance: %v, %v", wrapped, err)
	}

	cte := logical.NewCTE("C", proj, logical.NewScan("C", "C"), false)
	cte.CTEProducer = logical.NewCTE(cte.Name(), cte.Body(), nil, cte.Recursive(),
		logical.CTEColumns("a", "b", "c"), logical.CTETraversal(cte.TraversalOrder())).CTEProducer
	listed, err := ExactLogicalOutputLabelsAuthored(cte, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(listed, []OutputLabel{{Name: "a", Authored: true}, {Name: "b", Authored: true}, {Name: "c", Authored: true}}) {
		t.Fatalf("a CTE column list is the statement's own: %v", listed)
	}

	owner := &values.RecordType{Fields: []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "ARR", Ordinal: 1, FieldType: values.NewArrayType(true, values.NotNullLong)},
	}}
	root, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("SRC"), owner)
	if err != nil {
		t.Fatal(err)
	}
	collection, err := values.ResolveFieldOrdinals(root, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	unnest := &logical.LogicalUnnest{Alias: "e", AtAlias: "o", CorrelatedCollection: collection}
	legs, err := legLabelsForType(unnest, nil)
	if err != nil || !slices.Equal(legs, []OutputLabel{{Name: "e", Authored: true}, {Name: "o", Authored: true}}) {
		t.Fatalf("unnest AS/AT labels = %v, %v; want both authored", legs, err)
	}
	record := &values.RecordType{Fields: []values.Field{{Name: "id", FieldType: values.NotNullLong}}}
	rows, err := rowLabels(record, nil)
	if err != nil || !slices.Equal(rows, []OutputLabel{{Name: "id"}}) {
		t.Fatalf("a row's own field names are descriptor names: %v, %v", rows, err)
	}
}
