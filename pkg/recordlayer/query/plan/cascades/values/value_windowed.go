package values

// WindowedValue is the base shape for all SQL window-function values
// (RANK, ROW_NUMBER, DENSE_RANK, NTILE, percentile/lag/lead, etc.).
// Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.WindowedValue`
// (an abstract class; in Go we use embedding via a plain struct).
//
// The shape is two parallel Value lists:
//
//   - PartitioningValues — the PARTITION BY columns. Rows that share
//     the same tuple of these values form a single window-frame
//     group; the windowed aggregate restarts per group.
//   - ArgumentValues — the function-specific arguments. RANK has no
//     arguments (uses ORDER BY); NTILE has one (the bucket count);
//     LAG / LEAD have 1-3 (offset, default, etc.).
//
// Java's WindowedValue is abstract — concrete subclasses
// (RankValue, RowNumberValue, NTileValue, ...) supply the per-row
// evaluation semantics. The Go port keeps this as a plain struct
// embedded by concrete window types; the empty Evaluate contract
// matches the AggregateValue pattern (window functions can't eval
// per-row alone — they need the whole partition context).
//
// **Per Java's constructor preconditions**: argumentValues must be
// non-empty for non-RANK window functions; the Go port mirrors this
// at construction. RANK's empty argumentValues is a special case
// handled by NewRankValue.
type WindowedValue struct {
	PartitioningValues []Value
	ArgumentValues     []Value
}

// NewWindowedValue constructs the embedded base. argumentValues may
// be empty (RANK / DENSE_RANK / ROW_NUMBER take no operand args —
// the windowing happens via ORDER BY); concrete subclasses with
// required operand args validate at their constructor.
func NewWindowedValue(partitioningValues, argumentValues []Value) *WindowedValue {
	// Defensive copies — caller may mutate.
	pv := make([]Value, len(partitioningValues))
	copy(pv, partitioningValues)
	av := make([]Value, len(argumentValues))
	copy(av, argumentValues)
	return &WindowedValue{PartitioningValues: pv, ArgumentValues: av}
}

// Children returns partition + argument values concatenated.
// Mirrors Java's WindowedValue.computeChildren which builds
// ImmutableList<>.addAll(partitioning).addAll(argument).
func (w *WindowedValue) Children() []Value {
	out := make([]Value, 0, len(w.PartitioningValues)+len(w.ArgumentValues))
	out = append(out, w.PartitioningValues...)
	out = append(out, w.ArgumentValues...)
	return out
}

// Evaluate returns nil — windowed aggregates can't eval per-row
// without a full partition context. Concrete subclasses MAY override
// to evaluate against a window-frame harness (see RankValue.Evaluate
// for the rank-tracking eval seed).
func (*WindowedValue) Evaluate(any) (any, error) { return nil, nil }

// SplitNewChildren splits a flat newChildren slice back into
// (partition, argument) lists by position — matching Java's
// `splitNewChildren` helper that subclass `withChildren`
// implementations call to reconstruct the partition/arg split.
//
// Length contract:
//   - Expected: len(newChildren) == len(PartitioningValues) +
//     len(ArgumentValues). When the simplification driver passes
//     newChildren = w.Children() this holds by construction.
//   - SHORT input (len(newChildren) < len(PartitioningValues)):
//     n is clipped to len(newChildren), partition gets all of
//     newChildren, argument is empty. Permissive — silently
//     drops trailing partitioning slots.
//   - LONG input (len(newChildren) > len(PartitioningValues) +
//     len(ArgumentValues)): the surplus goes into argument
//     without bounds check. Permissive — caller's invariant
//     to enforce.
//
// Today's only callers are the per-subclass `WithChildren`
// implementations (RankValue, RowNumberValue, DistanceRowNumberValue),
// which receive exactly len(Children()) values from the planner,
// so the permissive-on-mismatch behaviour never bites in practice.
// New callers must pass an exactly-sized slice; otherwise either
// partition or argument silently truncates.
func (w *WindowedValue) SplitNewChildren(newChildren []Value) (partition, argument []Value) {
	n := len(w.PartitioningValues)
	if n > len(newChildren) {
		n = len(newChildren)
	}
	return newChildren[:n], newChildren[n:]
}
