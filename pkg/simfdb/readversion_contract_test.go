package simfdb

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/dst"
	fdb "fdb.dev/pkg/fdbgo/fdb"
)

func readVersionCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	return errCode(t, err)
}

func TestEmptyDatabaseReadVersionCanBeBorrowed(t *testing.T) {
	t.Parallel()
	for _, env := range []*dst.Env{nil, {}, dst.NewSim(17)} {
		db := New(env)
		parent := db.newTxn()
		rv, err := parent.GetReadVersion().Get()
		if err != nil || rv != 1 {
			t.Fatalf("empty GRV = %d, %v; want 1, nil", rv, err)
		}
		child := db.newTxn()
		child.SetReadVersion(rv)
		if got, err := child.Get(k("x")).Get(); err != nil || got != nil {
			t.Fatalf("borrowed empty snapshot = %q, %v; want absent, nil", got, err)
		}
	}
}

func TestEmptySnapshotPrecedesFirstCommit(t *testing.T) {
	t.Parallel()
	db := New(nil)
	parent := db.newTxn()
	rv := parent.GetReadVersion().MustGet()
	child := db.newTxn()
	child.SetReadVersion(rv)
	child.Get(k("x")).MustGet()
	writer := db.newTxn()
	writer.Set(k("x"), []byte("first"))
	if err := writer.Commit().Get(); err != nil {
		t.Fatal(err)
	}
	if cv, err := writer.GetCommittedVersion(); err != nil || cv != 2 {
		t.Fatalf("first commit = %d, %v; want 2, nil", cv, err)
	}
	if got := child.Get(k("x")).MustGet(); got != nil {
		t.Fatalf("empty snapshot observed later first commit: %q", got)
	}
	child.Set(k("y"), []byte("write skew"))
	if code := readVersionCode(t, child.Commit().Get()); code != 1020 {
		t.Fatalf("empty-snapshot conflict = %d, want 1020", code)
	}
	fresh := db.newTxn()
	if rv := fresh.GetReadVersion().MustGet(); rv != 2 {
		t.Fatalf("fresh GRV = %d, want 2", rv)
	}
	if got := fresh.Get(k("x")).MustGet(); string(got) != "first" {
		t.Fatalf("fresh snapshot = %q, want first", got)
	}
}

func TestSetReadVersionContract(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		setup    func(*simTxn)
		versions []int64
		want     int
		pin      int64
	}{
		{"zero", nil, []int64{0}, 2011, 0},
		{"negative", nil, []int64{-1}, 2011, 0},
		{"already_set_same", nil, []int64{123, 123}, 2010, 123},
		{"already_set_different", nil, []int64{123, 456}, 2010, 123},
		{"already_set_before_invalid", nil, []int64{123, 0}, 2010, 123},
		{"GRV_before_invalid", func(tx *simTxn) { tx.GetReadVersion().MustGet() }, []int64{-1}, 2010, -1},
		{"read_before_setter", func(tx *simTxn) { tx.Get(k("x")).MustGet() }, []int64{123}, 2010, -1},
		{"sticky_after_prior_error", nil, []int64{0, 123, -1}, 2011, 0},
		{"Reset_clears", func(tx *simTxn) { tx.SetReadVersion(0); tx.Reset() }, []int64{123}, 0, 123},
		{"valid", nil, []int64{123}, 0, 123},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tx := New(nil).newTxn()
			if tc.setup != nil {
				tc.setup(tx)
			}
			before, instant := tx.readVersion, tx.rvInstant
			for _, version := range tc.versions {
				tx.SetReadVersion(version)
			}
			got, err := tx.GetReadVersion().Get()
			if code := readVersionCode(t, err); code != tc.want {
				t.Fatalf("GetReadVersion code = %d, want %d", code, tc.want)
			}
			pin := tc.pin
			if pin == -1 {
				pin = before
			}
			if tx.rvSet != (pin > 0) {
				t.Fatalf("rvSet = %t, want %t", tx.rvSet, pin > 0)
			}
			if tx.readVersion != pin {
				t.Fatalf("cached version = %d, want %d", tx.readVersion, pin)
			}
			if !tx.rvInstant.Equal(instant) {
				t.Fatalf("setter changed observation instant: %v -> %v", instant, tx.rvInstant)
			}
			if tc.want == 0 && got != pin {
				t.Fatalf("getter = %d, want %d", got, pin)
			}
		})
	}
}

func TestOnErrorSuccessContract(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
			tx := New(nil).newTxn()
			if err := tx.Options().SetRetryLimit(tc.limit); err != nil {
				t.Fatal(err)
			}
			tx.SetReadVersion(123)
			if tc.cancel {
				tx.Cancel()
			}
			if code := readVersionCode(t, tx.OnError(fdb.Error{Code: 0}).Get()); code != tc.want {
				t.Fatalf("OnError(0) = %d, want %d", code, tc.want)
			}
			if tx.readVersion != 123 || tx.retryCount != 0 {
				t.Fatal("terminal OnError reset state or granted a retry")
			}
		})
	}
}

func TestExplicitReadVersionGetterDoesNotValidate(t *testing.T) {
	t.Parallel()
	for _, pin := range []int64{1, 1 << 62} {
		t.Run(fmt.Sprint(pin), func(t *testing.T) {
			t.Parallel()
			db, clk := simClockDB()
			clk.Advance(6 * time.Second)
			tx := db.newTxn()
			tx.SetReadVersion(pin)
			if got, err := tx.GetReadVersion().Get(); err != nil || got != pin {
				t.Fatalf("getter = %d, %v; want %d, nil", got, err, pin)
			}
			// SimFDB keeps snapshots indefinitely; it models age rejection at a writing commit.
			tx.Get(k("x")).MustGet()
			tx.Set(k("x"), []byte("v"))
			want := 0
			if pin == 1 {
				want = 1007
			}
			if code := readVersionCode(t, tx.Commit().Get()); code != want {
				t.Fatalf("commit = %d, want %d", code, want)
			}
		})
	}
}

func TestDeferredReadVersionErrorEntryPoints(t *testing.T) {
	t.Parallel()
	for _, cancelled := range []bool{false, true} {
		for _, tc := range []struct {
			name string
			op   func(*simTxn) error
		}{
			{"Get", func(tx *simTxn) error { _, err := tx.Get(k("x")).Get(); return err }},
			{"GetKey", func(tx *simTxn) error { _, err := tx.GetKey(fdb.FirstGreaterOrEqual(k("x"))).Get(); return err }},
			{"GetReadVersion", func(tx *simTxn) error { _, err := tx.GetReadVersion().Get(); return err }},
			{"exact_range", func(tx *simTxn) error {
				_, err := tx.GetRange(allRange(), fdb.RangeOptions{}).GetSliceWithError()
				return err
			}},
			{"selector_range", func(tx *simTxn) error {
				_, err := tx.GetRange(fdb.SelectorRange{Begin: fdb.LastLessThan(k("a")), End: fdb.FirstGreaterOrEqual(k("z"))}, fdb.RangeOptions{}).GetSliceWithError()
				return err
			}},
			{"range_iterator", func(tx *simTxn) error {
				it := tx.GetRange(allRange(), fdb.RangeOptions{}).Iterator()
				if it.Advance() {
					return fmt.Errorf("poisoned iterator advanced")
				}
				_, err := it.Get()
				return err
			}},
			{"snapshot_Get", func(tx *simTxn) error { _, err := tx.Snapshot().Get(k("x")).Get(); return err }},
			{"snapshot_GetKey", func(tx *simTxn) error {
				_, err := tx.Snapshot().GetKey(fdb.FirstGreaterOrEqual(k("x"))).Get()
				return err
			}},
			{"snapshot_GRV", func(tx *simTxn) error { _, err := tx.Snapshot().GetReadVersion().Get(); return err }},
			{"snapshot_estimated_size", func(tx *simTxn) error {
				_, err := tx.Snapshot().GetEstimatedRangeSizeBytes(allRange()).Get()
				return err
			}},
			{"snapshot_split_points", func(tx *simTxn) error { _, err := tx.Snapshot().GetRangeSplitPoints(allRange(), 100).Get(); return err }},
			{"snapshot_range", func(tx *simTxn) error {
				_, err := tx.Snapshot().GetRange(allRange(), fdb.RangeOptions{}).GetSliceWithError()
				return err
			}},
			{"size", func(tx *simTxn) error { _, err := tx.GetApproximateSize().Get(); return err }},
			{"estimated_size", func(tx *simTxn) error { _, err := tx.GetEstimatedRangeSizeBytes(allRange()).Get(); return err }},
			{"split_points", func(tx *simTxn) error { _, err := tx.GetRangeSplitPoints(allRange(), 100).Get(); return err }},
			{"versionstamp", func(tx *simTxn) error { _, err := tx.GetVersionstamp().Get(); return err }},
			{"Commit", func(tx *simTxn) error { return tx.Commit().Get() }},
		} {
			t.Run(fmt.Sprintf("%s/cancelled=%t", tc.name, cancelled), func(t *testing.T) {
				tx := New(nil).newTxn()
				tx.SetReadVersion(0)
				if cancelled {
					tx.Cancel()
				}
				if code := readVersionCode(t, tc.op(tx)); code != 2011 {
					t.Fatalf("deferred error = %d, want 2011", code)
				}
			})
		}
	}
}

func TestDeferredReadVersionErrorRangeAdmission(t *testing.T) {
	t.Parallel()
	for _, selectors := range []bool{false, true} {
		t.Run(fmt.Sprintf("selectors=%t", selectors), func(t *testing.T) {
			db := New(nil)
			value := strings.Repeat("v", 200)
			seed(db, "a", value, "b", value, "c", value, "d", value, "e", value)
			tx := db.newTxn()
			var r fdb.Range = allRange()
			if selectors {
				r = fdb.SelectorRange{Begin: fdb.LastLessThan(k("b")), End: fdb.FirstGreaterOrEqual(k("z"))}
			}
			result := tx.GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeSmall})
			tx.SetReadVersion(0)
			it := result.Iterator()
			if !it.Advance() {
				t.Fatal("late poison changed captured clean first batch")
			}
			for it.Advance() {
			}
			if _, err := it.Get(); readVersionCode(t, err) != 2010 {
				t.Fatalf("later range page bypassed deferred error: %v", err)
			}
		})
	}
}

func TestDeferredVersionstampErrorIsCapturedAtIssue(t *testing.T) {
	t.Parallel()
	tx := New(nil).newTxn()
	tx.SetReadVersion(0)
	future := tx.GetVersionstamp()
	if !future.IsReady() {
		t.Fatal("deferred-error versionstamp future is not ready")
	}
	tx.Reset()
	if _, err := future.Get(); readVersionCode(t, err) != 2011 {
		t.Fatalf("Reset changed issued versionstamp error: %v", err)
	}
}

func TestDeferredReadVersionErrorConstructionPrecedence(t *testing.T) {
	t.Parallel()
	tx := New(nil).newTxn()
	tx.SetReadVersion(0)
	tx.Cancel()
	inverted := fdb.KeyRange{Begin: k("z"), End: k("a")}
	if _, err := tx.GetEstimatedRangeSizeBytes(inverted).Get(); readVersionCode(t, err) != 2005 {
		t.Fatalf("inverted metric lost precedence: %v", err)
	}
	if _, err := tx.GetRangeSplitPoints(inverted, 100).Get(); readVersionCode(t, err) != 2005 {
		t.Fatalf("inverted split points lost precedence: %v", err)
	}
	if _, err := tx.GetRange(allRange(), fdb.RangeOptions{Mode: fdb.StreamingModeExact}).GetSliceWithError(); readVersionCode(t, err) != 2210 {
		t.Fatalf("invalid streaming limits lost precedence: %v", err)
	}
	if code := readVersionCode(t, tx.OnError(fdb.Error{Code: 1020}).Get()); code != 1025 {
		t.Fatalf("cancelled retry = %d, want 1025", code)
	}
}

func TestOnErrorTerminalAndDeferredLifetime(t *testing.T) {
	t.Parallel()
	for _, code := range []int{1031, 1235, 1242} {
		tx := New(nil).newTxn()
		tx.SetReadVersion(0)
		if got := readVersionCode(t, tx.OnError(fdb.Error{Code: code}).Get()); got != code {
			t.Fatalf("OnError(%d) = %d", code, got)
		}
		if _, err := tx.GetReadVersion().Get(); readVersionCode(t, err) != 2011 {
			t.Fatalf("terminal OnError cleared poison: %v", err)
		}
		if err := tx.OnError(fdb.Error{Code: 1020}).Get(); err != nil {
			t.Fatal(err)
		}
		tx.SetReadVersion(123)
		if got, err := tx.GetReadVersion().Get(); err != nil || got != 123 {
			t.Fatalf("retry did not clear poison: %d, %v", got, err)
		}
	}
}
