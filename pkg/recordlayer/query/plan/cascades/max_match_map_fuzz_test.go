package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func FuzzComputeMaxMatchMap_NoPanic(f *testing.F) {
	f.Add(uint8(0), uint8(0), uint8(0))
	f.Add(uint8(1), uint8(2), uint8(1))
	f.Add(uint8(3), uint8(3), uint8(2))
	f.Fuzz(func(t *testing.T, nQueryFields, nCandFields, aliasCount uint8) {
		nq := int(nQueryFields % 5)
		nc := int(nCandFields % 5)
		na := int(aliasCount % 3)

		query := makeRandomValue(t, nq, "q")
		cand := makeRandomValue(t, nc, "c")
		aliases := make(map[values.CorrelationIdentifier]struct{})
		for i := 0; i < na; i++ {
			aliases[values.NamedCorrelationIdentifier("a"+string(rune('0'+i)))] = struct{}{}
		}

		mmm := ComputeMaxMatchMap(query, cand, aliases)
		_ = mmm.GetQueryValue()
		_ = mmm.GetCandidateValue()
		_ = mmm.TranslateQueryValueMaybe(values.NamedCorrelationIdentifier("out"))
	})
}

func makeRandomValue(t testing.TB, nFields int, prefix string) values.Value {
	t.Helper()
	if nFields == 0 {
		qov, err := values.NewQuantifiedObjectValue(
			values.NamedCorrelationIdentifier(prefix),
			values.NotNullLong,
		)
		return mustConstruct(t, qov, err)
	}
	rowFields := make([]values.Field, nFields)
	for i := range rowFields {
		rowFields[i] = values.Field{
			Name:      prefix + "_f" + string(rune('a'+i)),
			FieldType: values.NullableLong,
			Ordinal:   i,
		}
	}
	rowType := values.NewRecordType(prefix+"Row", false, rowFields)
	root, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(prefix), rowType)
	root = mustConstruct(t, root, err)
	fields := make([]values.RecordConstructorField, nFields)
	for i := 0; i < nFields; i++ {
		field, resolveErr := values.ResolveFieldOrdinals(root, []int{i})
		fields[i] = values.RecordConstructorField{
			Name:  prefix + "_f" + string(rune('a'+i)),
			Value: mustConstruct(t, field, resolveErr),
		}
	}
	return &values.RecordConstructorValue{Fields: fields}
}

func TestExpandRecordValue_WithRecordType(t *testing.T) {
	t.Parallel()
	rt := values.NewRecordType("TestRec", true, []values.Field{
		{Name: "A", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "B", FieldType: values.NullableString, Ordinal: 1},
	})
	qov, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("q"), rt)
	qov = mustConstruct(t, qov, err)

	expanded := expandRecordValue(qov)
	if expanded == nil {
		t.Fatal("should expand QOV with RecordType")
	}
	rcv, ok := expanded.(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("expected *RecordConstructorValue, got %T", expanded)
	}
	if len(rcv.Fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(rcv.Fields))
	}
	if rcv.Fields[0].Name != "A" || rcv.Fields[1].Name != "B" {
		t.Fatalf("field names: %q, %q — expected A, B", rcv.Fields[0].Name, rcv.Fields[1].Name)
	}
}

func TestExpandRecordValue_NonRecordType(t *testing.T) {
	t.Parallel()
	qov, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("q"),
		values.NullableLong,
	)
	qov = mustConstruct(t, qov, err)
	expanded := expandRecordValue(qov)
	if expanded != nil {
		t.Fatal("should not expand QOV with non-Record type")
	}
}

func TestExpandRecordValue_AlreadyRCV(t *testing.T) {
	t.Parallel()
	rcv := &values.RecordConstructorValue{Fields: []values.RecordConstructorField{{
		Name: "x",
		Value: &values.ConstantValue{
			Value: int64(1),
			Typ:   values.NotNullLong,
		},
	}}}
	results := expandValueForMatching(rcv)
	if len(results) != 0 {
		t.Fatal("should not expand an already-RCV value")
	}
}

func TestMaxMatchMap_ExpansionHelpsMatching(t *testing.T) {
	t.Parallel()
	// Query: qov(q) with RecordType{A, B}
	// Candidate: rcv(fv("A"), fv("B"))
	// Without expansion: qov(q) doesn't structurally match rcv(...)
	// With expansion: qov(q) → rcv(fv("A"), fv("B")) which matches field-by-field

	rt := values.NewRecordType("", true, []values.Field{
		{Name: "A", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "B", FieldType: values.NullableString, Ordinal: 1},
	})
	query, queryErr := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("q"), rt)
	query = mustConstruct(t, query, queryErr)
	fieldA, fieldAErr := values.ResolveFieldOrdinals(query, []int{0})
	fieldA = mustConstruct(t, fieldA, fieldAErr)
	fieldB, fieldBErr := values.ResolveFieldOrdinals(query, []int{1})
	fieldB = mustConstruct(t, fieldB, fieldBErr)
	candidate := &values.RecordConstructorValue{Fields: []values.RecordConstructorField{
		{Name: "A", Value: fieldA},
		{Name: "B", Value: fieldB},
	}}

	mmm := ComputeMaxMatchMap(query, candidate, nil)
	if len(mmm.mapping) == 0 {
		t.Fatal("expansion should have enabled field-level matching")
	}
	// Verify individual field matches exist.
	for _, entry := range mmm.mapping {
		fv, ok := values.AsFieldValue(entry.queryValue)
		if !ok {
			continue
		}
		if fv.DisplayName() != "A" && fv.DisplayName() != "B" {
			t.Fatalf("unexpected matched field: %s", fv.DisplayName())
		}
	}
}
