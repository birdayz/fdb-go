package values

// RowNumberValue is the SQL ROW_NUMBER() window function — assigns
// a UNIQUE 1-based sequential number within each partition (no
// tie-sharing — distinct from RANK whose ties share a number).
// Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.RowNumberValue`.
//
// Per Java's contract, ROW_NUMBER is INDEX-ONLY (Java's class
// implements Value.IndexOnlyValue): the row number can only be
// produced during an HNSW index traversal — typically when the
// surrounding query is `ORDER BY <distance>` over a vector index.
// The query planner refuses to compute ROW_NUMBER without a
// suitable index. The Go seed records this constraint in
// IsIndexOnly() (the analyzer / matchers can read it without
// importing a separate Value.IndexOnlyValue marker interface).
//
// HNSW configuration parameters (from Java's `OPTIONS` clause):
//   - EfSearch: HNSW search-quality knob — higher values increase
//     recall (accuracy) at the cost of performance. nil = use the
//     index's default `ef_search`.
//   - IsReturningVectors: whether the index scan returns the actual
//     vector payloads (true) or only distance + ID (false / nil =
//     omit vectors, smaller transfer).
//
// Both are optional pointers so a missing OPTION in the SQL surface
// remains representable as nil rather than a sentinel value.
//
// Per the Java doc, ROW_NUMBER over distance(<vector>, queryVec)
// followed by `<= K` is the K-NN search pattern that gets
// transformed into a DistanceRankValueComparison + matched against
// HNSW indexes. The Go seed does NOT yet implement that transform —
// the supporting Value types (Euclidean / Cosine / Dot-product
// DistanceRowNumberValue specialisations) aren't ported. The bare
// RowNumberValue surface is enough for parser / planner reachability.
//
// Result type: NotNullLong (ROW_NUMBER is always populated, 1-based).
type RowNumberValue struct {
	WindowedValue
	EfSearch           *int  // optional HNSW ef_search override
	IsReturningVectors *bool // optional HNSW vector-payload toggle
}

// NewRowNumberValue constructs a ROW_NUMBER() value. partitioning
// values + argument values follow the Java contract (Java's
// argumentValues for the index-tied form is the distance argument
// list; bare ROW_NUMBER() takes empty arguments).
func NewRowNumberValue(partitioningValues, argumentValues []Value, efSearch *int, isReturningVectors *bool) *RowNumberValue {
	return &RowNumberValue{
		WindowedValue: WindowedValue{
			PartitioningValues: append([]Value(nil), partitioningValues...),
			ArgumentValues:     append([]Value(nil), argumentValues...),
		},
		EfSearch:           efSearch,
		IsReturningVectors: isReturningVectors,
	}
}

// Name returns the SQL function name.
func (*RowNumberValue) Name() string { return "ROW_NUMBER" }

// Type returns NotNullLong — ROW_NUMBER is always populated.
func (*RowNumberValue) Type() Type { return NotNullLong }

// IsIndexOnly returns true — ROW_NUMBER cannot be computed outside
// of an index scan (the row-number value is computed during the
// index's search-graph traversal, not from the base record). Java's
// equivalent is the IndexOnlyValue marker interface; Go uses an
// accessor.
//
// Planner / matcher code should consult IsIndexOnly to refuse to
// optimise ROW_NUMBER paths that don't have a matching index
// available — Java's MatchCandidate-side validation does the same.
func (*RowNumberValue) IsIndexOnly() bool { return true }

// Evaluate returns the current row number from the row-shape harness
// pattern. The harness exposes the streaming-window operator's
// per-row row-number counter via the `_row_number` key.
//
// Returns nil if evalCtx is nil / non-map / has no `_row_number`
// key — matches the placeholder-Value pattern.
func (r *RowNumberValue) Evaluate(evalCtx any) any {
	res, err := r.EvaluateErr(evalCtx)
	if err != nil {
		panic(err)
	}
	return res
}

// EvaluateErr is the error-returning twin of Evaluate (RFC-091).
func (*RowNumberValue) EvaluateErr(evalCtx any) (any, error) {
	if evalCtx == nil {
		return nil, nil
	}
	if m, ok := evalCtx.(map[string]any); ok {
		if r, ok := m["_row_number"]; ok {
			return r, nil
		}
	}
	return nil, nil
}

// WithChildren returns a new RowNumberValue with the children
// re-split via WindowedValue.SplitNewChildren. Both partition + arg
// lists are reconstructed; the HNSW config carries through unchanged.
func (r *RowNumberValue) WithChildren(newChildren []Value) *RowNumberValue {
	partition, argument := r.SplitNewChildren(newChildren)
	return NewRowNumberValue(partition, argument, r.EfSearch, r.IsReturningVectors)
}

var _ IndexOnly = (*RowNumberValue)(nil)
