// Portions derived from FoundationDB Record Layer (MessageHelpers.java,
// QueryResult.java, IndexOrphanBehavior.java, RecordCursor.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"fmt"

	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
)

// comparisonKeyPrograms specializes a comparison-key program per distinct stream
// ROW SHAPE instead of per row.
//
// comparisonKeyProgramForRow's answer depends on the row only through its Type and
// its selected Layout — every other input is the plan's own key list — and a merge
// presents ONE fixed pair per LEG, two or three for a whole cursor. Specializing
// per row therefore rebuilt the same two or three programs for every row that
// flowed: measured on a 2000-group aggregate merge, 37 allocations per group, the
// largest single item in that path. The programs are immutable value trees, so one
// answer serves every row of its leg.
//
// NOT SYNCHRONIZED, and it does not need to be TODAY: a cursor's OnNext is driven
// sequentially by its consumer and no executor cursor advances its children on
// another goroutine, so the read and the append below are single-threaded per
// cursor.
//
// KNOW THAT JAVA'S MERGE CURSOR IS THE INVERSE OF THIS CONTRACT. Java's
// MergeCursor.onNext builds CompletableFuture.allOf over every leg's onNext, and
// KeyedMergeCursorState applies the comparison-key function INSIDE each leg's
// per-completion lambda — so Java computes leg comparison keys on different threads,
// concurrently. Porting that async model is what will break this, and the symptom is
// not a crash: an unsynchronized append racing a read gives a torn program slice, a
// wrong comparison key, and wrong merge/intersection ROWS. Whoever ports it must give
// each puller its own instance rather than lock this one — the entries are per-leg,
// so a lock would serialise the row path to protect a list of at most
// comparisonKeyProgramCap entries.
//
// Keyed on the two POINTERS rather than on type equality, which is strictly finer:
// a leg whose row type is a fresh object per row simply rebuilds, as it did before.
// The entries are a list scanned linearly — a merge has as many as it has legs —
// and the list STOPS GROWING at a small cap so that a producer minting a fresh
// type per row cannot turn this into a leak; past the cap every row rebuilds,
// which is exactly the old behaviour.
type comparisonKeyPrograms struct {
	entries []comparisonKeyProgram
}

type comparisonKeyProgram struct {
	layout values.OrdinalLayout
	typ    *values.RecordType
	keys   []values.Value
	edges  []values.QuantifiedObjectValue
}

const comparisonKeyProgramCap = 8

func (c *comparisonKeyPrograms) forRow(
	keyVals []values.Value,
	row *PositionalRow,
) ([]values.Value, []values.QuantifiedObjectValue, error) {
	if row == nil {
		return comparisonKeyProgramForRow(keyVals, row)
	}
	for i := range c.entries {
		if c.entries[i].layout == row.Layout && c.entries[i].typ == row.Type {
			return c.entries[i].keys, c.entries[i].edges, nil
		}
	}
	keys, edges, err := comparisonKeyProgramForRow(keyVals, row)
	if err != nil {
		return nil, nil, err
	}
	if len(c.entries) < comparisonKeyProgramCap {
		c.entries = append(c.entries, comparisonKeyProgram{
			layout: row.Layout, typ: row.Type, keys: keys, edges: edges,
		})
	}
	return keys, edges, nil
}

// comparisonKeyProgramForRow specializes a comparison-key program to one
// concrete stream row. Multi-intersection uses one key program for child rows
// whose grouping prefix agrees but whose aggregate payload field differs. The
// plan-time program is rooted in the first child's exact row type; rebuilding
// it against a sibling's exact row type is therefore required before declaring
// the sibling edge. Checked translation preserves the resolved accessor paths
// and fails if the sibling changes any field the key actually reads.
func comparisonKeyProgramForRow(
	keyVals []values.Value,
	row *PositionalRow,
) ([]values.Value, []values.QuantifiedObjectValue, error) {
	roots := comparisonKeyRoots(keyVals)
	edges := comparisonKeyInputEdges(keyVals)
	if len(roots) == 0 || row == nil || row.Type == nil || row.Layout == nil {
		return keyVals, edges, nil
	}
	rebuilt := append([]values.Value(nil), keyVals...)
	// Current roots are exact layout handles, not merely the reserved alias plus
	// a compatible type. A comparison program can have been built against an
	// independently admitted current carrier; reanchor that handle to the row's
	// actual selected layout before evaluation.
	for _, root := range roots {
		if root.Correlation() != values.CurrentCorrelation() {
			continue
		}
		for i := range rebuilt {
			var err error
			rebuilt[i], err = values.TranslatePhaseRoot(rebuilt[i], root, row.Layout.Carrier())
			if err != nil {
				return nil, nil, fmt.Errorf("key %d current carrier: %w", i, err)
			}
		}
	}
	edges = comparisonKeyInputEdges(rebuilt)
	needsRebuild := false
	for _, edge := range edges {
		// Shared graph: compared against the row shape and never retained.
		if !values.SharedFlowedType(edge).Equals(row.Type) {
			needsRebuild = true
			break
		}
	}
	if !needsRebuild {
		return rebuilt, edges, nil
	}

	targets := make(map[values.CorrelationIdentifier]values.QuantifiedObjectValue, len(edges))
	builder := values.NewTranslationMapBuilder()
	for _, edge := range edges {
		if _, exists := targets[edge.Correlation()]; exists {
			continue
		}
		target, err := values.NewQuantifiedObjectValue(edge.Correlation(), row.Type)
		if err != nil {
			return nil, nil, err
		}
		targets[edge.Correlation()] = target
		targetForAlias := target
		builder.When(edge.Correlation()).Then(func(_ values.CorrelationIdentifier, leaf values.Value) values.Value {
			if _, ok := values.AsQuantifiedObjectValue(leaf); ok {
				return targetForAlias
			}
			return leaf
		})
	}
	translation := builder.Build()
	typeRebuilt := make([]values.Value, len(rebuilt))
	for i, key := range rebuilt {
		var err error
		typeRebuilt[i], err = values.TranslateCorrelationsChecked(key, translation)
		if err != nil {
			return nil, nil, fmt.Errorf("key %d: %w", i, err)
		}
	}
	rebuiltEdges := make([]values.QuantifiedObjectValue, 0, len(targets))
	for _, edge := range edges {
		if target, exists := targets[edge.Correlation()]; exists {
			rebuiltEdges = append(rebuiltEdges, target)
			delete(targets, edge.Correlation())
		}
	}
	return typeRebuilt, rebuiltEdges, nil
}

func comparisonKeyRoots(keyVals []values.Value) []values.QuantifiedObjectValue {
	var result []values.QuantifiedObjectValue
	for _, key := range keyVals {
		values.WalkValue(key, func(node values.Value) bool {
			qov, ok := values.AsQuantifiedObjectValue(node)
			if !ok {
				return true
			}
			for _, previous := range result {
				if previous == qov {
					return true
				}
			}
			result = append(result, qov)
			return true
		})
	}
	return result
}

// comparisonKeyInputEdges returns the exact QOV roots owned by one executable
// comparison-key program. Aggregate-index intersection keys intentionally use
// a synthetic row QOV rather than one child quantifier's alias: the same
// program is evaluated against every child stream. Once a row carries an
// admitted OrdinalLayout, ambient positional fallback is forbidden, so these
// roots must be declared explicitly as edges to the comparison-key phase.
func comparisonKeyInputEdges(keyVals []values.Value) []values.QuantifiedObjectValue {
	var result []values.QuantifiedObjectValue
	seen := make(map[values.CorrelationIdentifier][]values.Type)
	for _, key := range keyVals {
		values.WalkValue(key, func(node values.Value) bool {
			qov, ok := values.AsQuantifiedObjectValue(node)
			if !ok {
				return true
			}
			// Current is the layout carrier itself, not a quantifier edge. The
			// OrdinalLayout binder owns this reserved root and newEdgeObjectBinder
			// deliberately rejects attempts to forge it as an edge declaration.
			if qov.Correlation() == values.CurrentCorrelation() {
				return true
			}
			// Shared graphs: the type is only compared here and the map dies with
			// the call. FlowedType would thaw a fresh graph per QOV node — and this
			// walk runs once per ROW of every merge stream, so on a 2000-group
			// aggregate merge it was thawing 35 Type graphs per group to answer a
			// question about types that do not change.
			flowed := values.SharedFlowedType(qov)
			for _, typ := range seen[qov.Correlation()] {
				if typ.Equals(flowed) {
					return true
				}
			}
			seen[qov.Correlation()] = append(seen[qov.Correlation()], flowed)
			result = append(result, qov)
			return true
		})
	}
	return result
}
