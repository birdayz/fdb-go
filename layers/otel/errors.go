package otel

import "fmt"

// OtelError preserves an operation's category, context and underlying cause.
type OtelError struct {
	Kind    string
	Op      string
	Message string
	Cause   error
}

func (e *OtelError) Error() string {
	message := e.Message
	if message == "" && e.Cause != nil {
		message = e.Cause.Error()
	}
	return fmt.Sprintf("otel %s %s: %s", e.Kind, e.Op, message)
}
func (e *OtelError) Unwrap() error { return e.Cause }
func (e *OtelError) As(target any) bool {
	if ptr, ok := target.(**OtelError); ok {
		*ptr = e
		return true
	}
	return false
}

type (
	ConfigError      struct{ *OtelError }
	OwnershipError   struct{ *OtelError }
	AdmissionError   struct{ *OtelError }
	QueryBudgetError struct{ *OtelError }
	CorruptionError  struct{ *OtelError }
	HTTPError        struct{ *OtelError }
	AuditError       struct{ *OtelError }
	OracleError      struct{ *OtelError }
	OperationError   struct{ *OtelError }
)

func typedError(kind, op, message string, cause error) error {
	e := &OtelError{Kind: kind, Op: op, Message: message, Cause: cause}
	switch kind {
	case "config":
		return &ConfigError{e}
	case "ownership":
		return &OwnershipError{e}
	case "admission":
		return &AdmissionError{e}
	case "budget":
		return &QueryBudgetError{e}
	case "corruption":
		return &CorruptionError{e}
	case "http":
		return &HTTPError{e}
	case "audit":
		return &AuditError{e}
	case "oracle":
		return &OracleError{e}
	default:
		return &OperationError{e}
	}
}
func fail(kind, op, message string) error { return typedError(kind, op, message, nil) }
func failf(kind, op, format string, args ...any) error {
	return fail(kind, op, fmt.Sprintf(format, args...))
}

func wrap(kind, op string, cause error) error {
	if cause == nil {
		return nil
	}
	return typedError(kind, op, "", cause)
}
