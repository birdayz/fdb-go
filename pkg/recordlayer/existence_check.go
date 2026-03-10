package recordlayer

// RecordExistenceCheck controls record existence validation during save operations.
// Matches Java's FDBRecordStoreBase.RecordExistenceCheck enum.
//
// Java Reference: com.apple.foundationdb.record.provider.foundationdb.FDBRecordStoreBase.RecordExistenceCheck
// Location: fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/provider/foundationdb/FDBRecordStoreBase.java:394
type RecordExistenceCheck int

const (
	// RecordExistenceCheckNone performs no existence validation.
	// This is the default behavior for SaveRecord.
	//
	// Java equivalent: NONE
	RecordExistenceCheckNone RecordExistenceCheck = iota

	// RecordExistenceCheckErrorIfExists throws an error if the record already exists.
	// This corresponds to InsertRecord behavior.
	//
	// Returns: ErrRecordAlreadyExists if record exists
	// Java equivalent: ERROR_IF_EXISTS
	RecordExistenceCheckErrorIfExists

	// RecordExistenceCheckErrorIfNotExists throws an error if the record does not exist.
	// Use this for update-only operations where the record must pre-exist.
	//
	// Returns: ErrRecordDoesNotExist if record doesn't exist
	// Java equivalent: ERROR_IF_NOT_EXISTS
	RecordExistenceCheckErrorIfNotExists

	// RecordExistenceCheckErrorIfTypeChanged throws an error if an existing record has a different type.
	// Use this to prevent accidentally overwriting a record of a different type with the same primary key.
	//
	// Returns: ErrRecordTypeChanged if existing record has different type
	// Java equivalent: ERROR_IF_RECORD_TYPE_CHANGED
	RecordExistenceCheckErrorIfTypeChanged

	// RecordExistenceCheckErrorIfNotExistsOrTypeChanged combines ERROR_IF_NOT_EXISTS and ERROR_IF_RECORD_TYPE_CHANGED.
	// This corresponds to UpdateRecord behavior - the record must exist and must have the same type.
	//
	// Returns: ErrRecordDoesNotExist if record doesn't exist
	// Returns: ErrRecordTypeChanged if existing record has different type
	// Java equivalent: ERROR_IF_NOT_EXISTS_OR_RECORD_TYPE_CHANGED
	RecordExistenceCheckErrorIfNotExistsOrTypeChanged
)

// ErrorIfExists returns true if this check should error when a record already exists.
//
// Java equivalent: RecordExistenceCheck.errorIfExists()
func (c RecordExistenceCheck) ErrorIfExists() bool {
	return c == RecordExistenceCheckErrorIfExists
}

// ErrorIfNotExists returns true if this check should error when a record does not exist.
//
// Java equivalent: RecordExistenceCheck.errorIfNotExists()
func (c RecordExistenceCheck) ErrorIfNotExists() bool {
	return c == RecordExistenceCheckErrorIfNotExists ||
		c == RecordExistenceCheckErrorIfNotExistsOrTypeChanged
}

// ErrorIfTypeChanged returns true if this check should error when a record's type changes.
//
// Java equivalent: RecordExistenceCheck.errorIfTypeChanged()
func (c RecordExistenceCheck) ErrorIfTypeChanged() bool {
	return c == RecordExistenceCheckErrorIfTypeChanged ||
		c == RecordExistenceCheckErrorIfNotExistsOrTypeChanged
}

// String returns a human-readable representation of the existence check.
func (c RecordExistenceCheck) String() string {
	switch c {
	case RecordExistenceCheckNone:
		return "NONE"
	case RecordExistenceCheckErrorIfExists:
		return "ERROR_IF_EXISTS"
	case RecordExistenceCheckErrorIfNotExists:
		return "ERROR_IF_NOT_EXISTS"
	case RecordExistenceCheckErrorIfTypeChanged:
		return "ERROR_IF_RECORD_TYPE_CHANGED"
	case RecordExistenceCheckErrorIfNotExistsOrTypeChanged:
		return "ERROR_IF_NOT_EXISTS_OR_RECORD_TYPE_CHANGED"
	default:
		return "UNKNOWN"
	}
}

// RecordAlreadyExistsError is returned when attempting to insert a record that already exists.
// Includes structured context matching Java's RecordAlreadyExistsException.
//
// Java equivalent: com.apple.foundationdb.record.provider.foundationdb.RecordAlreadyExistsException
// Location: fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/provider/foundationdb/RecordAlreadyExistsException.java
// Java includes: LogMessageKeys.PRIMARY_KEY
type RecordAlreadyExistsError struct {
	Message    string
	PrimaryKey any // tuple.Tuple, but using any to avoid import cycle
}

func (e *RecordAlreadyExistsError) Error() string {
	return e.Message
}

// Is implements error matching for errors.Is()
func (e *RecordAlreadyExistsError) Is(target error) bool {
	_, ok := target.(*RecordAlreadyExistsError)
	return ok
}

// RecordDoesNotExistError is returned when attempting to update a record that does not exist.
// Includes structured context matching Java's RecordDoesNotExistException.
//
// Java equivalent: com.apple.foundationdb.record.provider.foundationdb.RecordDoesNotExistException
// Location: fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/provider/foundationdb/RecordDoesNotExistException.java
// Java includes: LogMessageKeys.PRIMARY_KEY
type RecordDoesNotExistError struct {
	Message    string
	PrimaryKey any // tuple.Tuple, but using any to avoid import cycle
}

func (e *RecordDoesNotExistError) Error() string {
	return e.Message
}

// Is implements error matching for errors.Is()
func (e *RecordDoesNotExistError) Is(target error) bool {
	_, ok := target.(*RecordDoesNotExistError)
	return ok
}

// RecordTypeChangedError is returned when attempting to update a record but its type has changed.
// Includes structured context matching Java's RecordTypeChangedException.
//
// Java equivalent: com.apple.foundationdb.record.provider.foundationdb.RecordTypeChangedException
// Location: fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/provider/foundationdb/RecordTypeChangedException.java
// Java includes: LogMessageKeys.PRIMARY_KEY, LogMessageKeys.ACTUAL_TYPE, LogMessageKeys.EXPECTED_TYPE
type RecordTypeChangedError struct {
	Message      string
	PrimaryKey   any // tuple.Tuple, but using any to avoid import cycle
	ActualType   string
	ExpectedType string
}

func (e *RecordTypeChangedError) Error() string {
	return e.Message
}

// Is implements error matching for errors.Is()
func (e *RecordTypeChangedError) Is(target error) bool {
	_, ok := target.(*RecordTypeChangedError)
	return ok
}

// Sentinel errors for backwards compatibility with existing code using errors.Is()
// These can be compared with errors.Is() for simple error checking
var (
	// ErrRecordAlreadyExists is a sentinel error for backwards compatibility.
	// Prefer using RecordAlreadyExistsError for structured context.
	ErrRecordAlreadyExists = &RecordAlreadyExistsError{Message: "record already exists"}

	// ErrRecordDoesNotExist is a sentinel error for backwards compatibility.
	// Prefer using RecordDoesNotExistError for structured context.
	ErrRecordDoesNotExist = &RecordDoesNotExistError{Message: "record does not exist"}

	// ErrRecordTypeChanged is a sentinel error for backwards compatibility.
	// Prefer using RecordTypeChangedError for structured context.
	ErrRecordTypeChanged = &RecordTypeChangedError{Message: "record type changed"}
)
