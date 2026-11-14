package simfdb_test

import (
	"errors"
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/simfdb"
)

func TestSimFDB_DifferentialReadVersionContract(t *testing.T) {
	t.Parallel()
	real := getRealDB(t)
	for _, tc := range []struct {
		name     string
		setup    func(fdb.WritableTransaction)
		versions []int64
		want     int
	}{
		{"zero", nil, []int64{0}, 2011},
		{"negative", nil, []int64{-1}, 2011},
		{"same", nil, []int64{123, 123}, 2010},
		{"different", nil, []int64{123, 456}, 2010},
		{"existing_before_invalid", nil, []int64{123, 0}, 2010},
		{"GRV_before_invalid", func(tx fdb.WritableTransaction) { tx.GetReadVersion().MustGet() }, []int64{0}, 2010},
		{"sticky", nil, []int64{0, 123}, 2011},
		{"Reset", func(tx fdb.WritableTransaction) { tx.SetReadVersion(0); tx.Reset() }, []int64{123}, 0},
		{"stale_getter", nil, []int64{1}, 0},
		{"future_getter", nil, []int64{1 << 62}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for name, db := range map[string]fdb.BackendDatabase{"real": real, "sim": simfdb.New(nil)} {
				tx, err := db.CreateWritableTransaction()
				if err != nil {
					t.Fatal(err)
				}
				if tc.setup != nil {
					tc.setup(tx)
				}
				for _, version := range tc.versions {
					tx.SetReadVersion(version)
				}
				got, err := tx.GetReadVersion().Get()
				if code := contractCode(t, err); code != tc.want {
					t.Fatalf("%s: code %d, want %d", name, code, tc.want)
				}
				if tc.want == 0 && got != tc.versions[len(tc.versions)-1] {
					t.Fatalf("%s: getter changed explicit pin to %d", name, got)
				}
				tx.Cancel()
			}
		})
	}
}

func TestSimFDB_DifferentialOnErrorSuccessContract(t *testing.T) {
	t.Parallel()
	real := getRealDB(t)
	for _, tc := range []struct {
		name   string
		limit  int64
		cancel bool
		want   int
	}{
		{"unlimited", -1, false, 2000},
		{"exhausted", 0, false, 4100},
		{"available", 1, false, 2000},
		{"cancelled_exhausted", 0, true, 1025},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for name, db := range map[string]fdb.BackendDatabase{"real": real, "sim": simfdb.New(nil)} {
				tx, err := db.CreateWritableTransaction()
				if err != nil {
					t.Fatal(err)
				}
				if err := tx.Options().SetRetryLimit(tc.limit); err != nil {
					t.Fatal(err)
				}
				if tc.cancel {
					tx.Cancel()
				}
				if code := contractCode(t, tx.OnError(fdb.Error{Code: 0}).Get()); code != tc.want {
					t.Fatalf("%s: OnError(0) = %d, want %d", name, code, tc.want)
				}
				tx.Cancel()
			}
		})
	}
}

func contractCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var fe fdb.Error
	if !errors.As(err, &fe) {
		t.Fatalf("non-FDB error: %v", err)
	}
	return fe.Code
}
