package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

type existentialCompensationTestState struct {
	needed     bool
	impossible bool
	filtering  bool
	final      bool
}

func (c *existentialCompensationTestState) IsNeeded() bool {
	return c.needed
}

func (c *existentialCompensationTestState) IsImpossible() bool {
	return c.impossible
}

func (c *existentialCompensationTestState) IsNeededForFiltering() bool {
	return c.filtering
}

func (c *existentialCompensationTestState) IsFinalNeeded() bool {
	return c.final
}

func (*existentialCompensationTestState) CanBeDeferred() bool {
	return true
}

type existentialCompensationTestChild struct {
	compensation Compensation
	prefix       map[values.CorrelationIdentifier]*predicates.ComparisonRange
	calls        int
}

func (*existentialCompensationTestChild) GetMatchCandidate() MatchCandidate {
	return nil
}

func (*existentialCompensationTestChild) GetMatchInfo() MatchInfo {
	return nil
}

func (*existentialCompensationTestChild) GetBoundAliasMap() *AliasMap {
	return EmptyAliasMap()
}

func (*existentialCompensationTestChild) GetQueryRef() *expressions.Reference {
	return nil
}

func (*existentialCompensationTestChild) GetQueryExpression() expressions.RelationalExpression {
	return nil
}

func (*existentialCompensationTestChild) GetCandidateRef() *expressions.Reference {
	return nil
}

func (*existentialCompensationTestChild) GetRegularMatchInfo() *RegularMatchInfo {
	return nil
}

func (c *existentialCompensationTestChild) CompensateExistential(
	prefix map[values.CorrelationIdentifier]*predicates.ComparisonRange,
) Compensation {
	c.calls++
	c.prefix = prefix
	return c.compensation
}

func existentialCompensationTestParent(
	t *testing.T,
	alias values.CorrelationIdentifier,
	kind expressions.QuantifierKind,
	child PartialMatch,
	setChild bool,
) *PartialMatchImpl {
	t.Helper()
	childRef := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression(
			[]string{"E"},
			values.UnknownType,
		),
	)
	var quantifier expressions.Quantifier
	switch kind {
	case expressions.QuantifierExistential:
		quantifier = expressions.NamedExistentialQuantifier(alias, childRef)
	case expressions.QuantifierForEach:
		quantifier = expressions.NamedForEachQuantifier(alias, childRef)
	default:
		t.Fatalf("unsupported owner kind %v", kind)
	}
	queryExpression := expressions.NewSelectExpression(
		values.NewRecordConstructorValue(),
		[]expressions.Quantifier{quantifier},
		[]predicates.QueryPredicate{predicates.NewExistentialAlias(alias)},
	)
	queryRef := expressions.InitialOf(queryExpression)
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
	if setChild {
		matchInfo.SetChildPartialMatch(alias, child)
	}
	return NewPartialMatch(
		EmptyAliasMap(),
		nil,
		queryRef,
		queryExpression,
		queryRef,
		matchInfo,
	)
}

func existentialCompensationTestSemanticMapping(
	t *testing.T,
	queryPredicate *predicates.ExistentialValuePredicate,
	queryAlias values.CorrelationIdentifier,
) *PredicateMapping {
	t.Helper()
	candidateAlias := values.NamedCorrelationIdentifier(
		queryAlias.Name() + "_candidate",
	)
	candidateRef := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression(
			[]string{"E"},
			values.UnknownType,
		),
	)
	candidatePredicate := predicates.NewExistentialAlias(candidateAlias)
	mapping, ok := selectSubsumptionPredicateImpliedMappingMaybe(
		queryPredicate,
		queryPredicate,
		candidatePredicate,
		[]expressions.Quantifier{
			expressions.NamedExistentialQuantifier(
				candidateAlias,
				candidateRef,
			),
		},
		AliasMapOfAliases(queryAlias, candidateAlias),
	)
	if !ok || mapping == nil {
		t.Fatal("semantic-equality EVP mapping was not constructed")
	}
	if mapping.predicateCompensationIdentity != "select-existential-child" {
		t.Fatalf(
			"EVP compensation identity = %q, want select-existential-child",
			mapping.predicateCompensationIdentity,
		)
	}
	return mapping
}

func TestSelectSubsumptionExistentialCompensation_SemanticEqualityChildStates(
	t *testing.T,
) {
	queryAlias := values.NamedCorrelationIdentifier("evp_query_owner")
	queryPredicate := predicates.NewExistentialAlias(queryAlias)
	mapping := existentialCompensationTestSemanticMapping(
		t,
		queryPredicate,
		queryAlias,
	)
	parameterAlias := values.NamedCorrelationIdentifier("evp_bound_parameter")
	comparison := predicates.NewLiteralComparison(
		predicates.ComparisonEquals,
		int64(4),
	)
	boundRange := predicates.EmptyComparisonRange().Merge(&comparison).Range
	prefix := map[values.CorrelationIdentifier]*predicates.ComparisonRange{
		parameterAlias: boundRange,
	}

	var typedNilCompensation *existentialCompensationTestState
	var typedNilChild *existentialCompensationTestChild
	tests := []struct {
		name        string
		child       PartialMatch
		setChild    bool
		wantReapply bool
		wantCall    bool
	}{
		{
			name:        "missing child",
			wantReapply: true,
		},
		{
			name: "filtering child",
			child: &existentialCompensationTestChild{
				compensation: &existentialCompensationTestState{
					needed:    true,
					filtering: true,
				},
			},
			setChild:    true,
			wantReapply: true,
			wantCall:    true,
		},
		{
			name: "impossible child",
			child: &existentialCompensationTestChild{
				compensation: ImpossibleCompensation,
			},
			setChild:    true,
			wantReapply: true,
			wantCall:    true,
		},
		{
			name: "possible no-filter child",
			child: &existentialCompensationTestChild{
				compensation: &existentialCompensationTestState{
					needed: true,
				},
			},
			setChild: true,
			wantCall: true,
		},
		{
			name: "result-only child",
			child: &existentialCompensationTestChild{
				compensation: &existentialCompensationTestState{
					needed: true,
					final:  true,
				},
			},
			setChild: true,
			wantCall: true,
		},
		{
			name: "no child compensation",
			child: &existentialCompensationTestChild{
				compensation: NoCompensation,
			},
			setChild: true,
			wantCall: true,
		},
		{
			name: "nil child compensation",
			child: &existentialCompensationTestChild{
				compensation: nil,
			},
			setChild:    true,
			wantReapply: true,
			wantCall:    true,
		},
		{
			name: "typed-nil child compensation",
			child: &existentialCompensationTestChild{
				compensation: typedNilCompensation,
			},
			setChild:    true,
			wantReapply: true,
			wantCall:    true,
		},
		{
			name:        "typed-nil child",
			child:       typedNilChild,
			setChild:    true,
			wantReapply: true,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			parent := existentialCompensationTestParent(
				t,
				queryAlias,
				expressions.QuantifierExistential,
				test.child,
				test.setChild,
			)
			compensationFunction := mapping.GetPredicateCompensation()(
				parent,
				prefix,
				nil,
			)
			if compensationFunction == nil {
				t.Fatal("EVP compensation factory returned nil")
			}
			if got := compensationFunction.IsNeeded(); got != test.wantReapply {
				t.Fatalf(
					"EVP reapply needed = %v, want %v",
					got,
					test.wantReapply,
				)
			}
			if compensationFunction.IsImpossible() {
				t.Fatal("EVP reapply must remain a possible predicate compensation")
			}
			applied := compensationFunction.ApplyCompensationForPredicate(
				NewTranslationMapBuilder().
					When(queryAlias).
					Then(func(
						values.CorrelationIdentifier,
						values.LeafValue,
					) values.Value {
						return values.NewQuantifiedObjectValue(
							values.NamedCorrelationIdentifier(
								"must_not_rebase_evp",
							),
						)
					}).
					Build(),
			)
			if test.wantReapply {
				if len(applied) != 1 || applied[0] != queryPredicate {
					t.Fatalf(
						"reapplied EVP = %v, want original predicate identity",
						applied,
					)
				}
			} else if len(applied) != 0 {
				t.Fatalf("unneeded EVP compensation applied %d predicates", len(applied))
			}

			child, isTestChild := test.child.(*existentialCompensationTestChild)
			if !isTestChild || child == nil {
				return
			}
			if gotCall := child.calls == 1; gotCall != test.wantCall {
				t.Fatalf(
					"child compensation called = %v, want %v",
					gotCall,
					test.wantCall,
				)
			}
			if test.wantCall && child.prefix[parameterAlias] != boundRange {
				t.Fatal("bound parameter prefix was not forwarded unchanged")
			}
		})
	}
}

func TestSelectSubsumptionExistentialCompensation_ResidualAndNilSafety(
	t *testing.T,
) {
	queryAlias := values.NamedCorrelationIdentifier("evp_residual_owner")
	queryPredicate := predicates.NewExistentialAlias(queryAlias)
	parameterBindings, predicateMap, ok := buildSelectSubsumptionPredicateAlternative(
		[]predicates.QueryPredicate{queryPredicate},
		[]predicates.QueryPredicate{queryPredicate},
		nil,
		nil,
	)
	if !ok || len(parameterBindings) != 0 || predicateMap == nil {
		t.Fatal("residual EVP alternative was not built")
	}
	mappings := predicateMap.Get(queryPredicate)
	if len(mappings) != 1 ||
		mappings[0].predicateCompensationIdentity !=
			"select-existential-child" {
		t.Fatal("residual EVP did not receive child-aware compensation")
	}

	child := &existentialCompensationTestChild{
		compensation: NoCompensation,
	}
	parent := existentialCompensationTestParent(
		t,
		queryAlias,
		expressions.QuantifierExistential,
		child,
		true,
	)
	if fn := mappings[0].GetPredicateCompensation()(
		parent,
		nil,
		nil,
	); fn.IsNeeded() {
		t.Fatal("residual EVP was reapplied despite a no-filter child")
	}

	candidateTrueMapping, implied := selectSubsumptionPredicateImpliedMappingMaybe(
		queryPredicate,
		queryPredicate,
		predicates.NewConstantPredicate(predicates.TriTrue),
		nil,
		EmptyAliasMap(),
	)
	if !implied ||
		candidateTrueMapping == nil ||
		candidateTrueMapping.predicateCompensationIdentity !=
			"select-existential-child" {
		t.Fatal("candidate TRUE EVP mapping did not receive child-aware compensation")
	}
	if fn := candidateTrueMapping.GetPredicateCompensation()(
		parent,
		nil,
		nil,
	); fn.IsNeeded() {
		t.Fatal("candidate TRUE EVP was reapplied despite a no-filter child")
	}

	var typedNilParent *PartialMatchImpl
	for name, malformedParent := range map[string]PartialMatch{
		"nil":       nil,
		"typed nil": typedNilParent,
	} {
		t.Run(name+" parent", func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("nil parent panicked: %v", recovered)
				}
			}()
			fn := mappings[0].GetPredicateCompensation()(
				malformedParent,
				nil,
				nil,
			)
			if !fn.IsNeeded() || fn.IsImpossible() {
				t.Fatal("nil parent did not fail safe by reapplying the EVP")
			}
		})
	}

	foreignParent := existentialCompensationTestParent(
		t,
		values.NamedCorrelationIdentifier("other_owner"),
		expressions.QuantifierExistential,
		nil,
		false,
	)
	if fn := mappings[0].GetPredicateCompensation()(
		foreignParent,
		nil,
		nil,
	); fn.IsNeeded() {
		t.Fatal("outer-owned EVP was compensated at the wrong match level")
	}

	wrongKindParent := existentialCompensationTestParent(
		t,
		queryAlias,
		expressions.QuantifierForEach,
		nil,
		false,
	)
	if fn := mappings[0].GetPredicateCompensation()(
		wrongKindParent,
		nil,
		nil,
	); !fn.IsNeeded() || fn.IsImpossible() {
		t.Fatal("wrong-kind EVP owner did not fail safe by reapplying")
	}

	var nilPartialMatch *PartialMatchImpl
	if compensation := nilPartialMatch.CompensateExistential(nil); compensation == nil || !compensation.IsImpossible() {
		t.Fatal("typed-nil PartialMatch existential compensation did not fail closed")
	}
}

var (
	_ PartialMatch                        = (*existentialCompensationTestChild)(nil)
	_ existentialCompensatingPartialMatch = (*existentialCompensationTestChild)(nil)
)
