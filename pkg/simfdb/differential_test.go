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

const nKeys = 20

// conflictScenario is a randomized read + a concurrent probe write over a SPARSE keyspace
// (k00..k19, each seeded with ~50% probability so gaps and entirely-empty sub-ranges occur). It
// is applied identically to any fdb.BackendDatabase (SimFDB and the real pure-Go client). The
// sparse seed + arbitrary read range + arbitrary probe deliberately exercise the empty-range /
// leading-gap / trailing-gap axis where a naive clamp-to-returned-data implementation
// under-conflicts (write-skew / phantom protection).
type conflictScenario struct {
	prefix   string
	seeded   [nKeys]bool
	readMode int // 0 = point Get, 1 = GetKey, 2 = range
	readPt   int // point / GetKey base key
	selKind  int // GetKey selector: 0 = FGE, 1 = FGT, 2 = LLE, 3 = LLT
	readLo   int // range read [readLo, readHi)
	readHi   int
	limit    int // 0 = unlimited
	reverse  bool
	probe    int // the concurrently-written key
}

// runConflictScenario seeds the masked keys, performs txnA's read, commits a concurrent probe
// write, then commits txnA (writing a disjoint key). Returns whether txnA committed — the SSI
// outcome under test.
func runConflictScenario(t *testing.T, db fdb.BackendDatabase, s conflictScenario) bool {
	t.Helper()
	kk := func(i int) fdb.Key { return fdb.Key(fmt.Sprintf("%sk%02d", s.prefix, i)) }

	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		for i := 0; i < nKeys; i++ {
			if s.seeded[i] {
				tx.Set(kk(i), []byte{byte(i)})
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	txA, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create txA: %v", err)
	}
	switch s.readMode {
	case 0:
		txA.Get(kk(s.readPt)).MustGet()
	case 1:
		base := kk(s.readPt)
		var sel fdb.KeySelector
		switch s.selKind {
		case 0:
			sel = fdb.FirstGreaterOrEqual(base)
		case 1:
			sel = fdb.FirstGreaterThan(base)
		case 2:
			sel = fdb.LastLessOrEqual(base)
		default:
			sel = fdb.LastLessThan(base)
		}
		txA.GetKey(sel).MustGet()
	default:
		txA.GetRange(fdb.KeyRange{Begin: kk(s.readLo), End: kk(s.readHi)},
			fdb.RangeOptions{Limit: s.limit, Reverse: s.reverse}).GetSliceOrPanic()
	}

	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(kk(s.probe), []byte{0xEE})
		return nil, nil
	}); err != nil {
		t.Fatalf("probe commit: %v", err)
	}

	txA.Set(kk(nKeys+5), []byte("x")) // disjoint write, above the k00..k19 range
	return txA.Commit().Get() == nil
}

// TestSimFDB_DifferentialConflictOutcome is RFC-179 Tier 1's differential oracle: for hundreds of
// randomized conflict scenarios over a sparse keyspace, SimFDB's commit/abort outcome must equal
// the pure-Go client's on a real FDB cluster. The sparse seed + random range + random probe cover
// the empty-range / gap / phantom axis (not just dense in-range probes). A divergence is the
// under-/over-conflict class the differential exists to catch. Because the pure-Go client is
// outcome-validated against libfdb_c, this transitively checks SimFDB against the C client without
// linking cgo.
func TestSimFDB_DifferentialConflictOutcome(t *testing.T) {
	t.Parallel()
	real := getRealDB(t)
	rng := mrand.New(mrand.NewPCG(1, 0))

	const scenarios = 400
	for i := 0; i < scenarios; i++ {
		var seeded [nKeys]bool
		for j := 0; j < nKeys; j++ {
			seeded[j] = rng.IntN(2) == 0 // ~50% → sparse: gaps, sometimes empty sub-ranges
		}
		lo := rng.IntN(nKeys)
		hi := lo + 1 + rng.IntN(nKeys-lo) // (lo, nKeys]
		s := conflictScenario{
			prefix:   fmt.Sprintf("diff/%d/%d/", os.Getpid(), i),
			seeded:   seeded,
			readMode: rng.IntN(3), // point Get / GetKey / range
			readPt:   rng.IntN(nKeys),
			selKind:  rng.IntN(4),
			readLo:   lo,
			readHi:   hi,
			limit:    rng.IntN(6), // 0..5 (0 = unlimited)
			reverse:  rng.IntN(2) == 0,
			probe:    rng.IntN(nKeys),
		}
		simOut := runConflictScenario(t, simfdb.New(nil), s)
		realOut := runConflictScenario(t, real, s)
		if simOut != realOut {
			t.Fatalf("scenario %d %+v: SimFDB committed=%v, real FDB committed=%v — SSI conflict-outcome divergence",
				i, s, simOut, realOut)
		}
	}
}
