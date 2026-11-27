/*
 * tuple_unpack.go
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
	"fmt"
	"math/big"
	"unicode/utf8"
)

// findTerminator returns the length of the bytes/string payload up to its
// 0x00 terminator (skipping escaped 0x00FF pairs), or -1 when the input has
// no terminator — a malformed encoding the caller must surface as an error,
// never index with (Unpack's contract is to return an error on bytes that do
// not correctly encode a tuple; the old code returned -1 implicitly and the
// caller sliced b[1:0], a panic found by fuzzing).
func findTerminator(b []byte) int {
	bp := b
	var length int

	for {
		idx := bytes.IndexByte(bp, 0x00)
		if idx < 0 {
			return -1
		}
		length += idx
		if idx+1 == len(bp) || bp[idx+1] != 0xFF {
			break
		}
		length += 2
		bp = bp[idx+2:]
	}

	return length
}

func decodeBytes(b []byte) ([]byte, int, error) {
	idx := findTerminator(b[1:])
	if idx < 0 {
		return nil, 0, fmt.Errorf("no 0x00 terminator found for bytes/string element")
	}
	return bytes.Replace(b[1:idx+1], []byte{0x00, 0xFF}, []byte{0x00}, -1), idx + 2, nil
}

func decodeString(b []byte, strict bool) (string, int, error) {
	bp, idx, err := decodeBytes(b)
	if err != nil {
		return "", 0, err
	}
	if strict && !utf8.Valid(bp) {
		return "", 0, fmt.Errorf("malformed UTF-8 string")
	}
	return string(bp), idx, nil
}

func decodeInt(b []byte) (any, int, error) {
	if b[0] == intZeroCode {
		return int64(0), 1, nil
	}

	var neg bool

	n := int(b[0]) - intZeroCode
	if n < 0 {
		n = -n
		neg = true
	}

	if n+1 > len(b) {
		return nil, 0, fmt.Errorf("insufficient bytes to decode %d-byte integer", n)
	}

	// The big-endian n bytes right-aligned in eight, as binary.Read of an
	// int64 decodes them, without its buffer and reader allocations.
	var bp [8]byte
	copy(bp[8-n:], b[1:n+1])
	ret := int64(binary.BigEndian.Uint64(bp[:]))

	if neg {
		return ret - int64(sizeLimits[n]), n + 1, nil
	}

	if ret > 0 {
		return ret, n + 1, nil
	}

	// The encoded value claimed to be positive yet when put in an int64
	// produced a negative value. This means that the number must be a positive
	// 64-bit value that uses the most significant bit. This can be fit in a
	// uint64, so return that. Note that this is the *only* time we return
	// a uint64.
	return uint64(ret), n + 1, nil
}

func decodeBigInt(b []byte) (any, int, error) {
	val := new(big.Int)
	offset := 1
	var length int

	if b[0] == negIntStart || b[0] == posIntEnd {
		if len(b) < 2 {
			return nil, 0, fmt.Errorf("insufficient bytes to decode big integer length")
		}
		length = int(b[1])
		if b[0] == negIntStart {
			length ^= 0xff
		}

		offset += 1
	} else {
		// Must be a negative 8 byte integer
		length = 8
	}

	if length+offset > len(b) {
		return nil, 0, fmt.Errorf("insufficient bytes to decode %d-byte big integer", length)
	}

	val.SetBytes(b[offset : length+offset])

	if b[0] < intZeroCode {
		sub := new(big.Int).Lsh(big.NewInt(1), uint(length)*8)
		sub.Sub(sub, big.NewInt(1))
		val.Sub(val, sub)
	}

	// This is the only value that fits in an int64 or uint64 that is decoded with this function
	if val.Cmp(minInt64BigInt) == 0 {
		return val.Int64(), length + offset, nil
	}

	return val, length + offset, nil
}

func decodeFloat(b []byte) (float32, int) {
	bp := make([]byte, 4)
	copy(bp, b[1:])
	adjustFloatBytes(bp, false)
	var ret float32
	binary.Read(bytes.NewBuffer(bp), binary.BigEndian, &ret)
	return ret, 5
}

func decodeDouble(b []byte) (float64, int) {
	bp := make([]byte, 8)
	copy(bp, b[1:])
	adjustFloatBytes(bp, false)
	var ret float64
	binary.Read(bytes.NewBuffer(bp), binary.BigEndian, &ret)
	return ret, 9
}

func decodeUUID(b []byte) (UUID, int) {
	var u UUID
	copy(u[:], b[1:])
	return u, 17
}

func decodeVersionstamp(b []byte) (Versionstamp, int) {
	var transactionVersion [10]byte
	var userVersion uint16

	copy(transactionVersion[:], b[1:11])

	userVersion = binary.BigEndian.Uint16(b[11:])

	return Versionstamp{
		TransactionVersion: transactionVersion,
		UserVersion:        userVersion,
	}, versionstampLength + 1
}

func decodeTuple(b []byte, nested, strict bool) (Tuple, int, error) {
	var t Tuple

	var i int

	for i < len(b) {
		var el any
		var off int

		switch {
		case b[i] == nilCode:
			if !nested {
				el = nil
				off = 1
			} else if i+1 < len(b) && b[i+1] == 0xff {
				el = nil
				off = 2
			} else {
				return t, i + 1, nil
			}
		case b[i] == bytesCode:
			var err error
			el, off, err = decodeBytes(b[i:])
			if err != nil {
				return nil, i, fmt.Errorf("unable to decode bytes starting at position %d of byte array for tuple: %w", i, err)
			}
		case b[i] == stringCode:
			var err error
			el, off, err = decodeString(b[i:], strict)
			if err != nil {
				return nil, i, fmt.Errorf("unable to decode string starting at position %d of byte array for tuple: %w", i, err)
			}
		case negIntStart+1 < b[i] && b[i] < posIntEnd:
			var err error
			el, off, err = decodeInt(b[i:])
			if err != nil {
				return nil, i, fmt.Errorf("unable to decode integer starting at position %d of byte array for tuple: %w", i, err)
			}
		// The 8-byte-negative-int discriminator needs the byte AFTER the type
		// code; when the input ends at the type code, fall through to the
		// bigint case below, whose own bounds check reports the truncation
		// (indexing b[i+1] unguarded was a fuzz-found panic).
		case negIntStart+1 == b[i] && i+1 < len(b) && (b[i+1]&0x80 != 0):
			var err error
			el, off, err = decodeInt(b[i:])
			if err != nil {
				return nil, i, fmt.Errorf("unable to decode integer starting at position %d of byte array for tuple: %w", i, err)
			}
		case negIntStart <= b[i] && b[i] <= posIntEnd:
			var err error
			el, off, err = decodeBigInt(b[i:])
			if err != nil {
				return nil, i, fmt.Errorf("unable to decode big integer starting at position %d of byte array for tuple: %w", i, err)
			}
		case b[i] == floatCode:
			if i+5 > len(b) {
				return nil, i, fmt.Errorf("insufficient bytes to decode float starting at position %d of byte array for tuple", i)
			}
			el, off = decodeFloat(b[i:])
		case b[i] == doubleCode:
			if i+9 > len(b) {
				return nil, i, fmt.Errorf("insufficient bytes to decode double starting at position %d of byte array for tuple", i)
			}
			el, off = decodeDouble(b[i:])
		case b[i] == trueCode:
			el = true
			off = 1
		case b[i] == falseCode:
			el = false
			off = 1
		case b[i] == uuidCode:
			if i+17 > len(b) {
				return nil, i, fmt.Errorf("insufficient bytes to decode UUID starting at position %d of byte array for tuple", i)
			}
			el, off = decodeUUID(b[i:])
		case b[i] == versionstampCode:
			if i+versionstampLength+1 > len(b) {
				return nil, i, fmt.Errorf("insufficient bytes to decode Versionstamp starting at position %d of byte array for tuple", i)
			}
			el, off = decodeVersionstamp(b[i:])
		case b[i] == nestedCode:
			var err error
			el, off, err = decodeTuple(b[i+1:], true, strict)
			if err != nil {
				return nil, i, err
			}
			off++
		default:
			return nil, i, fmt.Errorf("unable to decode tuple element with unknown typecode %02x", b[i])
		}

		t = append(t, el)
		i += off
	}

	if nested && strict {
		return nil, i, fmt.Errorf("no 0x00 terminator found for nested tuple")
	}
	return t, i, nil
}

// Unpack returns the tuple encoded by the provided byte slice, or an error if
// the key does not correctly encode a FoundationDB tuple.
func Unpack(b []byte) (Tuple, error) {
	t, _, err := decodeTuple(b, false, false)
	return t, err
}

// DecodeError describes malformed input to UnpackStrict and preserves its cause.
type DecodeError struct {
	// Offset is the byte offset of the top-level element containing the failure.
	Offset int
	Cause  error
}

func (e *DecodeError) Error() string {
	return fmt.Sprintf("unable to unpack strict tuple at byte %d: %v", e.Offset, e.Cause)
}

func (e *DecodeError) Unwrap() error { return e.Cause }

// UnpackStrict requires nested terminators and valid UTF-8, returning *DecodeError
// for malformed input. It still accepts noncanonical integer encodings.
func UnpackStrict(b []byte) (Tuple, error) {
	t, offset, err := decodeTuple(b, false, true)
	if err != nil {
		return nil, &DecodeError{Offset: offset, Cause: err}
	}
	return t, nil
}
