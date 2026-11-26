// Portions derived from FoundationDB Record Layer (
// TextIndexBunchedSerializer.java, bunchedSerializer.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import "fdb.dev/internal/recordlayer/rlcollections"

type (
	bunchedSerializer[K any, V any] = rlcollections.BunchedSerializer[K, V]
	BunchedEntry[K any, V any]      = rlcollections.BunchedEntry[K, V]
	TextIndexBunchedSerializer      = rlcollections.TextIndexBunchedSerializer
	BunchedSerializationError       = rlcollections.BunchedSerializationError
	BunchedMapException             = rlcollections.BunchedMapException
)

// TextIndexBunchedSerializerInstance returns the singleton serializer.
func TextIndexBunchedSerializerInstance() *TextIndexBunchedSerializer {
	return rlcollections.TextIndexBunchedSerializerInstance()
}
