package executor

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"time"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// The streaming-aggregate and in-memory-sort continuations are GO-INTERNAL: the
// aggregate reuses only the AggregateCursorContinuation / PartialAggregationResult
// proto message NAMES (never Java's payload structure — Go carries its own
// count + per-aggregate count/sum/int-sum/all-int/min/max accumulator model that
// Java's AggregateCursor cannot parse, and vice versa), and Java has no in-memory
// sort at all. Because neither payload ever interoperates with Java, both carry a
// lossless typed codec instead of encoding/json, which corrupts row values in two
// ways JSON cannot avoid:
//   - the aggregate group-break key is string(tuple.Pack(...)) — arbitrary bytes;
//     a tuple-packed int64>=128, any negative int, or any float64 is invalid UTF-8,
//     and json.Marshal rewrites invalid UTF-8 to U+FFFD, so the resumed key never
//     matches the recomputed key and one straddling group splits into two rows;
//   - every JSON number decodes as float64 and every []byte as a base64 string, so
//     a float64 / int64>2^53 / uint64 / []byte group key, a MIN/MAX partial, or an
//     ORDER BY sort slot loses its Go type and 64-bit precision.
//
// contVal* are the type tags for one continuation value. The codec encodes
// EXACTLY, with NO lossy fallback, every Go type the value layer can carry as a
// grouping key, a MIN/MAX partial, or a sort ORDER BY slot:
//   - nil (SQL NULL), bool, string;
//   - int64 / float64 (the record-materialized numerics) plus int / int32 /
//     float32 (the remaining widths isNumeric admits, so a defensively-typed
//     partial still round-trips exactly);
//   - []byte (BYTES/BINARY) and the neutral UUID [16]byte / tuple.UUID;
//   - uint64 — a positive index-sourced integer using bit 63 (in (2^63, 2^64))
//     decodes from the FDB tuple as uint64 (tuple.decodeInt) and
//     tupleElementToRowValue passes it through unchanged, so it is a reachable key;
//   - *big.Int / big.Int — an integer beyond the uint64 range a tuple key can
//     carry. Not producible from a proto scalar field today (those cap at uint64),
//     but encoded losslessly (sign + magnitude) so it can never silently round;
//   - time.Time — DATE/TIMESTAMP values flow through the row domain as STRINGS
//     today (so a time.Time key is not currently reachable), but a time.Time is
//     encoded losslessly (MarshalBinary) rather than left to round-trip wrong.
//   - []any — an ARRAY column slot (values.ProtoFieldToRowValue turns a repeated
//     field into []any): uvarint element count + each element recursively, so a
//     nested array (struct arrays' element lists) round-trips too;
//   - proto.Message — a STRUCT column slot (protoScalarToRowValue keeps a nested
//     message raw): one representation flag byte (0 = the encoded value was a
//     generated Go message, 1 = a *dynamicpb.Message) + uvarint-length-prefixed
//     full type name + uvarint-length-prefixed proto.Marshal bytes. Decode is
//     TWO-PHASE: readContValue returns a pendingProtoRowValue placeholder,
//     which the continuation decode path resolves (resolvePendingProtoValues)
//     BY THE ENCODED REPRESENTATION — flag 0 rebuilds via
//     protoregistry.GlobalTypes as the generated type; flag 1 rebuilds via the
//     store's metadata descriptors + dynamicpb. NEVER across representations:
//     fresh rows materialize as the schema's representation (a dynamic record
//     schema importing a compiled message type still materializes
//     *dynamicpb.Message rows even though the name IS registered), and the
//     %T-keyed group/dedup paths would split equal rows straddling a
//     checkpoint if the restored concrete type differed from a fresh row's.
//     An unresolvable representation errors LOUDLY — a placeholder never
//     leaks into the row domain.
//
// List decode and placeholder resolution recurse per nesting level; both are
// capped at maxContNestingDepth — a continuation token is EXTERNAL wire input,
// and a crafted token of deeply nested one-element lists would otherwise cost
// one stack frame per two payload bytes and overflow the stack (an
// unrecoverable runtime crash, not an error).
//
// A value whose Go type is NONE of the above — a map, or any other non-scalar
// the codec cannot faithfully reconstruct — is a correct-or-loud ERROR on
// encode, NOT a silent JSON blob: a straddling group / sort buffer with such a
// key must FAIL the resume loudly, never resume with a mistyped value.
const (
	contValNil      byte = 0
	contValInt64    byte = 1
	contValFloat64  byte = 2
	contValString   byte = 3
	contValBytes    byte = 4
	contValBool     byte = 5
	contValUUID     byte = 6 // neutral [16]byte / tuple.UUID
	contValInt32    byte = 7
	contValFloat32  byte = 8
	contValInt      byte = 9  // platform int, restored as int
	contValUint64   byte = 10 // index-sourced integer in (2^63, 2^64)
	contValBigInt   byte = 11 // *big.Int / big.Int, sign byte + big-endian magnitude
	contValTime     byte = 12 // time.Time via MarshalBinary
	contValList     byte = 13 // []any: uvarint count + each element recursively
	contValProtoMsg byte = 14 // proto.Message: full type name + proto.Marshal bytes
	// Tag 15 was contValJSON — the deleted lossy JSON fallback — and is
	// PERMANENTLY RETIRED: never reuse it. Decode treats 15 as an unknown tag,
	// so a legacy JSON payload keeps failing loudly instead of decoding wrong.
)

// appendContValue appends v to buf in the typed, lossless continuation encoding.
// A type the codec cannot faithfully reconstruct is a correct-or-loud error, never
// a silent JSON blob (which would resume a straddling group / sort buffer with a
// mistyped value).
func appendContValue(buf []byte, v any) ([]byte, error) {
	switch t := v.(type) {
	case nil:
		return append(buf, contValNil), nil
	case int64:
		return binary.BigEndian.AppendUint64(append(buf, contValInt64), uint64(t)), nil
	case int:
		return binary.BigEndian.AppendUint64(append(buf, contValInt), uint64(int64(t))), nil
	case int32:
		return binary.BigEndian.AppendUint32(append(buf, contValInt32), uint32(t)), nil
	case uint64:
		return binary.BigEndian.AppendUint64(append(buf, contValUint64), t), nil
	case float64:
		return binary.BigEndian.AppendUint64(append(buf, contValFloat64), math.Float64bits(t)), nil
	case float32:
		return binary.BigEndian.AppendUint32(append(buf, contValFloat32), math.Float32bits(t)), nil
	case bool:
		b := byte(0)
		if t {
			b = 1
		}
		return append(buf, contValBool, b), nil
	case string:
		buf = append(buf, contValString)
		buf = binary.AppendUvarint(buf, uint64(len(t)))
		return append(buf, t...), nil
	case []byte:
		buf = append(buf, contValBytes)
		buf = binary.AppendUvarint(buf, uint64(len(t)))
		return append(buf, t...), nil
	case [16]byte:
		return append(append(buf, contValUUID), t[:]...), nil
	case tuple.UUID:
		return append(append(buf, contValUUID), t[:]...), nil
	case *big.Int:
		return appendContBigInt(buf, t), nil
	case big.Int:
		return appendContBigInt(buf, &t), nil
	case time.Time:
		mb, err := t.MarshalBinary()
		if err != nil {
			return nil, fmt.Errorf("continuation: cannot encode time.Time value: %w", err)
		}
		buf = append(buf, contValTime)
		buf = binary.AppendUvarint(buf, uint64(len(mb)))
		return append(buf, mb...), nil
	case []any:
		// An ARRAY column slot. Elements recurse through appendContValue, so
		// nested lists and struct elements work; an unencodable element
		// propagates its loud error.
		buf = append(buf, contValList)
		buf = binary.AppendUvarint(buf, uint64(len(t)))
		for _, e := range t {
			var err error
			buf, err = appendContValue(buf, e)
			if err != nil {
				return nil, err
			}
		}
		return buf, nil
	case proto.Message:
		// A STRUCT column slot (protoScalarToRowValue keeps nested messages
		// raw — generated or dynamicpb). Encoded as full type name + marshal
		// bytes; decode rebuilds it via a metadata descriptor resolver (see
		// pendingProtoRowValue). This interface case must stay BELOW every
		// concrete case: none of them implement proto.Message today, and any
		// future one must keep its more specific encoding.
		return appendContProtoMessage(buf, t)
	default:
		// Correct-or-loud: a non-scalar the codec cannot faithfully reconstruct
		// (a map has no stable typed form) must FAIL the encode, so a
		// straddling group / sort buffer whose key or partial is such a value
		// fails the resume loudly rather than silently resuming with a
		// mistyped value.
		return nil, fmt.Errorf("continuation: cannot encode value of type %T (unsupported continuation value)", v)
	}
}

// appendContProtoMessage encodes a proto.Message row value as
// contValProtoMsg || flag || uvarint(len(fullName)) || fullName ||
// uvarint(len(payload)) || payload. The flag byte records the ORIGINAL Go
// representation — 1 = the value was a *dynamicpb.Message, 0 = a generated
// message — so the decode side rebuilds the SAME concrete type a freshly-
// materialized row carries (the %T-keyed group/dedup paths depend on that
// identity). The full name ALONE cannot pick the representation: a dynamic
// record schema importing a compiled message type materializes dynamicpb rows
// even though the name IS registered in protoregistry.GlobalTypes. The name is
// the decode-side lookup key — flag 0 in protoregistry.GlobalTypes, flag 1 in
// the store's metadata descriptors.
func appendContProtoMessage(buf []byte, msg proto.Message) ([]byte, error) {
	fullName := string(msg.ProtoReflect().Descriptor().FullName())
	payload, err := proto.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("continuation: cannot encode proto message value of type %q: %w", fullName, err)
	}
	flag := byte(0)
	if _, isDynamic := msg.(*dynamicpb.Message); isDynamic {
		flag = 1
	}
	buf = append(buf, contValProtoMsg, flag)
	buf = binary.AppendUvarint(buf, uint64(len(fullName)))
	buf = append(buf, fullName...)
	buf = binary.AppendUvarint(buf, uint64(len(payload)))
	return append(buf, payload...), nil
}

// appendContBigInt encodes an arbitrary-precision integer as
// contValBigInt || sign byte (0 = zero/positive, 1 = negative) ||
// uvarint(len(magnitude)) || big-endian magnitude. big.Int.Bytes() yields the
// big-endian magnitude (empty for zero); the sign byte restores the sign on decode.
func appendContBigInt(buf []byte, i *big.Int) []byte {
	buf = append(buf, contValBigInt)
	sign := byte(0)
	if i.Sign() < 0 {
		sign = 1
	}
	buf = append(buf, sign)
	mag := i.Bytes()
	buf = binary.AppendUvarint(buf, uint64(len(mag)))
	return append(buf, mag...)
}

// maxContNestingDepth caps list (tag 13) nesting for the decode recursion
// (readContValue) AND the placeholder-resolution walk (resolvePendingProtoValues).
// A continuation token is EXTERNAL wire input: without a cap, a crafted token of
// deeply nested one-element lists costs one stack frame per two payload bytes
// and overflows the stack — an unrecoverable runtime crash, not an error. No
// real row nests anywhere near this deep (an ARRAY column is one level, a
// struct array's element lists two).
const maxContNestingDepth = 64

// readContValue decodes one typed continuation value, returning it and the
// unconsumed tail. A truncated or unknown-tag payload is a corrupt continuation
// and errors — never a silent wrong resume. Nesting beyond maxContNestingDepth
// is rejected loudly (see the const).
func readContValue(b []byte) (any, []byte, error) {
	return readContValueDepth(b, 0)
}

// readContValueDepth is readContValue with the nesting budget threaded through:
// depth is the number of enclosing lists (0 at the top level).
func readContValueDepth(b []byte, depth int) (any, []byte, error) {
	if len(b) == 0 {
		return nil, nil, fmt.Errorf("continuation: truncated typed value")
	}
	tag := b[0]
	b = b[1:]
	need := func(n int) error {
		if len(b) < n {
			return fmt.Errorf("continuation: truncated typed value (tag %d)", tag)
		}
		return nil
	}
	switch tag {
	case contValNil:
		return nil, b, nil
	case contValInt64:
		if err := need(8); err != nil {
			return nil, nil, err
		}
		return int64(binary.BigEndian.Uint64(b)), b[8:], nil
	case contValInt:
		if err := need(8); err != nil {
			return nil, nil, err
		}
		return int(int64(binary.BigEndian.Uint64(b))), b[8:], nil
	case contValUint64:
		if err := need(8); err != nil {
			return nil, nil, err
		}
		return binary.BigEndian.Uint64(b), b[8:], nil
	case contValInt32:
		if err := need(4); err != nil {
			return nil, nil, err
		}
		return int32(binary.BigEndian.Uint32(b)), b[4:], nil
	case contValBigInt:
		if err := need(1); err != nil {
			return nil, nil, err
		}
		sign := b[0]
		if sign > 1 {
			return nil, nil, fmt.Errorf("continuation: bad big.Int sign byte %d (tag %d)", sign, tag)
		}
		mag, rest, err := readContBytes(b[1:], tag)
		if err != nil {
			return nil, nil, err
		}
		n := new(big.Int).SetBytes(mag)
		if sign == 1 {
			n.Neg(n)
		}
		return n, rest, nil
	case contValTime:
		run, rest, err := readContBytes(b, tag)
		if err != nil {
			return nil, nil, err
		}
		var tm time.Time
		if uErr := tm.UnmarshalBinary(run); uErr != nil {
			return nil, nil, fmt.Errorf("continuation: bad time.Time value (tag %d): %w", tag, uErr)
		}
		return tm, rest, nil
	case contValFloat64:
		if err := need(8); err != nil {
			return nil, nil, err
		}
		return math.Float64frombits(binary.BigEndian.Uint64(b)), b[8:], nil
	case contValFloat32:
		if err := need(4); err != nil {
			return nil, nil, err
		}
		return math.Float32frombits(binary.BigEndian.Uint32(b)), b[4:], nil
	case contValBool:
		if err := need(1); err != nil {
			return nil, nil, err
		}
		return b[0] != 0, b[1:], nil
	case contValString:
		run, rest, err := readContBytes(b, tag)
		if err != nil {
			return nil, nil, err
		}
		return string(run), rest, nil
	case contValBytes:
		run, rest, err := readContBytes(b, tag)
		if err != nil {
			return nil, nil, err
		}
		// Copy: run aliases the proto buffer; a []byte value must own its bytes.
		out := make([]byte, len(run))
		copy(out, run)
		return out, rest, nil
	case contValUUID:
		if err := need(16); err != nil {
			return nil, nil, err
		}
		var u [16]byte
		copy(u[:], b[:16])
		return u, b[16:], nil
	case contValList:
		// The nesting budget: reject a list nested deeper than the cap BEFORE
		// recursing into its elements (a crafted token of one-element lists
		// would otherwise recurse once per two payload bytes — stack overflow).
		if depth >= maxContNestingDepth {
			return nil, nil, fmt.Errorf("continuation: list nesting exceeds %d levels (tag %d) — corrupt or crafted token", maxContNestingDepth, tag)
		}
		cnt, m := binary.Uvarint(b)
		if m <= 0 {
			return nil, nil, fmt.Errorf("continuation: bad list-count prefix (tag %d)", tag)
		}
		b = b[m:]
		// Each element consumes at least one tag byte, so a count exceeding the
		// remaining bytes is corrupt — reject before allocating.
		if cnt > uint64(len(b)) {
			return nil, nil, fmt.Errorf("continuation: list count %d exceeds remaining bytes (tag %d)", cnt, tag)
		}
		out := make([]any, 0, cnt)
		for i := uint64(0); i < cnt; i++ {
			var v any
			var err error
			v, b, err = readContValueDepth(b, depth+1)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, v)
		}
		return out, b, nil
	case contValProtoMsg:
		// The representation flag: 0 = generated, 1 = *dynamicpb.Message (see
		// appendContProtoMessage). Any other value is corrupt/crafted.
		if err := need(1); err != nil {
			return nil, nil, err
		}
		flag := b[0]
		if flag > 1 {
			return nil, nil, fmt.Errorf("continuation: bad proto-message representation flag %d (tag %d)", flag, tag)
		}
		name, rest, err := readContBytes(b[1:], tag)
		if err != nil {
			return nil, nil, err
		}
		payload, rest, err := readContBytes(rest, tag)
		if err != nil {
			return nil, nil, err
		}
		// Copy: payload aliases the proto buffer; the placeholder must own its
		// bytes (string(name) already copies).
		own := make([]byte, len(payload))
		copy(own, payload)
		// No descriptor here — return the placeholder; the caller MUST resolve
		// it (resolvePendingProtoValues) or error loudly. It must never leak
		// into the row domain.
		return pendingProtoRowValue{TypeName: string(name), Bytes: own, Dynamic: flag == 1}, rest, nil
	default:
		return nil, nil, fmt.Errorf("continuation: unknown typed-value tag %d", tag)
	}
}

// pendingProtoRowValue is the DESCRIPTOR-LESS decode of a contValProtoMsg value
// (a STRUCT column slot in a buffered sort row). readContValue deliberately does
// NOT rebuild the message itself: rebuilding is a POLICY decision that differs
// per decode path — the sort path rebuilds the encoded representation
// (resolvePendingProtoValues), while the aggregate paths REJECT any
// placeholder outright (rejectPendingProtoValues — their state is scalar-only,
// and a resolvable crafted token must not be laundered into a resumed group).
// Every decode path must do one or the other; a placeholder leaking into the
// row domain would evaluate as a wrong, mistyped value.
type pendingProtoRowValue struct {
	TypeName string // the message's full proto name (the representation-specific lookup key)
	Bytes    []byte // proto.Marshal payload
	Dynamic  bool   // the ORIGINAL representation: true = *dynamicpb.Message, false = generated
}

// protoDescriptorResolver resolves a proto message full name to its message
// descriptor. A name the resolver cannot find MUST return an error (never
// (nil, nil)) — an unresolvable buffered struct value fails the resume loudly.
type protoDescriptorResolver func(fullName string) (protoreflect.MessageDescriptor, error)

// resolvePendingProtoValues walks a decoded row value RECURSIVELY (lists may
// contain messages — struct arrays) and replaces every pendingProtoRowValue
// with the real message, rebuilt as its ENCODED representation:
//   - flag 0 (generated): protoregistry.GlobalTypes — the value was a generated
//     Go message (e.g. *gen.Flower) when encoded, so it rebuilds as that
//     generated type. A name the registry does not carry is a LOUD error.
//   - flag 1 (dynamic): the resolver's metadata descriptor +
//     dynamicpb.NewMessage — the value was a *dynamicpb.Message when encoded (a
//     dynamic, metadata-built schema — INCLUDING one that imports a compiled
//     message type, whose rows are still dynamicpb even though the name is
//     registered). A nil resolver or an unresolvable name is a LOUD error.
//
// NEVER across representations: the group/dedup keys (computeGroupKey,
// packedDedupKey) fall back to %T:%v for composite
// slots, so a restored slot whose concrete Go type differs from a freshly-
// materialized row's would never key-match it — equal rows straddling a
// checkpoint would split into duplicate groups / duplicate DISTINCT rows
// (wrong results, no error). Falling back registry→resolver (or the reverse)
// recreates exactly that split from the other side. A payload that does not
// unmarshal is a LOUD error too — never a silent nil slot. The list walk
// shares readContValue's nesting budget (defense in depth: the cap must not
// depend on the caller having decoded through readContValue).
func resolvePendingProtoValues(v any, resolve protoDescriptorResolver) (any, error) {
	return resolvePendingProtoValuesDepth(v, resolve, 0)
}

// resolvePendingProtoValuesDepth is resolvePendingProtoValues with the nesting
// budget threaded through: depth is the number of enclosing lists.
func resolvePendingProtoValuesDepth(v any, resolve protoDescriptorResolver, depth int) (any, error) {
	switch t := v.(type) {
	case pendingProtoRowValue:
		if !t.Dynamic {
			// Flag 0: the encoder saw a generated message, so ONLY the registry
			// may rebuild it. A metadata + dynamicpb rebuild would change the
			// concrete Go type and split %T-keyed group/dedup identity.
			mt := recordlayer.FindRegisteredMessageType(protoreflect.FullName(t.TypeName))
			if mt == nil {
				return nil, fmt.Errorf("continuation: proto message type %q was encoded from a generated message but is not registered in this binary — cannot rebuild it", t.TypeName)
			}
			msg := mt.New().Interface()
			if uErr := proto.Unmarshal(t.Bytes, msg); uErr != nil {
				return nil, fmt.Errorf("continuation: cannot unmarshal buffered proto message of type %q: %w", t.TypeName, uErr)
			}
			return msg, nil
		}
		// Flag 1: the encoder saw a *dynamicpb.Message, so ONLY the metadata
		// resolver may rebuild it — even when the name IS registered (a dynamic
		// schema importing a compiled type still materializes dynamicpb rows).
		if resolve == nil {
			return nil, fmt.Errorf("continuation: value carries a dynamic proto message of type %q but no descriptor resolver is available — cannot rebuild it", t.TypeName)
		}
		desc, err := resolve(t.TypeName)
		if err != nil {
			return nil, fmt.Errorf("continuation: cannot resolve descriptor for proto message type %q: %w", t.TypeName, err)
		}
		if desc == nil {
			// Defensive: a resolver must error, not return nil — but a nil
			// descriptor here would panic dynamicpb, so reject it loudly.
			return nil, fmt.Errorf("continuation: descriptor resolver returned no descriptor for proto message type %q", t.TypeName)
		}
		msg := dynamicpb.NewMessage(desc)
		if uErr := proto.Unmarshal(t.Bytes, msg); uErr != nil {
			return nil, fmt.Errorf("continuation: cannot unmarshal buffered proto message of type %q: %w", t.TypeName, uErr)
		}
		return msg, nil
	case []any:
		if depth >= maxContNestingDepth {
			return nil, fmt.Errorf("continuation: list nesting exceeds %d levels while resolving proto placeholders — corrupt or crafted token", maxContNestingDepth)
		}
		for i, e := range t {
			nv, err := resolvePendingProtoValuesDepth(e, resolve, depth+1)
			if err != nil {
				return nil, err
			}
			t[i] = nv
		}
		return t, nil
	default:
		return v, nil
	}
}

// rejectPendingProtoValues walks a decoded value (lists recursively) and errors
// on any pendingProtoRowValue. The aggregate decode paths use it instead of
// resolvePendingProtoValues: group keys and MIN/MAX partials are never proto
// messages (the aggregate encoder never writes a contValProtoMsg there), so a
// tag-14 placeholder is crafted/corrupt input and must be rejected even when
// its type name IS resolvable — the representation-tagged resolution above
// would happily rebuild a well-formed token, and that must not launder a
// crafted token into a resumed group. Recursion depth is bounded by readContValue's
// decode budget (this helper only ever sees values it decoded).
func rejectPendingProtoValues(v any) error {
	switch t := v.(type) {
	case pendingProtoRowValue:
		return fmt.Errorf("continuation: unexpected proto message value of type %q (this state only carries scalar values)", t.TypeName)
	case []any:
		for _, e := range t {
			if err := rejectPendingProtoValues(e); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateAggExtremum gates a decoded MIN/MAX partial to exactly what the
// accumulator can produce: accumulateRow's isNumeric gate + aggMinMax only ever
// yield int64/int32/int/float64/float32 — or nil for "no non-NULL rows yet".
// Any other decoded shape (a tag-13 []any, a string, a proto placeholder) is a
// crafted or corrupt continuation and must error loudly, never seed the fold
// with a value aggMinMax cannot compare.
func validateAggExtremum(v any) error {
	switch v.(type) {
	case nil, int64, int32, int, float64, float32:
		return nil
	default:
		return fmt.Errorf("continuation: MIN/MAX partial has type %T — the accumulator only produces int64/int32/int/float64/float32 or nil (corrupt or crafted continuation)", v)
	}
}

// metadataMessageResolver builds a protoDescriptorResolver over the store's
// RecordMetaData: a dynamic schema's message descriptors are dynamicpb-built
// from store metadata (usually not in protoregistry.GlobalTypes — and a
// flag-1 slot must rebuild from metadata even when its name IS registered),
// so a buffered dynamic STRUCT value's descriptor is found in the metadata's
// record-type file descriptors — top-level and nested messages, plus
// transitive imports (a struct column's message type may live in an imported
// file). The full-name →
// descriptor index is built ONCE, lazily on the first lookup (a resume without
// unregistered struct slots never pays the schema walk), then reused — the
// pre-index resolver re-walked every record type's file tree PER placeholder,
// O(rows × schema) on a struct-heavy buffer. The returned closure memoizes
// without a lock: it serves a single sequential decode pass and is NOT safe
// for concurrent use.
func metadataMessageResolver(md *recordlayer.RecordMetaData) protoDescriptorResolver {
	var index map[protoreflect.FullName]protoreflect.MessageDescriptor
	return func(fullName string) (protoreflect.MessageDescriptor, error) {
		if md == nil {
			return nil, fmt.Errorf("no record metadata available to resolve message descriptors")
		}
		if index == nil {
			index = buildMetadataMessageIndex(md)
		}
		if found, ok := index[protoreflect.FullName(fullName)]; ok {
			return found, nil
		}
		return nil, fmt.Errorf("message type %q not found in the store's record metadata descriptors", fullName)
	}
}

// buildMetadataMessageIndex walks the metadata's record-type file descriptors
// — top-level and nested messages, plus transitive imports — into a full-name
// → descriptor map. First registration wins (files are visited in record-type
// order, matching the pre-index resolver's first-match file scan).
func buildMetadataMessageIndex(md *recordlayer.RecordMetaData) map[protoreflect.FullName]protoreflect.MessageDescriptor {
	index := make(map[protoreflect.FullName]protoreflect.MessageDescriptor)
	seen := make(map[string]bool)
	var addMsgs func(msgs protoreflect.MessageDescriptors)
	addMsgs = func(msgs protoreflect.MessageDescriptors) {
		for i := 0; i < msgs.Len(); i++ {
			m := msgs.Get(i)
			if _, dup := index[m.FullName()]; !dup {
				index[m.FullName()] = m
			}
			addMsgs(m.Messages())
		}
	}
	var addFile func(fd protoreflect.FileDescriptor)
	addFile = func(fd protoreflect.FileDescriptor) {
		if fd == nil || seen[fd.Path()] {
			return
		}
		seen[fd.Path()] = true
		addMsgs(fd.Messages())
		imports := fd.Imports()
		for i := 0; i < imports.Len(); i++ {
			addFile(imports.Get(i).FileDescriptor)
		}
	}
	for _, rt := range md.RecordTypes() {
		if rt.Descriptor != nil {
			addFile(rt.Descriptor.ParentFile())
		}
	}
	if ud := md.GetUnionDescriptor(); ud != nil {
		addFile(ud.ParentFile())
	}
	return index
}

// readContBytes reads a uvarint-length-prefixed byte run, returning a subslice
// (the caller copies when it must own the bytes) and the tail.
func readContBytes(b []byte, tag byte) (run, rest []byte, err error) {
	n, m := binary.Uvarint(b)
	if m <= 0 {
		return nil, nil, fmt.Errorf("continuation: bad length prefix (tag %d)", tag)
	}
	b = b[m:]
	if uint64(len(b)) < n {
		return nil, nil, fmt.Errorf("continuation: truncated byte run (tag %d)", tag)
	}
	return b[:n], b[n:], nil
}

// appendContSlice appends a uvarint count prefix followed by each value in the
// typed continuation encoding. Shared by the aggregate group-key keyVals and the
// sort buffer's ORDER BY slots. An unencodable element propagates the
// appendContValue error (correct-or-loud).
func appendContSlice(buf []byte, vals []any) ([]byte, error) {
	buf = binary.AppendUvarint(buf, uint64(len(vals)))
	for _, v := range vals {
		var err error
		buf, err = appendContValue(buf, v)
		if err != nil {
			return nil, err
		}
	}
	return buf, nil
}

// readContSlice reads a count-prefixed run of typed continuation values written
// by appendContSlice, returning the values and the unconsumed tail.
func readContSlice(b []byte) ([]any, []byte, error) {
	cnt, m := binary.Uvarint(b)
	if m <= 0 {
		return nil, nil, fmt.Errorf("continuation: bad value-count prefix")
	}
	b = b[m:]
	// Each value consumes at least one tag byte, so a count exceeding the
	// remaining bytes is corrupt — reject before allocating.
	if cnt > uint64(len(b)) {
		return nil, nil, fmt.Errorf("continuation: value count %d exceeds remaining bytes", cnt)
	}
	out := make([]any, 0, cnt)
	for i := uint64(0); i < cnt; i++ {
		var v any
		var err error
		v, b, err = readContValue(b)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, v)
	}
	return out, b, nil
}

// encodeAggGroupKey packs the in-progress group's identity into the
// PartialAggregationResult.group_key bytes: the packed group-key bytes VERBATIM
// (raw, not a JSON string — arbitrary tuple bytes must survive to match the
// recomputed key on resume) followed by the typed keyVals (carried unchanged into
// finalizeGroup's output row, so their Go type and precision are preserved). An
// unencodable keyVal errors (correct-or-loud) rather than silently lose type.
func encodeAggGroupKey(groupKey string, keyVals []any) ([]byte, error) {
	buf := binary.AppendUvarint(nil, uint64(len(groupKey)))
	buf = append(buf, groupKey...)
	return appendContSlice(buf, keyVals)
}

// decodeAggGroupKey is the inverse of encodeAggGroupKey.
func decodeAggGroupKey(b []byte) (groupKey string, keyVals []any, err error) {
	n, m := binary.Uvarint(b)
	if m <= 0 {
		return "", nil, fmt.Errorf("continuation: bad group-key length prefix")
	}
	b = b[m:]
	if uint64(len(b)) < n {
		return "", nil, fmt.Errorf("continuation: truncated group key")
	}
	groupKey = string(b[:n])
	b = b[n:]
	keyVals, _, err = readContSlice(b)
	if err != nil {
		return "", nil, err
	}
	return groupKey, keyVals, nil
}

// encodeAggregateContinuation serializes the streaming aggregate
// cursor's partial state using Java's AggregateCursorContinuation proto.
// Carries the inner cursor position + the single in-progress group's
// partial accumulator state.
func encodeAggregateContinuation(
	innerCont recordlayer.RecordCursorContinuation,
	groupKey string,
	keyVals []any,
	gs *groupState,
	aggregates []expressions.AggregateSpec,
) ([]byte, error) {
	var innerBytes []byte
	if innerCont != nil {
		var err error
		innerBytes, err = innerCont.ToBytes()
		if err != nil {
			return nil, err
		}
	}

	msg := &gen.AggregateCursorContinuation{
		Continuation: innerBytes,
	}

	if gs != nil {
		var states []*gen.AccumulatorState
		as := &gen.AccumulatorState{}

		// Pack: count, then per-aggregate (count_i, sum_i, sumsI_i, allInt_i, min_i, max_i)
		as.State = append(as.State, &gen.OneOfTypedState{
			State: &gen.OneOfTypedState_Int64State{Int64State: gs.count},
		})
		for i := range aggregates {
			as.State = append(as.State, &gen.OneOfTypedState{
				State: &gen.OneOfTypedState_Int64State{Int64State: gs.counts[i]},
			})
			as.State = append(as.State, &gen.OneOfTypedState{
				State: &gen.OneOfTypedState_DoubleState{DoubleState: gs.sums[i]},
			})
			as.State = append(as.State, &gen.OneOfTypedState{
				State: &gen.OneOfTypedState_Int64State{Int64State: gs.sumsI[i]},
			})
			allIntVal := int64(0)
			if gs.allInt[i] {
				allIntVal = 1
			}
			as.State = append(as.State, &gen.OneOfTypedState{
				State: &gen.OneOfTypedState_Int64State{Int64State: allIntVal},
			})
			// min_i / max_i: one typed value each (nil → a lone nil tag). The
			// typed codec preserves int64/float64 exactly — JSON collapsed both to
			// float64 and then re-narrowed integral doubles to int64, flipping a
			// DOUBLE MIN/MAX's type on resume. An unencodable partial errors
			// (correct-or-loud) rather than silently lose its type on resume.
			minBytes, err := appendContValue(nil, gs.mins[i])
			if err != nil {
				return nil, fmt.Errorf("failed to encode MIN state for aggregate continuation: %w", err)
			}
			maxBytes, err := appendContValue(nil, gs.maxs[i])
			if err != nil {
				return nil, fmt.Errorf("failed to encode MAX state for aggregate continuation: %w", err)
			}
			as.State = append(as.State, &gen.OneOfTypedState{
				State: &gen.OneOfTypedState_BytesState{BytesState: minBytes},
			})
			as.State = append(as.State, &gen.OneOfTypedState{
				State: &gen.OneOfTypedState_BytesState{BytesState: maxBytes},
			})
		}
		states = append(states, as)

		gkBytes, err := encodeAggGroupKey(groupKey, keyVals)
		if err != nil {
			return nil, fmt.Errorf("failed to encode group key for aggregate continuation: %w", err)
		}
		msg.PartialAggregationResults = &gen.PartialAggregationResult{
			GroupKey:          gkBytes,
			AccumulatorStates: states,
		}
	}

	return proto.Marshal(msg)
}

// decodeAggregateContinuation deserializes the AggregateCursorContinuation
// proto. Returns the inner continuation and the partial group state.
func decodeAggregateContinuation(data []byte, numAggs int) (
	innerContinuation []byte,
	groupKey string,
	gs *groupState,
	err error,
) {
	msg := &gen.AggregateCursorContinuation{}
	if err := proto.Unmarshal(data, msg); err != nil {
		return nil, "", nil, fmt.Errorf("failed to unmarshal aggregate continuation: %w", err)
	}

	innerContinuation = msg.Continuation

	if msg.PartialAggregationResults == nil {
		return innerContinuation, "", nil, nil
	}

	par := msg.PartialAggregationResults

	// Decode groupKey + keyVals from the typed payload. Unparseable bytes are a
	// corrupt continuation and must error: silently coercing them to a raw group
	// key would resume aggregation under a key that never matches the recomputed
	// keys (wrong results, no error).
	var keyVals []any
	if par.GroupKey != nil {
		var gkErr error
		groupKey, keyVals, gkErr = decodeAggGroupKey(par.GroupKey)
		if gkErr != nil {
			return nil, "", nil, fmt.Errorf("failed to decode group key in aggregate continuation: %w", gkErr)
		}
		// Group keys are SCALARS — the aggregate encoder never writes a
		// contValProtoMsg key, so a pendingProtoRowValue here is a crafted or
		// corrupt continuation. It must error loudly via the EXPLICIT rejection
		// walk (NOT resolvePendingProtoValues, whose representation-tagged
		// resolution would happily rebuild a resolvable crafted token), never
		// leak a placeholder into the resumed group's output row.
		for ki := range keyVals {
			if gErr := rejectPendingProtoValues(keyVals[ki]); gErr != nil {
				return nil, "", nil, fmt.Errorf("invalid group-key value %d in aggregate continuation: %w", ki, gErr)
			}
		}
	}

	if len(par.AccumulatorStates) == 0 {
		return innerContinuation, groupKey, nil, nil
	}

	as := par.AccumulatorStates[0]

	// SHAPE GUARD — the format is GO-ENGINE-PRIVATE (see the file header): the
	// AggregateCursorContinuation proto SCHEMA is shared with Java, but Go packs
	// a Go-specific layout into AccumulatorState.state — exactly
	// [count] ++ per-aggregate[count, sum, sumI, allInt, min, max] = 1+6*numAggs
	// typed slots. Java's StreamGrouping writes a different layout under the same
	// schema, so a foreign continuation would proto-Unmarshal cleanly and then be
	// mis-read positionally — silently zero-filling the partials (wrong
	// aggregates on resume, no error). Requiring the EXACT Go slot count rejects
	// any non-Go-format continuation loudly, and also catches internal
	// encode/decode drift. (Reachable only defensively today: the SQL layer
	// rejects external continuations with ErrCodeUnsupportedOperation, and the
	// record-layer executor is Go's own engine — but correct-or-loud, never
	// silent.)
	if want := 1 + 6*numAggs; len(as.State) != want {
		return nil, "", nil, fmt.Errorf(
			"aggregate continuation: accumulator has %d typed states, expected %d (1 + 6*%d aggregates) — not a Go-format continuation",
			len(as.State), want, numAggs)
	}

	gs = &groupState{
		keyVals: keyVals,
		counts:  make([]int64, numAggs),
		sums:    make([]float64, numAggs),
		sumsI:   make([]int64, numAggs),
		allInt:  make([]bool, numAggs),
		mins:    make([]any, numAggs),
		maxs:    make([]any, numAggs),
	}

	// Positional decode, correct-or-loud: each slot MUST carry the type the
	// encoder wrote. A type mismatch is a foreign/corrupt continuation (the
	// shape guard above already rejects a wrong slot COUNT; this rejects a wrong
	// slot TYPE at a matching count) — error, never silently coerce to zero.
	idx := 0
	int64At := func(what string) (int64, error) {
		v, ok := as.State[idx].State.(*gen.OneOfTypedState_Int64State)
		if !ok {
			return 0, fmt.Errorf("aggregate continuation: %s slot %d is not int64 — not a Go-format continuation", what, idx)
		}
		idx++
		return v.Int64State, nil
	}
	doubleAt := func(what string) (float64, error) {
		v, ok := as.State[idx].State.(*gen.OneOfTypedState_DoubleState)
		if !ok {
			return 0, fmt.Errorf("aggregate continuation: %s slot %d is not double — not a Go-format continuation", what, idx)
		}
		idx++
		return v.DoubleState, nil
	}
	if gs.count, err = int64At("count"); err != nil {
		return nil, "", nil, err
	}
	for i := 0; i < numAggs; i++ {
		if gs.counts[i], err = int64At("agg-count"); err != nil {
			return nil, "", nil, err
		}
		if gs.sums[i], err = doubleAt("agg-sum"); err != nil {
			return nil, "", nil, err
		}
		if gs.sumsI[i], err = int64At("agg-sumI"); err != nil {
			return nil, "", nil, err
		}
		allIntVal, aiErr := int64At("agg-allInt")
		if aiErr != nil {
			return nil, "", nil, aiErr
		}
		gs.allInt[i] = allIntVal != 0
		// min_i / max_i: one BYTES slot each (the encoder always writes
		// BytesState). A non-bytes slot is a foreign/corrupt continuation →
		// loud, matching the numeric slots above. An EMPTY byte run is the
		// legitimate "no partial" case (nil min/max); non-empty bytes must
		// decode and validate.
		extremum := func(what string) (any, error) {
			v, ok := as.State[idx].State.(*gen.OneOfTypedState_BytesState)
			if !ok {
				return nil, fmt.Errorf("aggregate continuation: %s slot %d is not bytes — not a Go-format continuation", what, idx)
			}
			idx++
			if len(v.BytesState) == 0 {
				return nil, nil // absent partial
			}
			val, _, dErr := readContValue(v.BytesState)
			if dErr != nil {
				return nil, fmt.Errorf("failed to decode %s state in aggregate continuation: %w", what, dErr)
			}
			// A partial is a scalar NUMERIC or nil — anything else (a []any, a
			// string, a proto placeholder) is crafted/corrupt.
			if gErr := validateAggExtremum(val); gErr != nil {
				return nil, fmt.Errorf("invalid %s state in aggregate continuation: %w", what, gErr)
			}
			return val, nil
		}
		if gs.mins[i], err = extremum("MIN"); err != nil {
			return nil, "", nil, err
		}
		if gs.maxs[i], err = extremum("MAX"); err != nil {
			return nil, "", nil, err
		}
	}

	return innerContinuation, groupKey, gs, nil
}

// sortInnerExhaustedMarker rides the proto's minimum_key field as the Go-owned
// EMIT-PHASE discriminator: the sort plan is a Go extension (Java's Cascades
// has no physical sort operator, so Java can neither produce nor consume these
// continuations), and Go's fill phase never uses minimum_key — in Java's own
// MemorySortCursor the field's presence likewise means "rows were already
// emitted". A present marker means the INNER WAS EXHAUSTED when the
// continuation was taken (a per-row emit-phase continuation): resume must go
// straight to emit over the restored remaining buffer — re-running the fill
// loop would re-scan the inner from scratch and duplicate every restored row
// (Go's buffer is a slice, not Java's key-deduped scratchpad map).
var sortInnerExhaustedMarker = []byte{1}

// encodeSortContinuation serializes the sort cursor's state using
// Java's MemorySortContinuation proto. The buffered records are
// serialized as JSON bytes in the SortedRecord.message field.
// Java uses protobuf-serialized records; Go serializes each buffered row's
// positional data (field names + a typed-codec slot blob) as JSON in that
// Go-owned field.
//
// innerExhausted=true is the EMIT-PHASE form (sortEmitContinuation): buf is the
// REMAINING sorted output and the inner continuation is omitted (the inner is
// done); the minimum_key marker tells decode to resume in emit phase.
func encodeSortContinuation(
	innerCont recordlayer.RecordCursorContinuation,
	buf []QueryResult,
	innerExhausted bool,
) ([]byte, error) {
	var innerBytes []byte
	if innerCont != nil && !innerExhausted {
		var err error
		innerBytes, err = innerCont.ToBytes()
		if err != nil {
			return nil, err
		}
	}

	msg := &gen.MemorySortContinuation{
		Continuation: innerBytes,
	}
	if innerExhausted {
		msg.MinimumKey = sortInnerExhaustedMarker
	}

	for _, qr := range buf {
		// Errors are PROPAGATED, never swallowed into a skipped record: a
		// dropped buffer row would silently vanish from the sorted output on
		// resume (wrong results, no error).
		//
		// PAYLOAD FORMAT (Go-owned): the SortedRecord proto mirrors Java's
		// byte-for-byte and cannot grow fields, but Go already stores a JSON
		// datum in `message` where Java stores record-proto bytes — the
		// payload inside the opaque field is ours to version. The ONE format
		// is a 3-element JSON array [_, _, positional]; the decoder rejects
		// every other shape (the pre-release object/2-element tolerances are
		// deleted). Slot 1 was the QueryResult
		// .Complete flag, now RETIRED — the field is deleted, decode IGNORES slot 1,
		// and it is written as a constant `false` dead placeholder purely to keep the
		// 3-slot array shape wire-identical (no format-version bump).
		//
		// The Positional row is the SOLE runtime output row (there is no
		// name-keyed row), so a resumed sort buffer MUST carry it or the reader
		// rejects every buffered row after a page boundary. Slot 0 (formerly the
		// JSON datum) is null, slot 1 is the retired-Complete placeholder, slot 2
		// is the positional {n:[field names], b:<typed-codec slot blob>}. The slots
		// ride the typed codec (appendContSlice), NOT JSON: JSON collapses []byte to
		// a base64 string, every number to float64 (int64>2^53 rounds, a whole
		// DOUBLE re-narrows to int64), corrupting a BYTES/DOUBLE ORDER BY key whose
		// buffer straddles a page. `b` is a []byte, so json.Marshal base64-encodes
		// the blob (invalid UTF-8 in the typed bytes survives). Decode reconstructs
		// the PositionalRow via positionalTypeFromNames + readContSlice. A
		// pre-positional binary's object / 2-element payload, or an OLD 3-element
		// {n, s:[JSON slots]} payload (no `b`), carries no typed blob and is rejected
		// on decode (fail-closed — its slots are unreconstructable losslessly).
		// A buffered sort row without a positional layout cannot round-trip:
		// the strict decoder (rightly) rejects a 2-element payload, so
		// emitting one would mint a continuation THIS binary cannot resume.
		// Fail the encode loudly instead.
		if qr.Positional == nil || qr.Positional.Type == nil {
			return nil, fmt.Errorf("sort continuation: a buffered row has no positional layout — cannot encode a resumable continuation")
		}
		names := make([]string, len(qr.Positional.Type.Fields))
		for fi, f := range qr.Positional.Type.Fields {
			names[fi] = f.Name
		}
		blob, bErr := appendContSlice(nil, qr.Positional.Slots)
		if bErr != nil {
			return nil, fmt.Errorf("failed to encode sorted record slots for continuation: %w", bErr)
		}
		payload := []any{nil, false, map[string]any{
			"n": names,
			"b": blob, // typed-codec slot blob; json.Marshal base64-encodes []byte
		}}
		jsonBytes, jErr := json.Marshal(payload)
		if jErr != nil {
			return nil, fmt.Errorf("failed to marshal sorted record for continuation: %w", jErr)
		}
		var pkBytes []byte
		if qr.PrimaryKey != nil {
			pkBytes = qr.PrimaryKey.Pack()
		}
		sr := &gen.SortedRecord{
			PrimaryKey: pkBytes,
			Message:    jsonBytes,
		}
		srBytes, srErr := proto.Marshal(sr)
		if srErr != nil {
			return nil, fmt.Errorf("failed to marshal sorted record for continuation: %w", srErr)
		}
		msg.Records = append(msg.Records, srBytes)
	}

	return proto.Marshal(msg)
}

// decodeSortContinuation deserializes the MemorySortContinuation proto.
// Buffered STRUCT column slots (contValProtoMsg placeholders, see
// pendingProtoRowValue) rebuild as their ENCODED representation — flag 0
// (generated) via protoregistry.GlobalTypes, flag 1 (*dynamicpb.Message) via
// resolve (metadata descriptors + dynamicpb). A nil resolve is valid ONLY for
// buffers with no dynamic (flag-1) struct slots — a dynamic placeholder with
// no resolver fails the resume loudly, never silently.
//
// innerExhausted reports the emit-phase marker (sortInnerExhaustedMarker in
// minimum_key): true means the inner was exhausted when the continuation was
// taken and buf is the remaining SORTED output — the resumed cursor must go
// straight to emit (loaded=true), never re-run the fill loop.
func decodeSortContinuation(data []byte, resolve protoDescriptorResolver) (innerContinuation []byte, buf []QueryResult, innerExhausted bool, err error) {
	msg := &gen.MemorySortContinuation{}
	if err := proto.Unmarshal(data, msg); err != nil {
		return nil, nil, false, fmt.Errorf("failed to unmarshal sort continuation: %w", err)
	}
	innerExhausted = len(msg.MinimumKey) > 0

	for i, srBytes := range msg.Records {
		// Errors are PROPAGATED, never swallowed into a skipped record: a
		// corrupt buffered record must fail the resume, not silently drop a
		// row from the sorted output (wrong results, no error).
		sr := &gen.SortedRecord{}
		if pErr := proto.Unmarshal(srBytes, sr); pErr != nil {
			return nil, nil, false, fmt.Errorf("failed to unmarshal sorted record %d in continuation: %w", i, pErr)
		}
		// SINGLE payload format (RFC-180 H6 — the pre-release tolerance
		// branches for the bare-object datum, the two-element array and the
		// lossy {n, s:[JSON slots]} form are deleted by the same argument
		// that deleted JSON tag 15: no shipped binary ever wrote them):
		// a 3-element JSON array whose slot 2 is {n:[names], b:<typed-codec
		// slot blob>}. Slots 0/1 are historical placeholders, ignored.
		// Anything else fails the resume loudly.
		var wrapper []json.RawMessage
		if jErr := json.Unmarshal(sr.Message, &wrapper); jErr != nil {
			return nil, nil, false, fmt.Errorf("failed to unmarshal sorted record %d payload in continuation: %w", i, jErr)
		}
		if len(wrapper) != 3 {
			return nil, nil, false, fmt.Errorf("sorted record %d payload has %d elements, want 3 — not a sort continuation this binary wrote", i, len(wrapper))
		}
		var pp struct {
			N []string `json:"n"`
			B []byte   `json:"b"` // typed-codec slot blob (base64 in JSON)
		}
		if jErr := json.Unmarshal(wrapper[2], &pp); jErr != nil {
			return nil, nil, false, fmt.Errorf("failed to unmarshal sorted record %d positional payload in continuation: %w", i, jErr)
		}
		// A present positional always encodes at least the uvarint slot
		// count, so an empty blob is not a shape this binary produces.
		if len(pp.B) == 0 {
			return nil, nil, false, fmt.Errorf("sorted record %d has no typed slot blob — not a sort continuation this binary wrote; restart the query", i)
		}
		slots, _, sErr := readContSlice(pp.B)
		if sErr != nil {
			return nil, nil, false, fmt.Errorf("failed to decode sorted record %d slots in continuation: %w", i, sErr)
		}
		if len(slots) != len(pp.N) {
			return nil, nil, false, fmt.Errorf("sorted record %d: %d slots for %d column names — corrupt continuation", i, len(slots), len(pp.N))
		}
		// Rebuild STRUCT column slots (and struct elements inside ARRAY
		// slots) from their descriptor-less placeholders. Any failure —
		// no resolver, unresolvable type, bad payload — is a LOUD
		// error: a placeholder must never leak into the row domain.
		for si := range slots {
			rv, rErr := resolvePendingProtoValues(slots[si], resolve)
			if rErr != nil {
				return nil, nil, false, fmt.Errorf("failed to rebuild sorted record %d slot %d in continuation: %w", i, si, rErr)
			}
			slots[si] = rv
		}
		positional := &PositionalRow{Type: positionalTypeFromNames(pp.N), Slots: slots}
		var pk tuple.Tuple
		if sr.PrimaryKey != nil {
			var pkErr error
			pk, pkErr = tuple.Unpack(sr.PrimaryKey)
			if pkErr != nil {
				return nil, nil, false, fmt.Errorf("failed to unpack sorted record %d primary key in continuation: %w", i, pkErr)
			}
		}
		buf = append(buf, QueryResult{PrimaryKey: pk, Positional: positional})
	}

	return msg.Continuation, buf, innerExhausted, nil
}
