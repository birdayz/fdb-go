package hunt

import (
	"context"
	"strings"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/simfdb"
)

// exhaustionDriver is a fault-free driver whose faults the test schedules
// itself, with one saved record (pk 1) to overwrite.
func exhaustionDriver(t *testing.T, wrap func(*simfdb.SimDB) fdb.Transactor) (*driver, *simfdb.SimDB) {
	t.Helper()
	env := NewSimEnv(11, 0)
	backend := simfdb.New(env)
	var db *recordlayer.FDBDatabase
	if wrap == nil {
		db = recordlayer.NewFDBDatabaseWithBackend(backend).SetEnv(env)
	} else {
		db = recordlayer.NewFDBDatabaseWithTransactor(wrap(backend), fdb.Database{}).SetEnv(env)
	}
	cleanEnv := &dst.Env{Clock: env.Clock, Random: env.Random, Buggify: dst.DisabledBuggifier()}
	cleanDB := recordlayer.NewFDBDatabaseWithBackend(backend).SetEnv(cleanEnv)
	d := newDriver(backend, db, cleanDB, KitchenSinkMetadata())
	// The setup save goes through the clean view, so a wrapping transactor
	// only sees the operation under test.
	d.db = cleanDB
	if err := d.save(context.Background(), 1, 100, 1); err != nil {
		t.Fatalf("setup save: %v", err)
	}
	d.db = db
	return d, backend
}

func repeat(code, n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = code
	}
	return out
}

// TestExhaustion_ReconcilesToThePredictedSide pins the hunt's exhaustion
// reconcile: an operation whose faults outlast Run's ten attempts is checked
// against the side the attempts predict, and the model takes that side.
func TestExhaustion_ReconcilesToThePredictedSide(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		faults []int
		price  int32 // the price pk 1 must hold afterwards
	}{
		{"applied 1021 last", append(repeat(1020, 9), simfdb.CommitUnknownApplied), 200},
		{"discarded 1021 last", append(repeat(1020, 9), simfdb.CommitUnknownDiscarded), 100},
		{"all 1020", repeat(1020, 10), 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, backend := exhaustionDriver(t, nil)
			backend.InjectSequence(tc.faults...)
			if err := d.save(context.Background(), 1, 200, 1); err != nil {
				t.Fatalf("exhausted save did not reconcile: %v", err)
			}
			if d.exhausted != 1 {
				t.Fatalf("exhausted = %d, want 1", d.exhausted)
			}
			if v := d.verify(context.Background()); len(v) > 0 {
				t.Fatalf("model and store disagree after the reconcile: %v", v)
			}
			if got := modelPrice(d, 1); got != tc.price {
				t.Fatalf("model price = %d, want %d", got, tc.price)
			}
		})
	}
}

// lyingTransactor commits every attempt's writes and then reports a 1021 that
// the simulator never issued, so the reconcile predicts "not applied" over a
// store that took the write: a store that matches no prediction.
type lyingTransactor struct{ inner *simfdb.SimDB }

func (l *lyingTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	if _, err := l.inner.Transact(fn); err != nil {
		return nil, err
	}
	return nil, fdb.Error{Code: 1021}
}

func (l *lyingTransactor) ReadTransact(fn func(fdb.ReadTransaction) (any, error)) (any, error) {
	return l.inner.ReadTransact(fn)
}

// TestExhaustion_AStoreMatchingNoPredictionFails is the fixture that shows the
// reconcile can go red: it must name the mismatch, not resync the model.
func TestExhaustion_AStoreMatchingNoPredictionFails(t *testing.T) {
	t.Parallel()
	d, _ := exhaustionDriver(t, func(b *simfdb.SimDB) fdb.Transactor { return &lyingTransactor{inner: b} })
	err := d.save(context.Background(), 1, 200, 1)
	if err == nil || !strings.Contains(err.Error(), "does not match the model from before") {
		t.Fatalf("save over a store matching no prediction returned %v; want the reconcile to fail", err)
	}
}

func modelPrice(d *driver, pk int64) int32 {
	for _, r := range d.model.Records {
		if r.PrimaryKey[0] == pk {
			return r.Message.(*gen.Order).GetPrice()
		}
	}
	return -1
}
