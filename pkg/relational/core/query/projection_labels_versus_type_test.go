package query

import (
	"errors"
	"slices"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
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
			cte.CTEProducer = logical.NewCTE(cte.Name(), cte.Body(), nil, cte.Recursive(), logical.CTEColumns(tc.aliases...), logical.CTETraversal(cte.TraversalOrder())).CTEProducer
			labels, err := ExactLogicalOutputLabels(cte, nil, nil)
			if err != nil || !slices.Equal(labels, tc.want) {
				t.Fatalf("CTE labels = %v, %v; want %v independently of pending promotion", labels, err, tc.want)
			}
			if typ, err := ExactLogicalResultType(cte, nil); err == nil || typ != nil {
				t.Fatalf("label derivation certified an unresolved UNION type: %v, %v", typ, err)
			}
			outer := logical.NewCTE("C", branch(values.NotNullLong, int64(1)), cte, false)
			outer.CTEProducer = logical.NewCTE(outer.Name(), outer.Body(), nil, outer.Recursive(), logical.CTEColumns([]string{"OUTER_LEFT", "OUTER_RIGHT"}...), logical.CTETraversal(outer.TraversalOrder())).CTEProducer
			labels, err = ExactLogicalOutputLabels(outer, nil, nil)
			if err != nil || !slices.Equal(labels, tc.want) {
				t.Fatalf("inner pending binding did not shadow outer labels: %v, %v; want %v", labels, err, tc.want)
			}
			// The inner Body still sees the outer binding, not its own aliases.
			inner := logical.NewCTE("C", logical.NewScan("C", "B"), logical.NewScan("C", "M"), false)
			inner.CTEProducer = logical.NewCTE(inner.Name(), inner.Body(), nil, inner.Recursive(), logical.CTEColumns([]string{"INNER_LEFT", "INNER_RIGHT"}...), logical.CTETraversal(inner.TraversalOrder())).CTEProducer
			outer.Main = inner
			labels, err = ExactLogicalOutputLabels(outer, nil, nil)
			if err != nil || !slices.Equal(labels, inner.ColumnAliases()) {
				t.Fatalf("inner body scope = %v, %v; want %v", labels, err, inner.ColumnAliases())
			}
		})
	}
	for _, aliases := range [][]string{{"A"}, {"A", "B", "C"}} {
		cte := logical.NewCTE("C", body, logical.NewScan("C", ""), false)
		cte.CTEProducer = logical.NewCTE(cte.Name(), cte.Body(), nil, cte.Recursive(), logical.CTEColumns(aliases...), logical.CTETraversal(cte.TraversalOrder())).CTEProducer
		if labels, err := ExactLogicalOutputLabels(cte, nil, nil); err == nil || labels != nil {
			t.Fatalf("mismatched aliases %v accepted for a pending two-column body: %v, %v", aliases, labels, err)
		}
	}
}

func TestExactLogicalTypeDoesNotReuseShadowedCTEAfterBodyFailure(t *testing.T) {
	t.Parallel()
	projection := func(name string, value any, typ values.Type) logical.LogicalOperator {
		return &logical.LogicalProject{
			Projections: []string{name}, Aliases: []string{name},
			ProjectedValues: []values.Value{&values.ConstantValue{Value: value, Typ: typ}},
		}
	}
	for _, tc := range []struct {
		name string
		body logical.LogicalOperator
	}{
		{"pending_promotion", logical.NewUnion([]logical.LogicalOperator{
			projection("INNER", int64(7), values.NotNullLong),
			projection("OTHER", float64(9.5), values.NotNullDouble),
		}, false)},
		{"incompatible_union", logical.NewUnion([]logical.LogicalOperator{
			projection("INNER", int64(7), values.NotNullLong),
			projection("OTHER", "nine", values.NotNullString),
		}, false)},
		{"unresolved_projection", &logical.LogicalProject{Projections: []string{"INNER"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inner := logical.NewCTE("C", tc.body, logical.NewScan("C", "R"), false)
			outer := logical.NewCTE("C", projection("OUTER", "old", values.NotNullString), inner, false)
			if typ, err := ExactLogicalResultType(outer, nil); err == nil || typ != nil {
				t.Fatalf("failed local CTE body reused an outer binding: type=%v err=%v", typ, err)
			}
		})
	}
}

func TestLogicalUnionPromotionPreservesSlotsAndShadowing(t *testing.T) {
	t.Parallel()
	branch := func(firstType values.Type, first any, names []string) logical.LogicalOperator {
		return &logical.LogicalProject{
			Input: scan("Order", "O"), Projections: names, Aliases: names,
			ProjectedValues: []values.Value{
				&values.ConstantValue{Value: first, Typ: firstType},
				&values.ConstantValue{Value: int64(2), Typ: values.NotNullLong},
			},
		}
	}
	for _, shadowed := range []bool{false, true} {
		name := "union"
		if shadowed {
			name = "shadowed_cte"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			left := branch(values.NullableLong, nil, []string{"X", "X"})
			right := branch(values.NotNullDouble, float64(9.5), []string{"OTHER", "LAST"})
			union := logical.NewUnion([]logical.LogicalOperator{left, right}, false)
			var op logical.LogicalOperator = union
			if shadowed {
				inner := logical.NewCTE("C", union, logical.NewScan("C", "R"), false)
				op = logical.NewCTE("C", &logical.LogicalProject{
					Input: scan("Order", "O"), Projections: []string{"OLD"},
					ProjectedValues: []values.Value{&values.ConstantValue{Value: "old", Typ: values.NotNullString}},
				}, inner, false)
			}
			tr := newGateTranslator(t)
			if typ, err := ExactLogicalResultType(op, tr.md); err == nil || typ != nil {
				t.Fatalf("strict typing accepted pending promotion: %v, %v", typ, err)
			}
			typ, err := LogicalResultTypeAfterUnionPromotionWithCTEs(op, tr.md, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := &values.RecordType{Fields: []values.Field{
				{Name: "X", Ordinal: 0, FieldType: values.NullableDouble},
				{Name: "X_2", Ordinal: 1, FieldType: values.NotNullLong},
			}}
			if !typ.Equals(want) {
				t.Fatalf("prospective row = %v, want %v", typ, want)
			}
			labels, err := ExactLogicalOutputLabels(op, tr.md, nil)
			if err != nil || !slices.Equal(labels, []string{"X", "X"}) {
				t.Fatalf("SQL labels = %v, %v; want [X X]", labels, err)
			}
			if union.Inputs[0] != left || union.Inputs[1] != right {
				t.Fatal("type inference replaced a retained branch")
			}
			if typ, err := ExactLogicalResultType(op, tr.md); err == nil || typ != nil {
				t.Fatalf("prospective inference rewrote the pending union: %v, %v", typ, err)
			}
			ref := tr.translateRef(op)
			if ref == nil {
				t.Fatalf("translation declined: %v", tr.translateErr)
			}
			if actual := ref.Get().GetResultValue().Type(); !actual.Equals(want) {
				t.Fatalf("translated row = %v, want independently specified %v", actual, want)
			}
		})
	}
}

func TestLogicalUnionPromotionPreservesTypedErrors(t *testing.T) {
	t.Parallel()
	projection := func(typs ...values.Type) logical.LogicalOperator {
		p := &logical.LogicalProject{Input: scan("Order", "O")}
		for _, typ := range typs {
			p.Projections = append(p.Projections, "X")
			p.ProjectedValues = append(p.ProjectedValues, exactTestNamedField(t, "O", "X", typ))
		}
		return p
	}
	for _, tc := range []struct {
		name  string
		right logical.LogicalOperator
		code  api.ErrorCode
	}{
		{"width", projection(values.NotNullLong, values.NotNullLong), api.ErrCodeUnionIncorrectColumnCount},
		{"incompatible", projection(values.NotNullString), api.ErrCodeUnionIncompatibleColumns},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := logical.NewUnion([]logical.LogicalOperator{projection(values.NotNullLong), tc.right}, false)
			inner := logical.NewCTE("C", body, logical.NewScan("C", "R"), false)
			outer := logical.NewCTE("C", projection(values.NotNullLong), inner, false)
			typ, err := LogicalResultTypeAfterUnionPromotionWithCTEs(outer, nil, nil)
			var apiErr *api.Error
			if typ != nil || !errors.As(err, &apiErr) || apiErr.Code != tc.code {
				t.Fatalf("failed local CTE: type=%v err=%v; want typed %s, not an outer binding", typ, err, tc.code)
			}
		})
	}
}
