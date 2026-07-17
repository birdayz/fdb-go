package executor

import (
	"sync"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// EvaluationContext holds runtime bindings for plan execution:
// parameter values, correlation bindings (for correlated subqueries),
// scalar subquery results, and any mutable state that plan nodes
// share. Mirrors Java's EvaluationContext.
type EvaluationContext struct {
	bindings         map[values.CorrelationIdentifier]any
	params           []any
	scalarSubqueries map[values.CorrelationIdentifier]any
}

// EmptyEvaluationContext returns a context with no bindings.
func EmptyEvaluationContext() *EvaluationContext {
	return &EvaluationContext{
		bindings: make(map[values.CorrelationIdentifier]any),
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
	return &EvaluationContext{
		bindings:         newBindings,
		params:           params,
		scalarSubqueries: ec.scalarSubqueries,
	}
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
		ScalarSubqueries: ec.scalarSubqueries,
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
		ScalarSubqueries: ec.scalarSubqueries,
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
func frontierRowContext(pos values.OrdinalRow, ec *EvaluationContext, hasBindingCtx bool) any {
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
		}
		return rc
	}
	if hasBindingCtx && ec != nil {
		return ec.RowContextPositional(pos)
	}
	return pos
}

// hasBindingContext reports whether an eval context carries any resolvable
// binding beyond a bare row — a param, a pre-evaluated scalar subquery, or a
// correlation binding. It gates whether a positional row needs a wrapping
// RowContextPositional (to resolve an outer correlation) or can flow as the
// bare ordinal row.
func hasBindingContext(ec *EvaluationContext) bool {
	return ec != nil && (len(ec.params) > 0 || len(ec.scalarSubqueries) > 0 || len(ec.bindings) > 0)
}

// WithScalarSubqueries returns a copy with pre-evaluated scalar
// subquery results bound by correlation alias.
func (ec *EvaluationContext) WithScalarSubqueries(results map[values.CorrelationIdentifier]any) *EvaluationContext {
	newBindings := make(map[values.CorrelationIdentifier]any, len(ec.bindings))
	for k, v := range ec.bindings {
		newBindings[k] = v
	}
	return &EvaluationContext{
		bindings:         newBindings,
		params:           ec.params,
		scalarSubqueries: results,
	}
}

// WithBinding returns a shallow copy with an additional binding.
func (ec *EvaluationContext) WithBinding(id values.CorrelationIdentifier, val any) *EvaluationContext {
	newBindings := make(map[values.CorrelationIdentifier]any, len(ec.bindings)+1)
	for k, v := range ec.bindings {
		newBindings[k] = v
	}
	newBindings[id] = val
	return &EvaluationContext{bindings: newBindings, params: ec.params, scalarSubqueries: ec.scalarSubqueries}
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
	return v, ok
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

// GetList returns a snapshot of the temp table contents.
func (tt *TempTable) GetList() []QueryResult {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	out := make([]QueryResult, len(tt.list))
	copy(out, tt.list)
	return out
}

// Clear removes all entries from the temp table.
func (tt *TempTable) Clear() {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	tt.list = tt.list[:0]
}

// ReplaceList replaces the temp-table contents with rows that have ALREADY
// been charged against the statement memory budget — it does NOT re-charge.
// Used by the recursive-CTE DISTINCT path, which filters the rows the
// recursive plan already inserted (and charged via Add) down to the
// non-duplicate subset; re-charging them through Add would double-count the
// same resident rows. memUsed is monotonic, so the rows dropped by the filter
// stay charged (a conservative ceiling) — that is intentional and correct.
func (tt *TempTable) ReplaceList(rows []QueryResult) {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	tt.list = append(tt.list[:0], rows...)
}
