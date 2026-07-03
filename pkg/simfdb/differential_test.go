package simfdb_test

import (
	"context"
	"fmt"
	mrand "math/rand/v2"
	"os"
	"sync"
	"testing"
	"time"

	fdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/simfdb"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

// The pure-Go client on a real FDB container is the differential oracle. It is byte- and
// outcome-validated against libfdb_c elsewhere, so matching it is transitively matching the C
// client — without cgo. The container is created lazily (only when this test runs) so the rest
// of the SimFDB suite stays Docker-free.
var (
	realDBOnce sync.Once
	realDB     fdb.BackendDatabase
	realDBErr  error
)

func getRealDB(t *testing.T) fdb.BackendDatabase {
	t.Helper()
	realDBOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		container, err := foundationdbtc.Run(ctx, "", foundationdbtc.WithAPIVersion(730))
		if err != nil {
			realDBErr = err
			return
		}
		cf, err := container.ClusterFile(ctx)
		if err != nil {
			realDBErr = err
			return
		}
		tmp, err := os.CreateTemp("", "fdb_diff_*.txt")
		if err != nil {
			realDBErr = err
			return
		}
		if _, err := tmp.WriteString(cf); err != nil {
			realDBErr = err
			return
		}
		tmp.Close()
		fdb.MustAPIVersion(730)
		d, err := fdb.OpenDatabase(tmp.Name())
		if err != nil {
			realDBErr = err
			return
		}
		realDB = d
	})
	if realDBErr != nil || realDB == nil {
		t.Skipf("FDB not available (no Docker): %v", realDBErr)
	}
	return realDB
}

// conflictScenario describes a read shape + a concurrent probe write. It is applied identically
// to any fdb.BackendDatabase (SimFDB and the real pure-Go client) via the shared interface.
type conflictScenario struct {
	prefix   string
	readKind int // 0 = point Get, 1 = forward range, 2 = reverse range
	readKey  int // 0..9, for a point read
	limit    int // range row limit (0 = unlimited)
	probe    int // 0..9, the concurrently-written key
}

// runConflictScenario seeds k00..k09 (under the scenario's unique prefix), performs txnA's read,
// commits a concurrent probe write to k<probe>, then commits txnA (writing a disjoint key).
// Returns whether txnA committed. The outcome is a pure function of whether the probe fell in
// txnA's read-conflict set — the SSI semantics under test.
func runConflictScenario(t *testing.T, db fdb.BackendDatabase, s conflictScenario) bool {
	t.Helper()
	kk := func(i int) fdb.Key { return fdb.Key(fmt.Sprintf("%sk%02d", s.prefix, i)) }
	fullRange := fdb.KeyRange{Begin: kk(0), End: kk(10)}

	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		for i := 0; i < 10; i++ {
			tx.Set(kk(i), []byte{byte(i)})
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	txA, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create txA: %v", err)
	}
	switch s.readKind {
	case 0:
		txA.Get(kk(s.readKey)).MustGet()
	case 1:
		txA.GetRange(fullRange, fdb.RangeOptions{Limit: s.limit}).GetSliceOrPanic()
	case 2:
		txA.GetRange(fullRange, fdb.RangeOptions{Reverse: true, Limit: s.limit}).GetSliceOrPanic()
	}

	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(kk(s.probe), []byte{0xEE})
		return nil, nil
	}); err != nil {
		t.Fatalf("probe commit: %v", err)
	}

	txA.Set(kk(50), []byte("x")) // disjoint write, outside [k00,k10)
	return txA.Commit().Get() == nil
}

// TestSimFDB_DifferentialConflictOutcome is RFC-179 Tier 1's differential oracle: for hundreds of
// randomized conflict scenarios, SimFDB's commit/abort outcome must equal the pure-Go client's on
// a real FDB cluster. A divergence is the under-/over-conflict class the differential exists to
// catch (it caught the RFC-121 conflict-set bugs in its go-vs-cgo form). Because the pure-Go
// client is outcome-validated against libfdb_c, this transitively checks SimFDB against the C
// client without linking cgo.
func TestSimFDB_DifferentialConflictOutcome(t *testing.T) {
	t.Parallel()
	real := getRealDB(t)
	rng := mrand.New(mrand.NewPCG(1, 0))

	const scenarios = 200
	for i := 0; i < scenarios; i++ {
		s := conflictScenario{
			prefix:   fmt.Sprintf("diff/%d/%d/", os.Getpid(), i),
			readKind: rng.IntN(3),
			readKey:  rng.IntN(10),
			limit:    rng.IntN(11), // 0..10 (0 = unlimited)
			probe:    rng.IntN(10),
		}
		// SimFDB gets a fresh isolated store per scenario; the real cluster is isolated by the
		// per-scenario key prefix.
		simOut := runConflictScenario(t, simfdb.New(nil), s)
		realOut := runConflictScenario(t, real, s)
		if simOut != realOut {
			t.Fatalf("scenario %d %+v: SimFDB committed=%v, real FDB committed=%v — SSI conflict-outcome divergence",
				i, s, simOut, realOut)
		}
	}
}
