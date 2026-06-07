package values

// RankValue is the SQL RANK() window function — assigns 1-based
// rank within each partition, sharing rank across ORDER BY ties
// (and skipping ranks accordingly: 1, 1, 3, 4, 4, 6).
// Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.RankValue`.
//
// RANK has NO operand arguments — Java's grammar is
// `RANK() OVER (PARTITION BY ... ORDER BY ...)`; the windowing
// happens via the surrounding window definition. ArgumentValues is
// always empty (Java's constructor takes them but RANK's BuiltIn
// passes empty argumentValues at parse time).
//
// Result type: NotNullLong. RANK over an empty partition still
// produces 1 for the first row; never NULL.
//
// Eval: window-aware. The seed accepts a row-shape evalCtx of
//
//	map[string]any{"_rank": int64(N)}
//
// — the test harness pattern. Real execution wires a
// streaming-window accumulator that increments rank counters per
// partition; the seed exposes the current rank via this side
// channel so window-tagged Values are testable without the full
// streaming framework.
type RankValue struct {
	WindowedValue
}

// NewRankValue constructs a RANK() value with the given partitioning
// columns. Java's constructor takes (partitioning, argument) — RANK
// passes empty argumentValues. We expose only the partitioning
// parameter to match RANK()'s actual SQL surface.
func NewRankValue(partitioningValues []Value) *RankValue {
	return &RankValue{
		WindowedValue: WindowedValue{
			PartitioningValues: append([]Value(nil), partitioningValues...),
			ArgumentValues:     nil,
		},
	}
}

// Name returns the SQL function name.
func (*RankValue) Name() string { return "RANK" }

// Type returns NotNullLong — RANK is always populated, 1-based.
func (*RankValue) Type() Type { return NotNullLong }

// Evaluate returns the current rank from the row-shape harness
// pattern. The harness supplies the window-accumulator's current
// rank via the `_rank` key; in a real execution the rank is
// computed by the streaming window operator.
//
// Returns nil if evalCtx is nil / non-map / has no `_rank` key —
// matches the placeholder-Value pattern used elsewhere in the seed.
func (*RankValue) Evaluate(evalCtx any) (any, error) {
	if evalCtx == nil {
		return nil, nil
	}
	if m, ok := evalCtx.(map[string]any); ok {
		if r, ok := m["_rank"]; ok {
			return r, nil
		}
	}
	return nil, nil
}

// WithChildren returns a new RankValue with the given children
// re-split via WindowedValue.SplitNewChildren. Java's withChildren
// reconstructs the partition+argument lists by position.
//
// Per Java's RANK semantics, argument values should remain empty
// after withChildren — the partitioning columns are the only
// children RANK actually carries.
func (r *RankValue) WithChildren(newChildren []Value) *RankValue {
	partition, _ := r.SplitNewChildren(newChildren)
	return NewRankValue(partition)
}
