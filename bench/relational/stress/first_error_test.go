//go:build stress

package stress_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// firstStressError retains the first worker failure without replacing it with
// an error raised by a later worker. Store a pointer to the error interface:
// atomic.Value would panic when workers report different concrete error types,
// even when a compare-and-swap is only trying to fill an empty holder.
type firstStressError struct {
	value atomic.Pointer[error]
}

func (f *firstStressError) Record(err error) {
	if err != nil {
		f.value.CompareAndSwap(nil, &err)
	}
}

func (f *firstStressError) Load() error {
	if value := f.value.Load(); value != nil {
		return *value
	}
	return nil
}

func TestFirstStressError(t *testing.T) {
	t.Parallel()
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint(reverse), func(t *testing.T) {
			t.Parallel()
			var first firstStressError
			if first.Load() != nil {
				t.Fatal("zero-value error holder must be empty")
			}
			first.Record(nil)
			errors := []error{context.DeadlineExceeded, fmt.Errorf("commit failed: %w", context.Canceled)}
			if reverse {
				errors[0], errors[1] = errors[1], errors[0]
			}
			first.Record(errors[0])
			first.Record(errors[1])
			first.Record(nil)
			if got := first.Load(); got != errors[0] {
				t.Fatalf("first worker failure changed: got %v, want %v", got, errors[0])
			}
		})
	}
}

func TestFirstStressErrorConcurrentMixedTypes(t *testing.T) {
	t.Parallel()
	var first firstStressError
	want := fmt.Errorf("initial commit failure: %w", context.DeadlineExceeded)
	first.Record(want)
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() {
			first.Record(context.Canceled)
			first.Record(fmt.Errorf("later commit failure: %w", context.DeadlineExceeded))
			if got := first.Load(); got != want {
				t.Errorf("concurrent failure replaced the first error: got %v, want %v", got, want)
			}
		})
	}
	workers.Wait()
}
