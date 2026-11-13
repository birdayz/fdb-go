package executor

// ExecutionError is a failure with no values error of its own, its Kind mapped to an SQLSTATE
// by the relational layer (NonNullableFieldError has other producers a global arm would recode).
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
