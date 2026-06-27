package bench

import (
	"bytes"
	"fmt"
	"testing"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// L3 differential read parity — RFC-053 (RFC-010 C2). The read-side counterpart to
// the write battery: the merged GetRange result and GetKey (key-selector)
// resolution must be identical across the pure-Go client and libfdb_c.
//
// Read-version pinning is MANDATORY here. The two clients keep INDEPENDENT GRV
// caches; data seeded via one client commits at version V, but the other client's
// cached read version may predate V, so it would read a stale snapshot — and a
// key selector that escapes the seeded prefix would then resolve into a region
// other parallel tests are concurrently mutating (all bench tests share one
// cluster). Both reads are therefore pinned to the seed's commit version via
// SetReadVersion, so they observe the identical snapshot and the comparison is
// deterministic. (This is also the correct way to differentially compare reads:
// compare at one fixed version, never across two independent GRVs.)
//
// Chunking-invariance: a raw range read returns (KV[], more) with no continuation
// token (continuations are a record-layer concern). The client-level invariant is
// that the MERGED result is identical regardless of how the range is split into
// chunks. The C client honours StreamingMode (chunk size); our Go client always
// reads exact. Varying the C client's Mode while the Go client reads exact, and
// asserting equal merged results, pins the invariant directly.

type kvPair struct{ k, v []byte }

func normGo(kvs []gofdb.KeyValue) []kvPair {
	out := make([]kvPair, len(kvs))
	for i, kv := range kvs {
		out[i] = kvPair{append([]byte(nil), kv.Key...), append([]byte(nil), kv.Value...)}
	}
	return out
}

func normC(kvs []cgofdb.KeyValue) []kvPair {
	out := make([]kvPair, len(kvs))
	for i, kv := range kvs {
		out[i] = kvPair{append([]byte(nil), kv.Key...), append([]byte(nil), kv.Value...)}
	}
	return out
}

func assertKVEqual(t *testing.T, label string, a, b []kvPair) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("%s: result length differs: go=%d cgo=%d", label, len(a), len(b))
	}
	for i := range a {
		if !bytes.Equal(a[i].k, b[i].k) || !bytes.Equal(a[i].v, b[i].v) {
			t.Fatalf("%s: pair %d differs: go=(%x,%x) cgo=(%x,%x)",
				label, i, a[i].k, a[i].v, b[i].k, b[i].v)
		}
	}
}

// seedKeys writes the seed data via the C binding and commits it.
func seedKeys(t *testing.T, seed func(tx cgofdb.Transaction)) {
	t.Helper()
	if _, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
		seed(tx)
		return nil, nil
	}); err != nil {
		t.Fatalf("cgo seed: %v", err)
	}
}

// freshSharedVersion returns a CURRENT read version (a fresh GRV via the C client).
// Both clients pin their comparison reads to it so they observe the identical
// snapshot — eliminating the independent-GRV-cache skew that otherwise lets a
// boundary selector resolve into concurrently-mutated keyspace. It is captured
// per read-unit (not once up front) so it stays inside FDB's 5s MVCC window even
// under heavy parallel load; being a fresh GRV it is always >= the seed commit.
func freshSharedVersion(t *testing.T) int64 {
	t.Helper()
	tr, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatalf("cgo CreateTransaction: %v", err)
	}
	v, err := tr.GetReadVersion().Get()
	if err != nil {
		t.Fatalf("cgo GetReadVersion: %v", err)
	}
	return v
}

// goRangeAt/cgoRangeAt read at a PINNED version, RETURNING any error (no Fatalf) so a transient
// transaction_too_old(1007) — when the pinned version drifts past the 5s MVCC window under heavy
// parallel-container load — is retried with a fresh version by the caller rather than failing the
// test. Mirrors goGetKeyAt/cgoGetKeyAt. Callers MUST re-pin a fresh shared version (via
// freshSharedVersion) and re-read BOTH clients on a retryable error — see the re-pin-and-retry
// loops in TestDifferential_RangeRead and runDifferentialSequence — so both clients keep
// observing the IDENTICAL snapshot.
func goRangeAt(t *testing.T, v int64, r gofdb.Range, opts gofdb.RangeOptions) ([]gofdb.KeyValue, error) {
	t.Helper()
	tr, err := goClient.CreateTransaction()
	if err != nil {
		t.Fatalf("go CreateTransaction: %v", err)
	}
	defer tr.Cancel()
	tr.SetReadVersion(v)
	return tr.GetRange(r, opts).GetSliceWithError()
}

func cgoRangeAt(t *testing.T, v int64, r cgofdb.Range, opts cgofdb.RangeOptions) ([]cgofdb.KeyValue, error) {
	t.Helper()
	tr, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatalf("cgo CreateTransaction: %v", err)
	}
	defer tr.Cancel()
	tr.SetReadVersion(v)
	return tr.GetRange(r, opts).GetSliceWithError()
}

// goGetKeyAt resolves a selector at a pinned version, RETURNING any error (no MustGet) so a
// transient transaction_too_old(1007) — when the pinned version drifts past the 5s MVCC window
// under heavy parallel-container load — is retried with a fresh version by the caller rather
// than panicking the test.
func goGetKeyAt(t *testing.T, v int64, sel gofdb.KeySelector) ([]byte, error) {
	t.Helper()
	tr, err := goClient.CreateTransaction()
	if err != nil {
		t.Fatalf("go CreateTransaction: %v", err)
	}
	defer tr.Cancel()
	tr.SetReadVersion(v)
	k, err := tr.GetKey(sel).Get()
	return []byte(k), err
}

func cgoGetKeyAt(t *testing.T, v int64, sel cgofdb.KeySelector) ([]byte, error) {
	t.Helper()
	tr, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatalf("cgo CreateTransaction: %v", err)
	}
	defer tr.Cancel()
	tr.SetReadVersion(v)
	k, err := tr.GetKey(sel).Get()
	return []byte(k), err
}

func TestDifferential_RangeRead(t *testing.T) {
	t.Parallel()
	prefix := "diff_rng_"
	seedKeys(t, func(tx cgofdb.Transaction) {
		for i := 0; i < 60; i++ {
			tx.Set(cgofdb.Key(fmt.Sprintf("%s%03d", prefix, i)), []byte{byte(i), byte((i * 7) & 0xff)})
		}
	})
	goPR, err := gofdb.PrefixRange([]byte(prefix))
	if err != nil {
		t.Fatalf("go PrefixRange: %v", err)
	}
	cPR, err := cgofdb.PrefixRange([]byte(prefix))
	if err != nil {
		t.Fatalf("cgo PrefixRange: %v", err)
	}

	cases := []struct {
		name    string
		limit   int
		reverse bool
		cMode   cgofdb.StreamingMode // varies C-side chunking; Go always reads exact
	}{
		{"all_wantall", 0, false, cgofdb.StreamingModeWantAll},
		{"all_iterator", 0, false, cgofdb.StreamingModeIterator}, // small chunks → many fetches, same merged result
		{"all_small", 0, false, cgofdb.StreamingModeSmall},
		{"exact_limit20", 20, false, cgofdb.StreamingModeExact}, // Exact REQUIRES a limit
		{"limit10", 10, false, cgofdb.StreamingModeWantAll},
		{"limit1", 1, false, cgofdb.StreamingModeSmall},
		{"reverse_all", 0, true, cgofdb.StreamingModeWantAll},
		{"reverse_limit7", 7, true, cgofdb.StreamingModeIterator},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			const maxAttempts = 12
			for attempt := 0; ; attempt++ {
				if attempt >= maxAttempts {
					t.Fatalf("%s: did not clear transient errors in %d attempts", tc.name, maxAttempts)
				}
				v := freshSharedVersion(t) // fresh per attempt → within the MVCC window
				goRaw, goErr := goRangeAt(t, v, goPR, gofdb.RangeOptions{Limit: tc.limit, Reverse: tc.reverse})
				cRaw, cErr := cgoRangeAt(t, v, cPR, cgofdb.RangeOptions{Limit: tc.limit, Reverse: tc.reverse, Mode: tc.cMode})
				if (goErr != nil && isFDBRetryable(goErr)) || (cErr != nil && isFDBRetryable(cErr)) {
					continue // stale pin (1007) under load — retry with a fresh shared version
				}
				if goErr != nil {
					t.Fatalf("%s: go range: %v", tc.name, goErr)
				}
				if cErr != nil {
					t.Fatalf("%s: cgo range: %v", tc.name, cErr)
				}
				assertKVEqual(t, tc.name, normGo(goRaw), normC(cRaw))
				return
			}
		})
	}
}

func TestDifferential_GetKey(t *testing.T) {
	t.Parallel()
	prefix := "diff_gk_"
	present := []string{"a", "c", "e", "g"}
	seedKeys(t, func(tx cgofdb.Transaction) {
		for _, s := range present {
			tx.Set(cgofdb.Key(prefix+s), []byte("x"))
		}
	})

	// Probe keys spanning gaps, exact hits, before-first and after-last. Selectors
	// that escape the seeded set resolve into the shared keyspace — but both clients
	// read at the SAME pinned version v, so they observe the identical neighbours.
	probes := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	for _, p := range probes {
		key := []byte(prefix + p)
		sels := []struct {
			name string
			goS  gofdb.KeySelector
			cS   cgofdb.KeySelector
		}{
			{"FGT", gofdb.FirstGreaterThan(gofdb.Key(key)), cgofdb.FirstGreaterThan(cgofdb.Key(key))},
			{"FGE", gofdb.FirstGreaterOrEqual(gofdb.Key(key)), cgofdb.FirstGreaterOrEqual(cgofdb.Key(key))},
			{"LLT", gofdb.LastLessThan(gofdb.Key(key)), cgofdb.LastLessThan(cgofdb.Key(key))},
			{"LLE", gofdb.LastLessOrEqual(gofdb.Key(key)), cgofdb.LastLessOrEqual(cgofdb.Key(key))},
		}
		for _, sel := range sels {
			sel := sel
			t.Run(fmt.Sprintf("%s_%s", p, sel.name), func(t *testing.T) {
				const maxAttempts = 12
				for attempt := 0; ; attempt++ {
					if attempt >= maxAttempts {
						t.Fatalf("GetKey %s(%q): retryable errors (1007 read-version staleness) did not clear in %d attempts", sel.name, key, maxAttempts)
					}
					v := freshSharedVersion(t) // fresh per attempt → within the MVCC window
					goK, goErr := goGetKeyAt(t, v, sel.goS)
					cK, cErr := cgoGetKeyAt(t, v, sel.cS)
					if isFDBRetryable(goErr) || isFDBRetryable(cErr) {
						continue // transient version staleness under load — re-version and retry
					}
					if goErr != nil || cErr != nil {
						t.Fatalf("GetKey %s(%q) error: go=%v cgo=%v", sel.name, key, goErr, cErr)
					}
					if !bytes.Equal(goK, cK) {
						t.Fatalf("GetKey %s(%q): go=%q cgo=%q", sel.name, key, goK, cK)
					}
					return
				}
			})
		}
	}
}
