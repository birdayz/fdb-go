package types

// SpanContext — fdbclient/include/fdbclient/Tracing.h:46
// C++ serialize: serializer(ar, traceID, spanID, m_Flags)
//   slot 0: traceID — UID (scalar_traits, 16 bytes inline at offset 4)
//   slot 1: spanID  — uint64_t (8 bytes at offset 20)
//   slot 2: m_Flags — TraceFlags/uint8_t (1 byte at offset 28)

import (
	"encoding/binary"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

type SpanContext struct {
	TraceID [16]byte // UID — 16 bytes inline
	SpanID  uint64
	Flags   uint8
}

func (m *SpanContext) TypeVTable() wire.VTable { return SpanContextVTable }

func (m *SpanContext) MarshalInto(obj *wire.ObjectWriter) {
	obj.WriteUID(4, m.TraceID)    // slot 0: traceID at offset 4
	obj.WriteUint64(20, m.SpanID) // slot 1: spanID at offset 20
	obj.WriteUint8(28, m.Flags)   // slot 2: m_Flags at offset 28
}

func (m *SpanContext) UnmarshalFrom(r *wire.Reader) error {
	if r.FieldPresent(0) {
		m.TraceID = r.ReadUID(0)
	}
	if r.FieldPresent(1) {
		m.SpanID = r.ReadUint64(1)
	}
	if r.FieldPresent(2) {
		m.Flags = r.ReadUint8(2)
	}
	return nil
}

// Ensure binary import is used (for future use in complex types).
var _ = binary.LittleEndian
