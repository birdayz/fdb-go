package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

// oracleBinaryPath is set by the DIFF_ORACLE_BIN environment variable.
// Build the oracle first: ./build.sh <fdb-source-dir>
var oracleBinaryPath string

func TestMain(m *testing.M) {
	oracleBinaryPath = os.Getenv("DIFF_ORACLE_BIN")
	if oracleBinaryPath == "" {
		oracleBinaryPath = "./diff-oracle"
	}
	// Bazel runfiles: resolve relative path against TEST_SRCDIR.
	if !filepath.IsAbs(oracleBinaryPath) {
		if srcDir := os.Getenv("TEST_SRCDIR"); srcDir != "" {
			ws := os.Getenv("TEST_WORKSPACE")
			if ws == "" {
				ws = "_main"
			}
			oracleBinaryPath = filepath.Join(srcDir, ws, oracleBinaryPath)
		}
	}
	os.Exit(m.Run())
}

func startOracle(t testing.TB) *Oracle {
	t.Helper()
	o, err := NewOracle(oracleBinaryPath)
	if err != nil {
		t.Skipf("oracle not available: %v (set DIFF_ORACLE_BIN)", err)
	}
	t.Cleanup(func() { o.Close() })
	return o
}

// emptyVersionVector returns the C++ default VersionVector serialization:
// 8 bytes of zero (utlCount=0) + 8 bytes of 0xFF (maxVersion=-1).
func emptyVersionVector() []byte {
	vv := make([]byte, 16)
	binary.LittleEndian.PutUint64(vv[8:], ^uint64(0))
	return vv
}

// --- Deterministic fuzz seed tests (run without oracle too) ---
// These always run and verify the Go serialization is consistent.

func TestDiffGetReadVersionRequest(t *testing.T) {
	o := startOracle(t)
	testDiffGetReadVersionRequest(t, o, 1, 1, -1)
	testDiffGetReadVersionRequest(t, o, 0, 0, 0)
	testDiffGetReadVersionRequest(t, o, 2, 100, 9999999)
	testDiffGetReadVersionRequest(t, o, 0xFFFFFFFF, 0xFFFFFFFF, -1)
}

func TestDiffGetValueRequest(t *testing.T) {
	o := startOracle(t)
	testDiffGetValueRequest(t, o, []byte("hello"), 12345, -1)
	testDiffGetValueRequest(t, o, []byte{}, 0, 0)
	testDiffGetValueRequest(t, o, []byte("a_longer_key_with_many_bytes_in_it"), 999999999, -1)
}

func TestDiffGetKeyRequest(t *testing.T) {
	o := startOracle(t)
	testDiffGetKeyRequest(t, o, []byte("selector"), true, 1, 99999, -1)
	testDiffGetKeyRequest(t, o, []byte{}, false, 0, 0, 0)
	testDiffGetKeyRequest(t, o, []byte("key"), false, -5, 54321, 42)
}

func TestDiffGetKeyValuesRequest(t *testing.T) {
	o := startOracle(t)
	testDiffGetKeyValuesRequest(t, o,
		[]byte("begin"), true, 1,
		[]byte("end"), false, 0,
		54321, 1000, 0x7fffffff, -1)
	testDiffGetKeyValuesRequest(t, o,
		[]byte{}, false, 0,
		[]byte{}, false, 0,
		0, 0, 0, 0)
}

func TestDiffGetKeyServerLocationsRequest(t *testing.T) {
	o := startOracle(t)
	testDiffGetKeyServerLocationsRequest(t, o, []byte("test_key"), false, nil, 100, false, -1, -1)
	testDiffGetKeyServerLocationsRequest(t, o, []byte("begin"), true, []byte("end"), 42, true, -1, -1)
	testDiffGetKeyServerLocationsRequest(t, o, []byte{}, false, nil, 0, false, 0, 0)
}

func TestDiffCommitTransactionRequest(t *testing.T) {
	o := startOracle(t)

	// Empty commit
	testDiffCommitTransactionRequest(t, o, 0, -1, nil, nil, nil)

	// Single set mutation
	testDiffCommitTransactionRequest(t, o, 42, -1,
		[]Mutation{{Type: 0, Param1: []byte("key1"), Param2: []byte("val1")}},
		[]ConflictRange{{Begin: []byte("key1"), End: []byte("key1\x00")}},
		[]ConflictRange{{Begin: []byte("key1"), End: []byte("key1\x00")}},
	)

	// Multiple mutations
	var muts []Mutation
	var wcs []ConflictRange
	for i := 0; i < 3; i++ {
		key := fmt.Appendf(nil, "key_%d", i)
		val := fmt.Appendf(nil, "val_%d", i)
		muts = append(muts, Mutation{Type: 0, Param1: key, Param2: val})
		wcs = append(wcs, ConflictRange{Begin: key, End: append(append([]byte{}, key...), 0)})
	}
	testDiffCommitTransactionRequest(t, o, 99999, -1, muts, nil, wcs)
}

// --- Fuzz targets ---

func FuzzGetReadVersionRequest(f *testing.F) {
	f.Add(uint32(1), uint32(1), int64(-1))
	f.Add(uint32(0), uint32(0), int64(0))
	f.Add(uint32(0xFFFFFFFF), uint32(0xFFFFFFFF), int64(-1))
	f.Add(uint32(2), uint32(100), int64(999999))

	o := startOracle(f)

	f.Fuzz(func(t *testing.T, flags, transactionCount uint32, maxVersion int64) {
		testDiffGetReadVersionRequest(t, o, flags, transactionCount, maxVersion)
	})
}

func FuzzGetValueRequest(f *testing.F) {
	f.Add([]byte("key"), int64(12345), int64(-1))
	f.Add([]byte{}, int64(0), int64(0))
	f.Add([]byte("long_key_with_lots_of_bytes"), int64(-1), int64(42))

	o := startOracle(f)

	f.Fuzz(func(t *testing.T, key []byte, version, tenantId int64) {
		testDiffGetValueRequest(t, o, key, version, tenantId)
	})
}

func FuzzGetKeyRequest(f *testing.F) {
	f.Add([]byte("key"), true, int32(1), int64(99999), int64(-1))
	f.Add([]byte{}, false, int32(0), int64(0), int64(0))

	o := startOracle(f)

	f.Fuzz(func(t *testing.T, key []byte, orEqual bool, offset int32, version, tenantId int64) {
		testDiffGetKeyRequest(t, o, key, orEqual, offset, version, tenantId)
	})
}

func FuzzGetKeyValuesRequest(f *testing.F) {
	f.Add([]byte("begin"), []byte("end"), true, false, int32(1), int32(0),
		int64(54321), int32(1000), int32(0x7fffffff), int64(-1))
	f.Add([]byte{}, []byte{}, false, false, int32(0), int32(0),
		int64(0), int32(0), int32(0), int64(0))

	o := startOracle(f)

	f.Fuzz(func(t *testing.T, beginKey, endKey []byte, beginOrEqual, endOrEqual bool,
		beginOffset, endOffset int32, version int64, limit, limitBytes int32, tenantId int64,
	) {
		testDiffGetKeyValuesRequest(t, o, beginKey, beginOrEqual, beginOffset,
			endKey, endOrEqual, endOffset, version, limit, limitBytes, tenantId)
	})
}

func FuzzGetKeyServerLocationsRequest(f *testing.F) {
	f.Add([]byte("key"), false, []byte{}, int32(100), false, int64(-1), int64(-1))
	f.Add([]byte("begin"), true, []byte("end"), int32(42), true, int64(-1), int64(-1))

	o := startOracle(f)

	f.Fuzz(func(t *testing.T, begin []byte, hasEnd bool, end []byte,
		limit int32, reverse bool, tenantId, minTenantVersion int64,
	) {
		testDiffGetKeyServerLocationsRequest(t, o, begin, hasEnd, end,
			limit, reverse, tenantId, minTenantVersion)
	})
}

// --- Comparison helpers ---

func testDiffGetReadVersionRequest(t testing.TB, o *Oracle, flags, transactionCount uint32, maxVersion int64) {
	t.Helper()

	// Go serialization
	goMsg := &types.GetReadVersionRequest{
		Flags:            flags,
		TransactionCount: transactionCount,
		MaxVersion:       maxVersion,
		Reply:            types.ReplyPromise{}, // zero token
	}
	goBytes := goMsg.MarshalFDB()

	// C++ serialization
	cppBytes, err := o.SerializeGetReadVersionRequest(flags, transactionCount, maxVersion)
	if err != nil {
		t.Fatalf("oracle error: %v", err)
	}
	if cppBytes == nil {
		t.Fatal("oracle returned error response")
	}

	compareBytes(t, goBytes, cppBytes, "GetReadVersionRequest")
}

func testDiffGetValueRequest(t testing.TB, o *Oracle, key []byte, version, tenantId int64) {
	t.Helper()

	goMsg := &types.GetValueRequest{
		Key:                    key,
		Version:                version,
		Reply:                  types.ReplyPromise{},
		TenantInfo:             types.TenantInfo{TenantId: tenantId},
		SsLatestCommitVersions: emptyVersionVector(),
	}
	goBytes := goMsg.MarshalFDB()

	cppBytes, err := o.SerializeGetValueRequest(key, version, tenantId)
	if err != nil {
		t.Fatalf("oracle error: %v", err)
	}
	if cppBytes == nil {
		t.Fatal("oracle returned error response")
	}

	compareBytes(t, goBytes, cppBytes, "GetValueRequest")
}

func testDiffGetKeyRequest(t testing.TB, o *Oracle, key []byte, orEqual bool, offset int32, version, tenantId int64) {
	t.Helper()

	goMsg := &types.GetKeyRequest{
		Sel: types.KeySelectorRef{
			Key:     key,
			OrEqual: orEqual,
			Offset:  offset,
		},
		Version:                version,
		Reply:                  types.ReplyPromise{},
		TenantInfo:             types.TenantInfo{TenantId: tenantId},
		SsLatestCommitVersions: emptyVersionVector(),
	}
	goBytes := goMsg.MarshalFDB()

	cppBytes, err := o.SerializeGetKeyRequest(key, orEqual, offset, version, tenantId)
	if err != nil {
		t.Fatalf("oracle error: %v", err)
	}
	if cppBytes == nil {
		t.Fatal("oracle returned error response")
	}

	compareBytes(t, goBytes, cppBytes, "GetKeyRequest")
}

func testDiffGetKeyValuesRequest(t testing.TB, o *Oracle,
	beginKey []byte, beginOrEqual bool, beginOffset int32,
	endKey []byte, endOrEqual bool, endOffset int32,
	version int64, limit, limitBytes int32, tenantId int64,
) {
	t.Helper()

	goMsg := &types.GetKeyValuesRequest{
		Begin: types.KeySelectorRef{
			Key:     beginKey,
			OrEqual: beginOrEqual,
			Offset:  beginOffset,
		},
		End: types.KeySelectorRef{
			Key:     endKey,
			OrEqual: endOrEqual,
			Offset:  endOffset,
		},
		Version:                version,
		Limit:                  limit,
		LimitBytes:             limitBytes,
		Reply:                  types.ReplyPromise{},
		TenantInfo:             types.TenantInfo{TenantId: tenantId},
		SsLatestCommitVersions: emptyVersionVector(),
	}
	goBytes := goMsg.MarshalFDB()

	cppBytes, err := o.SerializeGetKeyValuesRequest(
		beginKey, beginOrEqual, beginOffset,
		endKey, endOrEqual, endOffset,
		version, limit, limitBytes, tenantId)
	if err != nil {
		t.Fatalf("oracle error: %v", err)
	}
	if cppBytes == nil {
		t.Fatal("oracle returned error response")
	}

	compareBytes(t, goBytes, cppBytes, "GetKeyValuesRequest")
}

func testDiffGetKeyServerLocationsRequest(t testing.TB, o *Oracle,
	begin []byte, hasEnd bool, end []byte,
	limit int32, reverse bool, tenantId, minTenantVersion int64,
) {
	t.Helper()

	goMsg := &types.GetKeyServerLocationsRequest{
		Begin:            begin,
		HasEnd:           hasEnd,
		Limit:            limit,
		Reverse:          reverse,
		Reply:            types.ReplyPromise{},
		Tenant:           types.TenantInfo{TenantId: tenantId},
		MinTenantVersion: minTenantVersion,
	}
	if hasEnd {
		goMsg.End = end
	}
	goBytes := goMsg.MarshalFDB()

	cppBytes, err := o.SerializeGetKeyServerLocationsRequest(
		begin, hasEnd, end, limit, reverse, tenantId, minTenantVersion)
	if err != nil {
		t.Fatalf("oracle error: %v", err)
	}
	if cppBytes == nil {
		t.Fatal("oracle returned error response")
	}

	compareBytes(t, goBytes, cppBytes, "GetKeyServerLocationsRequest")
}

func testDiffCommitTransactionRequest(t testing.TB, o *Oracle,
	readSnapshot, tenantId int64,
	mutations []Mutation,
	readConflictRanges, writeConflictRanges []ConflictRange,
) {
	t.Helper()

	// Build Go message
	goMsg := &types.CommitTransactionRequest{
		Transaction: types.CommitTransactionRef{
			ReadSnapshot: readSnapshot,
		},
		Reply:      types.ReplyPromise{},
		TenantInfo: types.TenantInfo{TenantId: tenantId},
	}

	for _, m := range mutations {
		goMsg.Transaction.Mutations = append(goMsg.Transaction.Mutations,
			types.MutationRef{
				MutType: m.Type,
				Param1:  m.Param1,
				Param2:  m.Param2,
			})
	}
	for _, cr := range readConflictRanges {
		goMsg.Transaction.ReadConflictRanges = append(goMsg.Transaction.ReadConflictRanges,
			types.KeyRangeRef{Begin: cr.Begin, End: cr.End})
	}
	for _, cr := range writeConflictRanges {
		goMsg.Transaction.WriteConflictRanges = append(goMsg.Transaction.WriteConflictRanges,
			types.KeyRangeRef{Begin: cr.Begin, End: cr.End})
	}

	goBytes := goMsg.MarshalFDB()

	cppBytes, err := o.SerializeCommitTransactionRequest(
		readSnapshot, tenantId, mutations, readConflictRanges, writeConflictRanges)
	if err != nil {
		t.Fatalf("oracle error: %v", err)
	}
	if cppBytes == nil {
		t.Fatal("oracle returned error response")
	}

	compareBytes(t, goBytes, cppBytes, "CommitTransactionRequest")
}

// compareBytes compares Go and C++ serialized bytes. The only expected
// difference is the 16-byte reply token region (which C++ zeros
// post-serialization). If there are other differences, that's a bug.
func compareBytes(t testing.TB, goBytes, cppBytes []byte, typeName string) {
	t.Helper()

	if len(goBytes) != len(cppBytes) {
		t.Errorf("%s: SIZE MISMATCH Go=%d C++=%d", typeName, len(goBytes), len(cppBytes))
		dumpDiff(t, goBytes, cppBytes, typeName)
		return
	}

	if !bytes.Equal(goBytes, cppBytes) {
		// Count and report divergences
		divergences := 0
		firstDiv := -1
		for i := range goBytes {
			if goBytes[i] != cppBytes[i] {
				if firstDiv == -1 {
					firstDiv = i
				}
				divergences++
			}
		}
		t.Errorf("%s: %d byte divergences starting at offset %d", typeName, divergences, firstDiv)
		dumpDiff(t, goBytes, cppBytes, typeName)
	}
}

func dumpDiff(t testing.TB, goBytes, cppBytes []byte, typeName string) {
	t.Helper()

	t.Logf("Go  (%d bytes): %s", len(goBytes), hex.EncodeToString(goBytes))
	t.Logf("C++ (%d bytes): %s", len(cppBytes), hex.EncodeToString(cppBytes))

	// Show per-byte diff for first 32 divergent bytes
	minLen := len(goBytes)
	if len(cppBytes) < minLen {
		minLen = len(cppBytes)
	}
	shown := 0
	for i := 0; i < minLen && shown < 32; i++ {
		if goBytes[i] != cppBytes[i] {
			t.Logf("  byte %3d: Go=0x%02x C++=0x%02x", i, goBytes[i], cppBytes[i])
			shown++
		}
	}
}
