// Package fdb provides a pure-Go client for FoundationDB.
//
// This package is API-compatible with the official Apple FDB Go binding
// (github.com/apple/foundationdb/bindings/go/src/fdb) but requires no
// C library (libfdb_c). It uses a native Go wire protocol implementation.
//
// Basic usage:
//
//	fdb.MustAPIVersion(730)
//	db := fdb.MustOpenDefault()
//	defer db.Close()
//
//	ret, err := db.Transact(func(tr fdb.Transaction) (interface{}, error) {
//	    tr.Set(fdb.Key("hello"), []byte("world"))
//	    return tr.Get(fdb.Key("foo")).MustGet(), nil
//	})
package fdb

import (
	"encoding/hex"
	"fmt"
)

// Key represents a FoundationDB key, a lexicographically-ordered sequence
// of bytes. Key implements the KeyConvertible interface.
type Key []byte

// FDBKey returns the key itself. Satisfies KeyConvertible.
func (k Key) FDBKey() Key { return k }

// String returns a human-readable representation of the key.
func (k Key) String() string {
	return Printable(k)
}

// KeyConvertible can be converted to a FoundationDB Key.
// All functions that address a specific key accept a KeyConvertible.
type KeyConvertible interface {
	FDBKey() Key
}

// KeyValue represents a single key-value pair in the database.
type KeyValue struct {
	Key   Key
	Value []byte
}

// Printable returns a human-readable representation of a byte slice,
// replacing non-printable characters with \x## escapes.
func Printable(b []byte) string {
	buf := make([]byte, 0, len(b))
	for _, c := range b {
		if c >= 32 && c < 127 && c != '\\' {
			buf = append(buf, c)
		} else {
			buf = append(buf, '\\', 'x')
			buf = append(buf, hex.EncodeToString([]byte{c})...)
		}
	}
	return string(buf)
}

// Strinc returns the first key that would sort after the given prefix.
// It is used to define the end of a prefix range: [prefix, Strinc(prefix)).
func Strinc(prefix []byte) ([]byte, error) {
	for i := len(prefix) - 1; i >= 0; i-- {
		if prefix[i] != 0xFF {
			out := make([]byte, i+1)
			copy(out, prefix[:i+1])
			out[i]++
			return out, nil
		}
	}
	return nil, fmt.Errorf("strinc: prefix is all 0xFF bytes")
}

// PrefixRange returns a KeyRange covering all keys with the given prefix.
func PrefixRange(prefix []byte) (KeyRange, error) {
	end, err := Strinc(prefix)
	if err != nil {
		return KeyRange{}, err
	}
	return KeyRange{Begin: Key(prefix), End: Key(end)}, nil
}
