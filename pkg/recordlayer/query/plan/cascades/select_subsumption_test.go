package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestSelectSubsumptionQuantifierPairCompatibility(t *testing.T) {
	ref := selectSubsumptionTestLeafRef()
	plainA := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("plain_a"),
		ref,
	)
	plainB := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("plain_b"),
		ref,
	)
	nullA := expressions.NamedForEachNullOnEmptyQuantifier(
		values.NamedCorrelationIdentifier("null_a"),
		ref,
	)
	nullB := expressions.NamedForEachNullOnEmptyQuantifier(
		values.NamedCorrelationIdentifier("null_b"),
		ref,
	)
	strictA := expressions.NamedForEachStrictSingleQuantifier(
		values.NamedCorrelationIdentifier("strict_a"),
		ref,
	)
	strictB := expressions.NamedForEachStrictSingleQuantifier(
		values.NamedCorrelationIdentifier("strict_b"),
		ref,
	)
	existsA := expressions.NamedExistentialQuantifier(
		values.NamedCorrelationIdentifier("exists_a"),
		ref,
	)
	existsB := expressions.NamedExistentialQuantifier(
		values.NamedCorrelationIdentifier("exists_b"),
		ref,
	)
	physical := expressions.NamedPhysicalQuantifier(
		values.NamedCorrelationIdentifier("physical"),
		ref,
	)

	tests := []struct {
		name      string
		query     expressions.Quantifier
		candidate expressions.Quantifier
		want      bool
	}{
		{"plain foreach", plainA, plainB, true},
		{"null-on-empty foreach", nullA, nullB, true},
		{"strict-single foreach", strictA, strictB, true},
		{"plain to null-on-empty mismatch", plainA, nullB, false},
		{"plain to strict-single mismatch", plainA, strictB, false},
		{"null-on-empty mismatch", nullA, plainB, false},
		{"null-on-empty to strict-single mismatch", nullA, strictB, false},
		{"strict-single mismatch", strictA, plainB, false},
		{"strict-single to null-on-empty mismatch", strictA, nullB, false},
		{"existential", existsA, existsB, true},
		{"existential to foreach stays closed", existsA, plainB, false},
		{"foreach to existential", plainA, existsB, false},
		{"physical query", physical, plainB, false},
		{"physical candidate", plainA, physical, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := selectSubsumptionQuantifierPairCompatible(
				test.query,
				test.candidate,
			); got != test.want {
				t.Fatalf(
					"selectSubsumptionQuantifierPairCompatible() = %v, want %v",
					got,
					test.want,
				)
			}
		})
	}
}

func TestEnumerateSelectSubsumptionMappingsRejectsUnownedExactButVisitsForEachSubset(
	t *testing.T,
) {
	ref := selectSubsumptionTestLeafRef()
	queryQuantifiers := []expressions.Quantifier{
		expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("query_fe"),
			ref,
		),
		expressions.NamedExistentialQuantifier(
			values.NamedCorrelationIdentifier("query_e"),
			ref,
		),
	}
	candidateQuantifiers := []expressions.Quantifier{
		expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("candidate_fe"),
			ref,
		),
		expressions.NamedExistentialQuantifier(
			values.NamedCorrelationIdentifier("candidate_e"),
			ref,
		),
	}
	query := selectSubsumptionTestSelect(
		queryQuantifiers,
		nil, // no predicate owns the selected query existential
		expressions.JoinInner,
	)
	candidate := selectSubsumptionTestSelect(
		candidateQuantifiers,
		nil,
		expressions.JoinInner,
	)

	var visited [][]quantifierMapping
	var aliasMaps []*AliasMap
	enumerateSelectSubsumptionMappings(
		query,
		candidate,
		&matchIntermediateSearchBudget{},
		func(mapping []quantifierMapping, aliasMap *AliasMap) bool {
			visited = append(visited, mapping)
			aliasMaps = append(aliasMaps, aliasMap)
			return true
		},
	)

	if len(visited) != 1 {
		t.Fatalf("visited %d mappings, want only the FE subset", len(visited))
	}
	want := quantifierMapping{queryIndex: 0, candidateIndex: 0}
	if len(visited[0]) != 1 || visited[0][0] != want {
		t.Fatalf("visited mapping = %#v, want %#v", visited[0], []quantifierMapping{want})
	}
	if aliasMaps[0].Size() != 1 ||
		!aliasMaps[0].ContainsMapping(
			queryQuantifiers[0].GetAlias(),
			candidateQuantifiers[0].GetAlias(),
		) {
		t.Fatalf("visited alias map = %#v, want FE binding", aliasMaps[0].ForwardMap())
	}
}

func TestEnumerateSelectSubsumptionMappingsPassesStableCopies(t *testing.T) {
	ref := selectSubsumptionTestLeafRef()
	queryQuantifiers := []expressions.Quantifier{
		expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("query_a"),
			ref,
		),
		expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("query_b"),
			ref,
		),
	}
	candidateQuantifiers := []expressions.Quantifier{
		expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("candidate_a"),
			ref,
		),
		expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("candidate_b"),
			ref,
		),
	}
	query := selectSubsumptionTestSelect(
		queryQuantifiers,
		nil,
		expressions.JoinInner,
	)
	candidate := selectSubsumptionTestSelect(
		candidateQuantifiers,
		nil,
		expressions.JoinInner,
	)

	var captured [][]quantifierMapping
	var capturedAliases []*AliasMap
	enumerateSelectSubsumptionMappings(
		query,
		candidate,
		&matchIntermediateSearchBudget{},
		func(mapping []quantifierMapping, aliasMap *AliasMap) bool {
			// Retain both arguments directly. The wrapper owns the mapping
			// snapshot and AliasMap is immutable.
			captured = append(captured, mapping)
			capturedAliases = append(capturedAliases, aliasMap)
			return true
		},
	)

	if len(captured) != 2 {
		t.Fatalf("captured %d mappings, want both FE bijections", len(captured))
	}
	wantFirst := []quantifierMapping{
		{queryIndex: 0, candidateIndex: 0},
		{queryIndex: 1, candidateIndex: 1},
	}
	wantSecond := []quantifierMapping{
		{queryIndex: 1, candidateIndex: 0},
		{queryIndex: 0, candidateIndex: 1},
	}
	if !selectSubsumptionTestMappingsEqual(captured[0], wantFirst) ||
		!selectSubsumptionTestMappingsEqual(captured[1], wantSecond) {
		t.Fatalf(
			"captured mappings = %#v, want %#v then %#v",
			captured,
			wantFirst,
			wantSecond,
		)
	}
	for index, mapping := range captured {
		if capturedAliases[index].Size() != len(mapping) {
			t.Fatalf(
				"alias map %d size = %d, want %d",
				index,
				capturedAliases[index].Size(),
				len(mapping),
			)
		}
		for _, pair := range mapping {
			if !capturedAliases[index].ContainsMapping(
				queryQuantifiers[pair.queryIndex].GetAlias(),
				candidateQuantifiers[pair.candidateIndex].GetAlias(),
			) {
				t.Fatalf(
					"alias map %d does not correspond to retained mapping %#v",
					index,
					mapping,
				)
			}
		}
	}
}

func TestSelectSubsumptionChildMatchesCanBeDeferred(t *testing.T) {
	ref := selectSubsumptionTestLeafRef()
	forEach := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("selected_fe"),
		ref,
	)
	existential := expressions.NamedExistentialQuantifier(
		values.NamedCorrelationIdentifier("selected_e"),
		ref,
	)
	deferable := selectSubsumptionTestPartialMatch(
		expressions.NewFullUnorderedScanExpression(
			[]string{"deferable"},
			values.UnknownType,
		),
	)
	notDeferable := selectSubsumptionTestPartialMatch(
		selectSubsumptionTestSelect(
			[]expressions.Quantifier{
				expressions.NamedForEachQuantifier(
					values.NamedCorrelationIdentifier("unmatched_child_fe"),
					ref,
				),
			},
			nil,
			expressions.JoinInner,
		),
	)

	tests := []struct {
		name     string
		children []quantifierPartialMatch
		want     bool
	}{
		{"empty", nil, true},
		{
			"deferable foreach",
			[]quantifierPartialMatch{
				{quantifier: forEach, partialMatch: deferable},
			},
			true,
		},
		{
			"non-deferable foreach",
			[]quantifierPartialMatch{
				{quantifier: forEach, partialMatch: notDeferable},
			},
			false,
		},
		{
			"missing foreach match",
			[]quantifierPartialMatch{
				{quantifier: forEach, partialMatch: nil},
			},
			false,
		},
		{
			"existential match need not be concrete",
			[]quantifierPartialMatch{
				{quantifier: existential, partialMatch: nil},
			},
			true,
		},
		{
			"existential does not poison deferable foreach",
			[]quantifierPartialMatch{
				{quantifier: existential, partialMatch: nil},
				{quantifier: forEach, partialMatch: deferable},
			},
			true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := selectSubsumptionChildMatchesCanBeDeferred(
				test.children,
			); got != test.want {
				t.Fatalf(
					"selectSubsumptionChildMatchesCanBeDeferred() = %v, want %v",
					got,
					test.want,
				)
			}
		})
	}
}

func TestValidateSelectSubsumptionMappingJoinTypes(t *testing.T) {
	queryQuantifier := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("query"),
		selectSubsumptionTestLeafRef(),
	)
	candidateQuantifier := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("candidate"),
		selectSubsumptionTestLeafRef(),
	)
	mapping := []quantifierMapping{{queryIndex: 0, candidateIndex: 0}}

	allowed := []expressions.JoinType{
		expressions.JoinInner,
		expressions.JoinCross,
	}
	for _, queryJoinType := range allowed {
		for _, candidateJoinType := range allowed {
			query := selectSubsumptionTestSelect(
				[]expressions.Quantifier{queryQuantifier},
				nil,
				queryJoinType,
			)
			candidate := selectSubsumptionTestSelect(
				[]expressions.Quantifier{candidateQuantifier},
				nil,
				candidateJoinType,
			)
			if _, ok := validateSelectSubsumptionMapping(
				query,
				candidate,
				mapping,
			); !ok {
				t.Fatalf(
					"expected join types (%v, %v) to be allowed",
					queryJoinType,
					candidateJoinType,
				)
			}
		}
	}

	outerTypes := []expressions.JoinType{
		expressions.JoinLeftOuter,
		expressions.JoinFullOuter,
	}
	for _, outerType := range outerTypes {
		query := selectSubsumptionTestSelect(
			[]expressions.Quantifier{queryQuantifier},
			nil,
			outerType,
		)
		candidate := selectSubsumptionTestSelect(
			[]expressions.Quantifier{candidateQuantifier},
			nil,
			expressions.JoinInner,
		)
		if _, ok := validateSelectSubsumptionMapping(
			query,
			candidate,
			mapping,
		); ok {
			t.Fatalf("expected query join type %v to be rejected", outerType)
		}
		if _, ok := validateSelectSubsumptionMapping(
			candidate,
			query,
			mapping,
		); ok {
			t.Fatalf(
				"expected candidate join type %v to be rejected",
				outerType,
			)
		}
	}
}

func TestValidateSelectSubsumptionMappingRejectsInvalidAliasesAndPhysicalEdges(
	t *testing.T,
) {
	ref := selectSubsumptionTestLeafRef()
	queryAlias := values.NamedCorrelationIdentifier("query")
	candidateAlias := values.NamedCorrelationIdentifier("candidate")
	mapping := []quantifierMapping{{queryIndex: 0, candidateIndex: 0}}
	validQuery := selectSubsumptionTestSelect(
		[]expressions.Quantifier{
			expressions.NamedForEachQuantifier(queryAlias, ref),
		},
		nil,
		expressions.JoinInner,
	)
	validCandidate := selectSubsumptionTestSelect(
		[]expressions.Quantifier{
			expressions.NamedForEachQuantifier(candidateAlias, ref),
		},
		nil,
		expressions.JoinInner,
	)

	tests := []struct {
		name      string
		query     *expressions.SelectExpression
		candidate *expressions.SelectExpression
	}{
		{
			"zero query alias",
			selectSubsumptionTestSelect(
				[]expressions.Quantifier{
					expressions.NamedForEachQuantifier(
						values.CorrelationIdentifier{},
						ref,
					),
				},
				nil,
				expressions.JoinInner,
			),
			validCandidate,
		},
		{
			"zero-value query quantifier",
			selectSubsumptionTestSelect(
				[]expressions.Quantifier{{}},
				nil,
				expressions.JoinInner,
			),
			validCandidate,
		},
		{
			"zero candidate alias",
			validQuery,
			selectSubsumptionTestSelect(
				[]expressions.Quantifier{
					expressions.NamedForEachQuantifier(
						values.CorrelationIdentifier{},
						ref,
					),
				},
				nil,
				expressions.JoinInner,
			),
		},
		{
			"zero-value candidate quantifier",
			validQuery,
			selectSubsumptionTestSelect(
				[]expressions.Quantifier{{}},
				nil,
				expressions.JoinInner,
			),
		},
		{
			"nil query ranges-over",
			selectSubsumptionTestSelect(
				[]expressions.Quantifier{
					expressions.NamedForEachQuantifier(queryAlias, nil),
				},
				nil,
				expressions.JoinInner,
			),
			validCandidate,
		},
		{
			"nil candidate ranges-over",
			validQuery,
			selectSubsumptionTestSelect(
				[]expressions.Quantifier{
					expressions.NamedForEachQuantifier(
						candidateAlias,
						nil,
					),
				},
				nil,
				expressions.JoinInner,
			),
		},
		{
			"duplicate query alias",
			selectSubsumptionTestSelect(
				[]expressions.Quantifier{
					expressions.NamedForEachQuantifier(queryAlias, ref),
					expressions.NamedExistentialQuantifier(queryAlias, ref),
				},
				nil,
				expressions.JoinInner,
			),
			validCandidate,
		},
		{
			"duplicate candidate alias",
			validQuery,
			selectSubsumptionTestSelect(
				[]expressions.Quantifier{
					expressions.NamedForEachQuantifier(candidateAlias, ref),
					expressions.NamedExistentialQuantifier(
						candidateAlias,
						ref,
					),
				},
				nil,
				expressions.JoinInner,
			),
		},
		{
			"omitted query physical edge",
			selectSubsumptionTestSelect(
				[]expressions.Quantifier{
					expressions.NamedForEachQuantifier(queryAlias, ref),
					expressions.NamedPhysicalQuantifier(
						values.NamedCorrelationIdentifier("query_physical"),
						ref,
					),
				},
				nil,
				expressions.JoinInner,
			),
			validCandidate,
		},
		{
			"omitted candidate physical edge",
			validQuery,
			selectSubsumptionTestSelect(
				[]expressions.Quantifier{
					expressions.NamedForEachQuantifier(candidateAlias, ref),
					expressions.NamedPhysicalQuantifier(
						values.NamedCorrelationIdentifier(
							"candidate_physical",
						),
						ref,
					),
				},
				nil,
				expressions.JoinInner,
			),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, ok := validateSelectSubsumptionMapping(
				test.query,
				test.candidate,
				mapping,
			); ok {
				t.Fatal("expected malformed Select mapping to be rejected")
			}
		})
	}
}

func TestValidateSelectSubsumptionMappingCompleteForEachCoverage(t *testing.T) {
	ref := selectSubsumptionTestLeafRef()
	queryQuantifiers := []expressions.Quantifier{
		expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("query_a"),
			ref,
		),
		expressions.NamedExistentialQuantifier(
			values.NamedCorrelationIdentifier("query_exists"),
			ref,
		),
		expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("query_b"),
			ref,
		),
	}
	candidateQuantifiers := []expressions.Quantifier{
		expressions.NamedExistentialQuantifier(
			values.NamedCorrelationIdentifier("candidate_exists"),
			ref,
		),
		expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("candidate_a"),
			ref,
		),
		expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier("candidate_b"),
			ref,
		),
	}
	query := selectSubsumptionTestSelect(
		queryQuantifiers,
		nil,
		expressions.JoinInner,
	)
	candidate := selectSubsumptionTestSelect(
		candidateQuantifiers,
		nil,
		expressions.JoinCross,
	)

	complete := []quantifierMapping{
		{queryIndex: 0, candidateIndex: 1},
		{queryIndex: 2, candidateIndex: 2},
	}
	aliasMap, ok := validateSelectSubsumptionMapping(
		query,
		candidate,
		complete,
	)
	if !ok {
		t.Fatal("expected all-ForEach mapping to be accepted")
	}
	if aliasMap.Size() != 2 ||
		!aliasMap.ContainsMapping(
			queryQuantifiers[0].GetAlias(),
			candidateQuantifiers[1].GetAlias(),
		) ||
		!aliasMap.ContainsMapping(
			queryQuantifiers[2].GetAlias(),
			candidateQuantifiers[2].GetAlias(),
		) {
		t.Fatalf("unexpected selected alias map: %#v", aliasMap.ForwardMap())
	}

	tests := []struct {
		name    string
		mapping []quantifierMapping
	}{
		{
			name: "unmatched query foreach",
			mapping: []quantifierMapping{
				{queryIndex: 0, candidateIndex: 1},
			},
		},
		{
			name: "unmatched candidate foreach",
			mapping: []quantifierMapping{
				{queryIndex: 0, candidateIndex: 1},
				{queryIndex: 2, candidateIndex: 0},
			},
		},
		{
			name: "duplicate query index",
			mapping: []quantifierMapping{
				{queryIndex: 0, candidateIndex: 1},
				{queryIndex: 0, candidateIndex: 2},
			},
		},
		{
			name: "duplicate candidate index",
			mapping: []quantifierMapping{
				{queryIndex: 0, candidateIndex: 1},
				{queryIndex: 2, candidateIndex: 1},
			},
		},
		{
			name: "out of range",
			mapping: []quantifierMapping{
				{queryIndex: 0, candidateIndex: 1},
				{queryIndex: 2, candidateIndex: 3},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, ok := validateSelectSubsumptionMapping(
				query,
				candidate,
				test.mapping,
			); ok {
				t.Fatal("expected invalid coverage/mapping to be rejected")
			}
		})
	}

	t.Run("candidate-only extra foreach", func(t *testing.T) {
		singleQuery := selectSubsumptionTestSelect(
			queryQuantifiers[:1],
			nil,
			expressions.JoinInner,
		)
		if _, ok := validateSelectSubsumptionMapping(
			singleQuery,
			candidate,
			[]quantifierMapping{{queryIndex: 0, candidateIndex: 1}},
		); ok {
			t.Fatal("expected an unmatched candidate ForEach to be rejected")
		}
	})

	t.Run("query-only extra foreach", func(t *testing.T) {
		singleCandidate := selectSubsumptionTestSelect(
			candidateQuantifiers[1:2],
			nil,
			expressions.JoinInner,
		)
		if _, ok := validateSelectSubsumptionMapping(
			query,
			singleCandidate,
			[]quantifierMapping{{queryIndex: 0, candidateIndex: 0}},
		); ok {
			t.Fatal("expected an unmatched query ForEach to be rejected")
		}
	})
}

func TestValidateSelectSubsumptionMappingMatchedExistentialOwnership(
	t *testing.T,
) {
	ref := selectSubsumptionTestLeafRef()
	queryForEachAlias := values.NamedCorrelationIdentifier("query_for_each")
	queryExistentialAlias := values.NamedCorrelationIdentifier("query_exists")
	candidateForEachAlias := values.NamedCorrelationIdentifier("candidate_for_each")
	candidateExistentialAlias := values.NamedCorrelationIdentifier("candidate_exists")
	queryQuantifiers := []expressions.Quantifier{
		expressions.NamedForEachQuantifier(queryForEachAlias, ref),
		expressions.NamedExistentialQuantifier(queryExistentialAlias, ref),
	}
	candidateQuantifiers := []expressions.Quantifier{
		expressions.NamedForEachQuantifier(candidateForEachAlias, ref),
		expressions.NamedExistentialQuantifier(
			candidateExistentialAlias,
			ref,
		),
	}
	candidate := selectSubsumptionTestSelect(
		candidateQuantifiers,
		nil,
		expressions.JoinInner,
	)
	mapping := []quantifierMapping{
		{queryIndex: 0, candidateIndex: 0},
		{queryIndex: 1, candidateIndex: 1},
	}

	exactOwner := predicates.NewExistentialAlias(queryExistentialAlias)
	comparisonLookalike := predicates.NewComparisonPredicate(
		values.NewQuantifiedObjectValue(queryExistentialAlias),
		predicates.Comparison{Type: predicates.ComparisonIsNotNull},
	)
	extraCorrelationOwner := predicates.MustNewExistentialValuePredicate(
		values.NewQuantifiedObjectValue(queryExistentialAlias),
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: values.NewQuantifiedObjectValue(queryForEachAlias),
		},
	)
	malformedOwner := &predicates.ExistentialValuePredicate{
		Value: values.LiteralValue(int64(1)),
		Comparison: predicates.Comparison{
			Type: predicates.ComparisonEquals,
			Operand: values.NewQuantifiedObjectValue(
				queryExistentialAlias,
			),
		},
	}
	var typedNilAnd *predicates.AndPredicate
	var typedNilQOV *values.QuantifiedObjectValue
	typedNilValueOwner := &predicates.ExistentialValuePredicate{
		Value: typedNilQOV,
		Comparison: predicates.Comparison{
			Type: predicates.ComparisonIsNotNull,
		},
	}
	nonExistentialComparisonOwner := predicates.MustNewExistentialValuePredicate(
		values.NewQuantifiedObjectValue(queryExistentialAlias),
		predicates.Comparison{Type: predicates.ComparisonIsNull},
	)

	tests := []struct {
		name       string
		predicates []predicates.QueryPredicate
		want       bool
	}{
		{"exact owner", []predicates.QueryPredicate{exactOwner}, true},
		{
			"owner nested in conjunction",
			[]predicates.QueryPredicate{
				predicates.NewAnd(
					predicates.NewConstantPredicate(predicates.TriTrue),
					exactOwner,
				),
			},
			true,
		},
		{
			"owner nested in disjunction is not owner",
			[]predicates.QueryPredicate{
				predicates.NewOr(
					predicates.NewConstantPredicate(predicates.TriFalse),
					exactOwner,
				),
			},
			false,
		},
		{"missing owner", nil, false},
		{
			"comparison lookalike is not owner",
			[]predicates.QueryPredicate{comparisonLookalike},
			false,
		},
		{
			"wrapped owner is not owner",
			[]predicates.QueryPredicate{
				predicates.NewNot(exactOwner),
			},
			false,
		},
		{
			"owner has another correlation",
			[]predicates.QueryPredicate{extraCorrelationOwner},
			false,
		},
		{
			"owner names another existential",
			[]predicates.QueryPredicate{
				predicates.NewExistentialAlias(
					values.NamedCorrelationIdentifier("other_exists"),
				),
			},
			false,
		},
		{
			"malformed owner borrows alias from comparison",
			[]predicates.QueryPredicate{malformedOwner},
			false,
		},
		{
			"typed nil conjunction fails closed",
			[]predicates.QueryPredicate{typedNilAnd},
			false,
		},
		{
			"typed nil existential value fails closed",
			[]predicates.QueryPredicate{typedNilValueOwner},
			false,
		},
		{
			"non-not-null existential comparison is not owner",
			[]predicates.QueryPredicate{nonExistentialComparisonOwner},
			false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			query := selectSubsumptionTestSelect(
				queryQuantifiers,
				test.predicates,
				expressions.JoinInner,
			)
			if _, ok := validateSelectSubsumptionMapping(
				query,
				candidate,
				mapping,
			); ok != test.want {
				t.Fatalf(
					"validateSelectSubsumptionMapping() ok = %v, want %v",
					ok,
					test.want,
				)
			}
		})
	}
}

func TestValidateSelectSubsumptionMappingUnmatchedExistentialDependencies(
	t *testing.T,
) {
	queryBaseAlias := values.NamedCorrelationIdentifier("query_base")
	queryDependencyAlias := values.NamedCorrelationIdentifier("query_dependency_exists")
	queryUnmatchedAlias := values.NamedCorrelationIdentifier("query_unmatched_exists")
	candidateBaseAlias := values.NamedCorrelationIdentifier("candidate_base")
	candidateDependencyAlias := values.NamedCorrelationIdentifier("candidate_dependency_exists")

	queryQuantifiers := []expressions.Quantifier{
		expressions.NamedForEachQuantifier(
			queryBaseAlias,
			selectSubsumptionTestLeafRef(),
		),
		expressions.NamedExistentialQuantifier(
			queryDependencyAlias,
			selectSubsumptionTestCorrelatedRef(queryBaseAlias),
		),
		expressions.NamedExistentialQuantifier(
			queryUnmatchedAlias,
			selectSubsumptionTestCorrelatedRef(queryDependencyAlias),
		),
	}
	candidateQuantifiers := []expressions.Quantifier{
		expressions.NamedForEachQuantifier(
			candidateBaseAlias,
			selectSubsumptionTestLeafRef(),
		),
		expressions.NamedExistentialQuantifier(
			candidateDependencyAlias,
			selectSubsumptionTestCorrelatedRef(candidateBaseAlias),
		),
	}
	query := selectSubsumptionTestSelect(
		queryQuantifiers,
		[]predicates.QueryPredicate{
			// The selected existential has the exact required owner.
			predicates.NewExistentialAlias(queryDependencyAlias),
			// The unselected existential is a local predicate correlation.
			predicates.NewExistentialAlias(queryUnmatchedAlias),
		},
		expressions.JoinInner,
	)
	candidate := selectSubsumptionTestSelect(
		candidateQuantifiers,
		nil,
		expressions.JoinInner,
	)

	withFullClosure := []quantifierMapping{
		{queryIndex: 0, candidateIndex: 0},
		{queryIndex: 1, candidateIndex: 1},
	}
	if _, ok := validateSelectSubsumptionMapping(
		query,
		candidate,
		withFullClosure,
	); !ok {
		t.Fatal(
			"expected unmatched existential with fully bound dependency closure",
		)
	}

	withoutExistentialDependency := []quantifierMapping{
		{queryIndex: 0, candidateIndex: 0},
	}
	if _, ok := validateSelectSubsumptionMapping(
		query,
		candidate,
		withoutExistentialDependency,
	); ok {
		t.Fatal(
			"expected unmatched existential with unbound transitive dependency to be rejected",
		)
	}
}

func TestValidateSelectSubsumptionMappingUnmatchedExistentialDiamondDependencies(
	t *testing.T,
) {
	queryBaseAlias := values.NamedCorrelationIdentifier("query_diamond_base")
	queryLeftAlias := values.NamedCorrelationIdentifier("query_diamond_left")
	queryRightAlias := values.NamedCorrelationIdentifier("query_diamond_right")
	queryTipAlias := values.NamedCorrelationIdentifier("query_diamond_tip")
	candidateBaseAlias := values.NamedCorrelationIdentifier("candidate_diamond_base")
	candidateLeftAlias := values.NamedCorrelationIdentifier("candidate_diamond_left")
	candidateRightAlias := values.NamedCorrelationIdentifier("candidate_diamond_right")

	queryQuantifiers := []expressions.Quantifier{
		expressions.NamedForEachQuantifier(
			queryBaseAlias,
			selectSubsumptionTestLeafRef(),
		),
		expressions.NamedExistentialQuantifier(
			queryLeftAlias,
			selectSubsumptionTestCorrelatedRef(queryBaseAlias),
		),
		expressions.NamedExistentialQuantifier(
			queryRightAlias,
			selectSubsumptionTestCorrelatedRef(queryBaseAlias),
		),
		expressions.NamedExistentialQuantifier(
			queryTipAlias,
			selectSubsumptionTestCorrelatedRefMany(
				queryLeftAlias,
				queryRightAlias,
			),
		),
	}
	candidateQuantifiers := []expressions.Quantifier{
		expressions.NamedForEachQuantifier(
			candidateBaseAlias,
			selectSubsumptionTestLeafRef(),
		),
		expressions.NamedExistentialQuantifier(
			candidateLeftAlias,
			selectSubsumptionTestCorrelatedRef(candidateBaseAlias),
		),
		expressions.NamedExistentialQuantifier(
			candidateRightAlias,
			selectSubsumptionTestCorrelatedRef(candidateBaseAlias),
		),
	}
	query := selectSubsumptionTestSelect(
		queryQuantifiers,
		[]predicates.QueryPredicate{
			predicates.NewExistentialAlias(queryLeftAlias),
			predicates.NewExistentialAlias(queryRightAlias),
			predicates.NewExistentialAlias(queryTipAlias),
		},
		expressions.JoinInner,
	)
	candidate := selectSubsumptionTestSelect(
		candidateQuantifiers,
		nil,
		expressions.JoinInner,
	)

	tests := []struct {
		name    string
		mapping []quantifierMapping
		want    bool
	}{
		{
			name: "both diamond branches selected",
			mapping: []quantifierMapping{
				{queryIndex: 0, candidateIndex: 0},
				{queryIndex: 1, candidateIndex: 1},
				{queryIndex: 2, candidateIndex: 2},
			},
			want: true,
		},
		{
			name: "left diamond branch omitted",
			mapping: []quantifierMapping{
				{queryIndex: 0, candidateIndex: 0},
				{queryIndex: 2, candidateIndex: 2},
			},
			want: false,
		},
		{
			name: "right diamond branch omitted",
			mapping: []quantifierMapping{
				{queryIndex: 0, candidateIndex: 0},
				{queryIndex: 1, candidateIndex: 1},
			},
			want: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, ok := validateSelectSubsumptionMapping(
				query,
				candidate,
				test.mapping,
			); ok != test.want {
				t.Fatalf(
					"validateSelectSubsumptionMapping() ok = %v, want %v",
					ok,
					test.want,
				)
			}
		})
	}
}

func TestSelectSubsumptionUnmatchedLocalCorrelationRequiresExistential(
	t *testing.T,
) {
	ref := selectSubsumptionTestLeafRef()
	forEachAlias := values.NamedCorrelationIdentifier("local_for_each")
	quantifiers := []expressions.Quantifier{
		expressions.NamedForEachQuantifier(forEachAlias, ref),
	}
	queryPredicates := []predicates.QueryPredicate{
		predicates.NewComparisonPredicate(
			values.NewQuantifiedObjectValue(forEachAlias),
			predicates.NewLiteralComparison(
				predicates.ComparisonEquals,
				int64(1),
			),
		),
	}
	order := buildQuantifierDependencyOrder(quantifiers)
	if !order.ok {
		t.Fatal("expected valid dependency order")
	}
	if selectSubsumptionUnmatchedLocalCorrelationsValid(
		queryPredicates,
		quantifiers,
		map[int]struct{}{},
		order.transitive,
	) {
		t.Fatal("expected an unmatched local ForEach correlation to be rejected")
	}
}

func selectSubsumptionTestSelect(
	quantifiers []expressions.Quantifier,
	queryPredicates []predicates.QueryPredicate,
	joinType expressions.JoinType,
) *expressions.SelectExpression {
	return expressions.NewSelectExpressionWithJoinType(
		values.LiteralValue(int64(1)),
		quantifiers,
		queryPredicates,
		nil,
		joinType,
	)
}

func selectSubsumptionTestLeafRef() *expressions.Reference {
	return expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression(
			[]string{"select_subsumption_test"},
			values.UnknownType,
		),
	)
}

func selectSubsumptionTestCorrelatedRef(
	dependency values.CorrelationIdentifier,
) *expressions.Reference {
	return selectSubsumptionTestCorrelatedRefMany(dependency)
}

func selectSubsumptionTestCorrelatedRefMany(
	dependencies ...values.CorrelationIdentifier,
) *expressions.Reference {
	inner := expressions.NamedForEachQuantifier(
		values.UniqueCorrelationIdentifier(),
		selectSubsumptionTestLeafRef(),
	)
	correlatedPredicates := make(
		[]predicates.QueryPredicate,
		0,
		len(dependencies),
	)
	for _, dependency := range dependencies {
		correlatedPredicates = append(
			correlatedPredicates,
			predicates.NewComparisonPredicate(
				values.NewQuantifiedObjectValue(dependency),
				predicates.NewLiteralComparison(
					predicates.ComparisonEquals,
					int64(1),
				),
			),
		)
	}
	return expressions.InitialOf(
		expressions.NewLogicalFilterExpression(
			correlatedPredicates,
			inner,
		),
	)
}

func selectSubsumptionTestPartialMatch(
	queryExpression expressions.RelationalExpression,
) *PartialMatchImpl {
	matchInfo := NewRegularMatchInfo(
		nil,
		EmptyAliasMap(),
		nil,
		nil,
		nil,
		EmptyGroupByMappings(),
		nil,
		nil,
	)
	return NewPartialMatch(
		EmptyAliasMap(),
		nil,
		expressions.InitialOf(queryExpression),
		queryExpression,
		nil,
		matchInfo,
	)
}

func selectSubsumptionTestMappingsEqual(
	left, right []quantifierMapping,
) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
