package executor

// ExecutionError is an execution failure whose Kind the relational layer maps
// to an SQLSTATE, as Java's ExceptionUtil maps a SemanticException's ErrorCode.
// The record layer names the failure; the SQL code is the relational layer's.
type ExecutionError struct {
	Kind    ExecutionErrorKind
	Message string
}

func (e *ExecutionError) Error() string { return e.Message }

// ExecutionErrorKind classifies an ExecutionError.
type ExecutionErrorKind int

const (
	// ExecIncompatibleType is Java's SemanticException INCOMPATIBLE_TYPE: a
	// value cannot be assigned to a field of its target type.
	ExecIncompatibleType ExecutionErrorKind = iota + 1
	// ExecNotNullViolation is a struct value that leaves a non-nullable field null.
	ExecNotNullViolation
	// ExecCardinalityViolation is a scalar subquery that returned more than one row.
	ExecCardinalityViolation
	// ExecSubqueryColumnCount is a scalar subquery that projected more than one column.
	ExecSubqueryColumnCount
	// ExecInternalError is a broken executor invariant.
	ExecInternalError
)
