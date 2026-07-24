package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func selectTranslationTestReference() (
	expressions.RelationalExpression,
	*expressions.Reference,
) {
	expression := expressions.NewFullUnorderedScanExpression(
		[]string{"T"},
		values.UnknownType,
	)
	return expression, expressions.InitialOf(expression)
}

func selectTranslationTestRegularMatchInfo(
	maxMatchMap *MaxMatchMap,
) *RegularMatchInfo {
	return NewRegularMatchInfo(
		nil,
		EmptyAliasMap(),
		nil,
		nil,
		maxMatchMap,
		EmptyGroupByMappings(),
		nil,
		nil,
	)
}

func selectTranslationTestPartialMatch(
	matchInfo MatchInfo,
	queryRef *expressions.Reference,
	candidateRef *expressions.Reference,
) *PartialMatchImpl {
	queryMembers := queryRef.AllMembers()
	return NewPartialMatch(
		EmptyAliasMap(),
		nil,
		queryRef,
		queryMembers[0],
		candidateRef,
		matchInfo,
	)
}

func selectTranslationTestValidMaxMatchMap() *MaxMatchMap {
	queryPart := values.NewFlatFieldValue("QUERY_PART", values.NotNullLong)
	candidatePart := values.NewFlatFieldValue("CANDIDATE_PART", values.NotNullLong)
	return NewMaxMatchMap(
		map[values.Value]values.Value{queryPart: candidatePart},
		queryPart,
		candidatePart,
	)
}

func selectTranslationTestAliasMap(
	t *testing.T,
	pairs ...[2]values.CorrelationIdentifier,
) *AliasMap {
	t.Helper()
	builder := NewAliasMapBuilder()
	for _, pair := range pairs {
		if !builder.PutChecked(pair[0], pair[1]) {
			t.Fatalf(
				"failed to bind %s to %s",
				pair[0].Name(),
				pair[1].Name(),
			)
		}
	}
	return builder.Build()
}

func TestBuildSelectSubsumptionTranslationMap_ForEachUsesAdjustedChildMap(
	t *testing.T,
) {
	t.Parallel()

	_, queryChildRef := selectTranslationTestReference()
	_, candidateChildRef := selectTranslationTestReference()
	queryAlias := values.NamedCorrelationIdentifier("query_fe")
	candidateAlias := values.NamedCorrelationIdentifier("candidate_fe")
	queryQuantifier := expressions.NamedForEachQuantifier(queryAlias, queryChildRef)
	candidateQuantifier := expressions.NamedForEachQuantifier(candidateAlias, candidateChildRef)

	unpullable := NewMaxMatchMap(
		nil,
		values.NewFlatFieldValue("Q", values.NotNullLong),
		values.NewFlatFieldValue("C", values.NotNullLong),
	)
	regular := selectTranslationTestRegularMatchInfo(unpullable)
	adjusted := NewAdjustedMatchInfo(
		regular,
		nil,
		selectTranslationTestValidMaxMatchMap(),
		EmptyGroupByMappings(),
	)
	child := selectTranslationTestPartialMatch(
		adjusted,
		queryChildRef,
		candidateChildRef,
	)

	translation, ok := buildSelectSubsumptionTranslationMapMaybe(
		[]expressions.Quantifier{queryQuantifier},
		[]expressions.Quantifier{candidateQuantifier},
		[]quantifierMapping{{queryIndex: 0, candidateIndex: 0}},
		[]quantifierPartialMatch{{
			quantifier:   queryQuantifier,
			partialMatch: child,
		}},
		AliasMapOfAliases(queryAlias, candidateAlias),
	)
	if !ok {
		t.Fatal("adjusted child MaxMatchMap did not produce a translation")
	}
	translated := values.TranslateCorrelations(
		values.NewQuantifiedObjectValue(queryAlias),
		translation,
	)
	qov, ok := translated.(*values.QuantifiedObjectValue)
	if !ok || qov.Correlation != candidateAlias {
		t.Fatalf(
			"translated ForEach value = %T/%v, want QOV(%s)",
			translated,
			translated,
			candidateAlias.Name(),
		)
	}
}

func TestBuildSelectSubsumptionTranslationMap_ForwardedReferencesCanonicalize(
	t *testing.T,
) {
	t.Parallel()

	_, queryCanonicalRef := selectTranslationTestReference()
	_, queryForwardedRef := selectTranslationTestReference()
	_, candidateCanonicalRef := selectTranslationTestReference()
	_, candidateForwardedRef := selectTranslationTestReference()
	queryCanonicalRef.Absorb(queryForwardedRef)
	candidateCanonicalRef.Absorb(candidateForwardedRef)

	queryAlias := values.NamedCorrelationIdentifier("query_forwarded")
	candidateAlias := values.NamedCorrelationIdentifier("candidate_forwarded")
	queryQuantifier := expressions.NamedExistentialQuantifier(
		queryAlias,
		queryCanonicalRef,
	)
	childQuantifier := expressions.NamedExistentialQuantifier(
		queryAlias,
		queryForwardedRef,
	)
	candidateQuantifier := expressions.NamedExistentialQuantifier(
		candidateAlias,
		candidateCanonicalRef,
	)
	child := selectTranslationTestPartialMatch(
		selectTranslationTestRegularMatchInfo(nil),
		queryForwardedRef,
		candidateForwardedRef,
	)

	translation, ok := buildSelectSubsumptionTranslationMapMaybe(
		[]expressions.Quantifier{queryQuantifier},
		[]expressions.Quantifier{candidateQuantifier},
		[]quantifierMapping{{queryIndex: 0, candidateIndex: 0}},
		[]quantifierPartialMatch{{
			quantifier:   childQuantifier,
			partialMatch: child,
		}},
		AliasMapOfAliases(queryAlias, candidateAlias),
	)
	if !ok || translation == nil {
		t.Fatal("canonically aligned forwarded references were rejected")
	}
	if !translation.DefinesOnlyIdentities() {
		t.Fatal("E-to-E forwarded-reference translation should be empty")
	}
}

func TestBuildSelectSubsumptionTranslationMap_MissingOrUnpullableFailsClosed(
	t *testing.T,
) {
	t.Parallel()

	_, queryChildRef := selectTranslationTestReference()
	_, candidateChildRef := selectTranslationTestReference()
	queryAlias := values.NamedCorrelationIdentifier("query_fe")
	candidateAlias := values.NamedCorrelationIdentifier("candidate_fe")
	queryQuantifier := expressions.NamedForEachQuantifier(queryAlias, queryChildRef)
	candidateQuantifier := expressions.NamedForEachQuantifier(candidateAlias, candidateChildRef)
	binding := AliasMapOfAliases(queryAlias, candidateAlias)
	var typedNilRegular *RegularMatchInfo
	var typedNilValue *values.QuantifiedObjectValue
	nestedTypedNilValue := &values.FieldValue{
		Field: "nested_bad",
		Typ:   values.UnknownType,
		Child: typedNilValue,
	}
	validQueryPart := values.NewFlatFieldValue(
		"Q",
		values.NotNullLong,
	)
	validCandidatePart := values.NewFlatFieldValue(
		"C",
		values.NotNullLong,
	)

	tests := []struct {
		name         string
		maxMatchMap  *MaxMatchMap
		partialMatch PartialMatch
	}{
		{
			name:        "missing max-match map",
			maxMatchMap: nil,
		},
		{
			name: "unpullable max-match map",
			maxMatchMap: NewMaxMatchMap(
				nil,
				validQueryPart,
				validCandidatePart,
			),
		},
		{
			name: "nested typed-nil max-match query entry",
			maxMatchMap: &MaxMatchMap{
				mapping: map[string]maxMatchEntry{
					"malformed": {
						queryValue:     nestedTypedNilValue,
						candidateValue: validCandidatePart,
					},
				},
				queryValue:     validQueryPart,
				candidateValue: validCandidatePart,
			},
		},
		{
			name: "nested typed-nil max-match candidate entry",
			maxMatchMap: &MaxMatchMap{
				mapping: map[string]maxMatchEntry{
					"malformed": {
						queryValue:     validQueryPart,
						candidateValue: nestedTypedNilValue,
					},
				},
				queryValue:     validQueryPart,
				candidateValue: validCandidatePart,
			},
		},
		{
			name:         "missing partial match",
			partialMatch: nil,
		},
		{
			name: "typed-nil match info",
			partialMatch: selectTranslationTestPartialMatch(
				typedNilRegular,
				queryChildRef,
				candidateChildRef,
			),
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf(
						"invalid child value tree panicked instead of failing closed: %v",
						recovered,
					)
				}
			}()
			partialMatch := test.partialMatch
			if partialMatch == nil && test.name != "missing partial match" {
				partialMatch = selectTranslationTestPartialMatch(
					selectTranslationTestRegularMatchInfo(
						test.maxMatchMap,
					),
					queryChildRef,
					candidateChildRef,
				)
			}
			if translation, ok := buildSelectSubsumptionTranslationMapMaybe(
				[]expressions.Quantifier{queryQuantifier},
				[]expressions.Quantifier{candidateQuantifier},
				[]quantifierMapping{{
					queryIndex:     0,
					candidateIndex: 0,
				}},
				[]quantifierPartialMatch{{
					quantifier:   queryQuantifier,
					partialMatch: partialMatch,
				}},
				binding,
			); ok || translation != nil {
				t.Fatalf(
					"invalid child returned translation=%T, ok=%v",
					translation,
					ok,
				)
			}
		})
	}
}

func TestBuildSelectSubsumptionTranslationMap_ExistentialTargets(
	t *testing.T,
) {
	t.Parallel()

	_, queryChildRef := selectTranslationTestReference()
	_, candidateChildRef := selectTranslationTestReference()
	queryAlias := values.NamedCorrelationIdentifier("query_exists")
	queryQuantifier := expressions.NamedExistentialQuantifier(queryAlias, queryChildRef)
	child := selectTranslationTestPartialMatch(
		selectTranslationTestRegularMatchInfo(nil),
		queryChildRef,
		candidateChildRef,
	)
	children := []quantifierPartialMatch{{
		quantifier:   queryQuantifier,
		partialMatch: child,
	}}
	mapping := []quantifierMapping{{queryIndex: 0, candidateIndex: 0}}

	t.Run("existential target is omitted", func(t *testing.T) {
		candidateAlias := values.NamedCorrelationIdentifier("candidate_exists")
		candidateQuantifier := expressions.NamedExistentialQuantifier(
			candidateAlias,
			candidateChildRef,
		)
		translation, ok := buildSelectSubsumptionTranslationMapMaybe(
			[]expressions.Quantifier{queryQuantifier},
			[]expressions.Quantifier{candidateQuantifier},
			mapping,
			children,
			AliasMapOfAliases(queryAlias, candidateAlias),
		)
		if !ok || translation == nil {
			t.Fatal("E-to-E mapping should yield an empty translation map")
		}
		if translation.ContainsSourceAlias(queryAlias) ||
			!translation.DefinesOnlyIdentities() {
			t.Fatal("E-to-E target must be omitted from translation")
		}
		original := values.NewQuantifiedObjectValue(queryAlias)
		if translated := values.TranslateCorrelations(
			original,
			translation,
		); translated != original {
			t.Fatal("omitted E-to-E translation changed the query value")
		}
	})

	t.Run("ForEach target uses dormant direct rebase", func(t *testing.T) {
		candidateAlias := values.NamedCorrelationIdentifier("candidate_fe")
		candidateQuantifier := expressions.NamedForEachQuantifier(
			candidateAlias,
			candidateChildRef,
		)
		translation, ok := buildSelectSubsumptionTranslationMapMaybe(
			[]expressions.Quantifier{queryQuantifier},
			[]expressions.Quantifier{candidateQuantifier},
			mapping,
			children,
			AliasMapOfAliases(queryAlias, candidateAlias),
		)
		if !ok || !translation.ContainsSourceAlias(queryAlias) {
			t.Fatal("dormant E-to-FE mapping did not compose direct rebase")
		}
		translated := values.TranslateCorrelations(
			values.NewQuantifiedObjectValue(queryAlias),
			translation,
		)
		qov, ok := translated.(*values.QuantifiedObjectValue)
		if !ok || qov.Correlation != candidateAlias {
			t.Fatalf(
				"E-to-FE translation = %T/%v, want QOV(%s)",
				translated,
				translated,
				candidateAlias.Name(),
			)
		}
	})
}

func TestBuildSelectSubsumptionTranslationMap_MalformedMappingsFailClosed(
	t *testing.T,
) {
	t.Parallel()

	_, queryChildRef := selectTranslationTestReference()
	_, candidateChildRef := selectTranslationTestReference()
	_, otherQueryChildRef := selectTranslationTestReference()
	_, otherCandidateChildRef := selectTranslationTestReference()
	queryAlias := values.NamedCorrelationIdentifier("query")
	otherQueryAlias := values.NamedCorrelationIdentifier("other_query")
	candidateAliasOne := values.NamedCorrelationIdentifier("candidate_one")
	candidateAliasTwo := values.NamedCorrelationIdentifier("candidate_two")
	queryQuantifier := expressions.NamedExistentialQuantifier(queryAlias, queryChildRef)
	nilRangedQueryQuantifier := expressions.NamedExistentialQuantifier(queryAlias, nil)
	otherQueryQuantifier := expressions.NamedExistentialQuantifier(
		otherQueryAlias,
		queryChildRef,
	)
	misrangedQueryQuantifier := expressions.NamedExistentialQuantifier(
		queryAlias,
		otherQueryChildRef,
	)
	candidateQuantifiers := []expressions.Quantifier{
		expressions.NamedExistentialQuantifier(
			candidateAliasOne,
			candidateChildRef,
		),
		expressions.NamedForEachQuantifier(
			candidateAliasTwo,
			candidateChildRef,
		),
	}
	nilRangedCandidateQuantifiers := []expressions.Quantifier{
		expressions.NamedExistentialQuantifier(
			candidateAliasOne,
			nil,
		),
	}
	child := selectTranslationTestPartialMatch(
		selectTranslationTestRegularMatchInfo(nil),
		queryChildRef,
		candidateChildRef,
	)
	wrongQueryRefChild := selectTranslationTestPartialMatch(
		selectTranslationTestRegularMatchInfo(nil),
		otherQueryChildRef,
		candidateChildRef,
	)
	wrongCandidateRefChild := selectTranslationTestPartialMatch(
		selectTranslationTestRegularMatchInfo(nil),
		queryChildRef,
		otherCandidateChildRef,
	)
	nilQueryRefChildValue := *child
	nilQueryRefChildValue.queryRef = nil
	nilCandidateRefChildValue := *child
	nilCandidateRefChildValue.candidateRef = nil

	tests := []struct {
		name        string
		queryQs     []expressions.Quantifier
		candidateQs []expressions.Quantifier
		mapping     []quantifierMapping
		children    []quantifierPartialMatch
		bindingMap  *AliasMap
	}{
		{
			name:    "duplicate query index",
			queryQs: []expressions.Quantifier{queryQuantifier},
			mapping: []quantifierMapping{
				{queryIndex: 0, candidateIndex: 0},
				{queryIndex: 0, candidateIndex: 1},
			},
			children: []quantifierPartialMatch{
				{quantifier: queryQuantifier, partialMatch: child},
				{quantifier: queryQuantifier, partialMatch: child},
			},
			bindingMap: AliasMapOfAliases(
				queryAlias,
				candidateAliasOne,
			),
		},
		{
			name:    "misaligned child quantifier",
			queryQs: []expressions.Quantifier{queryQuantifier},
			mapping: []quantifierMapping{{
				queryIndex:     0,
				candidateIndex: 0,
			}},
			children: []quantifierPartialMatch{{
				quantifier:   otherQueryQuantifier,
				partialMatch: child,
			}},
			bindingMap: AliasMapOfAliases(
				queryAlias,
				candidateAliasOne,
			),
		},
		{
			name:    "misaligned quantifier ranges-over",
			queryQs: []expressions.Quantifier{queryQuantifier},
			mapping: []quantifierMapping{{
				queryIndex:     0,
				candidateIndex: 0,
			}},
			children: []quantifierPartialMatch{{
				quantifier:   misrangedQueryQuantifier,
				partialMatch: child,
			}},
			bindingMap: AliasMapOfAliases(
				queryAlias,
				candidateAliasOne,
			),
		},
		{
			name:    "misaligned child query ref",
			queryQs: []expressions.Quantifier{queryQuantifier},
			mapping: []quantifierMapping{{
				queryIndex:     0,
				candidateIndex: 0,
			}},
			children: []quantifierPartialMatch{{
				quantifier:   queryQuantifier,
				partialMatch: wrongQueryRefChild,
			}},
			bindingMap: AliasMapOfAliases(
				queryAlias,
				candidateAliasOne,
			),
		},
		{
			name:    "misaligned child candidate ref",
			queryQs: []expressions.Quantifier{queryQuantifier},
			mapping: []quantifierMapping{{
				queryIndex:     0,
				candidateIndex: 0,
			}},
			children: []quantifierPartialMatch{{
				quantifier:   queryQuantifier,
				partialMatch: wrongCandidateRefChild,
			}},
			bindingMap: AliasMapOfAliases(
				queryAlias,
				candidateAliasOne,
			),
		},
		{
			name:    "nil query ranges-over",
			queryQs: []expressions.Quantifier{nilRangedQueryQuantifier},
			mapping: []quantifierMapping{{
				queryIndex:     0,
				candidateIndex: 0,
			}},
			children: []quantifierPartialMatch{{
				quantifier:   nilRangedQueryQuantifier,
				partialMatch: child,
			}},
			bindingMap: AliasMapOfAliases(
				queryAlias,
				candidateAliasOne,
			),
		},
		{
			name:        "nil candidate ranges-over",
			queryQs:     []expressions.Quantifier{queryQuantifier},
			candidateQs: nilRangedCandidateQuantifiers,
			mapping: []quantifierMapping{{
				queryIndex:     0,
				candidateIndex: 0,
			}},
			children: []quantifierPartialMatch{{
				quantifier:   queryQuantifier,
				partialMatch: child,
			}},
			bindingMap: AliasMapOfAliases(
				queryAlias,
				candidateAliasOne,
			),
		},
		{
			name:    "nil child query ref",
			queryQs: []expressions.Quantifier{queryQuantifier},
			mapping: []quantifierMapping{{
				queryIndex:     0,
				candidateIndex: 0,
			}},
			children: []quantifierPartialMatch{{
				quantifier:   queryQuantifier,
				partialMatch: &nilQueryRefChildValue,
			}},
			bindingMap: AliasMapOfAliases(
				queryAlias,
				candidateAliasOne,
			),
		},
		{
			name:    "nil child candidate ref",
			queryQs: []expressions.Quantifier{queryQuantifier},
			mapping: []quantifierMapping{{
				queryIndex:     0,
				candidateIndex: 0,
			}},
			children: []quantifierPartialMatch{{
				quantifier:   queryQuantifier,
				partialMatch: &nilCandidateRefChildValue,
			}},
			bindingMap: AliasMapOfAliases(
				queryAlias,
				candidateAliasOne,
			),
		},
		{
			name: "missing exact alias binding",
			queryQs: []expressions.Quantifier{
				queryQuantifier,
			},
			mapping: []quantifierMapping{{
				queryIndex:     0,
				candidateIndex: 0,
			}},
			children: []quantifierPartialMatch{{
				quantifier:   queryQuantifier,
				partialMatch: child,
			}},
			bindingMap: EmptyAliasMap(),
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			testCandidateQuantifiers := test.candidateQs
			if testCandidateQuantifiers == nil {
				testCandidateQuantifiers = candidateQuantifiers
			}
			if translation, ok := buildSelectSubsumptionTranslationMapMaybe(
				test.queryQs,
				testCandidateQuantifiers,
				test.mapping,
				test.children,
				test.bindingMap,
			); ok || translation != nil {
				t.Fatalf(
					"malformed mapping returned translation=%T, ok=%v",
					translation,
					ok,
				)
			}
		})
	}
}

type countingSelectTranslationMap struct {
	delegate values.TranslationMap
	applied  int
}

func (m *countingSelectTranslationMap) ContainsSourceAlias(
	alias values.CorrelationIdentifier,
) bool {
	return m.delegate.ContainsSourceAlias(alias)
}

func (m *countingSelectTranslationMap) ApplyTranslationFunction(
	alias values.CorrelationIdentifier,
	leaf values.Value,
) values.Value {
	m.applied++
	return m.delegate.ApplyTranslationFunction(alias, leaf)
}

func (m *countingSelectTranslationMap) DefinesOnlyIdentities() bool {
	return m.delegate.DefinesOnlyIdentities()
}

func TestTranslateSelectSubsumptionInputs_MalformedExistentialOverForEachFailsClosed(
	t *testing.T,
) {
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf(
				"malformed existential-over-ForEach panicked: %v",
				recovered,
			)
		}
	}()

	_, queryChildRef := selectTranslationTestReference()
	_, candidateChildRef := selectTranslationTestReference()
	queryAlias := values.NamedCorrelationIdentifier("query_fe_evp")
	candidateAlias := values.NamedCorrelationIdentifier("candidate_fe_evp")
	queryQuantifier := expressions.NamedForEachQuantifier(queryAlias, queryChildRef)
	candidateQuantifier := expressions.NamedForEachQuantifier(
		candidateAlias,
		candidateChildRef,
	)

	queryPart := values.NewFlatFieldValue("QUERY_PART", values.NotNullLong)
	candidatePart := values.NewFlatFieldValue("CANDIDATE_PART", values.NotNullLong)
	candidateRoot := values.NewRecordConstructorValue(
		values.RecordConstructorField{
			Name:  "projected",
			Value: candidatePart,
		},
	)
	childMaxMatchMap := NewMaxMatchMap(
		map[values.Value]values.Value{queryPart: candidatePart},
		queryPart,
		candidateRoot,
	)
	pulledUp := childMaxMatchMap.TranslateQueryValueMaybe(candidateAlias)
	if _, isProjectedField := pulledUp.(*values.FieldValue); !isProjectedField {
		t.Fatalf(
			"fixture pull-up = %T, want projected FieldValue",
			pulledUp,
		)
	}
	translation, ok := buildSelectSubsumptionTranslationMapMaybe(
		[]expressions.Quantifier{queryQuantifier},
		[]expressions.Quantifier{candidateQuantifier},
		[]quantifierMapping{{queryIndex: 0, candidateIndex: 0}},
		[]quantifierPartialMatch{{
			quantifier: queryQuantifier,
			partialMatch: selectTranslationTestPartialMatch(
				selectTranslationTestRegularMatchInfo(childMaxMatchMap),
				queryChildRef,
				candidateChildRef,
			),
		}},
		AliasMapOfAliases(queryAlias, candidateAlias),
	)
	if !ok {
		t.Fatal("failed to build projected ForEach translation fixture")
	}
	counting := &countingSelectTranslationMap{delegate: translation}
	querySelect := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(queryAlias),
		[]expressions.Quantifier{queryQuantifier},
		[]predicates.QueryPredicate{
			predicates.NewExistentialAlias(queryAlias),
		},
	)

	translatedPredicates, translatedResult, translated := translateSelectSubsumptionInputs(querySelect, counting)
	if translated ||
		translatedPredicates != nil ||
		translatedResult != nil {
		t.Fatalf(
			"malformed EVP translated predicates=%v result=%v ok=%v",
			translatedPredicates,
			translatedResult,
			translated,
		)
	}
	if counting.applied != 0 {
		t.Fatalf(
			"malformed EVP applied translation %d times before rejection",
			counting.applied,
		)
	}
}

func TestTranslateSelectSubsumptionInputs_ExistentialToForEachTranslatesOnce(
	t *testing.T,
) {
	_, queryChildRef := selectTranslationTestReference()
	_, candidateChildRef := selectTranslationTestReference()
	queryAlias := values.NamedCorrelationIdentifier("query_exists_to_fe")
	candidateAlias := values.NamedCorrelationIdentifier("candidate_exists_to_fe")
	queryQuantifier := expressions.NamedExistentialQuantifier(
		queryAlias,
		queryChildRef,
	)
	candidateQuantifier := expressions.NamedForEachQuantifier(
		candidateAlias,
		candidateChildRef,
	)
	translation, ok := buildSelectSubsumptionTranslationMapMaybe(
		[]expressions.Quantifier{queryQuantifier},
		[]expressions.Quantifier{candidateQuantifier},
		[]quantifierMapping{{queryIndex: 0, candidateIndex: 0}},
		[]quantifierPartialMatch{{
			quantifier: queryQuantifier,
			partialMatch: selectTranslationTestPartialMatch(
				selectTranslationTestRegularMatchInfo(nil),
				queryChildRef,
				candidateChildRef,
			),
		}},
		AliasMapOfAliases(queryAlias, candidateAlias),
	)
	if !ok {
		t.Fatal("failed to build dormant E-to-FE translation")
	}
	counting := &countingSelectTranslationMap{delegate: translation}
	querySelect := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(queryAlias),
		[]expressions.Quantifier{queryQuantifier},
		[]predicates.QueryPredicate{
			predicates.NewExistentialAlias(queryAlias),
		},
	)

	translatedPredicates, translatedResult, translated := translateSelectSubsumptionInputs(querySelect, counting)
	if !translated {
		t.Fatal("valid dormant E-to-FE inputs failed translation")
	}
	if counting.applied != 2 {
		t.Fatalf(
			"E-to-FE translation applications = %d, want result+EVP exactly once each",
			counting.applied,
		)
	}
	translatedResultQOV, ok := translatedResult.(*values.QuantifiedObjectValue)
	if !ok || translatedResultQOV.Correlation != candidateAlias {
		t.Fatalf(
			"translated result = %T/%v, want QOV(%s)",
			translatedResult,
			translatedResult,
			candidateAlias.Name(),
		)
	}
	translatedEVP, ok := translatedPredicates[0].(*predicates.ExistentialValuePredicate)
	if !ok {
		t.Fatalf(
			"translated predicate = %T, want ExistentialValuePredicate",
			translatedPredicates[0],
		)
	}
	translatedEVPQOV, ok := translatedEVP.Value.(*values.QuantifiedObjectValue)
	if !ok || translatedEVPQOV.Correlation != candidateAlias {
		t.Fatalf(
			"translated EVP value = %T/%v, want QOV(%s)",
			translatedEVP.Value,
			translatedEVP.Value,
			candidateAlias.Name(),
		)
	}
}

func TestTranslateSelectSubsumptionInputs_NilAndTypedNilInputsFailClosed(
	t *testing.T,
) {
	alias := values.NamedCorrelationIdentifier("query_nil_input")
	validResult := values.NewQuantifiedObjectValue(alias)
	identityTranslation := values.NewTranslationMapBuilder().Build()
	validSelect := expressions.NewSelectExpression(validResult, nil, nil)
	var typedNilResult *values.QuantifiedObjectValue
	var typedNilPredicate *predicates.ComparisonPredicate
	var typedNilRegularTranslation *values.RegularTranslationMap

	tests := []struct {
		name        string
		querySelect *expressions.SelectExpression
		translation values.TranslationMap
	}{
		{
			name:        "nil Select",
			querySelect: nil,
			translation: identityTranslation,
		},
		{
			name: "nil result",
			querySelect: expressions.NewSelectExpression(
				nil,
				nil,
				nil,
			),
			translation: identityTranslation,
		},
		{
			name: "typed-nil result",
			querySelect: expressions.NewSelectExpression(
				typedNilResult,
				nil,
				nil,
			),
			translation: identityTranslation,
		},
		{
			name: "nil predicate",
			querySelect: expressions.NewSelectExpression(
				validResult,
				nil,
				[]predicates.QueryPredicate{nil},
			),
			translation: identityTranslation,
		},
		{
			name: "typed-nil predicate",
			querySelect: expressions.NewSelectExpression(
				validResult,
				nil,
				[]predicates.QueryPredicate{typedNilPredicate},
			),
			translation: identityTranslation,
		},
		{
			name:        "typed-nil translation",
			querySelect: validSelect,
			translation: typedNilRegularTranslation,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf(
						"invalid input panicked instead of failing closed: %v",
						recovered,
					)
				}
			}()
			translatedPredicates, translatedResult, translated := translateSelectSubsumptionInputs(
				test.querySelect,
				test.translation,
			)
			if translated ||
				translatedPredicates != nil ||
				translatedResult != nil {
				t.Fatalf(
					"invalid input translated predicates=%v result=%v ok=%v",
					translatedPredicates,
					translatedResult,
					translated,
				)
			}
		})
	}
}

func TestTranslateSelectSubsumptionInputs_MalformedEmbeddedValuesFailClosed(
	t *testing.T,
) {
	alias := values.NamedCorrelationIdentifier("query_bad_value")
	validValue := values.NewQuantifiedObjectValue(alias)
	var typedNilValue *values.QuantifiedObjectValue
	nestedTypedNilValue := &values.FieldValue{
		Field: "nested_bad",
		Typ:   values.UnknownType,
		Child: typedNilValue,
	}
	nestedComparison := predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: nestedTypedNilValue,
	}
	nestedComparisonRange := predicates.EmptyComparisonRange().
		Merge(&nestedComparison).
		Range

	tests := []struct {
		name      string
		predicate predicates.QueryPredicate
	}{
		{
			name: "nil comparison LHS",
			predicate: &predicates.ComparisonPredicate{
				Comparison: predicates.Comparison{
					Type: predicates.ComparisonIsNull,
				},
			},
		},
		{
			name: "nil non-unary comparison RHS",
			predicate: &predicates.ComparisonPredicate{
				Operand: validValue,
				Comparison: predicates.Comparison{
					Type: predicates.ComparisonEquals,
				},
			},
		},
		{
			name: "nil distance query vector",
			predicate: &predicates.ComparisonPredicate{
				Operand: validValue,
				Comparison: predicates.Comparison{
					Type: predicates.ComparisonDistanceRankLessThanOrEq,
					Operand: &values.ConstantValue{
						Value: int64(1),
						Typ:   values.NotNullLong,
					},
				},
			},
		},
		{
			name:      "nil ValuePredicate value",
			predicate: predicates.NewValuePredicate(nil),
		},
		{
			name: "nil Placeholder value",
			predicate: &predicates.Placeholder{
				ParameterAlias: values.NamedCorrelationIdentifier("p"),
				CompRange:      predicates.EmptyComparisonRange(),
			},
		},
		{
			name: "nil Placeholder range",
			predicate: &predicates.Placeholder{
				ParameterAlias: values.NamedCorrelationIdentifier("p"),
				Value:          validValue,
			},
		},
		{
			name: "nil existential value",
			predicate: &predicates.ExistentialValuePredicate{
				Comparison: predicates.Comparison{
					Type: predicates.ComparisonIsNotNull,
				},
			},
		},
		{
			name: "nil ranged-predicate value",
			predicate: predicates.NewPredicateWithValueAndRanges(
				nil,
				nil,
			),
		},
		{
			name:      "typed-nil required value",
			predicate: predicates.NewValuePredicate(typedNilValue),
		},
		{
			name: "nested typed-nil comparison LHS",
			predicate: predicates.NewComparisonPredicate(
				nestedTypedNilValue,
				predicates.Comparison{
					Type: predicates.ComparisonIsNull,
				},
			),
		},
		{
			name: "nested typed-nil comparison RHS",
			predicate: predicates.NewComparisonPredicate(
				validValue,
				predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: nestedTypedNilValue,
				},
			),
		},
		{
			name: "nested typed-nil Placeholder range comparand",
			predicate: &predicates.Placeholder{
				ParameterAlias: values.NamedCorrelationIdentifier("p"),
				Value:          validValue,
				CompRange:      nestedComparisonRange,
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf(
						"malformed embedded value panicked instead of failing closed: %v",
						recovered,
					)
				}
			}()
			querySelect := expressions.NewSelectExpression(
				validValue,
				nil,
				[]predicates.QueryPredicate{test.predicate},
			)
			translatedPredicates, translatedResult, translated := translateSelectSubsumptionInputs(querySelect, nil)
			if translated ||
				translatedPredicates != nil ||
				translatedResult != nil {
				t.Fatalf(
					"malformed value translated predicates=%v result=%v ok=%v",
					translatedPredicates,
					translatedResult,
					translated,
				)
			}
		})
	}
}

func TestTranslateSelectSubsumptionInputs_NestedTypedNilResultFailsClosed(
	t *testing.T,
) {
	t.Parallel()

	var typedNilValue *values.QuantifiedObjectValue
	nestedTypedNilResult := &values.FieldValue{
		Field: "nested_bad",
		Typ:   values.UnknownType,
		Child: typedNilValue,
	}
	querySelect := expressions.NewSelectExpression(
		nestedTypedNilResult,
		nil,
		nil,
	)

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf(
				"nested typed-nil result panicked instead of failing closed: %v",
				recovered,
			)
		}
	}()
	translatedPredicates, translatedResult, translated := translateSelectSubsumptionInputs(
		querySelect,
		nil,
	)
	if translated ||
		translatedPredicates != nil ||
		translatedResult != nil {
		t.Fatalf(
			"nested typed-nil result translated predicates=%v result=%v ok=%v",
			translatedPredicates,
			translatedResult,
			translated,
		)
	}
}

func TestTranslateSelectSubsumptionInputs_ParameterComparisonIsValid(
	t *testing.T,
) {
	alias := values.NamedCorrelationIdentifier("query_parameter")
	queryValue := values.NewQuantifiedObjectValue(alias)
	queryPredicate := predicates.NewComparisonPredicate(
		queryValue,
		predicates.Comparison{
			Type:          predicates.ComparisonEquals,
			ParameterName: "bound_parameter",
		},
	)
	querySelect := expressions.NewSelectExpression(
		queryValue,
		nil,
		[]predicates.QueryPredicate{queryPredicate},
	)

	translatedPredicates, translatedResult, translated := translateSelectSubsumptionInputs(querySelect, nil)
	if !translated ||
		len(translatedPredicates) != 1 ||
		translatedPredicates[0] != queryPredicate ||
		translatedResult != queryValue {
		t.Fatalf(
			"parameter comparison translated predicates=%v result=%v ok=%v",
			translatedPredicates,
			translatedResult,
			translated,
		)
	}
}

func TestTranslateSelectSubsumptionInputs_FullPredicateSpineOnce(
	t *testing.T,
) {
	_, queryChildRef := selectTranslationTestReference()
	_, candidateChildRef := selectTranslationTestReference()
	queryForEachAlias := values.NamedCorrelationIdentifier("query_fe")
	candidateForEachAlias := values.NamedCorrelationIdentifier("candidate_fe")
	queryExistentialAlias := values.NamedCorrelationIdentifier("query_exists")
	candidateExistentialAlias := values.NamedCorrelationIdentifier("candidate_exists")
	queryForEach := expressions.NamedForEachQuantifier(
		queryForEachAlias,
		queryChildRef,
	)
	candidateForEach := expressions.NamedForEachQuantifier(
		candidateForEachAlias,
		candidateChildRef,
	)
	queryExistential := expressions.NamedExistentialQuantifier(
		queryExistentialAlias,
		queryChildRef,
	)
	candidateExistential := expressions.NamedExistentialQuantifier(
		candidateExistentialAlias,
		candidateChildRef,
	)

	translation, ok := buildSelectSubsumptionTranslationMapMaybe(
		[]expressions.Quantifier{queryForEach, queryExistential},
		[]expressions.Quantifier{
			candidateForEach,
			candidateExistential,
		},
		[]quantifierMapping{
			{queryIndex: 0, candidateIndex: 0},
			{queryIndex: 1, candidateIndex: 1},
		},
		[]quantifierPartialMatch{
			{
				quantifier: queryForEach,
				partialMatch: selectTranslationTestPartialMatch(
					selectTranslationTestRegularMatchInfo(
						selectTranslationTestValidMaxMatchMap(),
					),
					queryChildRef,
					candidateChildRef,
				),
			},
			{
				quantifier: queryExistential,
				partialMatch: selectTranslationTestPartialMatch(
					selectTranslationTestRegularMatchInfo(nil),
					queryChildRef,
					candidateChildRef,
				),
			},
		},
		selectTranslationTestAliasMap(
			t,
			[2]values.CorrelationIdentifier{
				queryForEachAlias,
				candidateForEachAlias,
			},
			[2]values.CorrelationIdentifier{
				queryExistentialAlias,
				candidateExistentialAlias,
			},
		),
	)
	if !ok {
		t.Fatal("failed to build full-spine translation")
	}
	counting := &countingSelectTranslationMap{delegate: translation}
	queryQOV := values.NewQuantifiedObjectValue(queryForEachAlias)

	comparison := predicates.NewComparisonPredicate(
		values.NewFieldValue(queryQOV, "LHS", values.NotNullLong),
		predicates.Comparison{
			Type: predicates.ComparisonEquals,
			Operand: values.NewFieldValue(
				queryQOV,
				"RHS",
				values.NotNullLong,
			),
		},
	)
	nested := predicates.NewAnd(
		predicates.NewOr(
			comparison,
			predicates.NewNot(
				predicates.NewValuePredicate(queryQOV),
			),
		),
	)
	vector := predicates.NewComparisonPredicate(
		values.NewFlatFieldValue("EMB", nil),
		predicates.Comparison{
			Type: predicates.ComparisonDistanceRankLessThanOrEq,
			Operand: &values.ConstantValue{
				Value: int64(5),
				Typ:   values.NotNullLong,
			},
			QueryVector: values.NewFieldValue(
				queryQOV,
				"EMB",
				nil,
			),
		},
	)
	valueAndRanges := predicates.NewPredicateWithValueAndRanges(
		values.NewFieldValue(queryQOV, "ID", values.NotNullLong),
		[]*predicates.RangeConstraints{
			predicates.NewRangeConstraints(
				nil,
				[]predicates.Comparison{{
					Type: predicates.ComparisonEquals,
					Operand: values.NewFieldValue(
						queryQOV,
						"V",
						values.NotNullLong,
					),
				}},
			),
		},
	)
	placeholder := predicates.NewPlaceholder(
		values.NamedCorrelationIdentifier("parameter"),
		values.NewFieldValue(queryQOV, "P", values.NotNullLong),
	)
	existentialPredicate := predicates.NewExistentialAlias(queryExistentialAlias)
	querySelect := expressions.NewSelectExpression(
		values.NewRecordConstructorValue(
			values.RecordConstructorField{
				Name:  "fe",
				Value: queryQOV,
			},
			values.RecordConstructorField{
				Name: "exists",
				Value: values.NewQuantifiedObjectValue(
					queryExistentialAlias,
				),
			},
		),
		[]expressions.Quantifier{queryForEach, queryExistential},
		[]predicates.QueryPredicate{
			nested,
			vector,
			valueAndRanges,
			placeholder,
			existentialPredicate,
		},
	)

	translatedPredicates, translatedResult, translated := translateSelectSubsumptionInputs(querySelect, counting)
	if !translated {
		t.Fatal("valid Select inputs failed checked translation")
	}
	if len(translatedPredicates) != 5 {
		t.Fatalf(
			"translated predicates = %d, want 5",
			len(translatedPredicates),
		)
	}
	// Result (1), nested comparison/value predicate (3), vector (1),
	// PredicateWithValueAndRanges anchor/range (2), and Placeholder (1).
	// The E-to-E value is deliberately absent from the map.
	if counting.applied != 8 {
		t.Fatalf(
			"translation applications = %d, want exactly 8",
			counting.applied,
		)
	}

	resultCorrelations := values.GetCorrelatedToOfValue(translatedResult)
	if _, stale := resultCorrelations[queryForEachAlias]; stale {
		t.Fatal("translated result retains query ForEach alias")
	}
	if _, translated := resultCorrelations[candidateForEachAlias]; !translated {
		t.Fatal("translated result is missing candidate ForEach alias")
	}
	if _, retained := resultCorrelations[queryExistentialAlias]; !retained {
		t.Fatal("omitted E-to-E result alias was unexpectedly rewritten")
	}
	if _, wrong := resultCorrelations[candidateExistentialAlias]; wrong {
		t.Fatal("candidate existential alias entered translated result")
	}

	for predicateIndex := 0; predicateIndex < 4; predicateIndex++ {
		correlatedTo := predicates.GetCorrelatedToOfPredicate(
			translatedPredicates[predicateIndex],
		)
		if _, stale := correlatedTo[queryForEachAlias]; stale {
			t.Fatalf(
				"translated predicate %d retains query ForEach alias",
				predicateIndex,
			)
		}
		if _, translated := correlatedTo[candidateForEachAlias]; !translated {
			t.Fatalf(
				"translated predicate %d is missing candidate ForEach alias",
				predicateIndex,
			)
		}
	}
	existentialCorrelations := predicates.GetCorrelatedToOfPredicate(
		translatedPredicates[4],
	)
	if _, retained := existentialCorrelations[queryExistentialAlias]; !retained {
		t.Fatal("E-to-E existential predicate did not retain query alias")
	}
}

func TestBuildSelectSubsumptionMaxMatchMap_CandidateWideAliasesAndEquivalence(
	t *testing.T,
) {
	t.Parallel()

	_, candidateChildRef := selectTranslationTestReference()
	queryAlias := values.NamedCorrelationIdentifier("deep_query")
	candidateAlias := values.NamedCorrelationIdentifier("deep_candidate")
	extraExistentialAlias := values.NamedCorrelationIdentifier("extra_candidate_exists")
	candidateForEach := expressions.NamedForEachQuantifier(
		candidateAlias,
		candidateChildRef,
	)
	candidateExistential := expressions.NamedExistentialQuantifier(
		extraExistentialAlias,
		candidateChildRef,
	)
	// Use QOV roots so the test isolates alias equivalence itself. The current
	// MaxMatchMap reachable-candidate walk descends only record constructors;
	// a FieldValue root would additionally exercise that independent reachability
	// policy instead of this helper's equivalence wiring.
	translatedQueryResult := values.NewQuantifiedObjectValue(queryAlias)
	candidateResult := values.NewQuantifiedObjectValue(candidateAlias)
	candidateSelect := expressions.NewSelectExpression(
		candidateResult,
		[]expressions.Quantifier{
			candidateForEach,
			candidateExistential,
		},
		nil,
	)
	bindingAliasMap := AliasMapOfAliases(queryAlias, candidateAlias)

	maxMatchMap := buildSelectSubsumptionMaxMatchMap(
		translatedQueryResult,
		candidateSelect,
		bindingAliasMap,
	)
	if maxMatchMap == nil || maxMatchMap.Size() == 0 {
		t.Fatal("alias-equivalent translated result did not match candidate")
	}
	for _, alias := range []values.CorrelationIdentifier{
		candidateAlias,
		extraExistentialAlias,
	} {
		if _, rangedOver := maxMatchMap.rangedOverAliases[alias]; !rangedOver {
			t.Fatalf(
				"candidate alias %s missing from ranged-over set",
				alias.Name(),
			)
		}
	}

	withoutEquivalence := ComputeMaxMatchMap(
		translatedQueryResult,
		candidateResult,
		quantifierAliases(candidateSelect.GetQuantifiers()),
	)
	if withoutEquivalence.Size() != 0 {
		t.Fatal("control unexpectedly matched without alias equivalence")
	}
}

func TestBuildSelectSubsumptionMaxMatchMap_NilResultsFailClosed(
	t *testing.T,
) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("max_match_nil")
	validResult := values.NewQuantifiedObjectValue(alias)
	validCandidate := expressions.NewSelectExpression(validResult, nil, nil)
	var typedNilResult *values.QuantifiedObjectValue
	nestedTypedNilResult := &values.FieldValue{
		Field: "nested_bad",
		Typ:   values.UnknownType,
		Child: typedNilResult,
	}

	tests := []struct {
		name             string
		translatedResult values.Value
		candidateSelect  *expressions.SelectExpression
	}{
		{
			name:             "nil translated result",
			translatedResult: nil,
			candidateSelect:  validCandidate,
		},
		{
			name:             "typed-nil translated result",
			translatedResult: typedNilResult,
			candidateSelect:  validCandidate,
		},
		{
			name:             "nested typed-nil translated result",
			translatedResult: nestedTypedNilResult,
			candidateSelect:  validCandidate,
		},
		{
			name:             "nil candidate",
			translatedResult: validResult,
			candidateSelect:  nil,
		},
		{
			name:             "nil candidate result",
			translatedResult: validResult,
			candidateSelect: expressions.NewSelectExpression(
				nil,
				nil,
				nil,
			),
		},
		{
			name:             "typed-nil candidate result",
			translatedResult: validResult,
			candidateSelect: expressions.NewSelectExpression(
				typedNilResult,
				nil,
				nil,
			),
		},
		{
			name:             "nested typed-nil candidate result",
			translatedResult: validResult,
			candidateSelect: expressions.NewSelectExpression(
				nestedTypedNilResult,
				nil,
				nil,
			),
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf(
						"invalid result tree panicked instead of failing closed: %v",
						recovered,
					)
				}
			}()
			if maxMatchMap := buildSelectSubsumptionMaxMatchMap(
				test.translatedResult,
				test.candidateSelect,
				EmptyAliasMap(),
			); maxMatchMap != nil {
				t.Fatalf(
					"invalid result produced MaxMatchMap of size %d",
					maxMatchMap.Size(),
				)
			}
		})
	}
}
