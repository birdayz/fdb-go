package client

import (
	"fmt"

	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// getMappedRange — the client half of fdb_transaction_get_mapped_range
// (bindings/c/fdb_c.cpp:1063). A mapped range is an ordinary range read over a
// PRIMARY range (typically index entries) where the storage server additionally
// resolves each row through a `mapper` tuple and returns the resolved
// SECONDARY read alongside it, saving the client a round trip per row.
//
// Two facts about 7.3.77 shape everything here, and both were established by
// reading the C++ rather than assumed:
//
//   - The mapper is NOT parsed, validated, or substituted on the client. fdb_c
//     forwards the bytes verbatim (fdb_c.cpp:1088), NativeAPI copies them onto
//     the request (NativeAPI.actor.cpp:4285), and every mapper error —
//     mapper_not_tuple (2043), mapper_bad_index (2030), mapper_no_such_key
//     (2031), mapper_bad_range_decriptor (2032) — is raised by the storage
//     server (fdbserver/storageserver.actor.cpp:4875-4951, :5972). So this
//     client ships the mapper opaquely and its only obligation is to propagate
//     the server's error UNCHANGED. Adding a Go-side mapper parser would be a
//     divergence, not a hardening.
//
//   - Each returned row carries a UNION whose two arms are the two kinds of
//     secondary read the mapper can express: a point lookup
//     (GetValueReqAndResultRef) or a range scan (GetRangeReqAndResultRef, which
//     the `{...}` range descriptor produces). FDBTypes.h:893.
//
// There is deliberately no MATCH_INDEX support: the MATCH_INDEX_* behaviours do
// not exist in 7.3.77. `fdb_transaction_get_mapped_range` has no matchIndex
// parameter (fdb_c.h:587-603) and GetMappedKeyValuesRequest has no matchIndex
// field (StorageServerInterface.h:463-497), so there is no wire slot to carry
// one. It is a later-version feature.

// MappedResultKind names which arm of the per-row union is populated. It mirrors
// the std::variant index in MappedReqAndResultRef, and the underlying values are
// the flatbuffers union tags (1-based, 0 = unset) so a decode is a direct read.
type MappedResultKind uint8

const (
	// MappedResultNone means the row carried no secondary result at all. The
	// storage server does not produce this, but the tag is representable on the
	// wire (tag 0), so it is named rather than silently conflated with a
	// point-lookup miss — those are different facts.
	MappedResultNone MappedResultKind = 0
	// MappedResultGetValue: the mapper resolved to a single key.
	MappedResultGetValue MappedResultKind = 1
	// MappedResultGetRange: the mapper ended in a "{...}" range descriptor and
	// resolved to a key range.
	MappedResultGetRange MappedResultKind = 2
)

func (k MappedResultKind) String() string {
	switch k {
	case MappedResultNone:
		return "none"
	case MappedResultGetValue:
		return "getValue"
	case MappedResultGetRange:
		return "getRange"
	}
	return fmt.Sprintf("MappedResultKind(%d)", uint8(k))
}

// MappedGetValue is the point-lookup arm (C++ GetValueReqAndResultRef,
// FDBTypes.h:855). Present distinguishes "the mapped key holds an empty value"
// from "the mapped key does not exist" — the C++ field is Optional<ValueRef> and
// collapsing the two would silently invent a record.
type MappedGetValue struct {
	Key     []byte
	Value   []byte
	Present bool
}

// MappedGetRange is the range arm (C++ GetRangeReqAndResultRef,
// FDBTypes.h:873). Begin/End are the selectors the server actually resolved, so
// a range that matched nothing still reports what it probed.
type MappedGetRange struct {
	Begin KeySelector
	End   KeySelector
	Rows  []KeyValue
	More  bool
}

// KeySelector mirrors C++ KeySelectorRef for the secondary range bounds.
type KeySelector struct {
	Key     []byte
	OrEqual bool
	Offset  int32
}

// MappedKeyValue is one row of a mapped range: the primary row (Key/Value —
// the index entry that was scanned) plus the secondary read the mapper resolved
// it to. Mirrors C++ MappedKeyValueRef (FDBTypes.h:895), whose KeyValueRef base
// carries the primary row.
type MappedKeyValue struct {
	Key   []byte
	Value []byte

	Kind     MappedResultKind
	GetValue MappedGetValue
	GetRange MappedGetRange
}

// parseGetMappedKeyValuesReply decodes a GetMappedKeyValuesReply exactly as
// parseGetKeyValuesReply decodes its non-mapped twin, including the inline
// LoadBalancedReply.error arm: the storage server delivers wrong_shard_server
// AND every mapper_* error through that field rather than the ErrorOr root, so
// dropping it here would turn a mapper error into an empty successful read.
func parseGetMappedKeyValuesReply(data []byte) ([]MappedKeyValue, bool, float64, error) {
	var r wire.Reader
	if err := wire.ReadErrorOrInto(data, &r); err != nil {
		return nil, false, -1.0, fmt.Errorf("GetMappedKeyValues: %w", err)
	}
	var reply types.GetMappedKeyValuesReply
	reply.UnmarshalFromReader(&r)
	if ferr := wire.ReadInlineReplyError(&r, types.GetMappedKeyValuesReplySlotError); ferr != nil {
		return nil, false, reply.Penalty, ferr
	}

	rows := make([]MappedKeyValue, 0, len(reply.Data))
	for i := range reply.Data {
		rows = append(rows, convertMappedRow(&reply.Data[i]))
	}
	return rows, reply.More, reply.Penalty, nil
}

func convertMappedRow(src *types.MappedKeyValueRef) MappedKeyValue {
	row := MappedKeyValue{
		Key:   src.KeyValue.Key,
		Value: src.KeyValue.Value,
		Kind:  MappedResultKind(src.ReqAndResultTag),
	}
	switch row.Kind {
	case MappedResultGetValue:
		row.GetValue = MappedGetValue{
			Key:     src.ReqAndResultAlt0.Key,
			Value:   src.ReqAndResultAlt0.Result,
			Present: src.ReqAndResultAlt0.HasResult,
		}
	case MappedResultGetRange:
		gr := &src.ReqAndResultAlt1
		row.GetRange = MappedGetRange{
			Begin: KeySelector{Key: gr.Begin.Key, OrEqual: gr.Begin.OrEqual, Offset: gr.Begin.Offset},
			End:   KeySelector{Key: gr.End.Key, OrEqual: gr.End.OrEqual, Offset: gr.End.Offset},
			Rows:  gr.Result.Data,
			More:  gr.Result.More,
		}
	}
	return row
}
