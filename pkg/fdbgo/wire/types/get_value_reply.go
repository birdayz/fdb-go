package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// GetValueReply — fdbclient/StorageServerInterface.h
// serialize: serializer(ar, penalty, error, value, cached)
//
//	slot 0: penalty (float64)
//	slot 1: error type (Optional<Error>, uint8)
//	slot 2: error value (RelOff)
//	slot 3: value type (Optional<Value>, uint8)
//	slot 4: value data (RelOff)
//	slot 5: cached (bool)
type GetValueReply struct {
	Penalty  float64
	Value    []byte
	HasValue bool
	Cached   bool
}

// UnmarshalFrom reads GetValueReply fields from a Reader already positioned
// at the message object (after ErrorOr unwrapping).
func (m *GetValueReply) UnmarshalFrom(r *wire.Reader) {
	if r.FieldPresent(0) {
		m.Penalty = r.ReadFloat64(0)
	}
	// Optional<Value>: type tag at slot 3, value at slot 4
	if r.FieldPresent(3) && r.ReadUint8(3) > 0 {
		m.Value = r.ReadBytes(4)
		m.HasValue = true
	}
	if r.FieldPresent(5) {
		m.Cached = r.ReadBool(5)
	}
}
