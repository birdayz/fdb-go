package values

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

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
	// TupleSourceOther reads the VALUE tuple, as every non-KEY source does
	// in Java's IndexEntryObjectValue.eval.
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

// IndexEntryObjectValue is a LEAF Value that reads one element of an index
// entry's KEY or VALUE tuple by an ordinal path (Java's "Dewey id"). Mirrors
// Java's IndexEntryObjectValue: the entry is the correlation binding of
// IndexEntryAlias (Quantifier.current() in the plans that read entries), KEY
// reads the entry's key and any other source its value, a NULL midway answers
// NULL, and the element is converted into the row domain
// (TupleFieldsHelper.tupleValueToRuntimeValue; TupleElementToRowValue here).
// The result type is a primitive, enum or UUID (Java's constructor Verify).
type IndexEntryObjectValue struct {
	IndexEntryAlias CorrelationIdentifier
	Source          TupleSource
	OrdinalPath     []int
	ResultType      Type
}

// IndexEntryTuples is the raw KEY and VALUE tuples of an index entry, the
// binding an IndexEntryObjectValue reads (*recordlayer.IndexEntry).
type IndexEntryTuples interface {
	IndexEntryKey() tuple.Tuple
	IndexEntryValue() tuple.Tuple
}

// NewIndexEntryObjectValue constructs the leaf; a result type that is not a
// primitive, enum or UUID is refused, as Java's constructor refuses it.
func NewIndexEntryObjectValue(alias CorrelationIdentifier, source TupleSource, ordinalPath []int, resultType Type) (*IndexEntryObjectValue, error) {
	if resultType == nil || !(resultType.Code().IsPrimitive() || IsEnum(resultType) || IsUuid(resultType)) {
		return nil, fmt.Errorf("index entry object value: result type %v is not a primitive, enum or UUID", resultType)
	}
	return &IndexEntryObjectValue{
		IndexEntryAlias: alias,
		Source:          source,
		OrdinalPath:     slices.Clone(ordinalPath),
		ResultType:      resultType,
	}, nil
}

// Children returns the empty slice — leaf, no operands.
func (*IndexEntryObjectValue) Children() []Value { return []Value{} }

// Name returns the debug-print kind.
func (*IndexEntryObjectValue) Name() string { return "indexEntryObject" }

// Type returns the bound result type.
func (v *IndexEntryObjectValue) Type() Type { return v.ResultType }

// Explain is Java's `KEY:[0]`.
func (v *IndexEntryObjectValue) Explain() string {
	parts := make([]string, len(v.OrdinalPath))
	for i, ordinal := range v.OrdinalPath {
		parts[i] = strconv.Itoa(ordinal)
	}
	return v.Source.String() + ":[" + strings.Join(parts, ", ") + "]"
}

// Evaluate reads the element at OrdinalPath of the bound entry's KEY or VALUE
// tuple. A missing or mistyped binding is an error, as Java's
// requireNonNull and cast are.
func (v *IndexEntryObjectValue) Evaluate(evalCtx any) (any, error) {
	var binder CorrelationBinder
	switch ctx := evalCtx.(type) {
	case *RowEvalContext:
		binder = ctx.Correlations
	case CorrelationBinder:
		binder = ctx
	}
	if binder == nil {
		return nil, fmt.Errorf("index entry object value: no correlation bindings to read entry %s from", v.IndexEntryAlias.Name())
	}
	bound, ok := binder.GetCorrelationBinding(v.IndexEntryAlias)
	if !ok {
		return nil, fmt.Errorf("index entry object value: entry %s is not bound", v.IndexEntryAlias.Name())
	}
	entry, ok := bound.(IndexEntryTuples)
	if rv := reflect.ValueOf(entry); !ok || entry == nil || (rv.Kind() == reflect.Pointer && rv.IsNil()) {
		return nil, fmt.Errorf("index entry object value: %s is bound to %T, not an index entry", v.IndexEntryAlias.Name(), bound)
	}
	t := entry.IndexEntryValue()
	if v.Source == TupleSourceKey {
		t = entry.IndexEntryKey()
	}
	value, err := walkOrdinalPath(t, v.OrdinalPath)
	if err != nil || value == nil {
		return nil, err
	}
	return TupleElementToRowValue(value), nil
}

// walkOrdinalPath descends `t` along `path`. Mirrors Java's
// IndexKeyValueToPartialRecord.getForOrdinalPath: a NULL mid-path answers NULL,
// an out-of-bounds index or a non-tuple hop throws there, so here it is an
// error, never a NULL that would drop rows.
func walkOrdinalPath(t any, path []int) (any, error) {
	for _, idx := range path {
		if t == nil {
			return nil, nil
		}
		var element any
		switch s := t.(type) {
		case tuple.Tuple:
			if idx < 0 || idx >= len(s) {
				return nil, fmt.Errorf("index entry ordinal path: index %d out of bounds for tuple of length %d", idx, len(s))
			}
			element = s[idx]
		case []any:
			if idx < 0 || idx >= len(s) {
				return nil, fmt.Errorf("index entry ordinal path: index %d out of bounds for tuple of length %d", idx, len(s))
			}
			element = s[idx]
		default:
			return nil, fmt.Errorf("index entry ordinal path: hop %d addresses a non-tuple value %T", idx, t)
		}
		t = element
	}
	return t, nil
}

// TupleElementToRowValue converts a decoded tuple element read off an index
// entry or primary key into the row domain base records are read into, so a
// column compares, sorts, dedups and joins alike whichever access path sourced
// it: a tuple UUID is [16]byte, a FLOAT (a 32-bit tuple float) is float64 as
// ProtoScalarKindToRowValue widens it, and a VERSION index's versionstamp is
// the record version's 12 raw bytes (FDBRecordVersion.toBytes). Java's
// tupleValueToRuntimeValue conversions (INT, BYTES, ENUM) are identities here.
func TupleElementToRowValue(v any) any {
	switch tv := v.(type) {
	case tuple.UUID:
		return [16]byte(tv)
	case float32:
		return float64(tv)
	case tuple.Versionstamp:
		out := make([]byte, 0, len(tv.TransactionVersion)+2)
		out = append(out, tv.TransactionVersion[:]...)
		return append(out, byte(tv.UserVersion>>8), byte(tv.UserVersion))
	}
	return v
}

// GetCorrelatedTo returns the empty set — IndexEntryObjectValue
// matches Java's getCorrelatedToWithoutChildren contract which
// returns Set.of() (it deliberately doesn't surface the entry
// alias as a correlation, because the alias is a "binding-side"
// reference, not a dataflow correlation).
func (*IndexEntryObjectValue) GetCorrelatedTo() map[CorrelationIdentifier]struct{} {
	return map[CorrelationIdentifier]struct{}{}
}
