/*
 * tuple_pack.go
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

package tuple

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"sync"

	"fdb.dev/pkg/fdbgo/fdb"
)

type packer struct {
	versionstampPos int32
	buf             []byte
}

func newPacker() *packer {
	return &packer{
		versionstampPos: -1,
		buf:             make([]byte, 0, 64),
	}
}

func (p *packer) putByte(b byte) {
	p.buf = append(p.buf, b)
}

func (p *packer) putBytes(b []byte) {
	p.buf = append(p.buf, b...)
}

func (p *packer) putBytesNil(b []byte, i int) {
	for i >= 0 {
		p.putBytes(b[:i+1])
		p.putByte(0xFF)
		b = b[i+1:]
		i = bytes.IndexByte(b, 0x00)
	}
	p.putBytes(b)
}

func (p *packer) encodeBytes(code byte, b []byte) {
	p.putByte(code)
	if i := bytes.IndexByte(b, 0x00); i >= 0 {
		p.putBytesNil(b, i)
	} else {
		p.putBytes(b)
	}
	p.putByte(0x00)
}

func (p *packer) putStringNil(s string, i int) {
	for i >= 0 {
		p.buf = append(p.buf, s[:i+1]...)
		p.buf = append(p.buf, 0xFF)
		s = s[i+1:]
		i = strings.IndexByte(s, 0x00)
	}
	p.buf = append(p.buf, s...)
}

func (p *packer) encodeString(s string) {
	p.putByte(stringCode)
	if i := strings.IndexByte(s, 0x00); i >= 0 {
		p.putStringNil(s, i)
	} else {
		p.buf = append(p.buf, s...)
	}
	p.putByte(0x00)
}

// AppendString appends to dst the encoding Pack gives the string element s.
func AppendString(dst []byte, s string) []byte {
	p := packer{versionstampPos: -1, buf: dst}
	p.encodeString(s)
	return p.buf
}

func (p *packer) encodeUint(i uint64) {
	if i == 0 {
		p.putByte(intZeroCode)
		return
	}

	n := bisectLeft(i)
	var scratch [8]byte

	p.putByte(byte(intZeroCode + n))
	binary.BigEndian.PutUint64(scratch[:], i)

	p.putBytes(scratch[8-n:])
}

func (p *packer) encodeInt(i int64) {
	if i >= 0 {
		p.encodeUint(uint64(i))
		return
	}

	n := bisectLeft(uint64(-i))
	var scratch [8]byte

	p.putByte(byte(intZeroCode - n))
	offsetEncoded := int64(sizeLimits[n]) + i
	binary.BigEndian.PutUint64(scratch[:], uint64(offsetEncoded))

	p.putBytes(scratch[8-n:])
}

func (p *packer) encodeBigInt(i *big.Int) {
	length := len(i.Bytes())
	if length > 0xff {
		panic(fmt.Sprintf("Integer magnitude is too large (more than 255 bytes)"))
	}

	if i.Sign() >= 0 {
		intBytes := i.Bytes()
		if length > 8 {
			p.putByte(byte(posIntEnd))
			p.putByte(byte(len(intBytes)))
		} else {
			p.putByte(byte(intZeroCode + length))
		}

		p.putBytes(intBytes)
	} else {
		add := new(big.Int).Lsh(big.NewInt(1), uint(length*8))
		add.Sub(add, big.NewInt(1))
		transformed := new(big.Int)
		transformed.Add(i, add)

		intBytes := transformed.Bytes()
		if length > 8 {
			p.putByte(byte(negIntStart))
			p.putByte(byte(length ^ 0xff))
		} else {
			p.putByte(byte(intZeroCode - length))
		}

		// For large negative numbers whose absolute value begins with 0xff bytes,
		// the transformed bytes may begin with 0x00 bytes. However, intBytes
		// will only contain the non-zero suffix, so this loop is needed to make
		// the value written be the correct length.
		for i := len(intBytes); i < length; i++ {
			p.putByte(0x00)
		}

		p.putBytes(intBytes)
	}
}

func (p *packer) encodeFloat(f float32) {
	var scratch [4]byte
	binary.BigEndian.PutUint32(scratch[:], math.Float32bits(f))
	adjustFloatBytes(scratch[:], true)

	p.putByte(floatCode)
	p.putBytes(scratch[:])
}

func (p *packer) encodeDouble(d float64) {
	var scratch [8]byte
	binary.BigEndian.PutUint64(scratch[:], math.Float64bits(d))
	adjustFloatBytes(scratch[:], true)

	p.putByte(doubleCode)
	p.putBytes(scratch[:])
}

func (p *packer) encodeUUID(u UUID) {
	p.putByte(uuidCode)
	p.putBytes(u[:])
}

func (p *packer) encodeVersionstamp(v Versionstamp) {
	p.putByte(versionstampCode)

	isIncomplete := v.TransactionVersion == incompleteTransactionVersion
	if isIncomplete {
		if p.versionstampPos != -1 {
			panic(fmt.Sprintf("Tuple can only contain one incomplete versionstamp"))
		}

		p.versionstampPos = int32(len(p.buf))
	}

	p.putBytes(v.Bytes())
}

// encodeElement encodes a single tuple element into the packer.
func (p *packer) encodeElement(e TupleElement) {
	switch e := e.(type) {
	case Tuple:
		p.encodeTuple(e, true, false)
	case nil:
		p.putByte(nilCode)
	case int:
		p.encodeInt(int64(e))
	case int64:
		p.encodeInt(e)
	case uint:
		p.encodeUint(uint64(e))
	case uint64:
		p.encodeUint(e)
	case *big.Int:
		p.encodeBigInt(e)
	case big.Int:
		p.encodeBigInt(&e)
	case []byte:
		p.encodeBytes(bytesCode, e)
	case fdb.KeyConvertible:
		p.encodeBytes(bytesCode, []byte(e.FDBKey()))
	case string:
		p.encodeString(e)
	case float32:
		p.encodeFloat(e)
	case float64:
		p.encodeDouble(e)
	case bool:
		if e {
			p.putByte(trueCode)
		} else {
			p.putByte(falseCode)
		}
	case UUID:
		p.encodeUUID(e)
	case Versionstamp:
		// encodeElement backs only the vanilla (non-versionstamp) APIs — Pack1WithPrefix,
		// Pack1ConcatWithPrefix, Packer.EncodeElement — which do NOT track a versionstamp offset.
		// An incomplete versionstamp here would be silently encoded as the literal 0xFF×10 and
		// never resolved to the commit version. Panic, exactly as encodeTuple does for a vanilla
		// Pack (tuple.go encodeTuple), so the misuse fails loud instead of writing a corrupt key.
		// Callers needing a versionstamp must use PackWithVersionstamp.
		if e.TransactionVersion == incompleteTransactionVersion {
			panic("Incomplete Versionstamp included in vanilla tuple pack")
		}
		p.encodeVersionstamp(e)
	default:
		panic(fmt.Sprintf("unencodable element (%v, type %T)", e, e))
	}
}

func (p *packer) encodeTuple(t Tuple, nested bool, versionstamps bool) {
	if nested {
		p.putByte(nestedCode)
	}

	for i, e := range t {
		switch e := e.(type) {
		case Tuple:
			p.encodeTuple(e, true, versionstamps)
		case nil:
			p.putByte(nilCode)
			if nested {
				p.putByte(0xff)
			}
		case int:
			p.encodeInt(int64(e))
		case int64:
			p.encodeInt(e)
		case uint:
			p.encodeUint(uint64(e))
		case uint64:
			p.encodeUint(e)
		case *big.Int:
			p.encodeBigInt(e)
		case big.Int:
			p.encodeBigInt(&e)
		case []byte:
			p.encodeBytes(bytesCode, e)
		case fdb.KeyConvertible:
			p.encodeBytes(bytesCode, []byte(e.FDBKey()))
		case string:
			p.encodeString(e)
		case float32:
			p.encodeFloat(e)
		case float64:
			p.encodeDouble(e)
		case bool:
			if e {
				p.putByte(trueCode)
			} else {
				p.putByte(falseCode)
			}
		case UUID:
			p.encodeUUID(e)
		case Versionstamp:
			if versionstamps == false && e.TransactionVersion == incompleteTransactionVersion {
				panic(fmt.Sprintf("Incomplete Versionstamp included in vanilla tuple pack"))
			}

			p.encodeVersionstamp(e)
		default:
			panic(fmt.Sprintf("unencodable element at index %d (%v, type %T)", i, t[i], t[i]))
		}
	}

	if nested {
		p.putByte(0x00)
	}
}

// Pack returns a new byte slice encoding the provided tuple. Pack will panic if
// the tuple contains an element of any type other than []byte,
// fdb.KeyConvertible, string, int64, int, uint64, uint, *big.Int, big.Int, float32,
// float64, bool, tuple.UUID, tuple.Versionstamp, nil, or a Tuple with elements of
// valid types. It will also panic if an integer is specified with a value outside
// the range [-2**2040+1, 2**2040-1]
//
// Tuple satisfies the fdb.KeyConvertible interface, so it is not necessary to
// call Pack when using a Tuple with a FoundationDB API function that requires a
// key.
//
// This method will panic if it contains an incomplete Versionstamp. Use
// PackWithVersionstamp instead.
func (t Tuple) Pack() []byte {
	return t.PackWithPrefix(nil)
}

var packerPool = sync.Pool{
	New: func() any {
		return &packer{
			versionstampPos: -1,
			buf:             make([]byte, 0, 128),
		}
	},
}

// PackWithPrefix packs the tuple and prepends the given prefix in a single allocation.
// Uses a pooled packer to avoid allocating a new packer per call.
func (t Tuple) PackWithPrefix(prefix []byte) []byte {
	p := packerPool.Get().(*packer)
	p.versionstampPos = -1
	p.buf = p.buf[:0]
	p.encodeTuple(t, false, false)
	result := make([]byte, len(prefix)+len(p.buf))
	copy(result, prefix)
	copy(result[len(prefix):], p.buf)
	packerPool.Put(p)
	return result
}

// PackConcatWithPrefix packs multiple tuples concatenated into a single key,
// prepending the given prefix. Avoids creating an intermediate combined tuple.
// Saves 1 allocation vs indexEntryKey + Pack for VALUE index entry keys.
func PackConcatWithPrefix(prefix []byte, tuples ...Tuple) []byte {
	p := packerPool.Get().(*packer)
	p.versionstampPos = -1
	p.buf = p.buf[:0]
	for _, t := range tuples {
		p.encodeTuple(t, false, false)
	}
	result := make([]byte, len(prefix)+len(p.buf))
	copy(result, prefix)
	copy(result[len(prefix):], p.buf)
	packerPool.Put(p)
	return result
}

// Pack1WithPrefix packs a single tuple element with a prefix in one allocation.
// Avoids creating a Tuple slice for single-field index expressions.
func Pack1WithPrefix(prefix []byte, elem TupleElement) []byte {
	p := packerPool.Get().(*packer)
	p.versionstampPos = -1
	p.buf = p.buf[:0]
	p.encodeElement(elem)
	result := make([]byte, len(prefix)+len(p.buf))
	copy(result, prefix)
	copy(result[len(prefix):], p.buf)
	packerPool.Put(p)
	return result
}

// Pack1ConcatWithPrefix packs a single element plus a tuple with a prefix.
// Used for single-field index keys that also need a trimmed primary key suffix.
func Pack1ConcatWithPrefix(prefix []byte, elem TupleElement, suffix Tuple) []byte {
	p := packerPool.Get().(*packer)
	p.versionstampPos = -1
	p.buf = p.buf[:0]
	p.encodeElement(elem)
	p.encodeTuple(suffix, false, false)
	result := make([]byte, len(prefix)+len(p.buf))
	copy(result, prefix)
	copy(result[len(prefix):], p.buf)
	packerPool.Put(p)
	return result
}

// Packer exposes the internal packer for advanced callers that need to
// encode multiple elements and then append to a shared buffer.
type Packer struct{ p *packer }

func GetPacker() *Packer {
	p := packerPool.Get().(*packer)
	p.versionstampPos = -1
	p.buf = p.buf[:0]
	return &Packer{p: p}
}

func PutPacker(pk *Packer) { pk.Reset(); packerPool.Put(pk.p) }

func (pk *Packer) Reset()                       { pk.p.versionstampPos = -1; pk.p.buf = pk.p.buf[:0] }
func (pk *Packer) EncodeInt(val int64)          { pk.p.encodeInt(val) }
func (pk *Packer) EncodeString(val string)      { pk.p.encodeString(val) }
func (pk *Packer) EncodeTuple(t Tuple)          { pk.p.encodeTuple(t, false, false) }
func (pk *Packer) EncodeElement(e TupleElement) { pk.p.encodeElement(e) }

func (pk *Packer) AppendInto(buf *[]byte, prefix []byte) []byte {
	b := *buf
	start := len(b)
	needed := start + len(prefix) + len(pk.p.buf)
	if needed > cap(b) {
		newCap := max(2*cap(b), needed)
		newB := make([]byte, start, newCap)
		copy(newB, b)
		b = newB
	}
	b = b[:needed]
	copy(b[start:], prefix)
	copy(b[start+len(prefix):], pk.p.buf)
	*buf = b
	return b[start:needed]
}

// PackWithVersionstamp packs the specified tuple into a key for versionstamp
// operations. See Pack for more information. This function will return an error
// if you attempt to pack a tuple with more than one versionstamp. This function will
// return an error if you attempt to pack a tuple with a versionstamp position larger
// than an uint16 if the API version is less than 520.
func (t Tuple) PackWithVersionstamp(prefix []byte) ([]byte, error) {
	hasVersionstamp, err := t.HasIncompleteVersionstamp()
	if err != nil {
		return nil, err
	}

	apiVersion, err := fdb.GetAPIVersion()
	if err != nil {
		return nil, err
	}

	if hasVersionstamp == false {
		return nil, errors.New("No incomplete versionstamp included in tuple pack with versionstamp")
	}

	p := newPacker()

	if prefix != nil {
		p.putBytes(prefix)
	}

	p.encodeTuple(t, false, true)

	if hasVersionstamp {
		var scratch [4]byte
		var offsetIndex int
		if apiVersion < 520 {
			if p.versionstampPos > math.MaxUint16 {
				return nil, errors.New("Versionstamp position too large")
			}

			offsetIndex = 2
			binary.LittleEndian.PutUint16(scratch[:], uint16(p.versionstampPos))
		} else {
			offsetIndex = 4
			binary.LittleEndian.PutUint32(scratch[:], uint32(p.versionstampPos))
		}

		p.putBytes(scratch[0:offsetIndex])
	}

	return p.buf, nil
}

// HasIncompleteVersionstamp determines if there is at least one incomplete
// versionstamp in a tuple. This function will return an error this tuple has
// more than one versionstamp.
func (t Tuple) HasIncompleteVersionstamp() (bool, error) {
	incompleteCount := t.countIncompleteVersionstamps()

	var err error
	if incompleteCount > 1 {
		err = errors.New("Tuple can only contain one incomplete versionstamp")
	}

	return incompleteCount >= 1, err
}

func (t Tuple) countIncompleteVersionstamps() int {
	incompleteCount := 0

	for _, el := range t {
		switch e := el.(type) {
		case Versionstamp:
			if e.TransactionVersion == incompleteTransactionVersion {
				incompleteCount++
			}
		case Tuple:
			incompleteCount += e.countIncompleteVersionstamps()
		}
	}

	return incompleteCount
}
