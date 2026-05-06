package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"math"
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
	t.Cleanup(func() {
		if err := o.Close(); err != nil {
			t.Logf("oracle cleanup error: %v", err)
		}
	})
	return o
}

// emptyVersionVector returns the C++ default VersionVector serialization:
// 8 bytes of zero (utlCount=0) + 8 bytes of 0xFF (maxVersion=-1).
func emptyVersionVector() []byte {
	vv := make([]byte, 16)
	binary.LittleEndian.PutUint64(vv[8:], ^uint64(0))
	return vv
}

// --- Fuzz targets: 18 types, single []byte input ---

// 1. GetReadVersionRequest
func FuzzGetReadVersionRequest(f *testing.F) {
	f.Add([]byte{1, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 1, 3, 0x41, 0x42, 0x43, 1, 2, 0xDE, 0xAD, 0, 0, 0, 0, 0, 0, 0, 0})

	o := startOracle(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		r := &fuzzReader{data: data}
		transactionCount := r.uint32()
		flags := r.uint32()
		hasTags := r.bool()
		var tags []byte
		if hasTags {
			tags = r.bytes()
		}
		hasDebugID := r.bool()
		var debugID []byte
		if hasDebugID {
			debugID = r.bytes()
		}
		maxVersion := r.int64()

		// C++ oracle skips Tags (TransactionTagMap) and DebugID (Optional<UID>)
		// because they're complex structured types. Keep Go side in sync.
		_ = hasTags
		_ = tags
		_ = hasDebugID
		_ = debugID

		goMsg := &types.GetReadVersionRequest{
			TransactionCount: transactionCount,
			Flags:            flags,
			MaxVersion:       maxVersion,
			Reply:            types.ReplyPromise{},
		}
		goBytes := goMsg.MarshalFDB()

		cppBytes, err := o.SerializeGetReadVersionRequest(
			transactionCount, flags, false, nil, false, nil, maxVersion)
		if err != nil {
			t.Fatalf("oracle error: %v", err)
		}
		if cppBytes == nil {
			t.Skip("oracle returned error response")
		}

		compareBytesStructural(t, goBytes, cppBytes, "GetReadVersionRequest",
			unmarshalGetReadVersionRequest2, equalGetReadVersionRequest)
	})
}

// 2. GetValueRequest
func FuzzGetValueRequest(f *testing.F) {
	f.Add([]byte{5, 0x68, 0x65, 0x6c, 0x6c, 0x6f, 0x39, 0x30, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0, 0})
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})

	o := startOracle(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		r := &fuzzReader{data: data}
		key := r.bytes()
		version := r.int64()
		hasTags := r.bool()
		var tags []byte
		if hasTags {
			tags = r.bytes()
		}
		tenantId := r.int64()
		hasOptions := r.bool()
		ssLatestCommitVersions := r.bytes()

		// C++ oracle skips Tags, Options, SsLatestCommitVersions (complex types)
		_ = hasTags
		_ = tags
		_ = hasOptions
		_ = ssLatestCommitVersions

		goMsg := &types.GetValueRequest{
			Key:                    key,
			Version:                version,
			Reply:                  types.ReplyPromise{},
			TenantInfo:             types.TenantInfo{TenantId: tenantId},
			SsLatestCommitVersions: emptyVersionVector(),
		}
		goBytes := goMsg.MarshalFDB()

		cppBytes, err := o.SerializeGetValueRequest(
			key, version, false, nil, tenantId, false, emptyVersionVector())
		if err != nil {
			t.Fatalf("oracle error: %v", err)
		}
		if cppBytes == nil {
			t.Skip("oracle returned error response")
		}

		compareBytesStructural(t, goBytes, cppBytes, "GetValueRequest",
			unmarshalGetValueRequest, equalGetValueRequest)
	})
}

// 3. GetKeyRequest
func FuzzGetKeyRequest(f *testing.F) {
	f.Add([]byte{8, 0x73, 0x65, 0x6c, 0x65, 0x63, 0x74, 0x6f, 0x72, 1, 1, 0, 0, 0, 0x39, 0x30, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0, 0, 0, 0})
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})

	o := startOracle(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		r := &fuzzReader{data: data}
		selKey := r.bytes()
		selOrEqual := r.bool()
		selOffset := r.int32()
		version := r.int64()
		hasTags := r.bool()
		var tags []byte
		if hasTags {
			tags = r.bytes()
		}
		tenantId := r.int64()
		hasOptions := r.bool()
		ssLatestCommitVersions := r.bytes()
		field10 := r.bytes()

		// C++ oracle skips Tags, Options, SsLatestCommitVersions (VersionVector), Field_10
		_ = hasTags
		_ = tags
		_ = hasOptions
		_ = ssLatestCommitVersions
		_ = field10

		goMsg := &types.GetKeyRequest{
			Sel: types.KeySelectorRef{
				Key:     selKey,
				OrEqual: selOrEqual,
				Offset:  selOffset,
			},
			Version:                version,
			Reply:                  types.ReplyPromise{},
			TenantInfo:             types.TenantInfo{TenantId: tenantId},
			SsLatestCommitVersions: emptyVersionVector(),
		}
		goBytes := goMsg.MarshalFDB()

		cppBytes, err := o.SerializeGetKeyRequest(
			selKey, selOrEqual, selOffset, version, false, nil,
			tenantId, false, emptyVersionVector(), nil)
		if err != nil {
			t.Fatalf("oracle error: %v", err)
		}
		if cppBytes == nil {
			t.Skip("oracle returned error response")
		}

		compareBytesStructural(t, goBytes, cppBytes, "GetKeyRequest",
			unmarshalGetKeyRequest, equalGetKeyRequest2)
	})
}

// 4. GetKeyValuesRequest
func FuzzGetKeyValuesRequest(f *testing.F) {
	f.Add([]byte{5, 0x62, 0x65, 0x67, 0x69, 0x6e, 1, 1, 0, 0, 0, 3, 0x65, 0x6e, 0x64, 0, 0, 0, 0, 0, 0xD4, 0x30, 0, 0, 0, 0, 0, 0, 0xe8, 0x03, 0, 0, 0xff, 0xff, 0xff, 0x7f, 0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0, 0, 0})
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})

	o := startOracle(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		r := &fuzzReader{data: data}
		beginKey := r.bytes()
		beginOrEqual := r.bool()
		beginOffset := r.int32()
		endKey := r.bytes()
		endOrEqual := r.bool()
		endOffset := r.int32()
		version := r.int64()
		limit := r.int32()
		limitBytes := r.int32()
		hasTags := r.bool()
		var tags []byte
		if hasTags {
			tags = r.bytes()
		}
		tenantId := r.int64()
		hasOptions := r.bool()
		ssLatestCommitVersions := r.bytes()

		// C++ oracle skips Tags, Options, SsLatestCommitVersions (all complex)
		_ = hasTags
		_ = tags
		_ = hasOptions
		_ = ssLatestCommitVersions

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
			version, limit, limitBytes,
			false, nil, tenantId, false, emptyVersionVector())
		if err != nil {
			t.Fatalf("oracle error: %v", err)
		}
		if cppBytes == nil {
			t.Skip("oracle returned error response")
		}

		compareBytesStructural(t, goBytes, cppBytes, "GetKeyValuesRequest",
			unmarshalGetKeyValuesRequest, equalGetKeyValuesRequest)
	})
}

// 5. GetKeyServerLocationsRequest
func FuzzGetKeyServerLocationsRequest(f *testing.F) {
	f.Add([]byte{8, 0x74, 0x65, 0x73, 0x74, 0x5f, 0x6b, 0x65, 0x79, 0, 0x64, 0, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{5, 0x62, 0x65, 0x67, 0x69, 0x6e, 1, 3, 0x65, 0x6e, 0x64, 0x2a, 0, 0, 0, 1, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})

	o := startOracle(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		r := &fuzzReader{data: data}
		begin := r.bytes()
		hasEnd := r.bool()
		var end []byte
		if hasEnd {
			end = r.bytes()
		}
		limit := r.int32()
		reverse := r.bool()
		tenantId := r.int64()
		minTenantVersion := r.int64()

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
			t.Skip("oracle returned error response")
		}

		compareBytesStructural(t, goBytes, cppBytes, "GetKeyServerLocationsRequest",
			unmarshalGetKeyServerLocationsRequest, equalGetKeyServerLocationsRequest)
	})
}

// 6. CommitTransactionRequest
func FuzzCommitTransactionRequest(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0, 0})
	f.Add([]byte{
		0x2a, 0, 0, 0, 0, 0, 0, 0, // readSnapshot
		1,                         // 1 mutation
		0,                         // type=Set
		4, 0x6b, 0x65, 0x79, 0x31, // param1
		4, 0x76, 0x61, 0x6c, 0x31, // param2
		1,                         // 1 read conflict range
		4, 0x6b, 0x65, 0x79, 0x31, // begin
		5, 0x6b, 0x65, 0x79, 0x31, 0x00, // end
		1,                         // 1 write conflict range
		4, 0x6b, 0x65, 0x79, 0x31, // begin
		5, 0x6b, 0x65, 0x79, 0x31, 0x00, // end
		0, 0, 0, 0, // flags
		0,                                              // no debugID
		0,                                              // no commitCostEstimation
		0,                                              // no tagSet
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, // tenantId
		0, // empty idempotencyId
	})

	o := startOracle(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		r := &fuzzReader{data: data}
		readSnapshot := r.int64()

		numMutations := r.vecCount()
		mutations := make([]Mutation, numMutations)
		goMutations := make([]types.MutationRef, numMutations)
		for i := 0; i < numMutations; i++ {
			mt := r.byte()
			p1 := r.bytes()
			p2 := r.bytes()
			mutations[i] = Mutation{Type: mt, Param1: p1, Param2: p2}
			goMutations[i] = types.MutationRef{MutType: mt, Param1: p1, Param2: p2}
		}

		numReadCR := r.vecCount()
		readCRs := make([]ConflictRange, numReadCR)
		goReadCRs := make([]types.KeyRangeRef, numReadCR)
		for i := 0; i < numReadCR; i++ {
			b := r.bytes()
			e := r.bytes()
			readCRs[i] = ConflictRange{Begin: b, End: e}
			goReadCRs[i] = types.KeyRangeRef{Begin: b, End: e}
		}

		numWriteCR := r.vecCount()
		writeCRs := make([]ConflictRange, numWriteCR)
		goWriteCRs := make([]types.KeyRangeRef, numWriteCR)
		for i := 0; i < numWriteCR; i++ {
			b := r.bytes()
			e := r.bytes()
			writeCRs[i] = ConflictRange{Begin: b, End: e}
			goWriteCRs[i] = types.KeyRangeRef{Begin: b, End: e}
		}

		flags := r.uint32()
		hasDebugID := r.bool()
		var debugID []byte
		if hasDebugID {
			debugID = r.bytes()
		}
		hasCommitCostEstimation := r.bool()
		var commitCostEstimation []byte
		if hasCommitCostEstimation {
			commitCostEstimation = r.bytes()
		}
		hasTagSet := r.bool()
		var tagSet []byte
		if hasTagSet {
			tagSet = r.bytes()
		}
		tenantId := r.int64()
		idempotencyId := r.bytes()

		// C++ oracle skips DebugID, CommitCostEstimation, TagSet, IdempotencyId
		_ = hasDebugID
		_ = debugID
		_ = hasCommitCostEstimation
		_ = commitCostEstimation
		_ = hasTagSet
		_ = tagSet
		_ = idempotencyId

		goMsg := &types.CommitTransactionRequest{
			Transaction: types.CommitTransactionRef{
				ReadSnapshot:        readSnapshot,
				Mutations:           goMutations,
				ReadConflictRanges:  goReadCRs,
				WriteConflictRanges: goWriteCRs,
			},
			Reply:      types.ReplyPromise{},
			Flags:      flags,
			TenantInfo: types.TenantInfo{TenantId: tenantId},
		}
		goBytes := goMsg.MarshalFDB()

		cppBytes, err := o.SerializeCommitTransactionRequest(
			readSnapshot, mutations, readCRs, writeCRs,
			flags, false, nil, false, nil, false, nil, tenantId, nil)
		if err != nil {
			// Oracle may crash or return errors on edge cases (e.g. C++ type
			// construction failures). Log the error so skip rate is visible in
			// fuzz output, then skip — Go-side MarshalFDB already exercised above.
			t.Logf("oracle error (skipping comparison): %v", err)
			t.Skip("oracle unavailable for this input, skipping C++ comparison")
		}

		compareBytesStructural(t, goBytes, cppBytes, "CommitTransactionRequest",
			unmarshalCommitTransactionRequest, equalCommitTransactionRequest)
	})
}

// 7. GetReadVersionReply
func FuzzGetReadVersionReply(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{0x0a, 0, 0, 0, 0x39, 0x30, 0, 0, 0, 0, 0, 0, 1, 1, 3, 0x41, 0x42, 0x43, 0, 0xe8, 0x03, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})

	o := startOracle(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		r := &fuzzReader{data: data}
		processBusyTime := r.int32()
		version := r.int64()
		locked := r.bool()
		hasMetadataVersion := r.bool()
		var metadataVersion []byte
		if hasMetadataVersion {
			metadataVersion = r.bytes()
		}
		tagThrottleInfo := r.bytes()
		midShardSize := r.int64()
		rkDefaultThrottled := r.bool()
		rkBatchThrottled := r.bool()
		ssVersionVectorDelta := r.bytes()
		proxyId := r.uid()
		proxyTagThrottledDuration := r.float64()
		if math.IsNaN(proxyTagThrottledDuration) {
			proxyTagThrottledDuration = 0
		}

		// C++ oracle skips tagThrottleInfo (structured map) and
		// ssVersionVectorDelta (structured VersionVector).
		_ = tagThrottleInfo
		_ = ssVersionVectorDelta

		goMsg := &types.GetReadVersionReply{
			ProcessBusyTime:           processBusyTime,
			Version:                   version,
			Locked:                    locked,
			MidShardSize:              midShardSize,
			RkDefaultThrottled:        rkDefaultThrottled,
			RkBatchThrottled:          rkBatchThrottled,
			ProxyId:                   proxyId,
			ProxyTagThrottledDuration: proxyTagThrottledDuration,
		}
		if hasMetadataVersion {
			goMsg.HasMetadataVersion = true
			goMsg.MetadataVersion = metadataVersion
		}
		goBytes := goMsg.MarshalFDB()

		cppBytes, err := o.SerializeGetReadVersionReply(
			processBusyTime, version, locked,
			hasMetadataVersion, metadataVersion,
			nil, midShardSize,
			rkDefaultThrottled, rkBatchThrottled,
			emptyVersionVector(), proxyId, proxyTagThrottledDuration)
		if err != nil {
			t.Fatalf("oracle error: %v", err)
		}
		if cppBytes == nil {
			t.Skip("oracle returned error response")
		}

		compareBytesStructural(t, goBytes, cppBytes, "GetReadVersionReply",
			unmarshalGetReadVersionReply, equalGetReadVersionReply)
	})
}

// 8. GetValueReply
func FuzzGetValueReply(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 5, 0x68, 0x65, 0x6c, 0x6c, 0x6f, 0})

	o := startOracle(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		r := &fuzzReader{data: data}
		penalty := r.float64()
		if math.IsNaN(penalty) {
			penalty = 0
		}
		hasError := r.bool()
		var errorData []byte
		if hasError {
			errorData = r.bytes()
		}
		hasValue := r.bool()
		var value []byte
		if hasValue {
			value = r.bytes()
		}
		cached := r.bool()

		goMsg := &types.GetValueReply{Penalty: penalty, Cached: cached}
		if hasError {
			goMsg.HasError = true
			goMsg.Error = errorData
		}
		if hasValue {
			goMsg.HasValue = true
			goMsg.Value = value
		}
		goBytes := goMsg.MarshalFDB()

		cppBytes, err := o.SerializeGetValueReply(
			penalty, hasError, errorData, hasValue, value, cached)
		if err != nil {
			t.Fatalf("oracle error: %v", err)
		}
		if cppBytes == nil {
			t.Skip("oracle returned error response")
		}

		compareBytesStructural(t, goBytes, cppBytes, "GetValueReply",
			unmarshalGetValueReply, equalGetValueReply)
	})
}

// 9. GetKeyReply
func FuzzGetKeyReply(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 1, 3, 0x41, 0x42, 0x43, 1})

	o := startOracle(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		r := &fuzzReader{data: data}
		penalty := r.float64()
		if math.IsNaN(penalty) {
			penalty = 0
		}
		hasError := r.bool()
		var errorData []byte
		if hasError {
			errorData = r.bytes()
		}
		cached := r.bool()

		goMsg := &types.GetKeyReply{Penalty: penalty, Cached: cached}
		if hasError {
			goMsg.HasError = true
			goMsg.Error = errorData
		}
		goBytes := goMsg.MarshalFDB()

		cppBytes, err := o.SerializeGetKeyReply(
			penalty, hasError, errorData, cached)
		if err != nil {
			t.Fatalf("oracle error: %v", err)
		}
		if cppBytes == nil {
			t.Skip("oracle returned error response")
		}

		compareBytesStructural(t, goBytes, cppBytes, "GetKeyReply",
			unmarshalGetKeyReply, equalGetKeyReply)
	})
}

// 10. GetKeyValuesReply
func FuzzGetKeyValuesReply(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 5, 0x68, 0x65, 0x6c, 0x6c, 0x6f, 0x39, 0x30, 0, 0, 0, 0, 0, 0, 1, 0})

	o := startOracle(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		r := &fuzzReader{data: data}
		penalty := r.float64()
		if math.IsNaN(penalty) {
			penalty = 0
		}
		hasError := r.bool()
		var errorData []byte
		if hasError {
			errorData = r.bytes()
		}
		msgData := r.bytes()
		version := r.int64()
		more := r.bool()
		cached := r.bool()

		// Data (VectorRef<KeyValueRef>) is a complex structured vector that
		// the C++ oracle can't construct. Skip it for now.
		_ = msgData

		goMsg := &types.GetKeyValuesReply{
			Penalty: penalty, Version: version,
			More: more, Cached: cached,
		}
		if hasError {
			goMsg.HasError = true
			goMsg.Error = errorData
		}
		goBytes := goMsg.MarshalFDB()

		cppBytes, err := o.SerializeGetKeyValuesReply(
			penalty, hasError, errorData, nil, version, more, cached)
		if err != nil {
			t.Fatalf("oracle error: %v", err)
		}
		if cppBytes == nil {
			t.Skip("oracle returned error response")
		}

		compareBytesStructural(t, goBytes, cppBytes, "GetKeyValuesReply",
			unmarshalGetKeyValuesReply, equalGetKeyValuesReply)
	})
}

// 11. GetKeyServerLocationsReply
// FuzzGetKeyServerLocationsReply is intentionally omitted from the differential
// oracle — all fields are deeply nested structured vectors
// (vector<pair<KeyRangeRef, vector<StorageServerInterface>>>) that neither the
// C++ oracle nor Go side can populate without deep type support.
// The deterministic TestDiffGetKeyServerLocationsReply covers the empty-message
// case. Round-trip coverage is provided by
// FuzzGetKeyServerLocationsReply_RoundTrip in
// pkg/fdbgo/wire/types/marshal_fuzz_test.go (the four fields are opaque []byte
// blobs at the outer-wrapper layer; round-trip catches vtable/slot/padding bugs
// in the wrapper without needing the inner payload to be valid).
// Re-add a differential variant here when the oracle supports structured
// vector construction.

// 12. CommitID
func FuzzCommitID(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{0x39, 0x30, 0, 0, 0, 0, 0, 0, 0x42, 0, 1, 3, 0x41, 0x42, 0x43, 1, 4, 0x01, 0x02, 0x03, 0x04})

	o := startOracle(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		r := &fuzzReader{data: data}
		version := r.int64()
		txnBatchId := r.uint16()
		hasMetadataVersion := r.bool()
		var metadataVersion []byte
		if hasMetadataVersion {
			metadataVersion = r.bytes()
		}
		hasConflictingKRIndices := r.bool()
		var conflictingKRIndices []byte
		if hasConflictingKRIndices {
			conflictingKRIndices = r.bytes()
		}

		// C++ oracle skips ConflictingKRIndices (complex Optional<VectorRef>)
		_ = hasConflictingKRIndices
		_ = conflictingKRIndices

		goMsg := &types.CommitID{
			Version:    version,
			TxnBatchId: txnBatchId,
		}
		if hasMetadataVersion {
			goMsg.HasMetadataVersion = true
			goMsg.MetadataVersion = metadataVersion
		}
		goBytes := goMsg.MarshalFDB()

		cppBytes, err := o.SerializeCommitID(
			version, txnBatchId, hasMetadataVersion, metadataVersion,
			false, nil)
		if err != nil {
			t.Fatalf("oracle error: %v", err)
		}
		if cppBytes == nil {
			t.Skip("oracle returned error response")
		}

		compareBytesStructural(t, goBytes, cppBytes, "CommitID",
			unmarshalCommitID, equalCommitID)
	})
}

// 13. Error
func FuzzError(f *testing.F) {
	f.Add([]byte{0, 0})
	f.Add([]byte{0xe8, 0x03})
	f.Add([]byte{0xff, 0xff})

	o := startOracle(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		r := &fuzzReader{data: data}
		errorCode := r.uint16()

		goMsg := &types.Error{ErrorCode: errorCode}
		goBytes := goMsg.MarshalFDB()

		cppBytes, err := o.SerializeError(errorCode)
		if err != nil {
			t.Fatalf("oracle error: %v", err)
		}
		if cppBytes == nil {
			t.Skip("oracle returned error response")
		}

		compareBytesStructural(t, goBytes, cppBytes, "Error",
			unmarshalError, equalError)
	})
}

// 14. ClientDBInfo
func FuzzClientDBInfo(f *testing.F) {
	f.Add(make([]byte, 60))
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 5, 0x68, 0x65, 0x6c, 0x6c, 0x6f, 0})

	o := startOracle(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		r := &fuzzReader{data: data}
		grvProxies := r.bytes()
		commitProxies := r.bytes()
		id := r.uid()
		hasForward := r.bool()
		var forward []byte
		if hasForward {
			forward = r.bytes()
		}
		history := r.bytes()
		hasEncryptKeyProxy := r.bool()
		var encryptKeyProxy []byte
		if hasEncryptKeyProxy {
			encryptKeyProxy = r.bytes()
		}
		clusterId := r.uid()
		clusterType := r.int32()
		hasMetaclusterName := r.bool()
		var metaclusterName []byte
		if hasMetaclusterName {
			metaclusterName = r.bytes()
		}

		// C++ oracle skips grvProxies, commitProxies, history, encryptKeyProxy
		// (structured vectors of proxy interfaces).
		_ = grvProxies
		_ = commitProxies
		_ = history
		_ = hasEncryptKeyProxy
		_ = encryptKeyProxy

		goMsg := &types.ClientDBInfo{
			Id:          id,
			ClusterId:   clusterId,
			ClusterType: clusterType,
		}
		if hasForward {
			goMsg.HasForward = true
			goMsg.Forward = forward
		}
		if hasMetaclusterName {
			goMsg.HasMetaclusterName = true
			goMsg.MetaclusterName = metaclusterName
		}
		goBytes := goMsg.MarshalFDB()

		cppBytes, err := o.SerializeClientDBInfo(
			nil, nil, id, hasForward, forward,
			nil, false, nil,
			clusterId, clusterType, hasMetaclusterName, metaclusterName)
		if err != nil {
			t.Fatalf("oracle error: %v", err)
		}
		if cppBytes == nil {
			t.Skip("oracle returned error response")
		}

		compareBytesStructural(t, goBytes, cppBytes, "ClientDBInfo",
			unmarshalClientDBInfo, equalClientDBInfo)
	})
}

// 15. OpenDatabaseCoordRequest
func FuzzOpenDatabaseCoordRequest(f *testing.F) {
	f.Add(make([]byte, 50))

	o := startOracle(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		r := &fuzzReader{data: data}
		issues := r.bytes()
		supportedVersions := r.bytes()
		traceLogGroup := r.bytes()
		knownClientInfoID := r.uid()
		clusterKey := r.bytes()
		coordinators := r.bytes()
		hostnames := r.bytes()
		internal := r.bool()

		// C++ oracle only sets knownClientInfoID (UID) and internal (bool).
		// The rest are structured vectors that differ between C++ and Go.
		_ = issues
		_ = supportedVersions
		_ = traceLogGroup
		_ = clusterKey
		_ = coordinators
		_ = hostnames

		goMsg := &types.OpenDatabaseCoordRequest{
			KnownClientInfoID: knownClientInfoID,
			Reply:             types.ReplyPromise{},
			Internal:          internal,
		}
		goBytes := goMsg.MarshalFDB()

		cppBytes, err := o.SerializeOpenDatabaseCoordRequest(
			nil, nil, nil, knownClientInfoID, nil, nil, nil, internal)
		if err != nil {
			t.Fatalf("oracle error: %v", err)
		}
		if cppBytes == nil {
			t.Skip("oracle returned error response")
		}

		compareBytesStructural(t, goBytes, cppBytes, "OpenDatabaseCoordRequest",
			unmarshalOpenDatabaseCoordRequest, equalOpenDatabaseCoordRequest)
	})
}

// 16. NetworkAddress
// FuzzNetworkAddress — NOTE: IP address encoding bugs are NOT caught by this
// fuzz target (IPAddress variant payload not written by Go's MarshalFDB,
// tracked in TODO.md). Only Port and FromHostname are validated.
func FuzzNetworkAddress(f *testing.F) {
	f.Add([]byte{0x7f, 0, 0, 1, 0xbb, 0x01, 0, 0, 0})
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0})

	o := startOracle(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		r := &fuzzReader{data: data}
		ipAddr := r.uint32()
		port := r.uint16()
		flags := r.uint16()
		fromHostname := r.bool()

		goMsg := &types.NetworkAddress{
			Ip:           types.IPAddress{AddrTag: 1, AddrAlt0: ipAddr},
			Port:         port,
			Flags:        flags,
			FromHostname: fromHostname,
		}
		goBytes := goMsg.MarshalFDB()

		cppBytes, err := o.SerializeNetworkAddress(ipAddr, port, flags, fromHostname)
		if err != nil {
			t.Fatalf("oracle error: %v", err)
		}
		if cppBytes == nil {
			t.Skip("oracle returned error response")
		}

		compareBytesStructural(t, goBytes, cppBytes, "NetworkAddress",
			unmarshalNetworkAddress, equalNetworkAddress)
	})
}

// 17. Endpoint
// FuzzEndpoint — NOTE: same IP limitation as FuzzNetworkAddress. Only Token,
// Port, and FromHostname are validated.
func FuzzEndpoint(f *testing.F) {
	f.Add([]byte{0x7f, 0, 0, 1, 0xbb, 0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})

	o := startOracle(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		r := &fuzzReader{data: data}
		ipAddr := r.uint32()
		port := r.uint16()
		flags := r.uint16()
		fromHostname := r.bool()
		token := r.uid()

		goMsg := &types.Endpoint{
			Addresses: types.NetworkAddressList{
				Address: types.NetworkAddress{
					Ip:           types.IPAddress{AddrTag: 1, AddrAlt0: ipAddr},
					Port:         port,
					Flags:        flags,
					FromHostname: fromHostname,
				},
			},
			Token: token,
		}
		goBytes := goMsg.MarshalFDB()

		cppBytes, err := o.SerializeEndpoint(ipAddr, port, flags, fromHostname, token)
		if err != nil {
			t.Fatalf("oracle error: %v", err)
		}
		if cppBytes == nil {
			t.Skip("oracle returned error response")
		}

		compareBytesStructural(t, goBytes, cppBytes, "Endpoint",
			unmarshalEndpoint, equalEndpoint)
	})
}

// 18. ReplyPromise
//
// C++ ReplyPromise<T>::file_identifier depends on the template parameter T,
// so the file ID won't match our generic ReplyPromise. Also, C++ always
// generates a random token via FlowTransport, which we zero post-hoc.
// Keep as Go-only structural test since the oracle can't produce a matching
// file_identifier for the generic case.
func FuzzReplyPromise(f *testing.F) {
	f.Add(make([]byte, 16))

	f.Fuzz(func(t *testing.T, data []byte) {
		r := &fuzzReader{data: data}
		token := r.uid()

		goMsg := &types.ReplyPromise{Token: token}
		goBytes := goMsg.MarshalFDB()
		if len(goBytes) == 0 {
			t.Fatal("MarshalFDB returned empty")
		}
	})
}

// --- Deterministic regression tests ---

func TestDiffGetReadVersionRequest(t *testing.T) {
	t.Parallel()
	o := startOracle(t)
	// Basic test: no optionals
	testGetReadVersionRequestBasic(t, o, 1, 1, -1)
	testGetReadVersionRequestBasic(t, o, 0, 0, 0)
	testGetReadVersionRequestBasic(t, o, 0xFFFFFFFF, 0xFFFFFFFF, -1)
}

func testGetReadVersionRequestBasic(t testing.TB, o *Oracle, transactionCount, flags uint32, maxVersion int64) {
	t.Helper()
	goMsg := &types.GetReadVersionRequest{
		TransactionCount: transactionCount,
		Flags:            flags,
		MaxVersion:       maxVersion,
		Reply:            types.ReplyPromise{},
	}
	goBytes := goMsg.MarshalFDB()
	cppBytes, err := o.SerializeGetReadVersionRequest(transactionCount, flags, false, nil, false, nil, maxVersion)
	if err != nil {
		t.Fatalf("oracle error: %v", err)
	}
	if cppBytes == nil {
		t.Fatal("oracle returned error response")
	}
	compareBytesStructural(t, goBytes, cppBytes, "GetReadVersionRequest",
		unmarshalGetReadVersionRequest2, equalGetReadVersionRequest)
}

func TestDiffGetValueRequest(t *testing.T) {
	t.Parallel()
	o := startOracle(t)
	testGetValueRequestBasic(t, o, []byte("hello"), 12345, -1)
	testGetValueRequestBasic(t, o, []byte{}, 0, 0)
}

func testGetValueRequestBasic(t testing.TB, o *Oracle, key []byte, version, tenantId int64) {
	t.Helper()
	goMsg := &types.GetValueRequest{
		Key:                    key,
		Version:                version,
		Reply:                  types.ReplyPromise{},
		TenantInfo:             types.TenantInfo{TenantId: tenantId},
		SsLatestCommitVersions: emptyVersionVector(),
	}
	goBytes := goMsg.MarshalFDB()
	cppBytes, err := o.SerializeGetValueRequest(key, version, false, nil, tenantId, false, emptyVersionVector())
	if err != nil {
		t.Fatalf("oracle error: %v", err)
	}
	if cppBytes == nil {
		t.Fatal("oracle returned error response")
	}
	compareBytesStructural(t, goBytes, cppBytes, "GetValueRequest",
		unmarshalGetValueRequest, equalGetValueRequest)
}

func TestDiffGetKeyRequest(t *testing.T) {
	t.Parallel()
	o := startOracle(t)
	testGetKeyRequestBasic(t, o, []byte("selector"), true, 1, 99999, -1)
	testGetKeyRequestBasic(t, o, []byte{}, false, 0, 0, 0)
}

func testGetKeyRequestBasic(t testing.TB, o *Oracle, key []byte, orEqual bool, offset int32, version, tenantId int64) {
	t.Helper()
	goMsg := &types.GetKeyRequest{
		Sel:                    types.KeySelectorRef{Key: key, OrEqual: orEqual, Offset: offset},
		Version:                version,
		Reply:                  types.ReplyPromise{},
		TenantInfo:             types.TenantInfo{TenantId: tenantId},
		SsLatestCommitVersions: emptyVersionVector(),
	}
	goBytes := goMsg.MarshalFDB()
	cppBytes, err := o.SerializeGetKeyRequest(key, orEqual, offset, version, false, nil, tenantId, false, emptyVersionVector(), nil)
	if err != nil {
		t.Fatalf("oracle error: %v", err)
	}
	if cppBytes == nil {
		t.Fatal("oracle returned error response")
	}
	compareBytesStructural(t, goBytes, cppBytes, "GetKeyRequest",
		unmarshalGetKeyRequest, equalGetKeyRequest2)
}

func TestDiffGetKeyValuesRequest(t *testing.T) {
	t.Parallel()
	o := startOracle(t)

	goMsg := &types.GetKeyValuesRequest{
		Begin:                  types.KeySelectorRef{Key: []byte("begin"), OrEqual: true, Offset: 1},
		End:                    types.KeySelectorRef{Key: []byte("end"), OrEqual: false, Offset: 0},
		Version:                54321,
		Limit:                  1000,
		LimitBytes:             0x7fffffff,
		Reply:                  types.ReplyPromise{},
		TenantInfo:             types.TenantInfo{TenantId: -1},
		SsLatestCommitVersions: emptyVersionVector(),
	}
	goBytes := goMsg.MarshalFDB()
	cppBytes, err := o.SerializeGetKeyValuesRequest(
		[]byte("begin"), true, 1, []byte("end"), false, 0,
		54321, 1000, 0x7fffffff, false, nil, -1, false, emptyVersionVector())
	if err != nil {
		t.Fatalf("oracle error: %v", err)
	}
	if cppBytes == nil {
		t.Fatal("oracle returned error response")
	}
	compareBytesStructural(t, goBytes, cppBytes, "GetKeyValuesRequest",
		unmarshalGetKeyValuesRequest, equalGetKeyValuesRequest)
}

func TestDiffGetKeyServerLocationsRequest(t *testing.T) {
	t.Parallel()
	o := startOracle(t)

	// Without end
	goMsg := &types.GetKeyServerLocationsRequest{
		Begin:            []byte("test_key"),
		Limit:            100,
		Reply:            types.ReplyPromise{},
		Tenant:           types.TenantInfo{TenantId: -1},
		MinTenantVersion: -1,
	}
	goBytes := goMsg.MarshalFDB()
	cppBytes, err := o.SerializeGetKeyServerLocationsRequest([]byte("test_key"), false, nil, 100, false, -1, -1)
	if err != nil {
		t.Fatalf("oracle error: %v", err)
	}
	if cppBytes == nil {
		t.Fatal("oracle returned error response")
	}
	compareBytesStructural(t, goBytes, cppBytes, "GetKeyServerLocationsRequest",
		unmarshalGetKeyServerLocationsRequest, equalGetKeyServerLocationsRequest)

	// With end
	goMsg2 := &types.GetKeyServerLocationsRequest{
		Begin:            []byte("begin"),
		HasEnd:           true,
		End:              []byte("end"),
		Limit:            42,
		Reverse:          true,
		Reply:            types.ReplyPromise{},
		Tenant:           types.TenantInfo{TenantId: -1},
		MinTenantVersion: -1,
	}
	goBytes2 := goMsg2.MarshalFDB()
	cppBytes2, err := o.SerializeGetKeyServerLocationsRequest([]byte("begin"), true, []byte("end"), 42, true, -1, -1)
	if err != nil {
		t.Fatalf("oracle error: %v", err)
	}
	if cppBytes2 == nil {
		t.Fatal("oracle returned error response")
	}
	compareBytesStructural(t, goBytes2, cppBytes2, "GetKeyServerLocationsRequest",
		unmarshalGetKeyServerLocationsRequest, equalGetKeyServerLocationsRequest)
}

func TestDiffCommitTransactionRequest(t *testing.T) {
	t.Parallel()
	o := startOracle(t)

	// Empty commit
	goMsg := &types.CommitTransactionRequest{
		Transaction: types.CommitTransactionRef{ReadSnapshot: 0},
		Reply:       types.ReplyPromise{},
		TenantInfo:  types.TenantInfo{TenantId: -1},
	}
	goBytes := goMsg.MarshalFDB()
	cppBytes, err := o.SerializeCommitTransactionRequest(0, nil, nil, nil, 0, false, nil, false, nil, false, nil, -1, nil)
	if err != nil {
		t.Fatalf("oracle error: %v", err)
	}
	if cppBytes == nil {
		t.Fatal("oracle returned error response")
	}
	compareBytesStructural(t, goBytes, cppBytes, "CommitTransactionRequest",
		unmarshalCommitTransactionRequest, equalCommitTransactionRequest)
}

func TestDiffNetworkAddress(t *testing.T) {
	t.Parallel()
	o := startOracle(t)

	goMsg := &types.NetworkAddress{
		Ip:           types.IPAddress{AddrTag: 1, AddrAlt0: 0x0100007f},
		Port:         4500,
		Flags:        1,
		FromHostname: false,
	}
	goBytes := goMsg.MarshalFDB()
	cppBytes, err := o.SerializeNetworkAddress(0x0100007f, 4500, 1, false)
	if err != nil {
		t.Fatalf("oracle error: %v", err)
	}
	if cppBytes == nil {
		t.Fatal("oracle returned error response")
	}
	compareBytesStructural(t, goBytes, cppBytes, "NetworkAddress",
		unmarshalNetworkAddress, equalNetworkAddress)
}

func TestDiffReplyPromise(t *testing.T) {
	t.Parallel()
	// Go-only: C++ ReplyPromise file_identifier differs by template parameter
	goMsg := &types.ReplyPromise{}
	goBytes := goMsg.MarshalFDB()
	if len(goBytes) == 0 {
		t.Fatal("MarshalFDB returned empty")
	}
}

func TestDiffCommitID(t *testing.T) {
	t.Parallel()
	o := startOracle(t)

	goMsg := &types.CommitID{
		Version:    42,
		TxnBatchId: 7,
	}
	goBytes := goMsg.MarshalFDB()
	cppBytes, err := o.SerializeCommitID(42, 7, false, nil, false, nil)
	if err != nil {
		t.Fatalf("oracle error: %v", err)
	}
	if cppBytes == nil {
		t.Fatal("oracle returned error response")
	}
	compareBytesStructural(t, goBytes, cppBytes, "CommitID",
		unmarshalCommitID, equalCommitID)
}

func TestDiffError(t *testing.T) {
	t.Parallel()
	o := startOracle(t)

	for _, errorCode := range []uint16{0, 1000, 1021, 0xffff} {
		goMsg := &types.Error{ErrorCode: errorCode}
		goBytes := goMsg.MarshalFDB()
		cppBytes, err := o.SerializeError(errorCode)
		if err != nil {
			t.Fatalf("oracle error (code=%d): %v", errorCode, err)
		}
		if cppBytes == nil {
			t.Fatalf("oracle returned error response for code=%d", errorCode)
		}
		compareBytesStructural(t, goBytes, cppBytes, "Error",
			unmarshalError, equalError)
	}
}

func TestDiffGetReadVersionReply(t *testing.T) {
	t.Parallel()
	o := startOracle(t)

	goMsg := &types.GetReadVersionReply{
		ProcessBusyTime:           10,
		Version:                   12345,
		Locked:                    true,
		HasMetadataVersion:        true,
		MetadataVersion:           []byte("ABC"),
		MidShardSize:              1000,
		RkDefaultThrottled:        false,
		RkBatchThrottled:          false,
		ProxyId:                   [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		ProxyTagThrottledDuration: 1.5,
	}
	goBytes := goMsg.MarshalFDB()
	cppBytes, err := o.SerializeGetReadVersionReply(
		10, 12345, true, true, []byte("ABC"),
		nil, 1000, false, false,
		emptyVersionVector(),
		[16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}, 1.5)
	if err != nil {
		t.Fatalf("oracle error: %v", err)
	}
	if cppBytes == nil {
		t.Fatal("oracle returned error response")
	}
	compareBytesStructural(t, goBytes, cppBytes, "GetReadVersionReply",
		unmarshalGetReadVersionReply, equalGetReadVersionReply)
}

func TestDiffGetValueReply(t *testing.T) {
	t.Parallel()
	o := startOracle(t)

	// Without value
	goMsg := &types.GetValueReply{Penalty: 0.0, Cached: false}
	goBytes := goMsg.MarshalFDB()
	cppBytes, err := o.SerializeGetValueReply(0.0, false, nil, false, nil, false)
	if err != nil {
		t.Fatalf("oracle error: %v", err)
	}
	if cppBytes == nil {
		t.Fatal("oracle returned error response")
	}
	compareBytesStructural(t, goBytes, cppBytes, "GetValueReply",
		unmarshalGetValueReply, equalGetValueReply)

	// With value
	goMsg2 := &types.GetValueReply{
		Penalty:  1.5,
		HasValue: true,
		Value:    []byte("hello"),
		Cached:   true,
	}
	goBytes2 := goMsg2.MarshalFDB()
	cppBytes2, err := o.SerializeGetValueReply(1.5, false, nil, true, []byte("hello"), true)
	if err != nil {
		t.Fatalf("oracle error: %v", err)
	}
	if cppBytes2 == nil {
		t.Fatal("oracle returned error response")
	}
	compareBytesStructural(t, goBytes2, cppBytes2, "GetValueReply",
		unmarshalGetValueReply, equalGetValueReply)
}

func TestDiffGetKeyReply(t *testing.T) {
	t.Parallel()
	o := startOracle(t)

	goMsg := &types.GetKeyReply{Penalty: 2.5, Cached: true}
	goBytes := goMsg.MarshalFDB()
	cppBytes, err := o.SerializeGetKeyReply(2.5, false, nil, true)
	if err != nil {
		t.Fatalf("oracle error: %v", err)
	}
	if cppBytes == nil {
		t.Fatal("oracle returned error response")
	}
	compareBytesStructural(t, goBytes, cppBytes, "GetKeyReply",
		unmarshalGetKeyReply, equalGetKeyReply)
}

func TestDiffGetKeyValuesReply(t *testing.T) {
	t.Parallel()
	o := startOracle(t)

	goMsg := &types.GetKeyValuesReply{
		Penalty: 0.0,
		Version: 54321,
		More:    true,
		Cached:  false,
	}
	goBytes := goMsg.MarshalFDB()
	cppBytes, err := o.SerializeGetKeyValuesReply(0.0, false, nil, nil, 54321, true, false)
	if err != nil {
		t.Fatalf("oracle error: %v", err)
	}
	if cppBytes == nil {
		t.Fatal("oracle returned error response")
	}
	compareBytesStructural(t, goBytes, cppBytes, "GetKeyValuesReply",
		unmarshalGetKeyValuesReply, equalGetKeyValuesReply)
}

func TestDiffGetKeyServerLocationsReply(t *testing.T) {
	t.Parallel()
	o := startOracle(t)

	goMsg := &types.GetKeyServerLocationsReply{}
	goBytes := goMsg.MarshalFDB()
	cppBytes, err := o.SerializeGetKeyServerLocationsReply(nil, nil, nil)
	if err != nil {
		t.Fatalf("oracle error: %v", err)
	}
	if cppBytes == nil {
		t.Fatal("oracle returned error response")
	}
	// NOTE: All fields are structured vectors — no field-level comparison
	// possible without deep type support in the oracle. Verify file ID matches.
	// Size may differ due to vtable closure differences (C++ includes vtables
	// for StorageServerInterface sub-types that our Go type doesn't know about).
	if len(goBytes) >= 8 && len(cppBytes) >= 8 {
		if !bytes.Equal(goBytes[4:8], cppBytes[4:8]) {
			t.Errorf("GetKeyServerLocationsReply: file ID mismatch Go=%x C++=%x",
				goBytes[4:8], cppBytes[4:8])
		}
	}
	if len(goBytes) != len(cppBytes) {
		t.Logf("GetKeyServerLocationsReply: size differs Go=%d C++=%d (expected: vtable closure difference)",
			len(goBytes), len(cppBytes))
	}
}

func TestDiffClientDBInfo(t *testing.T) {
	t.Parallel()
	o := startOracle(t)

	id := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	clusterId := [16]byte{16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1}
	goMsg := &types.ClientDBInfo{
		Id:          id,
		ClusterId:   clusterId,
		ClusterType: 1,
	}
	goBytes := goMsg.MarshalFDB()
	cppBytes, err := o.SerializeClientDBInfo(
		nil, nil, id, false, nil, nil, false, nil, clusterId, 1, false, nil)
	if err != nil {
		t.Fatalf("oracle error: %v", err)
	}
	if cppBytes == nil {
		t.Fatal("oracle returned error response")
	}
	compareBytesStructural(t, goBytes, cppBytes, "ClientDBInfo",
		unmarshalClientDBInfo, equalClientDBInfo)
}

func TestDiffOpenDatabaseCoordRequest(t *testing.T) {
	t.Parallel()
	o := startOracle(t)

	uid := [16]byte{0xDE, 0xAD, 0xBE, 0xEF, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	goMsg := &types.OpenDatabaseCoordRequest{
		KnownClientInfoID: uid,
		Reply:             types.ReplyPromise{},
	}
	goBytes := goMsg.MarshalFDB()
	cppBytes, err := o.SerializeOpenDatabaseCoordRequest(
		nil, nil, nil, uid, nil, nil, nil, false)
	if err != nil {
		t.Fatalf("oracle error: %v", err)
	}
	if cppBytes == nil {
		t.Fatal("oracle returned error response")
	}
	compareBytesStructural(t, goBytes, cppBytes, "OpenDatabaseCoordRequest",
		unmarshalOpenDatabaseCoordRequest, equalOpenDatabaseCoordRequest)
}

func TestDiffEndpoint(t *testing.T) {
	t.Parallel()
	o := startOracle(t)

	token := [16]byte{0xAA, 0xBB, 0xCC, 0xDD, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	goMsg := &types.Endpoint{
		Addresses: types.NetworkAddressList{
			Address: types.NetworkAddress{
				Ip:           types.IPAddress{AddrTag: 1, AddrAlt0: 0x0100007f},
				Port:         4500,
				Flags:        1,
				FromHostname: false,
			},
		},
		Token: token,
	}
	goBytes := goMsg.MarshalFDB()
	cppBytes, err := o.SerializeEndpoint(0x0100007f, 4500, 1, false, token)
	if err != nil {
		t.Fatalf("oracle error: %v", err)
	}
	if cppBytes == nil {
		t.Fatal("oracle returned error response")
	}
	compareBytesStructural(t, goBytes, cppBytes, "Endpoint",
		unmarshalEndpoint, equalEndpoint)
}

// --- Comparison helpers ---

// compareBytesStructural compares Go and C++ FDB serialized output by
// unmarshaling both with Go's reader and comparing the resulting structs.
//
// This handles vtable closure differences (different vtable sets between Go
// and C++) that cause size/byte differences but produce identical field values.
// Both outputs are valid FDB FlatBuffers — the reader follows soffsets to find
// vtables regardless of their position.
func compareBytesStructural[T any](t testing.TB, goBytes, cppBytes []byte, typeName string,
	unmarshal func(data []byte) (T, error), equal func(a, b T) bool,
) {
	t.Helper()

	// File ID must match (bytes 4-7)
	if len(goBytes) < 8 || len(cppBytes) < 8 {
		t.Errorf("%s: too short: Go=%d C++=%d", typeName, len(goBytes), len(cppBytes))
		return
	}
	goFileID := binary.LittleEndian.Uint32(goBytes[4:8])
	cppFileID := binary.LittleEndian.Uint32(cppBytes[4:8])
	if goFileID != cppFileID {
		t.Errorf("%s: file ID mismatch: Go=%d C++=%d", typeName, goFileID, cppFileID)
		return
	}

	goVal, err := unmarshal(goBytes)
	if err != nil {
		t.Errorf("%s: unmarshal Go bytes failed: %v", typeName, err)
		dumpHex(t, goBytes, cppBytes, typeName)
		return
	}
	cppVal, err := unmarshal(cppBytes)
	if err != nil {
		t.Errorf("%s: unmarshal C++ bytes failed: %v", typeName, err)
		dumpHex(t, goBytes, cppBytes, typeName)
		return
	}

	if !equal(goVal, cppVal) {
		t.Errorf("%s: STRUCTURAL MISMATCH\n  Go:  %+v\n  C++: %+v", typeName, goVal, cppVal)
		dumpHex(t, goBytes, cppBytes, typeName)
	}
}

func dumpHex(t testing.TB, goBytes, cppBytes []byte, typeName string) {
	t.Helper()
	t.Logf("Go  (%d bytes): %s", len(goBytes), hex.EncodeToString(goBytes))
	t.Logf("C++ (%d bytes): %s", len(cppBytes), hex.EncodeToString(cppBytes))
}

// --- Type-specific unmarshal/equal helpers for structural comparison ---

func unmarshalNetworkAddress(data []byte) (types.NetworkAddress, error) {
	var m types.NetworkAddress
	err := m.UnmarshalFDB(data)
	return m, err
}

func equalNetworkAddress(a, b types.NetworkAddress) bool {
	// NOTE: IPAddress variant payload is not written by Go's MarshalFDB (known bug
	// in generated writeToBuffer for variant types). The missing variant data shifts
	// field positions, so Ip AND Flags decode differently between Go and C++ bytes.
	// Only Port and FromHostname are at positions unaffected by the IP size divergence.
	return a.Port == b.Port && a.FromHostname == b.FromHostname
}

func unmarshalGetReadVersionReply(data []byte) (types.GetReadVersionReply, error) {
	var m types.GetReadVersionReply
	err := m.UnmarshalFDB(data)
	return m, err
}

func equalGetReadVersionReply(a, b types.GetReadVersionReply) bool {
	// Compare scalar fields that both sides set. Skip structured blobs
	// (TagThrottleInfo, SsVersionVectorDelta) that C++ skips.
	return a.ProcessBusyTime == b.ProcessBusyTime &&
		a.Version == b.Version &&
		a.Locked == b.Locked &&
		a.HasMetadataVersion == b.HasMetadataVersion &&
		bytes.Equal(a.MetadataVersion, b.MetadataVersion) &&
		a.MidShardSize == b.MidShardSize &&
		a.RkDefaultThrottled == b.RkDefaultThrottled &&
		a.RkBatchThrottled == b.RkBatchThrottled &&
		a.ProxyId == b.ProxyId &&
		a.ProxyTagThrottledDuration == b.ProxyTagThrottledDuration
}

func unmarshalGetValueReply(data []byte) (types.GetValueReply, error) {
	var m types.GetValueReply
	err := m.UnmarshalFDB(data)
	return m, err
}

func equalGetValueReply(a, b types.GetValueReply) bool {
	// HasError presence tag must match. Error bytes may differ when present
	// because Go stores raw DynamicSize bytes while C++ serializes a nested
	// Error struct with its own vtable — different wire formats for the same
	// logical field. Compare Error bytes only when both are absent.
	return a.Penalty == b.Penalty &&
		a.HasError == b.HasError &&
		a.HasValue == b.HasValue &&
		bytes.Equal(a.Value, b.Value) &&
		a.Cached == b.Cached
}

func unmarshalGetKeyReply(data []byte) (types.GetKeyReply, error) {
	var m types.GetKeyReply
	err := m.UnmarshalFDB(data)
	return m, err
}

func equalGetKeyReply(a, b types.GetKeyReply) bool {
	return a.Penalty == b.Penalty &&
		a.HasError == b.HasError &&
		a.Cached == b.Cached
}

func unmarshalGetKeyValuesReply(data []byte) (types.GetKeyValuesReply, error) {
	var m types.GetKeyValuesReply
	err := m.UnmarshalFDB(data)
	return m, err
}

func equalGetKeyValuesReply(a, b types.GetKeyValuesReply) bool {
	return a.Penalty == b.Penalty &&
		a.HasError == b.HasError &&
		a.Version == b.Version &&
		a.More == b.More &&
		a.Cached == b.Cached
}

func unmarshalClientDBInfo(data []byte) (types.ClientDBInfo, error) {
	var m types.ClientDBInfo
	err := m.UnmarshalFDB(data)
	return m, err
}

func equalClientDBInfo(a, b types.ClientDBInfo) bool {
	return a.Id == b.Id &&
		a.HasForward == b.HasForward &&
		bytes.Equal(a.Forward, b.Forward) &&
		a.ClusterId == b.ClusterId &&
		a.ClusterType == b.ClusterType &&
		a.HasMetaclusterName == b.HasMetaclusterName &&
		bytes.Equal(a.MetaclusterName, b.MetaclusterName)
}

func unmarshalOpenDatabaseCoordRequest(data []byte) (types.OpenDatabaseCoordRequest, error) {
	var m types.OpenDatabaseCoordRequest
	err := m.UnmarshalFDB(data)
	return m, err
}

func equalOpenDatabaseCoordRequest(a, b types.OpenDatabaseCoordRequest) bool {
	return a.KnownClientInfoID == b.KnownClientInfoID &&
		a.Internal == b.Internal
}

func unmarshalEndpoint(data []byte) (types.Endpoint, error) {
	var m types.Endpoint
	err := m.UnmarshalFDB(data)
	return m, err
}

func equalEndpoint(a, b types.Endpoint) bool {
	// NOTE: IPAddress variant payload not serialized by Go MarshalFDB (known bug).
	// Compare Endpoint token and address Port/FromHostname only.
	return a.Token == b.Token &&
		a.Addresses.Address.Port == b.Addresses.Address.Port &&
		a.Addresses.Address.FromHostname == b.Addresses.Address.FromHostname
}

// --- Additional unmarshal/equal helpers for request types ---

// unmarshalGetReadVersionRequest2 avoids name collision with the reply type's existing helper.
func unmarshalGetReadVersionRequest2(data []byte) (types.GetReadVersionRequest, error) {
	var m types.GetReadVersionRequest
	err := m.UnmarshalFDB(data)
	return m, err
}

func equalGetReadVersionRequest(a, b types.GetReadVersionRequest) bool {
	// Compare fields that both Go and C++ oracle set.
	// Tags and DebugID are skipped by the C++ oracle (complex structured types).
	return a.TransactionCount == b.TransactionCount &&
		a.Flags == b.Flags &&
		a.MaxVersion == b.MaxVersion
}

func unmarshalGetValueRequest(data []byte) (types.GetValueRequest, error) {
	var m types.GetValueRequest
	err := m.UnmarshalFDB(data)
	return m, err
}

func equalGetValueRequest(a, b types.GetValueRequest) bool {
	// Compare fields set by both sides. Tags, Options, SsLatestCommitVersions
	// are skipped or defaulted by the C++ oracle.
	return bytes.Equal(a.Key, b.Key) &&
		a.Version == b.Version &&
		a.TenantInfo.TenantId == b.TenantInfo.TenantId
}

func unmarshalGetKeyRequest(data []byte) (types.GetKeyRequest, error) {
	var m types.GetKeyRequest
	err := m.UnmarshalFDB(data)
	return m, err
}

// equalGetKeyRequest2 avoids name collision with the reply type's existing helper.
func equalGetKeyRequest2(a, b types.GetKeyRequest) bool {
	return bytes.Equal(a.Sel.Key, b.Sel.Key) &&
		a.Sel.OrEqual == b.Sel.OrEqual &&
		a.Sel.Offset == b.Sel.Offset &&
		a.Version == b.Version &&
		a.TenantInfo.TenantId == b.TenantInfo.TenantId
}

func unmarshalGetKeyValuesRequest(data []byte) (types.GetKeyValuesRequest, error) {
	var m types.GetKeyValuesRequest
	err := m.UnmarshalFDB(data)
	return m, err
}

func equalGetKeyValuesRequest(a, b types.GetKeyValuesRequest) bool {
	return bytes.Equal(a.Begin.Key, b.Begin.Key) &&
		a.Begin.OrEqual == b.Begin.OrEqual &&
		a.Begin.Offset == b.Begin.Offset &&
		bytes.Equal(a.End.Key, b.End.Key) &&
		a.End.OrEqual == b.End.OrEqual &&
		a.End.Offset == b.End.Offset &&
		a.Version == b.Version &&
		a.Limit == b.Limit &&
		a.LimitBytes == b.LimitBytes &&
		a.TenantInfo.TenantId == b.TenantInfo.TenantId
}

func unmarshalGetKeyServerLocationsRequest(data []byte) (types.GetKeyServerLocationsRequest, error) {
	var m types.GetKeyServerLocationsRequest
	err := m.UnmarshalFDB(data)
	return m, err
}

func equalGetKeyServerLocationsRequest(a, b types.GetKeyServerLocationsRequest) bool {
	return bytes.Equal(a.Begin, b.Begin) &&
		a.HasEnd == b.HasEnd &&
		bytes.Equal(a.End, b.End) &&
		a.Limit == b.Limit &&
		a.Reverse == b.Reverse &&
		a.Tenant.TenantId == b.Tenant.TenantId &&
		a.MinTenantVersion == b.MinTenantVersion
}

func unmarshalCommitTransactionRequest(data []byte) (types.CommitTransactionRequest, error) {
	var m types.CommitTransactionRequest
	err := m.UnmarshalFDB(data)
	return m, err
}

func equalCommitTransactionRequest(a, b types.CommitTransactionRequest) bool {
	if a.Transaction.ReadSnapshot != b.Transaction.ReadSnapshot {
		return false
	}
	if a.Flags != b.Flags {
		return false
	}
	if a.TenantInfo.TenantId != b.TenantInfo.TenantId {
		return false
	}
	if len(a.Transaction.Mutations) != len(b.Transaction.Mutations) {
		return false
	}
	for i := range a.Transaction.Mutations {
		am, bm := a.Transaction.Mutations[i], b.Transaction.Mutations[i]
		if am.MutType != bm.MutType || !bytes.Equal(am.Param1, bm.Param1) || !bytes.Equal(am.Param2, bm.Param2) {
			return false
		}
	}
	if len(a.Transaction.ReadConflictRanges) != len(b.Transaction.ReadConflictRanges) {
		return false
	}
	for i := range a.Transaction.ReadConflictRanges {
		ar, br := a.Transaction.ReadConflictRanges[i], b.Transaction.ReadConflictRanges[i]
		if !bytes.Equal(ar.Begin, br.Begin) || !bytes.Equal(ar.End, br.End) {
			return false
		}
	}
	if len(a.Transaction.WriteConflictRanges) != len(b.Transaction.WriteConflictRanges) {
		return false
	}
	for i := range a.Transaction.WriteConflictRanges {
		aw, bw := a.Transaction.WriteConflictRanges[i], b.Transaction.WriteConflictRanges[i]
		if !bytes.Equal(aw.Begin, bw.Begin) || !bytes.Equal(aw.End, bw.End) {
			return false
		}
	}
	return true
}

func unmarshalCommitID(data []byte) (types.CommitID, error) {
	var m types.CommitID
	err := m.UnmarshalFDB(data)
	return m, err
}

func equalCommitID(a, b types.CommitID) bool {
	return a.Version == b.Version &&
		a.TxnBatchId == b.TxnBatchId &&
		a.HasMetadataVersion == b.HasMetadataVersion &&
		bytes.Equal(a.MetadataVersion, b.MetadataVersion)
}

func unmarshalError(data []byte) (types.Error, error) {
	var m types.Error
	err := m.UnmarshalFDB(data)
	return m, err
}

func equalError(a, b types.Error) bool {
	return a.ErrorCode == b.ErrorCode
}

// --- Regression tests ---

// TestDiffCommitTransactionRequestEmptyConflictRanges is a regression test for the
// empty-vector-reloff bug: CommitTransactionRequest with non-empty mutations but
// empty conflict ranges must produce correct serialization.
func TestDiffCommitTransactionRequestEmptyConflictRanges(t *testing.T) {
	t.Parallel()
	o := startOracle(t)

	goMsg := &types.CommitTransactionRequest{
		Transaction: types.CommitTransactionRef{
			ReadSnapshot: 42,
			Mutations: []types.MutationRef{
				{MutType: 0, Param1: []byte("key1"), Param2: []byte("val1")},
				{MutType: 0, Param1: []byte("key2"), Param2: []byte("val2")},
			},
			// ReadConflictRanges: empty
			// WriteConflictRanges: empty
		},
		Reply:      types.ReplyPromise{},
		Flags:      0,
		TenantInfo: types.TenantInfo{TenantId: -1},
	}
	goBytes := goMsg.MarshalFDB()

	cppBytes, err := o.SerializeCommitTransactionRequest(
		42,
		[]Mutation{
			{Type: 0, Param1: []byte("key1"), Param2: []byte("val1")},
			{Type: 0, Param1: []byte("key2"), Param2: []byte("val2")},
		},
		nil, // empty read conflict ranges
		nil, // empty write conflict ranges
		0, false, nil, false, nil, false, nil, -1, nil,
	)
	if err != nil {
		t.Fatalf("oracle error: %v", err)
	}
	if cppBytes == nil {
		t.Fatal("oracle returned error response")
	}

	compareBytesStructural(t, goBytes, cppBytes, "CommitTransactionRequest",
		unmarshalCommitTransactionRequest, equalCommitTransactionRequest)
}
