package collector

import "fmt"

// ComponentError is a local Collector-model configuration or operation failure.
// Cause retains parse, validation, sink and cancellation errors for errors.As/Is.
type ComponentError struct {
	Component string
	Operation string
	Message   string
	Cause     error
}

func (e *ComponentError) Error() string { return e.Message }
func (e *ComponentError) Unwrap() error { return e.Cause }

func componentErrorf(format string, args ...any) error {
	cause := fmt.Errorf(format, args...)
	return &ComponentError{Component: ComponentType, Operation: "collector model", Message: cause.Error(), Cause: cause}
}

// QueueFullError describes the admission capacity and retains a waiting caller's cause.
type QueueFullError struct {
	Capacity  int
	Queued    int
	Requested int
	Cause     error
}

func (e *QueueFullError) Error() string {
	return fmt.Sprintf("fdb_otel: sending queue is full (capacity=%d queued=%d requested=%d)", e.Capacity, e.Queued, e.Requested)
}
func (e *QueueFullError) Unwrap() error        { return e.Cause }
func (e *QueueFullError) Is(target error) bool { _, ok := target.(*QueueFullError); return ok }

// NotRunningError identifies a rejected lifecycle operation and observed state.
type NotRunningError struct {
	Operation string
	State     int
}

func (e *NotRunningError) Error() string {
	return fmt.Sprintf("fdb_otel: exporter is not running (operation=%s state=%d)", e.Operation, e.State)
}
func (e *NotRunningError) Is(target error) bool { _, ok := target.(*NotRunningError); return ok }
