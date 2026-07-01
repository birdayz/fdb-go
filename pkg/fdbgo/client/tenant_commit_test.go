package client

import (
	"encoding/binary"
	"testing"

	"github.com/onsi/gomega"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// parseSerialized deserializes the FDB wire-format body produced by
// buildCommitTransactionRequest into a CommitTransactionRequest for inspection.
func parseSerialized(g gomega.Gomega, body []byte) types.CommitTransactionRequest {
	var req types.CommitTransactionRequest
	err := req.UnmarshalFDB(body)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	return req
}

// tenantPrefix returns the 8-byte big-endian encoding of the given tenant ID.
func tenantPrefix(id int64) []byte {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(id))
	return buf[:]
}

func TestTenantPrefixInCommit(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	const tenantID int64 = 42
	prefix := tenantPrefix(tenantID)

	t.Run("set_mutation_gets_prefix", func(t *testing.T) {
		t.Parallel()
		g := gomega.NewWithT(t)

		tx := &Transaction{tenantId: tenantID}
		tx.mutations = []Mutation{
			{Type: MutSetValue, Key: []byte("mykey"), Value: []byte("myval")},
		}

		body, poolBuf := buildCommitTransactionRequest(tx, transport.UID{}, tx.mutations, tx.writeConflicts)
		defer marshalBufPool.Put(poolBuf)

		req := parseSerialized(g, body)
		g.Expect(req.Transaction.Mutations).To(gomega.HaveLen(1))

		m := req.Transaction.Mutations[0]
		g.Expect(m.MutType).To(gomega.Equal(uint8(MutSetValue)))
		g.Expect(m.Param1).To(gomega.Equal(append(prefix, []byte("mykey")...)))
		g.Expect(m.Param2).To(gomega.Equal([]byte("myval"))) // value unchanged
	})

	t.Run("clear_range_both_params_get_prefix", func(t *testing.T) {
		t.Parallel()
		g := gomega.NewWithT(t)

		tx := &Transaction{tenantId: tenantID}
		tx.mutations = []Mutation{
			{Type: MutClearRange, Key: []byte("begin"), Value: []byte("end")},
		}

		body, poolBuf := buildCommitTransactionRequest(tx, transport.UID{}, tx.mutations, tx.writeConflicts)
		defer marshalBufPool.Put(poolBuf)

		req := parseSerialized(g, body)
		g.Expect(req.Transaction.Mutations).To(gomega.HaveLen(1))

		m := req.Transaction.Mutations[0]
		g.Expect(m.MutType).To(gomega.Equal(uint8(MutClearRange)))
		g.Expect(m.Param1).To(gomega.Equal(append(prefix, []byte("begin")...)))
		g.Expect(m.Param2).To(gomega.Equal(append(prefix, []byte("end")...)))
	})

	t.Run("set_versionstamped_key_offset_adjusted", func(t *testing.T) {
		t.Parallel()
		g := gomega.NewWithT(t)

		// Build a key with a trailing 4-byte LE offset.
		// "somekey" is 7 bytes, versionstamp offset = 7 (where versionstamp starts).
		origKey := []byte("somekey")
		const origOffset uint32 = 7
		vsKey := make([]byte, len(origKey)+4)
		copy(vsKey, origKey)
		binary.LittleEndian.PutUint32(vsKey[len(vsKey)-4:], origOffset)

		tx := &Transaction{tenantId: tenantID}
		tx.mutations = []Mutation{
			{Type: MutSetVersionstampedKey, Key: vsKey, Value: []byte("vsval")},
		}

		body, poolBuf := buildCommitTransactionRequest(tx, transport.UID{}, tx.mutations, tx.writeConflicts)
		defer marshalBufPool.Put(poolBuf)

		req := parseSerialized(g, body)
		g.Expect(req.Transaction.Mutations).To(gomega.HaveLen(1))

		m := req.Transaction.Mutations[0]
		g.Expect(m.MutType).To(gomega.Equal(uint8(MutSetVersionstampedKey)))

		// Key should be: prefix (8 bytes) + "somekey" + adjusted offset (4 bytes)
		expectedKey := make([]byte, 8+len(origKey)+4)
		copy(expectedKey, prefix)
		copy(expectedKey[8:], origKey)
		binary.LittleEndian.PutUint32(expectedKey[len(expectedKey)-4:], origOffset+8)

		g.Expect(m.Param1).To(gomega.Equal(expectedKey))
		g.Expect(m.Param2).To(gomega.Equal([]byte("vsval"))) // value unchanged

		// Verify the offset value explicitly.
		gotOffset := binary.LittleEndian.Uint32(m.Param1[len(m.Param1)-4:])
		g.Expect(gotOffset).To(gomega.Equal(origOffset + 8))
	})

	t.Run("metadata_version_key_exempt", func(t *testing.T) {
		t.Parallel()
		g := gomega.NewWithT(t)

		tx := &Transaction{tenantId: tenantID}
		tx.mutations = []Mutation{
			{Type: MutSetValue, Key: []byte("\xff/metadataVersion"), Value: []byte("v")},
		}
		// Use an End key that does NOT trigger the equalsKeyAfter serialization
		// optimization (where begin+'\x00' == end), because that optimization
		// swaps Begin/End on deserialization. Use a clearly different End.
		tx.readConflicts = []KeyRange{
			{Begin: []byte("\xff/metadataVersion"), End: []byte("\xff/metadataVersionZZ")},
		}

		body, poolBuf := buildCommitTransactionRequest(tx, transport.UID{}, tx.mutations, tx.writeConflicts)
		defer marshalBufPool.Put(poolBuf)

		req := parseSerialized(g, body)

		// Mutation key must NOT have the prefix.
		g.Expect(req.Transaction.Mutations).To(gomega.HaveLen(1))
		g.Expect(req.Transaction.Mutations[0].Param1).To(gomega.Equal([]byte("\xff/metadataVersion")))

		// Read conflict range whose Begin == metadataVersionKey must NOT have prefix.
		g.Expect(req.Transaction.ReadConflictRanges).To(gomega.HaveLen(1))
		g.Expect(req.Transaction.ReadConflictRanges[0].Begin).To(gomega.Equal([]byte("\xff/metadataVersion")))
		g.Expect(req.Transaction.ReadConflictRanges[0].End).To(gomega.Equal([]byte("\xff/metadataVersionZZ")))
	})

	t.Run("read_write_conflict_ranges_get_prefix", func(t *testing.T) {
		t.Parallel()
		g := gomega.NewWithT(t)

		tx := &Transaction{tenantId: tenantID}
		tx.readConflicts = []KeyRange{
			{Begin: []byte("a"), End: []byte("z")},
		}
		tx.writeConflicts = []KeyRange{
			{Begin: []byte("b"), End: []byte("y")},
		}

		body, poolBuf := buildCommitTransactionRequest(tx, transport.UID{}, tx.mutations, tx.writeConflicts)
		defer marshalBufPool.Put(poolBuf)

		req := parseSerialized(g, body)

		g.Expect(req.Transaction.ReadConflictRanges).To(gomega.HaveLen(1))
		g.Expect(req.Transaction.ReadConflictRanges[0].Begin).To(gomega.Equal(append(prefix, []byte("a")...)))
		g.Expect(req.Transaction.ReadConflictRanges[0].End).To(gomega.Equal(append(prefix, []byte("z")...)))

		g.Expect(req.Transaction.WriteConflictRanges).To(gomega.HaveLen(1))
		g.Expect(req.Transaction.WriteConflictRanges[0].Begin).To(gomega.Equal(append(prefix, []byte("b")...)))
		g.Expect(req.Transaction.WriteConflictRanges[0].End).To(gomega.Equal(append(prefix, []byte("y")...)))
	})

	t.Run("no_tenant_no_prefix", func(t *testing.T) {
		t.Parallel()
		g := gomega.NewWithT(t)

		tx := &Transaction{tenantId: NoTenantID} // -1 means no tenant
		tx.mutations = []Mutation{
			{Type: MutSetValue, Key: []byte("mykey"), Value: []byte("myval")},
		}
		tx.readConflicts = []KeyRange{
			{Begin: []byte("a"), End: []byte("z")},
		}

		body, poolBuf := buildCommitTransactionRequest(tx, transport.UID{}, tx.mutations, tx.writeConflicts)
		defer marshalBufPool.Put(poolBuf)

		req := parseSerialized(g, body)

		// No prefix when tenantId == NoTenantID.
		g.Expect(req.Transaction.Mutations).To(gomega.HaveLen(1))
		g.Expect(req.Transaction.Mutations[0].Param1).To(gomega.Equal([]byte("mykey")))

		g.Expect(req.Transaction.ReadConflictRanges).To(gomega.HaveLen(1))
		g.Expect(req.Transaction.ReadConflictRanges[0].Begin).To(gomega.Equal([]byte("a")))
	})

	t.Run("multiple_mutations_mixed", func(t *testing.T) {
		t.Parallel()
		g := gomega.NewWithT(t)

		// SetVersionstampedKey with offset 3.
		vsKey := make([]byte, 7) // "abc" + 4 bytes offset
		copy(vsKey, []byte("abc"))
		binary.LittleEndian.PutUint32(vsKey[3:], 3)

		tx := &Transaction{tenantId: tenantID}
		tx.mutations = []Mutation{
			{Type: MutSetValue, Key: []byte("key1"), Value: []byte("v1")},
			{Type: MutClearRange, Key: []byte("from"), Value: []byte("to")},
			{Type: MutSetVersionstampedKey, Key: vsKey, Value: []byte{}},
			{Type: MutSetValue, Key: []byte("\xff/metadataVersion"), Value: []byte("x")},
			{Type: MutAddValue, Key: []byte("counter"), Value: []byte{1, 0, 0, 0, 0, 0, 0, 0}},
		}

		body, poolBuf := buildCommitTransactionRequest(tx, transport.UID{}, tx.mutations, tx.writeConflicts)
		defer marshalBufPool.Put(poolBuf)

		req := parseSerialized(g, body)
		g.Expect(req.Transaction.Mutations).To(gomega.HaveLen(5))

		// [0] Set: key prefixed, value unchanged
		g.Expect(req.Transaction.Mutations[0].Param1).To(gomega.Equal(append(prefix, []byte("key1")...)))
		g.Expect(req.Transaction.Mutations[0].Param2).To(gomega.Equal([]byte("v1")))

		// [1] ClearRange: both prefixed
		g.Expect(req.Transaction.Mutations[1].Param1).To(gomega.Equal(append(prefix, []byte("from")...)))
		g.Expect(req.Transaction.Mutations[1].Param2).To(gomega.Equal(append(prefix, []byte("to")...)))

		// [2] SetVersionstampedKey: prefixed + offset bumped
		expectedVS := make([]byte, 8+7)
		copy(expectedVS, prefix)
		copy(expectedVS[8:], []byte("abc"))
		binary.LittleEndian.PutUint32(expectedVS[len(expectedVS)-4:], 3+8)
		g.Expect(req.Transaction.Mutations[2].Param1).To(gomega.Equal(expectedVS))

		// [3] metadataVersionKey: NOT prefixed
		g.Expect(req.Transaction.Mutations[3].Param1).To(gomega.Equal([]byte("\xff/metadataVersion")))

		// [4] AddValue (atomic op): key prefixed, value unchanged
		g.Expect(req.Transaction.Mutations[4].Param1).To(gomega.Equal(append(prefix, []byte("counter")...)))
		g.Expect(req.Transaction.Mutations[4].Param2).To(gomega.Equal([]byte{1, 0, 0, 0, 0, 0, 0, 0}))
	})

	t.Run("tenant_id_zero", func(t *testing.T) {
		t.Parallel()
		g := gomega.NewWithT(t)

		// tenantId == 0 is valid (>= 0), should still apply prefix (all zeroes).
		zeroPrefix := tenantPrefix(0)
		tx := &Transaction{tenantId: 0}
		tx.mutations = []Mutation{
			{Type: MutSetValue, Key: []byte("k"), Value: []byte("v")},
		}

		body, poolBuf := buildCommitTransactionRequest(tx, transport.UID{}, tx.mutations, tx.writeConflicts)
		defer marshalBufPool.Put(poolBuf)

		req := parseSerialized(g, body)
		g.Expect(req.Transaction.Mutations).To(gomega.HaveLen(1))
		g.Expect(req.Transaction.Mutations[0].Param1).To(gomega.Equal(append(zeroPrefix, []byte("k")...)))
	})

	t.Run("large_tenant_id", func(t *testing.T) {
		t.Parallel()
		g := gomega.NewWithT(t)

		// Max int64 value as tenant ID.
		const largeTenant int64 = 0x7FFFFFFFFFFFFFFF
		largePrefix := tenantPrefix(largeTenant)
		tx := &Transaction{tenantId: largeTenant}
		tx.mutations = []Mutation{
			{Type: MutSetValue, Key: []byte("k"), Value: []byte("v")},
		}

		body, poolBuf := buildCommitTransactionRequest(tx, transport.UID{}, tx.mutations, tx.writeConflicts)
		defer marshalBufPool.Put(poolBuf)

		req := parseSerialized(g, body)
		g.Expect(req.Transaction.Mutations).To(gomega.HaveLen(1))
		g.Expect(req.Transaction.Mutations[0].Param1).To(gomega.Equal(append(largePrefix, []byte("k")...)))
	})

	t.Run("versionstamp_offset_overflow_large", func(t *testing.T) {
		t.Parallel()
		g := gomega.NewWithT(t)

		// Test with a large offset near uint32 max to verify the +8 doesn't panic.
		// Offset 0xFFFFFFF0 + 8 = 0xFFFFFFF8 (still fits in uint32).
		vsKey := make([]byte, 5) // 1 byte key + 4 byte offset
		vsKey[0] = 'x'
		binary.LittleEndian.PutUint32(vsKey[1:], 0xFFFFFFF0)

		tx := &Transaction{tenantId: tenantID}
		tx.mutations = []Mutation{
			{Type: MutSetVersionstampedKey, Key: vsKey, Value: []byte{}},
		}

		body, poolBuf := buildCommitTransactionRequest(tx, transport.UID{}, tx.mutations, tx.writeConflicts)
		defer marshalBufPool.Put(poolBuf)

		req := parseSerialized(g, body)
		g.Expect(req.Transaction.Mutations).To(gomega.HaveLen(1))

		gotOffset := binary.LittleEndian.Uint32(req.Transaction.Mutations[0].Param1[len(req.Transaction.Mutations[0].Param1)-4:])
		g.Expect(gotOffset).To(gomega.Equal(uint32(0xFFFFFFF0 + 8)))
	})

	_ = g // parent g used only for shared prefix; subtests create their own
}

// TestTenantCommit_BuildTwiceNoDoublePrefix pins the #4 fix: building the commit
// request must be a pure read of transaction state. The tenant path used to
// prefix tx.mutations IN PLACE through the zero-copy unsafe alias, so a second
// build (e.g. a re-Commit without an intervening reset — commit() leaves the tx
// in txStateActive on failure) double-prefixed every key and double-bumped the
// versionstamp offset. C++ avoids this entirely: applyTenantPrefix builds a fresh
// VectorRef<MutationRef> and tryCommit takes the request by value
// (NativeAPI.actor.cpp:6523,6565). We mirror that with a scratch copy.
//
// This test fails on both axes if the scratch-copy fix is reverted:
//  1. the two builds produce different bytes (second is double-prefixed), and
//  2. tx.mutations is mutated in place after the first build.
func TestTenantCommit_BuildTwiceNoDoublePrefix(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	const tenantID int64 = 42
	prefix := tenantPrefix(tenantID)

	// SetVersionstampedKey with a trailing 4-byte LE offset = 7.
	vsKey := make([]byte, len("somekey")+4)
	copy(vsKey, []byte("somekey"))
	binary.LittleEndian.PutUint32(vsKey[len(vsKey)-4:], 7)

	tx := &Transaction{tenantId: tenantID}
	tx.mutations = []Mutation{
		{Type: MutSetValue, Key: []byte("k"), Value: []byte("v")},
		{Type: MutClearRange, Key: []byte("a"), Value: []byte("z")},
		{Type: MutSetVersionstampedKey, Key: vsKey, Value: []byte("vv")},
		{Type: MutSetValue, Key: []byte("\xff/metadataVersion"), Value: []byte("m")},
	}
	tx.readConflicts = []KeyRange{{Begin: []byte("rk"), End: []byte("rkZ")}}
	tx.writeConflicts = []KeyRange{{Begin: []byte("wk"), End: []byte("wkZ")}}

	// Snapshot the originals (type + key + value bytes) before any build, so an
	// in-place mutation of the persistent buffer is detectable byte-for-byte.
	type snap struct {
		typ      MutationType
		key, val []byte
	}
	before := make([]snap, len(tx.mutations))
	for i, m := range tx.mutations {
		before[i] = snap{m.Type, append([]byte(nil), m.Key...), append([]byte(nil), m.Value...)}
	}

	build := func() []byte {
		body, poolBuf := buildCommitTransactionRequest(tx, transport.UID{}, tx.mutations, tx.writeConflicts)
		out := append([]byte(nil), body...) // copy before the buffer returns to the pool
		marshalBufPool.Put(poolBuf)
		return out
	}

	body1 := build()

	// After the first build, the persistent mutation buffer must be untouched.
	g.Expect(tx.mutations).To(gomega.HaveLen(len(before)))
	for i := range before {
		g.Expect(tx.mutations[i].Type).To(gomega.Equal(before[i].typ),
			"build mutated tx.mutations[%d].Type in place", i)
		g.Expect([]byte(tx.mutations[i].Key)).To(gomega.Equal(before[i].key),
			"build mutated tx.mutations[%d].Key in place", i)
		g.Expect([]byte(tx.mutations[i].Value)).To(gomega.Equal(before[i].val),
			"build mutated tx.mutations[%d].Value in place", i)
	}

	body2 := build()

	// Identical input → identical wire bytes. A double-prefix on the rebuild
	// would change every key and the versionstamp offset.
	g.Expect(body2).To(gomega.Equal(body1), "rebuild produced different bytes (double-prefix)")

	// Decode the second build and assert exactly ONE tenant prefix everywhere.
	req := parseSerialized(g, body2)
	g.Expect(req.Transaction.Mutations).To(gomega.HaveLen(4))
	g.Expect(req.Transaction.Mutations[0].Param1).To(gomega.Equal(append(prefix, 'k')))
	g.Expect(req.Transaction.Mutations[1].Param1).To(gomega.Equal(append(prefix, 'a')))
	g.Expect(req.Transaction.Mutations[1].Param2).To(gomega.Equal(append(prefix, 'z')))

	// Versionstamp offset bumped by exactly 8 (single prefix), not 16.
	vs := req.Transaction.Mutations[2].Param1
	gotOffset := binary.LittleEndian.Uint32(vs[len(vs)-4:])
	g.Expect(gotOffset).To(gomega.Equal(uint32(7 + 8)))

	// metadataVersionKey stays exempt across rebuilds.
	g.Expect(req.Transaction.Mutations[3].Param1).To(gomega.Equal([]byte("\xff/metadataVersion")))

	// Conflict ranges carry exactly one prefix.
	g.Expect(req.Transaction.ReadConflictRanges[0].Begin).To(gomega.Equal(append(prefix, []byte("rk")...)))
	g.Expect(req.Transaction.WriteConflictRanges[0].End).To(gomega.Equal(append(prefix, []byte("wkZ")...)))
}
