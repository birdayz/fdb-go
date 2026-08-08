package client

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"fdb.dev/pkg/fdbgo/transport"
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

// getMappedKeyValuesBufPool is deliberately separate from getKeyValuesBufPool.
// The two request kinds have different size profiles (a mapper tuple rides
// along on every mapped request) and sharing one pool would let mapped traffic
// reshape the buffers the plain read path — the client's hottest path — recycles.
var getMappedKeyValuesBufPool = sync.Pool{
	New: func() any { b := make([]byte, 0, 512); return &b },
}

func buildGetMappedKeyValuesRequest(mapper []byte) func(begin, end []byte, version int64, limit int32, lockAware bool, tenantId int64, span types.SpanContext, replyToken transport.UID, _ transport.UID) ([]byte, *[]byte) {
	return func(begin, end []byte, version int64, limit int32, lockAware bool, tenantId int64, span types.SpanContext, replyToken transport.UID, _ transport.UID) ([]byte, *[]byte) {
		req := types.GetMappedKeyValuesRequest{
			Begin:                  types.KeySelectorRef{Key: begin, OrEqual: false, Offset: 1}, // firstGreaterOrEqual(begin)
			End:                    types.KeySelectorRef{Key: end, OrEqual: false, Offset: 1},   // firstGreaterOrEqual(end)
			Mapper:                 mapper,
			Version:                version,
			Limit:                  limit,
			LimitBytes:             replyByteLimit,
			Reply:                  types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
			TenantInfo:             types.TenantInfo{TenantId: tenantId},
			SpanContext:            span,
			SsLatestCommitVersions: emptyVersionVector,
		}
		if lockAware {
			req.HasOptions = true
			req.Options = lockAwareReadOptions()
		}
		bufp := getMappedKeyValuesBufPool.Get().(*[]byte)
		result := req.MarshalFDBPooled(*bufp)
		*bufp = result
		return result, bufp
	}
}

// mappedRangeScanOps mirrors plainRangeScanOps. `key` returns the PRIMARY row's
// key: the shard-walk bounds it feeds are bounds in the primary range, and a
// secondary key from the mapped side would not even be in that keyspace.
var mappedRangeScanOps = rangeScanOps[MappedKeyValue]{
	key: func(m *MappedKeyValue) []byte { return m.Key },
	sizeBytes: func(m *MappedKeyValue) int64 {
		// The ceiling bounds what the CLIENT materializes, so it must count the
		// secondary payload too — that is the bulk of a mapped reply, and
		// charging only the primary row would let a mapped scan blow through a
		// ceiling the plain path respects.
		n := int64(len(m.Key) + len(m.Value))
		switch m.Kind {
		case MappedResultGetValue:
			n += int64(len(m.GetValue.Key) + len(m.GetValue.Value))
		case MappedResultGetRange:
			n += int64(len(m.GetRange.Begin.Key) + len(m.GetRange.End.Key))
			for i := range m.GetRange.Rows {
				n += int64(len(m.GetRange.Rows[i].Key) + len(m.GetRange.Rows[i].Value))
			}
		}
		return n
	},
}

func (tx *Transaction) sendGetMappedRange(mapper []byte) func(ctx context.Context, begin, end []byte, limit int, reverse bool, servers []ServerInfo) ([]MappedKeyValue, bool, error) {
	return func(ctx context.Context, begin, end []byte, limit int, reverse bool, servers []ServerInfo) ([]MappedKeyValue, bool, error) {
		return sendRangeRPC(tx, ctx, begin, end, limit, reverse, servers, rangeRPCOps[MappedKeyValue]{
			// getMappedKeyValues is adjusted endpoint 14, NOT 3. The field is
			// DECLARED immediately after getKeyValues (StorageServerInterface.h:103)
			// but is assigned getAdjustedEndpoint(14) (:184-185) — it was appended
			// after getKeyValuesStream to keep the older indices stable. Reading the
			// declaration order instead of the assignment would silently address
			// getShardState.
			endpoint: EndpointGetMappedKeyValues,
			build:    buildGetMappedKeyValuesRequest(mapper),
			putBuf:   func(bufp *[]byte) { getMappedKeyValuesBufPool.Put(bufp) },
			parse:    parseGetMappedKeyValuesReply,
		})
	}
}

func (tx *Transaction) getMappedRangeImpl(ctx context.Context, begin, end []byte, mapper []byte, limit int, reverse bool) ([]MappedKeyValue, bool, error) {
	ops := mappedRangeScanOps
	ops.send = tx.sendGetMappedRange(mapper)
	return rangeScanImpl(tx, ctx, begin, end, limit, reverse, ops)
}

// FDB error codes specific to getMappedRange. Source of truth:
// flow/include/flow/error_definitions.h at the 7.3.77 tag.
const (
	// ErrClientInvalidOperation is raised for a mapped read over the special
	// key space, which getMappedRange does not support.
	ErrClientInvalidOperation = 2000 // client_invalid_operation
	// ErrMapperBadIndex ... ErrMapperNotTuple are raised by the STORAGE SERVER,
	// never by this client — the mapper is not parsed here. They are named so a
	// caller can errors.As them and so the propagate-unchanged contract is
	// testable.
	ErrMapperBadIndex          = 2030 // mapper_bad_index
	ErrMapperNoSuchKey         = 2031 // mapper_no_such_key
	ErrMapperBadRangeDescrptor = 2032 // mapper_bad_range_decriptor (sic: the C++ spelling)
	// ErrGetMappedRangeReadsYourWrites is raised by THIS client when the mapped
	// read — primary range or any resolved secondary read — overlaps a key the
	// transaction has already written.
	ErrGetMappedRangeReadsYourWrites = 2039 // get_mapped_range_reads_your_writes
	ErrMapperNotTuple                = 2043 // mapper_not_tuple
	// ErrUnsupportedOperation is what snapshot=true and read-your-writes-disabled
	// both raise. Neither is a MODE for getMappedRange; both are errors.
	ErrUnsupportedOperation = 2108 // unsupported_operation
)

// GetMappedRange reads the primary range [begin, end) and, for each row, the
// secondary read the `mapper` tuple resolves it to — C++
// ReadYourWritesTransaction::getMappedRange (ReadYourWrites.actor.cpp:1779).
//
// The guard sequence below is C++'s, in C++'s order, and the order is load-bearing:
// the special-key, legal-range and limit checks all run BEFORE the snapshot /
// RYW-disabled guard, so a mapped read that is invalid for two reasons reports
// the FIRST one C++ would report.
//
// `limit` follows the C API, not the internal GetRangeLimits: fdb_c's
// validate_and_update_parameters maps 0 to ROW_LIMIT_UNLIMITED before RYW ever
// sees it (fdb_c.cpp:990-992), and fdb_transaction_get_mapped_range runs that
// same validator (:1080). So 0 here means UNLIMITED, and GetRangeLimits::isReached()'s
// "limit 0 returns an empty result" branch (NativeAPI.actor.cpp:2856) is
// unreachable from any C-API caller. A limit < -1 is range_limits_invalid,
// matching GetRangeLimits::isValid (FDBTypes.h:753).
//
// Isolation: this ALWAYS issues at Snapshot::True and then re-adds read conflict
// ranges by hand — for the primary range AND for every secondary read the server
// resolved (ReadYourWrites.actor.cpp:1163-1192). That is not an optimization; it
// is what makes a serializable mapped read correct, because the secondary keys
// are not known until the reply comes back and so cannot be conflict-ranged up
// front. Any caller building a remote-fetch cursor on top of this inherits the
// same obligation.
func (tx *Transaction) GetMappedRange(ctx context.Context, begin, end, mapper []byte, limit int, reverse bool) ([]MappedKeyValue, bool, error) {
	// C++ :1786 — the special key space is not mapped-readable. Checked first,
	// before even the read version is taken.
	if isSpecialKey(begin) && isSpecialKey(end) {
		return nil, false, &wire.FDBError{Code: ErrClientInvalidOperation}
	}
	if err := tx.ensureReadVersion(ctx); err != nil {
		return nil, false, tx.trackReadError(err)
	}
	// C++ :1801 — key_outside_legal_range.
	maxKey := tx.maxReadKey()
	if bytes.Compare(begin, maxKey) > 0 || bytes.Compare(end, maxKey) > 0 {
		return nil, false, &wire.FDBError{Code: 2004}
	}
	// C++ :1817 — range_limits_invalid.
	if limit < -1 {
		return nil, false, &wire.FDBError{Code: ErrRangeLimitsInvalid}
	}
	// C++ :1823 — an inverted range is an empty result, not an error.
	if bytes.Compare(begin, end) >= 0 {
		return nil, false, nil
	}
	// C++ :1219-1233 — snapshot isolation and RYW-disabled are both
	// unsupported_operation. These are the two shapes a caller reaches for
	// first, and neither degrades gracefully: they are hard errors.
	if tx.rywDisabled {
		return nil, false, &wire.FDBError{Code: ErrUnsupportedOperation}
	}

	rows, more, err := tx.getMappedRangeImpl(ctx, begin, end, mapper, limit, reverse)
	if err != nil {
		return nil, false, tx.trackReadError(err)
	}

	// C++ addConflictRangeAndMustUnmodified (:1163-1192): the read went out at
	// Snapshot::True, so every conflict range it needs is added here, and each
	// one is first checked against the write map — an overlap means the mapped
	// read observed a key this transaction wrote, which RYW cannot honour for
	// mapped reads and which C++ reports rather than silently serving stale data.
	if err := tx.addMappedRangeConflicts(begin, end, rows); err != nil {
		return nil, false, err
	}
	return rows, more, nil
}

// addMappedRangeConflicts implements C++ addConflictRangeAndMustUnmodified.
// The mustUnmodified check and the conflict insertion are the same walk in C++
// (updateConflictMap<true>), so they are the same walk here: every range is
// tested before ANY conflict is recorded for it.
func (tx *Transaction) addMappedRangeConflicts(begin, end []byte, rows []MappedKeyValue) error {
	// Primary range first, exactly as C++ does.
	//
	// KNOWN DIVERGENCE, in the conservative direction, and it is not free.
	// C++ addConflictRange (ReadYourWrites.actor.cpp:283-318) NARROWS the
	// primary range before recording it: when begin.offset <= 1 and the result
	// is truncated (result.more) it starts at read.end rather than read.begin,
	// and it clamps to the first/last keys actually returned. This uses the
	// caller's full [begin, end).
	//
	// A superset can never MISS a conflict, so serializability is safe. But the
	// same range feeds the mustUnmodified check below, and there a superset is
	// user-visible: a write that falls inside [begin, end) but outside the
	// narrowed range makes this raise get_mapped_range_reads_your_writes where
	// C++ would have completed the read. Porting the narrowing needs the
	// readToBegin / readThroughEnd flags, which rangeScanImpl does not yet
	// surface. Until it does, this errs toward refusing rather than toward
	// silently under-conflicting.
	ranges := [][2][]byte{{begin, end}}
	for i := range rows {
		switch rows[i].Kind {
		case MappedResultGetValue:
			k := rows[i].GetValue.Key
			// A single key is the half-open range [k, k\x00) — C++
			// singleKeyRange. The point lookup gets a conflict range whether or
			// not the key EXISTED: a miss is a real observation, and a later
			// insert at that key must conflict.
			ranges = append(ranges, [2][]byte{k, append(append([]byte{}, k...), 0)})
		case MappedResultGetRange:
			gr := &rows[i].GetRange
			// The selectors the server actually resolved, not the mapper's
			// intent — C++ passes getRange.begin/getRange.end straight through.
			ranges = append(ranges, [2][]byte{gr.Begin.Key, gr.End.Key})
		}
	}
	for _, r := range ranges {
		if bytes.Compare(r[0], r[1]) >= 0 {
			continue // empty range contributes no conflict and cannot overlap a write
		}
		if tx.ryw.hasModificationsInRange(r[0], r[1]) {
			return &wire.FDBError{Code: ErrGetMappedRangeReadsYourWrites}
		}
	}
	tx.addReadConflicts(ranges)
	return nil
}
