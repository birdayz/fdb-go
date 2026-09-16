package query

import (
	"reflect"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

func TestExactLogicalResultTypeBoundStructUnnest(t *testing.T) {
	t.Parallel()
	element := &values.RecordType{RecordName: "ITEM", Fields: []values.Field{{Name: "K", Ordinal: 0, FieldType: values.NotNullLong}}}
	array := values.NewArrayType(true, element)
	owner := &values.RecordType{Fields: []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "ITEMS", Ordinal: 1, FieldType: array},
	}}
	left, err := logical.NewInlineValues("SRC", values.NewArrayConstructorValue(owner, nil))
	if err != nil {
		t.Fatal(err)
	}
	root, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("SRC"), owner)
	if err != nil {
		t.Fatal(err)
	}
	collection, err := values.ResolveFieldOrdinals(root, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, as, at string
		labels       []string
	}{
		{"whole record", "X", "", []string{"X"}},
		{"record and ordinal", "X", "POS", []string{"X", "POS"}},
		{"ordinal only", "", "POS", []string{"POS"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u := &logical.LogicalUnnest{Alias: tc.as, AtAlias: tc.at, CorrelatedCollection: collection}
			joined := logical.NewJoin(left, u, logical.JoinInner, "")
			typ, err := ExactLogicalResultType(joined, nil)
			if err != nil {
				t.Fatal(err)
			}
			row, ok := typ.(*values.RecordType)
			if !ok || len(row.Fields) != 2+len(tc.labels) {
				t.Fatalf("joined type %v: expected two source slots plus %v", typ, tc.labels)
			}
			for i, name := range tc.labels {
				field := row.Fields[2+i]
				if field.Name != name || field.Ordinal != 2+i {
					t.Errorf("slot %d = %+v, want %s at physical ordinal %d", i, field, name, 2+i)
				}
				if name == "X" {
					record, ok := field.FieldType.(*values.RecordType)
					if !ok || record.RecordName != "ITEM" || len(record.Fields) != 1 || !record.Fields[0].FieldType.Equals(values.NotNullLong) {
						t.Errorf("element was flattened or lost its nominal type: %v", field.FieldType)
					}
				} else if !field.FieldType.Equals(values.NotNullInt) {
					t.Errorf("ordinal type %v, want INT NOT NULL", field.FieldType)
				}
			}
			labels, err := ExactLogicalOutputLabels(joined, nil, nil)
			want := append([]string{"ID", "ITEMS"}, tc.labels...)
			if err != nil || !reflect.DeepEqual(labels, want) {
				t.Errorf("labels = %v / %v, want %v", labels, err, want)
			}
		})
	}
}

func TestBoundBoxCollectionUsesOwnerWindow(t *testing.T) {
	t.Parallel()
	tr := newGateTranslator(t)
	outer := logical.NewJoin(scan("Order", "L"), scan("Order", "R"), logical.JoinFull, "")
	storedLayout, path := rawProtoPath(t, tr.md, "Order", "TAGS")
	layout := rawNullSuppliedLayout(storedLayout)
	for _, tc := range []struct {
		owner  string
		offset int
	}{{"L", 0}, {"R", len(layout.Fields)}} {
		u, _ := rawBoundUnnest(t, []string{"diagnostic", "not_the_path"}, "X", "", tc.owner, layout, path...)
		got := tr.unnestBakedRootCollection(outer, unnestOuterCorrelation(outer), u, -1)
		field, ok := values.AsFieldValue(got)
		want := []int{tc.offset + path[0]}
		if !ok || !reflect.DeepEqual(field.Path().Ordinals(), want) {
			t.Fatalf("owner %s: collection %v, want ordinal path %v", tc.owner, got, want)
		}
	}
	u, _ := rawBoundUnnest(t, []string{"L", "TAGS"}, "X", "", "FOREIGN", layout, path...)
	if got := tr.unnestBakedRootCollection(outer, unnestOuterCorrelation(outer), u, -1); got != nil {
		t.Fatalf("foreign owner borrowed a same-typed window: %v", got)
	}
}
