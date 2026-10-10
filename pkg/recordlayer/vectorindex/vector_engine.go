// Portions derived from FoundationDB Record Layer (VectorIndexEngineKind.java),
// Copyright 2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package vectorindex

import (
	"strings"

	"fdb.dev/pkg/recordlayer"
)

// VectorEngineKind is Java's VectorIndexEngineKind.
type VectorEngineKind int

const (
	VectorEngineHNSW VectorEngineKind = iota
	VectorEngineGuardiann
)

func (k VectorEngineKind) String() string {
	if k == VectorEngineGuardiann {
		return "GUARDIANN"
	}
	return "HNSW"
}

// VectorEngineOf is VectorIndexEngineKind.fromOptionValue over the index's
// vectorEngine option: case-insensitive, unknown values refused.
func VectorEngineOf(index *recordlayer.Index) (VectorEngineKind, error) {
	v, ok := index.Options[recordlayer.IndexOptionVectorEngine]
	if !ok {
		return VectorEngineHNSW, nil
	}
	switch strings.ToUpper(v) {
	case "HNSW":
		return VectorEngineHNSW, nil
	case "GUARDIANN":
		return VectorEngineGuardiann, nil
	}
	// Java logs the value as a log key; its message is the text alone
	// (VectorIndexEngineKind.java:57).
	return VectorEngineHNSW, &recordlayer.MetaDataError{Message: "unknown vector index engine"}
}
