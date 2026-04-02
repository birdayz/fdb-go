package client

import (
	"context"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

// commit sends a CommitTransactionRequest to a commit proxy.
func (tx *Transaction) commit(ctx context.Context) error {
	proxy, err := tx.db.getCommitProxy()
	if err != nil {
		return fmt.Errorf("get commit proxy: %w", err)
	}

	conn, err := tx.db.getOrDial(ctx, proxy.Address)
	if err != nil {
		return fmt.Errorf("dial commit proxy (%s): %w", proxy.Address, err)
	}

	// Allocate reply token first — embedded in request body.
	replyToken, replyCh := conn.PrepareReply()

	body := buildCommitTransactionRequest(tx, replyToken)

	// commit is at endpoint index 0 from commit proxy (base token).
	if err := conn.SendFrame(proxy.Token, body); err != nil {
		return fmt.Errorf("send commit: %w", err)
	}

	rctx, cancel := context.WithTimeout(ctx, DefaultRPCTimeout)
	defer cancel()

	select {
	case resp := <-replyCh:
		if resp.Err != nil {
			return fmt.Errorf("commit response: %w", resp.Err)
		}
		return tx.parseCommitReply(resp.Body)
	case <-rctx.Done():
		return fmt.Errorf("commit timed out: %w", rctx.Err())
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
