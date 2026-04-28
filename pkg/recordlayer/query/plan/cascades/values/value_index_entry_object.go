package values

// TupleSource enumerates the two tuple-bearing fields of an FDB
// IndexEntry — the index KEY (primary scan tuple) or the index
// VALUE (associated payload tuple). Mirrors Java's
// `IndexKeyValueToPartialRecord.TupleSource`.
type TupleSource int

const (
	// TupleSourceKey selects the index entry's KEY tuple.
	TupleSourceKey TupleSource = iota
	// TupleSourceValue selects the index entry's VALUE tuple.
	TupleSourceValue
	// TupleSourceOther is the fallback used when an index entry has
	// neither KEY nor VALUE semantics for a given column (e.g.
	// extracting a deferred-fetch field from a non-covering index).
	TupleSourceOther
)

// String renders the tuple source for explain / debug print.
func (s TupleSource) String() string {
	switch s {
	case TupleSourceKey:
		return "KEY"
	case TupleSourceValue:
		return "VALUE"
	case TupleSourceOther:
		return "OTHER"
	}
	return "INVALID"
}

// IndexEntryObjectValue is a LEAF Value that references a specific
// position inside an IndexEntry's KEY or VALUE tuple, identified by
// an ordinal path (the Java-side "Dewey id"). Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.IndexEntryObjectValue`.
//
// Used by covering-index plans + index-only access paths: when a
// query can be answered directly from the index entry's key/value
// tuples (without fetching the underlying record), this Value
// extracts a specific column from those tuples by ordinal walk.
//
// Conceptually:
//
//	IndexEntryObjectValue(alias, KEY,   [0])    — first KEY column
//	IndexEntryObjectValue(alias, KEY,   [2])    — third KEY column
//	IndexEntryObjectValue(alias, VALUE, [0, 1]) — second sub-field
//	                                              of first VALUE column
//
// The alias identifies WHICH index-entry binding to read in the
// EvaluationContext (a query may join several indexed scans, each
// flowing its own IndexEntry).
//
// Constraints (matches Java's Verify.verify in the constructor):
//   - resultType must be primitive, enum, or UUID — IndexEntry tuples
//     can only hold leaf-type Tuple-encodable values; structs are
//     extracted via separate FieldValue chains, not through ordinal
//     paths.
//   - The seed enforces this via planner-checked precondition rather
//     than a runtime panic — Java's Verify would crash; we surface as
//     a no-op-ready Value that evaluates to nil if the type contract
//     is violated.
//
// Eval contract:
//   - evalCtx must be a `map[CorrelationIdentifier]any` shape with a
//     binding for `IndexEntryAlias`. The bound value is expected to
//     expose `PrimaryKey() / IndexValues()` (the
//     `*recordlayer.IndexEntry` shape) — typed as `IndexEntryReader`
//     here to keep the values package free of the recordlayer
//     dependency.
//   - On lookup miss, returns nil (matches the
//     "non-evaluable yet" pattern of placeholder Values like
//     ObjectValue / VersionValue).
type IndexEntryObjectValue struct {
	IndexEntryAlias CorrelationIdentifier
	Source          TupleSource
	OrdinalPath     []int
	ResultType      Type
}

// IndexEntryReader is the minimal interface IndexEntryObjectValue
// needs to walk an FDB index entry. The Go *recordlayer.IndexEntry
// type satisfies this contract via its PrimaryKey + IndexValues
// methods. Defined here (rather than imported from recordlayer) to
// keep the cycle-free dependency direction values → ø.
type IndexEntryReader interface {
	// PrimaryKey returns the KEY tuple of the index entry — the
	// indexed-column tuple plus the trailing primary-key columns.
	PrimaryKey() any
	// IndexValues returns the VALUE tuple of the index entry — the
	// payload tuple (typically empty for VALUE indexes; populated
	// for KeyWithValue covering-index entries).
	IndexValues() any
}

// NewIndexEntryObjectValue constructs the leaf. Caller is responsible
// for the resultType-primitive-or-enum-or-UUID precondition; the
// constructor doesn't enforce because the Type-classification
// helpers (IsPrimitive / IsEnum / etc.) live alongside the planner
// pipeline and the seed defers the check to caller (matches Java's
// Verify.verify-vs-runtime split).
func NewIndexEntryObjectValue(alias CorrelationIdentifier, source TupleSource, ordinalPath []int, resultType Type) *IndexEntryObjectValue {
	if resultType == nil {
		resultType = UnknownType
	}
	// Defensive copy — callers may reuse / mutate their backing slice.
	cp := make([]int, len(ordinalPath))
	copy(cp, ordinalPath)
	return &IndexEntryObjectValue{
		IndexEntryAlias: alias,
		Source:          source,
		OrdinalPath:     cp,
		ResultType:      resultType,
	}
}

// Children returns the empty slice — leaf, no operands.
func (*IndexEntryObjectValue) Children() []Value { return []Value{} }

// Name returns the debug-print kind.
func (*IndexEntryObjectValue) Name() string { return "indexEntryObject" }

// Type returns the bound result type.
func (v *IndexEntryObjectValue) Type() Type { return v.ResultType }

// Evaluate walks the ordinal path through the bound IndexEntry's
// KEY or VALUE tuple. Returns nil if:
//   - evalCtx is nil.
//   - evalCtx is not a `map[CorrelationIdentifier]any`.
//   - The map has no binding for IndexEntryAlias.
//   - The bound value isn't an IndexEntryReader.
//   - The ordinal walk runs off the end of the tuple.
func (v *IndexEntryObjectValue) Evaluate(evalCtx any) any {
	if evalCtx == nil {
		return nil
	}
	m, ok := evalCtx.(map[CorrelationIdentifier]any)
	if !ok {
		return nil
	}
	bound, ok := m[v.IndexEntryAlias]
	if !ok {
		return nil
	}
	reader, ok := bound.(IndexEntryReader)
	if !ok {
		return nil
	}
	var t any
	switch v.Source {
	case TupleSourceKey:
		t = reader.PrimaryKey()
	case TupleSourceValue:
		t = reader.IndexValues()
	default:
		return nil
	}
	return walkOrdinalPath(t, v.OrdinalPath)
}

// walkOrdinalPath descends through `t` along `path`, treating each
// hop as an integer-index into a `[]any` (the Tuple representation).
// Returns nil if any hop runs off the end / hits a non-slice.
func walkOrdinalPath(t any, path []int) any {
	for _, idx := range path {
		s, ok := t.([]any)
		if !ok {
			return nil
		}
		if idx < 0 || idx >= len(s) {
			return nil
		}
		t = s[idx]
	}
	return t
}

// GetCorrelatedTo returns the empty set — IndexEntryObjectValue
// matches Java's getCorrelatedToWithoutChildren contract which
// returns Set.of() (it deliberately doesn't surface the entry
// alias as a correlation, because the alias is a "binding-side"
// reference, not a dataflow correlation).
func (*IndexEntryObjectValue) GetCorrelatedTo() map[CorrelationIdentifier]struct{} {
	return map[CorrelationIdentifier]struct{}{}
}
