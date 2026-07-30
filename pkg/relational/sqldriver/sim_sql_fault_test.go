package sqldriver

// The two `commit_unknown_result` (1021) autocommit hazards, pinned deterministically.
//
// READ THESE AS A PROBLEM STATEMENT, NOT AS BUGS UNDER TEST. Both assert what the engine
// does TODAY, and today's behaviour matches Java: fdb-relational autocommits through the
// same FDBDatabase.run retry-on-1021, and 1021 is a retryable code. So these are inherited
// FDB hazards surfaced by composition, not Go-specific defects, and "fixing" them here
// would be inventing semantics. RFC-198 (an explicit transaction's reads belong to that
// transaction) is what decides whether an explicit transaction changes them — these tests
// are its inputs, and they are what will go red, loudly and on purpose, when it does.
//
// 1021 means the write IS durable but the client is told the outcome is unknown. Because
// the code is retryable and the statement is autocommitted inside a retrying Run, the whole
// statement re-executes against data that already reflects it. What that costs depends
// entirely on whether the statement is idempotent:
//
//   - a RELATIVE update (SET a = a + 1) re-derives its value from the now-durable row, so
//     +1 silently becomes +2. Wrong data, no error.
//   - an INSERT re-inserts and finds its own durable row, so the caller gets 23505
//     (duplicate key) for a statement that SUCCEEDED. Right data, wrong error.
//
// They are the two halves of one hazard and are pinned separately because a fix could
// plausibly address one and not the other.
//
// Determinism is the point: SimFDB's InjectOnce places the 1021 at an exact commit, so
// these reproduce identically every run instead of requiring a lucky cluster fault.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/simfdb"
)

// injectSimFDBWithHandle is injectSimFDB plus the *simfdb.SimDB itself, so a test can place a
// fault at an exact commit. Kept separate from injectSimFDB so the non-fault tests keep the
// narrower helper.
func injectSimFDBWithHandle(t *testing.T, seed uint64) (string, *simfdb.SimDB) {
	t.Helper()
	env := dst.NewSim(seed)
	env.Buggify = dst.DisabledBuggifier()
	sim := simfdb.New(env)
	simDB := recordlayer.NewFDBDatabaseWithBackend(sim).SetEnv(env)
	simDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())
	key := "sim://" + t.Name()
	fdbDBCache.Store(key, simDB)
	t.Cleanup(func() { fdbDBCache.Delete(key) })
	return key, sim
}

// openSimSchema builds a database + schema over a SimFDB backend and returns a connection
// bound to the schema, plus the SimDB handle for fault placement.
func openSimSchema(t *testing.T, seed uint64, tableDDL string) (*sql.DB, *simfdb.SimDB) {
	t.Helper()
	key, sim := injectSimFDBWithHandle(t, seed)
	ctx := context.Background()

	setup, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///simdb?cluster_file=%s", key))
	if err != nil {
		t.Fatalf("open setup: %v", err)
	}
	defer setup.Close()
	mustExecSQL(t, setup, ctx, "CREATE DATABASE /simdb")
	mustExecSQL(t, setup, ctx, "CREATE SCHEMA TEMPLATE tmpl "+tableDDL)
	mustExecSQL(t, setup, ctx, "CREATE SCHEMA /simdb/s WITH TEMPLATE tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///simdb?cluster_file=%s&schema=s", key))
	if err != nil {
		t.Fatalf("open query conn: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, sim
}

// TestSQLFault_UpdateRelative_DoubleApply pins the SILENT half of the hazard: a relative
// UPDATE under an injected 1021 applies TWICE, and the statement reports success.
//
// This is the dangerous one — there is no error anywhere for a caller to react to, and the
// stored value is simply wrong. RFC-198 changes the disposition; until then, pinning it is
// what keeps it from being rediscovered as a mystery.
func TestSQLFault_UpdateRelative_DoubleApply(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, sim := openSimSchema(t, 7,
		"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")

	mustExecSQL(t, db, ctx, "INSERT INTO t (id, a) VALUES (1, 100)")

	readA := func() int64 {
		var a int64
		if err := db.QueryRowContext(ctx, "SELECT a FROM t WHERE id = 1").Scan(&a); err != nil {
			t.Fatalf("read a: %v", err)
		}
		return a
	}
	if got := readA(); got != 100 {
		t.Fatalf("seed value a = %d, want 100", got)
	}

	// The next commit reports commit_unknown_result while applying durably.
	sim.InjectOnce(1021)

	// The statement SUCCEEDS from the caller's point of view: 1021 is retryable, the
	// autocommit Run retries, and the retry sees its own durable write.
	if _, err := db.ExecContext(ctx, "UPDATE t SET a = a + 1 WHERE id = 1"); err != nil {
		t.Fatalf("relative UPDATE under an injected 1021 returned an error (%v). The pinned "+
			"hazard is that it returns SUCCESS while applying twice; an error here means the "+
			"autocommit retry behaviour changed and RFC-198's input has moved", err)
	}

	got := readA()
	switch got {
	case 102:
		// The pinned hazard: +1 applied twice.
	case 101:
		t.Fatalf("relative UPDATE applied EXACTLY ONCE under an injected 1021 (a = 101). That is " +
			"the CORRECT result and this hazard is FIXED — the autocommit path became idempotent " +
			"for relative updates. Update RFC-198 and convert this pin into a correctness " +
			"assertion (want 101)")
	default:
		t.Fatalf("a = %d after a relative UPDATE under an injected 1021; expected the pinned "+
			"double-apply (102) or a fixed single apply (101). Neither means the retry semantics "+
			"changed in a way nobody has characterised", got)
	}

	// Data integrity otherwise holds: exactly one row, no duplicate.
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("COUNT(*) = %d, want 1 — the hazard is a double-APPLY, not a duplicate row", n)
	}
}

// TestSQLFault_InsertDurablyCommitted_Spurious23505 pins the LOUD half: an autocommit INSERT
// whose row committed durably under 1021 reports a duplicate-key error for a statement that
// succeeded.
//
// Data is intact — the row is present exactly once — so this is a wrong-error hazard, not a
// wrong-data one. It is the strictly safer failure of the two, and it is pinned separately
// because a fix for the relative-update case would not necessarily reach it.
func TestSQLFault_InsertDurablyCommitted_Spurious23505(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, sim := openSimSchema(t, 11,
		"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")

	sim.InjectOnce(1021)

	_, err := db.ExecContext(ctx, "INSERT INTO t (id, a) VALUES (1, 100)")
	if err == nil {
		t.Fatalf("INSERT under an injected 1021 SUCCEEDED. That is the CORRECT outcome and this " +
			"hazard is FIXED — the autocommit retry stopped surfacing its own durable row as a " +
			"conflict. Update RFC-198 and convert this pin into a correctness assertion (want no error)")
	}

	// The pinned shape: a duplicate-key (23505) class error.
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("INSERT under an injected 1021 failed with %v (%T), which is not an *api.Error. "+
			"The pinned hazard is a 23505 duplicate-key error for a statement that succeeded; a "+
			"different error type means the retry path changed", err, err)
	}
	if apiErr.Code != api.ErrCodeUniqueConstraintViolation {
		t.Fatalf("INSERT under an injected 1021 failed with code %s, want %s (23505). The pinned "+
			"hazard is specifically the spurious duplicate-key report",
			apiErr.Code, api.ErrCodeUniqueConstraintViolation)
	}

	// The row IS there, exactly once: the write was durable and the error is spurious. This is
	// the assertion that makes the hazard "wrong error" rather than "lost write".
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("COUNT(*) = %d, want 1 — the durably-committed row must be present exactly "+
			"once; anything else is a data-integrity failure, which is a REAL bug, not this pin", n)
	}
	var a int64
	if err := db.QueryRowContext(ctx, "SELECT a FROM t WHERE id = 1").Scan(&a); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if a != 100 {
		t.Fatalf("a = %d, want 100 — the durable row must carry the value the statement wrote", a)
	}
}

// TestSQLFault_1021HazardsAreDeterministic is the property that makes the two pins above
// usable as RFC-198 inputs at all: the injected fault places the hazard at an exact commit,
// so the outcome is identical on every run. A hazard that only reproduces on a lucky cluster
// fault cannot be the acceptance criterion for a semantics change.
func TestSQLFault_1021HazardsAreDeterministic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Same seed, same injection, repeated: the double-applied value must be identical.
	const runs = 3
	var seen []int64
	for r := 0; r < runs; r++ {
		func() {
			db, sim := openSimSchema(t, 7,
				"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")
			mustExecSQL(t, db, ctx, "INSERT INTO t (id, a) VALUES (1, 100)")
			sim.InjectOnce(1021)
			if _, err := db.ExecContext(ctx, "UPDATE t SET a = a + 1 WHERE id = 1"); err != nil {
				t.Fatalf("run %d: update: %v", r, err)
			}
			var a int64
			if err := db.QueryRowContext(ctx, "SELECT a FROM t WHERE id = 1").Scan(&a); err != nil {
				t.Fatalf("run %d: read: %v", r, err)
			}
			seen = append(seen, a)
		}()
	}
	for r, a := range seen {
		if a != seen[0] {
			t.Fatalf("the 1021 hazard is NOT deterministic: run %d gave a = %d, run 0 gave %d "+
				"(all runs: %v). Without determinism these pins cannot serve as RFC-198's "+
				"acceptance criterion", r, a, seen[0], seen)
		}
	}
	t.Logf("1021 relative-update hazard is deterministic across %d runs: a = %d every time", runs, seen[0])
}
