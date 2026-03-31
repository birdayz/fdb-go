package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

type GetKeyValuesReply struct {
	Penalty float64
	Data    []byte
	Version int64
	More    bool
	Cached  bool
}

func (m *GetKeyValuesReply) UnmarshalFDB(data []byte) error {
	r, err := wire.NewReader(data)
	if err != nil {
		return err
	}
	if r.FieldPresent(0) {
		m.Penalty = r.ReadFloat64(0)
	}
	if r.FieldPresent(3) {
		m.Data = r.ReadBytes(3)
	}
	if r.FieldPresent(4) {
		m.Version = r.ReadInt64(4)
	}
	if r.FieldPresent(5) {
		m.More = r.ReadBool(5)
	}
	if r.FieldPresent(6) {
		m.Cached = r.ReadBool(6)
	}
	return nil
}
