package executor

import (
	"fmt"
	"sync"
	"time"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// EvaluationContext holds runtime bindings for plan execution:
// parameter values, correlation bindings (for correlated subqueries),
// scalar subquery results, and any mutable state that plan nodes
// share. Mirrors Java's EvaluationContext.
type EvaluationContext struct {
	bindings map[values.CorrelationIdentifier]any
	// quantifiedBindings retains multiple exact objects that legitimately share
	// one correlation spelling in different physical phases (for example a
	// FlatMap's whole record and a scalar UNNEST element retained inside it).
	// Legacy bindings remain for parameters/older correlation consumers; exact
	// QOV evaluation selects here by both correlation and flowed type first.
	quantifiedBindings map[values.CorrelationIdentifier][]quantifiedRuntimeBinding
	params             []any
	scalarSubqueries   map[values.CorrelationIdentifier]any
	// mergedLegReadBypass names aliases whose binder-produced leg window
	// GetCorrelationBinding must DECLINE to serve, so the same query can be driven
	// down the alias's OTHER resolution route. See WithMergedLegReadBypass.
	//
	// It rides the CONTEXT rather than a package global on purpose: the suite is
	// parallel and several tests use the same table names, so a process-wide switch
	// would reach into a concurrently running test's execution. Every With* copy is
	// a struct copy, so it propagates down the whole execution the way the other
	// fields do — including through bindMergedOuterLegs' own derived context, which
	// is what makes it reach the read it is aimed at.
	mergedLegReadBypass map[string]bool
	// mergedLegReadSink, when non-nil, receives THIS execution's binder-window
	// reads alongside the process-global census. Rides the context for the same
	// reason the bypass does, and for a sharper one: the census is process-global
	// and the suite is parallel, so a before/after delta around one execution
	// silently includes a concurrent test's reads. The pin that has to count its
	// OWN reads cannot be built on that.
	mergedLegReadSink *MergedLegReadSink
	// mergedLegWrongWindows makes bindMergedOuterLegs aim every leg window of a
	// merged row at its SIBLING's span instead of its own — the standing form of
	// the by-hand "bind every leg WRONG" mutation. See WithMergedLegWrongWindows.
	mergedLegWrongWindows bool
	// statementTime is the statement-stable CURRENT_TIMESTAMP-family
	// instant, stamped ONCE at statement execution start (the SQL layer
	// stamps its session clock via WithStatementTime before running the
	// plan). SQL fixes CURRENT_TIMESTAMP / CURRENT_DATE per STATEMENT;
	// every row context derived from this context carries the stamp, so
	// per-row evaluation cannot drift. Zero means "not stamped" —
	// StatementNow then degrades to the wall clock.
	statementTime time.Time
	// scratch is the statement-scoped home for operator resume state that is
	// too large to ride every page's continuation (see ExecutionScratch). It
	// is a POINTER, so it survives the per-page struct copies the way the
	// statement's ExecuteState survives ExecuteProperties' copies. Nil means
	// "no scratch" and every operator falls back to a self-contained
	// continuation.
	scratch *ExecutionScratch
}

type quantifiedRuntimeBinding struct {
	qov            values.QuantifiedObjectValue
	value          any
	explicitAbsent bool
}

// WithExecutionScratch returns a copy carrying the statement's scratch. The
// statement's paging loop stamps this once and reuses it for every page, which
// is what lets an operator hand its live resume state to the next page instead
// of serializing it. Like every other With* copy, it rides all derived
// contexts.
func (ec *EvaluationContext) WithExecutionScratch(s *ExecutionScratch) *EvaluationContext {
	cp := *ec
	cp.scratch = s
	return &cp
}

// Scratch returns the statement's execution scratch, or nil when the execution
// carries none.
func (ec *EvaluationContext) Scratch() *ExecutionScratch {
	if ec == nil {
		return nil
	}
	return ec.scratch
}

// WithStatementTime returns a copy stamped with the statement-stable
// CURRENT_TIMESTAMP-family instant. Like every other With* copy, the
// stamp rides all derived contexts (WithParams, WithBinding, …).
func (ec *EvaluationContext) WithStatementTime(t time.Time) *EvaluationContext {
	cp := *ec
	cp.statementTime = t
	return &cp
}

// StatementNow implements values.StatementClock: the one instant every
// CURRENT_TIMESTAMP-family reference in this statement observes. An
// unstamped context degrades to the wall clock — that is the per-row
// drift the stamp exists to prevent, so statement entry points must
// stamp via WithStatementTime.
func (ec *EvaluationContext) StatementNow() time.Time {
	if ec == nil || ec.statementTime.IsZero() {
		return time.Now().UTC()
	}
	return ec.statementTime
}

// WithMergedLegReadBypass returns a copy in which lookups of the named aliases
// DECLINE any window bindMergedOuterLegs produced, falling back to whatever that
// window displaced (nothing, for a window that displaced nothing).
//
// It exists for one caller: the redundancy pin, which has to run the SAME query
// down BOTH resolution routes and compare. A build-tagged neuter cannot do that
// job — it removes the binder from the whole binary, so the two routes never
// coexist in one run, and their agreement is the entire content of the pin.
//
// Honoured only while the leg-identity census gate is on (the read site's gate),
// so production never consults it.
func (ec *EvaluationContext) WithMergedLegReadBypass(aliases ...string) *EvaluationContext {
	cp := *ec
	cp.mergedLegReadBypass = make(map[string]bool, len(aliases))
	for _, a := range aliases {
		cp.mergedLegReadBypass[a] = true
	}
	return &cp
}

// WithMergedLegWrongWindows returns a copy in which bindMergedOuterLegs aims each
// leg window of a merged row at its SIBLING's span rather than its own, so every
// binding a reader can resolve through is DELIBERATELY WRONG.
//
// It exists so the mutation that licenses "the merged-leg bindings are not
// load-bearing" can STAND rather than be re-run by hand. That claim rested on
// someone editing the binder to misaim it and watching the suite stay green;
// nothing performed it in CI, so the day a read starts depending on which slots
// its window covers, no test went red and a green census read as "the bindings
// are correct" when it only ever meant "nobody looked".
//
// It rides EvaluationContext for the same two reasons WithMergedLegReadBypass
// does: the suite is parallel and several tests share these table names, so a
// process-wide switch would misaim a concurrently running test's execution; and
// an edited-out or build-tagged misaim cannot run the correct and the wrong
// window in ONE process, which is the entire comparison.
//
// The perturbation is a ROTATION onto the sibling's span, not a constant offset,
// because a constant is not reliably wrong: the first leg of a merged row already
// starts at 0, so "point everything at slot 0" leaves it aimed correctly. Rotation
// moves every window of a multi-leg row whose legs are not all identically shaped,
// and the windows it did move are the ones counted — an instrument that cannot
// state it perturbed anything proves nothing.
//
// Honoured only while the leg-identity census gate is on, like the rest of this
// instrumentation, so production never misaims.
func (ec *EvaluationContext) WithMergedLegWrongWindows() *EvaluationContext {
	cp := *ec
	cp.mergedLegWrongWindows = true
	return &cp
}

// WithMergedLegReadSink returns a copy whose binder-window reads and declined
// lookups are ALSO recorded into sink, scoped to this execution.
//
// Honoured only while the leg-identity census gate is on, like the rest of this
// instrumentation.
func (ec *EvaluationContext) WithMergedLegReadSink(sink *MergedLegReadSink) *EvaluationContext {
	cp := *ec
	cp.mergedLegReadSink = sink
	return &cp
}

// EmptyEvaluationContext returns a context with no bindings.
func EmptyEvaluationContext() *EvaluationContext {
	return &EvaluationContext{
		bindings:           make(map[values.CorrelationIdentifier]any),
		quantifiedBindings: make(map[values.CorrelationIdentifier][]quantifiedRuntimeBinding),
	}
}

// WithParams returns a copy with prepared-statement parameter bindings.
// Params is 0-indexed; ParameterValue ordinals are 1-based. The copy CARRIES
// scalarSubqueries like every other With* copy — dropping them would make
// binding ORDER load-bearing (WithScalarSubqueries().WithParams() would
// silently unbind the subqueries, and unbound is a loud
// *values.UnboundScalarSubqueryError at row time).
func (ec *EvaluationContext) WithParams(params []any) *EvaluationContext {
	newBindings := make(map[values.CorrelationIdentifier]any, len(ec.bindings))
	for k, v := range ec.bindings {
		newBindings[k] = v
	}
	cp := *ec
	cp.bindings = newBindings
	cp.params = params
	return &cp
}

// BindParameter implements values.ParameterBinder. Ordinal is 1-based;
// named parameters are not yet supported.
func (ec *EvaluationContext) BindParameter(ordinal int, name string) (any, bool) {
	if ordinal >= 1 && ordinal <= len(ec.params) {
		return ec.params[ordinal-1], true
	}
	return nil, false
}

// RowContext returns a binding-only RowEvalContext — this context's parameter
// bindings, correlation bindings, and scalar subquery results, with NO frontier
// row. Used when evaluating expressions that reference only params / correlations
// / scalar subqueries; a row-bearing context flows through RowContextPositional.
func (ec *EvaluationContext) RowContext() *values.RowEvalContext {
	return &values.RowEvalContext{
		Binder:           ec,
		Correlations:     ec,
		Objects:          &evaluationObjectBinder{base: ec},
		ScalarSubqueries: ec.scalarSubqueries,
		Clock:            ec,
	}
}

// RowContextPositional returns a RowEvalContext whose authoritative row is the
// ordinal-model positional row (resolved by ordinal, no name-map fallback),
// combined with this context's parameter bindings, correlation
// bindings, and scalar subquery results. Use it on the non-join frontier when a
// param / scalar subquery / outer correlation is in play; when none is, flow the
// bare OrdinalRow directly. An outer correlation resolves via Correlations first;
// only the (unbound) frontier quantifier reference falls to the positional row.
func (ec *EvaluationContext) RowContextPositional(pos values.OrdinalRow) *values.RowEvalContext {
	return &values.RowEvalContext{
		Positional:       pos,
		Binder:           ec,
		Correlations:     ec,
		Objects:          &evaluationObjectBinder{base: ec},
		ScalarSubqueries: ec.scalarSubqueries,
		Clock:            ec,
	}
}

// frontierRowContext returns the eval context a row on the non-join ordinal
// frontier is resolved against: the bare
// positional row (FieldValue resolves by ordinal, loud on a miss — NO name-map
// fallback) when no param / scalar-subquery / outer correlation binding
// is in play, else a RowContextPositional so an outer correlation resolves via
// the binder BEFORE the frontier quantifier falls to the positional row. Shared
// by executeProjection / executeFilter / executePredicatesFilter / executeMap so
// the frontier dispatch is identical across them. hasBindingCtx is
// params||scalarSubqueries||bindings for the caller's evalCtx.
func frontierRowContext(
	pos values.OrdinalRow,
	ec *EvaluationContext,
	hasBindingCtx bool,
	edges ...values.QuantifiedObjectValue,
) (any, error) {
	// An admitted physical row never uses the ambient positional fallback.
	// Its exact layout is the sole authority for current/source QOV bindings;
	// an omitted source therefore remains loudly unbound instead of reading a
	// same-shaped slot from another quantifier.
	if pr, isPR := pos.(*PositionalRow); isPR && pr != nil && pr.Layout != nil {
		binding := &frontierRowBinding{}
		base := binding.initContext(pr, ec)
		objects, err := initEdgeObjectBinder(&binding.edges, pr, base.Objects, edges...)
		if err != nil {
			return nil, err
		}
		base.Objects = objects
		return ordinalLayoutRowContext(&binding.windows, pr.Layout, pr, pr.LayoutPresence, base)
	}
	// A row whose TYPE carries leg boundaries (a merged concat /
	// clustered box row) serves its legs as correlation-bound windows, so a
	// quantifier-addressed source-relative baked reference (QOV(leg).col with the
	// source's declared-column ordinal) resolves positionally within its leg —
	// Java's quantifier binding. The leg binder chains to the eval context for
	// outer correlations.
	if pr, isPR := pos.(*PositionalRow); isPR && pr != nil && pr.Type != nil && len(pr.Type.Legs) > 0 {
		rc := &values.RowEvalContext{
			Positional:   pos,
			Correlations: &rowLegsBinder{base: correlationBase(ec), row: pr},
		}
		if ec != nil {
			rc.Binder = ec
			rc.ScalarSubqueries = ec.scalarSubqueries
			rc.Clock = ec
		}
		return rc, nil
	}
	if len(edges) > 0 {
		pr, ok := pos.(*PositionalRow)
		if !ok || pr == nil {
			return nil, layoutBindingError(values.LayoutRuntimeShape, "quantifier edge requires an executor positional row")
		}
		result := rowEvalContextForPositional(pr, ec)
		objects, err := newEdgeObjectBinder(pr, result.Objects, edges...)
		if err != nil {
			return nil, err
		}
		result.Objects = objects
		return result, nil
	}
	if hasBindingCtx && ec != nil {
		return ec.RowContextPositional(pos), nil
	}
	return pos, nil
}

// positionalRowContext holds a row context together with the QOV adapter that
// context points at, so building one costs ONE allocation instead of two.
//
// The adapter is an 8-byte wrapper whose whole content is the context it adapts,
// and this is built once per ROW: separately allocated, it was 1.1 allocations
// per row on a wide scan. It cannot instead be memoized on the EvaluationContext
// because every With* derivation is a struct copy — a cached adapter would ride
// that copy still pointing at the ORIGINAL context, and serve bindings that the
// derivation replaced. Co-allocating needs no such invariant: the adapter is
// reachable only from the context it was built for, and dies with it.
type positionalRowContext struct {
	ctx    values.RowEvalContext
	binder evaluationObjectBinder
}

// frontierRowBinding is the whole per-row chain of an admitted physical row's
// evaluation context in ONE allocation: the context itself, the outer-correlation
// adapter it starts from, the edge binder layered on that, and room for the
// layout binder values materializes on top.
//
// The chain is four objects that are born together, point only at each other,
// and die together at the end of the row — so allocating them separately bought
// nothing and cost three allocations per row where one does. Together with the
// layout stamp at mint time and the unboxed cursor results, this is what brought
// a 20k-row wide scan's allocation count back to master's.
//
// It is deliberately NOT reused across rows. Reuse would be measurably cheaper
// still and would rest on nothing enforcing that no Value retains the context it
// was evaluated against; the failure mode of that invariant breaking is a row
// silently evaluated against its successor's data, which no test shape reliably
// catches. One allocation per row is the version that needs no such promise.
type frontierRowBinding struct {
	ctx     values.RowEvalContext
	outer   evaluationObjectBinder
	edges   edgeObjectBinder
	windows values.OrdinalBinderStorage
}

// initContext fills the binding's context and outer adapter for pos, returning
// the context the rest of the chain layers onto. It is rowEvalContextForPositional
// against storage the caller already owns.
func (b *frontierRowBinding) initContext(
	pos values.OrdinalRow,
	ec *EvaluationContext,
) *values.RowEvalContext {
	b.ctx.Positional = pos
	if ec == nil {
		return &b.ctx
	}
	b.outer.base = ec
	b.ctx.Binder = ec
	b.ctx.Correlations = ec
	b.ctx.Objects = &b.outer
	b.ctx.ScalarSubqueries = ec.scalarSubqueries
	b.ctx.Clock = ec
	return &b.ctx
}

// rowEvalContextForPositional preserves every evaluation capability while
// adapting legacy correlation bindings into the exact-QOV namespace used as
// the base of an OrdinalLayout binder.
func rowEvalContextForPositional(pos values.OrdinalRow, ec *EvaluationContext) *values.RowEvalContext {
	holder := &positionalRowContext{ctx: values.RowEvalContext{Positional: pos}}
	if ec == nil {
		return &holder.ctx
	}
	holder.binder.base = ec
	holder.ctx.Binder = ec
	holder.ctx.Correlations = ec
	holder.ctx.Objects = &holder.binder
	holder.ctx.ScalarSubqueries = ec.scalarSubqueries
	holder.ctx.Clock = ec
	return &holder.ctx
}

// valuesDependOnStatementClock reports whether ANY of the value trees
// contains a CURRENT_TIMESTAMP-family function (values.DependsOnStatementClock).
// Operators compute this ONCE alongside hasBindingContext: a clock-needing
// value makes the frontier wrap the bare row so the statement-stable
// instant is in reach.
func valuesDependOnStatementClock(vs []values.Value) bool {
	for _, v := range vs {
		if values.DependsOnStatementClock(v) {
			return true
		}
	}
	return false
}

// predicatesDependOnStatementClock is valuesDependOnStatementClock over
// the value trees embedded in predicates (the shared rewrite spine —
// predicates.DependsOnStatementClock).
func predicatesDependOnStatementClock(ps []predicates.QueryPredicate) bool {
	for _, p := range ps {
		if predicates.DependsOnStatementClock(p) {
			return true
		}
	}
	return false
}

// aggregateOperandsDependOnStatementClock is valuesDependOnStatementClock
// over the operand value trees of aggregate specs.
func aggregateOperandsDependOnStatementClock(specs []expressions.AggregateSpec) bool {
	for _, s := range specs {
		if values.DependsOnStatementClock(s.Operand) {
			return true
		}
	}
	return false
}

// hasBindingContext reports whether an eval context carries any resolvable
// binding beyond a bare row — a param, a pre-evaluated scalar subquery, or a
// correlation binding. It gates whether a positional row needs a wrapping
// RowContextPositional (to resolve an outer correlation) or can flow as the
// bare ordinal row.
func hasBindingContext(ec *EvaluationContext) bool {
	return ec != nil && (len(ec.params) > 0 || len(ec.scalarSubqueries) > 0 ||
		len(ec.bindings) > 0 || len(ec.quantifiedBindings) > 0)
}

// WithScalarSubqueries returns a copy with pre-evaluated scalar
// subquery results bound by correlation alias.
func (ec *EvaluationContext) WithScalarSubqueries(results map[values.CorrelationIdentifier]any) *EvaluationContext {
	newBindings := make(map[values.CorrelationIdentifier]any, len(ec.bindings))
	for k, v := range ec.bindings {
		newBindings[k] = v
	}
	cp := *ec
	cp.bindings = newBindings
	cp.scalarSubqueries = results
	return &cp
}

// WithBinding returns a shallow copy with an additional binding.
func (ec *EvaluationContext) WithBinding(id values.CorrelationIdentifier, val any) *EvaluationContext {
	newBindings := make(map[values.CorrelationIdentifier]any, len(ec.bindings)+1)
	for k, v := range ec.bindings {
		newBindings[k] = v
	}
	newBindings[id] = val
	cp := *ec
	cp.bindings = newBindings
	return &cp
}

// withQuantifiedBinding is the executor-internal explicit-absence form. The
// true callers copy a positive proof from an exact OrdinalLayout binder or
// PositionalRow.wholeObjectBinding (FirstOrDefault's typed empty shell); false
// callers install an ordinary exact object. This keeps an unmatched
// non-nullable physical object distinct from both an arbitrary invalid nil and
// a matched all-NULL record.
func (ec *EvaluationContext) withQuantifiedBinding(
	qov values.QuantifiedObjectValue,
	val any,
	explicitAbsent bool,
) (*EvaluationContext, error) {
	exact, ok := values.AsQuantifiedObjectValue(qov)
	if !ok || exact == nil {
		return nil, layoutBindingError(values.CorrelationForeignValue, "quantified binding is not exact")
	}
	if explicitAbsent && val != nil {
		return nil, layoutBindingError(values.LayoutRuntimeShape, "explicitly absent quantified binding is non-nil")
	}
	if val == nil && !explicitAbsent && !exact.FlowedType().IsNullable() {
		return nil, layoutBindingError(values.LayoutNullabilityMismatch, "non-nullable quantified binding is SQL NULL")
	}
	bindings := make(map[values.CorrelationIdentifier][]quantifiedRuntimeBinding, len(ec.quantifiedBindings)+1)
	for correlation, existing := range ec.quantifiedBindings {
		bindings[correlation] = append([]quantifiedRuntimeBinding(nil), existing...)
	}
	correlation := exact.Correlation()
	entries := bindings[correlation]
	replaced := false
	for i := range entries {
		if values.FlowedTypesEqual(entries[i].qov, exact) {
			entries[i] = quantifiedRuntimeBinding{qov: exact, value: val, explicitAbsent: explicitAbsent}
			replaced = true
			break
		}
	}
	if !replaced {
		entries = append(entries, quantifiedRuntimeBinding{qov: exact, value: val, explicitAbsent: explicitAbsent})
	}
	bindings[correlation] = entries
	cp := *ec
	cp.quantifiedBindings = bindings
	return &cp, nil
}

// cloneBindings copies this context's bindings into a fresh map sized for extra
// additional entries, for a caller that is about to add SEVERAL bindings at once.
//
// WithBinding copies the whole map per call, so binding K correlations through
// it copies every existing binding K times — a cost that scales with the
// ENCLOSING context's size, not with what is being added. A caller that knows
// its K up front clones once and inserts K times instead. The returned map is
// the caller's alone until it is handed to withBindings; nothing else may hold a
// reference to it, or the copy-on-write contract every other With* method relies
// on is broken.
func (ec *EvaluationContext) cloneBindings(extra int) map[values.CorrelationIdentifier]any {
	out := make(map[values.CorrelationIdentifier]any, len(ec.bindings)+extra)
	for k, v := range ec.bindings {
		out[k] = v
	}
	return out
}

// withBindings returns a shallow copy carrying bindings, which MUST be a map the
// caller built via cloneBindings on this same context and has not shared. It is
// the multi-insert twin of WithBinding; the single-binding case keeps using
// WithBinding, whose one-copy-one-insert is already minimal.
func (ec *EvaluationContext) withBindings(bindings map[values.CorrelationIdentifier]any) *EvaluationContext {
	cp := *ec
	cp.bindings = bindings
	return &cp
}

// GetBinding retrieves a correlation binding.
func (ec *EvaluationContext) GetBinding(id values.CorrelationIdentifier) (any, bool) {
	v, ok := ec.bindings[id]
	return v, ok
}

// GetCorrelationBinding implements values.CorrelationBinder so that
// QuantifiedObjectValue can resolve correlated rows during scan
// comparison evaluation in the FlatMap execution path.
func (ec *EvaluationContext) GetCorrelationBinding(id values.CorrelationIdentifier) (any, bool) {
	v, ok := ec.bindings[id]
	if ok && values.LegIdentityCensusEnabled() {
		// Count a lookup that resolved to a merged-leg window. This is the READ
		// half of the merged-leg binding census: the binder's cost is per outer
		// row, and whether that cost buys anything is decided HERE, at the only
		// place a correlation binding is consulted. Gated because this is the
		// per-reference-per-row path.
		if w, isWindow := v.(*legWindowRow); isWindow && w != nil && w.fromMergedBinder {
			// The redundancy pin's bypass: decline the binder's window and hand back
			// what it shadowed, so the same query can be driven down the alias's
			// OTHER resolution route in the same run. A bypassed lookup is not a
			// read of a binder window, so it is not counted as one.
			//
			// On the corpus's reader shape there is nothing to hand back: every
			// read there is of a window that shadowed NOTHING, so the bypass is a
			// MISS — the alias resolves to no binding at all. That is the stronger
			// of the two things this can do, and it is what the redundancy pin
			// asserts it got, rather than assuming which one it took.
			if ec.mergedLegReadBypass[id.Name()] {
				ec.mergedLegReadSink.recordBypass(id.Name(), w.shadowsExisting, w.parentType)
				return w.shadowed, w.shadowsExisting
			}
			// siblingLegs and shadowsExisting are stamped by the binder, which is
			// the only site that holds the row's leg COUNT and the incoming
			// context's prior binding; a reader has one window and cannot tell a
			// lone leg from one of several, and the aliases it could ask about
			// collide across queries. parentType is the merged row's type, which
			// the window already carries — it is what the read gets keyed by, so
			// one query's shape is not attributed to another query's alias.
			recordMergedLegRead(id.Name(), w.siblingLegs, w.shadowsExisting, w.parentType)
			// misaimed is stamped by the binder too, and it is recorded HERE rather
			// than at the bind site on purpose: the claim the wrong-window instrument
			// has to make is that a misaimed window was READ, not merely that one was
			// produced. A row whose legs nothing ever looks up can be misaimed all day
			// and prove nothing.
			ec.mergedLegReadSink.recordRead(id.Name(), w.siblingLegs, w.misaimed, w.parentType)
		}
	}
	return v, ok
}

// GetQuantifiedBinding implements the exact QOV binder. When this context has
// exact declarations for a correlation, the requested flowed type must select
// one of them; a same-spelled foreign type cannot fall back to the legacy map.
func (ec *EvaluationContext) GetQuantifiedBinding(view values.QuantifiedObjectValue) (any, bool, error) {
	exact, ok := values.AsQuantifiedObjectValue(view)
	if !ok || exact == nil {
		return nil, false, layoutBindingError(values.CorrelationForeignValue, "quantified lookup is not exact")
	}
	entries := ec.quantifiedBindings[exact.Correlation()]
	for _, entry := range entries {
		if values.FlowedRowShapesAgree(entry.qov, exact) {
			return entry.value, true, nil
		}
	}
	if len(entries) > 0 {
		return nil, false, layoutBindingError(values.CorrelationTypeConflict,
			fmt.Sprintf("quantified lookup %s: read as %s, declared %s",
				exact.Correlation().Name(), values.DescribeType(exact.FlowedType()),
				describeDeclaredBindings(entries)))
	}
	value, present := ec.bindings[exact.Correlation()]
	return value, present, nil
}

// IsExplicitNullQuantifiedBinding implements the positive absence-proof
// channel consumed by values' strict ordinal binder. Exact type validation is
// identical to GetQuantifiedBinding, so a same-spelled foreign declaration
// cannot borrow another object's FirstOrDefault absence.
func (ec *EvaluationContext) IsExplicitNullQuantifiedBinding(view values.QuantifiedObjectValue) (bool, error) {
	exact, ok := values.AsQuantifiedObjectValue(view)
	if !ok || exact == nil {
		return false, layoutBindingError(values.CorrelationForeignValue, "quantified absence lookup is not exact")
	}
	entries := ec.quantifiedBindings[exact.Correlation()]
	for _, entry := range entries {
		if values.FlowedRowShapesAgree(entry.qov, exact) {
			return entry.explicitAbsent, nil
		}
	}
	if len(entries) > 0 {
		return false, layoutBindingError(values.CorrelationTypeConflict,
			fmt.Sprintf("quantified absence lookup %s: read as %s, declared %s",
				exact.Correlation().Name(), values.DescribeType(exact.FlowedType()),
				describeDeclaredBindings(entries)))
	}
	return false, nil
}

// GetOrCreateTempTable returns the TempTable at the given alias,
// creating one if it doesn't exist. Mutates ec.bindings directly
// (intentional — temp tables are shared mutable state across the
// execution, not copy-on-write like WithBinding). Callers must
// ensure this is called on the root context, not on a WithBinding copy.
//
// st is the statement's ExecuteState (RFC-130) charged when a temp table is
// freshly created here; an already-bound temp table keeps its original state
// (it was minted with the same statement's state). Callers pass props.State.
func (ec *EvaluationContext) GetOrCreateTempTable(id values.CorrelationIdentifier, st *recordlayer.ExecuteState) *TempTable {
	if v, ok := ec.bindings[id]; ok {
		if tt, ok := v.(*TempTable); ok {
			return tt
		}
	}
	tt := NewTempTableWithState(st)
	ec.bindings[id] = tt
	return tt
}

// TempTable is an in-memory list of QueryResult used by
// TempTableInsertPlan and TempTableScanPlan. Mirrors Java's
// com.apple.foundationdb.record.TempTable.
//
// RFC-130: a TempTable is a cardinality-growing buffer — the recursive-CTE
// per-level working set (ping-ponged scan/insert tables) and the
// TempTableInsertPlan target both accumulate into it, separate from the
// CollectAllBounded per-level materialization. It carries the statement's
// always-present *ExecuteState (st) and charges each appended row's byte
// estimate in Add. The pre-existing sync.Mutex is defensive (the zero-
// goroutine executor invariant makes it currently moot); charging under the
// lock is correct regardless — if the executor ever goes concurrent the pinned
// package_invariant_test fires and ChargeMemory moves to atomic.
type TempTable struct {
	mu   sync.Mutex
	list []QueryResult
	st   *recordlayer.ExecuteState
	// charged: total bytes charged against st over this table's lifetime
	// (monotonic — Clear/ReplaceList do not refund, matching the
	// accumulate-within-a-page model). The table's OWNER releases it once
	// at teardown via ReleaseCharges (live-bytes model): recursion owns its
	// ping-pong tables; a table created by a TempTableInsert cursor is
	// released by that cursor's Close.
	charged int64
}

// NewTempTable creates an empty temp table with no memory budget. Used by
// internal call sites that have no statement ExecuteState in scope (and by
// tests); production statement paths use NewTempTableWithState so the
// statement-wide memory budget covers the temp-table working set.
func NewTempTable() *TempTable {
	return &TempTable{}
}

// NewTempTableWithState creates an empty temp table that charges its rows
// against the supplied statement ExecuteState (RFC-130). st is the always-
// present statement state; a nil/zero-limit st makes the charge a no-op.
func NewTempTableWithState(st *recordlayer.ExecuteState) *TempTable {
	return &TempTable{st: st}
}

// Add appends a QueryResult to the temp table, charging its byte estimate
// against the statement memory budget first (RFC-130). On a budget breach the
// row is NOT appended and the *MemoryLimitExceededError is returned.
func (tt *TempTable) Add(qr QueryResult) error {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if tt.st.HasMemLimit() {
		n := estimateQueryResultBytes(qr)
		if err := tt.st.ChargeMemory(n); err != nil {
			return err
		}
		tt.charged += n
	}
	tt.list = append(tt.list, qr)
	return nil
}

// ReleaseAllTempTableCharges releases every temp table bound in THIS context's
// map back to the statement budget — the teardown half of the recursion's
// live-bytes accounting (the DFS accumulator is minted into the shared
// bindings map by GetOrCreateTempTable, so the recursion cannot name it
// directly). Idempotent per table (the tally zeroes). A nested recursion
// sharing the map would release an outer recursion's still-live tables a step
// early — an accepted under-account bounded by the outer working set; the
// outer teardown's release is then a harmless no-op.
func (ec *EvaluationContext) ReleaseAllTempTableCharges() {
	for _, v := range ec.bindings {
		if tt, ok := v.(*TempTable); ok {
			tt.ReleaseCharges()
		}
	}
}

// ReleaseCharges returns every byte this table has charged to the statement
// budget and zeroes the tally. Called exactly once by the table's owner at
// teardown; idempotent because the tally is zeroed.
func (tt *TempTable) ReleaseCharges() {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if tt.charged > 0 {
		tt.st.ReleaseMemory(tt.charged)
		tt.charged = 0
	}
}

// GetList returns a COPY of the temp table contents (safe to hold across any
// later mutation, at the cost of an O(len) copy on every call). Prefer
// Snapshot for a read that will be held across an unbounded number of future
// Add calls without needing that copy.
func (tt *TempTable) GetList() []QueryResult {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	out := make([]QueryResult, len(tt.list))
	copy(out, tt.list)
	return out
}

// Snapshot returns the table's current row slice with NO copy — O(1), just
// the three-word slice header. This is safe to hold indefinitely (across any
// number of subsequent Add calls, in particular across a lazily-serialized
// continuation that may never be read, or read long after further rows were
// added) because of an invariant the rest of this type maintains: Add only
// ever appends past the length a Snapshot already captured (so old data a
// Snapshot exposes is never overwritten by later growth), and Clear/
// ReplaceList always hand tt.list a FRESH backing array rather than
// truncating the existing one in place — so nothing this table does after a
// Snapshot was taken can ever become visible through it. Same principle as
// mergeSortCursor's per-child continuation snapshot (executor_new_plans.go):
// a held reference is safe because the type never mutates what it already
// exposed, only replaces it wholesale.
//
// That safety is one-directional: it covers what THIS TYPE does to the
// backing array, not what a careless caller could do to it. The returned
// slice aliases tt's own memory — never write through it (no element
// assignment, no append() within its existing capacity: both land exactly
// where Add's own next append would write, corrupting rows Add hasn't
// produced yet). Treat the result as read-only. Call GetList instead if you
// need a slice you're allowed to mutate.
func (tt *TempTable) Snapshot() []QueryResult {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	return tt.list
}

// Clear removes all entries from the temp table. Allocates a fresh backing
// array (tt.list = nil, not tt.list[:0]) rather than truncating in place —
// see Snapshot's doc comment for why: reusing the old array would let a
// still-unread Snapshot silently see whatever level's rows get Add-ed next.
func (tt *TempTable) Clear() {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	tt.list = nil
}

// ReplaceList replaces the temp-table contents with rows that have ALREADY
// been charged against the statement memory budget — it does NOT re-charge.
// Used by the recursive-CTE DISTINCT path, which filters the rows the
// recursive plan already inserted (and charged via Add) down to the
// non-duplicate subset; re-charging them through Add would double-count the
// same resident rows. memUsed is monotonic, so the rows dropped by the filter
// stay charged (a conservative ceiling) — that is intentional and correct.
// Allocates a fresh backing array for the same reason as Clear (see
// Snapshot): appending rows[:0] would let a still-unread Snapshot alias this
// replacement's writes.
func (tt *TempTable) ReplaceList(rows []QueryResult) {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	tt.list = append([]QueryResult(nil), rows...)
}

// describeDeclaredBindings renders the exact shapes a correlation IS bound to,
// so a type conflict reports both sides. A conflict whose message names neither
// the correlation nor the two shapes forces every occurrence to be
// re-investigated from scratch, and these arrive in clusters.
func describeDeclaredBindings(entries []quantifiedRuntimeBinding) string {
	if len(entries) == 0 {
		return "nothing"
	}
	out := ""
	for i, entry := range entries {
		if i > 0 {
			out += " | "
		}
		out += values.DescribeType(entry.qov.FlowedType())
	}
	return out
}
