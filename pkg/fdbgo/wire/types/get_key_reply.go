package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// GetKeyReply — fdbclient/StorageServerInterface.h
// serialize: serializer(ar, penalty, error, sel, cached)
//
//	slot 0: penalty (float64)
//	slot 1: error (union_like, Optional<Error>)
//	slot 3: sel (serialize_member, KeySelectorRef — contains the resolved key)
//	slot 4: cached (bool)
type GetKeyReply struct {
	Key []byte
}

// UnmarshalFrom reads the resolved key from the GetKeyReply.
// The key is inside a KeySelectorRef at slot 3.
func (m *GetKeyReply) UnmarshalFrom(r *wire.Reader) error {
	if r.FieldPresent(GetKeyReplySlotSel) {
		selR, err := r.ReadNestedReader(GetKeyReplySlotSel)
		if err != nil {
			return err
		}
		// KeySelectorRef slot 0 = key
		if selR.FieldPresent(KeySelectorRefSlotKey) {
			m.Key = selR.ReadBytes(KeySelectorRefSlotKey)
		}
	}
	return nil
}
