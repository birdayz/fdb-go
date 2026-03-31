package client

import (
	"context"
	"errors"
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
	vt := types.CommitTransactionRequestVTable
	fileID := types.CommitTransactionRequestFileID

	// Serialize vectors through wire/types.
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

	w := wire.NewWriter(nil)
	// Add nested structs in REVERSE serialization order so the Writer
	// produces byte layout matching C++ (last added = highest byte addr).
	return w.WriteMessage(fileID, vt, 4, func(obj *wire.ObjectWriter) {
		// slot 10: TenantInfo (nested struct with tenantId=-1)
		tenantVT := wire.VTable{10, 17, 4, 16, 12}
		obj.WriteStruct(int(vt[10+2]), tenantVT, 8, func(inner *wire.ObjectWriter) {
			inner.WriteInt64(4, -1) // tenantId = -1 (no tenant)
		})

		// slot 9: SpanContext (nested struct, all zeros = default)
		spanVT := wire.VTable{10, 29, 4, 20, 28}
		obj.WriteStruct(int(vt[9+2]), spanVT, 8, func(inner *wire.ObjectWriter) {
			// Default SpanContext: all zeros
		})

		// slot 1: Reply (nested ReplyPromise with UID)
		replyVT := wire.VTable{6, 20, 4}
		obj.WriteStruct(int(vt[1+2]), replyVT, 8, func(inner *wire.ObjectWriter) {
			inner.WriteUint64(4, replyToken.First)
			inner.WriteUint64(12, replyToken.Second)
		})

		// slot 0: Transaction (nested CommitTransactionRef)
		obj.WriteStruct(int(vt[0+2]), types.CommitTransactionRefVTable, 8, func(inner *wire.ObjectWriter) {
			// slot 3: read_snapshot (int64) at offset 4
			inner.WriteInt64(4, tx.readVersion)
			// slot 0: read_conflict_ranges at offset 12
			inner.WriteRawOOL(12, readCRData)
			// slot 1: write_conflict_ranges at offset 16
			inner.WriteRawOOL(16, writeCRData)
			// slot 2: mutations at offset 20
			inner.WriteRawOOL(20, mutData)
			// Remaining fields (report_conflicting_keys, lock_aware,
			// spanContext, tenantIds) left at zero defaults.
		})

		// slot 2: Flags (uint32) — 0
		obj.WriteUint32(int(vt[2+2]), 0)

		// slot 11: IdempotencyId (empty bytes)
		obj.WriteBytes(int(vt[11+2]), nil)
	})
}

// parseCommitReply parses an ErrorOr<CommitID> response.
func (tx *Transaction) parseCommitReply(data []byte) error {
	if _, err := wire.ReadErrorOr(data); err != nil {
		var we *wire.FDBWireError
		if errors.As(err, &we) {
			return &FDBError{Code: we.Code, Message: fmt.Sprintf("commit error %d", we.Code)}
		}
		return fmt.Errorf("commit: %w", err)
	}
	var reply types.CommitID
	if err := reply.UnmarshalFDB(data); err != nil {
		return fmt.Errorf("unmarshal CommitID: %w", err)
	}
	tx.committedVersion = reply.Version
	return nil
}
