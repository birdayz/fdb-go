package executor

import (
	"sync"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// EvaluationContext holds runtime bindings for plan execution:
// parameter values, correlation bindings (for correlated subqueries),
// and any mutable state that plan nodes share. Mirrors Java's
// EvaluationContext.
type EvaluationContext struct {
	bindings map[values.CorrelationIdentifier]any
}

// EmptyEvaluationContext returns a context with no bindings.
func EmptyEvaluationContext() *EvaluationContext {
	return &EvaluationContext{
		bindings: make(map[values.CorrelationIdentifier]any),
	}
}

// WithBinding returns a shallow copy with an additional binding.
func (ec *EvaluationContext) WithBinding(id values.CorrelationIdentifier, val any) *EvaluationContext {
	newBindings := make(map[values.CorrelationIdentifier]any, len(ec.bindings)+1)
	for k, v := range ec.bindings {
		newBindings[k] = v
	}
	newBindings[id] = val
	return &EvaluationContext{bindings: newBindings}
}

// GetBinding retrieves a correlation binding.
func (ec *EvaluationContext) GetBinding(id values.CorrelationIdentifier) (any, bool) {
	v, ok := ec.bindings[id]
	return v, ok
}

// GetOrCreateTempTable returns the TempTable at the given alias,
// creating one if it doesn't exist. Mirrors Java's pattern where
// TempTable is stored as a binding and lazily created.
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
