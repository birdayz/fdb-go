package fdb

// KeySelector represents a description of a key in a FoundationDB database.
// A KeySelector may be resolved to a specific key with Transaction.GetKey,
// or used as endpoints of a range for GetRange.
type KeySelector struct {
	Key     KeyConvertible
	OrEqual bool
	Offset  int
}

// FDBKeySelector returns the selector itself. Satisfies Selectable.
func (ks KeySelector) FDBKeySelector() KeySelector { return ks }

// FirstGreaterOrEqual returns a KeySelector specifying the lexicographically
// least key greater than or equal to the given key.
func FirstGreaterOrEqual(key KeyConvertible) KeySelector {
	return KeySelector{Key: key, OrEqual: true, Offset: 1}
}

// FirstGreaterThan returns a KeySelector specifying the lexicographically
// least key strictly greater than the given key.
func FirstGreaterThan(key KeyConvertible) KeySelector {
	return KeySelector{Key: key, OrEqual: false, Offset: 1}
}

// LastLessOrEqual returns a KeySelector specifying the lexicographically
// greatest key less than or equal to the given key.
func LastLessOrEqual(key KeyConvertible) KeySelector {
	return KeySelector{Key: key, OrEqual: true, Offset: 0}
}

// LastLessThan returns a KeySelector specifying the lexicographically
// greatest key strictly less than the given key.
func LastLessThan(key KeyConvertible) KeySelector {
	return KeySelector{Key: key, OrEqual: false, Offset: 0}
}

// Selectable can be converted to a FoundationDB KeySelector.
type Selectable interface {
	FDBKeySelector() KeySelector
}

// Range describes all keys between a begin (inclusive) and end (exclusive)
// key selector.
type Range interface {
	FDBRangeKeySelectors() (begin, end Selectable)
}

// ExactRange describes all keys between a begin (inclusive) and end
// (exclusive) key. Any ExactRange also implements Range.
type ExactRange interface {
	FDBRangeKeys() (begin, end KeyConvertible)
	Range
}

// KeyRange is an ExactRange constructed from a pair of KeyConvertibles.
type KeyRange struct {
	Begin KeyConvertible
	End   KeyConvertible
}

// FDBRangeKeys returns the begin and end keys.
func (kr KeyRange) FDBRangeKeys() (KeyConvertible, KeyConvertible) {
	return kr.Begin, kr.End
}

// FDBRangeKeySelectors returns selectors for the begin and end of this range.
func (kr KeyRange) FDBRangeKeySelectors() (Selectable, Selectable) {
	return FirstGreaterOrEqual(kr.Begin), FirstGreaterOrEqual(kr.End)
}

// SelectorRange is a Range constructed from a pair of Selectables.
type SelectorRange struct {
	Begin Selectable
	End   Selectable
}

// FDBRangeKeySelectors returns the begin and end selectors.
func (sr SelectorRange) FDBRangeKeySelectors() (Selectable, Selectable) {
	return sr.Begin, sr.End
}

// StreamingMode controls how range reads transfer data from the database.
type StreamingMode int

const (
	// StreamingModeWantAll transfers all data in as few server requests as
	// possible. Recommended for small reads within a transaction.
	// Value matches Apple binding: fdb_c_options.g.go.
	StreamingModeWantAll StreamingMode = -2

	// StreamingModeExact transfers data in one batch, sized to the exact
	// Limit specified. A Limit must be specified.
	StreamingModeExact StreamingMode = -1

	// StreamingModeIterator provides a good balance for typical iteration.
	// This is the default (zero value).
	StreamingModeIterator StreamingMode = 0

	// StreamingModeSmall hints that only a few key-value pairs are expected.
	StreamingModeSmall StreamingMode = 1

	// StreamingModeMedium hints that a moderate number of key-value pairs
	// are expected.
	StreamingModeMedium StreamingMode = 2

	// StreamingModeLarge hints that a large number of key-value pairs are
	// expected.
	StreamingModeLarge StreamingMode = 3

	// StreamingModeSerial transfers data in large batches, useful when the
	// client is processing each result before requesting more.
	StreamingModeSerial StreamingMode = 4
)

// RangeOptions specify how a database range read operation is carried out.
type RangeOptions struct {
	// Limit restricts the number of key-value pairs returned. 0 = no limit.
	Limit int

	// Mode sets the streaming mode of the range read.
	Mode StreamingMode

	// Reverse indicates that the read should be performed in reverse
	// lexicographic order.
	Reverse bool
}
