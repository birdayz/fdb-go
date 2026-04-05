package client

import (
	"context"
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

	rctx, cancel := context.WithTimeout(ctx, DefaultRPCTimeout)
	defer cancel()

	select {
	case resp := <-replyCh:
		if resp.Err != nil {
			tx.db.handleConnError(proxy.Address)
			tx.db.kickTopology()
			return &wire.FDBError{Code: ErrCommitUnknownResult}
		}
		return tx.parseCommitReply(resp.Body)
	case <-rctx.Done():
		cancelReply()
		return &wire.FDBError{Code: ErrCommitUnknownResult}
	}
}

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

	req := types.CommitTransactionRequest{
		Transaction: types.CommitTransactionRef{
			ReadConflictRanges:  readCRs,
			WriteConflictRanges: writeCRs,
			Mutations:           mutations,
			ReadSnapshot:        tx.readVersion,
			Lock_aware:          tx.lockAware,
		},
		Reply:      types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
		TenantInfo: types.TenantInfo{TenantId: NoTenantID},
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
