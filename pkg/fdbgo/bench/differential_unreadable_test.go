package bench

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"testing"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// accessed_unreadable (1036) differential vs libfdb_c — RFC-098. Reading a key whose
// value depends on a pending versionstamp throws 1036 through every read-path arm of
// the C++ dispatch (ReadYourWrites.actor.cpp:397-406): regular and snapshot reads
// throw; RYW-disabled reads keep storage semantics; BYPASS_UNREADABLE point reads return the
// write-map operand as written (placeholder + 4-byte offset suffix included,
// RYWIterator.cpp:433-449). SetVersionstampedKey marks the ENTIRE candidate stamp
// range unreadable (ReadYourWrites.actor.cpp:2271), so reads of DIFFERENT keys in
// that range throw too. GetRange throws only when the scan REACHES the pending key
// (the :685 limit-break precedes the :692 throw); GetKey throws when the selector
// resolution lands on the pending segment.
//
// Before RFC-098 the Go client resolved pending stamps to ABSENT — a silent wrong-
// answer divergence. This differential going red on that behavior is the revert proof.

const errAccessedUnreadable = 1036

// unreadableSVVOperand returns a SetVersionstampedValue operand with NONZERO
// placeholder bytes (so the bypass byte-compare is meaningful): 10×'A' + LE32(0).
func unreadableSVVOperand() []byte {
	op := append(bytes.Repeat([]byte{'A'}, 10), 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(op[10:], 0)
	return op
}

// unreadableSVKKey returns a SetVersionstampedKey key: prefix + 10-byte placeholder
// + LE32 offset pointing at the placeholder.
func unreadableSVKKey(prefix []byte) []byte {
	key := append(append([]byte(nil), prefix...), make([]byte, 14)...)
	binary.LittleEndian.PutUint32(key[len(key)-4:], uint32(len(prefix)))
	return key
}

func TestDifferential_BypassUnreadableIsPointOnly(t *testing.T) {
	t.Parallel()
	for _, snapshot := range []bool{false, true} {
		for _, overwrite := range []bool{false, true} {
			for _, read := range []string{"key", "range", "reverse"} {
				t.Run(fmt.Sprintf("snapshot=%t/overwrite=%t/%s", snapshot, overwrite, read), func(t *testing.T) {
					t.Parallel()
					key := []byte(t.Name())
					end := append(bytes.Clone(key), 0)
					goCode := goErrCode(func(tx gofdb.Transaction) error {
						if err := tx.Options().SetBypassUnreadable(); err != nil {
							return err
						}
						tx.SetVersionstampedValue(gofdb.Key(key), unreadableSVVOperand())
						if overwrite {
							tx.Set(gofdb.Key(key), []byte("plain"))
						}
						var reader gofdb.ReadTransaction = tx
						if snapshot {
							reader = tx.Snapshot()
						}
						if read == "key" {
							_, err := reader.GetKey(gofdb.FirstGreaterOrEqual(gofdb.Key(key))).Get()
							return err
						}
						_, err := reader.GetRange(gofdb.KeyRange{Begin: gofdb.Key(key), End: gofdb.Key(end)}, gofdb.RangeOptions{Reverse: read == "reverse"}).GetSliceWithError()
						return err
					})
					cCode := cgoErrCode(func(tx cgofdb.Transaction) error {
						if err := tx.Options().SetBypassUnreadable(); err != nil {
							return err
						}
						tx.SetVersionstampedValue(cgofdb.Key(key), unreadableSVVOperand())
						if overwrite {
							tx.Set(cgofdb.Key(key), []byte("plain"))
						}
						var reader cgofdb.ReadTransaction = tx
						if snapshot {
							reader = tx.Snapshot()
						}
						if read == "key" {
							_, err := reader.GetKey(cgofdb.FirstGreaterOrEqual(cgofdb.Key(key))).Get()
							return err
						}
						_, err := reader.GetRange(cgofdb.KeyRange{Begin: cgofdb.Key(key), End: cgofdb.Key(end)}, cgofdb.RangeOptions{Reverse: read == "reverse"}).GetSliceWithError()
						return err
					})
					// Only GetValueReq enables iterator bypass (C++ RYW :98-99).
					if goCode != 1036 || cCode != 1036 {
						t.Fatalf("unreadable range/selector: go=%d cgo=%d, want both 1036", goCode, cCode)
					}
				})
			}
		}
	}
}

func TestDifferential_Unreadable(t *testing.T) {
	t.Parallel()
	pfx := fmt.Sprintf("differ_unreadable_%d_", os.Getpid())
	clearPrefix(t, pfx)

	t.Run("svv_get", func(t *testing.T) {
		t.Parallel()
		k := pfx + "svv_get"
		goCode := goErrCode(func(tx gofdb.Transaction) error {
			tx.SetVersionstampedValue(gofdb.Key(k), unreadableSVVOperand())
			_, err := tx.Get(gofdb.Key(k)).Get()
			return err
		})
		cCode := cgoErrCode(func(tx cgofdb.Transaction) error {
			tx.SetVersionstampedValue(cgofdb.Key(k), unreadableSVVOperand())
			_, err := tx.Get(cgofdb.Key(k)).Get()
			return err
		})
		if goCode != cCode || goCode != errAccessedUnreadable {
			t.Fatalf("same-txn Get of pending SVV: go=%d cgo=%d, want both %d", goCode, cCode, errAccessedUnreadable)
		}
	})

	t.Run("svv_snapshot_get", func(t *testing.T) {
		// Snapshot reads with snapshot-RYW enabled (the default) traverse the write
		// map and throw too (C++ :400-405).
		t.Parallel()
		k := pfx + "svv_snap"
		goCode := goErrCode(func(tx gofdb.Transaction) error {
			tx.SetVersionstampedValue(gofdb.Key(k), unreadableSVVOperand())
			_, err := tx.Snapshot().Get(gofdb.Key(k)).Get()
			return err
		})
		cCode := cgoErrCode(func(tx cgofdb.Transaction) error {
			tx.SetVersionstampedValue(cgofdb.Key(k), unreadableSVVOperand())
			_, err := tx.Snapshot().Get(cgofdb.Key(k)).Get()
			return err
		})
		if goCode != cCode || goCode != errAccessedUnreadable {
			t.Fatalf("snapshot Get of pending SVV: go=%d cgo=%d, want both %d", goCode, cCode, errAccessedUnreadable)
		}
	})

	t.Run("svk_other_key_in_range", func(t *testing.T) {
		// SVK marks the whole candidate stamp range unreadable: a Get of a DIFFERENT
		// key inside it throws.
		t.Parallel()
		svkPfx := []byte(pfx + "svk/")
		other := append(append([]byte(nil), svkPfx...), bytes.Repeat([]byte{0x7f}, 10)...)
		goCode := goErrCode(func(tx gofdb.Transaction) error {
			tx.SetVersionstampedKey(gofdb.Key(unreadableSVKKey(svkPfx)), []byte("v"))
			_, err := tx.Get(gofdb.Key(other)).Get()
			return err
		})
		cCode := cgoErrCode(func(tx cgofdb.Transaction) error {
			tx.SetVersionstampedKey(cgofdb.Key(unreadableSVKKey(svkPfx)), []byte("v"))
			_, err := tx.Get(cgofdb.Key(other)).Get()
			return err
		})
		if goCode != cCode || goCode != errAccessedUnreadable {
			t.Fatalf("Get of other key in pending SVK range: go=%d cgo=%d, want both %d", goCode, cCode, errAccessedUnreadable)
		}
	})

	t.Run("svv_bypass_returns_operand", func(t *testing.T) {
		// BYPASS_UNREADABLE returns the write-map value as written, INCLUDING the
		// trailing 4-byte offset suffix.
		t.Parallel()
		k := pfx + "svv_bypass"
		op := unreadableSVVOperand()
		goV, goErr := goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
			tx := txw.(gofdb.Transaction)
			if err := tx.Options().SetBypassUnreadable(); err != nil {
				return nil, err
			}
			tx.SetVersionstampedValue(gofdb.Key(k), op)
			return tx.Get(gofdb.Key(k)).Get()
		})
		cV, cErr := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
			if err := tx.Options().SetBypassUnreadable(); err != nil {
				return nil, err
			}
			tx.SetVersionstampedValue(cgofdb.Key(k), op)
			v, err := tx.Get(cgofdb.Key(k)).Get()
			return []byte(v), err
		})
		if goErr != nil || cErr != nil {
			t.Fatalf("bypass Get: goErr=%v cErr=%v", goErr, cErr)
		}
		if !bytes.Equal(goV.([]byte), cV.([]byte)) || !bytes.Equal(goV.([]byte), op) {
			t.Fatalf("bypass Get: go=%x cgo=%x, want both the operand as written %x", goV, cV, op)
		}
	})

	t.Run("svv_rywdisabled_reads_storage", func(t *testing.T) {
		// RYW-disabled transactions keep storage semantics — no throw, storage
		// value. Per-client keys: each Transact COMMITS its pending SVV, so a
		// shared key would be stamped by whichever client runs first.
		t.Parallel()
		kGo, kC := pfx+"svv_rywoff_go", pfx+"svv_rywoff_c"
		// kGo seeded through the GO client (key ownership; the pre-RFC-104
		// GRV-cache causality requirement is gone) — see the getkey subtest comment.
		if _, err := goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
			tx := txw.(gofdb.Transaction)
			tx.Set(gofdb.Key(kGo), []byte("storage-v"))
			return nil, nil
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		mustCGo(t, func(tx cgofdb.Transaction) {
			tx.Set(cgofdb.Key(kC), []byte("storage-v"))
		})
		goV, goErr := goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
			tx := txw.(gofdb.Transaction)
			if err := tx.Options().SetReadYourWritesDisable(); err != nil {
				return nil, err
			}
			tx.SetVersionstampedValue(gofdb.Key(kGo), unreadableSVVOperand())
			return tx.Get(gofdb.Key(kGo)).Get()
		})
		cV, cErr := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
			if err := tx.Options().SetReadYourWritesDisable(); err != nil {
				return nil, err
			}
			tx.SetVersionstampedValue(cgofdb.Key(kC), unreadableSVVOperand())
			v, err := tx.Get(cgofdb.Key(kC)).Get()
			return []byte(v), err
		})
		if goErr != nil || cErr != nil {
			t.Fatalf("rywDisabled Get: goErr=%v cErr=%v", goErr, cErr)
		}
		if !bytes.Equal(goV.([]byte), cV.([]byte)) || string(goV.([]byte)) != "storage-v" {
			t.Fatalf("rywDisabled Get: go=%q cgo=%q, want both the storage value", goV, cV)
		}
	})

	t.Run("getkey", func(t *testing.T) {
		// firstGreaterOrEqual(stamp key) lands ON the pending segment → 1036;
		// firstGreaterThan resolves past it without touching it → the fence key.
		// The failed FGE also POISONS the transaction (its errored future stays
		// in ryw->reading, which commit() waits on): even though the closure
		// swallows the error, the COMMIT fails with the same 1036 — Transact
		// itself must return it on both clients.
		t.Parallel()
		k := pfx + "getkey"
		fence := k + "\x01fence"
		// Seed through the GO client (per-client key isolation). NOTE: pre-RFC-104
		// this was REQUIRED for causality — the Go client's then-always-on GRV
		// cache could serve a version older than a cgo seed commit, hiding it.
		// RFC-104 made the cache opt-in/default-off, so a default Go read now sees
		// cgo-committed data directly (pinned by TestDifferential_GRVCacheDefaultSeesCgoSeed);
		// seeding through go here is now just key-ownership hygiene, not a workaround.
		if _, err := goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
			tx := txw.(gofdb.Transaction)
			tx.Set(gofdb.Key(fence), []byte("f"))
			return nil, nil
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}

		type res struct {
			fgeCode int
			fgtKey  []byte
			fgtErr  error
		}
		var goR, cR res
		_, goErr := goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
			tx := txw.(gofdb.Transaction)
			tx.SetVersionstampedValue(gofdb.Key(k), unreadableSVVOperand())
			_, fgeErr := tx.GetKey(gofdb.KeySelector{Key: gofdb.Key(k), OrEqual: false, Offset: 1}).Get()
			fgtKey, fgtErr := tx.GetKey(gofdb.KeySelector{Key: gofdb.Key(k), OrEqual: true, Offset: 1}).Get()
			goR = res{fdbErrorCode(fgeErr), fgtKey, fgtErr}
			return nil, nil
		})
		_, cErr := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
			tx.SetVersionstampedValue(cgofdb.Key(k), unreadableSVVOperand())
			_, fgeErr := tx.GetKey(cgofdb.KeySelector{Key: cgofdb.Key(k), OrEqual: false, Offset: 1}).Get()
			fgtKey, fgtErr := tx.GetKey(cgofdb.KeySelector{Key: cgofdb.Key(k), OrEqual: true, Offset: 1}).Get()
			cR = res{fdbErrorCode(fgeErr), fgtKey, fgtErr}
			return nil, nil
		})
		if goR.fgeCode != cR.fgeCode || goR.fgeCode != errAccessedUnreadable {
			t.Fatalf("FGE(stamp key): go=%d cgo=%d, want both %d", goR.fgeCode, cR.fgeCode, errAccessedUnreadable)
		}
		if goR.fgtErr != nil || cR.fgtErr != nil {
			t.Fatalf("FGT(stamp key) resolves past the segment: goErr=%v cErr=%v", goR.fgtErr, cR.fgtErr)
		}
		if !bytes.Equal(goR.fgtKey, cR.fgtKey) || !bytes.Equal(goR.fgtKey, []byte(fence)) {
			t.Fatalf("FGT(stamp key): go=%q cgo=%q, want both the fence key %q", goR.fgtKey, cR.fgtKey, fence)
		}
		if goCode, cCode := fdbErrorCode(goErr), fdbErrorCode(cErr); goCode != cCode || goCode != errAccessedUnreadable {
			t.Fatalf("commit after swallowed 1036 read (reading poisoning): go=%d cgo=%d, want both %d", goCode, cCode, errAccessedUnreadable)
		}
	})

	t.Run("getkey_from_inside_svk_range_head", func(t *testing.T) {
		// FDB-C++ review catch on RFC-098: the SVK candidate range's head
		// [begin, pending entry) holds no write-map key; a reverse selector
		// anchored inside it must still classify the unreadable range (Go
		// needed explicit unreadableRanges boundaries in the selector walk;
		// libfdb_c gets them from addUnmodifiedAndUnreadableRange's nodes).
		t.Parallel()
		hPfx := pfx + "gkh/"
		svkPfx := []byte(hPfx + "b")
		// minVersion 0 on a fresh txn → range begin B = svkPfx + 10 zero
		// bytes; the entry sits at B + 4 suffix bytes. Anchor inside (B, entry).
		inside := append(append([]byte(nil), svkPfx...), make([]byte, 10)...)
		inside = append(inside, 0x00, 0x00, 0x01)
		goCode := goErrCode(func(tx gofdb.Transaction) error {
			tx.SetVersionstampedKey(gofdb.Key(unreadableSVKKey(svkPfx)), []byte("v"))
			_, err := tx.GetKey(gofdb.KeySelector{Key: gofdb.Key(inside), OrEqual: false, Offset: 0}).Get()
			return err
		})
		cCode := cgoErrCode(func(tx cgofdb.Transaction) error {
			tx.SetVersionstampedKey(cgofdb.Key(unreadableSVKKey(svkPfx)), []byte("v"))
			_, err := tx.GetKey(cgofdb.KeySelector{Key: cgofdb.Key(inside), OrEqual: false, Offset: 0}).Get()
			return err
		})
		if goCode != cCode || goCode != errAccessedUnreadable {
			t.Fatalf("lastLessThan(inside SVK range head): go=%d cgo=%d, want both %d", goCode, cCode, errAccessedUnreadable)
		}
	})

	t.Run("getrange_reach", func(t *testing.T) {
		// A limited scan that stops BEFORE the pending key succeeds with the rows
		// before it; an unlimited scan reaches it → 1036; reverse hits it first even
		// at limit 1.
		t.Parallel()
		rPfx := pfx + "reach/"
		a, b, z := rPfx+"a", rPfx+"b", rPfx+"z"
		// Seeded through the GO client (key ownership) — see the getkey subtest
		// comment. (Pre-RFC-104 a stale go read version could hide a/b here: a
		// limited scan saw 0 rows, "reached" the pending stamp, and threw a
		// spurious 1036. RFC-104's fresh-GRV default removed that hazard.)
		if _, err := goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
			tx := txw.(gofdb.Transaction)
			tx.Set(gofdb.Key(a), []byte("va"))
			tx.Set(gofdb.Key(b), []byte("vb"))
			return nil, nil
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}

		type res struct {
			limitedKeys   [][]byte
			limitedErr    error
			unlimitedCode int
			reverseCode   int
		}
		goRange, err := gofdb.PrefixRange([]byte(rPfx))
		if err != nil {
			t.Fatalf("go PrefixRange: %v", err)
		}
		cRange, err := cgofdb.PrefixRange([]byte(rPfx))
		if err != nil {
			t.Fatalf("cgo PrefixRange: %v", err)
		}
		var g, c res
		_, goErr := goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
			tx := txw.(gofdb.Transaction)
			tx.SetVersionstampedValue(gofdb.Key(z), unreadableSVVOperand())
			limited, limErr := tx.GetRange(goRange, gofdb.RangeOptions{Limit: 2}).GetSliceWithError()
			var keys [][]byte
			for _, kv := range limited {
				keys = append(keys, kv.Key)
			}
			_, unlimErr := tx.GetRange(goRange, gofdb.RangeOptions{}).GetSliceWithError()
			_, revErr := tx.GetRange(goRange, gofdb.RangeOptions{Limit: 1, Reverse: true}).GetSliceWithError()
			g = res{keys, limErr, fdbErrorCode(unlimErr), fdbErrorCode(revErr)}
			return nil, nil
		})
		_, cErr := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
			tx.SetVersionstampedValue(cgofdb.Key(z), unreadableSVVOperand())
			limited, limErr := tx.GetRange(cRange, cgofdb.RangeOptions{Limit: 2, Mode: cgofdb.StreamingModeWantAll}).GetSliceWithError()
			var keys [][]byte
			for _, kv := range limited {
				keys = append(keys, kv.Key)
			}
			_, unlimErr := tx.GetRange(cRange, cgofdb.RangeOptions{Mode: cgofdb.StreamingModeWantAll}).GetSliceWithError()
			_, revErr := tx.GetRange(cRange, cgofdb.RangeOptions{Limit: 1, Reverse: true, Mode: cgofdb.StreamingModeWantAll}).GetSliceWithError()
			c = res{keys, limErr, fdbErrorCode(unlimErr), fdbErrorCode(revErr)}
			return nil, nil
		})
		if g.limitedErr != nil || c.limitedErr != nil {
			t.Fatalf("limited scan stopping before the stamp: goErr=%v cErr=%v", g.limitedErr, c.limitedErr)
		}
		if len(g.limitedKeys) != 2 || len(c.limitedKeys) != 2 ||
			!bytes.Equal(g.limitedKeys[0], c.limitedKeys[0]) || !bytes.Equal(g.limitedKeys[1], c.limitedKeys[1]) ||
			string(g.limitedKeys[0]) != a || string(g.limitedKeys[1]) != b {
			t.Fatalf("limited scan rows: go=%q cgo=%q, want both [%q %q]", g.limitedKeys, c.limitedKeys, a, b)
		}
		if g.unlimitedCode != c.unlimitedCode || g.unlimitedCode != errAccessedUnreadable {
			t.Fatalf("unlimited scan reaching the stamp: go=%d cgo=%d, want both %d", g.unlimitedCode, c.unlimitedCode, errAccessedUnreadable)
		}
		if g.reverseCode != c.reverseCode || g.reverseCode != errAccessedUnreadable {
			t.Fatalf("reverse limit-1 scan (stamp first): go=%d cgo=%d, want both %d", g.reverseCode, c.reverseCode, errAccessedUnreadable)
		}
		// The two failed scans poisoned ryw->reading: the commit — and so
		// Transact itself — fails with the same 1036 on both clients.
		if goCode, cCode := fdbErrorCode(goErr), fdbErrorCode(cErr); goCode != cCode || goCode != errAccessedUnreadable {
			t.Fatalf("commit after swallowed 1036 reads (reading poisoning): go=%d cgo=%d, want both %d", goCode, cCode, errAccessedUnreadable)
		}
	})
}

// Small maps to C mode 1 (Go binding transaction.go:288), whose byte target is
// 256 (fdb_c.cpp:1002). Cold prefixes stop on byte exhaustion or unknown gaps.
func TestDifferential_ColdPrefixByteLimitBeforeUnreadable(t *testing.T) {
	t.Parallel()
	for _, reverse := range []bool{false, true} {
		for _, snapshot := range []bool{false, true} {
			for _, bypass := range []bool{false, true} {
				for _, scenario := range []string{"oversized", "budget_one_beyond", "ample_bytes"} {
					t.Run(fmt.Sprintf("reverse=%t/snapshot=%t/bypass=%t/%s", reverse, snapshot, bypass, scenario), func(t *testing.T) {
						t.Parallel()
						prefix := fmt.Sprintf("diff_coldbyte_%d_%t_%t_%t_%s/", os.Getpid(), reverse, snapshot, bypass, scenario)
						begin, end, stamp := prefix+"a", prefix+"~", prefix+"m"
						firstKey := begin
						if reverse {
							firstKey = prefix + "z"
						}
						valueSize := 512
						switch scenario {
						case "budget_one_beyond":
							// RYW consumes 255 bytes, but storage's larger per-row
							// overhead can truncate before the unknown gap to the SVV.
							valueSize = 255 - 8 - len(firstKey)
						case "ample_bytes":
							valueSize = 1
						}
						if valueSize <= 0 {
							t.Fatalf("key length %d leaves no value budget", len(firstKey))
						}
						value := bytes.Repeat([]byte{'v'}, valueSize)
						if _, err := goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
							tx := txw.(gofdb.Transaction)
							if err := tx.Options().SetTimeout(30000); err != nil {
								return nil, err
							}
							tx.ClearRange(gofdb.KeyRange{Begin: gofdb.Key(begin), End: gofdb.Key(end)})
							tx.Set(gofdb.Key(firstKey), value)
							return nil, nil
						}); err != nil {
							t.Fatalf("seed cold storage row: %v", err)
						}

						type pageResult struct {
							firstAdvanced bool
							key, value    []byte
							firstErr      error
							nextErr       error
						}
						cResult := func() pageResult {
							tx, err := cgoClient.CreateTransaction()
							if err != nil {
								t.Fatal(err)
							}
							defer tx.Cancel()
							if err := tx.Options().SetTimeout(30000); err != nil {
								t.Fatal(err)
							}
							if bypass {
								if err := tx.Options().SetBypassUnreadable(); err != nil {
									t.Fatal(err)
								}
							}
							tx.SetVersionstampedValue(cgofdb.Key(stamp), unreadableSVVOperand())
							var reader cgofdb.ReadTransaction = tx
							if snapshot {
								reader = tx.Snapshot()
							}
							it := reader.GetRange(cgofdb.KeyRange{Begin: cgofdb.Key(begin), End: cgofdb.Key(end)}, cgofdb.RangeOptions{Mode: cgofdb.StreamingModeSmall, Reverse: reverse}).Iterator()
							r := pageResult{firstAdvanced: it.Advance()}
							if r.firstAdvanced {
								kv, err := it.Get()
								r.key, r.value, r.firstErr = kv.Key, kv.Value, err
								// Apple's Advance returns true on an error; Get surfaces it.
								if err == nil && it.Advance() {
									_, r.nextErr = it.Get()
								}
							}
							return r
						}()
						goResult := func() pageResult {
							tx, err := goClient.CreateTransaction()
							if err != nil {
								t.Fatal(err)
							}
							defer tx.Cancel()
							if err := tx.Options().SetTimeout(30000); err != nil {
								t.Fatal(err)
							}
							if bypass {
								if err := tx.Options().SetBypassUnreadable(); err != nil {
									t.Fatal(err)
								}
							}
							tx.SetVersionstampedValue(gofdb.Key(stamp), unreadableSVVOperand())
							var reader gofdb.ReadTransaction = tx
							if snapshot {
								reader = tx.Snapshot()
							}
							it := reader.GetRange(gofdb.KeyRange{Begin: gofdb.Key(begin), End: gofdb.Key(end)}, gofdb.RangeOptions{Mode: gofdb.StreamingModeSmall, Reverse: reverse}).Iterator()
							r := pageResult{firstAdvanced: it.Advance()}
							kv, err := it.Get()
							r.key, r.value, r.firstErr = kv.Key, kv.Value, err
							if r.firstAdvanced && err == nil {
								// Go's Advance returns false on error; Get still reports it.
								it.Advance()
								_, r.nextErr = it.Get()
							}
							return r
						}()
						for _, result := range []struct {
							name string
							pageResult
						}{{"cgo", cResult}, {"go", goResult}} {
							if scenario == "ample_bytes" {
								if code := fdbErrorCode(result.firstErr); code != 1036 || len(result.key) != 0 || len(result.value) != 0 {
									t.Errorf("%s first page with ample budget: key=%q error=%v (code=%d), want no row and accessed_unreadable (1036)", result.name, result.key, result.firstErr, code)
								}
								continue
							}
							if !result.firstAdvanced || result.firstErr != nil || !bytes.Equal(result.key, []byte(firstKey)) || !bytes.Equal(result.value, value) {
								t.Errorf("%s first page: advanced=%t key=%q valueLen=%d error=%v, want cold row %q with its %d-byte value", result.name, result.firstAdvanced, result.key, len(result.value), result.firstErr, firstKey, valueSize)
							}
							if code := fdbErrorCode(result.nextErr); code != 1036 {
								t.Errorf("%s next page: error=%v (code=%d), want accessed_unreadable (1036)", result.name, result.nextErr, code)
							}
						}
					})
				}
			}
		}
	}
}
