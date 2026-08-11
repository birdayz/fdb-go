//go:build cgo && libfdbc

package libfdbc_test

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	fdbclient "fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/libfdbc"
)

// Read-your-writes differential for range reads issued under libfdb_c's PER-MODE BYTE LIMITS.
//
// THIS IS A SAFETY NET BUILT BEFORE THE CHANGE IT PROTECTS, and the ordering is the point.
// TODO.md Phase 12 books porting GetRangeLimits{rows,bytes} + isReached() into the pure-Go
// range path so batch division matches libfdb_c. The server-path half of that port is visible
// and cheap to check: if the division is wrong, a batch-count assertion says so. The RYW half
// is not. Go's merge helpers — applyLimitAndDirection, computeMore, limitReached,
// cacheWalkBudget (client/ryw.go) — decide what a read RETURNS, not merely how it is divided.
// Adding byte accounting there can drop, duplicate or resurrect a row while every batch-count
// assertion stays green, because the counts would still look plausible. That failure is silent,
// and silent is what this file exists to remove.
//
// WHAT IT ASSERTS, and why it holds both before and after the port. The two clients divide a
// range differently today — C stops a fetch on its per-mode byte target (mode_bytes_array,
// bindings/c/fdb_c.cpp:1002), Go stops only on rows — but a full DRAIN must return the same
// rows regardless. The row set is invariant under division. So this is green today, must stay
// green through the port, and reds the moment byte accounting corrupts the merged view.
//
// THE MODEL IS INDEPENDENT, deliberately. Each arm compares BOTH clients against a row set
// computed in Go from the base data and the mutation list — not against each other. Comparing
// the clients to one another is the paired-equality trap: two sides that share a derivation, or
// that break in the same direction, agree while both being wrong. An independent model cannot
// move with either of them.
//
// COVERAGE IS SPLIT FORWARD/REVERSE ON PURPOSE. C++ implements RYW range reads as two separate
// ~300-line functions with their own byte-limit early exits — forward at
// ReadYourWrites.actor.cpp:597-899 (exit at :693) and reverse at :900-1230 (exit at :1000).
// They are not one mechanism seen twice, so a harness exercising only forward proves nothing
// about reverse. Go likewise carries reverse as a distinct path.
type rywMut struct {
	op       string // "set" | "clearkey" | "clearrange"
	key, end string
	val      string
}

// rywApplyC applies the mutation list through libfdb_c. Its transaction handle has no
// single-key clear, so a key clear is spelled as the half-open range [k, k+\x00).
func rywApplyC(tr libfdbc.CTxnHandle, muts []rywMut) {
	for _, m := range muts {
		switch m.op {
		case "set":
			tr.Set([]byte(m.key), []byte(m.val))
		case "clearkey":
			tr.ClearRange([]byte(m.key), append([]byte(m.key), 0))
		case "clearrange":
			tr.ClearRange([]byte(m.key), []byte(m.end))
		}
	}
}

func rywApplyGo(t *testing.T, tx *fdbclient.Transaction, muts []rywMut) {
	t.Helper()
	for _, m := range muts {
		switch m.op {
		case "set":
			tx.Set([]byte(m.key), []byte(m.val))
		case "clearkey":
			tx.Clear([]byte(m.key))
		case "clearrange":
			if err := tx.ClearRange([]byte(m.key), []byte(m.end)); err != nil {
				t.Fatalf("ClearRange(%q,%q): %v", m.key, m.end, err)
			}
		}
	}
}

// rywModel computes the expected rows independently of either client: base data with the
// mutation list folded in, sorted, and reversed when the read is descending.
func rywModel(base map[string]string, muts []rywMut, reverse bool) []libfdbc.CKeyValue {
	view := make(map[string]string, len(base))
	for k, v := range base {
		view[k] = v
	}
	for _, m := range muts {
		switch m.op {
		case "set":
			view[m.key] = m.val
		case "clearkey":
			delete(view, m.key)
		case "clearrange":
			for k := range view {
				if k >= m.key && k < m.end {
					delete(view, k)
				}
			}
		}
	}
	keys := make([]string, 0, len(view))
	for k := range view {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if reverse {
		for i, j := 0, len(keys)-1; i < j; i, j = i+1, j-1 {
			keys[i], keys[j] = keys[j], keys[i]
		}
	}
	out := make([]libfdbc.CKeyValue, len(keys))
	for i, k := range keys {
		out[i] = libfdbc.CKeyValue{Key: []byte(k), Value: []byte(view[k])}
	}
	return out
}

// rywDrainC drives libfdb_c's own fetch loop to exhaustion and returns the rows plus the
// per-fetch counts. The per-fetch counts are what expose minRows.
func rywDrainC(t *testing.T, tr libfdbc.CTxnHandle, begin, end []byte, mode int, reverse bool) ([]libfdbc.CKeyValue, []int) {
	t.Helper()
	var rows []libfdbc.CKeyValue
	var counts []int
	curBegin := append([]byte(nil), begin...)
	curEnd := append([]byte(nil), end...)
	for iteration := 1; ; iteration++ {
		batch, more, err := libfdbc.CGetRangeBatch(tr, curBegin, curEnd,
			0 /* limit */, 0 /* target_bytes: C's per-mode default */, mode, iteration, reverse, false)
		if err != nil {
			t.Fatalf("CGetRangeBatch(mode=%d reverse=%v iteration=%d): %v", mode, reverse, iteration, err)
		}
		counts = append(counts, len(batch))
		rows = append(rows, batch...)
		if !more || len(batch) == 0 {
			return rows, counts
		}
		last := batch[len(batch)-1].Key
		if reverse {
			// Descending: the last row is the LOWEST key, and it becomes the next
			// window's exclusive end.
			curEnd = append([]byte(nil), last...)
		} else {
			curBegin = append(append([]byte(nil), last...), 0)
		}
		if iteration > 2000 {
			t.Fatalf("mode=%d reverse=%v did not terminate within 2000 fetches", mode, reverse)
		}
	}
}

func TestLibFDBC_RYWRangeUnderByteLimitDifferential(t *testing.T) {
	t.Parallel()

	clusterFile := startCluster(t)

	cdb, err := libfdbc.COpenDatabase(clusterFile)
	if err != nil {
		t.Fatalf("open libfdb_c database: %v", err)
	}
	defer cdb.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	goDB, err := fdbclient.OpenDatabase(ctx, clusterFile, fdbclient.WithAPIVersion(730))
	if err != nil {
		t.Fatalf("open pure-Go client: %v", err)
	}
	defer goDB.Close()

	// Rows are fat so the per-mode byte targets actually bite: at 300 bytes a row, SMALL's
	// 256-byte target is exceeded by a SINGLE row and MEDIUM's 1000 by four. On thin rows
	// every mode returns everything in one fetch and the byte dimension is untested.
	const valueLen = 300
	fat := func(tag string) string {
		v := make([]byte, valueLen)
		for i := range v {
			v[i] = tag[0]
		}
		return string(v)
	}

	modes := []struct {
		name string
		mode int
	}{
		{"small", libfdbc.CStreamingModeSmall},
		{"medium", libfdbc.CStreamingModeMedium},
		{"serial", libfdbc.CStreamingModeSerial},
	}

	for _, sc := range []struct {
		name string
		muts func(pfx string) []rywMut
	}{
		{
			// No local writes: the pure RYW-bypass baseline. If this arm ever fails, the
			// disagreement is in the read path itself, not in the merge.
			name: "no_local_writes",
			muts: func(pfx string) []rywMut { return nil },
		},
		{
			// SHADOW: overwrite existing keys in-transaction. The merged view must prefer the
			// local value at the same key — a byte-accounting bug that charges the stored row
			// and emits the local one (or vice versa) shows up here as a wrong VALUE, which a
			// key-only or count-only assertion cannot see.
			name: "shadow",
			muts: func(pfx string) []rywMut {
				var m []rywMut
				for i := 0; i < 20; i += 2 {
					m = append(m, rywMut{op: "set", key: fmt.Sprintf("%s%04d", pfx, i), val: fat("L")})
				}
				return m
			},
		},
		{
			// EXTEND: insert keys that do not exist in storage, interleaved between stored
			// ones. These rows come only from the write buffer, so they are the ones a
			// byte-limited merge is most likely to drop when the budget runs out mid-window.
			name: "extend",
			muts: func(pfx string) []rywMut {
				var m []rywMut
				for i := 0; i < 20; i++ {
					m = append(m, rywMut{op: "set", key: fmt.Sprintf("%s%04d5", pfx, i), val: fat("N")})
				}
				return m
			},
		},
		{
			// DELETE: individual keys cleared locally. A merge that charges bytes for a row it
			// then suppresses ends its fetch early and can shift every later boundary.
			name: "delete_keys",
			muts: func(pfx string) []rywMut {
				var m []rywMut
				for i := 1; i < 20; i += 3 {
					m = append(m, rywMut{op: "clearkey", key: fmt.Sprintf("%s%04d", pfx, i)})
				}
				return m
			},
		},
		{
			// CLEAR RANGE: a contiguous local hole. The scan must cross it without emitting
			// anything inside and without losing its place on the far side.
			name: "clear_range",
			muts: func(pfx string) []rywMut {
				return []rywMut{{
					op:  "clearrange",
					key: fmt.Sprintf("%s%04d", pfx, 5), end: fmt.Sprintf("%s%04d", pfx, 12),
				}}
			},
		},
		{
			// MIXED: all of the above at once, which is the shape a real scan-and-update
			// transaction produces.
			name: "mixed",
			muts: func(pfx string) []rywMut {
				var m []rywMut
				for i := 0; i < 20; i += 4 {
					m = append(m, rywMut{op: "set", key: fmt.Sprintf("%s%04d", pfx, i), val: fat("L")})
					m = append(m, rywMut{op: "set", key: fmt.Sprintf("%s%04d5", pfx, i), val: fat("N")})
				}
				m = append(m, rywMut{op: "clearkey", key: fmt.Sprintf("%s%04d", pfx, 3)})
				m = append(m, rywMut{
					op:  "clearrange",
					key: fmt.Sprintf("%s%04d", pfx, 14), end: fmt.Sprintf("%s%04d", pfx, 17),
				})
				return m
			},
		},
	} {
		sc := sc
		for _, reverse := range []bool{false, true} {
			reverse := reverse
			dir := "forward"
			if reverse {
				dir = "reverse"
			}
			t.Run(sc.name+"/"+dir, func(t *testing.T) {
				// Not t.Parallel(): these arms share cdb/goDB, whose Close is deferred in the
				// parent. A parallel subtest runs after the parent returns, so the handles
				// would already be closed and every arm would fail with database_closed(2015)
				// before issuing a read — measured, not hypothetical.
				pfx := fmt.Sprintf("rywbl_%s_%s_", sc.name, dir)
				begin, end := []byte(pfx), []byte(pfx+"~")

				// Committed base, visible to both clients.
				const n = 20
				base := make(map[string]string, n)
				seed, err := cdb.CreateTransaction()
				if err != nil {
					t.Fatalf("create seed transaction: %v", err)
				}
				for i := 0; i < n; i++ {
					k := fmt.Sprintf("%s%04d", pfx, i)
					v := fat("S")
					base[k] = v
					seed.Set([]byte(k), []byte(v))
				}
				if err := seed.Commit(); err != nil {
					t.Fatalf("commit base: %v", err)
				}
				seed.Close()

				muts := sc.muts(pfx)
				want := rywModel(base, muts, reverse)
				if len(want) == 0 {
					t.Fatalf("model produced an EMPTY expected set — the arm would assert nothing")
				}

				// ---- pure-Go client: local writes then a full drain in the SAME transaction.
				goTx := goDB.CreateTransaction()
				defer goTx.Cancel()
				rywApplyGo(t, goTx, muts)
				var goRows []fdbclient.KeyValue
				if reverse {
					goRows, _, err = goTx.GetRangeReverse(ctx, begin, end, 0)
				} else {
					goRows, _, err = goTx.GetRange(ctx, begin, end, 0)
				}
				if err != nil {
					t.Fatalf("pure-Go range read: %v", err)
				}
				if len(goRows) != len(want) {
					t.Fatalf("pure-Go returned %d rows, model says %d — the RYW merged view is "+
						"wrong independently of any byte limit", len(goRows), len(want))
				}
				for i := range want {
					if !bytes.Equal([]byte(goRows[i].Key), want[i].Key) ||
						!bytes.Equal(goRows[i].Value, want[i].Value) {
						t.Fatalf("pure-Go row %d = %q (value %d bytes), model says %q (value %d bytes)",
							i, goRows[i].Key, len(goRows[i].Value), want[i].Key, len(want[i].Value))
					}
				}

				// ---- libfdb_c: the same writes, drained under each per-mode byte target.
				for _, m := range modes {
					ctr, err := cdb.CreateTransaction()
					if err != nil {
						t.Fatalf("create transaction: %v", err)
					}
					rywApplyC(ctr, muts)
					cRows, counts := rywDrainC(t, ctr, begin, end, m.mode, reverse)
					ctr.Close()

					if len(cRows) != len(want) {
						t.Errorf("%s: libfdb_c returned %d rows across %d fetches %v, model says %d",
							m.name, len(cRows), len(counts), counts, len(want))
						continue
					}
					for i := range want {
						if !bytes.Equal(cRows[i].Key, want[i].Key) || !bytes.Equal(cRows[i].Value, want[i].Value) {
							t.Errorf("%s: libfdb_c row %d = %q (value %d bytes), model says %q (value %d bytes)",
								m.name, i, cRows[i].Key, len(cRows[i].Value), want[i].Key, len(want[i].Value))
							break
						}
					}

					// minRows: a byte-limited request must never return ZERO rows while data
					// remains, or a drain would spin forever. SMALL's 256-byte target is
					// smaller than a single 300-byte row here, so this arm is exactly the case
					// minRows exists for (NativeAPI.actor.cpp:2875, derived by RYW at
					// ReadYourWrites.actor.cpp:580-597). Only the final confirming fetch may
					// be empty.
					for i, got := range counts {
						if got == 0 && i != len(counts)-1 {
							t.Errorf("%s: fetch %d of %v returned ZERO rows before exhaustion — "+
								"minRows is not forcing progress under a byte target smaller "+
								"than one row", m.name, i, counts)
							break
						}
					}
					t.Logf("MEASURED %-12s %-7s %-6s rows=%d (model %d) fetches=%d division=%v",
						sc.name, dir, m.name, len(cRows), len(want), len(counts), counts)
				}
			})
		}
	}
}
