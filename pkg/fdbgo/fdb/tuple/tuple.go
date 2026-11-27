/*
 * tuple.go
 *
 * This source file is part of the FoundationDB open source project
 *
 * Copyright 2013-2018 Apple Inc. and the FoundationDB project authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// FoundationDB Go Tuple Layer

// Package tuple provides a layer for encoding and decoding multi-element tuples
// into keys usable by FoundationDB. The encoded key maintains the same sort
// order as the original tuple: sorted first by the first element, then by the
// second element, etc. This makes the tuple layer ideal for building a variety
// of higher-level data models.
//
// For general guidance on tuple usage, see the Tuple section of Data Modeling
// (https://apple.github.io/foundationdb/data-modeling.html#tuples).
//
// FoundationDB tuples can currently encode byte and unicode strings, integers,
// large integers, floats, doubles, booleans, UUIDs, tuples, and NULL values.
// In Go these are represented as []byte (or fdb.KeyConvertible), string, int64
// (or int, uint, uint64), *big.Int (or big.Int), float32, float64, bool,
// UUID, Tuple, and nil.
package tuple

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"

	"fdb.dev/pkg/fdbgo/fdb"
)

// A TupleElement is one of the types that may be encoded in FoundationDB
// tuples. Although the Go compiler cannot enforce this, it is a programming
// error to use an unsupported types as a TupleElement (and will typically
// result in a runtime panic).
//
// The valid types for TupleElement are []byte (or fdb.KeyConvertible), string,
// int64 (or int, uint, uint64), *big.Int (or big.Int), float, double, bool,
// UUID, Tuple, and nil.
type TupleElement any

// Tuple is a slice of objects that can be encoded as FoundationDB tuples. If
// any of the TupleElements are of unsupported types, a runtime panic will occur
// when the Tuple is packed.
//
// Given a Tuple T containing objects only of these types, then T will be
// identical to the Tuple returned by unpacking the byte slice obtained by
// packing T (modulo type normalization to []byte, uint64, and int64).
type Tuple []TupleElement

// String implements the fmt.Stringer interface and returns human-readable
// string representation of this tuple. For most elements, we use the
// object's default string representation.
func (tuple Tuple) String() string {
	sb := strings.Builder{}
	printTuple(tuple, &sb)
	return sb.String()
}

func printTuple(tuple Tuple, sb *strings.Builder) {
	sb.WriteString("(")

	for i, t := range tuple {
		switch t := t.(type) {
		case Tuple:
			printTuple(t, sb)
		case nil:
			sb.WriteString("<nil>")
		case string:
			sb.WriteString(strconv.Quote(t))
		case UUID:
			sb.WriteString("UUID(")
			sb.WriteString(t.String())
			sb.WriteString(")")
		case []byte:
			sb.WriteString("b\"")
			sb.WriteString(fdb.Printable(t))
			sb.WriteString("\"")
		default:
			// For user-defined and standard types, we use standard Go
			// printer, which itself uses Stringer interface.
			fmt.Fprintf(sb, "%v", t)
		}

		if i < len(tuple)-1 {
			sb.WriteString(", ")
		}
	}

	sb.WriteString(")")
}

// UUID wraps a basic byte array as a UUID. We do not provide any special
// methods for accessing or generating the UUID, but as Go does not provide
// a built-in UUID type, this simple wrapper allows for other libraries
// to write the output of their UUID type as a 16-byte array into
// an instance of this type.
type UUID [16]byte

func (uuid UUID) String() string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:])
}

// Versionstamp is struct for a FoundationDB verionstamp. Versionstamps are
// 12 bytes long composed of a 10 byte transaction version and a 2 byte user
// version. The transaction version is filled in at commit time and the user
// version is provided by the application to order results within a transaction.
type Versionstamp struct {
	TransactionVersion [10]byte
	UserVersion        uint16
}

// Returns a human-readable string for this Versionstamp.
func (vs Versionstamp) String() string {
	return fmt.Sprintf("Versionstamp(%s, %d)", fdb.Printable(vs.TransactionVersion[:]), vs.UserVersion)
}

var incompleteTransactionVersion = [10]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}

const versionstampLength = 12

// IncompleteVersionstamp is the constructor you should use to make
// an incomplete versionstamp to use in a tuple.
func IncompleteVersionstamp(userVersion uint16) Versionstamp {
	return Versionstamp{
		TransactionVersion: incompleteTransactionVersion,
		UserVersion:        userVersion,
	}
}

// Bytes converts a Versionstamp struct to a byte slice for encoding in a tuple.
func (v Versionstamp) Bytes() []byte {
	var scratch [versionstampLength]byte

	copy(scratch[:], v.TransactionVersion[:])

	binary.BigEndian.PutUint16(scratch[10:], v.UserVersion)

	return scratch[:]
}

// Type codes: These prefix the different elements in a packed Tuple
// to indicate what type they are.
const (
	nilCode          = 0x00
	bytesCode        = 0x01
	stringCode       = 0x02
	nestedCode       = 0x05
	intZeroCode      = 0x14
	posIntEnd        = 0x1d
	negIntStart      = 0x0b
	floatCode        = 0x20
	doubleCode       = 0x21
	falseCode        = 0x26
	trueCode         = 0x27
	uuidCode         = 0x30
	versionstampCode = 0x33
)

var sizeLimits = []uint64{
	1<<(0*8) - 1,
	1<<(1*8) - 1,
	1<<(2*8) - 1,
	1<<(3*8) - 1,
	1<<(4*8) - 1,
	1<<(5*8) - 1,
	1<<(6*8) - 1,
	1<<(7*8) - 1,
	1<<(8*8) - 1,
}

var minInt64BigInt = big.NewInt(math.MinInt64)

func bisectLeft(u uint64) int {
	var n int
	for sizeLimits[n] < u {
		n++
	}
	return n
}

func adjustFloatBytes(b []byte, encode bool) {
	if (encode && b[0]&0x80 != 0x00) || (!encode && b[0]&0x80 == 0x00) {
		// Negative numbers: flip all of the bytes.
		for i := 0; i < len(b); i++ {
			b[i] = b[i] ^ 0xff
		}
	} else {
		// Positive number: flip just the sign bit.
		b[0] = b[0] ^ 0x80
	}
}

// FDBKey returns the packed representation of a Tuple, and allows Tuple to
// satisfy the fdb.KeyConvertible interface. FDBKey will panic in the same
// circumstances as Pack.
func (t Tuple) FDBKey() fdb.Key {
	return t.Pack()
}

// FDBRangeKeys allows Tuple to satisfy the fdb.ExactRange interface. The range
// represents all keys that encode tuples strictly starting with a Tuple (that
// is, all tuples of greater length than the Tuple of which the Tuple is a
// prefix).
func (t Tuple) FDBRangeKeys() (fdb.KeyConvertible, fdb.KeyConvertible) {
	p := t.Pack()
	return fdb.Key(concat(p, 0x00)), fdb.Key(concat(p, 0xFF))
}

// FDBRangeKeySelectors allows Tuple to satisfy the fdb.Range interface. The
// range represents all keys that encode tuples strictly starting with a Tuple
// (that is, all tuples of greater length than the Tuple of which the Tuple is a
// prefix).
func (t Tuple) FDBRangeKeySelectors() (fdb.Selectable, fdb.Selectable) {
	b, e := t.FDBRangeKeys()
	return fdb.FirstGreaterOrEqual(b), fdb.FirstGreaterOrEqual(e)
}

func concat(a []byte, b ...byte) []byte {
	r := make([]byte, len(a)+len(b))
	copy(r, a)
	copy(r[len(a):], b)
	return r
}
