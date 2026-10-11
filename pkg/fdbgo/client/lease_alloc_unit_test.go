package client

import "testing"

// Not parallel: AllocsPerRun panics under t.Parallel and counts process-wide.
func TestStateLeaseDoesNotAllocate(t *testing.T) {
	if testing.CoverMode() != "" {
		t.Skip("coverage instrumentation changes inlining decisions")
	}
	tx := &Transaction{}
	defer tx.Cancel()
	for name, call := range map[string]func(){
		"enterState": func() { tx.enterState().release() },
		"TenantId":   func() { _ = tx.TenantId() },
	} {
		if n := testing.AllocsPerRun(100, call); n != 0 {
			t.Errorf("LEASE_ALLOC: %s allocated %v times per call, want 0", name, n)
		}
	}
}
