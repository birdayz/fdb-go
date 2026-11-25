// Portions derived from FoundationDB Record Layer (BuiltInFunctionCatalog.java,
// UnableToPlanException.java, ExpressionVisitor.java, GroupByExpression.java,
// and others),
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2023 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2026 Apple Inc. and the FoundationDB project authors
// Copyright 2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

// Package seedcorpus holds the conformance corpora loaded from embedded JSON fixtures.
package seedcorpus

import (
	"slices"
	"sync"

	"fdb.dev/pkg/relational/conformance/plandiff"
)

var cachedEmbeddedCorpus = sync.OnceValues(func() ([]plandiff.RunQuery, error) {
	return LoadEmbedded()
})

// SeedRunCorpus returns a fresh deep copy of the baseline RunQuery corpus
// loaded from the sharded embedded testdata JSON fixtures.
func SeedRunCorpus() []plandiff.RunQuery {
	corpus, err := cachedEmbeddedCorpus()
	if err != nil {
		panic(err)
	}
	return cloneRunQueries(corpus)
}

// cloneRunQueries returns independent mutable copies so parallel callers
// (e.g. ApplyRFC082Divergences) never race on shared slices or Divergence pointers.
func cloneRunQueries(src []plandiff.RunQuery) []plandiff.RunQuery {
	out := make([]plandiff.RunQuery, len(src))
	for i, q := range src {
		q.SetupSqls = slices.Clone(q.SetupSqls)
		if q.Divergence != nil {
			d := *q.Divergence
			if q.Divergence.GoExpectedRows != nil {
				rows := make([][]any, len(q.Divergence.GoExpectedRows))
				for rIdx, row := range q.Divergence.GoExpectedRows {
					if row == nil {
						continue
					}
					rowCopy := make([]any, len(row))
					for cIdx, cell := range row {
						rowCopy[cIdx] = cloneCell(cell)
					}
					rows[rIdx] = rowCopy
				}
				d.GoExpectedRows = rows
			}
			q.Divergence = &d
		}
		out[i] = q
	}
	return out
}

func cloneCell(v any) any {
	switch c := v.(type) {
	case []byte:
		return slices.Clone(c)
	case []any:
		if c == nil {
			return []any(nil)
		}
		out := make([]any, len(c))
		for i, elem := range c {
			out[i] = cloneCell(elem)
		}
		return out
	default:
		return v
	}
}
