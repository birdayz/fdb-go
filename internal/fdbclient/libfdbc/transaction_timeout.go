//go:build cgo && libfdbc

package libfdbc

/*
#define FDB_API_VERSION 730
#include <foundationdb/fdb_c.h>
*/
import "C"

import "encoding/binary"

// SetTimeout bounds operations on this raw C transaction using FDB's timeout
// option. Zero disables the timeout, as in the native client.
func (tr CTxnHandle) SetTimeout(milliseconds int64) error {
	if !tr.Valid() {
		return &CFDBError{Code: 2015, Msg: "transaction is closed"}
	}
	var value [8]byte
	binary.LittleEndian.PutUint64(value[:], uint64(milliseconds))
	p, n := cBytes(value[:])
	return cErr(C.fdb_transaction_set_option(tr.ptr, C.FDB_TR_OPTION_TIMEOUT, p, C.int(n)))
}
