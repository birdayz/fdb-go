package api

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Error is the Go equivalent of Java's
// com.apple.foundationdb.relational.api.exceptions.RelationalException.
//
// It carries a SQLSTATE-formatted ErrorCode, a human message, and an
// optional wrapped cause + structured context map. Match with
// errors.As:
//
//	var e *api.Error
//	if errors.As(err, &e) && e.Code == api.ErrCodeUndefinedTable {
//	    // handle undefined-table specifically
//	}
//
// Do NOT string-match on Error() output — the wording is not API.
type Error struct {
	// Code is the SQLSTATE error code.
	Code ErrorCode
	// Message is the human-readable error description.
	Message string
	// Cause is the underlying error, if any. Unwrap returns it.
	Cause error
	// Context carries optional structured logging fields (table name,
	// column name, etc.). Corresponds to Java's errorContext map.
	Context map[string]any
}

// NewError returns a new Error with the given code and message.
func NewError(code ErrorCode, message string) *Error {
	return &Error{Code: code, Message: message}
}

// NewErrorf is NewError with fmt.Sprintf formatting.
func NewErrorf(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WrapError wraps cause with a relational error code and message.
func WrapError(code ErrorCode, message string, cause error) *Error {
	return &Error{Code: code, Message: message, Cause: cause}
}

// WrapErrorf is WrapError with fmt.Sprintf formatting for the message.
// Use instead of fmt.Errorf("context: %w", err) at API boundaries so the
// error carries a SQLSTATE that errors.As can match.
func WrapErrorf(cause error, code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Cause: cause}
}

// Error renders the error in the form "SQLSTATE: message: cause"
// with the context fields appended. The wording is informational —
// callers must not parse it.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(string(e.Code))
	b.WriteString(": ")
	b.WriteString(e.Message)
	if len(e.Context) > 0 {
		// Sort keys so Error() output is deterministic — map iteration
		// is randomised, and log diffing / test assertions break
		// without a stable order.
		keys := make([]string, 0, len(e.Context))
		for k := range e.Context {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString(" [")
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s=%v", k, e.Context[k])
		}
		b.WriteString("]")
	}
	if e.Cause != nil {
		b.WriteString(": ")
		b.WriteString(e.Cause.Error())
	}
	return b.String()
}

// Unwrap returns the wrapped cause, enabling errors.Is / errors.As
// through the chain.
func (e *Error) Unwrap() error { return e.Cause }

// WithContext returns a copy of e with the given key/value added to
// the context map. Matches Java's RelationalException.addContext().
// The receiver is not mutated.
func (e *Error) WithContext(key string, value any) *Error {
	out := *e
	if out.Context != nil {
		merged := make(map[string]any, len(out.Context)+1)
		for k, v := range out.Context {
			merged[k] = v
		}
		out.Context = merged
	} else {
		out.Context = make(map[string]any, 1)
	}
	out.Context[key] = value
	return &out
}

// AsError extracts an *Error from the chain, or returns nil if there
// is none. Convenience around errors.As.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}
