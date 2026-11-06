package cascades

import (
	"slices"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TextIndexMatchCandidate describes a text access path. Text comparisons never
// enter ordinary ComparisonRanges: their posting streams require a text scan.
type TextIndexMatchCandidate struct {
	Name                string
	Columns             []string
	RecordTypes         []string
	GroupingCount       int
	TokenizerName       string
	TokenizerVersion    int
	TokenizerMaxVersion int
	OmitPositions       bool
	CreatesDuplicates   bool
	RowType             values.Type
	KeyTypes            []values.Type
}

func (c *TextIndexMatchCandidate) CandidateName() string                              { return c.Name }
func (c *TextIndexMatchCandidate) GetTraversal() *Traversal                           { return nil }
func (c *TextIndexMatchCandidate) GetColumnNames() []string                           { return nil }
func (c *TextIndexMatchCandidate) GetSargableAliases() []values.CorrelationIdentifier { return nil }
func (c *TextIndexMatchCandidate) GetRecordTypes() []string                           { return c.RecordTypes }

func (c *TextIndexMatchCandidate) IsUnique() bool { return false }

func (c *TextIndexMatchCandidate) ComputeBoundParameterPrefixMap(map[values.CorrelationIdentifier]*predicates.ComparisonRange) map[values.CorrelationIdentifier]*predicates.ComparisonRange {
	return nil
}

func (c *TextIndexMatchCandidate) ToScanPlan(PlanContext, map[values.CorrelationIdentifier]*predicates.ComparisonRange, bool) plans.RecordQueryPlan {
	return nil
}

// textAccessAlternatives matches only a base-record scan. Joins, fan-outs and
// computed projections cannot be substituted by a scan over a different domain.
func textAccessAlternatives(ctx PlanContext, inner plans.RecordQueryPlan, preds []predicates.QueryPredicate, alias values.CorrelationIdentifier) ([]plans.RecordQueryPlan, [][]predicates.QueryPredicate) {
	residualPredicates, err := predicates.ToResidualPredicates(preds)
	if err != nil {
		return nil, nil
	}
	preds = predicates.FlattenConjunction(residualPredicates)
	var types []string
	if filtered, ok := inner.(*plans.RecordQueryTypeFilterPlan); ok {
		types = filtered.GetRecordTypes()
		inner = filtered.GetInner()
	}
	scan, ok := inner.(*plans.RecordQueryScanPlan)
	if !ok || len(scan.GetScanComparisons()) != 0 {
		return nil, nil
	}
	if len(types) == 0 {
		types = scan.GetRecordTypes()
	}
	var result []plans.RecordQueryPlan
	var residuals [][]predicates.QueryPredicate
	for _, candidate := range ctx.GetMatchCandidates() {
		c, ok := candidate.(*TextIndexMatchCandidate)
		if !ok || c.GroupingCount < 0 || c.GroupingCount >= len(c.Columns) {
			continue
		}
		if len(types) != 1 || len(c.RecordTypes) != 1 || types[0] != c.RecordTypes[0] {
			continue
		}
		for textIndex, pred := range preds {
			cp, ok := pred.(*predicates.ComparisonPredicate)
			if !ok || !cp.Comparison.Type.IsText() {
				continue
			}
			field, ok := values.AsFieldValue(cp.Operand)
			if !ok || !textFieldMatchesColumn(field, c.Columns[c.GroupingCount], alias) || len(values.GetCorrelatedToOfValue(cp.Comparison.Operand)) != 0 {
				continue
			}
			requestedTokenizer := cp.Comparison.TextTokenizerName
			// Metadata upgrades retain older postings. Only initial version 0
			// proves a named latest-version scan has no earlier posting version.
			if requestedTokenizer != "" && (requestedTokenizer != c.TokenizerName || c.TokenizerVersion != c.TokenizerMaxVersion || c.TokenizerVersion != 0) {
				continue
			}
			typ := cp.Comparison.Type
			if c.OmitPositions && typ != predicates.ComparisonTextContainsAll && typ != predicates.ComparisonTextContainsAny && typ != predicates.ComparisonTextContainsPrefix {
				continue
			}
			grouping := make([]values.Value, c.GroupingCount)
			// A strict probable intersection keeps its predicate as a filter
			// (RecordQueryPlanner.java:1721-1728).
			strictPrefixes := typ == predicates.ComparisonTextContainsAllPrefixes && cp.Comparison.TextStrictPrefix
			// Current options cannot certify positions in historical postings.
			positional := typ == predicates.ComparisonTextContainsPhrase || typ == predicates.ComparisonTextContainsAllWithin
			consumed := map[int]bool{textIndex: !strictPrefixes && !positional}
			for i, col := range c.Columns[:c.GroupingCount] {
				for j, p := range preds {
					eq, ok := p.(*predicates.ComparisonPredicate)
					if !ok || eq.Comparison.Type != predicates.ComparisonEquals {
						continue
					}
					f, ok := values.AsFieldValue(eq.Operand)
					// An outer binding (an IN element, a join column) is a group;
					// a value of the scanned row is not.
					_, selfCorrelated := values.GetCorrelatedToOfValue(eq.Comparison.Operand)[alias]
					if ok && textFieldMatchesColumn(f, col, alias) && !selfCorrelated {
						grouping[i] = eq.Comparison.Operand
						consumed[j] = true
						break
					}
				}
			}
			complete := true
			for _, g := range grouping {
				if g == nil {
					complete = false
				}
			}
			if !complete {
				continue
			}
			comparison := cp.Comparison
			plan, err := plans.NewRecordQueryTextIndexPlan(c.Name, plans.TextScan{IndexName: c.Name, TextComparison: typ.Symbol(), Predicate: &comparison, Grouping: grouping, KeyTypes: c.KeyTypes}, c.RowType, false)
			if err != nil {
				continue
			}
			var access plans.RecordQueryPlan = plan
			if c.CreatesDuplicates || typ.IsTextPrefix() {
				distinct, e := plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlan(access)
				if e != nil {
					continue
				}
				access = distinct
			}
			residual := make([]predicates.QueryPredicate, 0, len(preds))
			for i, p := range preds {
				if !consumed[i] {
					residual = append(residual, p)
				}
			}
			result = append(result, access)
			residuals = append(residuals, residual)
		}
	}
	return result, residuals
}

// Resolve the metadata path against the same exact input row, then compare
// ordinal addresses rather than using a diagnostic field label as authority.
func textFieldMatchesColumn(field values.FieldValue, column string, alias values.CorrelationIdentifier) bool {
	root, ok := values.AsQuantifiedObjectValue(field.ChildValue())
	if !ok || root.Correlation() != alias {
		return false
	}
	request, err := values.FieldByName(column)
	if err != nil {
		return false
	}
	expected, err := values.ResolveFieldAccess(field.ChildValue(), []values.FieldRequest{request})
	if err != nil {
		return false
	}
	resolved, ok := values.AsFieldValue(expected)
	return ok && slices.Equal(field.Path().Ordinals(), resolved.Path().Ordinals())
}

var (
	_ MatchCandidate                   = (*TextIndexMatchCandidate)(nil)
	_ expressions.RelationalExpression = (*plans.RecordQueryTextIndexPlan)(nil)
)
