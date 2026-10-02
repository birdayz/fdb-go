package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// selectMergeDeclinedError keeps an untranslatable graph out of the merged
// alternative; keeping its original edge would leave a dissolved alias unbound.
type selectMergeDeclinedError struct{}

func (*selectMergeDeclinedError) Error() string { return "select merge cannot translate graph" }

type selectMergeTranslation struct {
	functions map[values.CorrelationIdentifier]TranslationFunction
	seeds     map[values.CorrelationIdentifier]values.Value
	targets   map[values.CorrelationIdentifier]map[values.CorrelationIdentifier]struct{}
	cache     map[*expressions.Reference]*expressions.Reference
	active    map[*expressions.Reference]bool
	call      *ExpressionRuleCall
}

func newSelectMergeTranslation(call *ExpressionRuleCall) *selectMergeTranslation {
	return &selectMergeTranslation{
		functions: make(map[values.CorrelationIdentifier]TranslationFunction),
		seeds:     make(map[values.CorrelationIdentifier]values.Value),
		targets:   make(map[values.CorrelationIdentifier]map[values.CorrelationIdentifier]struct{}),
		cache:     make(map[*expressions.Reference]*expressions.Reference),
		active:    make(map[*expressions.Reference]bool),
		call:      call,
	}
}

func (tr *selectMergeTranslation) add(alias values.CorrelationIdentifier, result values.Value, positional bool) {
	if positional {
		tr.seeds[alias] = result
	} else {
		tr.functions[alias] = func(values.CorrelationIdentifier, values.LeafValue) values.Value { return result }
	}
	tr.targets[alias] = values.GetCorrelatedToOfValue(result)
	clear(tr.cache)
}

func (tr *selectMergeTranslation) contains(alias values.CorrelationIdentifier) bool {
	_, ordinary := tr.functions[alias]
	_, seed := tr.seeds[alias]
	return ordinary || seed
}

func (tr *selectMergeTranslation) withoutBindings(qs []expressions.Quantifier) *selectMergeTranslation {
	bound := make(map[values.CorrelationIdentifier]bool, len(qs))
	shadowed := false
	for _, q := range qs {
		bound[q.GetAlias()] = true
		shadowed = shadowed || tr.contains(q.GetAlias())
	}
	if !shadowed {
		return tr
	}
	copy := newSelectMergeTranslation(tr.call)
	for alias, fn := range tr.functions {
		if !bound[alias] {
			copy.functions[alias] = fn
		}
	}
	for alias, value := range tr.seeds {
		if !bound[alias] {
			copy.seeds[alias] = value
		}
	}
	for alias, targets := range tr.targets {
		if !bound[alias] {
			copy.targets[alias] = targets
		}
	}
	return copy
}

func (tr *selectMergeTranslation) withAliases(aliases map[values.CorrelationIdentifier]values.CorrelationIdentifier) *selectMergeTranslation {
	if len(aliases) == 0 {
		return tr
	}
	copy := newSelectMergeTranslation(tr.call)
	for alias, fn := range tr.functions {
		copy.functions[alias] = fn
	}
	for alias, value := range tr.seeds {
		copy.seeds[alias] = value
	}
	for alias, targets := range tr.targets {
		copy.targets[alias] = targets
	}
	for source, target := range aliases {
		copy.functions[source] = func(_ values.CorrelationIdentifier, leaf values.LeafValue) values.Value {
			return leaf.RebaseLeaf(target)
		}
		delete(copy.seeds, source)
		copy.targets[source] = map[values.CorrelationIdentifier]struct{}{target: {}}
	}
	return copy
}

func (tr *selectMergeTranslation) value(value values.Value) (values.Value, error) {
	translated, ok := translateValueCorrelations(value, &RegularTranslationMap{aliasToFunctionMap: tr.functions})
	if !ok {
		return nil, &selectMergeDeclinedError{}
	}
	if len(tr.seeds) > 0 {
		translated = values.Replace(translated, bakedBoxRefCallback(tr.seeds))
	}
	return translated, nil
}

func (tr *selectMergeTranslation) predicates(preds []predicates.QueryPredicate) ([]predicates.QueryPredicate, error) {
	translated := make([]predicates.QueryPredicate, len(preds))
	for i, predicate := range preds {
		var err error
		translated[i], err = predicates.TransformEmbeddedValuesChecked(predicate, tr.value)
		if err != nil {
			return nil, err
		}
	}
	return translated, nil
}

func (tr *selectMergeTranslation) quantifier(q expressions.Quantifier) (expressions.Quantifier, error) {
	ref, err := tr.reference(q.GetRangesOver())
	if err != nil {
		return expressions.Quantifier{}, err
	}
	return expressions.RebuildQuantifier(q, ref), nil
}

// Like Java References.rebaseGraphs, preserve common subexpressions and both
// member lanes. Local bindings shadow substitutions in the named-alias model.
func (tr *selectMergeTranslation) reference(ref *expressions.Reference) (*expressions.Reference, error) {
	if ref == nil {
		return ref, nil
	}
	ref = ref.Canonical()
	if cached, ok := tr.cache[ref]; ok {
		return cached, nil
	}
	hit := false
	for alias := range ref.GetCorrelatedTo() {
		hit = hit || tr.contains(alias)
	}
	if !hit {
		return ref, nil
	}
	if tr.active[ref] {
		return nil, &selectMergeDeclinedError{}
	}
	tr.active[ref] = true
	defer delete(tr.active, ref)

	var result *expressions.Reference
	changed := false
	for _, lane := range []expressions.ReferenceMemberSet{expressions.ReferenceExploratoryMembers, expressions.ReferenceFinalMembers} {
		members := ref.Members()
		if lane == expressions.ReferenceFinalMembers {
			members = ref.FinalMembers()
		}
		for _, member := range members {
			translated, err := tr.expression(member)
			if err != nil {
				return nil, err
			}
			changed = changed || translated != member
			if result == nil {
				if lane == expressions.ReferenceFinalMembers {
					if ref.IsPinnedFinal() {
						result = expressions.PinnedFinalOf(translated)
					} else {
						result = expressions.FinalOfAtStage(translated, ref.Stage())
					}
				} else {
					result = expressions.ExploratoryOfAtStage(translated, ref.Stage())
				}
			} else if lane == expressions.ReferenceFinalMembers {
				result.InsertFinal(translated)
			} else {
				result.Insert(translated)
			}
		}
	}
	if !changed {
		return ref, nil
	}
	if tr.call.memo != nil {
		tr.call.memo.ScheduleFreshReference(result)
	}
	tr.cache[ref] = result
	return result, nil
}

func (tr *selectMergeTranslation) expression(member expressions.RelationalExpression) (expressions.RelationalExpression, error) {
	qs := member.GetQuantifiers()
	scoped := tr.withoutBindings(qs)
	incoming := make(map[values.CorrelationIdentifier]struct{})
	for source := range expressions.GetCorrelatedToOfExpression(member) {
		for target := range scoped.targets[source] {
			incoming[target] = struct{}{}
		}
	}
	renamed := make(map[values.CorrelationIdentifier]values.CorrelationIdentifier)
	for _, q := range qs {
		if _, captures := incoming[q.GetAlias()]; captures {
			renamed[q.GetAlias()] = values.UniqueCorrelationIdentifier()
		}
	}
	scoped = scoped.withAliases(renamed)
	children := tr
	if member.CanCorrelate() {
		children = scoped
	}
	translatedQs := make([]expressions.Quantifier, len(qs))
	changed := false
	for i, q := range qs {
		var err error
		translatedQs[i], err = children.quantifier(q)
		if err != nil {
			return nil, err
		}
		if alias, renamed := renamed[q.GetAlias()]; renamed {
			translatedQs[i] = translatedQs[i].WithAlias(alias)
		}
		changed = changed || translatedQs[i] != q
	}
	nodeHit := false
	for alias := range member.GetCorrelatedToWithoutChildren() {
		nodeHit = nodeHit || scoped.contains(alias)
	}
	if !nodeHit {
		if !changed {
			return member, nil
		}
		return member.WithQuantifiers(translatedQs)
	}
	switch e := member.(type) {
	case *expressions.SelectExpression:
		rv, err := scoped.value(e.GetResultValue())
		if err != nil {
			return nil, err
		}
		preds, err := scoped.predicates(e.GetPredicates())
		if err != nil {
			return nil, err
		}
		return e.WithTranslatedValues(rv, translatedQs, preds)
	case *expressions.LogicalFilterExpression:
		preds, err := scoped.predicates(e.GetPredicates())
		if err != nil {
			return nil, err
		}
		return expressions.NewLogicalFilterExpression(preds, translatedQs[0])
	case *expressions.ExplodeExpression:
		collection, err := scoped.value(e.GetCollectionValue())
		if err != nil {
			return nil, err
		}
		return e.WithCollection(collection)
	case *expressions.LogicalProjectionExpression:
		projected := make([]values.Value, len(e.GetProjectedValues()))
		for i, value := range e.GetProjectedValues() {
			var err error
			projected[i], err = scoped.value(value)
			if err != nil {
				return nil, err
			}
		}
		projection, err := expressions.NewLogicalProjectionExpressionWithAliasProvenance(projected, e.GetAliases(), e.GetAliasMinted(), translatedQs[0])
		if err != nil {
			return nil, err
		}
		return projection.WithInheritedOutputIdentity(e).WithAliasSources(e.GetAliasSources())
	case *expressions.LogicalSortExpression:
		keys := append([]expressions.SortKey(nil), e.GetSortKeys()...)
		for i := range keys {
			var err error
			keys[i].Value, err = scoped.value(keys[i].Value)
			if err != nil {
				return nil, err
			}
		}
		return expressions.NewLogicalSortExpression(keys, translatedQs[0])
	case *expressions.GroupByExpression:
		keys := e.GetGroupingKeys()
		for i, value := range keys {
			var err error
			keys[i], err = scoped.value(value)
			if err != nil {
				return nil, err
			}
		}
		aggregates := e.GetAggregates()
		for i := range aggregates {
			var err error
			aggregates[i].Operand, err = scoped.value(aggregates[i].Operand)
			if err != nil {
				return nil, err
			}
		}
		return expressions.NewGroupByExpression(keys, aggregates, translatedQs[0])
	case *expressions.TableFunctionExpression:
		value, err := scoped.value(e.GetValue())
		if err != nil {
			return nil, err
		}
		return expressions.NewTableFunctionExpression(value)
	default:
		return nil, &selectMergeDeclinedError{}
	}
}
