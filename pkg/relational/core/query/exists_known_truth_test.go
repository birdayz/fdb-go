package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

func TestFoldKnownExists(t *testing.T) {
	t.Parallel()

	trueAlias := values.NamedCorrelationIdentifier("known_true")
	falseAlias := values.NamedCorrelationIdentifier("known_false")
	unknownAlias := values.NamedCorrelationIdentifier("unknown")
	scan := logical.NewScan("T", "T")

	exists := func(alias values.CorrelationIdentifier) predicates.QueryPredicate {
		return predicates.NewExistentialAlias(alias)
	}
	notExists := func(alias values.CorrelationIdentifier) predicates.QueryPredicate {
		return predicates.NewNot(exists(alias))
	}
	trueSubquery := logical.ExistsSubquery{Alias: trueAlias, Plan: scan, KnownTruth: predicates.TriTrue}
	falseSubquery := logical.ExistsSubquery{Alias: falseAlias, Plan: scan, KnownTruth: predicates.TriFalse}
	unknownSubquery := logical.ExistsSubquery{Alias: unknownAlias, Plan: scan}
	filter := func(pred predicates.QueryPredicate, subqueries ...logical.ExistsSubquery) *logical.LogicalFilter {
		return &logical.LogicalFilter{
			Input:            scan,
			Predicate:        pred,
			ExistsSubqueries: subqueries,
		}
	}
	assertConstant := func(t *testing.T, got *logical.LogicalFilter, want predicates.TriBool) {
		t.Helper()
		constant, ok := got.Predicate.(*predicates.ConstantPredicate)
		if !ok || constant.Value != want {
			t.Fatalf("predicate = %T %v, want constant %v", got.Predicate, got.Predicate, want)
		}
	}

	t.Run("positive_true", func(t *testing.T) {
		got := (&cascadesTranslator{}).foldKnownExists(filter(exists(trueAlias), trueSubquery))
		assertConstant(t, got, predicates.TriTrue)
		if len(got.ExistsSubqueries) != 0 {
			t.Fatalf("subqueries = %d, want none", len(got.ExistsSubqueries))
		}
	})

	t.Run("positive_false", func(t *testing.T) {
		got := (&cascadesTranslator{}).foldKnownExists(filter(exists(falseAlias), falseSubquery))
		assertConstant(t, got, predicates.TriFalse)
		if len(got.ExistsSubqueries) != 0 {
			t.Fatalf("FALSE should absorb every existential conjunct, kept %d", len(got.ExistsSubqueries))
		}
	})

	t.Run("negated_true", func(t *testing.T) {
		got := (&cascadesTranslator{}).foldKnownExists(filter(notExists(trueAlias), trueSubquery))
		assertConstant(t, got, predicates.TriFalse)
	})

	t.Run("negated_false", func(t *testing.T) {
		got := (&cascadesTranslator{}).foldKnownExists(filter(notExists(falseAlias), falseSubquery))
		assertConstant(t, got, predicates.TriTrue)
	})

	t.Run("true_drops_from_and", func(t *testing.T) {
		other := predicates.NewConstantPredicate(predicates.TriUnknown)
		got := (&cascadesTranslator{}).foldKnownExists(filter(
			predicates.NewAnd(other, exists(trueAlias)), trueSubquery))
		if got.Predicate != other {
			t.Fatalf("predicate = %T %v, want the unrelated conjunct", got.Predicate, got.Predicate)
		}
	})

	t.Run("mixed_polarity_false_absorbs", func(t *testing.T) {
		got := (&cascadesTranslator{}).foldKnownExists(filter(
			predicates.NewAnd(exists(trueAlias), notExists(trueAlias)), trueSubquery))
		assertConstant(t, got, predicates.TriFalse)
	})

	t.Run("known_true_and_ordinary_exists", func(t *testing.T) {
		got := (&cascadesTranslator{}).foldKnownExists(filter(
			predicates.NewAnd(exists(trueAlias), exists(unknownAlias)),
			trueSubquery, unknownSubquery,
		))
		if alias, ok := predicates.IsExistentialPredicate(got.Predicate); !ok || alias != unknownAlias {
			t.Fatalf("predicate = %T %v, want the ordinary EXISTS marker", got.Predicate, got.Predicate)
		}
		if len(got.ExistsSubqueries) != 1 || got.ExistsSubqueries[0].Alias != unknownAlias {
			t.Fatalf("subqueries = %+v, want only ordinary alias", got.ExistsSubqueries)
		}
	})

	t.Run("unknown_is_untouched", func(t *testing.T) {
		input := filter(exists(unknownAlias), unknownSubquery)
		got := (&cascadesTranslator{}).foldKnownExists(input)
		if got != input {
			t.Fatal("unknown EXISTS should be returned unchanged")
		}
	})

	t.Run("nested_or_is_untouched", func(t *testing.T) {
		input := filter(predicates.NewOr(
			exists(trueAlias),
			predicates.NewConstantPredicate(predicates.TriFalse),
		), trueSubquery)
		translator := &cascadesTranslator{}
		got := translator.foldKnownExists(input)
		if got != input {
			t.Fatal("known alias below OR should be returned unchanged")
		}
		if translated := translator.translateFilter(input); translated != nil {
			t.Fatalf("translateFilter returned %T, want typed decline", translated)
		}
		if translator.translateErr == nil {
			t.Fatal("translateFilter did not record the typed decline")
		}
	})
}
