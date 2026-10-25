package foundationdb

import (
	"context"
	"fmt"
	"sync"

	"github.com/testcontainers/testcontainers-go"
)

// heldReaper is a connection to the session's reaper (Ryuk) kept for the life
// of the process. Ryuk removes the session 10 s after its last connection
// closes, and each container's connection closes when it terminates, so a suite
// that terminates its last container and starts another later (a Ginkgo
// AfterSuite before parallel tests resume) races that teardown: a reaper caught
// mid-removal fails the start. The held connection ends only with the process.
var heldReaper struct {
	mu   sync.Mutex
	conn chan bool
}

// holdReaper connects to the session's reaper once per process; a failure is
// returned and retried on the next call.
func holdReaper(ctx context.Context) error {
	heldReaper.mu.Lock()
	defer heldReaper.mu.Unlock()
	if heldReaper.conn != nil || testcontainers.ReadConfig().RyukDisabled {
		return nil
	}
	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		return fmt.Errorf("hold reaper: docker provider: %w", err)
	}
	// NewReaper is deprecated but the only API that hands out a reaper connection;
	// the release is pinned by TestTestcontainersVersionPinned.
	r, err := testcontainers.NewReaper(ctx, testcontainers.SessionID(), provider, "")
	if err != nil {
		return fmt.Errorf("hold reaper: %w", err)
	}
	conn, err := r.Connect()
	if err != nil {
		return fmt.Errorf("hold reaper: connect: %w", err)
	}
	heldReaper.conn = conn
	return nil
}
