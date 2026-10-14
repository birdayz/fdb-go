// Portions derived from FoundationDB Record Layer (IndexFetchMethod.java),
// Copyright Apple Inc. and the FoundationDB project authors. Licensed under the
// Apache License, Version 2.0; translated to Go and modified.

// Package indexfetch is Java's IndexFetchMethod, shared by the record layer's
// scans and the planner's index plans.
package indexfetch

import "fmt"

// Method selects how an index scan obtains the records it points to.
type Method int

const (
	// ScanAndFetch scans the index and loads each record separately.
	ScanAndFetch Method = iota
	// UseRemoteFetch has the storage server return each entry's record.
	UseRemoteFetch
	// UseRemoteFetchWithFallback uses remote fetch and falls back to
	// ScanAndFetch when remote fetch fails.
	UseRemoteFetchWithFallback
)

func (m Method) String() string {
	switch m {
	case ScanAndFetch:
		return "SCAN_AND_FETCH"
	case UseRemoteFetch:
		return "USE_REMOTE_FETCH"
	case UseRemoteFetchWithFallback:
		return "USE_REMOTE_FETCH_WITH_FALLBACK"
	default:
		return fmt.Sprintf("IndexFetchMethod(%d)", int(m))
	}
}
