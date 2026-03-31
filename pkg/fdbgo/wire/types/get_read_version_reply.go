package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

type GetReadVersionReply struct {
	ProcessBusyTime           int32
	Version                   int64
	Locked                    bool
	MidShardSize              int64
	RkDefaultThrottled        bool
	RkBatchThrottled          bool
	ProxyTagThrottledDuration float64
}

func (m *GetReadVersionReply) UnmarshalFDB(data []byte) error {
	r, err := wire.NewReader(data)
	if err != nil {
		return err
	}
	if r.FieldPresent(0) {
		m.ProcessBusyTime = r.ReadInt32(0)
	}
	if r.FieldPresent(1) {
		m.Version = r.ReadInt64(1)
	}
	if r.FieldPresent(2) {
		m.Locked = r.ReadBool(2)
	}
	if r.FieldPresent(5) {
		m.MidShardSize = r.ReadInt64(5)
	}
	if r.FieldPresent(6) {
		m.RkDefaultThrottled = r.ReadBool(6)
	}
	if r.FieldPresent(7) {
		m.RkBatchThrottled = r.ReadBool(7)
	}
	if r.FieldPresent(10) {
		m.ProxyTagThrottledDuration = r.ReadFloat64(10)
	}
	return nil
}
