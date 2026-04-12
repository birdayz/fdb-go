package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

// commit sends a CommitTransactionRequest to a commit proxy.
// ALL connection errors → commit_unknown_result, matching C++ AtMostOnce::True.
// No distinction between dial/send/response failure — C++ makes zero distinction
// (all are broken_promise → request_maybe_delivered → commit_unknown_result).
func (tx *Transaction) commit(ctx context.Context) error {
	proxy, err := tx.db.getCommitProxy()
	if err != nil {
		return &wire.FDBError{Code: ErrAllProxiesUnreachable}
	}

	conn, err := tx.db.getOrDial(ctx, proxy.Address)
	if err != nil {
		tx.db.handleConnError(proxy.Address)
		tx.db.kickTopology()
		return &wire.FDBError{Code: ErrCommitUnknownResult}
	}

	replyToken, replyCh, cancelReply := conn.PrepareReply()
	body, poolBuf := buildCommitTransactionRequest(tx, replyToken)

	if err := conn.SendFrame(proxy.Token, body); err != nil {
		marshalBufPool.Put(poolBuf)
		cancelReply()
		tx.db.handleConnError(proxy.Address)
		tx.db.kickTopology()
		return &wire.FDBError{Code: ErrCommitUnknownResult}
	}
	// body is copied into WriteFrame's own buffer — safe to return to pool.
	marshalBufPool.Put(poolBuf)

	// Monitor for proxy list changes while waiting for commit reply.
	// C++ onProxiesChanged: if the proxy we committed to is no longer in the
	// active set, the commit result is unknown. Detect this immediately instead
	// of waiting for the RPC timeout.
	proxiesChanged := tx.db.waitProxiesChanged()

	resp, err := waitReplyOrProxiesChanged(replyCh, ctx, DefaultRPCTimeout, proxiesChanged)
	if err != nil {
		cancelReply()
		return &wire.FDBError{Code: ErrCommitUnknownResult}
	}
	if resp.Err != nil {
		tx.db.handleConnError(proxy.Address)
		tx.db.kickTopology()
		return &wire.FDBError{Code: ErrCommitUnknownResult}
	}
	return tx.parseCommitReply(resp.Body)
}

// metadataVersionKey is \xff/metadataVersion — the only key exempt from tenant prefix.
var metadataVersionKey = []byte("\xff/metadataVersion")

// buildCommitTransactionRequest constructs the full request with
// typed mutations and conflict ranges — no pre-serialization blobs.
// Pools for commit request construction. Avoids per-commit slice allocations.
var (
	mutationSlicePool = sync.Pool{New: func() any { s := make([]types.MutationRef, 0, 16); return &s }}
	crSlicePool       = sync.Pool{New: func() any { s := make([]types.KeyRangeRef, 0, 8); return &s }}
)

// marshalBufPool pools the serialization buffer for CommitTransactionRequest.
// Avoids ~11% of total commit-path allocations. Uses *[]byte to avoid
// interface boxing allocation (same pattern as writeFramePool).
var marshalBufPool = sync.Pool{New: func() any {
	b := make([]byte, 0, 4096)
	return &b
}}

// buildCommitTransactionRequest constructs the full request. Returns the
// serialized body and a pool handle — caller MUST call releaseMarshalBuf
// after the body is no longer needed (after SendFrame).
func buildCommitTransactionRequest(tx *Transaction, replyToken transport.UID) (body []byte, poolBuf *[]byte) {
	mutSlice := mutationSlicePool.Get().(*[]types.MutationRef)
	mutations := (*mutSlice)[:0]
	for _, m := range tx.mutations {
		mutations = append(mutations, types.MutationRef{MutType: uint8(m.Type), Param1: m.Key, Param2: m.Value})
	}

	readCRSlice := crSlicePool.Get().(*[]types.KeyRangeRef)
	readCRs := (*readCRSlice)[:0]
	for _, kr := range tx.readConflicts {
		readCRs = append(readCRs, types.KeyRangeRef{Begin: kr.Begin, End: kr.End})
	}

	writeCRSlice := crSlicePool.Get().(*[]types.KeyRangeRef)
	writeCRs := (*writeCRSlice)[:0]
	for _, kr := range tx.writeConflicts {
		writeCRs = append(writeCRs, types.KeyRangeRef{Begin: kr.Begin, End: kr.End})
	}

	// C++ applyTenantPrefix: when a tenant is set, prepend 8-byte big-endian tenant ID
	// to all mutation keys, read/write conflict range keys. Skip metadataVersionKey.
	if tx.tenantId >= 0 {
		var prefix [8]byte
		binary.BigEndian.PutUint64(prefix[:], uint64(tx.tenantId))
		for i := range mutations {
			m := &mutations[i]
			if !bytes.Equal(m.Param1, metadataVersionKey) {
				m.Param1 = append(prefix[:], m.Param1...)
				if m.MutType == uint8(MutClearRange) {
					m.Param2 = append(prefix[:], m.Param2...)
				} else if m.MutType == uint8(MutSetVersionstampedKey) {
					// The last 4 bytes of the key are a LE uint32 offset where the
					// versionstamp should be placed. After prepending the tenant
					// prefix, the offset must be adjusted by the prefix length.
					// Matches C++ applyTenantPrefix (NativeAPI.actor.cpp:6533-6536).
					if len(m.Param1) >= 4 {
						off := binary.LittleEndian.Uint32(m.Param1[len(m.Param1)-4:])
						off += 8 // tenant prefix is 8 bytes
						binary.LittleEndian.PutUint32(m.Param1[len(m.Param1)-4:], off)
					}
				}
			}
		}
		for i := range readCRs {
			cr := &readCRs[i]
			if !bytes.Equal(cr.Begin, metadataVersionKey) {
				cr.Begin = append(prefix[:], cr.Begin...)
				cr.End = append(prefix[:], cr.End...)
			}
		}
		for i := range writeCRs {
			cr := &writeCRs[i]
			if !bytes.Equal(cr.Begin, metadataVersionKey) {
				cr.Begin = append(prefix[:], cr.Begin...)
				cr.End = append(prefix[:], cr.End...)
			}
		}
	}

	// C++ CommitTransactionRequest flags (CommitProxyInterface.h):
	//   FLAG_IS_LOCK_AWARE = 0x1 — allows system key writes through resolver
	//   FLAG_FIRST_IN_BATCH = 0x2
	//   FLAG_BYPASS_STORAGE_QUOTA = 0x4
	var flags uint32
	if tx.lockAware {
		flags |= 0x1 // FLAG_IS_LOCK_AWARE
	}
	req := types.CommitTransactionRequest{
		Transaction: types.CommitTransactionRef{
			ReadConflictRanges:  readCRs,
			WriteConflictRanges: writeCRs,
			Mutations:           mutations,
			ReadSnapshot:        tx.readVersion,
			Lock_aware:          tx.lockAware,
		},
		Flags:      flags,
		Reply:      types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
		TenantInfo: types.TenantInfo{TenantId: tx.tenantId},
	}

	// Marshal with pooled buffer.
	bufp := marshalBufPool.Get().(*[]byte)
	result := req.MarshalFDBPooled(*bufp)
	*bufp = result // track capacity for pool reuse

	// Return pooled slices.
	*mutSlice = mutations
	mutationSlicePool.Put(mutSlice)
	*readCRSlice = readCRs
	crSlicePool.Put(readCRSlice)
	*writeCRSlice = writeCRs
	crSlicePool.Put(writeCRSlice)

	return result, bufp
}

// parseCommitReply parses an ErrorOr<CommitID> response.
func (tx *Transaction) parseCommitReply(data []byte) error {
	var r wire.Reader
	if err := wire.ReadErrorOrInto(data, &r); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	var reply types.CommitID
	reply.UnmarshalFromReader(&r)
	tx.committedVersion = reply.Version
	tx.txnBatchId = reply.TxnBatchId
	return nil
}
