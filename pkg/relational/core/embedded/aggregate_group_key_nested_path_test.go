package embedded

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func nestedGroupKey(t testing.TB, correlation, leaf string, ordinal int) values.Value {
	t.Helper()
	nestedType := &values.RecordType{Fields: []values.Field{
		{Name: "SK", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "CO", Ordinal: 1, FieldType: values.NotNullLong},
		{Name: "OTHER", Ordinal: 2, FieldType: values.NotNullLong},
	}}
	rootType := &values.RecordType{Fields: []values.Field{{Name: "N", Ordinal: 0, FieldType: nestedType}}}
	qov, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(correlation), rootType)
	if err != nil {
		t.Fatalf("NewQuantifiedObjectValue: %v", err)
	}
	value, err := values.ResolveFieldOrdinals(qov, []int{0, ordinal})
	if err != nil {
		t.Fatalf("resolve N.%s: %v", leaf, err)
	}
	return value
}

func exactFlatGroupKey(t testing.TB, correlation, name string) values.Value {
	t.Helper()
	typ := &values.RecordType{Fields: []values.Field{{Name: name, Ordinal: 0, FieldType: values.NotNullString}}}
	qov, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(correlation), typ)
	if err != nil {
		t.Fatalf("NewQuantifiedObjectValue: %v", err)
	}
	value, err := values.ResolveFieldOrdinals(qov, []int{0})
	if err != nil {
		t.Fatalf("resolve %s: %v", name, err)
	}
	return value
}

func TestAggregateGroupKeyMirrorsTakeTheExactNestedPath(t *testing.T) {
	t.Parallel()

	sk := nestedGroupKey(t, "T1", "SK", 0)
	co := nestedGroupKey(t, "T1", "CO", 1)

	t.Run("aggregateGroupKeyOutputName", func(t *testing.T) {
		t.Parallel()
		gotSK, gotCO := aggregateGroupKeyOutputName(sk), aggregateGroupKeyOutputName(co)
		if gotSK != "T1.N.SK" || gotCO != "T1.N.CO" {
			t.Fatalf("aggregateGroupKeyOutputName = %q / %q, want T1.N.SK / T1.N.CO", gotSK, gotCO)
		}
		if gotSK != expressions.AggregateKeyColumnName(sk) || gotCO != expressions.AggregateKeyColumnName(co) {
			t.Fatalf("group-key naming mirror drifted from authority: got %q/%q, authority %q/%q",
				gotSK, gotCO, expressions.AggregateKeyColumnName(sk), expressions.AggregateKeyColumnName(co))
		}
	})

	t.Run("flat exact key keeps its bare output name", func(t *testing.T) {
		t.Parallel()
		flat := exactFlatGroupKey(t, "T1", "STATUS")
		if got := aggregateGroupKeyOutputName(flat); got != "STATUS" {
			t.Fatalf("flat exact group key names its output %q, want STATUS", got)
		}
	})

	t.Run("buildAggColumns follows the same authority", func(t *testing.T) {
		t.Parallel()
		cols := buildAggColumns([]values.Value{sk, co}, nil, nil)
		if len(cols) != 2 {
			t.Fatalf("buildAggColumns returned %d columns for 2 group keys", len(cols))
		}
		if cols[0].Name != "T1.N.SK" || cols[1].Name != "T1.N.CO" {
			t.Fatalf("ColumnDef.Name = %q / %q, want T1.N.SK / T1.N.CO", cols[0].Name, cols[1].Name)
		}
		if cols[0].Label != "SK" || cols[1].Label != "CO" {
			t.Fatalf("ColumnDef.Label = %q / %q, want SK / CO", cols[0].Label, cols[1].Label)
		}
	})

	t.Run("buildAggColumns keeps a flat key bare and unlabelled", func(t *testing.T) {
		t.Parallel()
		flat := exactFlatGroupKey(t, "T1", "STATUS")
		cols := buildAggColumns([]values.Value{flat}, nil, nil)
		if len(cols) != 1 || cols[0].Name != "STATUS" || cols[0].Label != "" {
			t.Fatalf("flat exact group key ColumnDef = %+v, want bare Name=STATUS and no display label", cols)
		}
	})
}
