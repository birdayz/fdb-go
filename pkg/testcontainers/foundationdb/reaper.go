package foundationdb

import (
	"context"
	"fmt"
	"sync"

	"github.com/testcontainers/testcontainers-go"
)

// heldReaper keeps Ryuk alive between a suite's last container and later parallel
// tests. The connection closes with the process, not with an individual container.
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
	provider, err := testcontainers.ProviderDefault.GetProvider()
	if err != nil {
		return fmt.Errorf("hold reaper: provider: %w", err)
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
