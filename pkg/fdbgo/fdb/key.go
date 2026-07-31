// Package fdb provides a pure-Go client for FoundationDB.
//
// This package is API-compatible with the official Apple FDB Go binding
// (fdb.dev/pkg/fdbgo/fdb) but requires no
// C library (libfdb_c). It uses a native Go wire protocol implementation.
//
// Basic usage:
//
//	fdb.MustAPIVersion(730)
//	db := fdb.MustOpenDefault()
//	defer db.Close()
//
//	ret, err := db.Transact(func(tr fdb.WritableTransaction) (interface{}, error) {
//	    tr.Set(fdb.Key("hello"), []byte("world"))
//	    return tr.Get(fdb.Key("foo")).MustGet(), nil
//	})
//
// Known behavioral differences from the Apple C binding:
//   - Error messages: Error.Error() returns "FoundationDB error: <code>" rather
//     than the human-readable description from libfdb_c. Use Error.Code for
//     programmatic matching.
//   - Future.Cancel() is a no-op — the underlying operation runs to completion.
//   - No per-transaction context.Context: matching the Apple binding, methods
//     like Get/GetRange do not accept a context parameter. Use SetTimeout for
//     deadlines, or call Cancel() from another goroutine for cancellation.
//     context.Background() is used internally for all operations.
//
// Retries and timeouts are UNBOUNDED by default here — exactly as in libfdb_c,
// whose per-transaction defaults are timeoutInSeconds=0.0 and maxRetries=-1
// (ReadYourWrites.actor.cpp:2078-2082; fdb.options on the timeout option: "If set
// to 0, will disable all timeouts"). A bare Transact against a down cluster
// retries until the cluster returns, and the no-context Transact runs on
// context.Background(), so nothing internal stops it. This is a matched default,
// NOT a divergence — but it does mean every caller must choose a bound:
// SetTimeout (the analog of C++ `timebomb`, and like it, it cancels in-flight RPC
// waits — RFC-112), SetRetryLimit, or a deadline ctx via TransactCtx (Go's extra
// bound, RFC-090). Bootstrap is the one place this client is STRICTER than
// libfdb_c: OpenDatabase bounds the initial coordinator connection at 60s where
// libfdb_c waits forever. See the package doc at fdb.dev/pkg/fdbgo.
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
		} else if c == '\\' {
			buf = append(buf, '\\', '\\') // Apple: backslash → \\
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
	begin := make([]byte, len(prefix))
	copy(begin, prefix)
	return KeyRange{Begin: Key(begin), End: Key(end)}, nil
}
