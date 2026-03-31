package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

type CommitID struct {
	Version    int64
	TxnBatchId uint16
}

func (m *CommitID) UnmarshalFDB(data []byte) error {
	r, err := wire.NewReader(data)
	if err != nil {
		return err
	}
	if r.FieldPresent(0) {
		m.Version = r.ReadInt64(0)
	}
	if r.FieldPresent(1) {
		m.TxnBatchId = r.ReadUint16(1)
	}
	return nil
}
