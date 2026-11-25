package transport

import (
	"context"
	"testing"
	"testing/synctest"
)

func TestSuspendHandoff(t *testing.T) {
	for _, release := range []string{"resume", "cancel"} {
		t.Run(release, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				resume := make(chan struct{})
				ctx, signal := WithSuspendHandoff(ctx, resume, nil)
				defer signal.Fire()
				continued := make(chan struct{})
				go func() { NotifySuspend(ctx); close(continued) }()
				synctest.Wait()
				select {
				case <-signal.Done():
				default:
					t.Fatal("handoff did not publish the first suspension")
				}
				select {
				case <-continued:
					t.Fatal("worker resumed before the startup handoff")
				default:
				}
				if release == "resume" {
					close(resume)
				} else {
					cancel()
				}
				synctest.Wait()
				select {
				case <-continued:
				default:
					t.Fatal("resume/cancellation stranded the notifying worker")
				}
			})
		})
	}
}

func TestSuspendHandoffObserverOnlyForUnreadyGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		_, sentinel := WithSuspendSignal(ctx)
		defer sentinel.Fire()
		resume := make(chan struct{})
		var waits int
		ctx, signal := WithSuspendHandoff(ctx, resume, func() { waits++ })
		defer signal.Fire()
		go NotifySuspend(ctx)
		synctest.Wait()
		if waits != 1 {
			t.Fatalf("unready gate observations = %d, want 1", waits)
		}
		close(resume)
		synctest.Wait()
		NotifySuspend(ctx)
		if waits != 1 {
			t.Fatalf("ready gate invoked its observer: %d observations", waits)
		}
	})
}

func TestSuspendHandoffCompletionDoesNotWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		resume := make(chan struct{})
		ctx, signal := WithSuspendHandoff(ctx, resume, nil)
		signal.Fire() // completion/panic cleanup publishes without joining the gate
		select {
		case <-signal.Done():
		default:
			t.Fatal("completion did not publish startup")
		}
		cancel()
		continued := make(chan struct{})
		go func() { NotifySuspend(ctx); close(continued) }()
		synctest.Wait()
		select {
		case <-continued:
		default:
			t.Fatal("already-canceled worker waited for an unreleased startup gate")
		}
	})
}
