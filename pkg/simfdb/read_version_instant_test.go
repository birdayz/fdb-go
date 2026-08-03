package simfdb_test

// The read-version instant must describe the CURRENT read version, or the
// budget built on it charges the wrong window.
//
// RFC-198 anchors an explicit transaction's time budget on the instant FDB's
// 5-second MVCC window opened, read through fdb.ReadVersionInstantReporter.
// The contract has two halves and only one of them is expressible as "has a
// read version": SetReadVersion supplies a version whose window opened on the
// cluster at an instant this process never observed, so there IS no anchor to
// report — ok must be false. The pure client says exactly this at
// client/transaction.go:2491-2497 and zeroes its stamp there deliberately.
//
// SimFDB is a backend of the same interface, so it owes the same contract. It
// did not: SetReadVersion set rvSet=true and left the PREVIOUS GRV's instant
// in place, so the reporter answered ok=true with the timestamp of a
// different version — an anchor that reads as a window still open. A budget
// anchored on it undercounts the transaction's age and lets a page start that
// FDB will refuse with 1007.
//
// Both directions are pinned below: a real GRV reports its instant, and a
// caller-supplied version reports nothing.

import (
	"testing"
	"time"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/simfdb"
)

// instantReporter is the capability under test, asserted rather than assumed:
// a backend that silently stopped implementing it would make every assertion
// below unreachable.
func instantReporter(t *testing.T, tx fdb.WritableTransaction) fdb.ReadVersionInstantReporter {
	t.Helper()
	rep, ok := tx.(fdb.ReadVersionInstantReporter)
	if !ok {
		t.Fatalf("SimFDB transaction (%T) does not implement ReadVersionInstantReporter: "+
			"the RFC-198 budget anchor has no source on this backend and every "+
			"assertion in this file is vacuous", tx)
	}
	return rep
}

func TestReadVersionInstant_ReportsTheGRVInstant(t *testing.T) {
	t.Parallel()
	env := dst.NewSim(4242)
	env.Buggify = dst.DisabledBuggifier()
	sim := simfdb.New(env)

	tx, err := sim.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create transaction: %v", err)
	}
	rep := instantReporter(t, tx)

	if _, ok := rep.ReadVersionInstant(); ok {
		t.Fatalf("a transaction that has not read yet reported an instant: " +
			"'not pinned' must not be mistakable for 'pinned at the zero time'")
	}

	// Any read fires the lazy GRV and stamps the instant on the env clock.
	if _, err := tx.Get(fdb.Key("anything")).Get(); err != nil {
		t.Fatalf("get: %v", err)
	}
	got, ok := rep.ReadVersionInstant()
	if !ok {
		t.Fatalf("after a read took the read version, no instant was reported: " +
			"the RFC-198 budget would fall back to a fresh statement-anchored " +
			"window and never pre-empt")
	}
	if got.IsZero() {
		t.Fatalf("reported instant is the zero time with ok=true — the exact " +
			"confusion the ok flag exists to prevent")
	}
}

func TestReadVersionInstant_CallerSuppliedVersionHasNoAnchor(t *testing.T) {
	t.Parallel()
	env := dst.NewSim(4243)
	env.Buggify = dst.DisabledBuggifier()
	sim := simfdb.New(env)

	tx, err := sim.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create transaction: %v", err)
	}
	rep := instantReporter(t, tx)

	// Take a REAL read version first, so there is a stamp to go stale. Without
	// this the test passes on a zero-valued field and proves nothing.
	if _, err := tx.Get(fdb.Key("anything")).Get(); err != nil {
		t.Fatalf("get: %v", err)
	}
	grvInstant, ok := rep.ReadVersionInstant()
	if !ok {
		t.Fatalf("no instant after the lazy GRV — see the sibling test")
	}

	// Move the clock so a stale stamp is unmistakable, then override the
	// version the way a caller replaying at a fixed version does.
	if sc, ok := env.Clock.(*dst.SimClock); ok {
		sc.Advance(90 * time.Second)
	} else {
		t.Fatalf("env.Clock is %T, want *dst.SimClock", env.Clock)
	}
	tx.SetReadVersion(12345)

	got, ok := rep.ReadVersionInstant()
	if ok {
		t.Fatalf("after SetReadVersion the reporter still answered ok=true with instant %v "+
			"(the GRV's own stamp was %v): the instant now describes a DIFFERENT read "+
			"version than the transaction holds. A budget anchored on it measures the "+
			"age of a window that already closed, so it under-counts and lets a page "+
			"start that FDB refuses with 1007. The pure client zeroes this stamp in "+
			"SetReadVersion (client/transaction.go:2491-2497) and SimFDB owes the same "+
			"contract", got, grvInstant)
	}
	if !got.IsZero() {
		t.Fatalf("reporter said ok=false but handed back a non-zero instant %v: callers "+
			"that ignore ok would read a stale anchor", got)
	}
}
