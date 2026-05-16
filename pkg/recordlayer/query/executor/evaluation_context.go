package executor

import (
	"sync"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
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
// Params is 0-indexed; ParameterValue ordinals are 1-based.
func (ec *EvaluationContext) WithParams(params []any) *EvaluationContext {
	newBindings := make(map[values.CorrelationIdentifier]any, len(ec.bindings))
	for k, v := range ec.bindings {
		newBindings[k] = v
	}
	return &EvaluationContext{bindings: newBindings, params: params}
}

// BindParameter implements values.ParameterBinder. Ordinal is 1-based;
// named parameters are not yet supported.
func (ec *EvaluationContext) BindParameter(ordinal int, name string) (any, bool) {
	if ordinal >= 1 && ordinal <= len(ec.params) {
		return ec.params[ordinal-1], true
	}
	return nil, false
}

// RowContext returns a RowEvalContext combining a datum map with this
// context's parameter bindings and scalar subquery results. Used when
// evaluating expressions that mix field references, prepared-statement
// parameters, and scalar subquery references.
func (ec *EvaluationContext) RowContext(datum map[string]any) *values.RowEvalContext {
	return &values.RowEvalContext{
		Datum:            datum,
		Binder:           ec,
		ScalarSubqueries: ec.scalarSubqueries,
	}
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
	return &EvaluationContext{bindings: newBindings, params: ec.params}
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
func (ec *EvaluationContext) GetOrCreateTempTable(id values.CorrelationIdentifier) *TempTable {
	if v, ok := ec.bindings[id]; ok {
		if tt, ok := v.(*TempTable); ok {
			return tt
		}
	}
	tt := NewTempTable()
	ec.bindings[id] = tt
	return tt
}

// TempTable is an in-memory list of QueryResult used by
// TempTableInsertPlan and TempTableScanPlan. Mirrors Java's
// com.apple.foundationdb.record.TempTable.
type TempTable struct {
	mu   sync.Mutex
	list []QueryResult
}

// NewTempTable creates an empty temp table.
func NewTempTable() *TempTable {
	return &TempTable{}
}

// Add appends a QueryResult to the temp table.
func (tt *TempTable) Add(qr QueryResult) {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	tt.list = append(tt.list, qr)
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
