// Portions derived from FoundationDB Record Layer (FDBRecordContext.java,
// APIVersion.java), Copyright Apple Inc. and the FoundationDB project authors.
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

// APIVersion is the FDB API version this context's transactions run at, or 0
// if none was selected. Matches Java's FDBRecordContext.getAPIVersion.
func (c *FDBRecordContext) APIVersion() int { return selectedAPIVersion() }

// SupportsRemoteFetch reports whether apiVersion is at least Java's
// APIVersion.API_VERSION_7_1, the first with mapped reads.
func SupportsRemoteFetch(apiVersion int) bool { return apiVersion >= remoteFetchMinAPIVersion }
