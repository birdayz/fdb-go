// Package api mirrors Java's fdb-relational-api module.
//
// It defines the public interfaces and types for the relational (SQL)
// layer: Connection, Statement, ResultSet, DataType, Options, ErrorCode,
// and related metadata types. Implementations live under
// pkg/relational/core (embedded) and pkg/relational/sqldriver
// (database/sql driver adapter).
package api

// ErrorCode is a 5-character SQLSTATE-formatted error code.
//
// Codes follow SQLSTATE format: first 2 characters are the class,
// remaining 3 describe the error. Tracks Java's
// com.apple.foundationdb.relational.api.exceptions.ErrorCode enum — every Java
// code exists here, and a new code on either side must be added to both.
//
// Go additionally defines a small set of codes Java has no member for. They are
// enumerated in goOnlyErrorCodes below, not described in prose: a prose claim
// about how many there are rots silently, whereas that list is an artifact
// anyone can diff against Java, and TestErrorCodesMatchJava does exactly that
// on every build.
//
// The first two characters are the SQLSTATE class; errorCodeClasses names each
// one. That list is data rather than prose for the same reason as
// goOnlyErrorCodes: as a doc comment it drifted in both directions at once,
// naming a class with no code and omitting a class that had two.
type ErrorCode string

// SQLSTATE error codes. These cover Java's ErrorCode enum in full, plus the
// deliberate Go-only additions listed in goOnlyErrorCodes.
const (
	// Class 00 — Successful Completion
	ErrCodeSuccess ErrorCode = "00000"

	// Class 02 — No Data
	ErrCodeNoResultSet ErrorCode = "02F01"

	// Class 08 — Connection Exception
	ErrCodeUnableToEstablishSQLConnection     ErrorCode = "08001"
	ErrCodeConnectionDoesNotExist             ErrorCode = "08003"
	ErrCodeInvalidPath                        ErrorCode = "08F01"
	ErrCodeCannotCommitRollbackWithAutocommit ErrorCode = "08F02"

	// Class 0A — Feature Not Supported
	ErrCodeUnsupportedOperation ErrorCode = "0A000"
	ErrCodeUnsupportedQuery     ErrorCode = "0AF00"
	ErrCodeUnsupportedSort      ErrorCode = "0AF01"

	// Class 21 — Cardinality Violation
	ErrCodeCardinalityViolation ErrorCode = "21000"

	// Class 22 — Data Exception
	ErrCodeCannotConvertType            ErrorCode = "22000"
	ErrCodeNumericValueOutOfRange       ErrorCode = "22003"
	ErrCodeDivisionByZero               ErrorCode = "22012"
	ErrCodeInvalidRowCountInLimitClause ErrorCode = "2201W"
	ErrCodeInvalidParameter             ErrorCode = "22023"
	ErrCodeArrayElementError            ErrorCode = "2202E"
	ErrCodeInvalidBinaryRepresentation  ErrorCode = "22F03"
	ErrCodeInvalidArgumentForFunction   ErrorCode = "22F00"
	ErrCodeInvalidCast                  ErrorCode = "22F3H"
	ErrCodeCopySerializationError       ErrorCode = "22F04"
	ErrCodeCopyImportValidationError    ErrorCode = "22F08"

	// Class 23 — Integrity Constraint Violation
	ErrCodeNotNullViolation          ErrorCode = "23502"
	ErrCodeUniqueConstraintViolation ErrorCode = "23505"

	// Class 24 — Invalid Cursor State
	ErrCodeInvalidCursorState  ErrorCode = "24000"
	ErrCodeInvalidContinuation ErrorCode = "24F00"

	// Class 25 — Invalid Transaction State
	ErrCodeInvalidTransactionState ErrorCode = "25000"
	ErrCodeTransactionInactive     ErrorCode = "25F01"

	// Class 40 — Transaction Rollback
	ErrCodeSerializationFailure ErrorCode = "40001"

	// Class 42 — Syntax Error or Access Rule Violation
	ErrCodeSyntaxOrAccessViolation           ErrorCode = "42000"
	ErrCodeInsufficientPrivilege             ErrorCode = "42501"
	ErrCodeSyntaxError                       ErrorCode = "42601"
	ErrCodeInvalidName                       ErrorCode = "42602"
	ErrCodeColumnAlreadyExists               ErrorCode = "42701"
	ErrCodeAmbiguousColumn                   ErrorCode = "42702"
	ErrCodeUndefinedColumn                   ErrorCode = "42703"
	ErrCodeDuplicateAlias                    ErrorCode = "42712"
	ErrCodeDuplicateFunction                 ErrorCode = "42723"
	ErrCodeGroupingError                     ErrorCode = "42803"
	ErrCodeDatatypeMismatch                  ErrorCode = "42804"
	ErrCodeWrongObjectType                   ErrorCode = "42809"
	ErrCodeUndefinedFunction                 ErrorCode = "42883"
	ErrCodeUndefinedDatabase                 ErrorCode = "42F00"
	ErrCodeUndefinedTable                    ErrorCode = "42F01"
	ErrCodeUndefinedParameter                ErrorCode = "42F02"
	ErrCodeDatabaseAlreadyExists             ErrorCode = "42F04"
	ErrCodeSchemaAlreadyExists               ErrorCode = "42F06"
	ErrCodeTableAlreadyExists                ErrorCode = "42F07"
	ErrCodeInvalidColumnReference            ErrorCode = "42F10"
	ErrCodeInvalidFunctionDefinition         ErrorCode = "42F13"
	ErrCodeInvalidTableDefinition            ErrorCode = "42F16"
	ErrCodeUnknownType                       ErrorCode = "42F18"
	ErrCodeInvalidRecursion                  ErrorCode = "42F19"
	ErrCodeIncompatibleTableAlias            ErrorCode = "42F20"
	ErrCodeWindowingError                    ErrorCode = "42F21"
	ErrCodeSchemaMappingAlreadyExists        ErrorCode = "42F50"
	ErrCodeUndefinedSchema                   ErrorCode = "42F51"
	ErrCodeUndefinedIndex                    ErrorCode = "42F54"
	ErrCodeUnknownSchemaTemplate             ErrorCode = "42F55"
	ErrCodeAnnotationAlreadyExists           ErrorCode = "42F56"
	ErrCodeIndexAlreadyExists                ErrorCode = "42F57"
	ErrCodeIncorrectMetadataTableVersion     ErrorCode = "42F58"
	ErrCodeInvalidSchemaTemplate             ErrorCode = "42F59"
	ErrCodeInvalidPreparedStatementParameter ErrorCode = "42F60"
	ErrCodeExecuteUpdateReturnedResultSet    ErrorCode = "42F61"
	ErrCodeDuplicateSchemaTemplate           ErrorCode = "42F62"
	ErrCodeUnknownDatabase                   ErrorCode = "42F63"
	ErrCodeUnionIncorrectColumnCount         ErrorCode = "42F64"
	ErrCodeUnionIncompatibleColumns          ErrorCode = "42F65"
	ErrCodeInvalidDatabase                   ErrorCode = "42F66"

	// Class 53 — Insufficient Resources
	ErrCodeTransactionTimeout ErrorCode = "53F00"

	// Class 54 — Program Limit Exceeded
	ErrCodeTooManyColumns        ErrorCode = "54011"
	ErrCodeExecutionLimitReached ErrorCode = "54F01"
	// ErrCodePlanComplexityLimitReached reports that the Cascades planner
	// exhausted a planning budget before the search converged. It is distinct
	// from ErrCodeExecutionLimitReached, which means an EXECUTION-time limit and
	// is tied to a scan/row continuation reason: conflating plan-time with it
	// would corrupt a meaning Java owns. Go-only — see the note on ErrorCode.
	ErrCodePlanComplexityLimitReached ErrorCode = "54F02"

	// Class 55 — Object Not In Prerequisite State
	ErrCodeStatementClosed ErrorCode = "55F00"

	// Class 58 — System Error
	ErrCodeUndefinedFile ErrorCode = "58F01"

	// Class XX — Internal Error
	ErrCodeUnknown                ErrorCode = "XXXXX"
	ErrCodeInternalError          ErrorCode = "XX000"
	ErrCodeDeserializationFailure ErrorCode = "XXF01"
)

// errorCodeClasses names every SQLSTATE class this package defines codes in,
// keyed by the two-character class prefix.
//
// TestErrorCodeClassesMatchConstants keeps it exact in both directions: a class
// with codes but no entry fails, and an entry with no codes fails. The doc
// comment it replaced had both defects simultaneously — it listed "01 Warning",
// for which neither Go nor Java has a single code, while omitting class 21,
// which has been defined here all along.
var errorCodeClasses = map[string]string{
	"00": "Successful Completion",
	"02": "No Data",
	"08": "Connection Exception",
	"0A": "Unsupported Operation",
	"21": "Cardinality Violation",
	"22": "Data Exception",
	"23": "Integrity Constraint Violation",
	"24": "Invalid Cursor State",
	"25": "Invalid Transaction State",
	"40": "Transaction Rollback",
	"42": "Syntax Error or Access Rule Violation",
	"53": "Insufficient Resources",
	"54": "Program Limit Exceeded",
	"55": "Object Not In Prerequisite State",
	"58": "System Error",
	"XX": "Internal Error",
}

// goOnlyErrorCodes enumerates every code Go defines that Java's ErrorCode enum
// has no member for, with the reason each exists. It is the authoritative
// answer to "how far has Go drifted from Java's enum" — TestErrorCodesMatchJava
// fails if this list and the actual Go/Java difference disagree in either
// direction, so it cannot quietly go stale the way a sentence in a doc comment
// can.
//
// Adding an entry here is a deliberate divergence and belongs in
// DIVERGENCES.md. Java-side codes missing from Go are NOT tracked here: that
// set must stay empty, and the same test enforces it.
var goOnlyErrorCodes = map[ErrorCode]string{
	ErrCodeCardinalityViolation: "SQL-standard class 21. Java's enum has no class-21 member and its " +
		"relational layer has no scalar-subquery cardinality check at all, so Go " +
		"needed a code to report a subquery that returned more than one row.",
	ErrCodeNumericValueOutOfRange: "SQL-standard 22003 for INTEGRAL arithmetic overflow (the " +
		"floating-point lane is IEEE in both engines and raises nothing). Java's " +
		"enum has no member for it; an overflow there escapes as a raw Java " +
		"exception and ExceptionUtil's fallthrough reports UNKNOWN.",
	ErrCodeDivisionByZero: "SQL-standard 22012 for INTEGRAL division by zero; the floating-point " +
		"lane matches Java's IEEE DIV_DD, where 1.0/0.0 is Infinity and no error " +
		"is raised. Java's enum has no member for the integral case; it raises " +
		"java.lang.ArithmeticException, which ExceptionUtil maps to UNKNOWN via " +
		"its final fallthrough.",
	ErrCodePlanComplexityLimitReached: "Go bounds Cascades planning at 100,000 tasks; Java's SQL layer " +
		"never enables its three planner caps, so the condition cannot arise " +
		"there and no Java code names it. See DIVERGENCES.md.",
}

// allErrorCodes is the fast-lookup set for ErrorCodeFromString.
// Built once at init() and never mutated.
var allErrorCodes = map[string]ErrorCode{}

func init() {
	for _, c := range []ErrorCode{
		ErrCodeSuccess,
		ErrCodeNoResultSet,
		ErrCodeUnableToEstablishSQLConnection, ErrCodeConnectionDoesNotExist, ErrCodeInvalidPath, ErrCodeCannotCommitRollbackWithAutocommit,
		ErrCodeUnsupportedOperation, ErrCodeUnsupportedQuery, ErrCodeUnsupportedSort,
		ErrCodeCardinalityViolation,
		ErrCodeCannotConvertType, ErrCodeNumericValueOutOfRange, ErrCodeDivisionByZero, ErrCodeInvalidRowCountInLimitClause, ErrCodeInvalidParameter, ErrCodeArrayElementError,
		ErrCodeInvalidBinaryRepresentation, ErrCodeInvalidArgumentForFunction, ErrCodeInvalidCast,
		ErrCodeCopySerializationError, ErrCodeCopyImportValidationError,
		ErrCodeNotNullViolation, ErrCodeUniqueConstraintViolation,
		ErrCodeInvalidCursorState, ErrCodeInvalidContinuation,
		ErrCodeInvalidTransactionState, ErrCodeTransactionInactive,
		ErrCodeSerializationFailure,
		ErrCodeSyntaxOrAccessViolation, ErrCodeInsufficientPrivilege, ErrCodeSyntaxError, ErrCodeInvalidName,
		ErrCodeColumnAlreadyExists, ErrCodeAmbiguousColumn, ErrCodeUndefinedColumn, ErrCodeDuplicateAlias,
		ErrCodeDuplicateFunction, ErrCodeGroupingError, ErrCodeDatatypeMismatch, ErrCodeWrongObjectType,
		ErrCodeUndefinedFunction, ErrCodeUndefinedDatabase, ErrCodeUndefinedTable, ErrCodeUndefinedParameter,
		ErrCodeDatabaseAlreadyExists, ErrCodeSchemaAlreadyExists, ErrCodeTableAlreadyExists,
		ErrCodeInvalidColumnReference, ErrCodeInvalidFunctionDefinition, ErrCodeInvalidTableDefinition,
		ErrCodeUnknownType, ErrCodeInvalidRecursion, ErrCodeIncompatibleTableAlias, ErrCodeWindowingError,
		ErrCodeSchemaMappingAlreadyExists, ErrCodeUndefinedSchema, ErrCodeUndefinedIndex,
		ErrCodeUnknownSchemaTemplate, ErrCodeAnnotationAlreadyExists, ErrCodeIndexAlreadyExists,
		ErrCodeIncorrectMetadataTableVersion, ErrCodeInvalidSchemaTemplate, ErrCodeInvalidPreparedStatementParameter,
		ErrCodeExecuteUpdateReturnedResultSet, ErrCodeDuplicateSchemaTemplate, ErrCodeUnknownDatabase,
		ErrCodeUnionIncorrectColumnCount, ErrCodeUnionIncompatibleColumns, ErrCodeInvalidDatabase,
		ErrCodeTransactionTimeout, ErrCodeTooManyColumns, ErrCodeExecutionLimitReached, ErrCodePlanComplexityLimitReached,
		ErrCodeStatementClosed, ErrCodeUndefinedFile,
		ErrCodeUnknown, ErrCodeInternalError, ErrCodeDeserializationFailure,
	} {
		allErrorCodes[string(c)] = c
	}
}

// ErrorCodeFromString looks up an ErrorCode by its 5-character SQLSTATE
// string. Unknown codes return ErrCodeUnknown (matches Java's
// ErrorCode.get() fallback).
func ErrorCodeFromString(s string) ErrorCode {
	if c, ok := allErrorCodes[s]; ok {
		return c
	}
	return ErrCodeUnknown
}

// String returns the 5-character SQLSTATE representation.
func (c ErrorCode) String() string { return string(c) }

// Class returns the 2-character SQLSTATE class prefix (e.g. "42" for
// syntax errors). Returns empty string if the code is shorter than 2
// characters (should never happen for well-formed codes).
func (c ErrorCode) Class() string {
	if len(c) < 2 {
		return ""
	}
	return string(c[:2])
}
