package bench

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"testing"

	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

// accessed_unreadable (1036) differential vs libfdb_c — RFC-098. Reading a key whose
// value depends on a pending versionstamp throws 1036 through every read-path arm of
// the C++ dispatch (ReadYourWrites.actor.cpp:397-406): regular and snapshot reads
// throw; RYW-disabled reads keep storage semantics; BYPASS_UNREADABLE returns the
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
		goV, goErr := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
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
		// kGo seeded through the GO client for GRV-cache causality — see the
		// getkey subtest comment.
		if _, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
			tx.Set(gofdb.Key(kGo), []byte("storage-v"))
			return nil, nil
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		mustCGo(t, func(tx cgofdb.Transaction) {
			tx.Set(cgofdb.Key(kC), []byte("storage-v"))
		})
		goV, goErr := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
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
		// Seed through the GO client: its always-on GRV cache (filed divergence,
		// TODO.md) does not guarantee causality with cgo-committed data — a
		// cached version older than a cgo seed commit makes the seed invisible
		// (this test caught exactly that in the full suite). The go client's own
		// commit advances its cache past the seed; cgo always fetches a real GRV.
		if _, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
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
		_, goErr := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
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
		// Seeded through the GO client for GRV-cache causality — see the getkey
		// subtest comment. (A stale go read version hid a/b here: the limited
		// scan saw 0 rows, "reached" the pending stamp, and threw a spurious 1036.)
		if _, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
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
		_, goErr := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
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
