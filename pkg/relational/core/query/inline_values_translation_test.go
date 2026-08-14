package query

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

func inlineValuesQueryFixture(t *testing.T) *logical.LogicalInlineValues {
	t.Helper()
	rowType := &values.RecordType{Fields: []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullInt},
		{Name: "ARR", Ordinal: 1, FieldType: values.NewArrayType(false, values.NotNullInt)},
	}}
	row := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: &values.ConstantValue{Value: int32(1), Typ: values.NotNullInt}},
		values.RecordConstructorField{Name: "ARR", Value: values.NewArrayConstructorValue(
			values.NotNullInt,
			[]values.Value{&values.ConstantValue{Value: int32(101), Typ: values.NotNullInt}},
		)},
	)
	if !row.Type().Equals(rowType) {
		t.Fatalf("fixture row = %v, want %v", row.Type(), rowType)
	}
	source, err := logical.NewInlineValues("V", values.NewArrayConstructorValue(rowType, []values.Value{row}))
	if err != nil {
		t.Fatalf("NewInlineValues: %v", err)
	}
	return source
}

func TestInlineValuesExactResultTypeAndExplodeLoweringAgree(t *testing.T) {
	t.Parallel()
	source := inlineValuesQueryFixture(t)

	exact, err := ExactLogicalResultType(source, nil)
	if err != nil {
		t.Fatalf("ExactLogicalResultType: %v", err)
	}
	if !exact.Equals(source.ResultType()) {
		t.Fatalf("exact row = %v, want frozen source row %v", exact, source.ResultType())
	}

	ref, _, err := TranslateToCascadesWithError(source, nil)
	if err != nil {
		t.Fatalf("TranslateToCascadesWithError: %v", err)
	}
	if ref == nil || len(ref.Members()) != 1 {
		t.Fatalf("translated ref = %#v, want one Explode member", ref)
	}
	explode, ok := ref.Members()[0].(*expressions.ExplodeExpression)
	if !ok {
		t.Fatalf("translated leaf = %T, want *ExplodeExpression", ref.Members()[0])
	}
	if explode.GetCollectionValue() != source.CollectionValue() {
		t.Fatal("translator replaced the literal collection instead of preserving its exact Value identity")
	}
	if explode.GetWithOrdinality() || !explode.GetExplodeResultType().Equals(exact) {
		t.Fatalf("explode result = %v ordinal=%v, want non-ordinal %v",
			explode.GetExplodeResultType(), explode.GetWithOrdinality(), exact)
	}
	if sourceAlias(source) != "V" || sourceBinding(source) != "V" {
		t.Fatalf("source identity = alias %q binding %q, want V/V", sourceAlias(source), sourceBinding(source))
	}
}

func TestInlineValuesTranslationRejectsCollectionTypeDrift(t *testing.T) {
	t.Parallel()
	source := inlineValuesQueryFixture(t)
	collection, ok := source.CollectionValue().(*values.ArrayConstructorValue)
	if !ok {
		t.Fatalf("collection = %T, want *ArrayConstructorValue", source.CollectionValue())
	}
	row, ok := collection.ElementType.(*values.RecordType)
	if !ok || len(row.Fields) == 0 {
		t.Fatalf("collection element = %T %v, want non-empty exact record", collection.ElementType, collection.ElementType)
	}

	// The logical leaf owns a frozen schema snapshot. Mutating the ordinary
	// collection Type graph after construction must therefore decline lowering,
	// not publish an Explode whose physical row disagrees with that snapshot.
	row.Fields[0].Name = "MUTATED"
	if ref, _, err := TranslateToCascadesWithError(source, nil); err == nil || ref != nil {
		t.Fatalf("translation after type drift = (%#v, %v), want nil,error", ref, err)
	}
}

func TestInlineValuesOwnerTypesLateralArrayWithoutMetadata(t *testing.T) {
	t.Parallel()
	source := inlineValuesQueryFixture(t)
	unnest := &logical.LogicalUnnest{Segments: []string{"V", "ARR"}, Alias: "EL", AtAlias: "O"}
	join := logical.NewJoin(source, unnest, logical.JoinInner, "")

	exact, err := ExactLogicalResultType(join, nil)
	if err != nil {
		t.Fatalf("ExactLogicalResultType(join): %v", err)
	}
	row, ok := exact.(*values.RecordType)
	if !ok || len(row.Fields) != 4 {
		t.Fatalf("joined result = %T %v, want four-field exact row", exact, exact)
	}
	for ordinal, want := range []struct {
		name string
		typ  values.Type
	}{
		{"V.ID", values.NotNullInt},
		{"V.ARR", values.NewArrayType(false, values.NotNullInt)},
		{"EL.EL", values.NotNullInt},
		{"EL.O", values.NotNullInt},
	} {
		field := row.Fields[ordinal]
		if field.Name != want.name || field.Ordinal != ordinal || !field.FieldType.Equals(want.typ) {
			t.Fatalf("joined field %d = %+v, want %s %v", ordinal, field, want.name, want.typ)
		}
	}

	ref, _, err := TranslateToCascadesWithError(join, nil)
	if err != nil {
		t.Fatalf("TranslateToCascadesWithError(join): %v", err)
	}
	if ref == nil || len(ref.Members()) != 1 {
		t.Fatalf("translated join = %#v, want one select member", ref)
	}
	selectExpr, ok := ref.Members()[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("translated join root = %T, want *SelectExpression", ref.Members()[0])
	}
	if aliases := selectExpr.GetSourceAliases(); len(aliases) != 2 || aliases[0] != "V" || aliases[1] != "EL" {
		t.Fatalf("select aliases = %v, want [V EL]", aliases)
	}
	quants := selectExpr.GetQuantifiers()
	if len(quants) != 2 {
		t.Fatalf("select quantifiers = %d, want 2", len(quants))
	}
	innerMembers := quants[1].GetRangesOver().Members()
	if len(innerMembers) != 1 {
		t.Fatalf("inner members = %d, want one Explode", len(innerMembers))
	}
	innerExplode, ok := innerMembers[0].(*expressions.ExplodeExpression)
	if !ok || !innerExplode.GetWithOrdinality() {
		t.Fatalf("inner member = %T ordinal=%v, want ordinal Explode", innerMembers[0], ok && innerExplode.GetWithOrdinality())
	}
	collection, ok := values.AsFieldValue(innerExplode.GetCollectionValue())
	if !ok || !collection.Path().IsFrontierPinned() || len(collection.Path().Ordinals()) != 1 ||
		collection.Path().Ordinals()[0] != 1 {
		t.Fatalf("inner collection = %v, want frontier-pinned V.ARR ordinal 1", innerExplode.GetCollectionValue())
	}
}

func TestInlineValuesOwnerRejectsForeignScalarAndDuplicateOwners(t *testing.T) {
	t.Parallel()
	source := inlineValuesQueryFixture(t)

	for name, unnest := range map[string]*logical.LogicalUnnest{
		"foreign": {Segments: []string{"X", "ARR"}, Alias: "EL"},
		"scalar":  {Segments: []string{"V", "ID"}, Alias: "EL"},
		"missing": {Segments: []string{"V", "NOPE"}, Alias: "EL"},
	} {
		t.Run(name, func(t *testing.T) {
			join := logical.NewJoin(source, unnest, logical.JoinInner, "")
			if typ, err := ExactLogicalResultType(join, nil); err == nil || typ != nil {
				t.Fatalf("ExactLogicalResultType = (%v, %v), want nil,error", typ, err)
			}
		})
	}

	left := logical.NewJoin(source, inlineValuesQueryFixture(t), logical.JoinInner, "")
	if owner := findInlineValuesOwner(left, "V"); owner != nil {
		t.Fatalf("duplicate V owners selected %p by traversal order", owner)
	}
	if typ, err := ExactLogicalResultType(
		logical.NewJoin(left, &logical.LogicalUnnest{Segments: []string{"V", "ARR"}, Alias: "EL"}, logical.JoinInner, ""), nil,
	); err == nil || typ != nil || !strings.Contains(err.Error(), "record metadata") {
		t.Fatalf("duplicate owner typed as (%v, %v), want loud owner ambiguity", typ, err)
	}
}
