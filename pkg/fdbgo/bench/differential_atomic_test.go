package bench

import (
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"

	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

// Atomic-fold differential vs libfdb_c across operand/base widths and edge operands — RFC-062.
//
// Atomic fold semantics are the wire hard line: the folded value is what gets persisted and what
// Java/C clients read. The Go fold (pkg/fdbgo/client/ryw.go: doAdd/doAnd/doMin/…) is a faithful
// 1:1 port of C++ Atomic.h, including the subtle "result width = operand width" truncation
// (Add/And/Or/Xor/Max/Min) and absent/empty semantics. But the existing differential
// (WriteBattery: missing-key 8-byte; RYWCoalescing: 8/1-byte) never proves that identity across
// the WIDTH dimension — where a port is most likely to drift (a fast-path/slow-path split, an
// off-by-one byte loop, a zero-pad-on-existing-win, an operand-longer-than-base).
//
// CRITICAL — where the Go fold actually runs. tx.Set/tx.Atomic append RAW mutations to
// tx.mutations (transaction.go:795); COMMIT ships [SetValue, AddValue, …] and the SERVER folds
// them. So a commit-then-read-back differential exercises the server fold + mutation wire format,
// NOT Go's doAdd/doMin/etc. Go's client-side fold runs ONLY on a read WITHIN the txn
// (read-your-writes: Site B coalesce / resolveAtomics). Therefore each case here does an IN-TXN
// read after Set(base)+Atomic(operand) — that read goes through Go's fold (the thing under test)
// — AND a committed read-back (server fold). Both are byte-compared go-vs-cgo. Fault-injecting a
// fold function (e.g. doAdd's width) makes the IN-TXN comparison go red; the committed
// comparison is the weaker wire-format check. (This in-txn requirement was found by the RFC-062
// teeth-check: a commit+read-back version passed even with doAdd broken.)
//
// The facade tx.Add/tx.And/tx.Min emit AddValue/AndV2/MinV2 at API 730 — the codes a modern app
// sends.

// atomicFoldCase: Set(base)+Atomic(op, operand); base==nil means no Set, so the atomic folds
// against the STORAGE value (here always absent — the per-case prefix is fresh). operand drives
// the result width for the width-sensitive ops.
type atomicFoldCase struct {
	name    string
	op      int
	base    []byte // nil → no Set (atomic folds against storage-absent)
	operand []byte
}

func bytesOf(n int, fill byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = fill
	}
	return b
}

func hexOrAbsent(v []byte) string {
	if v == nil {
		return "ABSENT"
	}
	return hex.EncodeToString(v)
}

func applyGoAtomic(tx gofdb.Transaction, op int, k gofdb.Key, operand []byte) {
	switch op {
	case fzAdd:
		tx.Add(k, operand)
	case fzAnd:
		tx.And(k, operand)
	case fzOr:
		tx.Or(k, operand)
	case fzXor:
		tx.Xor(k, operand)
	case fzMax:
		tx.Max(k, operand)
	case fzMin:
		tx.Min(k, operand)
	case fzByteMin:
		tx.ByteMin(k, operand)
	case fzByteMax:
		tx.ByteMax(k, operand)
	case fzAppendIfFits:
		tx.AppendIfFits(k, operand)
	case fzCompareAndClear:
		tx.CompareAndClear(k, operand)
	}
}

func applyCgoAtomic(tx cgofdb.Transaction, op int, k cgofdb.Key, operand []byte) {
	switch op {
	case fzAdd:
		tx.Add(k, operand)
	case fzAnd:
		tx.And(k, operand)
	case fzOr:
		tx.Or(k, operand)
	case fzXor:
		tx.Xor(k, operand)
	case fzMax:
		tx.Max(k, operand)
	case fzMin:
		tx.Min(k, operand)
	case fzByteMin:
		tx.ByteMin(k, operand)
	case fzByteMax:
		tx.ByteMax(k, operand)
	case fzAppendIfFits:
		tx.AppendIfFits(k, operand)
	case fzCompareAndClear:
		tx.CompareAndClear(k, operand)
	}
}

// goFold runs Set(base)+Atomic in one txn and reads the key WITHIN that txn (exercising Go's
// client-side fold via read-your-writes), then commits and reads back (server fold). Returns the
// (in-txn, committed) folded values as hex/"ABSENT".
func goFold(t *testing.T, pfx string, c atomicFoldCase) (inTxn, committed string) {
	t.Helper()
	k := gofdb.Key(pfx + "go_" + c.name)
	if _, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
		if c.base != nil {
			tx.Set(k, c.base)
		}
		applyGoAtomic(tx, c.op, k, c.operand)
		v, e := tx.Get(k).Get() // IN-TXN read → Go's RYW fold (doAdd/doMin/…)
		if e != nil {
			return nil, e
		}
		inTxn = hexOrAbsent(v)
		return nil, nil
	}); err != nil {
		t.Fatalf("go %s txn: %v", c.name, err)
	}
	cv, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) { return tx.Get(k).Get() })
	if err != nil {
		t.Fatalf("go %s read-back: %v", c.name, err)
	}
	b, _ := cv.([]byte)
	return inTxn, hexOrAbsent(b)
}

func cgoFold(t *testing.T, pfx string, c atomicFoldCase) (inTxn, committed string) {
	t.Helper()
	// Distinct prefix from goFold: a missing-key (base==nil) case folds against STORAGE, so the
	// two clients must not share a key (else the second client's fold sees the first's committed
	// value instead of absent).
	k := cgofdb.Key(pfx + "c_" + c.name)
	if _, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
		if c.base != nil {
			tx.Set(k, c.base)
		}
		applyCgoAtomic(tx, c.op, k, c.operand)
		v, e := tx.Get(k).Get()
		if e != nil {
			return nil, e
		}
		inTxn = hexOrAbsent(v)
		return nil, nil
	}); err != nil {
		t.Fatalf("cgo %s txn: %v", c.name, err)
	}
	cv, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) { return tx.Get(k).Get() })
	if err != nil {
		t.Fatalf("cgo %s read-back: %v", c.name, err)
	}
	b, _ := cv.([]byte)
	return inTxn, hexOrAbsent(b)
}

func runAtomicFoldCases(t *testing.T, cases []atomicFoldCase) {
	t.Helper()
	ns := strings.ReplaceAll(t.Name(), "/", "_")
	pfx := fmt.Sprintf("atomfold_%d_%s_", os.Getpid(), ns)
	// Clear the whole namespace (covers both the go_ and c_ sub-prefixes) before the cases run.
	// Without this, re-running the binary in one process (e.g. go test -count=2) would leave the
	// prior run's committed values in storage, and a base==nil (missing-key) case would fold over
	// them instead of the intended storage-absent path — passing while testing the wrong scenario
	// (codex review).
	clearPrefix(t, pfx)
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			gi, gc := goFold(t, pfx, c)
			ci, cc := cgoFold(t, pfx, c)
			// IN-TXN read = Go's client-side fold (the thing under test). This is the assertion
			// fault-injecting doAdd/doMin/etc. must break.
			if gi != ci {
				t.Fatalf("%s: IN-TXN client fold differs (doAdd/doMin/…): go=%s cgo=%s", c.name, gi, ci)
			}
			// Committed value = server fold of the raw mutation stream + wire format.
			if gc != cc {
				t.Fatalf("%s: committed value differs (server fold/wire format): go=%s cgo=%s", c.name, gc, cc)
			}
			// Read-your-writes consistency: the in-txn view must equal what got committed.
			if gi != gc {
				t.Fatalf("%s: go in-txn (%s) != committed (%s) — RYW inconsistency", c.name, gi, gc)
			}
		})
	}
}

// TestDifferential_AtomicFoldWidths exercises the width-sensitive arithmetic/bitwise ops across
// (base, operand) width combinations: operand shorter than base, operand longer, long operand
// past Go's 8-byte fast path, symmetric non-8, empty operand, empty base, and carry/borrow
// boundaries.
func TestDifferential_AtomicFoldWidths(t *testing.T) {
	t.Parallel()
	cases := []atomicFoldCase{
		// --- Add: 8-byte fast path vs byte-loop slow path; carry discarded at operand width ---
		// (operand-shorter (8,1) is already covered by RYWCoalescing's set_then_add — not repeated.)
		{"add_b1_o8", fzAdd, []byte{0x0a}, bytesOf(8, 0x00)},                                      // operand longer → result 8 bytes, base zero-extended
		{"add_b8_o16", fzAdd, bytesOf(8, 0x01), bytesOf(16, 0x02)},                                // long operand past fast path (slow loop)
		{"add_b16_o8", fzAdd, bytesOf(16, 0xff), bytesOf(8, 0x01)},                                // base longer than operand (high base bytes dropped)
		{"add_b3_o3", fzAdd, []byte{0xff, 0xff, 0x00}, []byte{0x01, 0x00, 0x00}},                  // slow path, multi-byte carry
		{"add_carry_o1_wrap", fzAdd, []byte{0xff}, []byte{0x01}},                                  // 1-byte wrap (carry discarded)
		{"add_carry_o8_wrap", fzAdd, bytesOf(8, 0xff), append([]byte{0x01}, bytesOf(7, 0x00)...)}, // 8-byte fast-path wrap → all 0x00
		{"add_empty_operand", fzAdd, bytesOf(8, 0x0a), []byte{}},                                  // empty operand → empty result
		{"add_empty_base", fzAdd, []byte{}, bytesOf(8, 0x05)},                                     // present-empty base → treated as 0
		{"add_missing_o8", fzAdd, nil, bytesOf(8, 0x05)},                                          // in-txn folds doAdd(nil,·) via resolveAtomics; committed = server fold

		// --- And (AndV2 at API 730): operand-longer-than-base → trailing 0x00 ---
		{"and_b8_o1", fzAnd, bytesOf(8, 0xff), []byte{0x0f}},
		{"and_b1_o3", fzAnd, []byte{0xff}, bytesOf(3, 0xff)}, // {0xff,0x00,0x00} — operand longer, trailing zeros
		{"and_b3_o8", fzAnd, []byte{0xf0, 0x0f, 0xff}, bytesOf(8, 0xaa)},
		{"and_b16_o8", fzAnd, bytesOf(16, 0xff), bytesOf(8, 0x0f)},
		{"and_empty_operand", fzAnd, bytesOf(8, 0xff), []byte{}},
		// present-empty base: AndV2 must fold via doAnd (→ all-zero result), NOT return operand
		// (the absent path). This is the one place absent ≠ present-empty bites (FDB-C++ dev).
		{"and_empty_base", fzAnd, []byte{}, bytesOf(8, 0xff)},
		{"and_missing_o8", fzAnd, nil, bytesOf(8, 0xff)}, // in-txn folds doAndV2(nil,·)→operand via resolveAtomics; committed = server fold

		// --- Or: operand-longer-than-base → trailing param bytes copied ---
		{"or_b8_o1", fzOr, bytesOf(8, 0xf0), []byte{0x0f}},
		{"or_b1_o3", fzOr, []byte{0xf0}, bytesOf(3, 0x0f)}, // {0xff,0x0f,0x0f}
		{"or_b3_o8", fzOr, []byte{0xf0, 0x0f, 0x00}, bytesOf(8, 0xaa)},
		{"or_empty_operand", fzOr, bytesOf(8, 0xff), []byte{}},
		{"or_empty_base", fzOr, []byte{}, bytesOf(4, 0x0f)},
		{"or_missing_o8", fzOr, nil, bytesOf(8, 0x0f)},

		// --- Xor: operand-longer-than-base → trailing param bytes copied ---
		{"xor_b8_o1", fzXor, bytesOf(8, 0xff), []byte{0xaa}},
		{"xor_b1_o3", fzXor, []byte{0xff}, bytesOf(3, 0xaa)}, // {0x55,0xaa,0xaa}
		{"xor_b3_o8", fzXor, []byte{0xff, 0x00, 0xff}, bytesOf(8, 0xaa)},
		{"xor_empty_operand", fzXor, bytesOf(8, 0xff), []byte{}},
		{"xor_empty_base", fzXor, []byte{}, bytesOf(4, 0xaa)},
		{"xor_missing_o8", fzXor, nil, bytesOf(8, 0xaa)},

		// --- Max: compares only operand-width LOW bytes; zero-pad-existing-win truncates existing ---
		// base low bytes {0,0,0,...,9-high}; operand 3-byte {1,0,0}: only low 3 bytes compared →
		// operand[0]=1 > base[0]=0 → operand wins (high base byte 9 is IGNORED — width truncation).
		{"max_b8high_o3_operandwins", fzMax, append([]byte{0, 0, 0, 0, 0, 0, 0}, 0x09), []byte{0x01, 0x00, 0x00}},
		// base {5,0,0}; operand {3,0}: low 2 bytes → operand[0]=3 < base[0]=5 → existing wins,
		// result = existing truncated to operand width (base WIDER than operand) = {5,0}.
		{"max_b3_o2_existingwins_trunc", fzMax, []byte{0x05, 0x00, 0x00}, []byte{0x03, 0x00}},
		// base {9,0,0} (3B); operand {5,0,0,0,0,0,0,0} (8B, high bytes 0): existing wins, result =
		// existing ZERO-PADDED to operand width (base SHORTER than operand) = {9,0,0,0,0,0,0,0}.
		{"max_b3_o8_existingwins_zeropad", fzMax, []byte{0x09, 0x00, 0x00}, append([]byte{0x05}, bytesOf(7, 0x00)...)},
		{"max_b8_o8_equal", fzMax, bytesOf(8, 0x07), bytesOf(8, 0x07)},
		{"max_b1_o8_operandwins_earlyreturn", fzMax, []byte{0xff}, append([]byte{0x00}, bytesOf(7, 0x01)...)}, // operand high bytes nonzero → operand wins (early return)
		{"max_empty_operand", fzMax, bytesOf(8, 0x07), []byte{}},
		{"max_empty_base", fzMax, []byte{}, bytesOf(8, 0x07)},
		{"max_missing_o8", fzMax, nil, bytesOf(8, 0x07)}, // in-txn folds doMax(nil,·) via resolveAtomics; committed = server fold

		// --- Min (MinV2 at API 730): symmetric to Max ---
		{"min_b8high_o3_existingwins_trunc", fzMin, append([]byte{0, 0, 0, 0, 0, 0, 0}, 0x09), []byte{0x05, 0x00, 0x00}},
		{"min_b3_o2_operandwins", fzMin, []byte{0x05, 0x00, 0x00}, []byte{0x03, 0x00}},
		// base {3,0,0} (3B); operand {5,0,0,0,0,0,0,0} (8B, high 0): existing wins (3<5), result =
		// existing ZERO-PADDED to operand width = {3,0,0,0,0,0,0,0} (short base existing-wins).
		{"min_b3_o8_existingwins_zeropad", fzMin, []byte{0x03, 0x00, 0x00}, append([]byte{0x05}, bytesOf(7, 0x00)...)},
		// base {0xff} (1B); operand {0,1,0,0,0,0,0,0} (8B, high byte nonzero): existing wins via the
		// MSB scan (param[i]!=0 for i>=len(e)), result = existing zero-padded = {0xff,0,0,0,0,0,0,0}.
		{"min_b1_o8_existingwins_earlyscan", fzMin, []byte{0xff}, append([]byte{0x00, 0x01}, bytesOf(6, 0x00)...)},
		{"min_b8_o8_equal", fzMin, bytesOf(8, 0x07), bytesOf(8, 0x07)},
		{"min_empty_operand", fzMin, bytesOf(8, 0x07), []byte{}},
		{"min_empty_base", fzMin, []byte{}, bytesOf(8, 0x07)}, // present-empty → doMin (not absent→operand)
		{"min_missing_o8", fzMin, nil, bytesOf(8, 0x07)},      // in-txn folds doMinV2(nil,·)→operand via resolveAtomics; committed = server fold
	}
	runAtomicFoldCases(t, cases)
}

// TestDifferential_ByteMinMaxFold exercises ByteMin/ByteMax against a present base with operands
// that are prefixes/extensions/equal lexicographically (no width truncation — the result is the
// full winning value), plus missing-key.
func TestDifferential_ByteMinMaxFold(t *testing.T) {
	t.Parallel()
	cases := []atomicFoldCase{
		{"bytemax_base_gt", fzByteMax, []byte("abc"), []byte("abb")},       // base > operand → base
		{"bytemax_base_lt", fzByteMax, []byte("abc"), []byte("abd")},       // base < operand → operand
		{"bytemax_operand_prefix", fzByteMax, []byte("abc"), []byte("ab")}, // "abc" > "ab" → base (full value)
		{"bytemax_base_prefix", fzByteMax, []byte("ab"), []byte("abc")},    // "ab" < "abc" → operand
		{"bytemax_equal", fzByteMax, []byte("abc"), []byte("abc")},
		{"bytemax_missing", fzByteMax, nil, []byte("abc")},
		{"bytemin_base_lt", fzByteMin, []byte("abc"), []byte("abd")},       // base < operand → base
		{"bytemin_base_gt", fzByteMin, []byte("abd"), []byte("abc")},       // base > operand → operand
		{"bytemin_operand_prefix", fzByteMin, []byte("abc"), []byte("ab")}, // "ab" < "abc" → operand
		{"bytemin_base_prefix", fzByteMin, []byte("ab"), []byte("abc")},    // "ab" < "abc" → base
		{"bytemin_equal", fzByteMin, []byte("abc"), []byte("abc")},
		{"bytemin_missing", fzByteMin, nil, []byte("abc")},
	}
	runAtomicFoldCases(t, cases)
}

// TestDifferential_CompareAndClearFold exercises CompareAndClear (full-byte compare): present-
// equal (clears), present-unequal (kept), present-vs-empty-operand, empty-base-vs-empty-operand,
// and missing-key (the WriteBattery gap) with non-empty and empty operand.
func TestDifferential_CompareAndClearFold(t *testing.T) {
	t.Parallel()
	cases := []atomicFoldCase{
		{"cac_present_equal_clears", fzCompareAndClear, []byte("v"), []byte("v")},
		{"cac_present_unequal_kept", fzCompareAndClear, []byte("v"), []byte("x")},
		{"cac_present_vs_empty_operand", fzCompareAndClear, []byte("v"), []byte{}},
		{"cac_empty_base_vs_empty_operand", fzCompareAndClear, []byte{}, []byte{}},     // equal → clears
		{"cac_present_prefix_operand", fzCompareAndClear, []byte("abc"), []byte("ab")}, // unequal → kept
		{"cac_missing_nonempty", fzCompareAndClear, nil, []byte("v")},                  // absent → clear (no-op)
		{"cac_missing_empty", fzCompareAndClear, nil, []byte{}},                        // absent → clear (no-op)
	}
	runAtomicFoldCases(t, cases)
}

// TestDifferential_AppendIfFitsFold exercises AppendIfFits: present-base concat, empty base
// (→ operand), empty operand (→ base), and a near-limit concat that still fits.
func TestDifferential_AppendIfFitsFold(t *testing.T) {
	t.Parallel()
	cases := []atomicFoldCase{
		{"append_concat", fzAppendIfFits, []byte("ab"), []byte("cd")},
		{"append_empty_base", fzAppendIfFits, []byte{}, []byte("cd")},
		{"append_empty_operand", fzAppendIfFits, []byte("ab"), []byte{}},
		{"append_missing", fzAppendIfFits, nil, []byte("cd")},
		{"append_wide", fzAppendIfFits, bytesOf(50000, 0x61), bytesOf(40000, 0x62)}, // 90KB total, still < 100KB
	}
	runAtomicFoldCases(t, cases)
}
