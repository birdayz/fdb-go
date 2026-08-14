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
	t testing.TB,
	name string,
	queryResult values.Value,
	candidateResult values.Value,
	groupByMappings *GroupByMappings,
) matchInfoConstantPullUpFixture {
	t.Helper()
	queryExpression := matchInfoConstantPullUpSelect(t, queryResult, nil)
	candidateExpression := matchInfoConstantPullUpSelect(t, candidateResult, nil)
	return newMatchInfoConstantPullUpExpressionFixture(
		name,
		queryExpression,
		candidateExpression,
		groupByMappings,
	)
}

func matchInfoConstantPullUpSelect(
	t testing.TB,
	result values.Value,
	quantifiers []expressions.Quantifier,
) *expressions.SelectExpression {
	t.Helper()
	selectExpression, err := expressions.NewSelectExpression(result, quantifiers, nil)
	return mustConstruct(t, selectExpression, err)
}

func matchInfoConstantPullUpQOV(
	t testing.TB,
	alias values.CorrelationIdentifier,
	typ values.Type,
) values.QuantifiedObjectValue {
	t.Helper()
	qov, err := values.NewQuantifiedObjectValue(alias, typ)
	return mustConstruct(t, qov, err)
}

func matchInfoConstantPullUpField(
	t testing.TB,
	alias values.CorrelationIdentifier,
	rowType values.Type,
	ordinals ...int,
) values.Value {
	t.Helper()
	root := matchInfoConstantPullUpQOV(t, alias, rowType)
	field, err := values.ResolveFieldOrdinals(root, ordinals)
	return mustConstruct(t, field, err)
}

func matchInfoConstantPullUpLong(value int64) values.Value {
	return &values.ConstantValue{Value: value, Typ: values.NotNullLong}
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

// A query value rooted at an alias that is both projected by the lower result
// and external to the lower expression is constant during pull-up. The exact
// access path does not have to occur in the result: outer.y remains outer.y
// even when the result projects only outer.x. The candidate value still takes
// the ordinary result-field pull-up path.
func TestMatchInfoGroupPullUp_PreservesMatchedQueryConstantAlias(t *testing.T) {
	t.Parallel()

	outerAlias := values.NamedCorrelationIdentifier("matched_query_outer")
	outerType := values.NewRecordType("Outer", false, []values.Field{
		{Name: "x", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "y", FieldType: values.NullableLong, Ordinal: 1},
	})
	outerX := matchInfoConstantPullUpField(t, outerAlias, outerType, 0)
	outerY := matchInfoConstantPullUpField(t, outerAlias, outerType, 1)
	queryResult := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "projected_x", Value: outerX},
	)

	candidateLowerAlias := values.NamedCorrelationIdentifier(
		"matched_query_candidate_lower",
	)
	candidateLowerType := values.NewRecordType("CandidateLower", false, []values.Field{
		{Name: "candidate_input", FieldType: values.NullableLong, Ordinal: 0},
	})
	candidateInput := matchInfoConstantPullUpField(
		t,
		candidateLowerAlias,
		candidateLowerType,
		0,
	)
	candidateResult := values.NewRecordConstructorValue(
		values.RecordConstructorField{
			Name:  "candidate_projected",
			Value: candidateInput,
		},
	)
	fixture := newMatchInfoConstantPullUpFixture(
		t,
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
	candidateField, ok := values.AsFieldValue(pulledCandidate)
	if !ok || candidateField.DisplayName() != "candidate_projected" {
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

	queryInput := matchInfoConstantPullUpLong(202)
	queryResult := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "query_projected", Value: queryInput},
	)

	candidateConstantAlias := values.NamedCorrelationIdentifier(
		"matched_candidate_constant",
	)
	candidateConstant := matchInfoConstantPullUpQOV(t, candidateConstantAlias, values.NotNullLong)
	candidateResult := matchInfoConstantPullUpLong(303)
	fixture := newMatchInfoConstantPullUpFixture(
		t,
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
	queryResult := matchInfoConstantPullUpQOV(t, resultAlias, values.NotNullLong)
	unmatchedValue := matchInfoConstantPullUpQOV(t, unmatchedRootAlias, values.NotNullLong)
	unmatchedAggregateAlias := values.NamedCorrelationIdentifier(
		"unmatched_aggregate",
	)
	fixture := newMatchInfoConstantPullUpFixture(
		t,
		"unmatched_query_root_constant",
		queryResult,
		matchInfoConstantPullUpLong(404),
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

	queryInput := matchInfoConstantPullUpLong(505)
	queryResult := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "query", Value: queryInput},
	)

	candidateLowerAlias := values.NamedCorrelationIdentifier(
		"ambiguous_candidate_lower",
	)
	candidateLowerType := values.NewRecordType("AmbiguousLower", false, []values.Field{
		{Name: "input", FieldType: values.NullableLong, Ordinal: 0},
	})
	candidateInput := matchInfoConstantPullUpField(t, candidateLowerAlias, candidateLowerType, 0)
	candidateResult := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "first", Value: candidateInput},
		values.RecordConstructorField{Name: "second", Value: candidateInput},
	)
	if got := len(candidateResult.Fields); got != 2 {
		t.Fatalf("ambiguous result fixture has %d fields, want 2", got)
	}
	fixture := newMatchInfoConstantPullUpFixture(
		t,
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
	queryLowerType := values.NewRecordType("PassthroughLower", false, []values.Field{
		{Name: "input", FieldType: values.NullableLong, Ordinal: 0},
	})
	queryLowerRef := expressions.InitialOf(
		mustFullUnorderedScan(t, []string{"T"}, queryLowerType),
	)
	queryLowerQ := expressions.NamedForEachQuantifier(
		queryLowerAlias,
		queryLowerRef,
	)
	queryResult, queryResultErr := queryLowerQ.RequireFlowedObjectValue()
	queryResult = mustConstruct(t, queryResult, queryResultErr)
	queryInput, queryInputErr := values.ResolveFieldOrdinals(queryResult, []int{0})
	queryInput = mustConstruct(t, queryInput, queryInputErr)
	queryExpression := matchInfoConstantPullUpSelect(
		t,
		queryResult,
		[]expressions.Quantifier{queryLowerQ},
	)

	candidateInput := matchInfoConstantPullUpLong(606)
	candidateResult := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "candidate", Value: candidateInput},
	)
	candidateExpression := matchInfoConstantPullUpSelect(
		t,
		candidateResult,
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
	pulledField, isField := values.AsFieldValue(pulledQuery)
	if !isField {
		t.Fatalf(
			"passthrough pull-up returned %T, want anchored FieldValue or rejection",
			pulledQuery,
		)
	}
	root, anchored := values.AsQuantifiedObjectValue(pulledField.ChildValue())
	if !anchored || root.Correlation() != fixture.queryAlias {
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
	lowerType := values.NewRecordType("AdjustLower", false, []values.Field{
		{Name: "input", FieldType: values.NullableLong, Ordinal: 0},
	})
	candidateInput := matchInfoConstantPullUpField(t, lowerAlias, lowerType, 0)
	candidateConstant := matchInfoConstantPullUpQOV(t, constantAlias, values.NotNullLong)
	candidateResult := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "projected", Value: candidateInput},
	)
	candidateExpression := matchInfoConstantPullUpSelect(
		t,
		candidateResult,
		nil,
	)

	ordinaryQueryKey := matchInfoConstantPullUpLong(1)
	constantQueryKey := matchInfoConstantPullUpLong(2)
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
	ordinaryField, isField := values.AsFieldValue(ordinary)
	if !isField || ordinaryField.Path() == nil {
		t.Fatalf("ordinary adjusted value = %T, want ordinal-resolved FieldValue", ordinary)
	}
	ordinals := ordinaryField.Path().Ordinals()
	if len(ordinals) != 1 || ordinals[0] != 0 {
		t.Fatalf("ordinary adjusted path = %+v, want output ordinal 0", ordinaryField.Path())
	}
	ordinaryBase, anchored := values.AsQuantifiedObjectValue(ordinaryField.ChildValue())
	if !anchored || ordinaryBase.Correlation() != upperAlias {
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
	// RFC-232 admits field access only over an exact QOV (or an already
	// admitted FieldValue/record constructor). The old FieldValue(QRV, "x")
	// fixture cannot be published, so pin that boundary before exercising the
	// still-supported whole queried-record passthrough below.
	if invalid, err := values.ResolveFieldOrdinals(lowerRecord, []int{0}); err == nil || invalid != nil {
		t.Fatalf("QuantifiedRecordValue unexpectedly published a FieldValue: (%v, %v)", invalid, err)
	}

	wholeRecordMap := NewValueBiMap()
	wholeRecordKey := matchInfoConstantPullUpLong(2)
	wholeRecordMap.Put(wholeRecordKey, lowerRecord)
	candidateExpression := matchInfoConstantPullUpSelect(t, lowerRecord, nil)
	wholeRecordAdjusted, ok := AdjustGroupByMappings(
		NewGroupByMappings(
			wholeRecordMap,
			NewValueBiMap(),
			NewCorrValueBiMap(),
		),
		upperAlias,
		candidateExpression,
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
	candidateInput := matchInfoConstantPullUpQOV(t, candidateLowerAlias, values.NotNullLong)
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
	queryConstant := matchInfoConstantPullUpLong(1904)
	fixture := newMatchInfoConstantPullUpFixture(
		t,
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
	field, isField := values.AsFieldValue(pulledCandidate)
	if !isField || field.Path() == nil {
		t.Fatalf(
			"nested constructor pull-up = %T/%s, want resolved FieldValue",
			pulledCandidate,
			values.ExplainValue(pulledCandidate),
		)
	}
	ordinals := field.Path().Ordinals()
	if got := len(ordinals); got != 2 {
		t.Fatalf(
			"nested constructor path = %+v, want two accessors",
			field.Path(),
		)
	}
	if ordinals[0] != 0 || ordinals[1] != 0 {
		t.Fatalf(
			"nested constructor path = %+v, want ordinals [0 0]",
			field.Path(),
		)
	}
	base, anchored := values.AsQuantifiedObjectValue(field.ChildValue())
	if !anchored || base.Correlation() != fixture.candidateAlias {
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
	xType := values.NewRecordType("X", false, []values.Field{
		{Name: "padding", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "y", FieldType: values.NullableLong, Ordinal: 1},
	})
	lowerType := values.NewRecordType("SuffixLower", false, []values.Field{
		{Name: "x", FieldType: xType, Ordinal: 0},
	})
	projectedPrefix := matchInfoConstantPullUpField(t, lowerAlias, lowerType, 0)
	requested := matchInfoConstantPullUpField(t, lowerAlias, lowerType, 0, 1)
	candidateResult := values.NewRecordConstructorValue(
		values.RecordConstructorField{
			Name:  "projected",
			Value: projectedPrefix,
		},
	)
	queryKey := matchInfoConstantPullUpLong(1)
	matched := NewValueBiMap()
	matched.Put(queryKey, requested)

	adjusted, ok := AdjustGroupByMappings(
		NewGroupByMappings(
			matched,
			NewValueBiMap(),
			NewCorrValueBiMap(),
		),
		upperAlias,
		matchInfoConstantPullUpSelect(t, candidateResult, nil),
	)
	if !ok || adjusted == nil {
		t.Fatal("projected field suffix group mapping was rejected")
	}
	pulled, present := adjusted.MatchedGroupingsMap().Get(queryKey)
	if !present {
		t.Fatal("projected field suffix mapping was dropped")
	}
	field, isField := values.AsFieldValue(pulled)
	if !isField || field.Path() == nil {
		t.Fatalf(
			"projected field suffix = %T/%s, want resolved FieldValue",
			pulled,
			values.ExplainValue(pulled),
		)
	}
	ordinals := field.Path().Ordinals()
	if got := len(ordinals); got != 2 {
		t.Fatalf(
			"projected field suffix path = %+v, want two accessors",
			field.Path(),
		)
	}
	if ordinals[0] != 0 || ordinals[1] != 1 {
		t.Fatalf(
			"projected field suffix path = %+v, want ordinals [0 1]",
			field.Path(),
		)
	}
	base, anchored := values.AsQuantifiedObjectValue(field.ChildValue())
	if !anchored || base.Correlation() != upperAlias {
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
	input := matchInfoConstantPullUpQOV(t, lowerAlias, values.NotNullLong)
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
	lowerType := values.NewRecordType("ConstructorFieldLower", false, []values.Field{
		{Name: "x", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "y", FieldType: values.NullableLong, Ordinal: 1},
	})
	lowerBase := matchInfoConstantPullUpQOV(t, lowerAlias, lowerType)
	requested := matchInfoConstantPullUpField(t, lowerAlias, lowerType, 1)
	constructor := values.NewRecordConstructorValue(
		values.RecordConstructorField{
			Name:  "x",
			Value: lowerBase,
		},
	)
	// Resolving a constructor field is atomic: selecting the field collapses
	// directly to its exact child rather than publishing a FieldValue rooted at
	// a record constructor.
	result, resultErr := values.ResolveFieldOrdinals(constructor, []int{0})
	result = mustConstruct(t, result, resultErr)
	if result != lowerBase {
		t.Fatalf("constructor-field resolution returned %T, want the selected QOV", result)
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
	field, isField := values.AsFieldValue(pulled)
	if !isField || field.Path() == nil {
		t.Fatalf(
			"constructor-field pull-up = %T/%s, want resolved FieldValue",
			pulled,
			values.ExplainValue(pulled),
		)
	}
	ordinals := field.Path().Ordinals()
	if len(ordinals) != 1 || ordinals[0] != 1 {
		t.Fatalf(
			"constructor-field path = %+v, want downstream ordinal [1]",
			field.Path(),
		)
	}
	base, anchored := values.AsQuantifiedObjectValue(field.ChildValue())
	if !anchored || base.Correlation() != upperAlias {
		t.Fatalf(
			"constructor-field pull-up is not anchored to upper alias %s",
			upperAlias,
		)
	}
}
