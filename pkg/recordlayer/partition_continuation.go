package recordlayer

import (
	"bytes"
	"crypto/sha256"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/internal/tuplefast"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
)

const (
	partitionContinuationVersion int64 = 1
	partitionDirectoryKind       int64 = 1
	partitionCheckerKind         int64 = 2
	partitionTokenMaximum              = 256 << 10
	partitionPositionStart       int64 = 0
	partitionPositionKey         int64 = 1
	partitionPositionEnd         int64 = 2
)

// PartitionContinuationInvalidatedError distinguishes a well-formed stale
// continuation from malformed bytes. Dirty topology tokens never become portable.
type PartitionContinuationInvalidatedError struct{ Reason string }

func (e *PartitionContinuationInvalidatedError) Error() string {
	return "partition continuation invalidated: " + e.Reason
}

type partitionPosition struct {
	state int64
	key   []byte
}

type partitionCheckpoint struct {
	header    []byte
	epoch     int64
	nonce     []byte
	revision  int64
	bound     bool
	phase     int64
	positions [6]partitionPosition // active, record landmark, legacy landmark, protocol, body, legacy audit
}

type partitionContinuation struct{ raw []byte }

func (c *partitionContinuation) IsEnd() bool { return false }
func (c *partitionContinuation) ToBytes() ([]byte, error) {
	if len(c.raw) > partitionTokenMaximum {
		return nil, &RecordCoreArgumentError{Message: "partition continuation exceeds maximum size"}
	}
	return bytes.Clone(c.raw), nil
}

func partitionBytes(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

func partitionLayoutIdentity(store *FDBRecordStore, rt *RecordType) ([]byte, error) {
	marshal := proto.MarshalOptions{Deterministic: true}
	key, err := marshal.Marshal(rt.PrimaryKey.ToKeyExpression())
	if err != nil {
		return nil, err
	}
	descriptor, err := marshal.Marshal(protodesc.ToFileDescriptorProto(rt.Descriptor.ParentFile()))
	if err != nil {
		return nil, err
	}
	// Identity validation must not trigger the store's lazy state loader before
	// an untrusted token is rejected or a read budget is admitted.
	store.stateMu.RLock()
	if store.storeHeader == nil {
		store.stateMu.RUnlock()
		return nil, &PartitionMetadataError{RecordType: rt.Name, Message: "partition reads require an opened store with loaded StoreInfo"}
	}
	format, omit := store.getFormatVersionLocked(), store.omitUnsplitRecordSuffix()
	store.stateMu.RUnlock()
	encoded := (tuple.Tuple{key, descriptor, int64(format), store.metaData.IsSplitLongRecords(), store.metaData.IsStoreRecordVersions(), omit}).Pack()
	hash := sha256.Sum256(encoded)
	return hash[:], nil
}

func (c *partitionReadCursor) tokenIdentity() tuple.Tuple {
	r := c.coordinateRange
	lower, upper := PartitionCoordinateBound{}, PartitionCoordinateBound{}
	if r.Lower != nil {
		lower = *r.Lower
	}
	if r.Upper != nil {
		upper = *r.Upper
	}
	return tuple.Tuple{
		"fdb_go.partition_cursor", partitionContinuationVersion, c.kind,
		partitionBytes(c.store.subspace.Bytes()), (tuple.Tuple{c.rt.GetRecordTypeKey()}).Pack(), c.rt.Name,
		int64(c.store.metaData.Version()), c.layoutIdentity, r.Empty,
		r.Lower != nil, lower.Value, lower.Inclusive, r.Upper != nil, upper.Value, upper.Inclusive, c.reverse,
	}
}

func (c *partitionReadCursor) continuation() RecordCursorContinuation {
	parts := c.tokenIdentity()
	cp := c.checkpoint
	parts = append(parts, partitionBytes(cp.header), cp.epoch, partitionBytes(cp.nonce), cp.revision, cp.bound, cp.phase)
	for _, p := range cp.positions {
		parts = append(parts, p.state, partitionBytes(p.key))
	}
	return &partitionContinuation{raw: parts.Pack()}
}

// The envelope is flat and accepts only these primitive tuple types. Reject
// nesting before tuple.Unpack, whose ordinary tuple decoder is recursive.
func partitionTokenTuple(raw []byte) (tuple.Tuple, error) {
	if len(raw) == 0 || len(raw) > partitionTokenMaximum {
		return nil, fmt.Errorf("invalid partition token length %d", len(raw))
	}
	for offset := 0; offset < len(raw); {
		tag := raw[offset]
		if tag != 0x01 && tag != 0x02 && tag != 0x26 && tag != 0x27 && !(tag >= 0x0c && tag <= 0x1c) {
			return nil, fmt.Errorf("invalid partition token primitive %x", tag)
		}
		n := tuplefast.Skip(raw[offset:])
		if n <= 0 || n > len(raw)-offset {
			return nil, fmt.Errorf("truncated partition token")
		}
		offset += n
	}
	parts, err := tuple.Unpack(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid partition token: %w", err)
	}
	if !bytes.Equal(raw, parts.Pack()) {
		return nil, fmt.Errorf("noncanonical partition token")
	}
	return parts, nil
}

func (c *partitionReadCursor) parseContinuation(raw []byte) error {
	bad := func(cause error) error {
		return &ContinuationParseError{RawBytes: bytes.Clone(raw), Cause: cause}
	}
	parts, err := partitionTokenTuple(raw)
	if err != nil {
		return bad(err)
	}
	if len(parts) != 34 {
		return bad(fmt.Errorf("partition token arity %d, want 34", len(parts)))
	}
	if !bytes.Equal(parts[:16].Pack(), c.tokenIdentity().Pack()) {
		return bad(fmt.Errorf("partition token version/kind/store/table/metadata/layout/range/direction mismatch"))
	}
	header, headerOK := parts[16].([]byte)
	epoch, epochOK := parts[17].(int64)
	nonce, nonceOK := parts[18].([]byte)
	revision, revisionOK := parts[19].(int64)
	bound, boundOK := parts[20].(bool)
	phase, phaseOK := parts[21].(int64)
	if !headerOK || !epochOK || !nonceOK || len(nonce) != 16 || !revisionOK || revision < 0 || !boundOK || bound != (revision > 0) || !phaseOK || phase < 0 || phase > 4 || (c.kind == partitionDirectoryKind && phase != 0) {
		return bad(fmt.Errorf("invalid partition token identity or phase"))
	}
	if (len(header) == 0 && epoch != 0) || (len(header) != 0 && epoch <= 0) {
		return bad(fmt.Errorf("contradictory partition header/epoch"))
	}
	if len(header) != 0 {
		if err := decodePartitionHeader(c.rt, c.layout.spec, header); err != nil {
			return bad(err)
		}
	}
	cp := partitionCheckpoint{header: bytes.Clone(header), epoch: epoch, nonce: bytes.Clone(nonce), revision: revision, bound: bound, phase: phase}
	for i := range cp.positions {
		state, stateOK := parts[22+2*i].(int64)
		key, keyOK := parts[23+2*i].([]byte)
		if !stateOK || !keyOK || state < partitionPositionStart || state > partitionPositionEnd || (state == partitionPositionKey) != (len(key) != 0) || len(key) > 10000 {
			return bad(fmt.Errorf("invalid partition stream %d position", i))
		}
		p := partitionPosition{state: state, key: bytes.Clone(key)}
		if state == partitionPositionKey {
			if err := c.validatePosition(i, key); err != nil {
				return bad(err)
			}
		}
		if c.kind == partitionDirectoryKind && i >= 3 && state != partitionPositionStart {
			return bad(fmt.Errorf("directory token contains checker state"))
		}
		if c.kind == partitionCheckerKind && i >= 3 {
			streamPhase := int64(i - 2)
			if (phase < streamPhase && state != partitionPositionStart) || (phase > streamPhase && state != partitionPositionEnd) {
				return bad(fmt.Errorf("checker phase disagrees with stream %d", i))
			}
		}
		cp.positions[i] = p
	}
	if phase > 0 {
		for i := 0; i < 3; i++ {
			if cp.positions[i].state != partitionPositionEnd {
				return bad(fmt.Errorf("checker advanced before directory exhaustion"))
			}
		}
	}
	if cp.positions[0].state == partitionPositionKey {
		for i := 1; i <= 2; i++ {
			if cp.positions[i].state == partitionPositionKey {
				d, _ := c.positionCoordinate(0, cp.positions[0].key)
				p, _ := c.positionCoordinate(i, cp.positions[i].key)
				if (!c.reverse && p > d) || (c.reverse && p < d) {
					return bad(fmt.Errorf("physical stream consumed ahead of directory"))
				}
			}
		}
	} else if cp.positions[0].state == partitionPositionStart && (cp.positions[1].state == partitionPositionKey || cp.positions[2].state == partitionPositionKey) {
		return bad(fmt.Errorf("physical stream consumed before directory start"))
	}
	c.checkpoint, c.resume = cp, true
	return nil
}
