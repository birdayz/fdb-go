package simfdb_test

import (
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"os"
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/simfdb"
)

// The crossed differential arms.
//
// The original arm swept one axis at a time: a read shape, a probe key, an outcome. Every defect
// the spec review found lived in a CROSSING it never made — snapshot × concurrent write ×
// outcome, local write × read of the same key × concurrent writer, an atomic × a selector. An
// arm that varies one dimension while pinning the others cannot see a bug that needs two, and
// each of these was reachable in a handful of scenarios once the axes were crossed.
//
// Each arm runs the SAME program against SimFDB and against a real cluster through the pure-Go
// client, and compares the OUTCOME (committed / aborted) and, where the program reads, the ROWS.
// Divergence is the finding.

// armSeeds returns the (rotating, fixed) seed pair every arm runs: one to explore, one as the
// regression floor. See differentialSeed.
func armSeeds() []struct {
	name string
	seed uint64
	n    int
} {
	return []struct {
		name string
		seed uint64
		n    int
	}{
		{"rotating", differentialSeed(), 200},
		{"fixed", 7, 200},
	}
}

// armProgram is one scenario applied identically to both backends. It returns whether the
// transaction under test committed, plus any rows it read (compared when non-nil).
type armProgram func(t *testing.T, db fdb.BackendDatabase, prefix string) (committed bool, rows []string)

// runArm sweeps n scenarios of one arm across both backends and fails on the first divergence.
func runArm(t *testing.T, name string, seed uint64, n int, build func(rng *mrand.Rand) armProgram) {
	t.Helper()
	real := getRealDB(t)
	// ONE SimDB for the whole arm, not one per scenario. The real cluster keeps every previous
	// scenario's keys, so a fresh sim per scenario gives the two backends different histories —
	// and a selector range CAN resolve outside its scenario's prefix (a backward end bound lands
	// on whatever key precedes it, which on the real cluster is the previous scenario's). That
	// is a divergence the harness manufactures, and it masquerades as a sim defect: the first
	// run of this arm reported "rows diverged" with the previous scenario's key in the real
	// result. Sharing one accumulating store on both sides removes the class.
	sim := simfdb.New(nil)
	rng := mrand.New(mrand.NewPCG(seed, 0))
	for i := 0; i < n; i++ {
		prog := build(rng)
		prefix := fmt.Sprintf("arm/%s/%d/%d/%d/", name, os.Getpid(), seed, i)
		simCommitted, simRows := prog(t, sim, prefix)
		realCommitted, realRows := prog(t, real, prefix)
		if simCommitted != realCommitted {
			t.Fatalf("[%s] seed %d scenario %d: SimFDB committed=%v, real FDB committed=%v",
				name, seed, i, simCommitted, realCommitted)
		}
		if !equalRows(simRows, realRows) {
			t.Fatalf("[%s] seed %d scenario %d: rows diverged\n  sim  = %v\n  real = %v",
				name, seed, i, simRows, realRows)
		}
	}
}

func equalRows(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func armKey(prefix string, i int) fdb.Key { return fdb.Key(fmt.Sprintf("%sk%02d", prefix, i)) }

// sentinelPad is the number of always-present keys seeded on each side of the k00..k19 window,
// inside the scenario prefix.
//
// A key selector with an OFFSET walks that many existing keys from its anchor, and the walk does
// not stop at a prefix boundary — it lands on whatever key is physically there. On the shared
// real cluster that is another scenario's (or another parallel arm's) data; on a per-arm SimDB
// there is nothing, so the two backends legitimately disagree and the harness reports a defect
// that is its own. Sentinels give every walk something to land on WITHIN the prefix, which is
// what makes the comparison meaningful. Pad > max |offset| used by selectorFor.
const sentinelPad = 5

// seedArmKeys writes the masked keys plus the boundary sentinels in one committed transaction.
func seedArmKeys(t *testing.T, db fdb.BackendDatabase, prefix string, mask [nKeys]bool) {
	t.Helper()
	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		for i := 0; i < sentinelPad; i++ {
			tx.Set(fdb.Key(fmt.Sprintf("%sa%02d", prefix, i)), []byte("lo"))
			tx.Set(fdb.Key(fmt.Sprintf("%sz%02d", prefix, i)), []byte("hi"))
		}
		for i := 0; i < nKeys; i++ {
			if mask[i] {
				tx.Set(armKey(prefix, i), []byte{byte(i)})
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func concurrentSet(t *testing.T, db fdb.BackendDatabase, key fdb.Key, val []byte) {
	t.Helper()
	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(key, val)
		return nil, nil
	}); err != nil {
		t.Fatalf("concurrent write: %v", err)
	}
}

func randMask(rng *mrand.Rand) [nKeys]bool {
	var m [nKeys]bool
	for i := range m {
		m[i] = rng.IntN(2) == 0
	}
	return m
}

// ---- ARM 1: snapshot × concurrent write × commit outcome ------------------------------------

// TestDifferentialArm_SnapshotConcurrentWrite is the arm that would have caught the snapshot
// fork. It crosses THREE axes the old sweep never crossed together: whether the read is a
// snapshot read, whether the transaction declares its own read conflict on the key (the
// BunchedMap "Grand Theory" shape), and whether a concurrent writer touches it.
//
// The fork was invisible to any arm that did not take the snapshot as the transaction's FIRST
// operation, because that is what decides which view pins the read version.
func TestDifferentialArm_SnapshotConcurrentWrite(t *testing.T) {
	t.Parallel()
	for _, s := range armSeeds() {
		s := s
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			runArm(t, "snapshot", s.seed, s.n, func(rng *mrand.Rand) armProgram {
				mask := randMask(rng)
				readPt := rng.IntN(nKeys)
				probe := rng.IntN(nKeys)
				snapFirst := rng.IntN(2) == 0 // is the snapshot read the FIRST operation?
				declare := rng.IntN(2) == 0   // AddReadConflictKey on the read key?
				rangeRead := rng.IntN(2) == 0 // snapshot GetRange instead of Get?
				writeBefore := rng.IntN(2) == 0
				return func(t *testing.T, db fdb.BackendDatabase, prefix string) (bool, []string) {
					seedArmKeys(t, db, prefix, mask)
					tx, err := db.CreateWritableTransaction()
					if err != nil {
						t.Fatalf("create: %v", err)
					}
					if writeBefore && !snapFirst {
						tx.Set(armKey(prefix, (readPt+7)%nKeys), []byte("own"))
					}
					snap := tx.Snapshot()
					if rangeRead {
						snap.GetRange(fdb.KeyRange{
							Begin: armKey(prefix, 0),
							End:   armKey(prefix, nKeys),
						}, fdb.RangeOptions{}).GetSliceOrPanic()
					} else {
						snap.Get(armKey(prefix, readPt)).MustGet()
					}
					if declare {
						_ = tx.AddReadConflictKey(armKey(prefix, readPt))
					}
					concurrentSet(t, db, armKey(prefix, probe), []byte{0xEE})
					tx.Set(armKey(prefix, nKeys+5), []byte("x"))
					return tx.Commit().Get() == nil, nil
				}
			})
		})
	}
}

// ---- ARM 2: local write × read of the same key × concurrent writer ---------------------------

// TestDifferentialArm_LocalWriteThenRead is the arm for the read-conflict filter. A read the
// transaction's own buffer answered took no database read, so a concurrent writer of that key
// must not abort it — but a read the buffer only PARTLY answered, or one answered by a DEPENDENT
// write (a standalone atomic reads the stored base), must still conflict.
//
// Crossing the write KIND with the read SHAPE is what makes it a test: with only plain Sets, a
// filter that drops every written key passes.
func TestDifferentialArm_LocalWriteThenRead(t *testing.T) {
	t.Parallel()
	for _, s := range armSeeds() {
		s := s
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			runArm(t, "localwrite", s.seed, s.n, func(rng *mrand.Rand) armProgram {
				mask := randMask(rng)
				key := rng.IntN(nKeys)
				probe := rng.IntN(nKeys)
				writeKind := rng.IntN(5) // 0 Set, 1 Clear, 2 ClearRange, 3 Add, 4 Set-then-Add
				readKind := rng.IntN(3)  // 0 Get, 1 GetRange over the whole span, 2 explicit conflict
				lo := rng.IntN(nKeys)
				hi := lo + 1 + rng.IntN(nKeys-lo)
				return func(t *testing.T, db fdb.BackendDatabase, prefix string) (bool, []string) {
					seedArmKeys(t, db, prefix, mask)
					tx, err := db.CreateWritableTransaction()
					if err != nil {
						t.Fatalf("create: %v", err)
					}
					k := armKey(prefix, key)
					switch writeKind {
					case 0:
						tx.Set(k, []byte("mine"))
					case 1:
						tx.Clear(k)
					case 2:
						tx.ClearRange(fdb.KeyRange{Begin: armKey(prefix, lo), End: armKey(prefix, hi)})
					case 3:
						tx.Add(k, []byte{1, 0, 0, 0, 0, 0, 0, 0})
					default:
						tx.Set(k, []byte{0, 0, 0, 0, 0, 0, 0, 0})
						tx.Add(k, []byte{1, 0, 0, 0, 0, 0, 0, 0})
					}
					var rows []string
					switch readKind {
					case 0:
						rows = append(rows, string(tx.Get(k).MustGet()))
					case 1:
						for _, kv := range tx.GetRange(fdb.KeyRange{
							Begin: armKey(prefix, lo), End: armKey(prefix, hi),
						}, fdb.RangeOptions{}).GetSliceOrPanic() {
							rows = append(rows, string(kv.Key)+"="+string(kv.Value))
						}
					default:
						_ = tx.AddReadConflictRange(fdb.KeyRange{
							Begin: armKey(prefix, lo), End: armKey(prefix, hi),
						})
					}
					concurrentSet(t, db, armKey(prefix, probe), []byte{0xEE})
					tx.Set(armKey(prefix, nKeys+5), []byte("x"))
					return tx.Commit().Get() == nil, rows
				}
			})
		})
	}
}

// ---- ARM 3: atomic × selector × outcome -----------------------------------------------------

// TestDifferentialArm_AtomicSelector crosses the atomic mutations with selector-resolved reads.
// A selector resolution is itself a read that takes a conflict range, and an atomic is a write
// whose conflict classification depends on what it resolved against — the two together are where
// the extent arithmetic and the write-map classification meet.
func TestDifferentialArm_AtomicSelector(t *testing.T) {
	t.Parallel()
	for _, s := range armSeeds() {
		s := s
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			runArm(t, "atomicsel", s.seed, s.n, func(rng *mrand.Rand) armProgram {
				mask := randMask(rng)
				lo := rng.IntN(nKeys)
				hi := lo + 1 + rng.IntN(nKeys-lo)
				k1, k2 := rng.IntN(selectorKinds), rng.IntN(selectorKinds)
				atomicKey := rng.IntN(nKeys)
				atomicKind := rng.IntN(4) // Add / Max / Min / ByteMax
				probe := rng.IntN(nKeys)
				reverse := rng.IntN(2) == 0
				return func(t *testing.T, db fdb.BackendDatabase, prefix string) (bool, []string) {
					seedArmKeys(t, db, prefix, mask)
					tx, err := db.CreateWritableTransaction()
					if err != nil {
						t.Fatalf("create: %v", err)
					}
					ak := armKey(prefix, atomicKey)
					param := []byte{byte(atomicKind + 1), 0, 0, 0, 0, 0, 0, 0}
					switch atomicKind {
					case 0:
						tx.Add(ak, param)
					case 1:
						tx.Max(ak, param)
					case 2:
						tx.Min(ak, param)
					default:
						tx.ByteMax(ak, param)
					}
					var rows []string
					for _, kv := range tx.GetRange(fdb.SelectorRange{
						Begin: selectorFor(k1, armKey(prefix, lo)),
						End:   selectorFor(k2, armKey(prefix, hi)),
					}, fdb.RangeOptions{Reverse: reverse}).GetSliceOrPanic() {
						rows = append(rows, string(kv.Key)+"="+fmt.Sprintf("%x", kv.Value))
					}
					concurrentSet(t, db, armKey(prefix, probe), []byte{0xEE})
					tx.Set(armKey(prefix, nKeys+5), []byte("x"))
					return tx.Commit().Get() == nil, rows
				}
			})
		})
	}
}

// ---- ARM 4: three or more concurrent transactions -------------------------------------------

// TestDifferentialArm_ThreeWayConcurrency raises the transaction count. With two transactions
// the resolver only ever compares one reader against one writer, so a version-comparison
// off-by-one that only shows when a THIRD commit lands between them is unreachable. The
// transactions are opened together and committed in a scrambled order.
func TestDifferentialArm_ThreeWayConcurrency(t *testing.T) {
	t.Parallel()
	for _, s := range armSeeds() {
		s := s
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			runArm(t, "threeway", s.seed, s.n, func(rng *mrand.Rand) armProgram {
				mask := randMask(rng)
				n := 3 + rng.IntN(2) // 3 or 4 open transactions
				readKeys := make([]int, n)
				writeKeys := make([]int, n)
				order := make([]int, n)
				for i := 0; i < n; i++ {
					readKeys[i] = rng.IntN(nKeys)
					writeKeys[i] = rng.IntN(nKeys)
					order[i] = i
				}
				rng.Shuffle(n, func(i, j int) { order[i], order[j] = order[j], order[i] })
				return func(t *testing.T, db fdb.BackendDatabase, prefix string) (bool, []string) {
					seedArmKeys(t, db, prefix, mask)
					txs := make([]fdb.WritableTransaction, n)
					for i := range txs {
						tx, err := db.CreateWritableTransaction()
						if err != nil {
							t.Fatalf("create %d: %v", i, err)
						}
						txs[i] = tx
						tx.Get(armKey(prefix, readKeys[i])).MustGet()
					}
					for i := range txs {
						txs[i].Set(armKey(prefix, writeKeys[i]), []byte{byte(i)})
					}
					// The full outcome VECTOR, in the scrambled commit order, is the comparison —
					// not just "did some transaction commit". A resolver that aborts the wrong
					// member of the set produces the same count and a different vector.
					var outcomes []string
					allCommitted := true
					for _, idx := range order {
						err := txs[idx].Commit().Get()
						code := 0
						var fe fdb.Error
						if errors.As(err, &fe) {
							code = fe.Code
						}
						if err != nil {
							allCommitted = false
						}
						outcomes = append(outcomes, fmt.Sprintf("t%d=%d", idx, code))
					}
					return allCommitted, outcomes
				}
			})
		})
	}
}

// ---- ARM 5: ClearRange × conflict extent ----------------------------------------------------

// TestDifferentialArm_ClearRangeExtent crosses a wide write-conflict range against a reader's
// extent. ClearRange is the only operation that produces a WIDE write conflict range, so the
// boundary arithmetic (keyAfter, half-open overlap, the gap between adjacent keys) is only
// exercised on that side by this shape.
func TestDifferentialArm_ClearRangeExtent(t *testing.T) {
	t.Parallel()
	for _, s := range armSeeds() {
		s := s
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			runArm(t, "clearrange", s.seed, s.n, func(rng *mrand.Rand) armProgram {
				mask := randMask(rng)
				rlo := rng.IntN(nKeys)
				rhi := rlo + 1 + rng.IntN(nKeys-rlo)
				clo := rng.IntN(nKeys)
				chi := clo + 1 + rng.IntN(nKeys-clo)
				limit := rng.IntN(6)
				reverse := rng.IntN(2) == 0
				return func(t *testing.T, db fdb.BackendDatabase, prefix string) (bool, []string) {
					seedArmKeys(t, db, prefix, mask)
					tx, err := db.CreateWritableTransaction()
					if err != nil {
						t.Fatalf("create: %v", err)
					}
					var rows []string
					for _, kv := range tx.GetRange(fdb.KeyRange{
						Begin: armKey(prefix, rlo), End: armKey(prefix, rhi),
					}, fdb.RangeOptions{Limit: limit, Reverse: reverse}).GetSliceOrPanic() {
						rows = append(rows, string(kv.Key))
					}
					// The concurrent transaction CLEARS a range rather than setting a key.
					if _, err := db.Transact(func(w fdb.WritableTransaction) (any, error) {
						w.ClearRange(fdb.KeyRange{Begin: armKey(prefix, clo), End: armKey(prefix, chi)})
						return nil, nil
					}); err != nil {
						t.Fatalf("concurrent clear: %v", err)
					}
					tx.Set(armKey(prefix, nKeys+5), []byte("x"))
					return tx.Commit().Get() == nil, rows
				}
			})
		})
	}
}

// ---- ARM 6: read STARTED, then a local write, then the read CONSUMED -------------------------

// TestDifferentialArm_ReadThenLocalWrite crosses the write with the read in the ORDER the
// local-write arm never uses: the range read is ISSUED (and, for the iterator shapes, partly
// drained) BEFORE the write lands, and consumed after it.
//
// That ordering is the whole point. TestDifferentialArm_LocalWriteThenRead always writes first,
// so a backend that resolves the entire range at GetRange() call time answers identically to one
// that resolves each batch at consumption — the buffer is already complete when the read is
// issued either way. It takes a write BETWEEN issue and consumption to tell them apart, and the
// difference is observable twice over: in the rows (a real client's RangeResult is lazy, so the
// write shows up in whatever has not been fetched yet) and in the commit outcome (the read
// conflict is filtered through the write map at consumption, so a read that answered from
// storage must not have its span subtracted as locally-satisfied).
//
// The record layer's scan-and-update cursors are exactly this shape, which is why the iterator
// variants prefetch a variable number of rows before writing.
func TestDifferentialArm_ReadThenLocalWrite(t *testing.T) {
	t.Parallel()
	for _, s := range armSeeds() {
		s := s
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			runArm(t, "readthenwrite", s.seed, s.n, func(rng *mrand.Rand) armProgram {
				mask := randMask(rng)
				key := rng.IntN(nKeys)
				probe := rng.IntN(nKeys)
				writeKind := rng.IntN(5) // 0 Set, 1 Clear, 2 ClearRange, 3 Add, 4 Set-then-Add
				readShape := rng.IntN(3) // 0 GetSlice, 1 iterator, 2 iterator prefetched
				prefetch := rng.IntN(5)
				reverse := rng.IntN(2) == 0
				limit := rng.IntN(6)
				lo := rng.IntN(nKeys)
				hi := lo + 1 + rng.IntN(nKeys-lo)
				clo := rng.IntN(nKeys)
				chi := clo + 1 + rng.IntN(nKeys-clo)
				return func(t *testing.T, db fdb.BackendDatabase, prefix string) (bool, []string) {
					seedArmKeys(t, db, prefix, mask)
					tx, err := db.CreateWritableTransaction()
					if err != nil {
						t.Fatalf("create: %v", err)
					}
					k := armKey(prefix, key)
					write := func() {
						switch writeKind {
						case 0:
							tx.Set(k, []byte("mine"))
						case 1:
							tx.Clear(k)
						case 2:
							tx.ClearRange(fdb.KeyRange{Begin: armKey(prefix, clo), End: armKey(prefix, chi)})
						case 3:
							tx.Add(k, []byte{1, 0, 0, 0, 0, 0, 0, 0})
						default:
							tx.Set(k, []byte{0, 0, 0, 0, 0, 0, 0, 0})
							tx.Add(k, []byte{1, 0, 0, 0, 0, 0, 0, 0})
						}
					}
					rr := tx.GetRange(fdb.KeyRange{
						Begin: armKey(prefix, lo), End: armKey(prefix, hi),
					}, fdb.RangeOptions{Limit: limit, Reverse: reverse, Mode: fdb.StreamingModeIterator})

					var rows []string
					switch readShape {
					case 0:
						// The write lands between GetRange() and GetSliceWithError().
						write()
						for _, kv := range rr.GetSliceOrPanic() {
							rows = append(rows, string(kv.Key)+"="+string(kv.Value))
						}
					default:
						it := rr.Iterator()
						if readShape == 2 {
							for i := 0; i < prefetch && it.Advance(); i++ {
								kv := it.MustGet()
								rows = append(rows, string(kv.Key)+"="+string(kv.Value))
							}
						}
						write()
						for it.Advance() {
							kv := it.MustGet()
							rows = append(rows, string(kv.Key)+"="+string(kv.Value))
						}
					}
					concurrentSet(t, db, armKey(prefix, probe), []byte{0xEE})
					tx.Set(armKey(prefix, nKeys+5), []byte("x"))
					return tx.Commit().Get() == nil, rows
				}
			})
		})
	}
}

// ---- ARM 7: injected commit_unknown_result vs the reality it stands for ----------------------

// TestDifferentialArm_CommitUnknownMatchesReality is the fault arm. A fault cannot be injected
// into a real cluster, so the comparison is made against what each of 1021's two branches CLAIMS
// to be: the APPLIED branch must leave the keyspace exactly as a successful commit would, and
// the DISCARDED branch exactly as an aborted one would. Real FDB supplies both references.
//
// This is what makes the two-branch model checkable rather than asserted. A sim that modelled
// 1021 as always-applied passes the applied comparison and has no discarded case to compare.
func TestDifferentialArm_CommitUnknownMatchesReality(t *testing.T) {
	t.Parallel()
	real := getRealDB(t)
	seed := differentialSeed()
	rng := mrand.New(mrand.NewPCG(seed, 0))

	const scenarios = 150
	for i := 0; i < scenarios; i++ {
		mask := randMask(rng)
		writeKey := rng.IntN(nKeys)
		clearLo := rng.IntN(nKeys)
		clearHi := clearLo + 1 + rng.IntN(nKeys-clearLo)
		useClear := rng.IntN(2) == 0
		applied := rng.IntN(2) == 0

		prefix := fmt.Sprintf("arm/unknown/%d/%d/%d/", os.Getpid(), seed, i)

		// The mutation the transaction performs, identical on both backends.
		mutate := func(tx fdb.WritableTransaction) {
			if useClear {
				tx.ClearRange(fdb.KeyRange{Begin: armKey(prefix, clearLo), End: armKey(prefix, clearHi)})
			} else {
				tx.Set(armKey(prefix, writeKey), []byte("payload"))
			}
		}

		// REFERENCE: on a real cluster, either commit it or throw it away.
		seedArmKeys(t, real, prefix, mask)
		if applied {
			if _, err := real.Transact(func(tx fdb.WritableTransaction) (any, error) {
				mutate(tx)
				return nil, nil
			}); err != nil {
				t.Fatalf("reference commit: %v", err)
			}
		} else {
			rtx, err := real.CreateWritableTransaction()
			if err != nil {
				t.Fatalf("reference create: %v", err)
			}
			mutate(rtx)
			rtx.Cancel()
		}
		want := dumpRange(t, real, prefix)

		// SUBJECT: the same mutation, ended by an injected 1021 on the named branch.
		sim := simfdb.New(nil)
		seedArmKeys(t, sim, prefix, mask)
		stx, err := sim.CreateWritableTransaction()
		if err != nil {
			t.Fatalf("sim create: %v", err)
		}
		mutate(stx)
		branch := simfdb.CommitUnknownDiscarded
		if applied {
			branch = simfdb.CommitUnknownApplied
		}
		sim.InjectOnce(branch)
		var fe fdb.Error
		if cerr := stx.Commit().Get(); !errors.As(cerr, &fe) || fe.Code != 1021 {
			t.Fatalf("scenario %d: sim commit = %v, want 1021", i, cerr)
		}
		got := dumpRange(t, sim, prefix)

		if !equalRows(got, want) {
			t.Fatalf("seed %d scenario %d (applied=%v, clear=%v): the %s branch of "+
				"commit_unknown_result left a keyspace real FDB does not produce\n  sim  = %v\n  real = %v",
				seed, i, applied, useClear, branchName(applied), got, want)
		}
	}
}

func branchName(applied bool) string {
	if applied {
		return "APPLIED"
	}
	return "DISCARDED"
}

// dumpRange reads the arm's whole key span as comparable strings.
func dumpRange(t *testing.T, db fdb.BackendDatabase, prefix string) []string {
	t.Helper()
	out, err := db.ReadTransact(func(rtx fdb.ReadTransaction) (any, error) {
		var rows []string
		for _, kv := range rtx.GetRange(fdb.KeyRange{
			Begin: fdb.Key(prefix), End: fdb.Key(prefix + "\xff"),
		}, fdb.RangeOptions{}).GetSliceOrPanic() {
			rows = append(rows, string(kv.Key)+"="+fmt.Sprintf("%x", kv.Value))
		}
		return rows, nil
	})
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	return out.([]string)
}

// ---- ARM 8: range-option validation, per consumption surface --------------------------------

// TestDifferential_RangeOptionValidation measures which range-option errors each consumption
// surface raises, against a real cluster.
//
// It exists because reasoning about it from the API docs gets it backwards. A row limit below -1
// is range_limits_invalid(2012) on both surfaces; EXACT with no row budget is
// exact_mode_without_limits(2210) on both; and 2012 WINS over 2210 when the two could both apply
// (EXACT with -7), because the C gate compares the limit against ROW_LIMIT_UNLIMITED and a limit
// below it is invalid rather than unlimited.
//
// SCOPE, and it is a real limit of this arm: the oracle here is the PURE-GO client, not libfdb_c.
// It can only show that SimFDB matches the Go client — if the Go client itself diverges from C,
// this arm stays green. It did exactly that: 2210-from-GetSlice was modelled as unreachable on
// both, on a source argument about Apple's binding rewriting the streaming mode, and both were
// wrong together. The cgo arm that settles it is
// pkg/fdbgo/bench:TestDifferential_ExactModeWithoutLimits.
func TestDifferential_RangeOptionValidation(t *testing.T) {
	t.Parallel()
	real := getRealDB(t)
	sim := simfdb.New(nil)
	prefix := fmt.Sprintf("optvalid/%d/", os.Getpid())

	for _, tc := range []struct {
		name string
		opts fdb.RangeOptions
	}{
		{"limit -2", fdb.RangeOptions{Limit: -2}},
		{"limit -7 with exact", fdb.RangeOptions{Mode: fdb.StreamingModeExact, Limit: -7}},
		{"exact without limit", fdb.RangeOptions{Mode: fdb.StreamingModeExact}},
		{"exact with limit -1", fdb.RangeOptions{Mode: fdb.StreamingModeExact, Limit: -1}},
		{"exact with limit", fdb.RangeOptions{Mode: fdb.StreamingModeExact, Limit: 1}},
		{"limit -1 unlimited", fdb.RangeOptions{Limit: -1}},
	} {
		tc := tc
		probe := func(db fdb.BackendDatabase) (sliceCode, iterCode int) {
			_, err := db.ReadTransact(func(rtx fdb.ReadTransaction) (any, error) {
				rng := fdb.KeyRange{Begin: fdb.Key(prefix), End: fdb.Key(prefix + "\xff")}
				_, sErr := rtx.GetRange(rng, tc.opts).GetSliceWithError()
				sliceCode = codeOf(sErr)
				it := rtx.GetRange(rng, tc.opts).Iterator()
				it.Advance()
				_, iErr := it.Get()
				iterCode = codeOf(iErr)
				return nil, nil
			})
			if err != nil {
				t.Fatalf("[%s] probe transaction: %v", tc.name, err)
			}
			return sliceCode, iterCode
		}
		simSlice, simIter := probe(sim)
		realSlice, realIter := probe(real)
		if simSlice != realSlice || simIter != realIter {
			t.Errorf("[%s] validation diverged: sim{GetSlice:%d Iterator:%d} real{GetSlice:%d Iterator:%d}",
				tc.name, simSlice, simIter, realSlice, realIter)
		}
	}
}
