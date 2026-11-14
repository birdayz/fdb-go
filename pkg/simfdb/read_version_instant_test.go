package simfdb_test

import (
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/simfdb"
)

func instantReporter(t *testing.T, tx fdb.WritableTransaction) fdb.ReadVersionInstantReporter {
	t.Helper()
	rep, ok := tx.(fdb.ReadVersionInstantReporter)
	if !ok {
		t.Fatalf("SimFDB transaction (%T) does not implement ReadVersionInstantReporter", tx)
	}
	return rep
}

func TestReadVersionInstant_ReportsTheGRVInstant(t *testing.T) {
	t.Parallel()
	env := dst.NewSim(4242)
	env.Buggify = dst.DisabledBuggifier()
	tx, err := simfdb.New(env).CreateWritableTransaction()
	if err != nil {
		t.Fatal(err)
	}
	rep := instantReporter(t, tx)
	if _, ok := rep.ReadVersionInstant(); ok {
		t.Fatal("unread transaction reported an instant")
	}
	if _, err := tx.Get(fdb.Key("anything")).Get(); err != nil {
		t.Fatal(err)
	}
	if got, ok := rep.ReadVersionInstant(); !ok || got.IsZero() || !got.Equal(env.Now()) {
		t.Fatalf("GRV instant = %v, %t; want %v, true", got, ok, env.Now())
	}
}

func TestReadVersionInstant_CallerSuppliedVersionHasNoAnchor(t *testing.T) {
	t.Parallel()
	env := dst.NewSim(4243)
	env.Buggify = dst.DisabledBuggifier()
	tx, err := simfdb.New(env).CreateWritableTransaction()
	if err != nil {
		t.Fatal(err)
	}
	rep := instantReporter(t, tx)
	if _, err := tx.Get(fdb.Key("anything")).Get(); err != nil {
		t.Fatal(err)
	}
	grvInstant, ok := rep.ReadVersionInstant()
	if !ok {
		t.Fatal("no instant after lazy GRV")
	}
	env.Clock.(*dst.SimClock).Advance(90 * time.Second)
	tx.SetReadVersion(12345)
	if got, ok := rep.ReadVersionInstant(); !ok || !got.Equal(grvInstant) {
		t.Fatalf("rejected setter changed instant: %v, %t; want %v, true", got, ok, grvInstant)
	}
	var fe fdb.Error
	if _, err := tx.GetReadVersion().Get(); !errors.As(err, &fe) || fe.Code != 2010 {
		t.Fatalf("setter after GRV: %v; want 2010", err)
	}
	// A new logical transaction can accept the pin, but has no observed anchor.
	tx.Reset()
	tx.SetReadVersion(12345)
	if got, err := tx.GetReadVersion().Get(); err != nil || got != 12345 {
		t.Fatalf("explicit pin = %d, %v; want 12345, nil", got, err)
	}
	if got, ok := rep.ReadVersionInstant(); ok || !got.IsZero() {
		t.Fatalf("explicit pin instant = %v, %t; want zero, false", got, ok)
	}
}
