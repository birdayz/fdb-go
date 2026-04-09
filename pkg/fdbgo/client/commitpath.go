package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"

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
	body := buildCommitTransactionRequest(tx, replyToken)

	if err := conn.SendFrame(proxy.Token, body); err != nil {
		cancelReply()
		tx.db.handleConnError(proxy.Address)
		tx.db.kickTopology()
		return &wire.FDBError{Code: ErrCommitUnknownResult}
	}

	resp, err := waitReply(replyCh, ctx, DefaultRPCTimeout)
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
func buildCommitTransactionRequest(tx *Transaction, replyToken transport.UID) []byte {
	mutations := make([]types.MutationRef, len(tx.mutations))
	for i, m := range tx.mutations {
		mutations[i] = types.MutationRef{MutType: uint8(m.Type), Param1: m.Key, Param2: m.Value}
	}

	readCRs := make([]types.KeyRangeRef, len(tx.readConflicts))
	for i, kr := range tx.readConflicts {
		readCRs[i] = types.KeyRangeRef{Begin: kr.Begin, End: kr.End}
	}

	writeCRs := make([]types.KeyRangeRef, len(tx.writeConflicts))
	for i, kr := range tx.writeConflicts {
		writeCRs[i] = types.KeyRangeRef{Begin: kr.Begin, End: kr.End}
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

	return req.MarshalFDB()
}

// parseCommitReply parses an ErrorOr<CommitID> response.
func (tx *Transaction) parseCommitReply(data []byte) error {
	if _, err := wire.ReadErrorOr(data); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	var reply types.CommitID
	if err := reply.UnmarshalFDB(data); err != nil {
		return fmt.Errorf("unmarshal CommitID: %w", err)
	}
	tx.committedVersion = reply.Version
	tx.txnBatchId = reply.TxnBatchId
	return nil
}
