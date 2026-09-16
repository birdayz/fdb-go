package query

import (
	"slices"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// A projection that repeats an output name has TWO rows: the SQL names a
// derived source publishes for resolution, where the repeated name stays
// repeated so a reference that spells it is ambiguous, and the row the plan
// flows, where the record constructor names the repeat by the
// name-addressability suffix. One per-slot naming rule feeds both derivations
// and only the exact type deduplicates. When the dedup reached the labels,
// `C."X"` over `WITH C AS (SELECT MIN(…) AS "X", MAX(…) AS "X" …)` resolved to
// one column instead of reporting 42702.
func TestProjectionLabelsStayRepeatedWhereTheExactTypeDeduplicates(t *testing.T) {
	t.Parallel()
	// Two exact typed values: a quantified object over a primitive flowed type
	// IS an exact LONG, where a bare literal only carries a placeholder type.
	first, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("A"), values.NotNullLong)
	if err != nil {
		t.Fatal(err)
	}
	second, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("B"), values.NotNullLong)
	if err != nil {
		t.Fatal(err)
	}
	proj := &logical.LogicalProject{
		Input:           &logical.LogicalScan{Table: "T"},
		Projections:     []string{"1", "2"},
		Aliases:         []string{"X", "X"},
		ProjectedValues: []values.Value{first, second},
	}
	labels, err := ExactLogicalOutputLabels(proj, nil, nil)
	if err != nil {
		t.Fatalf("ExactLogicalOutputLabels: %v", err)
	}
	if want := []string{"X", "X"}; !slices.Equal(labels, want) {
		t.Fatalf("labels = %v, want %v — the SQL names must stay repeated for resolution", labels, want)
	}
	typ, err := ExactLogicalResultType(proj, nil)
	if err != nil {
		t.Fatalf("ExactLogicalResultType: %v", err)
	}
	record, ok := typ.(*values.RecordType)
	if !ok {
		t.Fatalf("exact type = %T, want a record", typ)
	}
	names := make([]string, len(record.Fields))
	for i, f := range record.Fields {
		names[i] = f.Name
	}
	if want := []string{"X", "X_2"}; !slices.Equal(names, want) {
		t.Fatalf("exact row = %v, want %v — the row the plan flows names the repeat by the suffix", names, want)
	}
}

func TestProjectionMintedAliasDoesNotBecomeASQLLabel(t *testing.T) {
	t.Parallel()
	proj := &logical.LogicalProject{
		Projections:    []string{"X.K"},
		Aliases:        []string{"X.K"},
		AliasMinted:    []bool{true},
		ProjectionRefs: []logical.ColumnRef{{Present: true, Bare: "K", Qualifier: "X", Qualified: true}},
		ProjectedValues: []values.Value{
			&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong},
		},
	}
	labels, err := ExactLogicalOutputLabels(proj, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"K"}; !slices.Equal(labels, want) {
		t.Fatalf("labels = %v, want %v — a machinery alias became a published SQL name", labels, want)
	}
	typ, err := ExactLogicalResultType(proj, nil)
	if err != nil {
		t.Fatal(err)
	}
	record := typ.(*values.RecordType)
	if got := record.Fields[0].Name; got != "X.K" {
		t.Fatalf("physical slot = %q, want X.K", got)
	}
}

func TestProjectionSemanticNamesDoNotPublishPhysicalKeys(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, physical string
		sqlNames       []string
		computed       bool
		badWidth       bool
	}{
		{"unnamed_aggregate", "_0", []string{""}, false, false},
		{"authored_positional_name", "_0", []string{"_0"}, false, false},
		{"computed_without_alias", "CAST(1 AS BIGINT)", nil, true, false},
		{"empty_vector", "_0", []string{}, false, true},
		{"long_vector", "_0", []string{"", "X"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Use the actual producer kinds: a whole scalar QOV has an
			// inherited source name and is not an anonymous computation.
			var value values.Value = &values.AggregateValue{Op: values.AggCountStar}
			key := "COUNT(*)"
			if tc.computed {
				value = values.NewCastValue(&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}, values.NotNullLong)
				key = "CAST(ID AS BIGINT)"
			}
			proj := &logical.LogicalProject{
				Projections: []string{key}, ProjectedValues: []values.Value{value},
				SQLNames: tc.sqlNames, IsComputed: []bool{tc.computed},
				// The frontend materializes anonymous slots under ordinal
				// aliases; the semantic name vector keeps that alias private.
				Aliases: []string{tc.physical}, AliasMinted: []bool{tc.name != "authored_positional_name"},
			}
			if tc.computed {
				// The raw-IR no-alias path uses the Value's physical slot
				// name, while IsComputed prevents publishing it for SQL lookup.
				proj.Aliases, proj.AliasMinted = nil, nil
			}
			labels, err := ExactLogicalOutputLabels(proj, nil, nil)
			if tc.badWidth {
				if err == nil {
					t.Fatal("accepted incomplete semantic-name vector")
				}
				return
			}
			want := []string{""}
			if tc.sqlNames != nil {
				want = tc.sqlNames
			}
			if err != nil || !slices.Equal(labels, want) {
				t.Fatalf("semantic names = %q / %v, want %q", labels, err, want)
			}
			typ, err := ExactLogicalResultType(proj, nil)
			if err != nil {
				t.Fatal(err)
			}
			record, ok := typ.(*values.RecordType)
			if !ok || len(record.Fields) != 1 || record.Fields[0].Name != tc.physical {
				t.Fatalf("physical row = %v, want one field %q", typ, tc.physical)
			}
			labels[0] = "mutated"
			if tc.sqlNames != nil && proj.SQLNames[0] == "mutated" {
				t.Fatal("returned semantic labels alias the logical node")
			}
		})
	}
}

func TestCTELabelsDoNotRequireUnionTypePromotion(t *testing.T) {
	t.Parallel()
	branch := func(typ values.Type, value any) logical.LogicalOperator {
		return &logical.LogicalProject{
			Projections: []string{"first", "second"},
			Aliases:     []string{"X", "X"},
			ProjectedValues: []values.Value{
				&values.ConstantValue{Value: value, Typ: typ},
				&values.ConstantValue{Value: value, Typ: typ},
			},
		}
	}
	body := &logical.LogicalUnion{Inputs: []logical.LogicalOperator{
		branch(values.NotNullLong, int64(1)), branch(values.NotNullDouble, float64(2.5)),
	}}
	for _, tc := range []struct {
		name    string
		aliases []string
		want    []string
	}{
		{"repeated body labels", nil, []string{"X", "X", "X", "X"}},
		{"explicit aliases", []string{"A", "B"}, []string{"A", "B", "A", "B"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cte := logical.NewCTE("C", body, logical.NewJoin(
				logical.NewScan("C", "L"), logical.NewScan("C", "R"), logical.JoinInner, ""), false)
			cte.ColumnAliases = tc.aliases
			labels, err := ExactLogicalOutputLabels(cte, nil, nil)
			if err != nil || !slices.Equal(labels, tc.want) {
				t.Fatalf("CTE labels = %v, %v; want %v independently of pending promotion", labels, err, tc.want)
			}
			if typ, err := ExactLogicalResultType(cte, nil); err == nil || typ != nil {
				t.Fatalf("label derivation certified an unresolved UNION type: %v, %v", typ, err)
			}
			outer := logical.NewCTE("C", branch(values.NotNullLong, int64(1)), cte, false)
			outer.ColumnAliases = []string{"OUTER_LEFT", "OUTER_RIGHT"}
			labels, err = ExactLogicalOutputLabels(outer, nil, nil)
			if err != nil || !slices.Equal(labels, tc.want) {
				t.Fatalf("inner pending binding did not shadow outer labels: %v, %v; want %v", labels, err, tc.want)
			}
			// The inner Body still sees the outer binding, not its own aliases.
			inner := logical.NewCTE("C", logical.NewScan("C", "B"), logical.NewScan("C", "M"), false)
			inner.ColumnAliases = []string{"INNER_LEFT", "INNER_RIGHT"}
			outer.Main = inner
			labels, err = ExactLogicalOutputLabels(outer, nil, nil)
			if err != nil || !slices.Equal(labels, inner.ColumnAliases) {
				t.Fatalf("inner body scope = %v, %v; want %v", labels, err, inner.ColumnAliases)
			}
		})
	}
	for _, aliases := range [][]string{{"A"}, {"A", "B", "C"}} {
		cte := logical.NewCTE("C", body, logical.NewScan("C", ""), false)
		cte.ColumnAliases = aliases
		if labels, err := ExactLogicalOutputLabels(cte, nil, nil); err == nil || labels != nil {
			t.Fatalf("mismatched aliases %v accepted for a pending two-column body: %v, %v", aliases, labels, err)
		}
	}
}
