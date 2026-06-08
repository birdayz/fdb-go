package fdb

import (
	"context"
	"testing"
	"time"
)

// TestBootstrapContext pins the P0.4 bootstrap-bound fix: a deadline-less context
// (context.Background(), which OpenWithConnectionString/OpenDatabaseFromConfig used
// to pass straight through) must be bounded so an unreachable cluster fails fast
// instead of blocking forever; a caller-supplied deadline must be respected.
func TestBootstrapContext(t *testing.T) {
	t.Parallel()

	// No caller deadline -> default bootstrap timeout applied.
	ctx, cancel := bootstrapContext(context.Background())
	defer cancel()
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("deadline-less context must be bounded so bootstrap cannot hang forever")
	}
	if d := time.Until(dl); d <= 0 || d > defaultBootstrapTimeout+time.Second {
		t.Errorf("bootstrap deadline = %v from now, want ~%v", d, defaultBootstrapTimeout)
	}

	// Caller-supplied deadline is respected, not overridden.
	want := time.Now().Add(3 * time.Second)
	cctx, ccancel := context.WithDeadline(context.Background(), want)
	defer ccancel()
	got, gotCancel := bootstrapContext(cctx)
	defer gotCancel()
	if gotDL, _ := got.Deadline(); !gotDL.Equal(want) {
		t.Errorf("caller deadline not respected: got %v, want %v", gotDL, want)
	}
}
