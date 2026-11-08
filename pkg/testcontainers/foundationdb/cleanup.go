package foundationdb

import (
	"context"
	"fmt"
	"time"
)

// TerminateTimeout is two minutes, CLAUDE.md's setup bound: Docker's forced removal can exceed 30s under concurrent load.
const TerminateTimeout = 2 * time.Minute

// Terminator is a container or cluster that can be stopped during fixture cleanup.
type Terminator interface {
	GetContainerID() string
	Terminate(context.Context) error
}

// TerminationError identifies a failed cleanup, including during container startup.
type TerminationError struct {
	ContainerID string
	Err         error
}

func (e *TerminationError) Error() string {
	return fmt.Sprintf("terminate container %s: %v", e.ContainerID, e.Err)
}

func (e *TerminationError) Unwrap() error { return e.Err }

// Terminate stops c with the caller's cleanup context and includes its ID on failure.
func Terminate(ctx context.Context, c Terminator) error {
	id := c.GetContainerID()
	if err := c.Terminate(ctx); err != nil {
		return &TerminationError{ContainerID: id, Err: err}
	}
	return nil
}

// Teardown terminates c under a fresh TerminateTimeout context, so a cancelled caller context cannot cut cleanup short.
func Teardown(c Terminator) error {
	ctx, cancel := context.WithTimeout(context.Background(), TerminateTimeout)
	defer cancel()
	return Terminate(ctx, c)
}

// TeardownT runs Teardown and fails the test or benchmark if cleanup fails.
func TeardownT(t interface {
	Helper()
	Errorf(string, ...any)
}, c Terminator,
) {
	t.Helper()
	if err := Teardown(c); err != nil {
		t.Errorf("%v", err)
	}
}

// TerminateT stops c and fails the test or benchmark if cleanup fails.
// Call it from a defer or Cleanup callback with the existing cleanup context.
func TerminateT(t interface {
	Helper()
	Errorf(string, ...any)
}, ctx context.Context, c Terminator,
) {
	t.Helper()
	if err := Terminate(ctx, c); err != nil {
		t.Errorf("%v", err)
	}
}
