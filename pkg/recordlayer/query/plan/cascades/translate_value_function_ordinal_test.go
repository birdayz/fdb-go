package cascades

// Pins the fetch-push-down ordinal gate in
// ValueIndexScanMatchCandidate.buildTranslateValueFunction. A covered
// top-level field keeps its exact ordinal and is rebased to the index-entry
// object. Nested paths, foreign layouts, and foreign correlations decline;
// none may be reinterpreted from display text.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func mustFetchOrdinalConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct fetch-ordinal fixture: " + err.Error())
	}
	return value
}

func fetchOrdinalRowType() *values.RecordType {
	address := values.NewRecordType("fetch_address", false, []values.Field{
		{Name: "CITY", FieldType: values.NullableString},
	})
	return values.NewRecordType("fetch_row", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "ADDR", FieldType: address},
		{Name: "CITY", FieldType: values.NullableString},
	})
}

func fetchOrdinalCandidate(rowType values.Type) *ValueIndexScanMatchCandidate {
	return newKnownDistinctValueIndexCandidate(
		"IDX_CITY",
		[]string{"T"},
		[]string{"CITY"},
		[]values.CorrelationIdentifier{values.NamedCorrelationIdentifier("p0")},
		rowType,
		false,
		[]string{"ID"},
	)
}

func fetchOrdinalRoot(alias values.CorrelationIdentifier, rowType values.Type) values.QuantifiedObjectValue {
	return mustFetchOrdinalConstruct(values.NewQuantifiedObjectValue(alias, rowType))
}

func fetchOrdinalField(root values.Value, ordinals ...int) values.Value {
	return mustFetchOrdinalConstruct(values.ResolveFieldOrdinals(root, ordinals))
}

func assertFetchOrdinalField(
	t testing.TB,
	value values.Value,
	wantAlias values.CorrelationIdentifier,
	wantOrdinals ...int,
) {
	t.Helper()
	field, ok := values.AsFieldValue(value)
	if !ok {
		t.Fatalf("translated value = %T, want exact FieldValue", value)
	}
	root, ok := values.AsQuantifiedObjectValue(field.ChildValue())
	if !ok || root.Correlation() != wantAlias {
		t.Fatalf("translated root = %T/%v, want QOV(%s)",
			field.ChildValue(), root, wantAlias.Name())
	}
	gotOrdinals := field.Path().Ordinals()
	if len(gotOrdinals) != len(wantOrdinals) {
		t.Fatalf("translated ordinal path = %v, want %v", gotOrdinals, wantOrdinals)
	}
	for i := range gotOrdinals {
		if gotOrdinals[i] != wantOrdinals[i] {
			t.Fatalf("translated ordinal path = %v, want %v", gotOrdinals, wantOrdinals)
		}
	}
}

func TestFetchTranslate_PreservesSingleAccessorOrdinal_DeclinesFused(t *testing.T) {
	t.Parallel()

	rowType := fetchOrdinalRowType()
	candidate := fetchOrdinalCandidate(rowType)
	source := values.NamedCorrelationIdentifier("src")
	target := values.NamedCorrelationIdentifier("tgt")
	sourceRoot := fetchOrdinalRoot(source, rowType)

	// A normal source-relative top-level field keeps the descriptor ordinal and
	// is rebound to the index-entry target.
	city := fetchOrdinalField(sourceRoot, 2)
	translated, ok := candidate.PushValueThroughFetch(city, source, target)
	if !ok {
		t.Fatal("bare covered column must translate")
	}
	assertFetchOrdinalField(t, translated, target, 2)

	// The machinery-pinned form states the same exact row/slot and translates
	// identically; the pin is not used as a name fallback.
	pinned := mustFetchOrdinalConstruct(values.ResolveOrdinalSeedField(sourceRoot, 2))
	translatedPinned, ok := candidate.PushValueThroughFetch(pinned, source, target)
	if !ok {
		t.Fatal("frontier-pinned covered column must translate")
	}
	assertFetchOrdinalField(t, translatedPinned, target, 2)

	// ADDR.CITY has leaf display name CITY but is a two-step read. Reducing it
	// to top-level CITY would silently drop the descent.
	fused := fetchOrdinalField(sourceRoot, 1, 0)
	if out, ok := candidate.PushValueThroughFetch(fused, source, target); ok {
		t.Fatalf("fused ADDR.CITY must not push below fetch; got %v", out)
	}

	// Even when the fused path's root itself is the covered column, the suffix
	// cannot disappear. Use a second exact row whose CITY is a nested record.
	nestedCity := values.NewRecordType("nested_city", false, []values.Field{
		{Name: "SUB", FieldType: values.NullableString},
	})
	nestedRow := values.NewRecordType("nested_fetch_row", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "ADDR", FieldType: values.NullableString},
		{Name: "CITY", FieldType: nestedCity},
	})
	nestedRoot := fetchOrdinalRoot(source, nestedRow)
	nested := fetchOrdinalField(nestedRoot, 2, 0)
	if out, ok := fetchOrdinalCandidate(nestedRow).PushValueThroughFetch(nested, source, target); ok {
		t.Fatalf("fused CITY.SUB must not push below fetch; got %v", out)
	}

	// The same integer in another exact row layout is not the same column.
	foreignRow := values.NewRecordType("foreign_fetch_row", false, []values.Field{
		{Name: "CITY", FieldType: values.NullableString},
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "ADDR", FieldType: values.NullableString},
	})
	foreign := fetchOrdinalField(fetchOrdinalRoot(source, foreignRow), 0)
	if out, ok := candidate.PushValueThroughFetch(foreign, source, target); ok {
		t.Fatalf("foreign-layout CITY must not push below fetch; got %v", out)
	}

	// RFC-232 makes the old lazy/name-only carrier unpublishable. This is an
	// admission boundary, not a rule-side decline that can be revived later.
	if root, err := values.NewQuantifiedObjectValue(source, values.UnknownType); err == nil || root != nil {
		t.Fatalf("unresolved QOV admitted as (%v, %v), want exact-type rejection", root, err)
	}
}

// The correlation element of identity is decisive for a self-join: the row
// layout and ordinal are identical, but only the fetch's own quantifier may be
// rebound onto its index entry.
func TestFetchTranslate_SameTableTwoQuantifiers_CorrelationDecides(t *testing.T) {
	t.Parallel()

	rowTypeQ1 := fetchOrdinalRowType()
	rowTypeQ2 := fetchOrdinalRowType()
	if !rowTypeQ1.Equals(rowTypeQ2) {
		t.Fatal("self-join row layouts must be structurally identical")
	}
	candidate := fetchOrdinalCandidate(rowTypeQ1)
	q1 := values.NamedCorrelationIdentifier("q1")
	q2 := values.NamedCorrelationIdentifier("q2")
	target := values.NamedCorrelationIdentifier("tgt")

	own := fetchOrdinalField(fetchOrdinalRoot(q1, rowTypeQ1), 2)
	translated, ok := candidate.PushValueThroughFetch(own, q1, target)
	if !ok {
		t.Fatal("source quantifier's covered CITY must push")
	}
	assertFetchOrdinalField(t, translated, target, 2)

	foreign := fetchOrdinalField(fetchOrdinalRoot(q2, rowTypeQ2), 2)
	if out, ok := candidate.PushValueThroughFetch(foreign, q1, target); ok {
		t.Fatalf("other quantifier's same-layout CITY pushed onto q1 fetch: %v", out)
	}
	if translatedQ2, ok := candidate.PushValueThroughFetch(foreign, q2, target); !ok {
		t.Fatal("q2 CITY must push through q2's own fetch")
	} else {
		assertFetchOrdinalField(t, translatedQ2, target, 2)
	}

	// A childless baked field used to bypass the correlation decision. Exact
	// FieldValue admission now requires an exact QOV owner, so no public
	// constructor can fabricate that ambiguous state.
	if _, err := values.ResolveFieldOrdinals(nil, []int{2}); err == nil {
		t.Fatal("childless ordinal field unexpectedly admitted")
	}
}
