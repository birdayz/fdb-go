package client

import (
	"context"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// C++ extractReadVersion waits for the GRV reply and only then checks the
// transaction's lock awareness (NativeAPI.actor.cpp:7405, 7425-7426), and both
// options stay settable meanwhile (:7072-7091). A public GRV whose locked reply
// arrives after SetLockAware or SetReadLockAware therefore succeeds.
func TestGRVLockAwarenessReadAtReply(t *testing.T) {
	for _, setter := range []string{"SetLockAware", "SetReadLockAware"} {
		t.Run(setter, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			db, gate, _ := newGRVReplyGateDB(t, ctx)
			tx := db.CreateTransaction()
			defer tx.Cancel()

			gate.arm(1, false)
			done := make(chan error, 1)
			go func() {
				_, err := tx.GetReadVersion(ctx)
				done <- err
			}()
			awaitGateSignal(t, gate.heldCh, "the GRV reply being withheld")
			switch setter {
			case "SetLockAware":
				tx.SetLockAware()
			default:
				tx.SetReadLockAware()
			}
			// Deliver the withheld reply as a locked database's.
			for _, r := range gate.takeHeld() {
				var rd wire.Reader
				if err := wire.ReadErrorOrInto(r.body, &rd); err != nil {
					t.Fatal(err)
				}
				var reply types.GetReadVersionReply
				if err := reply.UnmarshalFromReader(&rd); err != nil {
					t.Fatal(err)
				}
				reply.Locked = true
				if err := r.conn.forward(r.token, types.MarshalErrorOrValueGetReadVersionReply(&reply)); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("GRV_LOCK_AWARE_FROZEN: GetReadVersion = %v after %s before the locked reply", err, setter)
				}
			case <-ctx.Done():
				t.Fatal("GetReadVersion never returned")
			}
		})
	}
}
