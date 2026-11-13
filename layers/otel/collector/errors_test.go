package collector

import (
	"context"
	"errors"
	"testing"
)

func TestCollectorErrorsRetainPublicContextAndCauses(t *testing.T) {
	t.Parallel()
	cause := context.DeadlineExceeded
	err := componentErrorf("configuration file: %w", cause)
	var component *ComponentError
	if !errors.As(err, &component) || component.Component != ComponentType || component.Operation == "" || !errors.Is(err, cause) {
		t.Fatalf("component error lost context/cause: %v", err)
	}
	cfg := defaultConfig()
	cfg.Timeout = 0
	if err := cfg.Validate(); !errors.As(err, &component) {
		t.Fatalf("validation is not typed: %v", err)
	}
	if _, err := ParseComponentID("bad-type"); !errors.As(err, &component) {
		t.Fatalf("parser is not typed: %v", err)
	}
	bench := DefaultCollectorBenchConfig()
	bench.Conversations = -1
	if err := bench.Validate(); !errors.As(err, &component) {
		t.Fatalf("benchmark validation is not typed: %v", err)
	}
	queue := &QueueFullError{Capacity: 7, Queued: 6, Requested: 2, Cause: cause}
	var gotQueue *QueueFullError
	wrapped := componentErrorf("enqueue: %w", queue)
	if !errors.As(wrapped, &gotQueue) || gotQueue.Capacity != 7 || gotQueue.Queued != 6 || gotQueue.Requested != 2 || !errors.Is(wrapped, ErrQueueFull) || !errors.Is(wrapped, cause) {
		t.Fatalf("queue context/cause lost: %v", wrapped)
	}
	stopped := &NotRunningError{Operation: "consume", State: stateStopped}
	var lifecycle *NotRunningError
	if !errors.As(stopped, &lifecycle) || lifecycle.Operation != "consume" || !errors.Is(stopped, ErrNotRunning) {
		t.Fatalf("lifecycle context lost: %v", stopped)
	}
}
