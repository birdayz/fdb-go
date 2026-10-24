package executor

// ExecutionError is an execution failure whose Kind the relational layer maps
// to an SQLSTATE, as Java's ExceptionUtil maps a SemanticException's ErrorCode.
// Failures with a values error of their own (type mismatch, range bounds, enum,
// UUID) return it; a global arm for NonNullableFieldError would recode its other
// producers.
type ExecutionError struct {
	Kind    ExecutionErrorKind
	Message string
}

func (e *ExecutionError) Error() string { return e.Message }

// ExecutionErrorKind classifies an ExecutionError.
type ExecutionErrorKind int

const (
	// ExecNotNullViolation is a struct value that leaves a non-nullable field null.
	ExecNotNullViolation ExecutionErrorKind = iota + 1
	// ExecCardinalityViolation is a scalar subquery that returned more than one row.
	ExecCardinalityViolation
	// ExecSubqueryColumnCount is a scalar subquery that projected more than one column.
	ExecSubqueryColumnCount
	// ExecInternalError is a broken executor invariant.
	ExecInternalError
)
