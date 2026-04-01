package types

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

// Benchmarks for wire marshal/unmarshal of generated FDB types.
//
// Run:
//   bazelisk run //pkg/fdbgo/wire/types:types_test -- \
//     -test.run='^$' -test.bench=. -test.benchmem -test.count=3

// --- Marshal benchmarks ---

func BenchmarkMarshal_GetValueRequest(b *testing.B) {
	req := GetValueRequest{
		Key:                    []byte("test_key_1234567890"),
		Version:                12345678900,
		Reply:                  ReplyPromise{Token: wire.UIDFromParts(0xDEADBEEF, 0xCAFEBABE)},
		TenantInfo:             TenantInfo{TenantId: -1},
		SsLatestCommitVersions: make([]byte, 16),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = req.MarshalFDB()
	}
}

func BenchmarkMarshal_GetKeyValuesRequest(b *testing.B) {
	req := GetKeyValuesRequest{
		Begin:                  KeySelectorRef{Key: []byte("range_begin_key"), OrEqual: true, Offset: 1},
		End:                    KeySelectorRef{Key: []byte("range_end_key_zz"), OrEqual: true, Offset: 1},
		Version:                99999999999,
		Limit:                  1000,
		LimitBytes:             80000000,
		Reply:                  ReplyPromise{Token: wire.UIDFromParts(0x11111111, 0x22222222)},
		TenantInfo:             TenantInfo{TenantId: -1},
		SsLatestCommitVersions: make([]byte, 16),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = req.MarshalFDB()
	}
}

func BenchmarkMarshal_CommitTransactionRequest(b *testing.B) {
	// Simulate a commit with 5 mutations and 5 read/write conflict ranges.
	mutBlobs := make([][]byte, 5)
	for i := range mutBlobs {
		mutBlobs[i] = MarshalMutationRef(0x06, []byte("set_key_xxxxxxxx"), []byte("set_value_yyyyyyyy"))
	}
	mutData := wire.PackVectorOfStructBlobs(mutBlobs)

	readCRBlobs := make([][]byte, 5)
	for i := range readCRBlobs {
		readCRBlobs[i] = MarshalKeyRangeRef([]byte("read_begin"), []byte("read_end"))
	}
	readCRData := wire.PackVectorOfStructBlobs(readCRBlobs)

	writeCRBlobs := make([][]byte, 5)
	for i := range writeCRBlobs {
		writeCRBlobs[i] = MarshalKeyRangeRef([]byte("write_begin"), []byte("write_end"))
	}
	writeCRData := wire.PackVectorOfStructBlobs(writeCRBlobs)

	req := CommitTransactionRequest{
		Transaction: CommitTransactionRef{
			ReadConflictRanges:  readCRData,
			WriteConflictRanges: writeCRData,
			Mutations:           mutData,
			ReadSnapshot:        12345678900,
		},
		Reply:      ReplyPromise{Token: wire.UIDFromParts(0xAAAAAAAA, 0xBBBBBBBB)},
		TenantInfo: TenantInfo{TenantId: -1},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = req.MarshalFDB()
	}
}

func BenchmarkMarshal_GetReadVersionRequest(b *testing.B) {
	req := GetReadVersionRequest{
		TransactionCount: 1,
		Flags:            2,
		MaxVersion:       -1,
		Reply:            ReplyPromise{Token: wire.UIDFromParts(0x33333333, 0x44444444)},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = req.MarshalFDB()
	}
}

func BenchmarkMarshal_MutationRef_Blob(b *testing.B) {
	key := []byte("mutation_key_xxxx")
	val := []byte("mutation_value_yyyy")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = MarshalMutationRef(0x06, key, val)
	}
}

func BenchmarkMarshal_KeyRangeRef_Blob(b *testing.B) {
	begin := []byte("range_begin_key")
	end := []byte("range_end_key_zz")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = MarshalKeyRangeRef(begin, end)
	}
}

// --- Unmarshal benchmarks ---
// We marshal once to get valid wire bytes, then benchmark unmarshal.

func BenchmarkUnmarshal_GetValueRequest(b *testing.B) {
	req := GetValueRequest{
		Key:                    []byte("test_key_1234567890"),
		Version:                12345678900,
		Reply:                  ReplyPromise{Token: wire.UIDFromParts(0xDEADBEEF, 0xCAFEBABE)},
		TenantInfo:             TenantInfo{TenantId: -1},
		SsLatestCommitVersions: make([]byte, 16),
	}
	data := req.MarshalFDB()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var out GetValueRequest
		_ = out.UnmarshalFDB(data)
	}
}

func BenchmarkUnmarshal_GetValueReply(b *testing.B) {
	reply := GetValueReply{
		Penalty: 0.0,
		Cached:  false,
	}
	data := reply.MarshalFDB()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var out GetValueReply
		_ = out.UnmarshalFDB(data)
	}
}

func BenchmarkUnmarshal_GetKeyValuesReply(b *testing.B) {
	// Build a realistic reply with 10 KV pairs.
	kvs := make([]KeyValueRef, 10)
	for i := range kvs {
		kvs[i] = KeyValueRef{
			Key:   []byte("key_0123456789abcdef"),
			Value: []byte("value_xxxxxxxxyyyyyyyy"),
		}
	}
	kvData := packKVVector(kvs)

	reply := GetKeyValuesReply{
		Data:    kvData,
		Version: 99999999999,
		More:    true,
	}
	data := reply.MarshalFDB()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var out GetKeyValuesReply
		_ = out.UnmarshalFDB(data)
	}
}

func BenchmarkUnmarshal_GetReadVersionReply(b *testing.B) {
	reply := GetReadVersionReply{
		Version:                   99999999999,
		Locked:                    false,
		MidShardSize:              500000,
		SsVersionVectorDelta:      make([]byte, 16),
		ProxyId:                   wire.UIDFromParts(0x55555555, 0x66666666),
		ProxyTagThrottledDuration: 0.001,
	}
	data := reply.MarshalFDB()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var out GetReadVersionReply
		_ = out.UnmarshalFDB(data)
	}
}

func BenchmarkUnmarshal_CommitTransactionRequest(b *testing.B) {
	mutBlobs := make([][]byte, 5)
	for i := range mutBlobs {
		mutBlobs[i] = MarshalMutationRef(0x06, []byte("set_key_xxxxxxxx"), []byte("set_value_yyyyyyyy"))
	}
	mutData := wire.PackVectorOfStructBlobs(mutBlobs)

	readCRBlobs := make([][]byte, 3)
	for i := range readCRBlobs {
		readCRBlobs[i] = MarshalKeyRangeRef([]byte("read_begin"), []byte("read_end"))
	}
	readCRData := wire.PackVectorOfStructBlobs(readCRBlobs)

	req := CommitTransactionRequest{
		Transaction: CommitTransactionRef{
			ReadConflictRanges: readCRData,
			Mutations:          mutData,
			ReadSnapshot:       12345678900,
		},
		Reply:      ReplyPromise{Token: wire.UIDFromParts(0xAAAAAAAA, 0xBBBBBBBB)},
		TenantInfo: TenantInfo{TenantId: -1},
	}
	data := req.MarshalFDB()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var out CommitTransactionRequest
		_ = out.UnmarshalFDB(data)
	}
}

// --- Round-trip benchmarks ---

func BenchmarkRoundtrip_GetValueRequest(b *testing.B) {
	req := GetValueRequest{
		Key:                    []byte("test_key_1234567890"),
		Version:                12345678900,
		Reply:                  ReplyPromise{Token: wire.UIDFromParts(0xDEADBEEF, 0xCAFEBABE)},
		TenantInfo:             TenantInfo{TenantId: -1},
		SsLatestCommitVersions: make([]byte, 16),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		data := req.MarshalFDB()
		var out GetValueRequest
		_ = out.UnmarshalFDB(data)
	}
}

// --- Vector parse benchmarks ---

func BenchmarkParseKeyValueRefStringVector_10(b *testing.B) {
	kvs := make([]KeyValueRef, 10)
	for i := range kvs {
		kvs[i] = KeyValueRef{
			Key:   []byte("key_0123456789abcdef"),
			Value: []byte("value_xxxxxxxxyyyyyyyy"),
		}
	}
	data := packKVVector(kvs)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ParseKeyValueRefStringVector(data)
	}
}

func BenchmarkParseKeyValueRefStringVector_100(b *testing.B) {
	kvs := make([]KeyValueRef, 100)
	for i := range kvs {
		kvs[i] = KeyValueRef{
			Key:   []byte("key_0123456789abcdef"),
			Value: []byte("value_xxxxxxxxyyyyyyyy"),
		}
	}
	data := packKVVector(kvs)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ParseKeyValueRefStringVector(data)
	}
}

// --- StructBlob benchmarks (sub-object serialization) ---

func BenchmarkMarshalStructBlob_MutationRef(b *testing.B) {
	key := []byte("mutation_key_xxxx")
	val := []byte("mutation_value_yyyy")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = MarshalMutationRef(0x06, key, val)
	}
}

func BenchmarkPackVectorOfStructBlobs_5(b *testing.B) {
	blobs := make([][]byte, 5)
	for i := range blobs {
		blobs[i] = MarshalMutationRef(0x06, []byte("set_key_xxxxxxxx"), []byte("set_value_yyyyyyyy"))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = wire.PackVectorOfStructBlobs(blobs)
	}
}
