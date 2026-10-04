package query

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// TestTranslateUpdateNeedsAResolvedColumn: translateUpdate builds each
// transform from the SET column the catalog builder resolved, and refuses an
// assignment that was never resolved (a zero Assignment, whose ordinal would
// otherwise name the first column) or whose path starts at a column other
// than the one tableColumns has at that ordinal.
func TestTranslateUpdateNeedsAResolvedColumn(t *testing.T) {
	t.Parallel()
	md := demoMetaData(t)
	update := func(a logical.Assignment) logical.LogicalOperator {
		a.Value = values.LiteralValue(int64(7))
		return logical.NewUpdate("Order", []logical.Assignment{a}, logical.NewScan("Order", ""))
	}
	ref, _, err := TranslateToCascadesWithError(update(logical.Assignment{
		Column: "price", Segments: []string{"price"}, FieldOrdinals: []int{2}, FieldNames: []string{"price"},
	}), md)
	if err != nil || ref == nil {
		t.Fatalf("a resolved column: %v", err)
	}
	ref = queryBody(t, ref)
	upd, ok := ref.Members()[0].(*expressions.UpdateExpression)
	if !ok || len(upd.GetTransforms()) != 1 || upd.GetTransforms()[0].FieldPath() != "price" {
		t.Fatalf("a resolved column: %T %v", ref.Members()[0], ref.Members()[0])
	}
	for _, c := range []struct {
		name string
		a    logical.Assignment
	}{
		{"never resolved", logical.Assignment{Column: "price", Segments: []string{"price"}}},
		{"an ordinal outside", logical.Assignment{Column: "price", FieldOrdinals: []int{99}, FieldNames: []string{"price"}}},
		{"another column at the ordinal", logical.Assignment{Column: "price", FieldOrdinals: []int{0}, FieldNames: []string{"price"}}},
		{"names and ordinals apart", logical.Assignment{Column: "price", FieldOrdinals: []int{2, 0}, FieldNames: []string{"price"}}},
	} {
		_, _, err := TranslateToCascadesWithError(update(c.a), md)
		if err == nil || !strings.Contains(err.Error(), "no resolved target column") {
			t.Errorf("%s: %v, want \"no resolved target column\"", c.name, err)
		}
	}
}
