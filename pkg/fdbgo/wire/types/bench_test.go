package types

import (
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
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
	mutations := make([]MutationRef, 5)
	for i := range mutations {
		mutations[i] = MutationRef{MutType: 0x06, Param1: []byte("set_key_xxxxxxxx"), Param2: []byte("set_value_yyyyyyyy")}
	}
	readCRs := make([]KeyRangeRef, 5)
	for i := range readCRs {
		readCRs[i] = KeyRangeRef{Begin: []byte("read_begin"), End: []byte("read_end")}
	}
	writeCRs := make([]KeyRangeRef, 5)
	for i := range writeCRs {
		writeCRs[i] = KeyRangeRef{Begin: []byte("write_begin"), End: []byte("write_end")}
	}

	req := CommitTransactionRequest{
		Transaction: CommitTransactionRef{
			ReadConflictRanges:  readCRs,
			WriteConflictRanges: writeCRs,
			Mutations:           mutations,
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

func BenchmarkMarshal_CommitTransactionRequest_Pooled(b *testing.B) {
	mutations := make([]MutationRef, 5)
	for i := range mutations {
		mutations[i] = MutationRef{MutType: 0x06, Param1: []byte("set_key_xxxxxxxx"), Param2: []byte("set_value_yyyyyyyy")}
	}
	readCRs := make([]KeyRangeRef, 5)
	for i := range readCRs {
		readCRs[i] = KeyRangeRef{Begin: []byte("read_begin"), End: []byte("read_end")}
	}
	writeCRs := make([]KeyRangeRef, 5)
	for i := range writeCRs {
		writeCRs[i] = KeyRangeRef{Begin: []byte("write_begin"), End: []byte("write_end")}
	}

	req := CommitTransactionRequest{
		Transaction: CommitTransactionRef{
			ReadConflictRanges:  readCRs,
			WriteConflictRanges: writeCRs,
			Mutations:           mutations,
			ReadSnapshot:        12345678900,
		},
		Reply:      ReplyPromise{Token: wire.UIDFromParts(0xAAAAAAAA, 0xBBBBBBBB)},
		TenantInfo: TenantInfo{TenantId: -1},
	}
	var buf []byte
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf = req.MarshalFDBPooled(buf)
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

// --- Unmarshal benchmarks ---

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
	reply := GetValueReply{Penalty: 0.0, Cached: false}
	data := reply.MarshalFDB()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var out GetValueReply
		_ = out.UnmarshalFDB(data)
	}
}

func BenchmarkUnmarshal_GetKeyValuesReply(b *testing.B) {
	kvs := make([]KeyValueRef, 10)
	for i := range kvs {
		kvs[i] = KeyValueRef{Key: []byte("key_0123456789abcdef"), Value: []byte("value_xxxxxxxxyyyyyyyy")}
	}
	kvData := packKVVector(kvs)
	reply := GetKeyValuesReply{Data: kvData, Version: 99999999999, More: true}
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
		Version: 99999999999, MidShardSize: 500000,
		SsVersionVectorDelta: make([]byte, 16),
		ProxyId:              wire.UIDFromParts(0x55555555, 0x66666666),
	}
	data := reply.MarshalFDB()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var out GetReadVersionReply
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
		kvs[i] = KeyValueRef{Key: []byte("key_0123456789abcdef"), Value: []byte("value_xxxxxxxxyyyyyyyy")}
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
		kvs[i] = KeyValueRef{Key: []byte("key_0123456789abcdef"), Value: []byte("value_xxxxxxxxyyyyyyyy")}
	}
	data := packKVVector(kvs)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ParseKeyValueRefStringVector(data)
	}
}
