package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// SpanContext — flow/Tracing.h
// serialize: serializer(ar, traceID, spanID, m_Flags)
//
//	slot 0: traceID (UID, 16 bytes inline)
//	slot 1: spanID (uint64)
//	slot 2: m_Flags (uint8)
type SpanContext struct {
	TraceID [16]byte
	SpanID  uint64
	Flags   uint8
}

func (m *SpanContext) TypeVTable() wire.VTable { return SpanContextVTable }

func (m *SpanContext) MarshalInto(obj *wire.ObjectWriter) {
	vt := SpanContextVTable
	obj.WriteUID(int(vt[2]), m.TraceID)
	obj.WriteUint64(int(vt[3]), m.SpanID)
	obj.WriteUint8(int(vt[4]), m.Flags)
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
