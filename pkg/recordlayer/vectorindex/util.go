package vectorindex

import (
	"bytes"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// rangeIterator is the part of fdb.RangeIterator a graph read consumes.
type rangeIterator interface {
	Advance() bool
	Get() (fdb.KeyValue, error)
}

// tupleEqual compares two tuples by their packed representations.
func tupleEqual(a, b tuple.Tuple) bool {
	return bytes.Equal(a.Pack(), b.Pack())
}

// asInt64 extracts an int64 from a tuple element.
func asInt64(v any) (int64, bool) {
	switch val := v.(type) {
	case int64:
		return val, true
	case int:
		return int64(val), true
	case int32:
		return int64(val), true
	default:
		return 0, false
	}
}

// optionValueOrDefault returns the option value if present, otherwise the default.
func optionValueOrDefault(opts map[string]string, key, defaultValue string) string {
	if v, ok := opts[key]; ok {
		return v
	}
	return defaultValue
}

// positiveEfSearch adapts the int-valued entry points, where 0 means "not
// set", to the typed option.
func positiveEfSearch(efSearch int) *int {
	if efSearch <= 0 {
		return nil
	}
	return &efSearch
}
