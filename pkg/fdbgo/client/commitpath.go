package client

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/protocol"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

// CommitTransactionRef vtable extracted from C++ test vector.
// Fields (serialize order):
//
//	slot 0: read_conflict_ranges (VectorRef<KeyRangeRef>) at offset 12
//	slot 1: write_conflict_ranges (VectorRef<KeyRangeRef>) at offset 16
//	slot 2: mutations (VectorRef<MutationRef>) at offset 20
//	slot 3: read_snapshot (Version = int64) at offset 4
//	slot 4: report_conflicting_keys (bool) at offset 32
//	slot 5: lock_aware (bool) at offset 33
//	slot 6: spanContext type (uint8) at offset 34 (Optional)
//	slot 7: spanContext value (RelOff) at offset 24 (Optional)
//	slot 8: tenantIds type (uint8) at offset 35 (Optional)
//	slot 9: tenantIds value (RelOff) at offset 28 (Optional)
var commitTransactionRefVTable = wire.VTable{24, 36, 12, 16, 20, 4, 32, 33, 34, 24, 35, 28}

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
	vt := protocol.CommitTransactionRequest_VTable
	fileID := protocol.CommitTransactionRequest_FileIdentifier

	// Pre-serialize vectors as proper FlatBuffers nested objects.
	mutData := serializeMutationVector(tx.mutations)
	readCRData := serializeConflictRangeVector(tx.readConflicts)
	writeCRData := serializeConflictRangeVector(tx.writeConflicts)

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
		obj.WriteStruct(int(vt[0+2]), commitTransactionRefVTable, 8, func(inner *wire.ObjectWriter) {
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
	r, err := wire.NewReader(data)
	if err != nil {
		return fmt.Errorf("parse commit reply: %w", err)
	}

	// ErrorOr flattened by FakeRoot: ≤1 fields = Error, >1 = CommitID.
	nfields := r.VTableLength() - 2
	if nfields <= 1 {
		if r.FieldPresent(0) {
			errCode := r.ReadInt32(0)
			return &FDBError{Code: int(errCode), Message: fmt.Sprintf("commit error %d", errCode)}
		}
		return fmt.Errorf("empty commit response")
	}

	// Parse CommitID from the inner object.
	var reply protocol.CommitID
	if err := reply.UnmarshalFDB(data); err != nil {
		return fmt.Errorf("unmarshal CommitID: %w", err)
	}

	tx.committedVersion = reply.Version
	return nil
}

// MutationRef vtable: serializer(ar, type, param1, param2)
// type=uint8 at offset 12, param1=StringRef(RelOff) at offset 4, param2=StringRef(RelOff) at offset 8
var mutationRefVTable = wire.VTable{10, 13, 12, 4, 8}

// KeyRangeRef vtable: serializer(ar, begin, end)
// begin=StringRef(RelOff) at offset 4, end=StringRef(RelOff) at offset 8
var keyRangeRefVTable = wire.VTable{8, 12, 4, 8}

// serializeMutationVector packs mutations as VectorRef<MutationRef>.
// Each MutationRef is a full FlatBuffers nested object (serialize_member, NOT dynamic_size_traits).
func serializeMutationVector(muts []Mutation) []byte {
	// Build each mutation as a standalone FlatBuffers object blob
	blobs := make([][]byte, len(muts))
	for i, m := range muts {
		blobs[i] = buildMutationRefBlob(m)
	}
	return packVectorOfObjects(blobs)
}

// buildMutationRefBlob builds a single MutationRef as a FlatBuffers object blob.
// Layout: [vtable][padding][soffset+fields][param2_ool][param1_ool]
// C++ allocates OOL in reverse field order (param2 first, then param1).
func buildMutationRefBlob(m Mutation) []byte {
	vt := mutationRefVTable
	vtBytes := 10         // len(vt) * 2
	objSize := int(vt[1]) // 13

	// OOL: param2 first (C++ reverse allocation order), then param1
	param2OOL := packStringRef(m.Value)
	param1OOL := packStringRef(m.Key)

	// Compute positions
	vtPos := 0
	vtEnd := vtPos + vtBytes
	// Pad to align object to max(4, field_align)
	objPos := (vtEnd + 3) &^ 3
	objEnd := objPos + objSize

	// OOL goes AFTER object (higher byte address = forward from object)
	oolPos := (objEnd + 3) &^ 3
	p2Start := oolPos
	p2End := p2Start + len(param2OOL)
	p1Start := (p2End + 3) &^ 3
	p1End := p1Start + len(param1OOL)
	total := (p1End + 3) &^ 3

	buf := make([]byte, total)

	// Write vtable
	for j, v := range vt {
		binary.LittleEndian.PutUint16(buf[vtPos+j*2:], v)
	}

	// Write object
	// soffset: distance from object to vtable (positive = backward)
	binary.LittleEndian.PutUint32(buf[objPos:], uint32(int32(objPos-vtPos)))
	// param1 RelOff at object+4: forward to param1 OOL
	binary.LittleEndian.PutUint32(buf[objPos+4:], uint32(p1Start-(objPos+4)))
	// param2 RelOff at object+8: forward to param2 OOL
	binary.LittleEndian.PutUint32(buf[objPos+8:], uint32(p2Start-(objPos+8)))
	// type at object+12
	buf[objPos+12] = byte(m.Type)

	// Write OOL
	copy(buf[p2Start:], param2OOL)
	copy(buf[p1Start:], param1OOL)

	return buf
}

// serializeConflictRangeVector packs ranges as VectorRef<KeyRangeRef>.
// Each KeyRangeRef is a full FlatBuffers nested object (serialize_member).
func serializeConflictRangeVector(ranges []KeyRange) []byte {
	blobs := make([][]byte, len(ranges))
	for i, kr := range ranges {
		blobs[i] = buildKeyRangeRefBlob(kr)
	}
	return packVectorOfObjects(blobs)
}

// buildKeyRangeRefBlob builds a single KeyRangeRef as a FlatBuffers object blob.
// Layout: [vtable][padding][soffset+fields][end_ool][begin_ool]
func buildKeyRangeRefBlob(kr KeyRange) []byte {
	vt := keyRangeRefVTable
	vtBytes := 8          // len(vt) * 2
	objSize := int(vt[1]) // 12

	// OOL: end first (C++ reverse allocation order), then begin
	endOOL := packStringRef(kr.End)
	beginOOL := packStringRef(kr.Begin)

	vtPos := 0
	objPos := (vtBytes + 3) &^ 3 // align to 4
	objEnd := objPos + objSize

	oolPos := (objEnd + 3) &^ 3
	endStart := oolPos
	beginStart := (endStart + len(endOOL) + 3) &^ 3
	total := (beginStart + len(beginOOL) + 3) &^ 3

	buf := make([]byte, total)

	// Write vtable
	for j, v := range vt {
		binary.LittleEndian.PutUint16(buf[vtPos+j*2:], v)
	}

	// Write object
	binary.LittleEndian.PutUint32(buf[objPos:], uint32(int32(objPos-vtPos)))     // soffset
	binary.LittleEndian.PutUint32(buf[objPos+4:], uint32(beginStart-(objPos+4))) // begin RelOff
	binary.LittleEndian.PutUint32(buf[objPos+8:], uint32(endStart-(objPos+8)))   // end RelOff

	// Write OOL
	copy(buf[endStart:], endOOL)
	copy(buf[beginStart:], beginOOL)

	return buf
}

// packStringRef packs a StringRef as [length(4)][data] with 4-byte padding.
func packStringRef(data []byte) []byte {
	buf := make([]byte, 4+len(data))
	binary.LittleEndian.PutUint32(buf, uint32(len(data)))
	copy(buf[4:], data)
	return buf
}

// packVectorOfObjects builds a VectorRef<T> for serialize_member elements.
// Format: [count(4)][RelOff_0(4)]...[RelOff_N-1(4)][blob_0][blob_1]...
// Each RelOff points from its own position to the element blob's soffset.
func packVectorOfObjects(blobs [][]byte) []byte {
	n := len(blobs)
	headerSize := 4 + n*4 // count + RelOffs

	// Compute blob positions (4-byte aligned)
	pos := headerSize
	positions := make([]int, n)
	for i, blob := range blobs {
		pos = (pos + 3) &^ 3
		positions[i] = pos
		pos += len(blob)
	}
	total := (pos + 3) &^ 3

	buf := make([]byte, total)
	binary.LittleEndian.PutUint32(buf, uint32(n))

	for i, blob := range blobs {
		copy(buf[positions[i]:], blob)

		// RelOff points to the blob's object (after vtable).
		// The object starts after the vtable bytes + padding.
		// For MutationRef: vtable=10 bytes, padded to 12, object at blob+12
		// We need to find the object position within the blob.
		// The soffset is at the start of the object, pointing back to vtable.
		// Object position = vtable_size padded to 4
		vtSize := int(binary.LittleEndian.Uint16(blob[0:]))
		objPosInBlob := (vtSize + 3) &^ 3

		reloffPos := 4 + i*4
		targetInBuf := positions[i] + objPosInBlob
		binary.LittleEndian.PutUint32(buf[reloffPos:], uint32(targetInBuf-reloffPos))
	}

	return buf
}
