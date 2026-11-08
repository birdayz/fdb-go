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
	// TransientError is an FDB failure a new call can pass: a transaction
	// timeout or a retryable condition that outlasted the timeout.
	TransientError struct{ *OtelError }
	// StaleMetadataError (kind stale) is a store whose header names a newer
	// metadata version than the Reader opened with: reopening it reloads the
	// schema's metadata.
	StaleMetadataError struct{ *OtelError }
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
	case "transient":
		return &TransientError{e}
	case "stale":
		return &StaleMetadataError{e}
	default:
		return &OperationError{e}
	}
}

// NewError is this package's typed error of kind (config, ownership,
// admission, budget, corruption, http, audit, oracle, transient, stale; anything
// else is an OperationError), wrapping cause when it is set: the errors a
// Backend outside this package returns, as this package's backends do.
func NewError(kind, op, message string, cause error) error {
	return typedError(kind, op, message, cause)
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
