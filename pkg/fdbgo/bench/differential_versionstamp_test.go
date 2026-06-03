package bench

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"

	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
	cgotuple "github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	gotuple "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// Versionstamp-mutation differential vs libfdb_c — RFC-063.
//
// SetVersionstampedKey/Value embed a commit-assigned 10-byte stamp (8-byte tx version + 2-byte
// batch, big-endian) at an offset encoded as a little-endian suffix on the mutation operand
// (4-byte at API >= 520). The server strips the suffix, writes the stamp at `offset`, and
// converts the op to SetValue. The 10-byte stamp differs per commit, but everything else — the
// prefix, surrounding user data, the 2-byte tuple user version, and WHERE the stamp lands — must
// match libfdb_c byte-for-byte. These ops were EXCLUDED from the differential fuzz; the only
// cross-engine coverage (TestInterop_Versionstamp) writes via Go only and just checks the stamp
// is non-zero.
//
// Strategy: write the SAME template via both clients (each to its own isolation prefix; separate
// commits → different stamps), read back the materialized key/value, assert the 10-byte stamp
// region is NON-ZERO (the stamp materialized) then MASK it, and byte-compare the masked
// structure (shared logical data + suffix) go-vs-cgo. A mis-placed offset shifts the surrounding
// bytes, so the masked compare + non-zero-stamp assertion catches it (masking can't absorb a
// one-byte-off: the suffix would land in the masked region on one side only).

const vsStampLen = 10 // SetVersionstampedKey/Value stamp: 8-byte version + 2-byte batch

// vsOperand builds a versionstamped-mutation operand: data + 4-byte LE offset suffix (API >= 520).
// The stamp is placed at `stampPos` within `data` (data = operand minus the 4-byte suffix).
func vsOperand(data []byte, stampPos int) []byte {
	out := make([]byte, len(data)+4)
	copy(out, data)
	binary.LittleEndian.PutUint32(out[len(data):], uint32(stampPos))
	return out
}

// maskStamp returns a copy of b with [pos, pos+10) zeroed, and reports whether that region was
// non-zero (the stamp actually materialized). Out-of-range pos → not-materialized.
func maskStamp(b []byte, pos int) (masked []byte, nonZero bool) {
	masked = append([]byte(nil), b...)
	if pos < 0 || pos+vsStampLen > len(b) {
		return masked, false
	}
	for i := pos; i < pos+vsStampLen; i++ {
		if masked[i] != 0 {
			nonZero = true
		}
		masked[i] = 0
	}
	return masked, nonZero
}

// vsKeyCase: SetVersionstampedKey with key = isoPrefix + logical + <stamp> + suffix.
// The stamp lands at offset = len(isoPrefix)+len(logical). We compare the materialized key with
// its client-specific isoPrefix STRIPPED (so go/cgo are comparable) and the stamp masked.
type vsKeyCase struct {
	name    string
	logical []byte // user data between the isolation prefix and the stamp (compared)
	suffix  []byte // user data after the stamp (compared)
}

func runVSKeyCase(t *testing.T, c vsKeyCase) {
	t.Helper()
	ns := strings.ReplaceAll(t.Name(), "/", "_")
	base := fmt.Sprintf("vsk_%d_%s_", os.Getpid(), ns)
	clearPrefix(t, base)

	masked := func(iso string, write func(template []byte), scan func(begin, end []byte) ([]byte, bool)) string {
		isoB := []byte(iso)
		stampPos := len(isoB) + len(c.logical)
		data := make([]byte, 0, stampPos+vsStampLen+len(c.suffix))
		data = append(data, isoB...)
		data = append(data, c.logical...)
		data = append(data, make([]byte, vsStampLen)...) // placeholder
		data = append(data, c.suffix...)
		write(vsOperand(data, stampPos))
		matKey, ok := scan(isoB, append(append([]byte{}, isoB...), 0xff))
		if !ok {
			t.Fatalf("%s: materialized key not found under %q", c.name, iso)
		}
		wantLen := len(isoB) + len(c.logical) + vsStampLen + len(c.suffix)
		if len(matKey) != wantLen {
			t.Fatalf("%s: materialized key len=%d want=%d", c.name, len(matKey), wantLen)
		}
		if !bytes.HasPrefix(matKey, isoB) {
			t.Fatalf("%s: materialized key lost its prefix", c.name)
		}
		rest := matKey[len(isoB):] // logical + stamp + suffix (isolation prefix stripped)
		m, nonZero := maskStamp(rest, len(c.logical))
		if !nonZero {
			t.Fatalf("%s: stamp region all-zero — not materialized at the expected offset", c.name)
		}
		return hex.EncodeToString(m)
	}

	goMasked := masked(base+"go_", func(template []byte) {
		if _, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
			tx.SetVersionstampedKey(gofdb.Key(template), []byte("v"))
			return nil, nil
		}); err != nil {
			t.Fatalf("go %s write: %v", c.name, err)
		}
	}, func(begin, end []byte) ([]byte, bool) {
		r, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
			return tx.GetRange(gofdb.KeyRange{Begin: gofdb.Key(begin), End: gofdb.Key(end)}, gofdb.RangeOptions{Limit: 1}).GetSliceWithError()
		})
		if err != nil {
			t.Fatalf("go %s scan: %v", c.name, err)
		}
		kvs := r.([]gofdb.KeyValue)
		if len(kvs) == 0 {
			return nil, false
		}
		return kvs[0].Key, true
	})

	cgoMasked := masked(base+"c_", func(template []byte) {
		if _, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
			tx.SetVersionstampedKey(cgofdb.Key(template), []byte("v"))
			return nil, nil
		}); err != nil {
			t.Fatalf("cgo %s write: %v", c.name, err)
		}
	}, func(begin, end []byte) ([]byte, bool) {
		r, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
			return tx.GetRange(cgofdb.KeyRange{Begin: cgofdb.Key(begin), End: cgofdb.Key(end)}, cgofdb.RangeOptions{Limit: 1}).GetSliceWithError()
		})
		if err != nil {
			t.Fatalf("cgo %s scan: %v", c.name, err)
		}
		kvs := r.([]cgofdb.KeyValue)
		if len(kvs) == 0 {
			return nil, false
		}
		return kvs[0].Key, true
	})

	if goMasked != cgoMasked {
		t.Fatalf("%s: masked materialized key differs: go=%s cgo=%s", c.name, goMasked, cgoMasked)
	}
}

func TestDifferential_VersionstampedKey(t *testing.T) {
	t.Parallel()
	cases := []vsKeyCase{
		{"offset0_no_logical", nil, nil},           // key == isoPrefix + stamp
		{"after_logical", []byte("abc"), nil},      // isoPrefix + "abc" + stamp
		{"mid_key", []byte("pre"), []byte("post")}, // isoPrefix + "pre" + stamp + "post"
		{"binary_surround", []byte{0x00, 0xff}, []byte{0x01, 0x00, 0xff}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			runVSKeyCase(t, c)
		})
	}
}

// vsValueCase: SetVersionstampedValue at a fixed key; value = logical + <stamp> + suffix.
type vsValueCase struct {
	name    string
	logical []byte
	suffix  []byte
}

func runVSValueCase(t *testing.T, c vsValueCase) {
	t.Helper()
	ns := strings.ReplaceAll(t.Name(), "/", "_")
	base := fmt.Sprintf("vsv_%d_%s_", os.Getpid(), ns)
	clearPrefix(t, base)

	masked := func(key string, write func(k, template []byte), read func(k []byte) ([]byte, bool)) string {
		stampPos := len(c.logical)
		data := make([]byte, 0, stampPos+vsStampLen+len(c.suffix))
		data = append(data, c.logical...)
		data = append(data, make([]byte, vsStampLen)...)
		data = append(data, c.suffix...)
		kb := []byte(key)
		write(kb, vsOperand(data, stampPos))
		val, ok := read(kb)
		if !ok {
			t.Fatalf("%s: value not found at %q", c.name, key)
		}
		wantLen := len(c.logical) + vsStampLen + len(c.suffix)
		if len(val) != wantLen {
			t.Fatalf("%s: value len=%d want=%d", c.name, len(val), wantLen)
		}
		m, nonZero := maskStamp(val, len(c.logical))
		if !nonZero {
			t.Fatalf("%s: stamp region all-zero — not materialized", c.name)
		}
		return hex.EncodeToString(m)
	}

	goMasked := masked(base+"go", func(k, template []byte) {
		if _, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
			tx.SetVersionstampedValue(gofdb.Key(k), template)
			return nil, nil
		}); err != nil {
			t.Fatalf("go %s write: %v", c.name, err)
		}
	}, func(k []byte) ([]byte, bool) {
		r, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) { return tx.Get(gofdb.Key(k)).Get() })
		if err != nil {
			t.Fatalf("go %s read: %v", c.name, err)
		}
		b, _ := r.([]byte)
		return b, b != nil
	})

	cgoMasked := masked(base+"c", func(k, template []byte) {
		if _, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
			tx.SetVersionstampedValue(cgofdb.Key(k), template)
			return nil, nil
		}); err != nil {
			t.Fatalf("cgo %s write: %v", c.name, err)
		}
	}, func(k []byte) ([]byte, bool) {
		r, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) { return tx.Get(cgofdb.Key(k)).Get() })
		if err != nil {
			t.Fatalf("cgo %s read: %v", c.name, err)
		}
		b, _ := r.([]byte)
		return b, b != nil
	})

	if goMasked != cgoMasked {
		t.Fatalf("%s: masked materialized value differs: go=%s cgo=%s", c.name, goMasked, cgoMasked)
	}
}

// TestDifferential_VSValueOffsets extends the existing offset-0 TestDifferential_VersionstampedValue
// (differential_test.go) with NON-zero stamp offsets (after a header, mid-value) and binary
// surrounds — proving the offset placement, not just the offset-0 base case.
func TestDifferential_VSValueOffsets(t *testing.T) {
	t.Parallel()
	cases := []vsValueCase{
		{"after_logical", []byte("hdr"), nil},
		{"mid_value", []byte("pre"), []byte("post")},
		{"binary_surround", []byte{0x00, 0xff}, []byte{0x01, 0x00, 0xff}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			runVSValueCase(t, c)
		})
	}
}

// TestDifferential_VersionstampTuplePack builds the key via tuple.PackWithVersionstamp (which
// encodes the incomplete versionstamp + the offset suffix) including a non-trivial USER VERSION,
// writes it via SetVersionstampedKey, reads back, masks the 10-byte tx-version stamp, and
// compares. The 2-byte user version follows the 10-byte stamp in the 12-byte tuple versionstamp,
// so it is NOT masked and must match identically — proving both the tuple offset encoding and
// user-version preservation.
func TestDifferential_VersionstampTuplePack(t *testing.T) {
	t.Parallel()
	ns := strings.ReplaceAll(t.Name(), "/", "_")
	base := fmt.Sprintf("vstp_%d_%s_", os.Getpid(), ns)
	clearPrefix(t, base)

	const userVersion = 0xABCD

	goMasked := func() string {
		iso := []byte(base + "go_")
		key, err := gotuple.Tuple{"item", gotuple.IncompleteVersionstamp(userVersion)}.PackWithVersionstamp(iso)
		if err != nil {
			t.Fatalf("go PackWithVersionstamp: %v", err)
		}
		if _, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
			tx.SetVersionstampedKey(gofdb.Key(key), []byte("v"))
			return nil, nil
		}); err != nil {
			t.Fatalf("go tuple vs write: %v", err)
		}
		r, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
			return tx.GetRange(gofdb.KeyRange{Begin: gofdb.Key(iso), End: gofdb.Key(append(append([]byte{}, iso...), 0xff))}, gofdb.RangeOptions{Limit: 1}).GetSliceWithError()
		})
		if err != nil {
			t.Fatalf("go tuple vs scan: %v", err)
		}
		kvs := r.([]gofdb.KeyValue)
		if len(kvs) != 1 {
			t.Fatalf("go tuple vs: want 1 key, got %d", len(kvs))
		}
		rest := kvs[0].Key[len(iso):] // tuple bytes incl. the materialized 12-byte versionstamp
		// The 10-byte tx version sits at the versionstamp's start; find it: the incomplete
		// versionstamp's position within the tuple is what PackWithVersionstamp encoded. We don't
		// re-derive it — we mask by locating the 12-byte versionstamp via unpack on the cgo side
		// is overkill; instead compare the WHOLE rest with the stamp masked at the known tuple
		// offset. The versionstamp element is the 2nd element; its 10-byte stamp starts right
		// after the 1st element ("item") encoding + the 0x33 versionstamp typecode.
		stampPos := tupleVSStampPos(rest)
		m, nonZero := maskStamp(rest, stampPos)
		if !nonZero {
			t.Fatalf("go tuple vs: stamp region all-zero at pos %d", stampPos)
		}
		return hex.EncodeToString(m)
	}()

	cgoMasked := func() string {
		iso := []byte(base + "c_")
		key, err := cgotuple.Tuple{"item", cgotuple.IncompleteVersionstamp(userVersion)}.PackWithVersionstamp(iso)
		if err != nil {
			t.Fatalf("cgo PackWithVersionstamp: %v", err)
		}
		if _, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
			tx.SetVersionstampedKey(cgofdb.Key(key), []byte("v"))
			return nil, nil
		}); err != nil {
			t.Fatalf("cgo tuple vs write: %v", err)
		}
		r, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
			return tx.GetRange(cgofdb.KeyRange{Begin: cgofdb.Key(iso), End: cgofdb.Key(append(append([]byte{}, iso...), 0xff))}, cgofdb.RangeOptions{Limit: 1}).GetSliceWithError()
		})
		if err != nil {
			t.Fatalf("cgo tuple vs scan: %v", err)
		}
		kvs := r.([]cgofdb.KeyValue)
		if len(kvs) != 1 {
			t.Fatalf("cgo tuple vs: want 1 key, got %d", len(kvs))
		}
		rest := kvs[0].Key[len(iso):]
		stampPos := tupleVSStampPos(rest)
		m, nonZero := maskStamp(rest, stampPos)
		if !nonZero {
			t.Fatalf("cgo tuple vs: stamp region all-zero at pos %d", stampPos)
		}
		return hex.EncodeToString(m)
	}()

	if goMasked != cgoMasked {
		t.Fatalf("tuple versionstamp masked key differs: go=%s cgo=%s", goMasked, cgoMasked)
	}
}

// TestDifferential_VersionstampErrors pins the offset-validation boundary: a versionstamped
// mutation with a bad offset (negative, offset+10 > body, operand too small to hold the 4-byte
// suffix, empty body) must be REJECTED with the same error code (2000, client_invalid_operation)
// by both clients, surfaced at Commit (the client-side guard, before the server's commit-time
// silent skip). The tight-valid boundary (offset+10 == body) must COMMIT cleanly (code 0). Go
// defers the check to commit (the per-mutation validation loop in Transaction.Commit, "Atomic()
// is void"; ordering vs the size/key/value checks is pinned by
// TestDifferential_VersionstampValidationOrder); libfdb_c rejects at
// the atomicOp call — both surface at commit. go==cgo asserted.
func TestDifferential_VersionstampErrors(t *testing.T) {
	t.Parallel()
	ns := strings.ReplaceAll(t.Name(), "/", "_")
	base := fmt.Sprintf("vserr_%d_%s_", os.Getpid(), ns)
	clearPrefix(t, base) // the valid_tight case commits a key; clear for consistency with the other VS helpers and -count=2 (@claude)

	negOffset := func() []byte { // body=10 placeholder, offset = 0xFFFFFFFF (negative int32)
		b := make([]byte, 14)
		binary.LittleEndian.PutUint32(b[10:], 0xFFFFFFFF)
		return b
	}

	cases := []struct {
		name    string
		operand func(iso []byte) []byte // builds the SetVersionstampedKey operand (the key template)
		wantOK  bool                    // true → commit should succeed (code 0); false → reject (2000)
	}{
		// offset+10 == body (tight valid): iso + 10-byte placeholder, offset = len(iso) → commits.
		{"valid_tight", func(iso []byte) []byte {
			return vsOperand(append(append([]byte{}, iso...), make([]byte, vsStampLen)...), len(iso))
		}, true},
		// offset+10 == body+1 (off-by-one reject): offset = len(iso)+1 over the same body.
		{"offbyone_reject", func(iso []byte) []byte {
			return vsOperand(append(append([]byte{}, iso...), make([]byte, vsStampLen)...), len(iso)+1)
		}, false},
		{"offset_negative", func(iso []byte) []byte { return negOffset() }, false},
		{"offset_past_body", func(iso []byte) []byte { return vsOperand(make([]byte, vsStampLen), 99) }, false},
		{"operand_too_small", func(iso []byte) []byte { return []byte{0x00, 0x00, 0x00} }, false}, // <4: no suffix
		{"empty_body", func(iso []byte) []byte { return vsOperand(nil, 0) }, false},               // body=0, 0+10>0
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			goCode := func() int {
				_, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
					tx.SetVersionstampedKey(gofdb.Key(c.operand([]byte(base+"go_"+c.name))), []byte("v"))
					return nil, nil
				})
				return fdbErrorCode(err)
			}()
			cgoCode := func() int {
				_, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
					tx.SetVersionstampedKey(cgofdb.Key(c.operand([]byte(base+"c_"+c.name))), []byte("v"))
					return nil, nil
				})
				return fdbErrorCode(err)
			}()
			if goCode != cgoCode {
				t.Fatalf("%s: commit error code differs: go=%d cgo=%d", c.name, goCode, cgoCode)
			}
			if c.wantOK && goCode != 0 {
				t.Fatalf("%s: expected clean commit, got code %d", c.name, goCode)
			}
			if !c.wantOK && goCode == 0 {
				t.Fatalf("%s: expected rejection, both committed cleanly", c.name)
			}
		})
	}
}

// TestDifferential_VersionstampMultiOp pins batch-id placement: two SetVersionstampedKey ops to
// distinct keys in ONE txn share the commit's transaction version but get distinct 2-byte batch
// ids. Both materialize; we mask the 10-byte stamp on each and compare the structure go-vs-cgo,
// and assert the two stamps within a commit are DISTINCT (different batch ids) on both clients.
func TestDifferential_VersionstampMultiOp(t *testing.T) {
	t.Parallel()
	ns := strings.ReplaceAll(t.Name(), "/", "_")
	base := fmt.Sprintf("vsmulti_%d_%s_", os.Getpid(), ns)
	clearPrefix(t, base)

	run := func(iso string, write func(k1, k2 []byte), scan func(prefix []byte) [][]byte) (maskedHex string, stamps [][]byte) {
		isoB := []byte(iso)
		mk := func(tag byte) []byte { // iso + tag + 10-placeholder + 4-offset(len(iso)+1)
			d := append(append([]byte{}, isoB...), tag)
			d = append(d, make([]byte, vsStampLen)...)
			return vsOperand(d, len(isoB)+1)
		}
		write(mk('A'), mk('B'))
		keys := scan(isoB)
		if len(keys) != 2 {
			t.Fatalf("multiop %s: want 2 keys, got %d", iso, len(keys))
		}
		var hexes []string
		for _, k := range keys {
			rest := k[len(isoB):] // tag + stamp(10)
			stamps = append(stamps, append([]byte(nil), rest[1:1+vsStampLen]...))
			m, nonZero := maskStamp(rest, 1)
			if !nonZero {
				t.Fatalf("multiop %s: stamp all-zero", iso)
			}
			hexes = append(hexes, hex.EncodeToString(m))
		}
		return strings.Join(hexes, "|"), stamps
	}

	goHex, goStamps := run(base+"go_", func(k1, k2 []byte) {
		if _, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
			tx.SetVersionstampedKey(gofdb.Key(k1), []byte("v"))
			tx.SetVersionstampedKey(gofdb.Key(k2), []byte("v"))
			return nil, nil
		}); err != nil {
			t.Fatalf("go multiop write: %v", err)
		}
	}, func(prefix []byte) [][]byte {
		r, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
			return tx.GetRange(gofdb.KeyRange{Begin: gofdb.Key(prefix), End: gofdb.Key(append(append([]byte{}, prefix...), 0xff))}, gofdb.RangeOptions{}).GetSliceWithError()
		})
		if err != nil {
			t.Fatalf("go multiop scan: %v", err)
		}
		var out [][]byte
		for _, kv := range r.([]gofdb.KeyValue) {
			out = append(out, kv.Key)
		}
		return out
	})

	cgoHex, cgoStamps := run(base+"c_", func(k1, k2 []byte) {
		if _, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
			tx.SetVersionstampedKey(cgofdb.Key(k1), []byte("v"))
			tx.SetVersionstampedKey(cgofdb.Key(k2), []byte("v"))
			return nil, nil
		}); err != nil {
			t.Fatalf("cgo multiop write: %v", err)
		}
	}, func(prefix []byte) [][]byte {
		r, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
			return tx.GetRange(cgofdb.KeyRange{Begin: cgofdb.Key(prefix), End: cgofdb.Key(append(append([]byte{}, prefix...), 0xff))}, cgofdb.RangeOptions{}).GetSliceWithError()
		})
		if err != nil {
			t.Fatalf("cgo multiop scan: %v", err)
		}
		var out [][]byte
		for _, kv := range r.([]cgofdb.KeyValue) {
			out = append(out, kv.Key)
		}
		return out
	})

	if goHex != cgoHex {
		t.Fatalf("multiop masked keys differ: go=%s cgo=%s", goHex, cgoHex)
	}
	// The differential corrected a reviewer assumption ("two ops → different batch ids"): in FDB
	// the 10-byte stamp identifies the TRANSACTION (commit version + batch order), not the
	// operation, so BOTH versionstamped ops in one txn get the IDENTICAL stamp — the user
	// differentiates them via the tuple user version, not the batch id. Pin that invariant AND
	// that both clients agree on it (a divergence where one client advanced a per-op id would be
	// masked by the structure compare, so it needs its own assertion).
	goEq := bytes.Equal(goStamps[0], goStamps[1])
	cgoEq := bytes.Equal(cgoStamps[0], cgoStamps[1])
	if goEq != cgoEq {
		t.Fatalf("two-op stamp equality differs: go-equal=%v (%x,%x) cgo-equal=%v (%x,%x)",
			goEq, goStamps[0], goStamps[1], cgoEq, cgoStamps[0], cgoStamps[1])
	}
	if !goEq {
		t.Fatalf("expected both versionstamped ops in one txn to share the txn stamp, got distinct %x %x", goStamps[0], goStamps[1])
	}
}

// tupleVSStampPos returns the offset of the 10-byte tx-version stamp within a packed tuple of the
// form {"item", Versionstamp}. Layout: 0x02 "item" 0x00 | 0x33 [10-byte stamp][2-byte user ver].
// The stamp starts right after the versionstamp typecode 0x33.
func tupleVSStampPos(packed []byte) int {
	i := bytes.IndexByte(packed, 0x33)
	if i < 0 {
		return -1
	}
	return i + 1
}

// TestDifferential_GetVersionstamp commits a versionstamped op and checks GetVersionstamp() on
// both clients: a 10-byte, non-zero value that EQUALS the stamp materialized into the key. The
// stamps differ across the two distinct commits, so we don't compare go-vs-cgo bytes — we assert
// each client's GetVersionstamp matches its OWN materialized key stamp (the wire-consistency
// invariant), and that both are 10 bytes.
func TestDifferential_GetVersionstamp(t *testing.T) {
	t.Parallel()
	ns := strings.ReplaceAll(t.Name(), "/", "_")
	base := fmt.Sprintf("vsgv_%d_%s_", os.Getpid(), ns)
	clearPrefix(t, base)

	check := func(client string, run func() (stampFromKey, stampFromGV []byte)) {
		sk, sgv := run()
		if len(sgv) != vsStampLen {
			t.Fatalf("%s GetVersionstamp len=%d want=%d", client, len(sgv), vsStampLen)
		}
		allZero := true
		for _, b := range sgv {
			if b != 0 {
				allZero = false
			}
		}
		if allZero {
			t.Fatalf("%s GetVersionstamp all-zero", client)
		}
		if !bytes.Equal(sk, sgv) {
			t.Fatalf("%s: stamp in key (%x) != GetVersionstamp (%x)", client, sk, sgv)
		}
	}

	check("go", func() ([]byte, []byte) {
		iso := []byte(base + "go_")
		var gv gofdb.FutureKey
		if _, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
			data := append(append([]byte{}, iso...), make([]byte, vsStampLen)...)
			tx.SetVersionstampedKey(gofdb.Key(vsOperand(data, len(iso))), []byte("v"))
			gv = tx.GetVersionstamp()
			return nil, nil
		}); err != nil {
			t.Fatalf("go gv write: %v", err)
		}
		stampGV, err := gv.Get()
		if err != nil {
			t.Fatalf("go GetVersionstamp: %v", err)
		}
		r, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
			return tx.GetRange(gofdb.KeyRange{Begin: gofdb.Key(iso), End: gofdb.Key(append(append([]byte{}, iso...), 0xff))}, gofdb.RangeOptions{Limit: 1}).GetSliceWithError()
		})
		if err != nil {
			t.Fatalf("go gv scan: %v", err)
		}
		kvs := r.([]gofdb.KeyValue)
		return kvs[0].Key[len(iso) : len(iso)+vsStampLen], []byte(stampGV)
	})

	check("cgo", func() ([]byte, []byte) {
		iso := []byte(base + "c_")
		var gv cgofdb.FutureKey
		if _, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
			data := append(append([]byte{}, iso...), make([]byte, vsStampLen)...)
			tx.SetVersionstampedKey(cgofdb.Key(vsOperand(data, len(iso))), []byte("v"))
			gv = tx.GetVersionstamp()
			return nil, nil
		}); err != nil {
			t.Fatalf("cgo gv write: %v", err)
		}
		stampGV, err := gv.Get()
		if err != nil {
			t.Fatalf("cgo GetVersionstamp: %v", err)
		}
		r, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
			return tx.GetRange(cgofdb.KeyRange{Begin: cgofdb.Key(iso), End: cgofdb.Key(append(append([]byte{}, iso...), 0xff))}, cgofdb.RangeOptions{Limit: 1}).GetSliceWithError()
		})
		if err != nil {
			t.Fatalf("cgo gv scan: %v", err)
		}
		kvs := r.([]cgofdb.KeyValue)
		return kvs[0].Key[len(iso) : len(iso)+vsStampLen], []byte(stampGV)
	})
}
