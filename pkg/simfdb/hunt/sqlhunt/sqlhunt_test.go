package sqlhunt

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/simfdb"

	"fdb.dev/pkg/simfdb/hunt"
)

func smokeCfg() hunt.Config {
	return hunt.Config{Workload: SQLWorkload{}, NumOps: 30, MaxPKs: 12, VerifyEvery: 10, FaultProb: 0.3}
}

// TestSQLWorkloadRunsClean drives the full SQL stack over SimFDB under faults for a few seeds
// and asserts every one is clean — idempotent DML (absolute UPDATE + DELETE) survives
// commit_unknown/conflict/too_old application retry with no row/aggregate drift.
func TestSQLWorkloadRunsClean(t *testing.T) {
	t.Parallel()
	cfg := smokeCfg()
	for seed := uint64(0); seed < 4; seed++ {
		if rep := hunt.Run(seed, cfg); rep.Failed() {
			t.Fatalf("sql-dml seed %d found a bug:\n%s", seed, rep)
		}
	}
}

// TestSQLWorkloadDeterminism pins that the SQL persist path is deterministic: the same seed
// yields byte-identical state and the same fault schedule. A mismatch would mean nondeterminism
// (plan-order, map iteration, a clock/rand leak) has crept into the relational stack.
func TestSQLWorkloadDeterminism(t *testing.T) {
	t.Parallel()
	cfg := smokeCfg()
	faultsFired := 0
	for _, seed := range []uint64{1, 7, 42} {
		a := hunt.Run(seed, cfg)
		b := hunt.Run(seed, cfg)
		if a.Failed() || b.Failed() {
			t.Fatalf("seed %d unexpectedly failed:\n%s\n%s", seed, a, b)
		}
		if a.Fingerprint != b.Fingerprint {
			t.Fatalf("seed %d nondeterministic: fingerprint %s != %s", seed, a.Fingerprint, b.Fingerprint)
		}
		faultsFired += a.FaultsFired
		if a.FaultsFired != b.FaultsFired {
			t.Fatalf("seed %d nondeterministic fault schedule: %d != %d", seed, a.FaultsFired, b.FaultsFired)
		}
	}
	// The per-site activation gate can legitimately leave one seed fault-free;
	// the complete retained population must exercise the schedule.
	if faultsFired == 0 {
		t.Fatal("determinism population did not exercise the fault schedule")
	}
}

// TestSQLOracleHasTeeth proves the row-model oracle catches a store/model divergence over the
// SQL stack — otherwise a clean sweep would be meaningless.
func TestSQLOracleHasTeeth(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, err := newHarness(5, 0) // faults off; we corrupt by hand
	if err != nil {
		t.Fatalf("newHarness: %v", err)
	}
	defer h.close()

	model := map[int64]int64{}
	for id := int64(0); id < 5; id++ {
		if _, err := h.db.ExecContext(ctx, "INSERT INTO t (id, a) VALUES (?, ?)", id, id*10); err != nil {
			t.Fatalf("insert id=%d: %v", id, err)
		}
		model[id] = id * 10
	}
	if v := verify(ctx, h.db, model); len(v) > 0 {
		t.Fatalf("clean state should verify, got: %v", v)
	}

	// Untracked UPDATE: the store changes but the model does not → the oracle must fire.
	if _, err := h.db.ExecContext(ctx, "UPDATE t SET a = ? WHERE id = ?", int64(999), int64(2)); err != nil {
		t.Fatalf("untracked update: %v", err)
	}
	if v := verify(ctx, h.db, model); len(v) == 0 {
		t.Fatal("oracle has no teeth: an untracked row change produced no violation")
	}
}

// TestProfiles sanity-checks the exported profile set.
func TestProfiles(t *testing.T) {
	t.Parallel()
	ps := Profiles()
	if len(ps) == 0 {
		t.Fatal("no SQL profiles")
	}
	for _, p := range ps {
		if p.Cfg.Workload == nil || p.Cfg.Workload.Name() != "sql-dml" {
			t.Fatalf("profile %s: unexpected workload", p.Name)
		}
	}
}

func TestIdempotentRetryPolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"success", nil, false},
		{"conflict", api.WrapError("40001", "conflict", fdb.Error{Code: 1020}), true},
		{"too old pointer", api.WrapError("40001", "too old", &fdb.Error{Code: 1007}), true},
		{"marked FDB window", api.MarkFDBTransactionTooOld(fdb.Error{Code: 1007}), true},
		{"ambiguity", fmt.Errorf("wrapped: %w", api.WrapError("40003", "unknown", fdb.Error{Code: 1021})), true},
		{"ambiguity pointer", api.WrapError("40003", "unknown", &fdb.Error{Code: 1021}), true},
		{"bare conflict state", api.NewError("40001", "conflict"), false},
		{"bare ambiguity state", api.NewError("40003", "unknown"), false},
		{"bare FDB conflict", fdb.Error{Code: 1020}, false},
		{"bare FDB ambiguity", &fdb.Error{Code: 1021}, false},
		{"untyped FDB window", api.MarkFDBTransactionTooOld(errors.New("injected 1007")), false},
		{"driver window", api.NewTransactionTimeLimitError(5, 4), false},
		{"wrong conflict cause", api.WrapError("40001", "conflict", fdb.Error{Code: 1021}), false},
		{"wrong ambiguity cause", api.WrapError("40003", "unknown", fdb.Error{Code: 1020}), false},
		{"other FDB cause", api.WrapError("40001", "timeout", fdb.Error{Code: 1031}), false},
		{"domain with FDB cause", api.WrapError("23505", "duplicate", fdb.Error{Code: 1020}), false},
		{"joined sibling cause", errors.Join(api.NewError("40003", "unknown"), fdb.Error{Code: 1021}), false},
		{"joined cancellation", errors.Join(api.WrapError("40003", "unknown", fdb.Error{Code: 1021}), context.Canceled), false},
		{"joined nested cause", api.WrapError("40001", "conflict", errors.Join(fdb.Error{Code: 1020}, errors.New("other failure"))), false},
		{"constraint", api.NewError("23505", "duplicate"), false},
		{"execution limit", api.NewError("54F01", "limit"), false},
		{"cancellation", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"untyped", errors.New("40003 is not an error code channel"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := retryableIdempotentDMLError(tc.err); got != tc.want {
				t.Fatalf("retry %v = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIdempotentDMLRetryIsApplicationOwned(t *testing.T) {
	t.Parallel()
	for _, fault := range []struct {
		name    string
		code    int
		state   api.ErrorCode
		applied bool
	}{
		{"conflict", 1020, "40001", false},
		{"too old", 1007, "40001", false},
		{"unknown applied", simfdb.CommitUnknownApplied, "40003", true},
		{"unknown discarded", simfdb.CommitUnknownDiscarded, "40003", false},
	} {
		t.Run(fault.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			h, err := newHarness(712, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer h.close()
			if _, err := h.db.ExecContext(ctx, "INSERT INTO t VALUES (1,10)"); err != nil {
				t.Fatal(err)
			}
			// A direct SQL call must surface the fault. This control kills a
			// driver retry that would otherwise make the application helper moot.
			h.backend.InjectOnce(fault.code)
			_, err = h.db.ExecContext(ctx, "UPDATE t SET a = 20 WHERE id = 1")
			var sqlErr *api.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != fault.state {
				t.Fatalf("direct statement = %v, want %s without driver replay", err, fault.state)
			}
			want := int64(10)
			if fault.applied {
				want = 20
			}
			if violations := verify(ctx, h.db, map[int64]int64{1: want}); len(violations) != 0 {
				t.Fatal(violations)
			}
			h.backend.InjectOnce(fault.code)
			if err := execIdempotentDML(ctx, h.db, "UPDATE t SET a = ? WHERE id = ?", int64(30), int64(1)); err != nil {
				t.Fatal(err)
			}
			if violations := verify(ctx, h.db, map[int64]int64{1: 30}); len(violations) != 0 {
				t.Fatal(violations)
			}
			h.backend.InjectOnce(fault.code)
			if err := execIdempotentDML(ctx, h.db, "DELETE FROM t WHERE id = ?", int64(1)); err != nil {
				t.Fatal(err)
			}
			if violations := verify(ctx, h.db, map[int64]int64{}); len(violations) != 0 {
				t.Fatal(violations)
			}
		})
	}
}

func TestIdempotentDMLRetryStopsAtItsBudget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, err := newHarness(713, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	if _, err := h.db.ExecContext(ctx, "INSERT INTO t VALUES (1,10)"); err != nil {
		t.Fatal(err)
	}
	// One initial attempt plus 100 retries, all definitely discarded.
	faults := make([]int, 101)
	for i := range faults {
		faults[i] = 1020
	}
	h.backend.InjectSequence(faults...)
	err = execIdempotentDML(ctx, h.db, "UPDATE t SET a = 20 WHERE id = 1")
	var sqlErr *api.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != "40001" {
		t.Fatalf("retry exhaustion = %v, want a surfaced 40001, not success", err)
	}
	if violations := verify(ctx, h.db, map[int64]int64{1: 10}); len(violations) != 0 {
		t.Fatal(violations)
	}
	// No fault remains: a short retry loop would leave a scheduled conflict.
	if _, err := h.db.ExecContext(ctx, "UPDATE t SET a = 30 WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := execIdempotentDML(canceled, h.db, "DELETE FROM t WHERE id = 1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled retry = %v, want context.Canceled", err)
	}
	if violations := verify(ctx, h.db, map[int64]int64{1: 30}); len(violations) != 0 {
		t.Fatal(violations)
	}
}
