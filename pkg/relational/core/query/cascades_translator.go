package query

import (
	"strconv"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
)

// ScalarSubqueryPlan pairs a correlation alias with a logical operator
// tree for a scalar subquery. Collected during translation and passed
// to the executor for pre-evaluation.
type ScalarSubqueryPlan struct {
	Alias values.CorrelationIdentifier
	Plan  logical.LogicalOperator
}

// TranslateToCascades converts a logical.LogicalOperator tree into a
// cascades RelationalExpression tree rooted in a Reference. This is
// the bridge between the SQL parser's logical plan and the Cascades
// optimizer.
//
// Returns the root Reference suitable for passing to Planner.Plan().
// Returns nil if the operator tree contains shapes that can't be
// translated (unsupported operators fall through to nil).
func TranslateToCascades(op logical.LogicalOperator) *expressions.Reference {
	ref, _ := TranslateToCascadesWithSubqueries(op)
	return ref
}

// TranslateToCascadesWithSubqueries is like TranslateToCascades but
// also returns any scalar subquery plans collected during translation.
// These must be planned independently and pre-evaluated by the
// executor before running the main plan.
func TranslateToCascadesWithSubqueries(op logical.LogicalOperator) (*expressions.Reference, []ScalarSubqueryPlan) {
	t := &cascadesTranslator{
		cteScope:     make(map[string]logical.LogicalOperator),
		cteExprScope: make(map[string]expressions.RelationalExpression),
	}
	ref := t.translateRef(op)
	return ref, t.scalarSubqueries
}

type cascadesTranslator struct {
	cteScope         map[string]logical.LogicalOperator
	cteExprScope     map[string]expressions.RelationalExpression
	scalarSubqueries []ScalarSubqueryPlan
}

func (t *cascadesTranslator) translateRef(op logical.LogicalOperator) *expressions.Reference {
	expr := t.translateOp(op)
	if expr == nil {
		return nil
	}
	return expressions.InitialOf(expr)
}

func (t *cascadesTranslator) translateOp(op logical.LogicalOperator) expressions.RelationalExpression {
	if op == nil {
		return nil
	}
	switch o := op.(type) {
	case *logical.LogicalScan:
		return t.translateScan(o)
	case *logical.LogicalFilter:
		return t.translateFilter(o)
	case *logical.LogicalLimit:
		// Top-level LIMIT/OFFSET is applied post-execution by paginatingRows.
		// Skip the LogicalLimit wrapper here — inner-plan limits (e.g.
		// inside correlated scalar subqueries) are handled by
		// translateProjectWithCorrelatedScalar which peels the
		// LogicalLimit and emits a LogicalLimitExpression.
		return t.translateOp(o.Input)
	case *logical.LogicalUnion:
		return t.translateUnion(o)
	case *logical.LogicalSort:
		return t.translateSort(o)
	case *logical.LogicalProject:
		return t.translateProject(o)
	case *logical.LogicalJoin:
		return t.translateJoin(o)
	case *logical.LogicalAggregate:
		return t.translateAggregate(o)
	case *logical.LogicalDistinct:
		return t.translateDistinct(o)
	case *logical.LogicalCTE:
		return t.translateCTE(o)
	case *logical.LogicalInsert:
		return t.translateInsert(o)
	case *logical.LogicalUpdate:
		return t.translateUpdate(o)
	case *logical.LogicalDelete:
		return t.translateDelete(o)
	default:
		return nil
	}
}

func (t *cascadesTranslator) translateScan(s *logical.LogicalScan) expressions.RelationalExpression {
	key := strings.ToUpper(s.Table)
	// Pre-translated expression scope (recursive CTE references).
	if expr, ok := t.cteExprScope[key]; ok {
		return expr
	}
	if body, ok := t.cteScope[key]; ok {
		// Remove this CTE from scope while translating its body so that
		// scans inside the body resolve to real tables, not back to the
		// CTE itself (which would cause infinite recursion when the CTE
		// name shadows the underlying table name).
		delete(t.cteScope, key)
		result := t.translateOp(body)
		t.cteScope[key] = body
		return result
	}
	return expressions.NewFullUnorderedScanExpression(
		[]string{s.Table}, values.UnknownType)
}

func (t *cascadesTranslator) translateFilter(f *logical.LogicalFilter) expressions.RelationalExpression {
	if f.Predicate == nil && f.PredicateText != "" {
		return nil
	}
	if f.Predicate != nil && isBareFieldPredicate(f.Predicate) {
		return nil
	}
	if f.Predicate != nil && predicateContainsUnsafeFunction(f.Predicate) {
		return nil
	}

	// Collect scalar subquery plans — they'll be planned independently
	// and pre-evaluated by the executor.
	for _, ssq := range f.ScalarSubqueries {
		t.scalarSubqueries = append(t.scalarSubqueries, ScalarSubqueryPlan{
			Alias: ssq.Alias,
			Plan:  ssq.Plan,
		})
	}

	// EXISTS subqueries: when the filter carries existential subquery
	// plans, build a SelectExpression with ForEach + Existential
	// quantifiers. The ExistsPredicate in the predicate tree references
	// the existential alias; the planner's ImplementSimpleSelectRule
	// handles the existential quantifier via FirstOrDefaultPlan.
	if len(f.ExistsSubqueries) > 0 && f.Predicate != nil {
		// When the filter's input is a join, flatten into a single
		// SelectExpression with ForEach(left), ForEach(right), and
		// Existential(exists_scan). This avoids nesting one
		// SelectExpression (the join) inside another (the EXISTS filter),
		// which causes the Cascades planner to diverge. The NLJ rule
		// handles the 2+1 quantifier shape directly.
		if join, ok := f.Input.(*logical.LogicalJoin); ok {
			return t.translateJoinWithExists(join, f)
		}
	}

	// When the filter wraps an INNER join (FROM a, b WHERE ...), merge
	// the WHERE predicates into the SelectExpression so the NLJ rule
	// sees them as join predicates. For LEFT OUTER joins, the WHERE
	// must stay as a filter ABOVE the join — merging would turn WHERE
	// conditions into ON conditions, preventing NULL-padded rows from
	// being properly filtered.
	if join, ok := f.Input.(*logical.LogicalJoin); ok && f.Predicate != nil && len(f.ExistsSubqueries) == 0 && join.Kind != logical.JoinLeft && join.Kind != logical.JoinRight && join.Kind != logical.JoinFull {
		joinExpr := t.translateJoin(join)
		if joinExpr == nil {
			return nil
		}
		if sel, ok := joinExpr.(*expressions.SelectExpression); ok {
			merged := append(sel.GetPredicates(), f.Predicate)
			return expressions.NewSelectExpressionWithJoinType(
				sel.GetResultValue(),
				sel.GetQuantifiers(),
				merged,
				sel.GetSourceAliases(),
				sel.GetJoinType(),
			)
		}
	}

	innerRef := t.translateRef(f.Input)
	if innerRef == nil {
		return nil
	}

	if len(f.ExistsSubqueries) > 0 && f.Predicate != nil {
		outerAlias := sourceAlias(f.Input)
		outerQ := t.namedQuantifier(outerAlias, innerRef)
		quantifiers := []expressions.Quantifier{outerQ}

		allPreds := splitNonExistsPredicates(f.Predicate)
		allPreds = append(allPreds, extractExistsPredicates(f.Predicate)...)
		for _, esq := range f.ExistsSubqueries {
			subRef := t.translateRef(esq.Plan)
			if subRef == nil {
				return nil
			}
			existQ := expressions.NamedExistentialQuantifier(esq.Alias, subRef)
			quantifiers = append(quantifiers, existQ)
			if esq.JoinPredicate != nil {
				allPreds = append(allPreds, esq.JoinPredicate)
			}
		}

		var sourceAliases []string
		if outerAlias != "" {
			sourceAliases = []string{outerAlias}
			for _, esq := range f.ExistsSubqueries {
				innerA := sourceAlias(esq.Plan)
				sourceAliases = append(sourceAliases, innerA)
			}
		}

		resultValue := values.NewQuantifiedObjectValue(outerQ.GetAlias())
		return expressions.NewSelectExpressionWithAliases(
			resultValue,
			quantifiers,
			allPreds,
			sourceAliases,
		)
	}

	var preds []predicates.QueryPredicate
	if f.Predicate != nil {
		preds = []predicates.QueryPredicate{f.Predicate}
	}
	return expressions.NewLogicalFilterExpression(
		preds,
		t.namedQuantifier(sourceAlias(f.Input), innerRef),
	)
}

func valueContainsUnsafeScalarFunction(v values.Value) bool {
	unsafe := false
	values.WalkValue(v, func(node values.Value) bool {
		if sf, ok := node.(*values.ScalarFunctionValue); ok {
			if !values.IsCascadesSafeScalarFunction(sf.FuncName) {
				unsafe = true
				return false
			}
		}
		return true
	})
	return unsafe
}

func predicateContainsUnsafeFunction(p predicates.QueryPredicate) bool {
	unsafe := false
	predicates.WalkPredicate(p, func(qp predicates.QueryPredicate) bool {
		switch pred := qp.(type) {
		case *predicates.ComparisonPredicate:
			if valueContainsUnsafeScalarFunction(pred.Operand) {
				unsafe = true
				return false
			}
			if pred.Comparison.Operand != nil && valueContainsUnsafeScalarFunction(pred.Comparison.Operand) {
				unsafe = true
				return false
			}
		case *predicates.ValuePredicate:
			if valueContainsUnsafeScalarFunction(pred.Value) {
				unsafe = true
				return false
			}
		}
		return true
	})
	return unsafe
}

func isBareFieldPredicate(p predicates.QueryPredicate) bool {
	vp, ok := p.(*predicates.ValuePredicate)
	if !ok {
		return false
	}
	_, isField := vp.Value.(*values.FieldValue)
	return isField
}

func (t *cascadesTranslator) translateUnion(u *logical.LogicalUnion) expressions.RelationalExpression {
	quantifiers := make([]expressions.Quantifier, 0, len(u.Inputs))
	for _, branch := range u.Inputs {
		ref := t.translateRef(branch)
		if ref == nil {
			return nil
		}
		quantifiers = append(quantifiers, expressions.ForEachQuantifier(ref))
	}
	if u.Distinct {
		return nil
	}
	return expressions.NewLogicalUnionExpression(quantifiers)
}

func (t *cascadesTranslator) translateSort(s *logical.LogicalSort) expressions.RelationalExpression {
	innerRef := t.translateRef(s.Input)
	if innerRef == nil {
		return nil
	}
	sortKeys := make([]expressions.SortKey, len(s.Keys))
	for i, k := range s.Keys {
		nf := k.NullsFirst
		v := k.Value
		if v == nil {
			v = &values.FieldValue{Field: k.Expr, Typ: values.UnknownType}
		}
		sortKeys[i] = expressions.SortKey{
			Value:      v,
			Reverse:    k.Dir == logical.SortDesc,
			NullsFirst: &nf,
		}
	}
	return expressions.NewLogicalSortExpression(
		sortKeys,
		t.namedQuantifier(sourceAlias(s.Input), innerRef),
	)
}

func (t *cascadesTranslator) translateProject(p *logical.LogicalProject) expressions.RelationalExpression {
	// Collect scalar subquery plans from projections.
	for _, ssq := range p.ScalarSubqueries {
		t.scalarSubqueries = append(t.scalarSubqueries, ScalarSubqueryPlan{
			Alias: ssq.Alias,
			Plan:  ssq.Plan,
		})
	}

	if len(p.CorrelatedScalarSubqueries) > 1 {
		return nil
	}
	if len(p.CorrelatedScalarSubqueries) == 1 {
		return t.translateProjectWithCorrelatedScalar(p)
	}

	innerRef := t.translateRef(p.Input)
	if innerRef == nil {
		return nil
	}
	projected := make([]values.Value, len(p.Projections))
	for i, col := range p.Projections {
		if i < len(p.ProjectedValues) && p.ProjectedValues[i] != nil {
			projected[i] = p.ProjectedValues[i]
			continue
		}
		// Computed expression without a resolved Value — the walker
		// couldn't handle this shape. Bail so the query falls back.
		if i < len(p.IsComputed) && p.IsComputed[i] {
			return nil
		}
		projected[i] = &values.FieldValue{Field: strings.ToUpper(col), Typ: values.UnknownType}
	}
	return expressions.NewLogicalProjectionExpressionWithAliases(
		projected,
		p.Aliases,
		t.namedQuantifier(sourceAlias(p.Input), innerRef),
	)
}

func (t *cascadesTranslator) translateProjectWithCorrelatedScalar(p *logical.LogicalProject) expressions.RelationalExpression {
	csq := p.CorrelatedScalarSubqueries[0]

	outerRef := t.translateRef(p.Input)
	if outerRef == nil {
		return nil
	}
	outerAlias := sourceAlias(p.Input)
	outerQ := t.namedQuantifier(outerAlias, outerRef)

	// Peel LogicalLimit from the inner plan — translateOp skips it,
	// but for correlated scalar subqueries the limit must be in the
	// Cascades plan so the inner side returns at most N rows per
	// outer row.
	innerPlan := csq.InnerPlan
	var innerLimit *logical.LogicalLimit
	if lim, ok := innerPlan.(*logical.LogicalLimit); ok {
		innerLimit = lim
		innerPlan = lim.Input
	}

	innerRef := t.translateRef(innerPlan)
	if innerRef == nil {
		return nil
	}

	// Wrap with LogicalLimitExpression if the inner plan had a LIMIT.
	if innerLimit != nil {
		innerAlias := sourceAlias(innerPlan)
		limitQ := t.namedQuantifier(innerAlias, innerRef)
		limitExpr := expressions.NewLogicalLimitExpression(innerLimit.Limit, innerLimit.Offset, limitQ)
		innerRef = expressions.InitialOf(limitExpr)
	}

	innerQ := t.namedQuantifier(csq.InnerAlias, innerRef)

	resultValue := values.NewJoinMergeResultValue(
		outerQ.GetAlias(),
		innerQ.GetAlias(),
	)

	joinSelect := expressions.NewSelectExpressionWithJoinType(
		resultValue,
		[]expressions.Quantifier{outerQ, innerQ},
		nil,
		[]string{outerAlias, csq.InnerAlias},
		expressions.JoinLeftOuter,
	)
	joinRef := expressions.InitialOf(joinSelect)

	projected := make([]values.Value, len(p.Projections))
	innerCorr := values.NamedCorrelationIdentifier(csq.InnerAlias)
	for i, col := range p.Projections {
		if i < len(p.ProjectedValues) && p.ProjectedValues[i] != nil {
			projected[i] = replaceScalarSubqueryRef(p.ProjectedValues[i], csq, innerCorr)
			continue
		}
		if i < len(p.IsComputed) && p.IsComputed[i] {
			return nil
		}
		projected[i] = &values.FieldValue{Field: strings.ToUpper(col), Typ: values.UnknownType}
	}

	projQ := t.namedQuantifier("", joinRef)
	return expressions.NewLogicalProjectionExpressionWithAliases(
		projected,
		p.Aliases,
		projQ,
	)
}

func replaceScalarSubqueryRef(v values.Value, csq logical.CorrelatedScalarSubquery, innerCorr values.CorrelationIdentifier) values.Value {
	return values.Replace(v, func(node values.Value) values.Value {
		if ssq, ok := node.(*values.ScalarSubqueryValue); ok && ssq.Alias == csq.Alias {
			qualifiedName := strings.ToUpper(innerCorr.Name()) + "." + strings.ToUpper(csq.ScalarCol)
			return &values.FieldValue{Field: qualifiedName, Typ: values.UnknownType}
		}
		return node
	})
}

func (t *cascadesTranslator) translateDistinct(d *logical.LogicalDistinct) expressions.RelationalExpression {
	innerRef := t.translateRef(d.Input)
	if innerRef == nil {
		return nil
	}
	return expressions.NewLogicalDistinctExpression(
		t.namedQuantifier(sourceAlias(d.Input), innerRef))
}

// Go extension: Java's fdb-relational 4.11.1.0 does not support GROUP BY;
// its AstNormalizer rejects it with UNSUPPORTED_QUERY before reaching the planner.
func (t *cascadesTranslator) translateAggregate(a *logical.LogicalAggregate) expressions.RelationalExpression {
	if a.Having != "" && a.HavingPredicate == nil {
		return nil
	}
	for _, ssq := range a.HavingScalarSubqueries {
		t.scalarSubqueries = append(t.scalarSubqueries, ScalarSubqueryPlan{
			Alias: ssq.Alias,
			Plan:  ssq.Plan,
		})
	}
	innerRef := t.translateRef(a.Input)
	if innerRef == nil {
		return nil
	}
	groupKeys := make([]values.Value, len(a.GroupKeys))
	for i, key := range a.GroupKeys {
		if i < len(a.GroupKeyValues) && a.GroupKeyValues[i] != nil {
			groupKeys[i] = a.GroupKeyValues[i]
		} else {
			groupKeys[i] = &values.FieldValue{Field: key, Typ: values.UnknownType}
		}
	}
	aggSpecs := make([]expressions.AggregateSpec, 0, len(a.Aggregates))
	for i, aggText := range a.Aggregates {
		spec, ok := parseAggregateText(aggText)
		if !ok {
			return nil
		}
		if i < len(a.AggregateOperands) && a.AggregateOperands[i] != nil {
			if _, isArith := spec.Operand.(*values.ArithmeticValue); !isArith {
				spec.Operand = a.AggregateOperands[i]
			}
		}
		if i < len(a.Aliases) && a.Aliases[i] != "" {
			spec.Alias = strings.ToUpper(a.Aliases[i])
		}
		aggSpecs = append(aggSpecs, spec)
	}
	groupBy := expressions.NewGroupByExpression(
		groupKeys,
		aggSpecs,
		t.namedQuantifier(sourceAlias(a.Input), innerRef),
	)
	if a.HavingPredicate == nil {
		return groupBy
	}
	groupByRef := expressions.InitialOf(groupBy)

	// HAVING with EXISTS subqueries is not supported — the correlation
	// references pre-GROUP-BY scope (table columns) but the HAVING
	// evaluates in post-GROUP-BY scope (group keys + aggregates).
	// Java doesn't support this either (no test coverage). Return nil
	// so the planner produces "could not plan query" instead of
	// silently returning wrong results.
	if len(a.HavingExistsSubqueries) > 0 {
		return nil
	}

	return expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{a.HavingPredicate},
		expressions.ForEachQuantifier(groupByRef),
	)
}

func parseAggregateText(text string) (expressions.AggregateSpec, bool) {
	upper := strings.ToUpper(strings.TrimSpace(text))
	lparen := strings.Index(upper, "(")
	if lparen < 0 {
		return expressions.AggregateSpec{}, false
	}
	rparen := strings.LastIndex(upper, ")")
	if rparen < lparen {
		return expressions.AggregateSpec{}, false
	}
	funcName := strings.TrimSpace(upper[:lparen])
	operandText := strings.TrimSpace(upper[lparen+1 : rparen])

	var fn expressions.AggregateFunction
	switch funcName {
	case "COUNT":
		fn = expressions.AggCount
	case "SUM":
		fn = expressions.AggSum
	case "MIN":
		fn = expressions.AggMin
	case "MAX":
		fn = expressions.AggMax
	case "AVG":
		fn = expressions.AggAvg
	default:
		return expressions.AggregateSpec{}, false
	}

	if strings.HasPrefix(operandText, "DISTINCT ") {
		return expressions.AggregateSpec{}, false
	}

	var operand values.Value
	if operandText == "*" {
		operand = &values.ConstantValue{Value: nil, Typ: values.UnknownType}
	} else {
		operand = parseOperandValue(operandText)
	}

	return expressions.AggregateSpec{Function: fn, Operand: operand, OperandName: operandText}, true
}

func parseOperandValue(text string) values.Value {
	for _, op := range []struct {
		sym string
		op  values.ArithmeticOp
	}{
		{"+", values.OpAdd},
		{"-", values.OpSub},
		{"*", values.OpMul},
		{"/", values.OpDiv},
	} {
		idx := strings.Index(text, op.sym)
		if idx > 0 && idx < len(text)-1 {
			left := strings.TrimSpace(text[:idx])
			right := strings.TrimSpace(text[idx+1:])
			if left != "" && right != "" {
				return &values.ArithmeticValue{
					Op:    op.op,
					Left:  parseAtomValue(left),
					Right: parseAtomValue(right),
				}
			}
		}
	}
	return parseAtomValue(text)
}

func parseAtomValue(text string) values.Value {
	if n, err := strconv.ParseInt(text, 10, 64); err == nil {
		return &values.ConstantValue{Value: n, Typ: values.NullableLong}
	}
	if f, err := strconv.ParseFloat(text, 64); err == nil {
		return &values.ConstantValue{Value: f, Typ: values.NullableDouble}
	}
	return &values.FieldValue{Field: text, Typ: values.UnknownType}
}

func (t *cascadesTranslator) translateJoin(j *logical.LogicalJoin) expressions.RelationalExpression {
	// For RIGHT JOIN, swap branches and treat as LEFT JOIN. The NLJ
	// executor iterates the "outer" (left) and for each unmatched row
	// emits NULLs for the inner (right) columns. Swapping makes the
	// originally-right table the outer, which is exactly RIGHT JOIN
	// semantics. This matches the standard approach — Java's Cascades
	// doesn't distinguish RIGHT from LEFT either; the planner
	// normalises RIGHT → LEFT with swapped children.
	left := j.Left
	right := j.Right
	kind := j.Kind
	if kind == logical.JoinRight {
		left, right = right, left
		kind = logical.JoinLeft
	}

	leftRef := t.translateRef(left)
	if leftRef == nil {
		return nil
	}
	rightRef := t.translateRef(right)
	if rightRef == nil {
		return nil
	}
	leftAlias := sourceAlias(left)
	rightAlias := sourceAlias(right)

	// Use named quantifiers so aliases match the predicate QOV
	// correlations created by the SQL resolver.
	leftQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(leftAlias), leftRef)
	rightQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(rightAlias), rightRef)

	var preds []predicates.QueryPredicate
	if j.OnPredicate != nil {
		if qp, ok := j.OnPredicate.(predicates.QueryPredicate); ok {
			preds = []predicates.QueryPredicate{qp}
		}
	}

	var joinType expressions.JoinType
	switch kind {
	case logical.JoinLeft:
		joinType = expressions.JoinLeftOuter
	case logical.JoinFull:
		// FULL OUTER is symmetric — no operand swap (the JoinRight swap
		// above does not fire for JoinFull). The materialized NLJ keeps
		// the original left/right column layout.
		joinType = expressions.JoinFullOuter
	default:
		joinType = expressions.JoinInner
	}

	resultValue := values.NewJoinMergeResultValue(
		values.NamedCorrelationIdentifier(leftAlias),
		values.NamedCorrelationIdentifier(rightAlias),
	)
	return expressions.NewSelectExpressionWithJoinType(
		resultValue,
		[]expressions.Quantifier{leftQ, rightQ},
		preds,
		[]string{leftAlias, rightAlias},
		joinType,
	)
}

// translateJoinWithExists builds a flat SelectExpression from a LogicalJoin
// + LogicalFilter that carries EXISTS subqueries. Instead of nesting one
// SelectExpression (the join) inside another (the EXISTS filter), this
// method produces a single SelectExpression with ForEach(left),
// ForEach(right), and Existential quantifiers. The combined predicate
// covers both the join ON and the filter WHERE. The NLJ rule's
// implementJoinWithExistential path handles this 2+1 pattern.
func (t *cascadesTranslator) translateJoinWithExists(
	j *logical.LogicalJoin,
	f *logical.LogicalFilter,
) expressions.RelationalExpression {
	// FULL OUTER cannot be expressed through the join+EXISTS flatten shape
	// (the semi-join cannot carry the FULL drain). The production path
	// rejects this earlier with a clear error (findFullOuterWithExists),
	// but harness callers (plan_harness) invoke the translator directly and
	// bypass that guard — refuse here too so FULL+EXISTS is never silently
	// mistranslated to INNER (the kind switch below defaults to JoinInner).
	if j.Kind == logical.JoinFull {
		return nil
	}
	left := j.Left
	right := j.Right
	kind := j.Kind
	if kind == logical.JoinRight {
		left, right = right, left
		kind = logical.JoinLeft
	}

	// Collect scalar subquery plans from the filter.
	for _, ssq := range f.ScalarSubqueries {
		t.scalarSubqueries = append(t.scalarSubqueries, ScalarSubqueryPlan{
			Alias: ssq.Alias,
			Plan:  ssq.Plan,
		})
	}

	// Flatten join + EXISTS into a single SelectExpression
	// with ForEach(left), ForEach(right), and Existential quantifiers.
	leftRef := t.translateRef(left)
	if leftRef == nil {
		return nil
	}
	rightRef := t.translateRef(right)
	if rightRef == nil {
		return nil
	}

	leftAlias := sourceAlias(left)
	rightAlias := sourceAlias(right)

	leftQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(leftAlias), leftRef)
	rightQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(rightAlias), rightRef)
	quantifiers := []expressions.Quantifier{leftQ, rightQ}

	// Combine join ON predicates + filter WHERE predicates.
	var allPreds []predicates.QueryPredicate
	if j.OnPredicate != nil {
		if qp, ok := j.OnPredicate.(predicates.QueryPredicate); ok {
			allPreds = append(allPreds, qp)
		}
	}
	if f.Predicate != nil {
		if and, ok := f.Predicate.(*predicates.AndPredicate); ok {
			allPreds = append(allPreds, and.SubPredicates...)
		} else {
			allPreds = append(allPreds, f.Predicate)
		}
	}

	// Add EXISTS subqueries as existential quantifiers.
	for _, esq := range f.ExistsSubqueries {
		subRef := t.translateRef(esq.Plan)
		if subRef == nil {
			return nil
		}
		existQ := expressions.NamedExistentialQuantifier(esq.Alias, subRef)
		quantifiers = append(quantifiers, existQ)
		if esq.JoinPredicate != nil {
			allPreds = append(allPreds, esq.JoinPredicate)
		}
	}

	sourceAliases := []string{leftAlias, rightAlias}
	for _, esq := range f.ExistsSubqueries {
		sourceAliases = append(sourceAliases, sourceAlias(esq.Plan))
	}

	var joinType expressions.JoinType
	switch kind {
	case logical.JoinLeft:
		joinType = expressions.JoinLeftOuter
	default:
		joinType = expressions.JoinInner
	}

	resultValue := values.NewJoinMergeResultValue(
		values.NamedCorrelationIdentifier(leftAlias),
		values.NamedCorrelationIdentifier(rightAlias),
	)
	return expressions.NewSelectExpressionWithJoinType(
		resultValue,
		quantifiers,
		allPreds,
		sourceAliases,
		joinType,
	)
}

// splitNonExistsPredicates extracts the non-EXISTS parts of a predicate
// tree. EXISTS predicates (and NOT EXISTS) are dropped — they're
// represented by the Existential quantifier in the SelectExpression.
// Compound AND predicates are flattened: AND(ExistsPredicate, c.id < 10)
// yields just [c.id < 10].
func splitNonExistsPredicates(pred predicates.QueryPredicate) []predicates.QueryPredicate {
	if pred == nil {
		return nil
	}
	if _, ok := pred.(*predicates.ExistsPredicate); ok {
		return nil
	}
	if not, ok := pred.(*predicates.NotPredicate); ok {
		if len(not.Children()) == 1 {
			if _, ok := not.Children()[0].(*predicates.ExistsPredicate); ok {
				return nil
			}
		}
	}
	if and, ok := pred.(*predicates.AndPredicate); ok {
		var result []predicates.QueryPredicate
		for _, sub := range and.SubPredicates {
			result = append(result, splitNonExistsPredicates(sub)...)
		}
		return result
	}
	return []predicates.QueryPredicate{pred}
}

// extractExistsPredicates returns the EXISTS-related predicates that
// splitNonExistsPredicates drops: bare ExistsPredicate or
// NOT(ExistsPredicate). The rule's implementExistentialSelect needs
// these to detect EXISTS vs NOT EXISTS.
func extractExistsPredicates(pred predicates.QueryPredicate) []predicates.QueryPredicate {
	if pred == nil {
		return nil
	}
	if _, ok := pred.(*predicates.ExistsPredicate); ok {
		return []predicates.QueryPredicate{pred}
	}
	if not, ok := pred.(*predicates.NotPredicate); ok {
		if len(not.Children()) == 1 {
			if _, ok := not.Children()[0].(*predicates.ExistsPredicate); ok {
				return []predicates.QueryPredicate{pred}
			}
		}
	}
	if and, ok := pred.(*predicates.AndPredicate); ok {
		var result []predicates.QueryPredicate
		for _, sub := range and.SubPredicates {
			result = append(result, extractExistsPredicates(sub)...)
		}
		return result
	}
	return nil
}

func (t *cascadesTranslator) namedQuantifier(alias string, ref *expressions.Reference) expressions.Quantifier {
	if alias != "" {
		return expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier(alias), ref)
	}
	return expressions.ForEachQuantifier(ref)
}

func sourceAlias(op logical.LogicalOperator) string {
	for cur := op; cur != nil; {
		switch o := cur.(type) {
		case *logical.LogicalScan:
			if o.Alias != "" {
				return strings.ToUpper(o.Alias)
			}
			return strings.ToUpper(o.Table)
		case *logical.LogicalJoin:
			return sourceAlias(o.Right)
		case *logical.LogicalCTE:
			// CTE-wrapped derived tables: the CTE name IS the
			// derived-table alias. Return it directly so the NLJ
			// executor qualifies merged-row keys under the alias
			// the user specified (e.g. "sq1"), not the underlying
			// table name buried inside the CTE body.
			return strings.ToUpper(o.Name)
		default:
			ch := cur.Children()
			if len(ch) == 1 {
				cur = ch[0]
				continue
			}
			return ""
		}
	}
	return ""
}

func (t *cascadesTranslator) translateCTE(c *logical.LogicalCTE) expressions.RelationalExpression {
	if c.Recursive {
		return t.translateRecursiveCTE(c)
	}
	body := c.Body
	if len(c.ColumnAliases) > 0 {
		if origCols := extractOutputColumns(body); len(origCols) == len(c.ColumnAliases) {
			body = logical.NewProject(body, origCols, c.ColumnAliases)
		}
	}
	t.cteScope[strings.ToUpper(c.Name)] = body
	result := t.translateOp(c.Main)
	delete(t.cteScope, strings.ToUpper(c.Name))
	return result
}

func extractOutputColumns(op logical.LogicalOperator) []string {
	switch o := op.(type) {
	case *logical.LogicalProject:
		return o.Projections
	case *logical.LogicalAggregate:
		var cols []string
		cols = append(cols, o.GroupKeys...)
		for i, agg := range o.Aggregates {
			if i < len(o.Aliases) && o.Aliases[i] != "" {
				cols = append(cols, o.Aliases[i])
			} else {
				cols = append(cols, agg)
			}
		}
		return cols
	case *logical.LogicalDistinct:
		return extractOutputColumns(o.Input)
	case *logical.LogicalSort:
		return extractOutputColumns(o.Input)
	case *logical.LogicalLimit:
		return extractOutputColumns(o.Input)
	case *logical.LogicalFilter:
		return extractOutputColumns(o.Input)
	}
	return nil
}

// translateRecursiveCTE translates a WITH RECURSIVE CTE into a
// RecursiveUnionExpression. Mirrors Java's
// QueryVisitor.handleRecursiveNamedQuery:
//  1. Partition the UNION ALL body into seed (non-recursive) and
//     recursive (self-referencing) branches.
//  2. Translate the seed branch normally.
//  3. Translate the recursive branch with the CTE self-reference
//     resolving to a TempTableScanExpression.
//  4. Wrap both legs in TempTableInsertExpression.
//  5. Create RecursiveUnionExpression with scan/insert aliases.
//  6. Translate the Main query with the CTE name resolving to the
//     RecursiveUnionExpression.
func (t *cascadesTranslator) translateRecursiveCTE(c *logical.LogicalCTE) expressions.RelationalExpression {
	cteName := strings.ToUpper(c.Name)

	// The body must be a UNION ALL or UNION DISTINCT.
	union, ok := c.Body.(*logical.LogicalUnion)
	if !ok || len(union.Inputs) < 2 {
		return nil
	}

	// Partition branches into seed (no self-reference) and recursive
	// (references the CTE name).
	var seedBranches, recursiveBranches []logical.LogicalOperator
	for _, branch := range union.Inputs {
		if logicalOpReferencesCTE(branch, cteName) {
			recursiveBranches = append(recursiveBranches, branch)
		} else {
			seedBranches = append(seedBranches, branch)
		}
	}
	if len(seedBranches) == 0 || len(recursiveBranches) == 0 {
		return nil
	}

	scanAlias := values.NamedCorrelationIdentifier(cteName + "forScan")
	insertAlias := values.NamedCorrelationIdentifier(cteName + "forInsert")

	// Translate the seed leg. Multiple seed branches become a union.
	var seedExpr expressions.RelationalExpression
	if len(seedBranches) == 1 {
		seedExpr = t.translateOp(seedBranches[0])
	} else {
		seedExpr = t.translateUnion(&logical.LogicalUnion{Inputs: seedBranches, Distinct: false})
	}
	if seedExpr == nil {
		return nil
	}

	// Wrap seed in TempTableInsert.
	seedRef := expressions.InitialOf(seedExpr)
	seedInsert := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(seedRef), insertAlias, false)

	// Translate the recursive leg with the CTE self-reference resolving
	// to a TempTableScanExpression(scanAlias).
	t.cteExprScope[cteName] = expressions.NewTempTableScanExpression(scanAlias)
	var recursiveExpr expressions.RelationalExpression
	if len(recursiveBranches) == 1 {
		recursiveExpr = t.translateOp(recursiveBranches[0])
	} else {
		recursiveExpr = t.translateUnion(&logical.LogicalUnion{Inputs: recursiveBranches, Distinct: false})
	}
	delete(t.cteExprScope, cteName)
	if recursiveExpr == nil {
		return nil
	}

	// Normalize the recursive leg's output columns to match the seed's
	// schema. In standard SQL, the CTE's output column names are defined
	// by the seed. The recursive branch often uses qualified column
	// references (e.g. SELECT b.id, b.parent) which produce datum keys
	// like "B.ID" instead of the seed's unqualified "ID". Without this
	// normalization, the outer query (and DFS recursion) can't find the
	// expected columns, yielding NULL for every row.
	//
	// The temp table MUST use the seed's original column names (not the
	// CTE column aliases). The semantic analyzer's ColumnAliasMap
	// reverse-maps aliased references (e.g. `a.up`) back to the
	// original column names (e.g. `A.PARENT`) in the WHERE predicate's
	// FieldValues. So the temp table datum keys must be the originals
	// for the recursive branch's join predicates to match.
	seedCols := extractOuterProjectionColumns(seedBranches[0])
	recCols := extractOuterProjectionColumns(recursiveBranches[0])
	if len(seedCols) > 0 && len(recCols) > 0 && len(seedCols) == len(recCols) {
		// ALWAYS wrap the recursive leg in a normalization projection that
		// reads the body's output columns and stores them under the seed's
		// column names. This is not only a name remap — it is what makes the
		// temp-table rows CLEAN. When the recursive body is a join, its output
		// is the merged JoinMergeResultValue row carrying stale qualified keys
		// (e.g. B.ID, B.PARENT, A.ID from the join's outer/inner sides). If
		// those rows are inserted verbatim, the next recursion level scans them
		// as the temp-table alias and its join predicate (`b.parent = a.id`)
		// resolves a.id against keys that don't match the schema — the recursion
		// stalls one level early (missing the deepest descendants). The flat
		// FieldValue reads each seed-schema column by its bare name (the bare
		// key in a JoinMergeResultValue is the inner/new row's value), producing
		// a clean {id, parent} row. Doing this unconditionally removes the
		// dependency on the Go-only PushProjectionBelowJoinRule, which was the
		// only other mechanism that narrowed the body's columns (RFC-042 L1).
		remapVals := make([]values.Value, len(recCols))
		for i, rc := range recCols {
			// Read the body's output for this column and emit it under the
			// seed's UNQUALIFIED column name so the temp-table row is CLEAN.
			// A qualified recursive projection (SELECT b.id) emits its value
			// under the qualified key "B.ID". If we copied that key verbatim
			// into the temp table, the NEXT recursion level — which joins the
			// temp-table scan (aliased) against a fresh quantifier — would have
			// the stale "B.ID" collide with the new join side's own "B.ID",
			// clobbering the live row and stalling the recursion one level
			// early. Build FieldValue{Field: <bare col>, Child: QOV(<qualifier>)}:
			// evaluateCorrelated reads the qualified datum key ("B.ID"), while
			// projectionColumnName returns just the bare field — so the emitted
			// row has ONLY the clean schema column, no qualified leak. This is
			// what lets the Go-only PushProjectionBelowJoinRule be removed: it
			// was the only other mechanism narrowing the body's columns
			// (RFC-042 L1).
			ru := strings.ToUpper(rc)
			var rv values.Value
			if dot := strings.IndexByte(ru, '.'); dot >= 0 {
				qualifier := ru[:dot]
				col := ru[dot+1:]
				rv = &values.FieldValue{
					Field: col,
					Typ:   values.UnknownType,
					Child: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(qualifier)),
				}
			} else {
				rv = &values.FieldValue{Field: ru, Typ: values.UnknownType}
			}
			remapVals[i] = rv
		}
		remapAliases := make([]string, len(seedCols))
		for i, sc := range seedCols {
			remapAliases[i] = strings.ToUpper(sc)
		}
		recursiveExpr = expressions.NewLogicalProjectionExpressionWithAliases(
			remapVals,
			remapAliases,
			expressions.ForEachQuantifier(expressions.InitialOf(recursiveExpr)),
		)
	}

	// Wrap recursive leg in TempTableInsert.
	recursiveRef := expressions.InitialOf(recursiveExpr)
	recursiveInsert := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(recursiveRef), insertAlias, false)

	// Build RecursiveUnionExpression.
	seedInsertRef := expressions.InitialOf(seedInsert)
	recursiveInsertRef := expressions.InitialOf(recursiveInsert)
	strategy := expressions.TraversalAny
	switch c.TraversalOrder {
	case logical.TraversalPreOrder:
		strategy = expressions.TraversalPreorder
	case logical.TraversalPostOrder:
		strategy = expressions.TraversalPostorder
	}
	var recUnion *expressions.RecursiveUnionExpression
	if union.Distinct {
		recUnion = expressions.NewRecursiveUnionExpressionDistinct(
			expressions.ForEachQuantifier(seedInsertRef),
			expressions.ForEachQuantifier(recursiveInsertRef),
			scanAlias, insertAlias,
			strategy,
		)
	} else {
		recUnion = expressions.NewRecursiveUnionExpression(
			expressions.ForEachQuantifier(seedInsertRef),
			expressions.ForEachQuantifier(recursiveInsertRef),
			scanAlias, insertAlias,
			strategy,
		)
	}

	// Apply CTE column aliases as a rename projection over the
	// recursive union's output. The temp table internally uses the
	// seed's original column names (ID, PARENT) because the semantic
	// analyzer's ColumnAliasMap reverse-maps aliased references
	// (`a.up` → `A.PARENT`) in the recursive branch's predicates.
	// The rename only applies to the outward-facing datum so the
	// main query can reference the aliased names (NODE, UP).
	var cteResult expressions.RelationalExpression = recUnion
	if len(c.ColumnAliases) > 0 && len(seedCols) > 0 && len(seedCols) == len(c.ColumnAliases) {
		needsRename := false
		for i := range seedCols {
			if !strings.EqualFold(seedCols[i], c.ColumnAliases[i]) {
				needsRename = true
				break
			}
		}
		if needsRename {
			renameVals := make([]values.Value, len(seedCols))
			for i, sc := range seedCols {
				renameVals[i] = &values.FieldValue{
					Field: strings.ToUpper(sc),
					Typ:   values.UnknownType,
				}
			}
			renameAliases := make([]string, len(c.ColumnAliases))
			for i, a := range c.ColumnAliases {
				renameAliases[i] = strings.ToUpper(a)
			}
			cteResult = expressions.NewLogicalProjectionExpressionWithAliases(
				renameVals,
				renameAliases,
				expressions.ForEachQuantifier(expressions.InitialOf(recUnion)),
			)
		}
	}

	// Register the (possibly-renamed) result so that the Main query's
	// scan of the CTE name resolves to it.
	t.cteExprScope[cteName] = cteResult
	result := t.translateOp(c.Main)
	delete(t.cteExprScope, cteName)
	return result
}

// extractOuterProjectionColumns returns the column names from the
// outermost LogicalProject in a logical operator tree. Returns nil if
// no LogicalProject is found. Used by translateRecursiveCTE to detect
// schema mismatches between seed and recursive branches.
func extractOuterProjectionColumns(op logical.LogicalOperator) []string {
	if p, ok := op.(*logical.LogicalProject); ok {
		return p.Projections
	}
	return nil
}

// logicalOpReferencesCTE walks a LogicalOperator tree and reports
// whether any LogicalScan references the given CTE name (case-
// insensitive). Used to partition UNION ALL branches into seed vs
// recursive legs.
func logicalOpReferencesCTE(op logical.LogicalOperator, cteName string) bool {
	if op == nil {
		return false
	}
	if scan, ok := op.(*logical.LogicalScan); ok {
		if strings.EqualFold(scan.Table, cteName) {
			return true
		}
	}
	for _, child := range op.Children() {
		if logicalOpReferencesCTE(child, cteName) {
			return true
		}
	}
	return false
}

func (t *cascadesTranslator) translateInsert(ins *logical.LogicalInsert) expressions.RelationalExpression {
	var innerRef *expressions.Reference
	switch {
	case ins.Source != nil:
		// INSERT … SELECT: the source plan produces the rows.
		innerRef = t.translateRef(ins.Source)
		if innerRef == nil {
			return nil
		}
	case ins.ValuesArray != nil:
		// INSERT … VALUES: explode the literal array of records into a
		// stream, matching Java (ExplodeExpression over the array Value).
		explode := expressions.NewExplodeExpression(ins.ValuesArray)
		innerRef = expressions.InitialOf(explode)
	}
	var q expressions.Quantifier
	if innerRef != nil {
		q = expressions.ForEachQuantifier(innerRef)
	}
	return expressions.NewInsertExpression(q, ins.Table, values.UnknownType)
}

func (t *cascadesTranslator) translateUpdate(upd *logical.LogicalUpdate) expressions.RelationalExpression {
	var innerRef *expressions.Reference
	if upd.Input != nil {
		innerRef = t.translateRef(upd.Input)
		if innerRef == nil {
			return nil
		}
	}
	transforms := make([]expressions.UpdateTransform, len(upd.Sets))
	for i, a := range upd.Sets {
		// Prefer the catalog-resolved RHS Value (evaluated per row by the
		// executor); fall back to the canonical text only when the builder
		// ran without catalog resolution (then the executor cannot evaluate
		// it — but this keeps the structure for explain/legacy paths).
		newVal := a.Value
		if newVal == nil {
			newVal = &values.ConstantValue{Value: a.Expr, Typ: values.UnknownType}
		}
		transforms[i] = expressions.UpdateTransform{
			FieldPath: strings.ToUpper(a.Column),
			NewValue:  newVal,
		}
	}
	var q expressions.Quantifier
	if innerRef != nil {
		q = expressions.ForEachQuantifier(innerRef)
	}
	return expressions.NewUpdateExpression(q, upd.Target, transforms)
}

func (t *cascadesTranslator) translateDelete(del *logical.LogicalDelete) expressions.RelationalExpression {
	var innerRef *expressions.Reference
	if del.Input != nil {
		innerRef = t.translateRef(del.Input)
		if innerRef == nil {
			return nil
		}
	}
	var q expressions.Quantifier
	if innerRef != nil {
		q = expressions.ForEachQuantifier(innerRef)
	}
	return expressions.NewDeleteExpression(q, del.Target)
}

// FindUnsupportedFunction walks the logical plan tree and returns the
// name of the first ScalarFunctionValue that isn't in the supported set.
// Returns "" if all functions are supported.
func FindUnsupportedFunction(op logical.LogicalOperator) string {
	if op == nil {
		return ""
	}
	if proj, ok := op.(*logical.LogicalProject); ok {
		for _, v := range proj.ProjectedValues {
			if fn := findUnsafeFuncInValue(v); fn != "" {
				return fn
			}
		}
	}
	if f, ok := op.(*logical.LogicalFilter); ok && f.Predicate != nil {
		if fn := findUnsafeFuncInPredicate(f.Predicate); fn != "" {
			return fn
		}
	}
	if u, ok := op.(*logical.LogicalUpdate); ok {
		// UPDATE SET RHS expressions must reject unsupported functions
		// just like projections, matching the naive path.
		for _, a := range u.Sets {
			if a.Value != nil {
				if fn := findUnsafeFuncInValue(a.Value); fn != "" {
					return fn
				}
			}
		}
	}
	for _, child := range op.Children() {
		if fn := FindUnsupportedFunction(child); fn != "" {
			return fn
		}
	}
	return ""
}

func findUnsafeFuncInValue(v values.Value) string {
	if v == nil {
		return ""
	}
	var found string
	values.WalkValue(v, func(node values.Value) bool {
		if sf, ok := node.(*values.ScalarFunctionValue); ok {
			if !values.IsCascadesSafeScalarFunction(sf.FuncName) {
				found = sf.FuncName
				return false
			}
		}
		return true
	})
	return found
}

func findUnsafeFuncInPredicate(p predicates.QueryPredicate) string {
	var found string
	predicates.WalkPredicate(p, func(qp predicates.QueryPredicate) bool {
		switch pred := qp.(type) {
		case *predicates.ComparisonPredicate:
			if fn := findUnsafeFuncInValue(pred.Operand); fn != "" {
				found = fn
				return false
			}
			if pred.Comparison.Operand != nil {
				if fn := findUnsafeFuncInValue(pred.Comparison.Operand); fn != "" {
					found = fn
					return false
				}
			}
		case *predicates.ValuePredicate:
			if fn := findUnsafeFuncInValue(pred.Value); fn != "" {
				found = fn
				return false
			}
		}
		return true
	})
	return found
}
