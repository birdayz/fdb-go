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
type IndexFetchMethod int

const (
	// IndexFetchScanAndFetch scans the index and loads each record separately.
	IndexFetchScanAndFetch IndexFetchMethod = iota
	// IndexFetchUseRemoteFetch has the storage server return each entry's record.
	IndexFetchUseRemoteFetch
	// IndexFetchUseRemoteFetchWithFallback uses remote fetch and falls back to
	// ScanAndFetch when remote fetch fails.
	IndexFetchUseRemoteFetchWithFallback
)

func (m IndexFetchMethod) String() string {
	switch m {
	case IndexFetchScanAndFetch:
		return "SCAN_AND_FETCH"
	case IndexFetchUseRemoteFetch:
		return "USE_REMOTE_FETCH"
	case IndexFetchUseRemoteFetchWithFallback:
		return "USE_REMOTE_FETCH_WITH_FALLBACK"
	default:
		return fmt.Sprintf("IndexFetchMethod(%d)", int(m))
	}
}

func (m IndexFetchMethod) toInternal() indexfetch.Method {
	switch m {
	case IndexFetchScanAndFetch:
		return indexfetch.ScanAndFetch
	case IndexFetchUseRemoteFetch:
		return indexfetch.UseRemoteFetch
	case IndexFetchUseRemoteFetchWithFallback:
		return indexfetch.UseRemoteFetchWithFallback
	default:
		return indexfetch.Method(m)
	}
}

func indexFetchMethodFromInternal(m indexfetch.Method) IndexFetchMethod {
	switch m {
	case indexfetch.ScanAndFetch:
		return IndexFetchScanAndFetch
	case indexfetch.UseRemoteFetch:
		return IndexFetchUseRemoteFetch
	case indexfetch.UseRemoteFetchWithFallback:
		return IndexFetchUseRemoteFetchWithFallback
	default:
		return IndexFetchMethod(m)
	}
}

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
