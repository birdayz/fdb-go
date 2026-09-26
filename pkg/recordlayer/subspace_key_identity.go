package recordlayer

import (
	"encoding/binary"
	"math"
	"math/big"
	"reflect"

	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// subspaceKeyIdentity returns a comparable Go value that two index or
// former-index subspace keys share exactly when Java holds them equal.
//
// Java normalizes a subspace key whenever one is assigned (Index.java:88-97,
// :132, :187, :222-224, :413-415; FormerIndex.java:51-58) with
// TupleTypeUtil.toTupleEquivalentValue (TupleTypeUtil.java:98-124). Every
// comparison after that is Object.equals and hashCode on the normalized
// objects: Index.equals (Index.java:695-711), MetaDataValidator's
// assignedPrefixes and assignedFormerPrefixes maps (MetaDataValidator.java:
// 103-165), RecordMetaData.getIndexFromSubspaceKey (RecordMetaData.java:329-336)
// and MetaDataEvolutionValidator's index and former-index maps
// (MetaDataEvolutionValidator.java:449-475). Go normalizes a key when
// SetSubspaceKey assigns it and a decoded key is already in normalized form, but
// FormerIndex.SubspaceKey is an exported field that can hold any Go value, so
// every one of those comparisons normalizes again through this function. The
// normalized forms are:
//
//   - null is nil.
//   - Byte, Short, Integer and Long are one Long. So are every Go signed
//     integer, and every unsigned one that fits in an int64.
//   - A BigInteger strictly between Long.MIN_VALUE and Long.MAX_VALUE becomes
//     that Long. One AT either bound, or beyond, stays a BigInteger, because
//     TupleTypeUtil's comparisons are strict. So a big.Int holding MaxInt64 is
//     not equal to int64 MaxInt64, in Java and here. An unsigned Go integer
//     above MaxInt64 is such a BigInteger (it is what Java decodes it as).
//   - byte[] becomes a ByteString, equal by content. An fdb.Key, and any other
//     fdb.KeyConvertible that is not a Tuple, is bytes too: the tuple encoder
//     writes it as bytes.
//   - A Tuple and a List become one List, equal element by element after each
//     element is normalized.
//   - A protobuf enum becomes its number as a Long, and an FDBRecordVersion
//     its versionstamp.
//   - Float and Double stay distinct from each other and from every integer.
//     Each is equal by Float.equals or Double.equals, which compare the bit
//     pattern after collapsing every NaN into one. So NaN equals NaN, and 0.0
//     does not equal -0.0.
//   - String, Boolean, UUID and Versionstamp are equal by value.
//
// This is not "pack both keys and compare the bytes", which it resembles.
// Packing equates a BigInteger at a Long bound with that Long, where Java does
// not. It keeps two NaN payloads apart, where Java does not. And it panics on a
// value the encoder rejects.
//
// Anything else is Java's default arm, the object as given. It equals a value
// of the same dynamic type when that type is comparable and the two are ==. A
// value Go cannot compare, and a list holding a value outside the forms above,
// equals nothing, not even itself. Neither can be tuple-encoded, so no index
// whose key is one can reach a store in either engine.
func subspaceKeyIdentity(key any) any {
	if key == nil {
		return nil
	}
	if enc, ok := appendSubspaceKeyIdentity(nil, key); ok {
		return subspaceKeyID(enc)
	}
	switch key.(type) {
	case tuple.Tuple, []any:
		return new(unidentifiableSubspaceKey)
	}
	if reflect.ValueOf(key).Comparable() {
		return opaqueSubspaceKey{value: key}
	}
	return new(unidentifiableSubspaceKey)
}

// isNilSubspaceKey reports whether a subspace key is Java's null once
// normalized: nil, or a typed nil tupleEquivalentValue maps to it.
func isNilSubspaceKey(v any) bool {
	return tupleEquivalentValue(v) == nil
}

// tupleEquivalentValue is TupleTypeUtil.toTupleEquivalentValue
// (TupleTypeUtil.java:98-124) in the forms the Go tuple encoder writes: what
// Index.setSubspaceKey stores (Index.java:88-97, :413-415). Every signed
// integer, and every unsigned one that fits, becomes an int64; a big.Int
// strictly inside the int64 range becomes that int64, and one outside stays a
// *big.Int, copied; a []byte is copied, as ByteString.copyFrom copies; a Tuple
// or a []any becomes a Tuple of normalized items; a protobuf enum becomes its
// number and an FDBRecordVersion its versionstamp. The rest is kept as given,
// an unsigned integer above MaxInt64 included, which the encoder writes as the
// same bytes Java writes for that BigInteger. Nil, and a typed nil of a type
// that stands for a Java reference (*big.Int, *FDBRecordVersion, a pointer to
// a generated enum), map to nil, Java's null.
//
// subspaceKeyIdentity of the result equals subspaceKeyIdentity of the
// argument, so normalizing on assignment changes no comparison. What it
// changes is packing: the encoder has no case for an int32, an unsigned type
// narrower than uint, a []any or an enum, and panics on each, where Java
// normalizes the same key and writes it.
func tupleEquivalentValue(v any) any {
	switch k := v.(type) {
	case int:
		return int64(k)
	case int8:
		return int64(k)
	case int16:
		return int64(k)
	case int32:
		return int64(k)
	case uint8:
		return int64(k)
	case uint16:
		return int64(k)
	case uint32:
		return int64(k)
	case uint:
		if uint64(k) <= math.MaxInt64 {
			return int64(k)
		}
		return uint64(k)
	case uint64:
		if k <= math.MaxInt64 {
			return int64(k)
		}
		return k
	case *big.Int:
		if k == nil {
			// A typed nil is Java's null: a nil BigInteger reference.
			return nil
		}
		return tupleEquivalentBigInteger(k)
	case big.Int:
		return tupleEquivalentBigInteger(&k)
	case []byte:
		dup := make([]byte, len(k))
		copy(dup, k)
		return dup
	case fdb.Key:
		// The tuple encoder writes an fdb.Key as bytes, so it is the same key
		// as the []byte of its content, and it is copied as a []byte is.
		dup := make([]byte, len(k))
		copy(dup, k)
		return dup
	case tuple.Tuple:
		return tupleEquivalentList(len(k), func(i int) any { return k[i] })
	case []any:
		return tupleEquivalentList(len(k), func(i int) any { return k[i] })
	case *FDBRecordVersion:
		if k == nil {
			// A typed nil is Java's null, as for *big.Int.
			return nil
		}
		vs, err := k.ToVersionstamp()
		if err != nil {
			return v
		}
		return vs
	case protoreflect.Enum:
		// A pointer to a generated enum has the enum's methods, so a typed
		// nil one lands here; Number would dereference it. It is Java's null.
		if rv := reflect.ValueOf(k); rv.Kind() == reflect.Pointer && rv.IsNil() {
			return nil
		}
		return int64(k.Number())
	default:
		return v
	}
}

func tupleEquivalentBigInteger(v *big.Int) any {
	if v.IsInt64() {
		if n := v.Int64(); n != math.MinInt64 && n != math.MaxInt64 {
			return n
		}
	}
	return new(big.Int).Set(v)
}

func tupleEquivalentList(n int, item func(int) any) tuple.Tuple {
	out := make(tuple.Tuple, n)
	for i := range n {
		out[i] = tupleEquivalentValue(item(i))
	}
	return out
}

// subspaceKeysEqual is Objects.equals over two normalized subspace keys.
func subspaceKeysEqual(a, b any) bool {
	return subspaceKeyIdentity(a) == subspaceKeyIdentity(b)
}

// subspaceKeyID is the canonical encoding of a normalized key. Each form
// carries a tag byte and is self-delimiting, so two keys have one encoding
// exactly when their normalized forms are equal.
type subspaceKeyID string

// opaqueSubspaceKey is Java's default arm: a value with none of the forms
// above, compared as its dynamic type and value.
type opaqueSubspaceKey struct{ value any }

// unidentifiableSubspaceKey is handed out once per call, so a key that cannot
// be compared equals nothing. The field keeps the struct non-zero-sized: Go
// may give every allocation of a zero-sized type the same address.
type unidentifiableSubspaceKey struct{ _ byte }

func appendSubspaceKeyIdentity(buf []byte, v any) ([]byte, bool) {
	switch k := v.(type) {
	case nil:
		return append(buf, 'n'), true
	case int:
		return appendIdentityLong(buf, int64(k)), true
	case int8:
		return appendIdentityLong(buf, int64(k)), true
	case int16:
		return appendIdentityLong(buf, int64(k)), true
	case int32:
		return appendIdentityLong(buf, int64(k)), true
	case int64:
		return appendIdentityLong(buf, k), true
	case uint:
		return appendIdentityUnsigned(buf, uint64(k)), true
	case uint8:
		return appendIdentityLong(buf, int64(k)), true
	case uint16:
		return appendIdentityLong(buf, int64(k)), true
	case uint32:
		return appendIdentityLong(buf, int64(k)), true
	case uint64:
		return appendIdentityUnsigned(buf, k), true
	case *big.Int:
		if k == nil {
			return buf, false
		}
		return appendIdentityBigInteger(buf, k), true
	case big.Int:
		return appendIdentityBigInteger(buf, &k), true
	case tuple.Tuple:
		return appendIdentityList(buf, len(k), func(i int) any { return k[i] })
	case []any:
		return appendIdentityList(buf, len(k), func(i int) any { return k[i] })
	case []byte:
		return appendIdentityBytes(buf, 'b', k), true
	case string:
		return appendIdentityBytes(buf, 's', []byte(k)), true
	case bool:
		if k {
			return append(buf, 'z', 1), true
		}
		return append(buf, 'z', 0), true
	case float32:
		bits := math.Float32bits(k)
		if k != k {
			bits = 0x7fc00000 // Float.floatToIntBits of every NaN
		}
		return binary.BigEndian.AppendUint32(append(buf, 'f'), bits), true
	case float64:
		bits := math.Float64bits(k)
		if k != k {
			bits = 0x7ff8000000000000 // Double.doubleToLongBits of every NaN
		}
		return binary.BigEndian.AppendUint64(append(buf, 'd'), bits), true
	case tuple.UUID:
		return append(append(buf, 'u'), k[:]...), true
	case tuple.Versionstamp:
		return appendIdentityVersionstamp(buf, k), true
	case *FDBRecordVersion:
		if k == nil {
			return buf, false
		}
		vs, err := k.ToVersionstamp()
		if err != nil {
			return buf, false
		}
		return appendIdentityVersionstamp(buf, vs), true
	case protoreflect.Enum:
		return appendIdentityLong(buf, int64(k.Number())), true
	case fdb.KeyConvertible:
		// After the Tuple arm: a Tuple is a KeyConvertible too, and packs as a
		// nested tuple, not as bytes.
		return appendIdentityBytes(buf, 'b', k.FDBKey()), true
	default:
		return buf, false
	}
}

func appendIdentityLong(buf []byte, v int64) []byte {
	return binary.BigEndian.AppendUint64(append(buf, 'l'), uint64(v))
}

func appendIdentityUnsigned(buf []byte, v uint64) []byte {
	if v <= math.MaxInt64 {
		return appendIdentityLong(buf, int64(v))
	}
	return appendIdentityBigInteger(buf, new(big.Int).SetUint64(v))
}

// appendIdentityBigInteger is TupleTypeUtil's BigInteger arm: strictly inside
// the Long range it is a Long, otherwise it stays a BigInteger.
func appendIdentityBigInteger(buf []byte, v *big.Int) []byte {
	if v.IsInt64() {
		if n := v.Int64(); n != math.MinInt64 && n != math.MaxInt64 {
			return appendIdentityLong(buf, n)
		}
	}
	return appendIdentityBytes(buf, 'B', []byte(v.String()))
}

func appendIdentityBytes(buf []byte, tag byte, b []byte) []byte {
	buf = binary.AppendUvarint(append(buf, tag), uint64(len(b)))
	return append(buf, b...)
}

func appendIdentityVersionstamp(buf []byte, vs tuple.Versionstamp) []byte {
	buf = append(append(buf, 'v'), vs.TransactionVersion[:]...)
	return binary.BigEndian.AppendUint16(buf, vs.UserVersion)
}

func appendIdentityList(buf []byte, n int, item func(int) any) ([]byte, bool) {
	buf = binary.AppendUvarint(append(buf, 'L'), uint64(n))
	for i := range n {
		var ok bool
		if buf, ok = appendSubspaceKeyIdentity(buf, item(i)); !ok {
			return buf, false
		}
	}
	return buf, true
}
