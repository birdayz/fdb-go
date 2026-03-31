package client

import (
	"context"
	"fmt"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

// CommitTransactionRef, MutationRef, KeyRangeRef vtables from types.vtables_generated.go

// commit sends a CommitTransactionRequest to a commit proxy.
func (tx *Transaction) commit(ctx context.Context) error {
	proxy, err := tx.db.cluster.GetCommitProxy()
	if err != nil {
		return fmt.Errorf("get commit proxy: %w", err)
	}

	conn, err := tx.db.cluster.getOrDial(ctx, proxy.Address)
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

	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
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

// buildCommitTransactionRequest constructs the full request with embedded
// CommitTransactionRef (mutations + conflict ranges), ReplyPromise, and TenantInfo.
func buildCommitTransactionRequest(tx *Transaction, replyToken transport.UID) []byte {
	mutBlobs := make([][]byte, len(tx.mutations))
	for i, m := range tx.mutations {
		mutBlobs[i] = types.MarshalMutationRef(uint8(m.Type), m.Key, m.Value)
	}
	mutData := wire.PackVectorOfStructBlobs(mutBlobs)

	readCRBlobs := make([][]byte, len(tx.readConflicts))
	for i, kr := range tx.readConflicts {
		readCRBlobs[i] = types.MarshalKeyRangeRef(kr.Begin, kr.End)
	}
	readCRData := wire.PackVectorOfStructBlobs(readCRBlobs)

	writeCRBlobs := make([][]byte, len(tx.writeConflicts))
	for i, kr := range tx.writeConflicts {
		writeCRBlobs[i] = types.MarshalKeyRangeRef(kr.Begin, kr.End)
	}
	writeCRData := wire.PackVectorOfStructBlobs(writeCRBlobs)

	return types.MarshalCommitTransactionRequest(
		tx.readVersion,
		mutData, readCRData, writeCRData,
		replyToken.First, replyToken.Second,
		-1, // tenantId
	)
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
	return nil
}
