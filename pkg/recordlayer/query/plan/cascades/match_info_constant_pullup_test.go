package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

type matchInfoConstantPullUpFixture struct {
	queryAlias          values.CorrelationIdentifier
	candidateAlias      values.CorrelationIdentifier
	queryExpression     expressions.RelationalExpression
	candidateExpression expressions.RelationalExpression
	queryQuantifier     expressions.Quantifier
	childPartialMatch   *PartialMatchImpl
}

func newMatchInfoConstantPullUpFixture(
	name string,
	queryResult values.Value,
	candidateResult values.Value,
	groupByMappings *GroupByMappings,
) matchInfoConstantPullUpFixture {
	queryExpression := expressions.NewSelectExpression(queryResult, nil, nil)
	candidateExpression := expressions.NewSelectExpression(candidateResult, nil, nil)
	return newMatchInfoConstantPullUpExpressionFixture(
		name,
		queryExpression,
		candidateExpression,
		groupByMappings,
	)
}

func newMatchInfoConstantPullUpExpressionFixture(
	name string,
	queryExpression expressions.RelationalExpression,
	candidateExpression expressions.RelationalExpression,
	groupByMappings *GroupByMappings,
) matchInfoConstantPullUpFixture {
	queryRef := expressions.InitialOf(queryExpression)
	candidateRef := expressions.InitialOf(candidateExpression)
	matchInfo := NewRegularMatchInfo(
		nil,
		EmptyAliasMap(),
		nil,
		nil,
		NewMaxMatchMap(nil, nil, nil),
		groupByMappings,
		nil,
		nil,
	)
	childPartialMatch := NewPartialMatch(
		EmptyAliasMap(),
		stubMatchCandidate{name: name},
		queryRef,
		queryExpression,
		candidateRef,
		matchInfo,
	)
	queryAlias := values.NamedCorrelationIdentifier(name + "_query_upper")
	candidateAlias := values.NamedCorrelationIdentifier(name + "_candidate_upper")
	return matchInfoConstantPullUpFixture{
		queryAlias:          queryAlias,
		candidateAlias:      candidateAlias,
		queryExpression:     queryExpression,
		candidateExpression: candidateExpression,
		queryQuantifier: expressions.NamedForEachQuantifier(
			queryAlias,
			queryRef,
		),
		childPartialMatch: childPartialMatch,
	}
}

func (f matchInfoConstantPullUpFixture) merge() (*RegularMatchInfo, bool) {
	return tryMergeRegularMatchInfo(
		AliasMapOfAliases(f.queryAlias, f.candidateAlias),
		[]quantifierPartialMatch{{
			quantifier:   f.queryQuantifier,
			partialMatch: f.childPartialMatch,
		}},
		nil,
		nil,
		NewMaxMatchMap(nil, nil, nil),
		EmptyGroupByMappings(),
		nil,
		nil,
	)
}

func matchInfoConstantPullUpMatchedGrouping(
	queryValue values.Value,
	candidateValue values.Value,
) *GroupByMappings {
	matchedGroupings := NewValueBiMap()
	matchedGroupings.Put(queryValue, candidateValue)
	return NewGroupByMappings(
		matchedGroupings,
		NewValueBiMap(),
		NewCorrValueBiMap(),
	)
}

func matchInfoConstantPullUpUnmatchedAggregate(
	aggregateAlias values.CorrelationIdentifier,
	queryValue values.Value,
) *GroupByMappings {
	unmatchedAggregates := NewCorrValueBiMap()
	unmatchedAggregates.Put(aggregateAlias, queryValue)
	return NewGroupByMappings(
		NewValueBiMap(),
		NewValueBiMap(),
		unmatchedAggregates,
	)
}

func matchInfoConstantPullUpOnlyMatchedGrouping(
	t *testing.T,
	matchInfo *RegularMatchInfo,
) (values.Value, values.Value) {
	t.Helper()
	matched := matchInfo.GetGroupByMappings().MatchedGroupingsMap()
	if got := matched.Len(); got != 1 {
		t.Fatalf("pulled matched groupings = %d, want 1", got)
	}
	var queryValue values.Value
	var candidateValue values.Value
	matched.Range(func(query, candidate values.Value) bool {
		queryValue = query
		candidateValue = candidate
		return false
	})
	return queryValue, candidateValue
}

func matchInfoConstantPullUpField(
	alias values.CorrelationIdentifier,
	field string,
) *values.FieldValue {
	return values.NewFieldValue(
		values.NewQuantifiedObjectValue(alias),
		field,
		values.NullableLong,
	)
}

// A query value rooted at an alias that is both projected by the lower result
// and external to the lower expression is constant during pull-up. The exact
// access path does not have to occur in the result: outer.y remains outer.y
// even when the result projects only outer.x. The candidate value still takes
// the ordinary result-field pull-up path.
func TestMatchInfoGroupPullUp_PreservesMatchedQueryConstantAlias(t *testing.T) {
	t.Parallel()

	outerAlias := values.NamedCorrelationIdentifier("matched_query_outer")
	outerX := matchInfoConstantPullUpField(outerAlias, "x")
	outerY := matchInfoConstantPullUpField(outerAlias, "y")
	queryResult := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "projected_x", Value: outerX},
	)

	candidateLowerAlias := values.NamedCorrelationIdentifier(
		"matched_query_candidate_lower",
	)
	candidateInput := matchInfoConstantPullUpField(
		candidateLowerAlias,
		"candidate_input",
	)
	candidateResult := values.NewRecordConstructorValue(
		values.RecordConstructorField{
			Name:  "candidate_projected",
			Value: candidateInput,
		},
	)
	fixture := newMatchInfoConstantPullUpFixture(
		"matched_query_constant",
		queryResult,
		candidateResult,
		matchInfoConstantPullUpMatchedGrouping(outerY, candidateInput),
	)
	if _, externallyCorrelated := expressions.GetCorrelatedToOfExpression(
		fixture.queryExpression,
	)[outerAlias]; !externallyCorrelated {
		t.Fatal("query fixture is not externally correlated to outer")
	}

	merged, ok := fixture.merge()
	if !ok || merged == nil {
		t.Fatal("matched query constant-alias mapping was rejected")
	}
	pulledQuery, pulledCandidate := matchInfoConstantPullUpOnlyMatchedGrouping(t, merged)
	if !values.SemanticEqualsUnderAliasMap(pulledQuery, outerY, nil) {
		t.Fatalf(
			"pulled query constant = %s, want identity %s",
			values.ExplainValue(pulledQuery),
			values.ExplainValue(outerY),
		)
	}
	candidateField, ok := pulledCandidate.(*values.FieldValue)
	if !ok || candidateField.Field != "candidate_projected" {
		t.Fatalf(
			"ordinarily pulled candidate = %s, want candidate_projected field",
			values.ExplainValue(pulledCandidate),
		)
	}
}

// The candidate-side constant set is the root correlations of the matched
// candidate key minus the candidate lower expression's external correlations.
// A root alias absent from the lower expression is therefore an identity
// constant, even when the candidate result cannot otherwise express it.
func TestMatchInfoGroupPullUp_PreservesMatchedCandidateRootConstant(t *testing.T) {
	t.Parallel()

	queryInput := values.LiteralValue(int64(202))
	queryResult := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "query_projected", Value: queryInput},
	)

	candidateConstantAlias := values.NamedCorrelationIdentifier(
		"matched_candidate_constant",
	)
	candidateConstant := values.NewQuantifiedObjectValue(candidateConstantAlias)
	candidateResult := values.LiteralValue(int64(303))
	fixture := newMatchInfoConstantPullUpFixture(
		"matched_candidate_root_constant",
		queryResult,
		candidateResult,
		matchInfoConstantPullUpMatchedGrouping(queryInput, candidateConstant),
	)
	if _, externallyCorrelated := expressions.GetCorrelatedToOfExpression(
		fixture.candidateExpression,
	)[candidateConstantAlias]; externallyCorrelated {
		t.Fatal("candidate constant alias unexpectedly belongs to the lower expression")
	}

	merged, ok := fixture.merge()
	if !ok || merged == nil {
		t.Fatal("matched candidate root-constant mapping was rejected")
	}
	pulledQuery, pulledCandidate := matchInfoConstantPullUpOnlyMatchedGrouping(t, merged)
	if !values.SemanticEqualsUnderAliasMap(pulledQuery, queryInput, nil) {
		t.Fatalf(
			"pulled query-side constant = %s, want identity %s",
			values.ExplainValue(pulledQuery),
			values.ExplainValue(queryInput),
		)
	}
	if !values.SemanticEqualsUnderAliasMap(
		pulledCandidate,
		candidateConstant,
		nil,
	) {
		t.Fatalf(
			"pulled candidate constant = %s, want identity %s",
			values.ExplainValue(pulledCandidate),
			values.ExplainValue(candidateConstant),
		)
	}
}

// Unmatched query aggregates use their root-correlation difference from the
// query lower expression as the constant set. Preserve such a root value
// rather than discarding all child group metadata merely because it is absent
// from the lower result.
func TestMatchInfoGroupPullUp_PreservesUnmatchedQueryRootConstant(t *testing.T) {
	t.Parallel()

	resultAlias := values.NamedCorrelationIdentifier("unmatched_result_root")
	unmatchedRootAlias := values.NamedCorrelationIdentifier(
		"unmatched_constant_root",
	)
	queryResult := values.NewQuantifiedObjectValue(resultAlias)
	unmatchedValue := values.NewQuantifiedObjectValue(unmatchedRootAlias)
	unmatchedAggregateAlias := values.NamedCorrelationIdentifier(
		"unmatched_aggregate",
	)
	fixture := newMatchInfoConstantPullUpFixture(
		"unmatched_query_root_constant",
		queryResult,
		values.LiteralValue(int64(404)),
		matchInfoConstantPullUpUnmatchedAggregate(
			unmatchedAggregateAlias,
			unmatchedValue,
		),
	)
	queryCorrelations := expressions.GetCorrelatedToOfExpression(
		fixture.queryExpression,
	)
	if _, present := queryCorrelations[resultAlias]; !present {
		t.Fatal("query result root is missing from lower-expression correlations")
	}
	if _, present := queryCorrelations[unmatchedRootAlias]; present {
		t.Fatal("unmatched root unexpectedly belongs to the lower expression")
	}

	merged, ok := fixture.merge()
	if !ok || merged == nil {
		t.Fatal("unmatched query root-constant mapping was rejected")
	}
	unmatched := merged.GetGroupByMappings().UnmatchedAggregatesMap()
	if got := unmatched.Len(); got != 1 {
		t.Fatalf("pulled unmatched aggregates = %d, want 1", got)
	}
	pulled, present := unmatched.Get(unmatchedAggregateAlias)
	if !present {
		t.Fatal("pulled unmatched aggregate is missing")
	}
	if !values.SemanticEqualsUnderAliasMap(pulled, unmatchedValue, nil) {
		t.Fatalf(
			"pulled unmatched constant = %s, want identity %s",
			values.ExplainValue(pulled),
			values.ExplainValue(unmatchedValue),
		)
	}
}

// Pull-up through a RecordConstructor is not a function when two output fields
// contain the same input value. GroupByMappings cannot represent that
// ambiguity, so the enclosing RegularMatchInfo merge must fail closed instead
// of choosing the first field.
func TestMatchInfoGroupPullUp_RejectsAmbiguousRecordConstructor(t *testing.T) {
	t.Parallel()

	queryInput := values.LiteralValue(int64(505))
	queryResult := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "query", Value: queryInput},
	)

	candidateLowerAlias := values.NamedCorrelationIdentifier(
		"ambiguous_candidate_lower",
	)
	candidateInput := matchInfoConstantPullUpField(candidateLowerAlias, "input")
	candidateResult := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "first", Value: candidateInput},
		values.RecordConstructorField{Name: "second", Value: candidateInput},
	)
	if got := len(candidateResult.Fields); got != 2 {
		t.Fatalf("ambiguous result fixture has %d fields, want 2", got)
	}
	fixture := newMatchInfoConstantPullUpFixture(
		"ambiguous_record_constructor",
		queryResult,
		candidateResult,
		matchInfoConstantPullUpMatchedGrouping(queryInput, candidateInput),
	)

	merged, ok := fixture.merge()
	if ok || merged != nil {
		t.Fatal("ambiguous RecordConstructor group pull-up was accepted")
	}
}

// Go's generic passthrough pull-up historically copied a FieldValue without
// its source. In a group mapping that can silently retarget a column in a
// multi-source parent. The group path may decline this pull-up, but if it
// returns a mapping the field must be rooted at the supplied upper alias.
func TestMatchInfoGroupPullUp_PassthroughFieldAnchoredOrRejected(t *testing.T) {
	t.Parallel()

	queryLowerAlias := values.NamedCorrelationIdentifier("passthrough_query_lower")
	queryLowerRef := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression(
			[]string{"T"},
			values.UnknownType,
		),
	)
	queryLowerQ := expressions.NamedForEachQuantifier(
		queryLowerAlias,
		queryLowerRef,
	)
	queryResult := queryLowerQ.GetFlowedObjectValue()
	queryInput := values.NewFieldValue(
		queryResult,
		"input",
		values.NullableLong,
	)
	queryExpression := expressions.NewSelectExpression(
		queryResult,
		[]expressions.Quantifier{queryLowerQ},
		nil,
	)

	candidateInput := values.LiteralValue(int64(606))
	candidateResult := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "candidate", Value: candidateInput},
	)
	candidateExpression := expressions.NewSelectExpression(
		candidateResult,
		nil,
		nil,
	)
	fixture := newMatchInfoConstantPullUpExpressionFixture(
		"passthrough_field_anchor",
		queryExpression,
		candidateExpression,
		matchInfoConstantPullUpMatchedGrouping(queryInput, candidateInput),
	)

	merged, ok := fixture.merge()
	if !ok {
		if merged != nil {
			t.Fatal("rejected passthrough pull-up returned non-nil MatchInfo")
		}
		return
	}
	if merged == nil {
		t.Fatal("successful passthrough pull-up returned nil MatchInfo")
	}
	matched := merged.GetGroupByMappings().MatchedGroupingsMap()
	if matched.Len() == 0 {
		return // safely rejected at the individual group-mapping boundary
	}
	pulledQuery, _ := matchInfoConstantPullUpOnlyMatchedGrouping(t, merged)
	pulledField, isField := pulledQuery.(*values.FieldValue)
	if !isField {
		t.Fatalf(
			"passthrough pull-up returned %T, want anchored FieldValue or rejection",
			pulledQuery,
		)
	}
	root, anchored := pulledField.Child.(*values.QuantifiedObjectValue)
	if !anchored || root.Correlation != fixture.queryAlias {
		t.Fatalf(
			"passthrough field = %s, want root alias %s or rejection",
			values.ExplainValue(pulledField),
			fixture.queryAlias,
		)
	}
}

func TestAdjustGroupByMappings_ConstantAndOrdinaryCandidateValues(t *testing.T) {
	t.Parallel()

	lowerAlias := values.NamedCorrelationIdentifier("adjust_lower")
	upperAlias := values.NamedCorrelationIdentifier("adjust_upper")
	constantAlias := values.NamedCorrelationIdentifier("adjust_constant")
	candidateInput := matchInfoConstantPullUpField(lowerAlias, "input")
	candidateConstant := values.NewQuantifiedObjectValue(constantAlias)
	candidateResult := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "projected", Value: candidateInput},
	)
	candidateExpression := expressions.NewSelectExpression(
		candidateResult,
		nil,
		nil,
	)

	ordinaryQueryKey := values.LiteralValue(int64(1))
	constantQueryKey := values.LiteralValue(int64(2))
	matched := NewValueBiMap()
	matched.Put(ordinaryQueryKey, candidateInput)
	matched.Put(constantQueryKey, candidateConstant)
	adjusted, ok := AdjustGroupByMappings(
		NewGroupByMappings(
			matched,
			NewValueBiMap(),
			NewCorrValueBiMap(),
		),
		upperAlias,
		candidateExpression,
	)
	if !ok || adjusted == nil {
		t.Fatal("compatible adjusted group mappings were rejected")
	}

	ordinary, present := adjusted.MatchedGroupingsMap().Get(ordinaryQueryKey)
	if !present {
		t.Fatal("ordinary adjusted mapping is missing")
	}
	ordinaryField, isField := ordinary.(*values.FieldValue)
	if !isField || ordinaryField.Resolved == nil {
		t.Fatalf("ordinary adjusted value = %T, want ordinal-resolved FieldValue", ordinary)
	}
	accessor, single := ordinaryField.Resolved.Single()
	if !single || accessor.Ordinal != 0 {
		t.Fatalf("ordinary adjusted path = %+v, want output ordinal 0", ordinaryField.Resolved)
	}
	ordinaryBase, anchored := ordinaryField.Child.(*values.QuantifiedObjectValue)
	if !anchored || ordinaryBase.Correlation != upperAlias {
		t.Fatalf(
			"ordinary adjusted field is not anchored to upper alias %s",
			upperAlias,
		)
	}

	constant, present := adjusted.MatchedGroupingsMap().Get(constantQueryKey)
	if !present {
		t.Fatal("constant adjusted mapping is missing")
	}
	if !values.SemanticEqualsUnderAliasMap(constant, candidateConstant, nil) {
		t.Fatalf(
			"adjusted constant = %s, want identity %s",
			values.ExplainValue(constant),
			values.ExplainValue(candidateConstant),
		)
	}
}

func TestAdjustGroupByMappings_QuantifiedRecordPassthrough(t *testing.T) {
	t.Parallel()

	lowerAlias := values.NamedCorrelationIdentifier("record_lower")
	upperAlias := values.NamedCorrelationIdentifier("record_upper")
	recordType := values.NewRecordType(
		"",
		true,
		[]values.Field{{Name: "x", FieldType: values.NullableLong}},
	)
	lowerRecord := values.NewQuantifiedRecordValue(lowerAlias, recordType)
	candidateValue := values.NewFieldValue(
		lowerRecord,
		"x",
		values.NullableLong,
	)
	queryKey := values.LiteralValue(int64(1))
	matched := NewValueBiMap()
	matched.Put(queryKey, candidateValue)

	adjusted, ok := AdjustGroupByMappings(
		NewGroupByMappings(
			matched,
			NewValueBiMap(),
			NewCorrValueBiMap(),
		),
		upperAlias,
		expressions.NewSelectExpression(lowerRecord, nil, nil),
	)
	if !ok || adjusted == nil {
		t.Fatal("quantified-record passthrough was rejected")
	}
	pulled, present := adjusted.MatchedGroupingsMap().Get(queryKey)
	if !present {
		t.Fatal("quantified-record passthrough mapping is missing")
	}
	field, isField := pulled.(*values.FieldValue)
	if !isField {
		t.Fatalf("pulled value = %T, want FieldValue", pulled)
	}
	base, isRecord := field.Child.(*values.QuantifiedRecordValue)
	if !isRecord || base.Alias != upperAlias {
		t.Fatalf(
			"pulled field base = %T/%s, want QuantifiedRecordValue(%s)",
			field.Child,
			values.ExplainValue(field.Child),
			upperAlias,
		)
	}

	wholeRecordMap := NewValueBiMap()
	wholeRecordKey := values.LiteralValue(int64(2))
	wholeRecordMap.Put(wholeRecordKey, lowerRecord)
	wholeRecordAdjusted, ok := AdjustGroupByMappings(
		NewGroupByMappings(
			wholeRecordMap,
			NewValueBiMap(),
			NewCorrValueBiMap(),
		),
		upperAlias,
		expressions.NewSelectExpression(lowerRecord, nil, nil),
	)
	if !ok {
		t.Fatal("whole quantified-record passthrough was rejected")
	}
	wholeRecord, present := wholeRecordAdjusted.
		MatchedGroupingsMap().
		Get(wholeRecordKey)
	upperRecord, isRecord := wholeRecord.(*values.QuantifiedRecordValue)
	if !present || !isRecord || upperRecord.Alias != upperAlias {
		t.Fatalf(
			"whole-record pull-up = %T/%s, want QuantifiedRecordValue(%s)",
			wholeRecord,
			values.ExplainValue(wholeRecord),
			upperAlias,
		)
	}
}

// Java's CompensateRecordConstructorRule composes every constructor field on
// the route from a requested lower value to the upper result. A direct-only
// lookup loses valid aggregate/index matches when an intermediate constructor
// nests the value.
func TestMatchInfoGroupPullUp_ComposesNestedRecordConstructors(t *testing.T) {
	t.Parallel()

	candidateLowerAlias := values.NamedCorrelationIdentifier(
		"nested_candidate_lower",
	)
	candidateInput := values.NewQuantifiedObjectValue(candidateLowerAlias)
	candidateResult := values.NewRecordConstructorValue(
		values.RecordConstructorField{
			Name: "outer",
			Value: values.NewRecordConstructorValue(
				values.RecordConstructorField{
					Name:  "inner",
					Value: candidateInput,
				},
			),
		},
	)
	queryConstant := values.LiteralValue(int64(1904))
	fixture := newMatchInfoConstantPullUpFixture(
		"nested_constructor",
		queryConstant,
		candidateResult,
		matchInfoConstantPullUpMatchedGrouping(
			queryConstant,
			candidateInput,
		),
	)

	merged, ok := fixture.merge()
	if !ok || merged == nil {
		t.Fatal("nested constructor group mapping was rejected")
	}
	_, pulledCandidate := matchInfoConstantPullUpOnlyMatchedGrouping(t, merged)
	field, isField := pulledCandidate.(*values.FieldValue)
	if !isField || field.Resolved == nil {
		t.Fatalf(
			"nested constructor pull-up = %T/%s, want resolved FieldValue",
			pulledCandidate,
			values.ExplainValue(pulledCandidate),
		)
	}
	if got := len(field.Resolved.Accessors); got != 2 {
		t.Fatalf(
			"nested constructor path = %+v, want two accessors",
			field.Resolved,
		)
	}
	if field.Resolved.Accessors[0].Ordinal != 0 ||
		field.Resolved.Accessors[1].Ordinal != 0 {
		t.Fatalf(
			"nested constructor path = %+v, want ordinals [0 0]",
			field.Resolved,
		)
	}
	base, anchored := field.Child.(*values.QuantifiedObjectValue)
	if !anchored || base.Correlation != fixture.candidateAlias {
		t.Fatalf(
			"nested constructor field is not anchored to upper alias %s",
			fixture.candidateAlias,
		)
	}
}

// Java's MatchOrCompensateFieldValueRule strips the projected prefix and
// retains the remaining field path. Thus projecting $a.x still exposes
// $a.x.y as $upper.projected.y.
func TestAdjustGroupByMappings_ComposesProjectedFieldSuffix(t *testing.T) {
	t.Parallel()

	lowerAlias := values.NamedCorrelationIdentifier("suffix_lower")
	upperAlias := values.NamedCorrelationIdentifier("suffix_upper")
	lowerBase := values.NewQuantifiedObjectValue(lowerAlias)
	projectedPrefix := &values.FieldValue{
		Field:    "x",
		Typ:      values.UnknownType,
		Child:    lowerBase,
		Resolved: values.NewFieldPathOfSingle("x", 0, false),
	}
	requested := &values.FieldValue{
		Field: "y",
		Typ:   values.NullableLong,
		Child: lowerBase,
		Resolved: values.NewFieldPathOfSingle(
			"x",
			0,
			false,
		).WithSuffix(values.NewFieldPathOfSingle("y", 1, false)),
	}
	candidateResult := values.NewRecordConstructorValue(
		values.RecordConstructorField{
			Name:  "projected",
			Value: projectedPrefix,
		},
	)
	queryKey := values.LiteralValue(int64(1))
	matched := NewValueBiMap()
	matched.Put(queryKey, requested)

	adjusted, ok := AdjustGroupByMappings(
		NewGroupByMappings(
			matched,
			NewValueBiMap(),
			NewCorrValueBiMap(),
		),
		upperAlias,
		expressions.NewSelectExpression(candidateResult, nil, nil),
	)
	if !ok || adjusted == nil {
		t.Fatal("projected field suffix group mapping was rejected")
	}
	pulled, present := adjusted.MatchedGroupingsMap().Get(queryKey)
	if !present {
		t.Fatal("projected field suffix mapping was dropped")
	}
	field, isField := pulled.(*values.FieldValue)
	if !isField || field.Resolved == nil {
		t.Fatalf(
			"projected field suffix = %T/%s, want resolved FieldValue",
			pulled,
			values.ExplainValue(pulled),
		)
	}
	if got := len(field.Resolved.Accessors); got != 2 {
		t.Fatalf(
			"projected field suffix path = %+v, want two accessors",
			field.Resolved,
		)
	}
	if field.Resolved.Accessors[0].Ordinal != 0 ||
		field.Resolved.Accessors[1].Ordinal != 1 {
		t.Fatalf(
			"projected field suffix path = %+v, want ordinals [0 1]",
			field.Resolved,
		)
	}
	base, anchored := field.Child.(*values.QuantifiedObjectValue)
	if !anchored || base.Correlation != upperAlias {
		t.Fatalf(
			"projected field suffix is not anchored to upper alias %s",
			upperAlias,
		)
	}
}

func TestMatchInfoGroupPullUp_RejectsAmbiguousNestedConstructorPaths(t *testing.T) {
	t.Parallel()

	lowerAlias := values.NamedCorrelationIdentifier("ambiguous_nested_lower")
	upperAlias := values.NamedCorrelationIdentifier("ambiguous_nested_upper")
	input := values.NewQuantifiedObjectValue(lowerAlias)
	nested := func(name string) values.RecordConstructorField {
		return values.RecordConstructorField{
			Name: name,
			Value: values.NewRecordConstructorValue(
				values.RecordConstructorField{
					Name:  "value",
					Value: input,
				},
			),
		}
	}
	result := values.NewRecordConstructorValue(
		nested("left"),
		nested("right"),
	)

	pulled, ok := pullUpGroupByValue(
		input,
		result,
		upperAlias,
		nil,
	)
	if ok || pulled != nil {
		t.Fatalf(
			"ambiguous nested pull-up = %s, ok=%v; want rejection",
			values.ExplainValue(pulled),
			ok,
		)
	}
}

func TestMatchInfoGroupPullUp_CancelsProjectedConstructorField(t *testing.T) {
	t.Parallel()

	lowerAlias := values.NamedCorrelationIdentifier(
		"constructor_field_lower",
	)
	upperAlias := values.NamedCorrelationIdentifier(
		"constructor_field_upper",
	)
	lowerBase := values.NewQuantifiedObjectValue(lowerAlias)
	requested := &values.FieldValue{
		Field:    "y",
		Typ:      values.NullableLong,
		Child:    lowerBase,
		Resolved: values.NewFieldPathOfSingle("y", 1, false),
	}
	constructor := values.NewRecordConstructorValue(
		values.RecordConstructorField{
			Name:  "x",
			Value: lowerBase,
		},
	)
	result := &values.FieldValue{
		Field:    "x",
		Typ:      lowerBase.Type(),
		Child:    constructor,
		Resolved: values.NewFieldPathOfSingle("x", 0, false),
	}

	pulled, ok := pullUpGroupByValue(
		requested,
		result,
		upperAlias,
		nil,
	)
	if !ok || pulled == nil {
		t.Fatal("constructor-field compensation cancellation was dropped")
	}
	field, isField := pulled.(*values.FieldValue)
	if !isField || field.Resolved == nil {
		t.Fatalf(
			"constructor-field pull-up = %T/%s, want resolved FieldValue",
			pulled,
			values.ExplainValue(pulled),
		)
	}
	accessor, single := field.Resolved.Single()
	if !single || accessor.Ordinal != 1 {
		t.Fatalf(
			"constructor-field path = %+v, want downstream ordinal [1]",
			field.Resolved,
		)
	}
	base, anchored := field.Child.(*values.QuantifiedObjectValue)
	if !anchored || base.Correlation != upperAlias {
		t.Fatalf(
			"constructor-field pull-up is not anchored to upper alias %s",
			upperAlias,
		)
	}
}
