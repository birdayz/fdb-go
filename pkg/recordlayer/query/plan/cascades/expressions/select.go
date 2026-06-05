package expressions

import (
	"encoding/binary"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// JoinType carried on a SelectExpression to distinguish INNER vs OUTER
// join semantics. Mirrors plans.JoinType but lives in the expressions
// package to avoid a circular dependency (plans imports expressions).
// The ImplementNestedLoopJoinRule maps this to the corresponding
// plans.JoinType when creating the physical plan.
type JoinType int

const (
	JoinInner     JoinType = iota
	JoinLeftOuter          // LEFT OUTER JOIN — unmatched outer rows emit NULLs for inner columns
	JoinCross              // CROSS JOIN — no predicate, cartesian product
	// JoinFullOuter — FULL OUTER JOIN. Unmatched rows from BOTH sides
	// are emitted NULL-padded on the opposite side. Go-only extension
	// (Java has no outer joins). Maps to plans.JoinFullOuter.
	JoinFullOuter
)

// SelectExpression is the FROM-list / JOIN anchor — the one logical
// operator that returns true from CanCorrelate. All others have at
// most one child or have the SQL-set semantics that forbid
// inter-child binding; SelectExpression is the workhorse that fuses
// multiple inputs into one row stream and lets predicates / projection
// reference any of them.
//
// Three pieces of node-information:
//   - resultValue: the Value describing the row this SELECT emits.
//     For the seed, the SQL parser is responsible for constructing
//     this Value (typically a RecordConstructorValue over the
//     projection list).
//   - quantifiers: the FROM-list inputs. Children are commutative
//     (same caveat as Union — positional SemanticEquals here, full
//     alias-permutation matcher lands in B2 follow-on).
//   - predicates: the WHERE clause, as a list to be AND'd. Empty
//     list = no WHERE.
//
// Ports the structural surface of Java's
// `com.apple.foundationdb.record.query.plan.cascades.expressions.SelectExpression`.
// Java's full implementation also memoises a PartiallyOrderedSet
// correlation order, an independent-quantifiers partitioning, and a
// conjuncted-predicate handle. Those are derived projections on top
// of the three seed fields and land when their consumers (rules in
// B5, the Memo in B3) actually need them.
type SelectExpression struct {
	resultValue     values.Value
	quantifiers     []Quantifier
	queryPredicates []predicates.QueryPredicate
	sourceAliases   []string
	joinType        JoinType // default JoinInner (zero value)
	// quantifiersSwapped is true when this expression was created by
	// WithSwappedQuantifiers, meaning the physical join direction is
	// reversed relative to the SQL FROM-clause order. Used by the NLJ
	// rule to mark the plan so column derivation can restore the
	// original SQL column ordering.
	quantifiersSwapped bool
}

// NewSelectExpression builds a SELECT. quantifiers and predicates are
// copied. resultValue is captured by reference (Values are immutable).
func NewSelectExpression(resultValue values.Value, quantifiers []Quantifier, queryPredicates []predicates.QueryPredicate) *SelectExpression {
	copiedQ := make([]Quantifier, len(quantifiers))
	copy(copiedQ, quantifiers)
	copiedP := make([]predicates.QueryPredicate, len(queryPredicates))
	copy(copiedP, queryPredicates)
	return &SelectExpression{
		resultValue:     resultValue,
		quantifiers:     copiedQ,
		queryPredicates: copiedP,
	}
}

// NewSelectExpressionWithAliases builds a SELECT with source aliases
// parallel to quantifiers.
func NewSelectExpressionWithAliases(resultValue values.Value, quantifiers []Quantifier, queryPredicates []predicates.QueryPredicate, sourceAliases []string) *SelectExpression {
	copiedQ := make([]Quantifier, len(quantifiers))
	copy(copiedQ, quantifiers)
	copiedP := make([]predicates.QueryPredicate, len(queryPredicates))
	copy(copiedP, queryPredicates)
	copiedA := make([]string, len(sourceAliases))
	copy(copiedA, sourceAliases)
	return &SelectExpression{
		resultValue:     resultValue,
		quantifiers:     copiedQ,
		queryPredicates: copiedP,
		sourceAliases:   copiedA,
	}
}

// NewSelectExpressionWithJoinType builds a SELECT with source aliases
// and an explicit join type (LEFT OUTER, CROSS, etc.).
func NewSelectExpressionWithJoinType(resultValue values.Value, quantifiers []Quantifier, queryPredicates []predicates.QueryPredicate, sourceAliases []string, joinType JoinType) *SelectExpression {
	copiedQ := make([]Quantifier, len(quantifiers))
	copy(copiedQ, quantifiers)
	copiedP := make([]predicates.QueryPredicate, len(queryPredicates))
	copy(copiedP, queryPredicates)
	copiedA := make([]string, len(sourceAliases))
	copy(copiedA, sourceAliases)
	return &SelectExpression{
		resultValue:     resultValue,
		quantifiers:     copiedQ,
		queryPredicates: copiedP,
		sourceAliases:   copiedA,
		joinType:        joinType,
	}
}

// GetJoinType returns the join type (INNER, LEFT OUTER, CROSS).
// Default (zero value) is JoinInner.
func (e *SelectExpression) GetJoinType() JoinType { return e.joinType }

// IsQuantifiersSwapped reports whether this expression's quantifiers
// were swapped relative to the SQL FROM-clause order. Used by the NLJ
// rule to mark the physical plan so column derivation can restore the
// original SQL column ordering.
func (e *SelectExpression) IsQuantifiersSwapped() bool { return e.quantifiersSwapped }

// GetSourceAliases returns the SQL-level table aliases, parallel to
// quantifiers. May be nil or shorter than quantifiers if aliases
// weren't provided.
func (e *SelectExpression) GetSourceAliases() []string { return e.sourceAliases }

// GetResultValue returns the row-shape Value of this SELECT.
func (e *SelectExpression) GetResultValue() values.Value { return e.resultValue }

// GetResultValues — Java exposes a flattened version of the projection
// list. Seed returns a single-element slice with the result Value
// since we don't track the projection separately.
func (e *SelectExpression) GetResultValues() []values.Value {
	return []values.Value{e.resultValue}
}

// GetPredicates returns the WHERE-clause predicate list. Read-only.
func (e *SelectExpression) GetPredicates() []predicates.QueryPredicate {
	return e.queryPredicates
}

// HasPredicates reports whether the WHERE clause is non-empty.
func (e *SelectExpression) HasPredicates() bool { return len(e.queryPredicates) > 0 }

// GetQuantifiers returns the FROM-list inputs.
func (e *SelectExpression) GetQuantifiers() []Quantifier { return e.quantifiers }

// CanCorrelate is TRUE for SelectExpression — this is the
// distinguishing property. Predicates / projection in this expression
// can reference any quantifier's flowed object value, and the planner
// must respect that when deciding whether to swap or split children.
func (e *SelectExpression) CanCorrelate() bool { return true }

// ChildrenAsSet reports whether the quantifier list is an unordered set
// for memo equality — i.e. the operator is invariant under quantifier
// permutation. matchChildrenInMemo permutes children only when this is
// true. Java marks SelectExpression unconditionally because Java's
// SelectExpression only ever models commutative (INNER/CROSS) joins. Go's
// read-side extension overloads SelectExpression with outer join types
// whose NULL-extension is DIRECTIONAL — `A LEFT JOIN B` != `B LEFT JOIN A`
// — so the set marker must narrow to the commutative join types. Were it
// left unconditionally true, matchChildrenInMemo would permute a left
// join's quantifiers and intern swapped left joins into one Reference
// (REVIEW.md #215). The swap-EXPLORATION guards (expression_matcher,
// implementation_rule, unified_tasks) are a separate concern — they
// suppress GENERATING an invalid commuted alternative; this controls memo
// dedup, and findCandidateParents is order-insensitive, so the property
// itself must be honest.
func (e *SelectExpression) ChildrenAsSet() bool {
	return e.joinType == JoinInner || e.joinType == JoinCross
}

// GetCorrelatedToWithoutChildren returns the union of correlation
// sets across predicates + the resultValue. Java's behaviour matches.
func (e *SelectExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	out := values.GetCorrelatedToOfValue(e.resultValue)
	if out == nil {
		out = map[values.CorrelationIdentifier]struct{}{}
	}
	for _, p := range e.queryPredicates {
		for k := range predicates.GetCorrelatedToOfPredicate(p) {
			out[k] = struct{}{}
		}
	}
	return out
}

// EqualsWithoutChildren compares predicate lists + result value. The
// quantifier list is intentionally NOT consulted — that's the
// children, compared by the SemanticEquals walk.
func (e *SelectExpression) EqualsWithoutChildren(other RelationalExpression, aliases *AliasMap) bool {
	o, ok := other.(*SelectExpression)
	if !ok {
		return false
	}
	if e.joinType != o.joinType {
		return false
	}
	// Alias-aware result-value + predicate equality (RFC-040 040.2). Under
	// the memo's empty-alias path this reduces to identity-alias equality —
	// inert until PR-A threads real alias maps.
	vm := aliases.ToValuesAliasMap()
	if !values.SemanticEqualsUnderAliasMap(e.resultValue, o.resultValue, vm) {
		return false
	}
	if len(e.queryPredicates) != len(o.queryPredicates) {
		return false
	}
	for i := range e.queryPredicates {
		if !predicates.SemanticEqualsUnderAliasMap(e.queryPredicates[i], o.queryPredicates[i], vm) {
			return false
		}
	}
	return true
}

// InternsAliasAware reports whether this expression should dedup ALIAS-AWARE in
// Reference.Insert/InsertFinal (RFC-077 7.5). True ONLY for a merge re-enumeration
// select — one whose result value is a source-anchored join RC (AnchoredJoin). Such a select's merge
// quantifier is a planner-INTERNAL synthetic alias with no external consumer:
// PartitionSelectRule re-stamps all column access through the merge value and
// rebases spanning predicates onto it, so two merge selects equal up to a
// consistent quantifier-alias renaming ARE the same memo member and must intern
// (otherwise the join re-enumeration's shared sub-products re-explode per path).
// This replaces the former synthetic STABLE merge alias that made them
// byte-identical for the alias-IDENTITY dedup.
//
// All OTHER expressions return false. The reason is NOT alias-namespace naming
// (item 7.1 already unified quantifier and table alias naming) — it is the column
// RESOLUTION model: Go's column derivation (cascades_generator) resolves some
// references by quantifier-alias IDENTITY, whereas Java resolves by group/ordinal
// and so can dedup members alias-aware globally. An alias-aware member collapse
// keeps a survivor whose quantifier aliases differ from the discarded member's;
// any external structure that captured the discarded member's aliases by identity
// then mis-resolves — empirically, a CTE column-rename select collapsed this way
// reads its renamed column as NULL. So non-merge selects keep the alias-IDENTITY
// dedup, exactly as before this change. Widening this gate is gated on migrating
// Go's column resolution to Java's ordinal/group model, not on 7.1.
func (e *SelectExpression) InternsAliasAware() bool {
	// RFC-077 7.6: the source-anchored join RESULT value (a RecordConstructorValue
	// marked AnchoredJoin) is the marker of a merge re-enumeration select — its
	// merge quantifier is planner-internal with no external consumer, so it must
	// intern alias-aware; otherwise the re-enumeration's shared sub-products
	// re-explode per bipartition (the ≥4-way chain/STAR task count blows past
	// budget). It is the structural successor of the retired opaque-merge marker.
	if rc, ok := e.resultValue.(*values.RecordConstructorValue); ok && rc.AnchoredJoin {
		return true
	}
	return false
}

// HashCodeWithoutChildren mixes a class-discriminating constant + join type
// with the ALIAS-INVARIANT semantic hashes of the result value and predicates
// (RFC-040 040.2), consistent with the alias-aware EqualsWithoutChildren.
func (e *SelectExpression) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("select|"))
	h.Write([]byte{byte(e.joinType)})
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], values.SemanticHashCode(e.resultValue))
	h.Write(buf[:])
	for _, p := range e.queryPredicates {
		binary.LittleEndian.PutUint64(buf[:], predicates.SemanticHashCode(p))
		h.Write(buf[:])
	}
	return h.Sum64()
}

// WithSwappedQuantifiers returns a shallow copy of this SelectExpression
// with the first two quantifiers (and their corresponding source aliases)
// in reversed order. Used by the planner to explore both join directions
// for ChildrenAsSet expressions. Returns the receiver unchanged if there
// are fewer than 2 quantifiers.
func (e *SelectExpression) WithSwappedQuantifiers() *SelectExpression {
	if len(e.quantifiers) < 2 {
		return e
	}
	swapped := make([]Quantifier, len(e.quantifiers))
	copy(swapped, e.quantifiers)
	swapped[0], swapped[1] = swapped[1], swapped[0]

	var swappedAliases []string
	if len(e.sourceAliases) >= 2 {
		swappedAliases = make([]string, len(e.sourceAliases))
		copy(swappedAliases, e.sourceAliases)
		swappedAliases[0], swappedAliases[1] = swappedAliases[1], swappedAliases[0]
	} else {
		swappedAliases = e.sourceAliases
	}

	return &SelectExpression{
		resultValue:        e.resultValue,
		quantifiers:        swapped,
		queryPredicates:    e.queryPredicates,
		sourceAliases:      swappedAliases,
		joinType:           e.joinType,
		quantifiersSwapped: !e.quantifiersSwapped, // toggle: swap of swap = original
	}
}

func (e *SelectExpression) WithQuantifiers(quantifiers []Quantifier) RelationalExpression {
	copied := make([]Quantifier, len(quantifiers))
	copy(copied, quantifiers)
	return &SelectExpression{
		resultValue:     e.resultValue,
		quantifiers:     copied,
		queryPredicates: e.queryPredicates,
		sourceAliases:   e.sourceAliases,
		joinType:        e.joinType,
	}
}

var _ RelationalExpression = (*SelectExpression)(nil)
