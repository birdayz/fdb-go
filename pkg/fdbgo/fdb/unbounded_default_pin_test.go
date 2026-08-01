package fdb

import (
	"testing"
)

// The fdb-layer half of the claim the package doc at fdb.dev/pkg/fdbgo makes: the
// no-context Transact / ReadTransact carry NO internal deadline. Together with the
// client-layer pins (client/unbounded_default_pin_test.go) this fixes the whole
// documented contract — nothing internal bounds a default transaction, so the caller
// must choose one of SetTimeout / SetRetryLimit / TransactCtx.
//
// The complementary bound — bootstrap IS internally capped at 60s, the one place this
// client is stricter than libfdb_c — is pinned separately in database_bootstrap_test.go.
// Both halves are load-bearing for the doc: the first says "we do not bound you", the
// second says "except at open".
func TestBareTransactHasNoInternalDeadline(t *testing.T) {
	t.Parallel()

	// WrapDatabase builds the same internalDB the Open* paths do; db.d.ctx is exactly
	// the context the no-arg Transact / ReadTransact hand to client.Database.Transact.
	db := WrapDatabase(nil)

	if db.d.ctx == nil {
		t.Fatal("the database's ongoing-operation context must be non-nil")
	}
	if dl, ok := db.d.ctx.Deadline(); ok {
		t.Fatalf("the no-context Transact path must carry NO internal deadline, got %v.\n"+
			"The package doc at fdb.dev/pkg/fdbgo tells migrators the default is unbounded "+
			"(matching libfdb_c's timeoutInSeconds=0.0 / maxRetries=-1, "+
			"ReadYourWrites.actor.cpp:2078-2082) and that they must supply a bound. "+
			"If an internal deadline is added here, update doc.go, key.go and database.go too.", dl)
	}
	if db.d.ctx.Done() != nil {
		t.Fatal("the no-context Transact path must use a context that never cancels " +
			"(context.Background()), mirroring the Apple Go binding")
	}
}
