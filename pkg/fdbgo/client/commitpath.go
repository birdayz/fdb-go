package client

import (
	"context"
	"fmt"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/protocol"
)

// commit sends a CommitTransactionRequest to a commit proxy.
func (tx *Transaction) commit(ctx context.Context) error {
	proxy, err := tx.db.cluster.GetCommitProxy()
	if err != nil {
		return fmt.Errorf("get commit proxy: %w", err)
	}

	conn, err := tx.db.cluster.getOrDial(ctx, proxy.Address)
	if err != nil {
		return fmt.Errorf("dial commit proxy: %w", err)
	}

	// Build CommitTransactionRequest.
	// The transaction field (CommitTransactionRef) is serialized as a nested struct
	// containing read_conflict_ranges, write_conflict_ranges, mutations, read_snapshot.
	// For now we send a minimal request.
	req := protocol.CommitTransactionRequest{
		Flags: 0,
	}
	// TODO: Serialize mutations, conflict ranges into the Transaction field.
	// This requires building CommitTransactionRef wire format.

	body := req.MarshalFDB()

	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	replyBody, err := conn.SendAndWait(rctx, proxy.Token, body)
	if err != nil {
		return fmt.Errorf("commit RPC: %w", err)
	}

	// Parse CommitID reply.
	var reply protocol.CommitID
	if err := reply.UnmarshalFDB(replyBody); err != nil {
		return fmt.Errorf("unmarshal CommitID: %w", err)
	}

	// Check for conflict.
	if reply.Version == -1 {
		return &FDBError{Code: ErrNotCommitted, Message: "transaction conflict"}
	}

	tx.committedVersion = reply.Version
	return nil
}
