package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestEmptyTranslationMap_ContainsSourceAlias(t *testing.T) {
	t.Parallel()
	tm := EmptyTranslationMap()
	alias := values.NamedCorrelationIdentifier("q1")
	if tm.ContainsSourceAlias(alias) {
		t.Error("empty translation map should not contain any alias")
	}
}

func TestEmptyTranslationMap_DefinesOnlyIdentities(t *testing.T) {
	t.Parallel()
	tm := EmptyTranslationMap()
	if !tm.DefinesOnlyIdentities() {
		t.Error("empty translation map should define only identities")
	}
}

func TestEmptyTranslationMap_GetTargetAlias(t *testing.T) {
	t.Parallel()
	tm := EmptyTranslationMap()
	alias := values.NamedCorrelationIdentifier("q1")
	_, ok := tm.GetTargetAlias(alias)
	if ok {
		t.Error("empty translation map should not have a target alias")
	}
}

func TestEmptyTranslationMap_GetAliasMap(t *testing.T) {
	t.Parallel()
	tm := EmptyTranslationMap()
	_, ok := tm.GetAliasMap()
	if ok {
		t.Error("empty translation map should not have an alias map")
	}
}

func TestTranslationMapOfAliases_ContainsSource(t *testing.T) {
	t.Parallel()
	src := values.NamedCorrelationIdentifier("src")
	tgt := values.NamedCorrelationIdentifier("tgt")
	tm := TranslationMapOfAliases(src, tgt)

	if !tm.ContainsSourceAlias(src) {
		t.Error("should contain source alias")
	}
	if tm.ContainsSourceAlias(tgt) {
		t.Error("should not contain target alias as a source")
	}
	other := values.NamedCorrelationIdentifier("other")
	if tm.ContainsSourceAlias(other) {
		t.Error("should not contain unrelated alias")
	}
}

func TestTranslationMapOfAliases_GetTargetAlias(t *testing.T) {
	t.Parallel()
	src := values.NamedCorrelationIdentifier("src")
	tgt := values.NamedCorrelationIdentifier("tgt")
	tm := TranslationMapOfAliases(src, tgt)

	got, ok := tm.GetTargetAlias(src)
	if !ok {
		t.Fatal("should find target for source alias")
	}
	if got != tgt {
		t.Errorf("target alias = %v, want %v", got, tgt)
	}

	_, ok = tm.GetTargetAlias(tgt)
	if ok {
		t.Error("should not find target for the target alias itself")
	}
}

func TestTranslationMapOfAliases_ApplyTranslationFunction_QuantifiedObjectValue(t *testing.T) {
	t.Parallel()
	src := values.NamedCorrelationIdentifier("src")
	tgt := values.NamedCorrelationIdentifier("tgt")
	tm := TranslationMapOfAliases(src, tgt)

	qov := &values.QuantifiedObjectValue{
		Correlation: src,
		Typ:         values.NullableLong,
	}

	result := tm.ApplyTranslationFunction(src, qov)
	rebased, ok := result.(*values.QuantifiedObjectValue)
	if !ok {
		t.Fatalf("result should be *QuantifiedObjectValue, got %T", result)
	}
	if rebased.Correlation != tgt {
		t.Errorf("rebased correlation = %v, want %v", rebased.Correlation, tgt)
	}
	if rebased.Typ != values.NullableLong {
		t.Errorf("rebased type should be preserved, got %v", rebased.Typ)
	}
}

func TestTranslationMapOfAliases_GetAliasMap(t *testing.T) {
	t.Parallel()
	src := values.NamedCorrelationIdentifier("src")
	tgt := values.NamedCorrelationIdentifier("tgt")
	tm := TranslationMapOfAliases(src, tgt)

	am, ok := tm.GetAliasMap()
	if !ok {
		t.Fatal("ofAliases should have an alias map")
	}
	if am.Size() != 1 {
		t.Errorf("alias map size = %d, want 1", am.Size())
	}
	gotTarget := am.GetTarget(src)
	if gotTarget != tgt {
		t.Errorf("alias map target = %v, want %v", gotTarget, tgt)
	}
}

func TestTranslationMapOfAliases_DefinesOnlyIdentities_Different(t *testing.T) {
	t.Parallel()
	src := values.NamedCorrelationIdentifier("src")
	tgt := values.NamedCorrelationIdentifier("tgt")
	tm := TranslationMapOfAliases(src, tgt)

	if tm.DefinesOnlyIdentities() {
		t.Error("src != tgt, should not define only identities")
	}
}

func TestTranslationMapOfAliases_DefinesOnlyIdentities_Same(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("same")
	tm := TranslationMapOfAliases(alias, alias)

	// Even though source == target (identity alias map), the function
	// map is non-empty, so DefinesOnlyIdentities returns false.
	// This matches Java: an identity alias mapping still has a rebase
	// function registered.
	if tm.DefinesOnlyIdentities() {
		t.Error("identity alias with function entries should not define only identities")
	}
}

func TestRebaseWithAliasMap_MultipleEntries(t *testing.T) {
	t.Parallel()
	a := values.NamedCorrelationIdentifier("a")
	b := values.NamedCorrelationIdentifier("b")
	x := values.NamedCorrelationIdentifier("x")
	y := values.NamedCorrelationIdentifier("y")

	am := NewAliasMapBuilder()
	am.Put(a, x)
	am.Put(b, y)
	aliasMap := am.Build()

	tm := RebaseWithAliasMap(aliasMap)

	if !tm.ContainsSourceAlias(a) {
		t.Error("should contain alias a")
	}
	if !tm.ContainsSourceAlias(b) {
		t.Error("should contain alias b")
	}
	if tm.ContainsSourceAlias(x) {
		t.Error("should not contain target x as source")
	}

	// Apply translation to a QOV with alias a -> should get x.
	qovA := &values.QuantifiedObjectValue{Correlation: a, Typ: values.UnknownType}
	resultA := tm.ApplyTranslationFunction(a, qovA)
	rebasedA, ok := resultA.(*values.QuantifiedObjectValue)
	if !ok {
		t.Fatalf("result should be *QuantifiedObjectValue, got %T", resultA)
	}
	if rebasedA.Correlation != x {
		t.Errorf("rebased a correlation = %v, want %v", rebasedA.Correlation, x)
	}

	// Apply translation to a QOV with alias b -> should get y.
	qovB := &values.QuantifiedObjectValue{Correlation: b, Typ: values.NullableString}
	resultB := tm.ApplyTranslationFunction(b, qovB)
	rebasedB, ok := resultB.(*values.QuantifiedObjectValue)
	if !ok {
		t.Fatalf("result should be *QuantifiedObjectValue, got %T", resultB)
	}
	if rebasedB.Correlation != y {
		t.Errorf("rebased b correlation = %v, want %v", rebasedB.Correlation, y)
	}
	if rebasedB.Typ != values.NullableString {
		t.Errorf("rebased b type should be preserved, got %v", rebasedB.Typ)
	}
}

func TestRebaseWithAliasMap_GetAliasMap(t *testing.T) {
	t.Parallel()
	a := values.NamedCorrelationIdentifier("a")
	b := values.NamedCorrelationIdentifier("b")
	am := AliasMapOfAliases(a, b)
	tm := RebaseWithAliasMap(am)

	gotAM, ok := tm.GetAliasMap()
	if !ok {
		t.Fatal("rebase-based translation map should have an alias map")
	}
	if gotAM.Size() != 1 {
		t.Errorf("alias map size = %d, want 1", gotAM.Size())
	}
}

func TestBuilder_WhenThen_SingleEntry(t *testing.T) {
	t.Parallel()
	src := values.NamedCorrelationIdentifier("src")
	custom := values.NamedCorrelationIdentifier("custom")

	tm := NewTranslationMapBuilder().
		When(src).Then(func(_ values.CorrelationIdentifier, lv values.LeafValue) values.Value {
		return &values.QuantifiedObjectValue{
			Correlation: custom,
			Typ:         values.NullableBoolean,
		}
	}).Build()

	if !tm.ContainsSourceAlias(src) {
		t.Error("should contain source alias")
	}

	qov := &values.QuantifiedObjectValue{Correlation: src, Typ: values.UnknownType}
	result := tm.ApplyTranslationFunction(src, qov)
	rebased, ok := result.(*values.QuantifiedObjectValue)
	if !ok {
		t.Fatalf("result should be *QuantifiedObjectValue, got %T", result)
	}
	if rebased.Correlation != custom {
		t.Errorf("rebased correlation = %v, want %v", rebased.Correlation, custom)
	}
	if rebased.Typ != values.NullableBoolean {
		t.Errorf("rebased type = %v, want NullableBoolean", rebased.Typ)
	}
}

func TestBuilder_WhenThen_MultipleEntries(t *testing.T) {
	t.Parallel()
	a := values.NamedCorrelationIdentifier("a")
	b := values.NamedCorrelationIdentifier("b")

	tm := NewTranslationMapBuilder().
		When(a).Then(func(src values.CorrelationIdentifier, lv values.LeafValue) values.Value {
		return &values.ConstantValue{Value: "translated_a", Typ: values.NullableString}
	}).
		When(b).Then(func(src values.CorrelationIdentifier, lv values.LeafValue) values.Value {
		return &values.ConstantValue{Value: "translated_b", Typ: values.NullableString}
	}).Build()

	if !tm.ContainsSourceAlias(a) {
		t.Error("should contain alias a")
	}
	if !tm.ContainsSourceAlias(b) {
		t.Error("should contain alias b")
	}

	qovA := &values.QuantifiedObjectValue{Correlation: a, Typ: values.UnknownType}
	resultA := tm.ApplyTranslationFunction(a, qovA)
	constA, ok := resultA.(*values.ConstantValue)
	if !ok {
		t.Fatalf("result should be *ConstantValue, got %T", resultA)
	}
	if constA.Value != "translated_a" {
		t.Errorf("translated value = %v, want translated_a", constA.Value)
	}

	qovB := &values.QuantifiedObjectValue{Correlation: b, Typ: values.UnknownType}
	resultB := tm.ApplyTranslationFunction(b, qovB)
	constB, ok := resultB.(*values.ConstantValue)
	if !ok {
		t.Fatalf("result should be *ConstantValue, got %T", resultB)
	}
	if constB.Value != "translated_b" {
		t.Errorf("translated value = %v, want translated_b", constB.Value)
	}
}

func TestBuilder_WhenAny(t *testing.T) {
	t.Parallel()
	a := values.NamedCorrelationIdentifier("a")
	b := values.NamedCorrelationIdentifier("b")
	c := values.NamedCorrelationIdentifier("c")
	tgt := values.NamedCorrelationIdentifier("target")

	tm := NewTranslationMapBuilder().
		WhenAny([]values.CorrelationIdentifier{a, b, c}).Then(func(_ values.CorrelationIdentifier, lv values.LeafValue) values.Value {
		return &values.QuantifiedObjectValue{Correlation: tgt, Typ: values.UnknownType}
	}).Build()

	for _, alias := range []values.CorrelationIdentifier{a, b, c} {
		if !tm.ContainsSourceAlias(alias) {
			t.Errorf("should contain alias %v", alias)
		}
	}
}

func TestBuilder_EmptyBuild(t *testing.T) {
	t.Parallel()
	tm := NewTranslationMapBuilder().Build()
	if !tm.DefinesOnlyIdentities() {
		t.Error("empty builder should produce identity-only map")
	}
	if tm.ContainsSourceAlias(values.NamedCorrelationIdentifier("anything")) {
		t.Error("empty builder should not contain any alias")
	}
}

func TestBuilder_Compose(t *testing.T) {
	t.Parallel()
	a := values.NamedCorrelationIdentifier("a")
	b := values.NamedCorrelationIdentifier("b")
	x := values.NamedCorrelationIdentifier("x")
	y := values.NamedCorrelationIdentifier("y")

	tm1 := TranslationMapOfAliases(a, x)
	tm2 := TranslationMapOfAliases(b, y)

	composed := ComposeTranslationMaps(tm1, tm2)

	if !composed.ContainsSourceAlias(a) {
		t.Error("composed should contain alias a")
	}
	if !composed.ContainsSourceAlias(b) {
		t.Error("composed should contain alias b")
	}

	// Check alias map is composed too.
	am, ok := composed.GetAliasMap()
	if !ok {
		t.Fatal("composed should have an alias map")
	}
	if am.Size() != 2 {
		t.Errorf("composed alias map size = %d, want 2", am.Size())
	}

	gotX, okX := am.GetTargetOrEmpty(a)
	if !okX || gotX != x {
		t.Errorf("composed alias map target for a = %v (ok=%v), want x", gotX, okX)
	}
	gotY, okY := am.GetTargetOrEmpty(b)
	if !okY || gotY != y {
		t.Errorf("composed alias map target for b = %v (ok=%v), want y", gotY, okY)
	}
}

func TestApplyTranslationFunction_PanicsOnMissing(t *testing.T) {
	t.Parallel()
	tm := EmptyTranslationMap()
	alias := values.NamedCorrelationIdentifier("missing")
	qov := &values.QuantifiedObjectValue{Correlation: alias, Typ: values.UnknownType}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("ApplyTranslationFunction should panic for missing alias")
		}
	}()
	tm.ApplyTranslationFunction(alias, qov)
}

func TestTranslationMap_InterfaceCompliance(t *testing.T) {
	t.Parallel()
	// Verify that RegularTranslationMap satisfies TranslationMap.
	var tm TranslationMap = EmptyTranslationMap()
	if tm.ContainsSourceAlias(values.NamedCorrelationIdentifier("x")) {
		t.Error("should not contain x")
	}
}

func TestTranslationMapOfAliases_PreservesType(t *testing.T) {
	t.Parallel()
	src := values.NamedCorrelationIdentifier("src")
	tgt := values.NamedCorrelationIdentifier("tgt")
	tm := TranslationMapOfAliases(src, tgt)

	// Apply with a QOV that has a specific type — the type should
	// be preserved through the rebase.
	recordType := &values.RecordType{
		Nullable: false,
		Fields: []values.Field{
			{Name: "id", FieldType: values.NotNullLong, Ordinal: 0},
			{Name: "name", FieldType: values.NullableString, Ordinal: 1},
		},
	}
	qov := &values.QuantifiedObjectValue{Correlation: src, Typ: recordType}
	result := tm.ApplyTranslationFunction(src, qov)
	rebased, ok := result.(*values.QuantifiedObjectValue)
	if !ok {
		t.Fatalf("result should be *QuantifiedObjectValue, got %T", result)
	}
	if rebased.Correlation != tgt {
		t.Errorf("correlation = %v, want %v", rebased.Correlation, tgt)
	}
	rt, ok := rebased.Typ.(*values.RecordType)
	if !ok {
		t.Fatalf("type should be *RecordType, got %T", rebased.Typ)
	}
	if len(rt.Fields) != 2 {
		t.Errorf("fields count = %d, want 2", len(rt.Fields))
	}
}

func TestBuilder_GetAliasMap_Present(t *testing.T) {
	t.Parallel()
	src := values.NamedCorrelationIdentifier("src")
	custom := values.NamedCorrelationIdentifier("custom")

	tm := NewTranslationMapBuilder().
		When(src).Then(func(_ values.CorrelationIdentifier, lv values.LeafValue) values.Value {
		return &values.QuantifiedObjectValue{Correlation: custom, Typ: values.UnknownType}
	}).Build()

	// Builder always builds with an alias map (even if the alias map
	// is empty — the builder tracks it).
	am, ok := tm.GetAliasMap()
	if !ok {
		t.Fatal("builder-produced map should have an alias map")
	}
	// No alias map entries were added via the builder's alias map
	// builder — only function entries were added. So the alias map
	// should be empty.
	if am.Size() != 0 {
		t.Errorf("alias map size = %d, want 0", am.Size())
	}
}

func TestComposeTranslationMaps_PanicsOnDuplicate(t *testing.T) {
	t.Parallel()
	a := values.NamedCorrelationIdentifier("a")
	x := values.NamedCorrelationIdentifier("x")
	y := values.NamedCorrelationIdentifier("y")

	tm1 := TranslationMapOfAliases(a, x)
	tm2 := TranslationMapOfAliases(a, y)

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("composing maps with duplicate source alias should panic")
		}
	}()
	ComposeTranslationMaps(tm1, tm2)
}

func TestAliasMapBuilder_PutAll(t *testing.T) {
	t.Parallel()
	a := values.NamedCorrelationIdentifier("a")
	b := values.NamedCorrelationIdentifier("b")
	x := values.NamedCorrelationIdentifier("x")
	y := values.NamedCorrelationIdentifier("y")

	am1 := AliasMapOfAliases(a, x)

	builder := NewAliasMapBuilder()
	builder.Put(b, y)
	builder.PutAll(am1)

	result := builder.Build()
	if result.Size() != 2 {
		t.Errorf("size = %d, want 2", result.Size())
	}
	if result.GetTarget(a) != x {
		t.Errorf("target of a = %v, want x", result.GetTarget(a))
	}
	if result.GetTarget(b) != y {
		t.Errorf("target of b = %v, want y", result.GetTarget(b))
	}
}

func TestAliasMapBuilder_PutAll_SkipsConflicts(t *testing.T) {
	t.Parallel()
	a := values.NamedCorrelationIdentifier("a")
	x := values.NamedCorrelationIdentifier("x")
	y := values.NamedCorrelationIdentifier("y")

	am1 := AliasMapOfAliases(a, x)

	builder := NewAliasMapBuilder()
	builder.Put(a, y)   // a is already mapped to y
	builder.PutAll(am1) // tries to map a -> x, should be skipped

	result := builder.Build()
	if result.Size() != 1 {
		t.Errorf("size = %d, want 1", result.Size())
	}
	// a should still map to y (the first mapping wins).
	if result.GetTarget(a) != y {
		t.Errorf("target of a = %v, want y", result.GetTarget(a))
	}
}

func TestLeafValue_QuantifiedObjectValue(t *testing.T) {
	t.Parallel()
	src := values.NamedCorrelationIdentifier("src")
	tgt := values.NamedCorrelationIdentifier("tgt")

	qov := &values.QuantifiedObjectValue{Correlation: src, Typ: values.NullableLong}

	// QuantifiedObjectValue implements LeafValue.
	var lv values.LeafValue = qov
	result := lv.RebaseLeaf(tgt)

	rebased, ok := result.(*values.QuantifiedObjectValue)
	if !ok {
		t.Fatalf("result should be *QuantifiedObjectValue, got %T", result)
	}
	if rebased.Correlation != tgt {
		t.Errorf("correlation = %v, want %v", rebased.Correlation, tgt)
	}
	if rebased.Typ != values.NullableLong {
		t.Errorf("type should be preserved")
	}
	// Should be a new value, not the same pointer.
	if rebased == qov {
		t.Error("RebaseLeaf should return a new value")
	}
}
