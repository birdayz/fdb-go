// Package expr is the parse-tree → values.Value resolver. It
// bridges the two main Phase 3 seam packages:
//
//   - pkg/relational/core/query/semantic — identifier resolution,
//     catalog lookup, scope chain.
//   - pkg/recordlayer/query/plan/cascades — Value / Predicate
//     hierarchy.
//
// Neither semantic nor cascades depends on the other. expr sits
// above both and owns the logic that turns a parsed SQL expression
// into a typed Value tree with every identifier resolved against a
// Scope.
//
// # API
//
// Two layers:
//
//   - Resolver.Resolve* primitives — programmatic construction of
//     Value / Predicate trees from already-walked parts (caller
//     supplies each argument). Useful for tests and for synthetic
//     expressions the analyzer constructs from other inputs.
//   - Resolver.WalkExpression / WalkPredicate — parse-tree driver.
//     Dispatches ANTLR IExpressionContext variants to the right
//     Resolve* method, recursing into child contexts.
//
// Call WalkExpression for Value-returning expressions (SELECT list
// items, arithmetic operands). Call WalkPredicate for QueryPredicate-
// returning expressions (WHERE / HAVING clauses).
//
// # Handled shapes
//
//   - Columns: bare (`col`) and qualified (`t.col`).
//   - Constants: integer (width-narrowed like Java's parseDecimal),
//     float, string, NULL.
//   - Arithmetic: +, -, *, /.
//   - Comparisons: =, <>, !=, <, <=, >, >=, IS [NOT] DISTINCT FROM.
//   - Logical: AND / OR / NOT (with left-deep chain flattening).
//   - Unary predicates: IS NULL / IS NOT NULL.
//   - BETWEEN (desugars to AND(>=, <=)).
//   - IN with explicit literal list.
//   - LIKE (no ESCAPE).
//   - Aggregate function calls: COUNT / COUNT(*) / SUM / MIN / MAX / AVG.
//   - Parenthesised expressions (single-element RecordConstructor
//     unwrap).
//
// # Not handled (returns UnsupportedExpressionShapeError)
//
//   - Scalar function calls (UPPER, LOWER, LENGTH, …).
//   - LIKE with ESCAPE.
//   - IN with subquery / parameter / single-column.
//   - CAST to FLOAT / DOUBLE / BYTES / UUID / VECTOR (seed
//     ValueType only covers INT / STRING / BOOL — the full Type
//     hierarchy port lands in Phase 4.0).
//   - Multi-element or named-field record constructors.
//
// CAST and CONVERT to INT / BIGINT / STRING / BOOLEAN are wired
// via DataTypeFunctionCall; XOR desugars to
// (a OR b) AND NOT (a AND b); IS [NOT] TRUE / FALSE desugars via
// the 2VL `(x IS NOT NULL) AND (x = literal)` shape.
//
// Callers catching UnsupportedExpressionShapeError can fall back to
// the existing logical-builder path, which handles the full grammar
// surface at a less-structured level.
package expr

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/google/uuid"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query/semantic"
)

// SubqueryPlanner is the callback interface for building subquery
// plans from the expr walker. The embedded package provides the
// implementation that calls buildLogicalPlanForQuery and stores the
// result. The Resolver itself is agnostic to how the plan is built —
// it only needs a fresh alias and the plan reference.
//
// BuildExists receives the inner Query context from an
// ExistsExpressionAtomContext and returns:
//   - alias: a unique CorrelationIdentifier for the existential quantifier
//   - err: non-nil when the inner query cannot be planned
//
// BuildScalar receives the inner Query context from a
// SubqueryExpressionAtomContext (scalar subquery) and returns:
//   - alias: a unique CorrelationIdentifier for the scalar subquery
//   - typ: the inner plan's single output column type (UnknownType when
//     the shape is underivable) — flowed into ScalarSubqueryValue so the
//     plan-time gates see the real type
//   - err: non-nil when the inner query cannot be planned
//
// The planner stores the (alias → plan) mapping externally; the
// Resolver creates an ExistentialValuePredicate or ScalarSubqueryValue
// referencing the alias.
type SubqueryPlanner interface {
	BuildExists(query antlrgen.IQueryContext) (alias values.CorrelationIdentifier, err error)
	BuildScalar(query antlrgen.IQueryContext) (alias values.CorrelationIdentifier, typ values.Type, err error)
}

// Resolver converts parsed SQL expressions into cascades Values. It
// needs a Scope (to resolve identifiers) and an Analyzer (to run
// column-reference lookup). Stateless beyond those inputs — one
// Resolver per analyzer is fine.
//
// The FunctionCatalog is built lazily on first aggregate-function
// lookup (to keep the construction path cheap when the resolver
// never sees a function call) and reused thereafter. Callers that
// want a custom catalog (scalar extensions, overridden defaults)
// can pass one via NewWithFunctionCatalog.
// Concurrency contract: Resolver is intended for a single statement
// walk and is NOT goroutine-safe — `nextOrdinal` (the positional `?`
// counter) and the lazy `funcCat` initialization both rely on
// single-goroutine access during a walk. Callers that fan parsing
// out across goroutines should construct one Resolver per goroutine.
// `funcCatOnce` makes the lazy build defensive against
// existing concurrent test patterns that share a Resolver across
// `t.Parallel()` subtests; `nextOrdinal` would still be racy in
// that pattern but no test exercises that path.
type Resolver struct {
	analyzer    *semantic.Analyzer
	scope       *semantic.Scope
	funcCat     *semantic.FunctionCatalog
	funcCatOnce sync.Once
	// nextOrdinal counts positional `?` placeholders in the order they
	// appear within a single statement walk. Statement-scoped via the
	// Resolver lifetime — one Resolver per statement, so resetting
	// per-walk falls out of construction. Matches Go's database/sql
	// NamedValue.Ordinal (1-based). Named parameters (`?foo` / `$bar`)
	// keep their declared name and consume no ordinal slot.
	nextOrdinal int
	// subqueryPlanner is the callback for building EXISTS subquery
	// plans. Set via SetSubqueryPlanner by the catalog-aware builder
	// before walking WHERE predicates. nil means EXISTS subqueries
	// decline with UnsupportedExpressionShapeError.
	subqueryPlanner SubqueryPlanner
}

// SetSubqueryPlanner installs a callback that builds logical plans for
// EXISTS subqueries. Must be called before WalkPredicate if EXISTS
// support is desired. Passing nil disables EXISTS handling (the
// walker declines with UnsupportedExpressionShapeError).
func (r *Resolver) SetSubqueryPlanner(p SubqueryPlanner) {
	r.subqueryPlanner = p
}

// New constructs a Resolver bound to a scope. Nil analyzer or nil
// scope panics — the resolver has nothing to do without either.
// Function-call resolution uses the seed defaults
// (COUNT/SUM/MIN/MAX/AVG).
func New(analyzer *semantic.Analyzer, scope *semantic.Scope) *Resolver {
	return NewWithFunctionCatalog(analyzer, scope, nil)
}

// NewWithFunctionCatalog is the New variant that lets callers
// plug in a pre-built FunctionCatalog — used when scalar functions
// or user-registered aggregates need to be resolvable.
// Passing nil uses the seed defaults on first demand.
func NewWithFunctionCatalog(analyzer *semantic.Analyzer, scope *semantic.Scope, fc *semantic.FunctionCatalog) *Resolver {
	if analyzer == nil {
		panic("expr.New: analyzer is nil")
	}
	if scope == nil {
		panic("expr.New: scope is nil")
	}
	return &Resolver{analyzer: analyzer, scope: scope, funcCat: fc}
}

// functionCatalog returns the resolver's FunctionCatalog, lazily
// building the defaults on first use when none was supplied to New.
// The sync.Once guard makes the lazy build defensive — production
// usage is single-goroutine per Resolver, but several existing test
// patterns share one Resolver across `t.Parallel()` subtests.
func (r *Resolver) functionCatalog() *semantic.FunctionCatalog {
	r.funcCatOnce.Do(func() {
		if r.funcCat == nil {
			r.funcCat = semantic.NewFunctionCatalog()
			r.funcCat.RegisterDefaults()
		}
	})
	return r.funcCat
}

// ResolveIdentifier produces a cascades Value for a bare or
// qualified identifier reference. qualifier may be the zero
// Identifier for bare lookups.
//
// Currently produces a values.FieldValue for scope-resolved
// columns. Once QuantifiedObjectValue lookup lands in the logical-
// builder, this will produce a FieldValue wrapping a
// QuantifiedObjectValue to carry the source correlation.
//
// Row-key contract: FieldValue.Field is stored in the Identifier's
// case-folded form (upper-case unless the source was quoted).
// Callers feeding FieldValue.Evaluate a row map MUST key the map
// with the same case-folded names — otherwise lookup silently
// returns nil. The SQL executor's row-projection layer is
// responsible for that normalisation. Documented explicitly here
// because a subtle-lookup-fail is an easy trap when integrating.
//
// Returns the underlying semantic errors verbatim so callers can
// match via errors.As.
func (r *Resolver) ResolveIdentifier(qualifier, id semantic.Identifier) (values.Value, error) {
	col, src, err := r.analyzer.ResolveColumnRef(r.scope, qualifier, id)
	if err != nil {
		return nil, err
	}
	// Resolve to the OUTPUT column name verbatim (Java's SemanticAnalyzer.lookup
	// returns the output attribute as-is; FieldValue is indexed by the output
	// column). Derived-table/CTE quantifiers expose their columns under the
	// OUTPUT name the projection emits — no reverse-map to a source column.
	field := col.Id.Name()
	needsQualification := len(r.scope.Sources()) > 1
	if !needsQualification && src.CorrelationName != "" {
		isLocal := false
		for _, localSrc := range r.scope.Sources() {
			if localSrc.CorrelationName == src.CorrelationName {
				isLocal = true
				break
			}
		}
		if !isLocal {
			needsQualification = true
		}
	}
	if src.CorrelationName != "" && needsQualification {
		// A PARENT-scope resolution (Java's zero-match fallthrough) whose
		// correlation name is SHADOWED by a local source is
		// emitted-uncorrelatable — QOV(name) would bind the INNER leg's
		// quantifier (the executor's rebase cannot tell the two apart).
		// Decline LOUDLY here; this matches Java's behavior (unique
		// quantifier ids everywhere) and will flip once cross-scope binding
		// ids are supported (a known follow-on). The UNSHADOWED fallthrough
		// (distinct inner alias) emits normally below and answers.
		//
		// This branch is the MULTI-SOURCE-inner catch: with ≥2 inner sources
		// needsQualification is already true, so a shadowing local leg that
		// LACKS the column is caught here at PLAN time (CorrelatedShadowError
		// → 42703). The SINGLE-source-inner shadow (`… EXISTS (SELECT 1 FROM
		// q AS p …)`) no longer reaches either arm: the duplicate-FROM-alias
		// mint gives a single-table correlated-EXISTS inner a unique
		// CorrelationName, so the parent hit is NOT isLocal, no local
		// corrName matches src's, and the fallthrough EMITS QOV(parent)
		// normally — the query ANSWERS with Java's semantics. Both variants
		// are pinned by an FDB integration test for duplicate FROM-alias
		// handling.
		for _, localSrc := range r.scope.Sources() {
			if localSrc.CorrelationName != src.CorrelationName {
				continue
			}
			if _, ok := localSrc.Table.LookupColumn(id); ok {
				// The local source itself resolves this column — src IS
				// local (resolution never fell through).
				continue
			}
			return nil, &semantic.CorrelatedShadowError{Qualifier: qualifier.Name(), Field: field}
		}
		corrID := values.NamedCorrelationIdentifier(src.CorrelationName)
		// The CORRELATED arm binds the SOURCE-RELATIVE ordinal at
		// construction (Java's FieldValue.ofFieldName against the referent's
		// result type). The node is SourceRelativeBaked — UNPINNED and
		// single-accessor — so every admission/placement gate that keys on
		// the lazy reference shape treats it exactly like its lazy twin. At
		// runtime the correlation binds a source-shaped row (the source's own
		// row or its leg window), where the declared-column-order ordinal
		// reads the same slot a name lookup would have found. Unresolvable
		// (computed alias, empty derived-table catalog) is LOUD at plan
		// time (UnresolvableOrdinalError — born-baked, slice 2).
		if ord, rowType, domain, ok := sourceColumnOrdinal(src, field); ok {
			// A SHADOWING source is a lateral unnest's AS/AT binding, whose scope
			// entry is a VIRTUAL one-column table (RFC-142). That column list is a
			// RESOLUTION convenience — it is what makes `SELECT "X"` resolve — and
			// it is NOT the row the quantifier flows: an unnest element is ONE
			// array element, a scalar, and Java's own seed calls that the
			// isPrimitive() whole-object case. Stating it as a row here is the
			// difference between `_1 UNKNOWN` and `_1 RECORD<X>` in the merged
			// seed, and values.IsMixedSeedElementType discriminates the element
			// from a leg by exactly that record-ness.
			//
			// Written as an explicit UnknownType rather than a nil *RecordType:
			// a nil typed pointer in a Type interface is NOT a nil interface, so
			// it would type-assert as a *RecordType and read as a row anyway —
			// the exact conflation this branch exists to avoid, arrived at by the
			// one Go idiom that looks like it avoids it.
			flowed := values.Type(values.UnknownType)
			if !src.Shadowing {
				flowed = rowType
			}
			// The quantifier object CARRIES the row it flows, as Java's always
			// does (Quantifier.java:801-803). It used to be minted untyped, and
			// the cost was measured rather than theoretical: every consumer that
			// derives a frontier from this child — `legSlotIdentity` and the
			// join-rebase machinery through it — got an UNKNOWN domain and
			// declined the reference, beside the correct ordinal stamped on its
			// own path one argument later. All 126 leg-correlated reads on the
			// real-FDB corpus declined that way, which is what left the
			// qualified-name channel carrying reads that already knew their slot.
			return values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
				values.NewQuantifiedObjectValueOfType(corrID, flowed),
				field,
				ord,
				columnCascadesType(col),
				domain,
			), nil
		}
		return nil, &UnresolvableOrdinalError{Field: field, Source: src.CorrelationName}
	}
	// Bind the LOGICAL ordinal at plan time (Java's FieldValue.ofFieldName
	// resolving against the referent's result type, FieldValue.java:273-299).
	// The referent is the resolved source's output row; its logical column
	// order is src.Table.Columns() (declared order). First-match by
	// case-folded name — identical to the RecordType.FieldIndex rule — so the
	// bound slot is the same one a name read would have found. Unresolvable
	// (computed alias, no source table) is LOUD at plan time
	// (UnresolvableOrdinalError — born-baked, slice 2).
	// The row type is discarded here and only here: this arm emits a CHILDLESS
	// (source-relative) reference, which has no quantifier object to carry it.
	// Its domain still states the layout the ordinal indexes.
	if ord, _, domain, ok := sourceColumnOrdinal(src, field); ok {
		return values.NewFieldValueWithResolvedOrdinalInDomain(field, ord, columnCascadesType(col), domain), nil
	}
	return nil, &UnresolvableOrdinalError{Field: field, Source: src.Alias.Name()}
}

// UnresolvableOrdinalError reports a column resolution whose source
// cannot bind a plan-time ordinal (no declared column order — an empty
// derived-table catalog or a computed alias outside it). Every
// production SQL resolution binds one (verified empirically across the
// yamsql, embedded, and full FDB driver suites: the lazy fallbacks were
// dead-in-effect), so this is LOUD at plan time — never a lazy
// FieldValue that dies later as a runtime OrdinalResolutionError or,
// worse, reads by name (WS-N Phase A slice 2: born-baked resolutions).
type UnresolvableOrdinalError struct {
	Field  string
	Source string
}

func (e *UnresolvableOrdinalError) Error() string {
	return fmt.Sprintf("column %q resolves against source %q, which declares no column order to bind a plan-time ordinal", e.Field, e.Source)
}

// sourceRowType builds the ROW the resolved source flows: its declared columns,
// in declared order, each carrying the catalog's own type.
//
// This is the layout a source-relative ordinal indexes, stated as a type rather
// than as a bare signature — which is what lets a reference's quantifier object
// CARRY it (Java's `QuantifiedObjectValue.of(getAlias(), getFlowedObjectType())`,
// Quantifier.java:801-803, where the flowed value is never untyped).
//
// Built with a struct literal, not NewRecordType: that constructor PANICS on a
// duplicate field name, and a catalog is not this function's to validate — a
// degenerate source should decline downstream, not abort resolution.
//
// nil when the source declares no column order, which is exactly the condition
// sourceColumnOrdinal declines on, so the two answers cannot disagree.
func sourceRowType(src semantic.ScopeSource) *values.RecordType {
	if src.Table == nil {
		return nil
	}
	cols := src.Table.Columns()
	if len(cols) == 0 {
		return nil
	}
	fields := make([]values.Field, len(cols))
	for i, c := range cols {
		fields[i] = values.Field{Name: c.Id.Name(), FieldType: columnCascadesType(c), Ordinal: i}
	}
	return &values.RecordType{Fields: fields}
}

// sourceColumnOrdinal returns the 0-based position of field within the
// resolved source's declared column order — the LOGICAL ordinal of the
// column in the row the source flows. Matching is case-insensitive
// first-match, mirroring values.RecordType.FieldIndex.
//
// It also returns the ROW TYPE that ordinal indexes and the DOMAIN token for it.
// All three come from one walk of one column list, and the domain is derived
// FROM the row type rather than beside it, so a caller that stamps the row type
// on a reference's quantifier object and the domain on its resolved path is
// guaranteed to have stated ONE layout twice rather than two layouts that agree
// today. That guarantee is the point: `values.OrdinalIn` compares the path's
// domain against the frontier a consumer derives from the quantifier object's
// type, and those two derivations meeting is the whole precondition for a
// reference being able to state its identity.
func sourceColumnOrdinal(src semantic.ScopeSource, field string) (int, *values.RecordType, values.OrdinalDomain, bool) {
	rowType := sourceRowType(src)
	if rowType == nil {
		return 0, nil, values.OrdinalDomain{}, false
	}
	for i, f := range rowType.Fields {
		if strings.EqualFold(f.Name, field) {
			return i, rowType, values.OrdinalDomainOfType(rowType), true
		}
	}
	return 0, nil, values.OrdinalDomain{}, false
}

// ResolveQualifiedProjection resolves a QUALIFIED projection reference on the
// join path: it ALWAYS runs the per-attribute ambiguity check (Java's 42702)
// and returns a non-nil Value only when the reference binds to a LATER
// duplicate-alias leg (src.CorrelationName differs from the alias) — the
// QOV-correlated read addressing THAT leg's quantifier, which the ordinal
// bake resolves positionally. nil Value + nil error keeps the caller's legacy
// alias-keyed emission (behavior-preserving for
// every non-duplicate query); non-ambiguity resolution misses also return
// nil,nil (column validation owns 42703 on this path, as before). The
// AmbiguousColumnError disposition is per-caller: the projection path
// SURFACES it; the ORDER BY (qualifyShadowedSortKeys) and GROUP BY
// (upgradeAggregateOperands) callers deliberately DISCARD it, because the
// upstream reference validation has already terminated an ambiguous
// sort/group key with 42702 before those helpers run — the swallow is not a
// dead error path, do not "fix" it into one.
func (r *Resolver) ResolveQualifiedProjection(qualifier, id semantic.Identifier) (values.Value, error) {
	col, src, err := r.analyzer.ResolveColumnRef(r.scope, qualifier, id)
	if err != nil {
		var ambig *semantic.AmbiguousColumnError
		if errors.As(err, &ambig) {
			return nil, err
		}
		return nil, nil
	}
	if src.CorrelationName == "" {
		return nil, nil
	}
	if src.CorrelationName == src.Alias.Name() && !r.QualifierIsDuplicated(src.Alias) {
		// Unique alias bound by its own name — the caller's ordinary
		// emission owns it.
		return nil, nil
	}
	// Bind the source-relative ordinal at construction when the source's
	// declared column order resolves it (see ResolveIdentifier's correlated
	// arm) — the LATER-DUP-LEG binding (`q AS a`) resolves through the leg
	// window, since a lazy ref over the ordinal row is a loud runtime error.
	// The FIRST dup leg (which keeps the alias as its binding) bakes the
	// SAME way: its correlation is unique at the quantifier level — only
	// the later duplicates were renamed — so QOV(alias) addresses exactly
	// one leg and the executor's binding-keyed leg windows resolve it.
	// This retires the flat "ALIAS.COL" projection carve-out (the last
	// flat-name projection mint).
	// A dup-alias branch under UNION ALL reaches here too and bakes the
	// same per-binding way — no upstream decline remains.
	if ord, rowType, domain, ok := sourceColumnOrdinal(src, col.Id.Name()); ok {
		// Typed for the reason ResolveIdentifier's correlated arm states.
		return values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
			values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier(src.CorrelationName), rowType),
			col.Id.Name(),
			ord,
			columnCascadesType(col),
			domain,
		), nil
	}
	return nil, &UnresolvableOrdinalError{Field: col.Id.Name(), Source: src.CorrelationName}
}

// QualifierIsDuplicated reports whether the given qualifier ALIAS names MORE
// than one local scope source (`FROM p AS a, q AS a`) — a duplicate plain
// alias. A duplicated qualifier cannot be served by ordinary display-keyed
// emission (the duplicate-FROM-alias mint gives the sources distinct
// correlations), so the caller forces the per-binding bake: a
// source-relative ordinal against the resolved leg. A unique alias bound by
// its own name stays with ordinary emission.
func (r *Resolver) QualifierIsDuplicated(qualifier semantic.Identifier) bool {
	if qualifier.IsZero() {
		return false
	}
	n := 0
	for _, s := range r.scope.Sources() {
		if s.Alias.EqualsIgnoreQuoting(qualifier) {
			n++
		}
	}
	return n > 1
}

// ResolveColumnShadowingQualified resolves a column reference and, when it binds
// to a SHADOWING scope source (a lateral array unnest's AS/AT binding, RFC-142),
// returns a Value QUALIFIED to that source's correlation (FieldValue over
// QuantifiedObjectValue) — even for a BARE column where ResolveIdentifier would
// normally emit an UNqualified FieldValue.
//
// This is load-bearing for `FROM t, t.arr AS v, u` where a LATER FROM item `u`
// also has a column `v`: the unnest's element flows the merged row under BOTH the
// bare key `v` AND the qualified key `v.v`, but a subsequent join's mergeRows
// overwrites the bare `v` last-leg-wins with u.v. A bare `SELECT v` resolved to
// the unnest must therefore read the QUALIFIED `v.v` key (which mergeRows
// preserves verbatim — dotted keys are never re-prefixed), not the clobbered bare
// key. ok=false (and a nil Value) when the column does not bind to a shadowing
// source — the caller keeps its existing bare-column handling. A resolution error
// is returned verbatim (callers already validate separately, so they may ignore
// it). RFC-142.
func (r *Resolver) ResolveColumnShadowingQualified(qualifier, id semantic.Identifier) (values.Value, bool, error) {
	col, src, err := r.analyzer.ResolveColumnRef(r.scope, qualifier, id)
	if err != nil {
		return nil, false, err
	}
	if !src.Shadowing || src.CorrelationName == "" {
		return nil, false, nil
	}
	// OUTPUT column name verbatim (see ResolveIdentifier).
	field := col.Id.Name()
	corrID := values.NamedCorrelationIdentifier(src.CorrelationName)
	// Bind the source-relative ordinal at construction when the shadowing
	// source's declared column order resolves it (see ResolveIdentifier's
	// correlated arm); unresolvable is LOUD at plan time (born-baked).
	if ord, rowType, domain, ok := sourceColumnOrdinal(src, field); ok {
		// Typed for the reason ResolveIdentifier's correlated arm states.
		return values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
			values.NewQuantifiedObjectValueOfType(corrID, rowType),
			field,
			ord,
			columnCascadesType(col),
			domain,
		), true, nil
	}
	return nil, false, &UnresolvableOrdinalError{Field: field, Source: src.CorrelationName}
}

// ResolveArithmetic wraps left/right Values in a cascades
// ArithmeticValue with the given operator. Used when the parser
// produces an arithmetic expression node — the analyzer resolves
// each operand recursively, then pairs them here.
//
// Operand types aren't cross-checked in the seed (both assumed
// int); real type inference replaces this when the Type hierarchy
// port lands.
func (r *Resolver) ResolveArithmetic(op values.ArithmeticOp, left, right values.Value) (values.Value, error) {
	if left == nil || right == nil {
		return nil, fmt.Errorf("expr.ResolveArithmetic: operand is nil")
	}
	return &values.ArithmeticValue{Op: op, Left: left, Right: right}, nil
}

// ResolveComparison wraps left/right Values in a cascades
// ComparisonPredicate. Mirrors the analyzer's job of lifting
// `a > b` from a parse-tree comparison node to a predicate node.
//
// Both LHS and RHS are carried as Values — non-constant RHS
// (`a = b`, `a < b + 1`, `a = CAST(col AS INT)`) composes uniformly
// with constant RHS. Plan-time folding (`5 = 5` → TRUE) happens in
// ComparisonConstantSimplifyRule when both sides are constant;
// row-context evaluation (FieldValue RHS) runs through
// ComparisonPredicate.Eval.
//
// Does NOT pre-fold even when both operands are constant. `5 = 5`
// produces a real ComparisonPredicate; the fixpoint simplifier
// folds it to TRUE via ComparisonConstantSimplifyRule. Eager
// folding here would hide foldable shapes from rule matchers that
// expect to see them.
func (r *Resolver) ResolveComparison(op predicates.ComparisonType, left, right values.Value) (predicates.QueryPredicate, error) {
	if left == nil || right == nil {
		return nil, fmt.Errorf("expr.ResolveComparison: operand is nil")
	}
	left, right = widenConstAgainstDoubleColumn(op, left, right)
	op, left, right = narrowFloatConstAgainstInt(op, left, right)
	op, left, right = narrowConstAgainstFloatColumn(op, left, right)
	left, right = promoteColumnColumnNumeric(left, right)
	left, right = promoteStringComparandToUuid(op, left, right)
	// PLAN-TIME promotion gate (Java SemanticAnalyzer + PromoteValue
	// lattice): a comparison whose operand types have NO common maximum
	// (STRING vs a number, BOOLEAN vs a number, BYTES vs anything else)
	// rejects 42804 at plan time with Java's exact message. The runtime
	// dispatch degraded such pairs to UNKNOWN per row — silent empty
	// results where Java errors, including the empty-table shape where
	// no row was ever evaluated. UNKNOWN-typed operands (bound
	// parameters, internal untyped expressions) keep the runtime path.
	if lt, rt := left.Type(), right.Type(); lt != nil && rt != nil &&
		lt.Code() != values.TypeCodeUnknown && rt.Code() != values.TypeCodeUnknown {
		if values.MaximumType(lt, rt) == nil {
			return nil, api.NewErrorf(api.ErrCodeDatatypeMismatch,
				"The operands of a comparison operator are not compatible.")
		}
	}
	return predicates.NewComparisonPredicate(left, predicates.Comparison{
		Type: op, Operand: right,
	}), nil
}

// promoteStringComparandToUuid types a STRING comparand compared against a UUID
// operand as UUID, mirroring Java (where the comparand is a java.util.UUID, not
// a String). A UUID column has no native proto/SQL primitive — `uuid_col =
// '<uuid>'` arrives with the column typed UUID and the literal typed STRING.
// Without this promotion the comparand evaluates to a Go string: an index-scan
// range packs a 0x02 string tuple element that never matches the 0x30 UUID
// entry, and the residual/filter path (predicates.cmpAny) has no string↔[16]byte
// arm so it returns UNKNOWN. Wrapping the STRING operand in a PromoteValue
// toward the UUID type makes it parse to a neutral [16]byte at eval time
// (PromoteValue.Evaluate's STRING_TO_UUID arm), which the executor's scan packer
// then seeks as a tuple.UUID and cmpAny compares as [16]byte==[16]byte.
//
// This is comparand TYPING (a semantic property of the value), deliberately NOT
// gated on the SARG operator whitelist the numeric widenConstAgainstDoubleColumn
// sibling uses: for numerics cmpAny widens int↔float at compare time so a
// non-SARG operator (e.g. IS DISTINCT FROM) still compares correctly unpromoted,
// but cmpAny cannot compensate for string-vs-[16]byte, so EVERY comparison
// operator that reaches here (=, <>, <, <=, >, >=, IS [NOT] DISTINCT FROM) must
// type its comparand or return wrong rows. LIKE/STARTS_WITH/IN take separate
// resolvers, so only genuine equality/ordering/distinct-from ops arrive.
//
// A col-vs-col UUID join (both sides already UUID) is left untouched — its
// comparand already evaluates to [16]byte, so no string coercion is needed and
// none is inserted (this is the INL-join-key case a positional mask got wrong).
func promoteStringComparandToUuid(_ predicates.ComparisonType, left, right values.Value) (values.Value, values.Value) {
	lt, rt := left.Type(), right.Type()
	if lt == nil || rt == nil {
		return left, right
	}
	if values.IsUuid(lt) && promotableToUuid(rt) {
		return left, values.NewPromoteValue(right, lt)
	}
	if values.IsUuid(rt) && promotableToUuid(lt) {
		return values.NewPromoteValue(left, rt), right
	}
	return left, right
}

// promotableToUuid reports whether a comparand of type t, compared against a
// UUID operand, should be typed UUID. STRING is the literal case
// (`uuid_col = '<uuid>'`); UNKNOWN covers a bound parameter (`uuid_col = ?`,
// ParameterValue.Typ stays UnknownType until inference — which never types it
// UUID) reached via the exported planner path (the sqldriver substitutes `?`
// as text, so it lands as a STRING there, but PlanRecordQueryWithMetadata +
// ParameterValue bindings do not). Wrapping either in PromoteValue(UUID) is
// safe: PromoteValue.Evaluate parses a bound string to [16]byte and passes a
// non-string (already-[16]byte, or a genuinely mismatched value) through
// unchanged, so a non-string parameter degrades to the same UNKNOWN compare it
// gave before — never a wrong-typed tuple probe.
func promotableToUuid(t values.Type) bool {
	switch t.Code() {
	case values.TypeCodeString, values.TypeCodeUnknown:
		return true
	default:
		return false
	}
}

// widenConstAgainstDoubleColumn fixes a cross-type index-SARG hole: when one
// operand of an ordered/equality comparison is a compile-time CONSTANT NARROWER
// than DOUBLE (an INT/LONG integer, or a binary32 FLOAT) and the other is a
// non-constant DOUBLE-typed value (typically an indexed column), it widens the
// constant to a DOUBLE constant (e.g. `5` → `5.0`). Without this the constant is
// packed into the index range under its own tuple type code — int, or 0x20
// single-float — which never interleaves with the column's 0x21 tuple-double
// entries: an equality probe misses every row and an inequality range
// degenerates to all-or-nothing. Coercing the constant to the column's type
// makes the SARG pack it under the right tuple type while leaving the (indexed)
// column operand untouched, so its index is still matched.
//
// The FLOAT arm widens the ALREADY-ROUNDED binary32 value rather than the
// original literal: `d = CAST(0.1 AS FLOAT)` must compare d against the binary32
// value of 0.1 promoted back to binary64, which is NOT binary64 0.1. Rounding
// happens in CastValue; this only re-declares the type so the value packs under
// the column's wire code.
//
// Deliberately NARROW — only a constant vs a DOUBLE non-constant. INT↔LONG share
// tuple encoding (no fix needed; wrapping would needlessly do work), and the
// col-vs-col cases go through promoteColumnColumnNumeric. The residual
// (non-index) path already coerces via cmpAny, so this only changes the value's
// declared type, never the matched rows for non-index cases.
func widenConstAgainstDoubleColumn(op predicates.ComparisonType, left, right values.Value) (values.Value, values.Value) {
	switch op {
	case predicates.ComparisonEquals, predicates.ComparisonNotEquals,
		predicates.ComparisonLessThan, predicates.ComparisonLessThanOrEq,
		predicates.ComparisonGreaterThan, predicates.ComparisonGreaterThanEq:
	default:
		return left, right
	}
	lc, rc := values.IsConstantValue(left), values.IsConstantValue(right)
	if lc == rc { // need exactly one constant operand
		return left, right
	}
	constV, colV := left, right
	if rc {
		constV, colV = right, left
	}
	ct, colt := constV.Type(), colV.Type()
	if ct == nil || colt == nil {
		return left, right
	}
	narrowerThanDouble := ct.Code() == values.TypeCodeInt || ct.Code() == values.TypeCodeLong ||
		ct.Code() == values.TypeCodeFloat
	if !narrowerThanDouble || colt.Code() != values.TypeCodeDouble {
		return left, right
	}
	cv, ok := values.EvaluateConstant(constV)
	if !ok {
		return left, right
	}
	var f float64
	switch n := cv.(type) {
	case int64:
		f = float64(n)
	case int32:
		f = float64(n)
	case int:
		f = float64(n)
	case float64:
		// A FLOAT-typed constant already carries binary32 precision (CastValue's
		// FLOAT arm rounded it); widening to binary64 is exact and lossless.
		f = n
	case float32:
		f = float64(n)
	default:
		return left, right
	}
	// Typ: colt (the COLUMN's actual DOUBLE type) is load-bearing — do NOT change it
	// to a generic float/double type var. The SARG packing path keys on the declared
	// column type to choose the FDB tuple type code; using anything but the column's
	// own type descriptor would encode the value with a different wire type and
	// silently miss every index entry (the bug this function fixes).
	coerced := &values.ConstantValue{Value: f, Typ: colt}
	if lc {
		return coerced, right
	}
	return left, coerced
}

// mirrorOp flips an ordered comparison to the other operand's point of
// view (a < b ≡ b > a). Equality-family ops are their own mirror.
func mirrorOp(op predicates.ComparisonType) predicates.ComparisonType {
	switch op {
	case predicates.ComparisonLessThan:
		return predicates.ComparisonGreaterThan
	case predicates.ComparisonLessThanOrEq:
		return predicates.ComparisonGreaterThanEq
	case predicates.ComparisonGreaterThan:
		return predicates.ComparisonLessThan
	case predicates.ComparisonGreaterThanEq:
		return predicates.ComparisonLessThanOrEq
	}
	return op
}

// narrowFloatConstAgainstInt is widenConstAgainstDoubleColumn's INVERSE:
// a DOUBLE/FLOAT compile-time CONSTANT compared against a non-constant
// INT/LONG-typed value (an integer column) packs as a tuple-double
// against int-encoded index/PK entries — a different tuple type code,
// so an equality probe silently missed every row (`bigint_col = 1.0`
// returned empty where Java compares 1 = 1.0 true) and range bounds
// ordered by type code, not value. An INTEGRAL constant narrows to the
// column's integer type (same value, correct tuple code); a
// NON-integral bound rewrites to the equivalent integer predicate:
//
//	col >  1.5  ≡  col >= 2      col <  1.5  ≡  col <= 1
//	col >= 1.5  ≡  col >= 2      col <= 1.5  ≡  col <= 1
//
// Equality/inequality against a non-integral constant stays as-is:
// `=` matches nothing (value-correct on any path) and `<>` is served
// by the residual comparison, which widens numerics itself.
func narrowFloatConstAgainstInt(op predicates.ComparisonType, left, right values.Value) (predicates.ComparisonType, values.Value, values.Value) {
	switch op {
	case predicates.ComparisonEquals, predicates.ComparisonNotEquals,
		predicates.ComparisonLessThan, predicates.ComparisonLessThanOrEq,
		predicates.ComparisonGreaterThan, predicates.ComparisonGreaterThanEq:
	default:
		return op, left, right
	}
	lc, rc := values.IsConstantValue(left), values.IsConstantValue(right)
	if lc == rc {
		return op, left, right
	}
	constV, colV := left, right
	if rc {
		constV, colV = right, left
	}
	ct, colt := constV.Type(), colV.Type()
	if ct == nil || colt == nil {
		return op, left, right
	}
	isFloatConst := ct.Code() == values.TypeCodeDouble || ct.Code() == values.TypeCodeFloat
	isIntCol := colt.Code() == values.TypeCodeInt || colt.Code() == values.TypeCodeLong
	if !isFloatConst || !isIntCol {
		return op, left, right
	}
	cv, ok := values.EvaluateConstant(constV)
	if !ok {
		return op, left, right
	}
	f, fok := cv.(float64)
	if !fok {
		return op, left, right
	}
	if math.IsNaN(f) {
		return op, left, right
	}
	// A bound BEYOND the int64 range: the raw double would pack with the
	// wrong tuple type code (ordering by type, not value — the bug this
	// helper fixes), so clamp the ORDERED ops to the always-true/false
	// integer form instead: every int64 is > -1e19, none is > 1e19.
	// Equality leaves as-is (a probe that misses everything IS the
	// correct empty result); inequality reaches the residual comparison,
	// which widens numerics itself.
	if math.IsInf(f, 0) || f < math.MinInt64 || f >= math.MaxInt64 {
		switch op {
		case predicates.ComparisonEquals, predicates.ComparisonNotEquals:
			return op, left, right
		}
		below := f < 0 // out of range on the NEGATIVE side
		clamp := int64(math.MaxInt64)
		if below {
			clamp = math.MinInt64
		}
		colRelOp := op
		if lc {
			colRelOp = mirrorOp(op)
		}
		// col > +huge / col >= +huge → false ≡ col > MaxInt64;
		// col < +huge / col <= +huge → true ≡ col <= MaxInt64;
		// col > -huge / col >= -huge → true ≡ col >= MinInt64;
		// col < -huge / col <= -huge → false ≡ col < MinInt64.
		switch colRelOp {
		case predicates.ComparisonGreaterThan, predicates.ComparisonGreaterThanEq:
			if below {
				colRelOp = predicates.ComparisonGreaterThanEq
			} else {
				colRelOp = predicates.ComparisonGreaterThan
			}
		case predicates.ComparisonLessThan, predicates.ComparisonLessThanOrEq:
			if below {
				colRelOp = predicates.ComparisonLessThan
			} else {
				colRelOp = predicates.ComparisonLessThanOrEq
			}
		}
		if lc {
			op = mirrorOp(colRelOp)
		} else {
			op = colRelOp
		}
		coerced := &values.ConstantValue{Value: clamp, Typ: colt}
		if lc {
			return op, coerced, right
		}
		return op, left, coerced
	}
	integral := f == math.Trunc(f)
	var n int64
	if integral {
		n = int64(f)
	} else {
		// Rewrite the bound to the tightest integer predicate. The op
		// direction is relative to the COLUMN side: when the constant is
		// on the LEFT (`1.5 < col`), the column-relative op is mirrored.
		colRelOp := op
		if lc {
			colRelOp = mirrorOp(op)
		}
		switch colRelOp {
		case predicates.ComparisonGreaterThan, predicates.ComparisonGreaterThanEq:
			n = int64(math.Ceil(f))
			colRelOp = predicates.ComparisonGreaterThanEq
		case predicates.ComparisonLessThan, predicates.ComparisonLessThanOrEq:
			n = int64(math.Floor(f))
			colRelOp = predicates.ComparisonLessThanOrEq
		default:
			// Equality family with a non-integral constant: leave as-is.
			return op, left, right
		}
		if lc {
			op = mirrorOp(colRelOp)
		} else {
			op = colRelOp
		}
	}
	// Typ: colt (the COLUMN's integer type) is load-bearing for the SARG
	// packing path, exactly as in widenConstAgainstDoubleColumn.
	coerced := &values.ConstantValue{Value: n, Typ: colt}
	if lc {
		return op, coerced, right
	}
	return op, left, coerced
}

// constAsFloat64 widens a plan-time-evaluated numeric literal (whatever
// concrete Go type EvaluateConstant handed back) to float64, the common
// working precision for the exactness/ceil/floor arithmetic below.
func constAsFloat64(cv any) (float64, bool) {
	switch n := cv.(type) {
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case int:
		return float64(n), true
	case float64:
		return n, true
	case float32:
		return float64(n), true
	}
	return 0, false
}

// narrowConstAgainstFloatColumn fixes the cross-WIDTH sibling of
// widenConstAgainstDoubleColumn/narrowFloatConstAgainstInt: an INT/LONG/DOUBLE
// compile-time CONSTANT compared against a non-constant FLOAT (32-bit)
// value (an indexed FLOAT column) packs the wrong FDB tuple type code
// (0x21 double, or a bare int code) against the column's 0x20 single-float
// index entries — an equality probe misses every row and an inequality
// range degenerates to all-or-nothing, exactly like the int-vs-DOUBLE bug,
// just one width narrower. `f = 1.5` / `f > 1.0` against an indexed FLOAT
// column returned ZERO rows for every comparison before this: FLOAT was
// the only numeric column type NO widen/narrow rule covered at all.
//
// Unlike widenConstAgainstDoubleColumn, this function checks exactness. The
// two are asymmetric for a REASON, and the asymmetry is easy to misread as an
// oversight:
//
//   - int→double is NOT always exact — float64 has a 53-bit mantissa, so any
//     |n| > 2^53 rounds. But widening anyway is CORRECT: SQL converts an exact
//     numeric to approximate when comparing the two, so `double_col = 2^53+1`
//     legitimately matches a stored 2^53. Postgres agrees. Adding an exactness
//     check there would DECLINE a conversion the standard mandates and turn
//     correct rows into missing ones. (An earlier version of this comment said
//     int→double is "always exact for any realistic int64", which is false and
//     invited exactly that non-fix.)
//   - float64→float32 is different: the FLOAT column's index entries are packed
//     under a different tuple type code, so a non-exact narrowing does not just
//     lose precision, it probes a key that cannot exist. That is why this
//     function checks exactness explicitly (round-trip float64(float32(f)) == f)
//     rather than assuming it:
//   - exact: retype the bare constant to float32 with the column's own
//     Type (same pattern as the two sibling helpers — a bare re-typed
//     ConstantValue, not a PromoteValue, because the SARG range-builder
//     packs ConstantValue.Evaluate() directly).
//   - non-exact equality/inequality (=, <>): left UNPROMOTED. This is
//     value-correct on every path: a float32-tuple index entry can never
//     byte-match a differently-type-coded double/int constant, so the SARG
//     probe returns empty — which coincides with the true answer, since no
//     float32 can equal a value it cannot exactly represent.
//   - non-exact ordered bound (<, <=, >, >=): rewritten to the TIGHTEST
//     float32 predicate via float32Ceil/float32Floor (this function's
//     analogue of narrowFloatConstAgainstInt's math.Ceil/math.Floor),
//     exactly mirroring that helper's op-rewrite table: GT/GE round up to
//     the ceiling and become GE; LT/LE round down to the floor and become
//     LE; sides mirrored when the constant is on the left.
//   - NaN: left unpromoted (every comparison with NaN is false/unknown in
//     SQL, and the mismatched tuple type code makes the SARG naturally
//     empty — the same fortunate coincidence as the non-exact EQ case).
func narrowConstAgainstFloatColumn(op predicates.ComparisonType, left, right values.Value) (predicates.ComparisonType, values.Value, values.Value) {
	switch op {
	case predicates.ComparisonEquals, predicates.ComparisonNotEquals,
		predicates.ComparisonLessThan, predicates.ComparisonLessThanOrEq,
		predicates.ComparisonGreaterThan, predicates.ComparisonGreaterThanEq:
	default:
		return op, left, right
	}
	lc, rc := values.IsConstantValue(left), values.IsConstantValue(right)
	if lc == rc {
		return op, left, right
	}
	constV, colV := left, right
	if rc {
		constV, colV = right, left
	}
	ct, colt := constV.Type(), colV.Type()
	if ct == nil || colt == nil || colt.Code() != values.TypeCodeFloat {
		return op, left, right
	}
	switch ct.Code() {
	case values.TypeCodeInt, values.TypeCodeLong, values.TypeCodeDouble:
	default:
		return op, left, right
	}
	cv, ok := values.EvaluateConstant(constV)
	if !ok {
		return op, left, right
	}
	f, ok := constAsFloat64(cv)
	if !ok {
		return op, left, right
	}
	if math.IsNaN(f) {
		return op, left, right
	}
	if f32 := float32(f); float64(f32) == f {
		// Exact — same value, correct tuple code (colt, not a generic
		// float type descriptor — same load-bearing reason as the sibling
		// helpers' Typ comment).
		coerced := &values.ConstantValue{Value: f32, Typ: colt}
		if lc {
			return op, coerced, right
		}
		return op, left, coerced
	}
	switch op {
	case predicates.ComparisonEquals, predicates.ComparisonNotEquals:
		return op, left, right
	}
	colRelOp := op
	if lc {
		colRelOp = mirrorOp(op)
	}
	var bound float32
	switch colRelOp {
	case predicates.ComparisonGreaterThan, predicates.ComparisonGreaterThanEq:
		bound = float32Ceil(f)
		colRelOp = predicates.ComparisonGreaterThanEq
	case predicates.ComparisonLessThan, predicates.ComparisonLessThanOrEq:
		bound = float32Floor(f)
		colRelOp = predicates.ComparisonLessThanOrEq
	}
	if lc {
		op = mirrorOp(colRelOp)
	} else {
		op = colRelOp
	}
	coerced := &values.ConstantValue{Value: bound, Typ: colt}
	if lc {
		return op, coerced, right
	}
	return op, left, coerced
}

// isNumericTypeCode reports whether c is one of the four primitive
// numeric type codes the promotion lattice orders (INT < LONG < FLOAT <
// DOUBLE, matching Java's Type.maximumType). Used to gate
// promoteColumnColumnNumeric to genuinely numeric pairs.
func isNumericTypeCode(c values.TypeCode) bool {
	switch c {
	case values.TypeCodeInt, values.TypeCodeLong, values.TypeCodeFloat, values.TypeCodeDouble:
		return true
	}
	return false
}

// sharesIntegerWireEncoding reports whether both codes are INT and/or LONG —
// the only two numeric codes that funnel to the SAME FDB tuple encoding.
// pkg/fdbgo/fdb/tuple's encodeInt takes any Go integer width as int64 and
// packs purely by magnitude, so an INT column and a LONG column holding the
// same value produce IDENTICAL wire bytes. FLOAT and DOUBLE do not share
// this property — they use distinct tuple type codes (0x20 vs 0x21, see
// encodeFloat/encodeDouble) — so this deliberately does not generalize to
// "any two numeric codes". Used by promoteColumnColumnNumeric to skip a
// promotion wrapper that would change nothing about the bytes.
func sharesIntegerWireEncoding(a, b values.TypeCode) bool {
	isIntFamily := func(c values.TypeCode) bool {
		return c == values.TypeCodeInt || c == values.TypeCodeLong
	}
	return isIntFamily(a) && isIntFamily(b)
}

// promoteColumnColumnNumeric wraps the narrower-typed operand of a
// numeric comparison in a values.PromoteValue toward the pair's
// MaximumType, when NEITHER operand is a compile-time constant. Mirrors
// Java's RelOpValue.encapsulate (PromoteValue.java call sites in
// RelOpValue.java: `lhs = PromoteValue.inject(lhs, maximumType); rhs =
// PromoteValue.inject(rhs, maximumType);`) for the one shape the
// constant-specific helpers above cannot handle: a correlated equi-join
// comparand — `a.xbig (BIGINT) = bd.ydbl (DOUBLE)` lowered to an
// index-nested-loop probe against bd's DOUBLE index — whose concrete
// value isn't known until each outer row is read, so there is no
// compile-time literal to retype in place.
//
// The widen/narrow helpers above retype a bare ConstantValue directly
// (Java's PromoteValue.inject "value.with(promoteToType)" fast path,
// taken because a literal's concrete value IS known at plan time); this
// helper takes Java's OTHER branch — wrap in an actual PromoteValue node
// — because a FieldValue/CorrelatedFieldValue cannot declare a different
// result type without a real per-row coercion. values.PromoteValue.Evaluate
// (coerceNumericResult) performs that coercion at row-eval time, and the
// executor's tuple-packing boundary (coerceTupleElement) narrows a
// FLOAT-targeted result to a genuine Go float32 so the wire encoding
// matches the indexed column's tuple type code — the same division of
// labor as the bare-constant path, just split across plan time (retype)
// vs. row time (wrap + coerce) depending on whether a value is known yet.
//
// An INT-vs-LONG pair is deliberately left UNWRAPPED (sharesIntegerWireEncoding):
// the two codes pack to identical wire bytes, so a PromoteValue here buys
// nothing at the encoding boundary and only costs a match — AccessorNamePath
// (values/accessor_name_path.go) walks *FieldValue chains and stops at any
// other node type, so a wrapped column no longer matches an index
// placeholder's raw FieldValue in the SARG matcher (valuesMatchColumn),
// degrading a point lookup to a residual full scan. cmpAny already compares
// mixed-width ints correctly (values.CompareExactInts) with no promotion
// needed. FLOAT-vs-DOUBLE still needs the wrapper: different wire type
// codes, and the exact-representable-bound narrowing the comparison-
// resolution callers above perform depends on it.
func promoteColumnColumnNumeric(left, right values.Value) (values.Value, values.Value) {
	if values.IsConstantValue(left) || values.IsConstantValue(right) {
		return left, right
	}
	lt, rt := left.Type(), right.Type()
	if lt == nil || rt == nil || lt.Code() == rt.Code() {
		return left, right
	}
	if !isNumericTypeCode(lt.Code()) || !isNumericTypeCode(rt.Code()) {
		return left, right
	}
	if sharesIntegerWireEncoding(lt.Code(), rt.Code()) {
		return left, right
	}
	common := values.MaximumType(lt, rt)
	if common == nil {
		return left, right
	}
	if lt.Code() != common.Code() {
		left = values.NewPromoteValue(left, common)
	}
	if rt.Code() != common.Code() {
		right = values.NewPromoteValue(right, common)
	}
	return left, right
}

// ResolveCast wraps v in a CastValue with the target type. Rejects
// nil child (programmer error) and Unknown target (use the direct
// Value if the target is genuinely unknown).
func (r *Resolver) ResolveCast(v values.Value, target values.Type) (values.Value, error) {
	if v == nil {
		return nil, fmt.Errorf("expr.ResolveCast: child is nil")
	}
	if target == nil || target.Code() == values.TypeCodeUnknown {
		return nil, fmt.Errorf("expr.ResolveCast: target UnknownType")
	}
	// PLAN-TIME pair check (Java resolves the cast operator at
	// construction and fails "No cast defined from X to Y",
	// CastValue.java:480-489) — a per-row rejection alone leaves the
	// empty-table shape silently succeeding. Unknown-typed children keep
	// the runtime dispatch.
	if st := v.Type(); st != nil && st.Code() != values.TypeCodeUnknown {
		if !values.CastPairDefined(st.Code(), target.Code()) {
			return nil, api.NewErrorf(api.ErrCodeInvalidCast,
				"No cast defined from %v to %v", st.Code(), target.Code())
		}
	}
	return values.NewCastValue(v, target), nil
}

// ResolveIsNull builds `v IS NULL`. Unary — Comparison.Operand is
// nil (Eval ignores it for unary types).
func (r *Resolver) ResolveIsNull(v values.Value) (predicates.QueryPredicate, error) {
	if v == nil {
		return nil, fmt.Errorf("expr.ResolveIsNull: operand is nil")
	}
	return predicates.NewComparisonPredicate(v, predicates.Comparison{Type: predicates.ComparisonIsNull}), nil
}

// ResolveIsNotNull builds `v IS NOT NULL`. Unary.
func (r *Resolver) ResolveIsNotNull(v values.Value) (predicates.QueryPredicate, error) {
	if v == nil {
		return nil, fmt.Errorf("expr.ResolveIsNotNull: operand is nil")
	}
	return predicates.NewComparisonPredicate(v, predicates.Comparison{Type: predicates.ComparisonIsNotNull}), nil
}

// ResolveLike builds `lhs LIKE pattern`. Pattern must be a plan-time
// constant string (parameter-bound patterns land with the
// parameter-Comparison design).
func (r *Resolver) ResolveLike(lhs values.Value, pattern values.Value) (predicates.QueryPredicate, error) {
	return r.ResolveLikeWithEscape(lhs, pattern, 0)
}

// ResolveLikeWithEscape is the LIKE … ESCAPE form. escape == 0 is
// equivalent to ResolveLike. Pattern must be a plan-time constant
// string. The escape rune is carried verbatim on the resulting
// Comparison.
func (r *Resolver) ResolveLikeWithEscape(lhs values.Value, pattern values.Value, escape rune) (predicates.QueryPredicate, error) {
	if lhs == nil || pattern == nil {
		return nil, fmt.Errorf("expr.ResolveLike: operand is nil")
	}
	lit, ok := values.EvaluateConstant(pattern)
	if !ok {
		return nil, fmt.Errorf("expr.ResolveLike: pattern must be a constant in the seed; got %T", pattern)
	}
	s, ok := lit.(string)
	if !ok {
		return nil, fmt.Errorf("expr.ResolveLike: pattern must be a string; got %T", lit)
	}
	// PLAN-TIME LHS gate: LIKE is a string predicate — a numeric or
	// boolean LHS rejects 42804 like Java's SemanticAnalyzer, never a
	// silent per-row UNKNOWN. STRING and the string-promotable temporal
	// extension types pass; Unknown keeps the runtime path.
	if lt := lhs.Type(); lt != nil {
		switch lt.Code() {
		case values.TypeCodeString, values.TypeCodeUnknown, values.TypeCodeNull,
			values.TypeCodeEnum, values.TypeCodeDate, values.TypeCodeTimestamp:
		default:
			return nil, api.NewErrorf(api.ErrCodeDatatypeMismatch,
				"The operands of a comparison operator are not compatible.")
		}
	}
	return predicates.NewComparisonPredicate(lhs, predicates.Comparison{
		Type:    predicates.ComparisonLike,
		Operand: values.LiteralValue(s),
		Escape:  escape,
	}), nil
}

// ResolveStartsWith builds `lhs STARTS_WITH prefix`. Prefix must be
// a constant string.
func (r *Resolver) ResolveStartsWith(lhs values.Value, prefix values.Value) (predicates.QueryPredicate, error) {
	if lhs == nil || prefix == nil {
		return nil, fmt.Errorf("expr.ResolveStartsWith: operand is nil")
	}
	lit, ok := values.EvaluateConstant(prefix)
	if !ok {
		return nil, fmt.Errorf("expr.ResolveStartsWith: prefix must be a constant in the seed; got %T", prefix)
	}
	s, ok := lit.(string)
	if !ok {
		return nil, fmt.Errorf("expr.ResolveStartsWith: prefix must be a string; got %T", lit)
	}
	return predicates.NewComparisonPredicate(lhs, predicates.Comparison{
		Type: predicates.ComparisonStartsWith, Operand: values.LiteralValue(s),
	}), nil
}

// ResolveIn builds a ComparisonPredicate{ComparisonIn} from a left
// Value and a list of constant RHS values. Every RHS must be a
// plan-time constant (per values.EvaluateConstant); non-constant
// elements return an error so callers lift the expression-based
// IN-list to the value-space form explicitly.
//
// The RHS Operand is a []any of evaluated literals.
func (r *Resolver) ResolveIn(left values.Value, rhs []values.Value) (predicates.QueryPredicate, error) {
	if left == nil {
		return nil, fmt.Errorf("expr.ResolveIn: LHS is nil")
	}
	// Same cross-type index-SARG fix as widenConstAgainstDoubleColumn, for IN: when
	// the LHS is a DOUBLE column, widen integer list elements to float64 so each
	// IN sub-probe packs the right tuple type (else `d IN (5,7)` over a DOUBLE
	// index misses everything — int and double tuple elements don't interleave).
	widenIntToDouble := left.Type() != nil && left.Type().Code() == values.TypeCodeDouble
	// Sibling fix for a FLOAT (32-bit) indexed column: narrow each element to
	// float32 IF exactly representable (same exactness rule as
	// narrowConstAgainstFloatColumn — a float32 index entry can never
	// byte-match a differently-type-coded comparand, so a non-exact element
	// is left as its original numeric type: the sub-probe naturally matches
	// nothing, which is correct since no float32 value could equal it anyway).
	narrowToFloat32 := left.Type() != nil && left.Type().Code() == values.TypeCodeFloat
	// Same SARG-type fix for a UUID column: parse each STRING element to the
	// neutral [16]byte the value layer carries UUIDs as, so `uuid_col IN
	// ('<u1>','<u2>')` packs a tuple.UUID per sub-probe (the InJoin binds each
	// element and the executor's scan packer converts [16]byte→tuple.UUID) and
	// the residual path compares [16]byte==[16]byte. Mirrors the equality
	// PromoteValue arm in promoteStringComparandToUuid.
	parseStringToUUID := left.Type() != nil && values.IsUuid(left.Type())
	// Java's constant-array type unification (SemanticAnalyzer.
	// resolveArrayTypeFromElementTypes): all non-NULL elements must have ONE
	// type, else DATATYPE_MISMATCH — checked on the RAW literal types, before
	// any LHS-driven normalization, exactly where Java checks. Without this,
	// a mixed list (`id IN (1, 'two', 3)`) kept the string verbatim and the
	// point-probe silently matched nothing — silent wrong rows where Java
	// rejects at plan time.
	inElemClass := func(lit any) string {
		switch lit.(type) {
		case int64, int32, int:
			return "LONG"
		case float64, float32:
			return "DOUBLE"
		case string:
			return "STRING"
		case bool:
			return "BOOLEAN"
		case []byte:
			return "BYTES"
		}
		return fmt.Sprintf("%T", lit)
	}
	// PLAN-TIME LHS-vs-element promotion gate, matching the equality
	// gate in ResolveComparison: `s IN (1, 2)` must reject 42804 like
	// Java, not silently match nothing. Unknown-typed sides keep the
	// runtime path.
	if lt := left.Type(); lt != nil && lt.Code() != values.TypeCodeUnknown {
		for _, v := range rhs {
			if v == nil {
				continue
			}
			et := v.Type()
			if et == nil || et.Code() == values.TypeCodeUnknown {
				continue
			}
			if values.MaximumType(lt, et) == nil {
				return nil, api.NewErrorf(api.ErrCodeDatatypeMismatch,
					"The operands of a comparison operator are not compatible.")
			}
		}
	}
	seenClass := ""
	list := make([]any, 0, len(rhs))
	for i, v := range rhs {
		lit, ok := values.EvaluateConstant(v)
		if !ok {
			return nil, fmt.Errorf("expr.ResolveIn: element %d is not constant (%T)", i, v)
		}
		if lit != nil {
			if c := inElemClass(lit); seenClass == "" {
				seenClass = c
			} else if c != seenClass {
				// Java verbatim message (Assert.thatUnchecked, ErrorCode.DATATYPE_MISMATCH).
				return nil, api.NewError(api.ErrCodeDatatypeMismatch, "could not determine type of constant array")
			}
		}
		if widenIntToDouble {
			switch n := lit.(type) {
			case int64:
				lit = float64(n)
			case int32:
				lit = float64(n)
			case int:
				lit = float64(n)
			}
		}
		if narrowToFloat32 {
			if f, ok := constAsFloat64(lit); ok && !math.IsNaN(f) {
				if f32 := float32(f); float64(f32) == f {
					lit = f32
				}
			}
		}
		if parseStringToUUID {
			if s, sok := lit.(string); sok {
				u, perr := uuid.Parse(s)
				if perr != nil {
					// Java verbatim wording (SemanticException INVALID_UUID_VALUE).
					return nil, fmt.Errorf("Invalid UUID value for the UUID type %s", s)
				}
				lit = [16]byte(u)
			}
		}
		list = append(list, lit)
	}
	return predicates.NewComparisonPredicate(left, predicates.Comparison{
		Type:    predicates.ComparisonIn,
		Operand: &values.ConstantValue{Value: list, Typ: values.TypeUnknown},
	}), nil
}

// ResolveAnd combines N predicates via Kleene AND. A single
// predicate returns verbatim (no wrapping); empty list returns
// ConstantPredicate(TRUE) — the AND identity.
func (r *Resolver) ResolveAnd(preds ...predicates.QueryPredicate) predicates.QueryPredicate {
	switch len(preds) {
	case 0:
		return predicates.NewConstantPredicate(predicates.TriTrue)
	case 1:
		return preds[0]
	}
	return predicates.NewAnd(preds...)
}

// ResolveOr combines N predicates via Kleene OR. Empty list returns
// ConstantPredicate(FALSE) — the OR identity. Single predicate
// returns verbatim.
func (r *Resolver) ResolveOr(preds ...predicates.QueryPredicate) predicates.QueryPredicate {
	switch len(preds) {
	case 0:
		return predicates.NewConstantPredicate(predicates.TriFalse)
	case 1:
		return preds[0]
	}
	return predicates.NewOr(preds...)
}

// ResolveNot wraps a predicate in a Kleene NOT. Nil child returns
// ConstantPredicate(UNKNOWN) — the only sensible interpretation.
func (r *Resolver) ResolveNot(pred predicates.QueryPredicate) predicates.QueryPredicate {
	if pred == nil {
		return predicates.NewConstantPredicate(predicates.TriUnknown)
	}
	return predicates.NewNot(pred)
}

// ResolveFunctionCall dispatches a function call against the given
// catalogue. For known aggregates (COUNT/SUM/MIN/MAX/AVG) it returns
// the corresponding values.AggregateValue. Scalar function support
// comes once the scalar-function catalogue is wired in.
//
// isStar=true signals the argument was `*` (COUNT(*)) — args must be
// empty in that case.
func (r *Resolver) ResolveFunctionCall(
	funcCatalog *semantic.FunctionCatalog,
	name semantic.Identifier,
	isStar bool,
	args []values.Value,
) (values.Value, error) {
	if funcCatalog == nil {
		return nil, fmt.Errorf("expr.ResolveFunctionCall: function catalog is nil")
	}
	spec, ok := funcCatalog.Lookup(name)
	if !ok {
		return nil, &semantic.FunctionNotFoundError{Name: name}
	}
	if isStar {
		if !spec.AllowsStar {
			return nil, fmt.Errorf("expr.ResolveFunctionCall: %s does not accept *", name)
		}
		if len(args) > 0 {
			return nil, fmt.Errorf("expr.ResolveFunctionCall: star form takes no args; got %d", len(args))
		}
	} else {
		if err := spec.ValidateArity(len(args)); err != nil {
			return nil, err
		}
	}
	if spec.Kind != semantic.FunctionAggregate {
		return nil, fmt.Errorf("expr.ResolveFunctionCall: scalar function %s not supported in seed", name)
	}
	// Aggregate dispatch — seed knows the five SQL standards.
	op, ok := aggregateOpForName(spec.Name, isStar)
	if !ok {
		return nil, fmt.Errorf("expr.ResolveFunctionCall: unknown aggregate %s", spec.Name)
	}
	if op == values.AggCountStar {
		return values.NewAggregateValue(op, nil), nil
	}
	return values.NewAggregateValue(op, args[0]), nil
}

// aggregateOpForName maps a normalized aggregate function name +
// star flag to the corresponding values.AggregateOp. Not exported
// — called via ResolveFunctionCall.
func aggregateOpForName(name string, isStar bool) (values.AggregateOp, bool) {
	switch name {
	case "COUNT":
		if isStar {
			return values.AggCountStar, true
		}
		return values.AggCount, true
	case "SUM":
		return values.AggSum, true
	case "MIN":
		return values.AggMin, true
	case "MAX":
		return values.AggMax, true
	case "AVG":
		return values.AggAvg, true
	}
	return values.AggInvalid, false
}

// sqlTypeToCascadesType maps the seed's string-valued SQL type
// (from semantic.Column.Type) to a cascades values.Type. Coarse —
// the seed maps INT/STRING/BOOL/ENUM to the matching primitive
// singletons; everything else falls through to UnknownType. Real
// type inference (proper nullability + structured-type recursion)
// is future work.
//
// INTEGER is a recognized SYNONYM for INT (the standard SQL spelling —
// the metadata-derivation paths in cascades_generator / system_rows
// already emit "INTEGER", so it must not silently fall to UNKNOWN
// here). Both bridge to the legacy nullable-LONG default (matching
// Java Record Layer's int64 representation), consistent with the
// existing INT→LONG width contract.
//
// "INT NOT NULL" / "INTEGER NOT NULL" map to the NON-NULL INT singleton
// (NotNullInt) — Java's Type.primitiveType(INT, false). This is the
// planner-internal spelling for a column known to be a non-null integer
// at construction time, notably the array-unnest WITH ORDINALITY ordinal
// (RFC-142): a 1-based, never-NULL INT whose result-set metadata must
// report INT, not UNKNOWN, and which matches the translator's ordinal
// FieldValue type (values.NotNullInt). Real catalog columns never carry
// this spelling, so the legacy INT→LONG bridge above is undisturbed.
func sqlTypeToCascadesType(sqlType string) values.Type {
	switch sqlType {
	case "INT", "INTEGER":
		// Genuine 32-bit INT (TypeCodeInt), NOT the width the retired
		// `TypeInt` alias carried (it WAS NullableLong): Java types INTEGER
		// columns INT and dispatches the int32-bounded arithmetic lane
		// (ADD_II = Math.addExact(int,int) — overflow at 2^31 errors
		// 22003 where the LONG lane silently returns the wide value).
		// Only the NOT NULL spelling carried the real code before —
		// a nullable INTEGER column silently lost its width.
		return values.NullableInt
	case "INT NOT NULL", "INTEGER NOT NULL":
		return values.NotNullInt
	case "BIGINT":
		return values.NullableLong
	case "BIGINT NOT NULL":
		return values.NotNullLong
	case "STRING", "ENUM":
		return values.TypeString
	case "UUID":
		// UUID is a first-class scalar (Java's DataType.Primitives.UUID),
		// stored as the tuple_fields.UUID message. Carrying the real UUID
		// type — not Unknown — lets the predicate builder promote a STRING
		// comparand (`uuid_col = '<uuid>'`) to UUID so the index-scan range
		// packs a tuple.UUID and the non-boolean predicate gate rejects a
		// bare `WHERE uuid_col`.
		return values.NullableUuid
	case "BOOL":
		return values.TypeBool
	case "FLOAT":
		// Genuine 32-bit FLOAT (TypeCodeFloat): Java computes FLOAT
		// arithmetic in float32 (ADD_FF — overflow saturates to ±Inf at
		// ~3.4e38 where the double lane returns the finite wide value).
		// The old "FLOAT, DOUBLE → NullableDouble" conflation erased the
		// width.
		return values.NullableFloat
	case "FLOAT NOT NULL":
		return values.NotNullFloat
	case "DOUBLE", "DOUBLE NOT NULL":
		// Carrying the real TypeCodeDouble — rather than Unknown — lets
		// the predicate-lift type gate reject a bare `WHERE <double_col>`
		// as non-boolean (42804) instead of silently lifting it to
		// `col = TRUE` (RFC-146).
		return values.NullableDouble
	case "BYTES":
		// Real TypeCodeBytes, same reason as FLOAT/DOUBLE.
		return values.NullableBytes
	case "RECORD":
		// No struct/record Type in the seed enum yet — stays Unknown.
		return values.TypeUnknown
	}
	return values.TypeUnknown
}

// columnCascadesType maps a resolved semantic.Column to its cascades
// values.Type, wrapping the element type in an ArrayType when the column
// is a SQL ARRAY (a repeated proto field). The placeholder Type string
// carries only the element kind; col.IsArray is the array signal. An
// array-typed resolved Value is what lets CARDINALITY()'s isArray() check
// pass and what the result-set metadata reports as an array. The array's
// nullability follows col.Nullable; the element type stays the (legacy)
// scalar mapping of the Type string.
func columnCascadesType(col semantic.Column) values.Type {
	elem := sqlTypeToCascadesType(col.Type)
	if !col.IsArray {
		// Honor the catalog's declared nullability (Java's
		// Type.primitiveType(typeCode, isNullable)): a NOT NULL column's
		// flowed type is non-nullable. Without this every resolver-produced
		// reference reads as nullable and the column-def derivation
		// (deriveProjectionColumnDef's flowed-type upgrade) wrongly reports
		// NOT NULL columns as nullable.
		if elem != nil && elem.Code() != values.TypeCodeUnknown && elem.IsNullable() != col.Nullable {
			elem = values.WithNullability(elem, col.Nullable)
		}
		return elem
	}
	return values.NewArrayType(col.Nullable, elem)
}

// ResolveConstant wraps a Go-native literal in a cascades
// ConstantValue with the appropriate type tag. Useful for inlining
// literal arguments when building a Value tree from a parsed
// expression.
//
// Returns an error when the literal's runtime type doesn't map to a
// known type — nil, int, int32, int64, float32, float64, string,
// bool, []byte are supported. Integer literals width-narrow like Java
// ParseHelpers.parseDecimal (in-int32-range → INT, else LONG); a
// float64 carrier is a DOUBLE literal and a float32 carrier a FLOAT
// one, so arithmetic over them rides ArithmeticValue's matching
// per-width lanes.
func (r *Resolver) ResolveConstant(lit any) (values.Value, error) {
	switch v := lit.(type) {
	case nil:
		return values.NewNullValue(values.TypeUnknown), nil
	case bool:
		return values.NewBooleanValue(v), nil
	case int:
		return &values.ConstantValue{Value: int64(v), Typ: intLiteralType(int64(v))}, nil
	case int32:
		return &values.ConstantValue{Value: int64(v), Typ: values.NullableInt}, nil
	case int64:
		return &values.ConstantValue{Value: v, Typ: intLiteralType(v)}, nil
	case string:
		return &values.ConstantValue{Value: v, Typ: values.TypeString}, nil
	case float32:
		// A float32 carrier is a genuine FLOAT literal (Java's 'f'-suffix
		// arm of ParseHelpers.parseDecimal returns Float).
		return &values.ConstantValue{Value: float64(v), Typ: values.NullableFloat}, nil
	case float64:
		return &values.ConstantValue{Value: v, Typ: values.NullableDouble}, nil
	case []byte:
		return &values.ConstantValue{Value: v, Typ: values.NullableBytes}, nil
	}
	return nil, fmt.Errorf("expr.ResolveConstant: unsupported literal type %T", lit)
}

// intLiteralType mirrors Java ParseHelpers.parseDecimal: an unsuffixed
// integer literal that fits in int32 is typed INT (Math.toIntExact →
// Integer), else LONG. This is what puts `int_col + 1` on the int32
// arithmetic lane (Java ADD_II) instead of silently widening. The
// carrier stays int64 either way; only the static width narrows.
func intLiteralType(v int64) values.Type {
	if v >= math.MinInt32 && v <= math.MaxInt32 {
		return values.NullableInt
	}
	return values.NullableLong
}
