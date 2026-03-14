package recordlayer

import (
	"fmt"

	"google.golang.org/protobuf/proto"
)

// Store function name constants matching Java's FunctionNames.
const (
	FunctionNameVersion = "version"
)

// StoreRecordFunction identifies a function to be evaluated on a record
// using store context (not index-backed).
// Matches Java's com.apple.foundationdb.record.metadata.StoreRecordFunction.
type StoreRecordFunction struct {
	Name string
}

// EvaluateStoreFunction evaluates a store-level function on a record.
// Unlike EvaluateRecordFunction (which uses indexes), store functions
// access store state directly.
//
// Currently supported functions:
//   - "version": returns the record's FDBRecordVersion (nil if none)
//
// Matches Java's FDBRecordStore.evaluateStoreFunction().
func (store *FDBRecordStore) EvaluateStoreFunction(
	fn *StoreRecordFunction,
	record *FDBStoredRecord[proto.Message],
) (any, error) {
	if fn == nil {
		return nil, fmt.Errorf("store function is nil")
	}
	if record == nil {
		return nil, fmt.Errorf("record is nil")
	}

	switch fn.Name {
	case FunctionNameVersion:
		return store.evaluateVersionFunction(record)
	default:
		return nil, fmt.Errorf("unknown store function %q", fn.Name)
	}
}

// evaluateVersionFunction returns the version for a record.
// If the record already has a complete version, returns it directly.
// Otherwise loads it from FDB.
// Matches Java's FDBRecordStore.evaluateTypedStoreFunction() VERSION branch.
func (store *FDBRecordStore) evaluateVersionFunction(
	record *FDBStoredRecord[proto.Message],
) (*FDBRecordVersion, error) {
	if record.Version != nil && record.Version.IsComplete() {
		return record.Version, nil
	}
	return store.LoadRecordVersion(record.PrimaryKey, false)
}
