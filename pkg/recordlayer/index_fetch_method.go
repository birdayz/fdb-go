// Portions derived from FoundationDB Record Layer (IndexOrphanBehavior.java,
// UnsupportedRemoteFetchIndexException.java, UnsupportedMethodException.java),
// Copyright Apple Inc. and the FoundationDB project authors. Licensed under the
// Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/indexfetch"
)

// IndexFetchMethod is Java's com.apple.foundationdb.record.IndexFetchMethod.
type IndexFetchMethod = indexfetch.Method

const (
	IndexFetchScanAndFetch               = indexfetch.ScanAndFetch
	IndexFetchUseRemoteFetch             = indexfetch.UseRemoteFetch
	IndexFetchUseRemoteFetchWithFallback = indexfetch.UseRemoteFetchWithFallback
)

// IndexOrphanBehavior says what an index-to-record scan does with an entry
// whose record does not exist. Matches Java's IndexOrphanBehavior.
type IndexOrphanBehavior int

const (
	// IndexOrphanError fails the scan with a RecordCoreStorageError.
	IndexOrphanError IndexOrphanBehavior = iota
	// IndexOrphanSkip drops the entry.
	IndexOrphanSkip
	// IndexOrphanReturn returns the entry with a nil record.
	IndexOrphanReturn
)

func (b IndexOrphanBehavior) String() string {
	switch b {
	case IndexOrphanError:
		return "ERROR"
	case IndexOrphanSkip:
		return "SKIP"
	case IndexOrphanReturn:
		return "RETURN"
	default:
		return fmt.Sprintf("IndexOrphanBehavior(%d)", int(b))
	}
}

// UnsupportedRemoteFetchIndexError is Java's UnsupportedRemoteFetchIndexException:
// the index maintainer, or the transaction's client, cannot fetch remotely.
type UnsupportedRemoteFetchIndexError struct {
	Message   string
	IndexName string
}

func (e *UnsupportedRemoteFetchIndexError) Error() string { return e.Message }

func (*UnsupportedRemoteFetchIndexError) JavaRecordCoreException() {}

// UnsupportedMethodError is Java's UnsupportedMethodException.
type UnsupportedMethodError struct{ Message string }

func (e *UnsupportedMethodError) Error() string { return e.Message }

func (*UnsupportedMethodError) JavaRecordCoreException() {}
