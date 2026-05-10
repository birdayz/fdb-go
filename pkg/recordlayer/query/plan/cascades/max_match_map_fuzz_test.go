package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func FuzzComputeMaxMatchMap_NoPanic(f *testing.F) {
	f.Add(uint8(0), uint8(0), uint8(0))
	f.Add(uint8(1), uint8(2), uint8(1))
	f.Add(uint8(3), uint8(3), uint8(2))
	f.Fuzz(func(t *testing.T, nQueryFields, nCandFields, aliasCount uint8) {
		nq := int(nQueryFields % 5)
		nc := int(nCandFields % 5)
		na := int(aliasCount % 3)

		query := makeRandomValue(nq, "q")
		cand := makeRandomValue(nc, "c")
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

func makeRandomValue(nFields int, prefix string) values.Value {
	if nFields == 0 {
		return values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(prefix))
	}
	fields := make([]values.RecordConstructorField, nFields)
	for i := 0; i < nFields; i++ {
		fields[i] = values.RecordConstructorField{
			Name:  prefix + "_f" + string(rune('a'+i)),
			Value: &values.FieldValue{Field: prefix + "_f" + string(rune('a'+i)), Typ: values.UnknownType},
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
	qov := &values.QuantifiedObjectValue{
		Correlation: values.NamedCorrelationIdentifier("q"),
		Typ:         rt,
	}

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
	qov := &values.QuantifiedObjectValue{
		Correlation: values.NamedCorrelationIdentifier("q"),
		Typ:         values.NullableLong,
	}
	expanded := expandRecordValue(qov)
	if expanded != nil {
		t.Fatal("should not expand QOV with non-Record type")
	}
}

func TestExpandRecordValue_AlreadyRCV(t *testing.T) {
	t.Parallel()
	rcv := &values.RecordConstructorValue{Fields: []values.RecordConstructorField{
		{Name: "x", Value: &values.FieldValue{Field: "x"}},
	}}
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
	query := &values.QuantifiedObjectValue{
		Correlation: values.NamedCorrelationIdentifier("q"),
		Typ:         rt,
	}
	candidate := &values.RecordConstructorValue{Fields: []values.RecordConstructorField{
		{Name: "A", Value: &values.FieldValue{Field: "A", Typ: values.NullableLong}},
		{Name: "B", Value: &values.FieldValue{Field: "B", Typ: values.NullableString}},
	}}

	mmm := ComputeMaxMatchMap(query, candidate, nil)
	if len(mmm.mapping) == 0 {
		t.Fatal("expansion should have enabled field-level matching")
	}
	// Verify individual field matches exist.
	for _, entry := range mmm.mapping {
		fv, ok := entry.queryValue.(*values.FieldValue)
		if !ok {
			continue
		}
		if fv.Field != "A" && fv.Field != "B" {
			t.Fatalf("unexpected matched field: %s", fv.Field)
		}
	}
}
