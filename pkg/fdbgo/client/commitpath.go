package client

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"

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

	// DEBUG: verify file ID in footer (first 8 bytes: [rootOff][fileID])
	if len(body) >= 8 {
		rootOff := binary.LittleEndian.Uint32(body[0:4])
		fid := binary.LittleEndian.Uint32(body[4:8])
		fmt.Fprintf(os.Stderr, "COMMIT bodyLen=%d rootOff=%d fileID=0x%x first16=%x\n",
			len(body), rootOff, fid, body[:16])
	}

	// DEBUG: round-trip validation — marshal, unmarshal, compare.
	if os.Getenv("FDB_DEBUG_COMMIT") != "" {
		var check types.CommitTransactionRequest
		if err := check.UnmarshalFDB(body); err != nil {
			fmt.Fprintf(os.Stderr, "COMMIT ROUNDTRIP UNMARSHAL FAILED: %v\nmutations=%d readCR=%d writeCR=%d bodyLen=%d\n",
				err, len(tx.mutations), len(tx.readConflicts), len(tx.writeConflicts), len(body))
		} else {
			if len(check.Transaction.Mutations) != len(tx.mutations) {
				fmt.Fprintf(os.Stderr, "COMMIT MUTATION COUNT MISMATCH: sent=%d got=%d\n",
					len(tx.mutations), len(check.Transaction.Mutations))
			}
			for i, m := range tx.mutations {
				fmt.Fprintf(os.Stderr, "  mutation[%d]: type=%d keyLen=%d valLen=%d\n", i, m.Type, len(m.Key), len(m.Value))
			}
			// Also validate mutation data round-trips correctly
			for i, m := range check.Transaction.Mutations {
				if i < len(tx.mutations) {
					orig := tx.mutations[i]
					if m.MutType != uint8(orig.Type) {
						fmt.Fprintf(os.Stderr, "  MISMATCH mutation[%d] type: sent=%d got=%d\n", i, orig.Type, m.MutType)
					}
					if !bytesEqual(m.Param1, orig.Key) {
						fmt.Fprintf(os.Stderr, "  MISMATCH mutation[%d] key: sentLen=%d gotLen=%d\n", i, len(orig.Key), len(m.Param1))
					}
					if !bytesEqual(m.Param2, orig.Value) {
						fmt.Fprintf(os.Stderr, "  MISMATCH mutation[%d] value: sentLen=%d gotLen=%d\n", i, len(orig.Value), len(m.Param2))
					}
				}
			}
		}
	}

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

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
