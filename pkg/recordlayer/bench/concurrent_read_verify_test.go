package bench

import (
	"context"
	"encoding/binary"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// TestFDBConcurrentReadVerify isolates the "union descriptor does not contain
// any known record type" failure seen in concurrent HNSW ingest: a tx.Get of a
// key returning a *different* key's value. It strips HNSW out entirely and tests
// the bare client/record-layer read path under the same shape of load — many
// concurrent transactions, each doing a batch of point reads over one shared
// connection per storage server.
//
// Every value embeds its own (gid, idx) identity, so a wrong-value read is both
// detected and traced to the key whose value actually leaked. Reproduction here
// = pure client/record-layer bug (escalate via fdb-client-review). No
// reproduction here while HNSW reproduces = the bug needs HNSW's deep
// read-per-op pattern, narrowing the search.
//
//	FDB_READVERIFY=1 [VECTOR_BENCH_PROCS=10 RV_GOROUTINES=24 RV_KEYS=2000 \
//	  RV_VALSIZE=12288 RV_BATCH=64 RV_SECONDS=240] \
//	  go test ./pkg/recordlayer/bench -run '^TestFDBConcurrentReadVerify$' -v -timeout 30m
func TestFDBConcurrentReadVerify(t *testing.T) {
	if os.Getenv("FDB_READVERIFY") != "1" {
		t.Skip("set FDB_READVERIFY=1 to run")
	}
	procs := vecEnvInt("VECTOR_BENCH_PROCS", 10)
	N := vecEnvInt("RV_GOROUTINES", 24)
	M := vecEnvInt("RV_KEYS", 2000)
	valSize := vecEnvInt("RV_VALSIZE", 12288) // ~1536-D double vector record size
	batch := vecEnvInt("RV_BATCH", 64)        // reads per transaction (HNSW-like)
	seconds := vecEnvInt("RV_SECONDS", 240)
	db := vecMultiProcDB(t, procs)
	ctx := context.Background()
	root := vecBenchSubspace(t.Name())

	encode := func(gid, idx int) []byte {
		v := make([]byte, valSize)
		binary.BigEndian.PutUint64(v[0:8], uint64(gid))
		binary.BigEndian.PutUint64(v[8:16], uint64(idx))
		for j := 16; j < valSize; j++ {
			v[j] = byte((gid*131 + idx*17 + j) & 0xff)
		}
		return v
	}
	keyOf := func(gid, idx int) fdb.Key {
		return fdb.Key(root.Sub(int64(gid)).Pack(tuple.Tuple{int64(idx)}))
	}

	// Phase 1: populate every (gid, idx) key, batched like the ingest harness.
	var wg sync.WaitGroup
	for g := 0; g < N; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for start := 0; start < M; start += 16 {
				end := min(start+16, M)
				if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					tx := rtx.Transaction()
					for i := start; i < end; i++ {
						tx.Set(keyOf(gid, i), encode(gid, i))
					}
					return nil, nil
				}); err != nil {
					t.Errorf("populate g%d [%d,%d): %v", gid, start, end, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	t.Logf("populated %d goroutines x %d keys (%dB each); verifying for %ds at C=%d, batch=%d",
		N, M, valSize, seconds, N, batch)

	// Phase 2: concurrent batched read-verify + read-modify-write, time-bounded.
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	var reads, mismatches, missing, txErrs int64
	var firstMismatch atomic.Value // string
	for g := 0; g < N; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(gid)*2654435761 + 1))
			for time.Now().Before(deadline) && atomic.LoadInt64(&mismatches) == 0 {
				// Pick a batch of random indices. Half target POPULATED keys
				// [0,M) (verify value); half target permanently-EMPTY keys
				// [M,2M) (verify nil). The reported bug is an empty key — first
				// save of a PK — returning non-nil garbage, so the empty-key
				// reads are the critical dimension.
				idxs := make([]int, batch)
				empty := make([]bool, batch)
				for k := range idxs {
					if rng.Intn(2) == 0 {
						idxs[k] = rng.Intn(M) // populated
					} else {
						idxs[k] = M + rng.Intn(M) // never written
						empty[k] = true
					}
				}
				rmw := rng.Intn(2) == 0 // half the txns also write back (read-modify-write)
				_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					tx := rtx.Transaction()
					// Fire the batch of gets, then resolve — mirrors HNSW's many
					// in-flight reads multiplexed over one connection.
					futs := make([]fdb.FutureByteSlice, batch)
					for k, idx := range idxs {
						futs[k] = tx.Get(keyOf(gid, idx))
					}
					for k, idx := range idxs {
						val, e := futs[k].Get()
						if e != nil {
							return nil, e
						}
						atomic.AddInt64(&reads, 1)
						if empty[k] {
							// Never-written key MUST read back nil.
							if val != nil {
								rg := int(binary.BigEndian.Uint64(val[0:8]))
								ri := int(binary.BigEndian.Uint64(val[8:16]))
								firstMismatch.CompareAndSwap(nil,
									fmtMismatch("EMPTY-NONNIL", gid, idx, rg, ri, len(val)))
								atomic.AddInt64(&mismatches, 1)
								return nil, nil
							}
							continue
						}
						if val == nil {
							atomic.AddInt64(&missing, 1)
							firstMismatch.CompareAndSwap(nil,
								fmtMismatch("MISSING", gid, idx, -1, -1, 0))
							atomic.AddInt64(&mismatches, 1)
							return nil, nil
						}
						rg := int(binary.BigEndian.Uint64(val[0:8]))
						ri := int(binary.BigEndian.Uint64(val[8:16]))
						if rg != gid || ri != idx {
							firstMismatch.CompareAndSwap(nil,
								fmtMismatch("WRONG", gid, idx, rg, ri, len(val)))
							atomic.AddInt64(&mismatches, 1)
							return nil, nil
						}
					}
					if rmw {
						for k, idx := range idxs {
							if !empty[k] {
								tx.Set(keyOf(gid, idx), encode(gid, idx))
							}
						}
					}
					return nil, nil
				})
				if err != nil {
					atomic.AddInt64(&txErrs, 1)
				}
			}
		}(g)
	}
	wg.Wait()

	t.Logf("reads=%d txErrs=%d missing=%d mismatches=%d",
		atomic.LoadInt64(&reads), atomic.LoadInt64(&txErrs),
		atomic.LoadInt64(&missing), atomic.LoadInt64(&mismatches))
	if m := firstMismatch.Load(); m != nil {
		t.Fatalf("WRONG-VALUE READ DETECTED: %s", m.(string))
	}
}

func fmtMismatch(kind string, gid, idx, gotG, gotI, valLen int) string {
	if kind == "MISSING" {
		return kind + ": key (gid=" + itoa(gid) + " idx=" + itoa(idx) + ") returned nil but was populated"
	}
	return kind + ": key (gid=" + itoa(gid) + " idx=" + itoa(idx) +
		") returned value belonging to (gid=" + itoa(gotG) + " idx=" + itoa(gotI) +
		"), valLen=" + itoa(valLen)
}

func itoa(n int) string {
	if n < 0 {
		return "-" + itoa(-n)
	}
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
