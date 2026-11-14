package simfdb_test

import (
	"errors"
	"fmt"
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

func TestSimFDB_DifferentialTerminalOnErrorLifetime(t *testing.T) {
	t.Parallel()
	real := getRealDB(t)
	for _, tc := range []struct {
		name   string
		code   int
		limit  int64
		cancel bool
		want   int
	}{
		{"timeout", 1031, -1, false, 1031},
		{"hot_shard", 1235, -1, false, 1235},
		{"range_locked", 1242, -1, false, 1242},
		{"non_retryable", 2101, -1, false, 2101},
		{"exhausted", 1020, 0, false, 1020},
		{"success_unlimited", 0, -1, false, 2000},
		{"success_exhausted", 0, 0, false, 4100},
		{"cancelled", 1020, -1, true, 1025},
	} {
		for _, poison := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/poison=%t", tc.name, poison), func(t *testing.T) {
				for name, db := range map[string]fdb.BackendDatabase{"real": real, "sim": simfdb.New(nil)} {
					tx, err := db.CreateWritableTransaction()
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Cancel()
					key := fdb.Key(t.Name() + "/" + name)
					r := fdb.KeyRange{Begin: key, End: fdb.Key(string(key) + "\x00")}
					if err := tx.Options().SetRetryLimit(tc.limit); err != nil {
						t.Fatal(err)
					}
					tx.Set(key, []byte("pending"))
					if poison {
						tx.SetReadVersion(0)
					}
					if tc.cancel {
						tx.Cancel()
					}
					if got := contractCode(t, tx.OnError(fdb.Error{Code: tc.code}).Get()); got != tc.want {
						t.Fatalf("%s: terminal OnError = %d, want %d", name, got, tc.want)
					}
					if err := tx.Options().SetRetryLimit(-1); err != nil {
						t.Fatal(err)
					}
					if got := contractCode(t, tx.OnError(fdb.Error{Code: 1020}).Get()); got != 1025 {
						t.Fatalf("%s: later OnError = %d, want 1025", name, got)
					}
					want := 1025
					if poison {
						want = 2011
					}
					for _, op := range []struct {
						name string
						run  func() error
					}{
						{"Get", func() error { _, err := tx.Get(key).Get(); return err }},
						{"GRV", func() error { _, err := tx.GetReadVersion().Get(); return err }},
						{"range", func() error { _, err := tx.GetRange(r, fdb.RangeOptions{}).GetSliceWithError(); return err }},
						{"snapshot", func() error { _, err := tx.Snapshot().Get(key).Get(); return err }},
						{"versionstamp", func() error { _, err := tx.GetVersionstamp().Get(); return err }},
						{"commit", func() error { return tx.Commit().Get() }},
					} {
						if got := contractCode(t, op.run()); got != want {
							t.Fatalf("%s: %s = %d, want %d", name, op.name, got, want)
						}
					}
					_, err = tx.GetApproximateSize().Get()
					sizeWant := 0
					if poison {
						sizeWant = 2011
					}
					if got := contractCode(t, err); got != sizeWant {
						t.Fatalf("%s: approximate size = %d, want %d", name, got, sizeWant)
					}
					tx.Reset()
					if err := tx.OnError(fdb.Error{Code: 1020}).Get(); err != nil {
						t.Fatalf("%s: retry after Reset: %v", name, err)
					}
					if got, err := tx.Get(key).Get(); err != nil || got != nil {
						t.Fatalf("%s: read after Reset = %q, %v", name, got, err)
					}
					if err := tx.Commit().Get(); err != nil {
						t.Fatalf("%s: commit after Reset: %v", name, err)
					}
				}
			})
		}
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
