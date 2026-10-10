// Portions derived from FoundationDB Record Layer (MatchPartition.java),
// Copyright 2015-2020 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package cascades

// MatchPartition groups PartialMatch instances for a specific
// expression and reference. Simple container.
//
// Ports Java's com.apple.foundationdb.record.query.plan.cascades.MatchPartition.
type MatchPartition struct {
	partialMatches []PartialMatch
}

// NewMatchPartition constructs a MatchPartition from the given
// matches. The slice is defensively copied.
func NewMatchPartition(matches []PartialMatch) *MatchPartition {
	cp := make([]PartialMatch, len(matches))
	copy(cp, matches)
	return &MatchPartition{partialMatches: cp}
}

// GetPartialMatches returns the partial matches in this partition.
func (p *MatchPartition) GetPartialMatches() []PartialMatch {
	return p.partialMatches
}
