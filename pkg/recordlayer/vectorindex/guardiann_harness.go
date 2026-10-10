// Portions derived from FoundationDB Record Layer (Guardiann.java),
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package vectorindex

import (
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// GuardiannEngine is one GuardiANN structure over a raw subspace, Java's
// com.apple.foundationdb.async.guardiann.Guardiann, for cross-engine
// conformance: it drives the engine the record layer's vector maintainer
// drives, with no record store around it. options are the GuardiANN index
// options (the canonical index option names), so the configuration is read
// exactly as a GUARDIANN index's is.
type GuardiannEngine struct{ g *guardiann }

// NewGuardiannEngine builds the engine over ss from index options.
func NewGuardiannEngine(ss subspace.Subspace, options map[string]string) (*GuardiannEngine, error) {
	index := &recordlayer.Index{Name: "guardiann_engine", Type: recordlayer.IndexTypeVector, Options: map[string]string{}}
	for k, v := range options {
		index.Options[k] = v
	}
	config, err := parseGuardiannConfig(index)
	if err != nil {
		return nil, err
	}
	return &GuardiannEngine{g: newGuardiann(ss, config, nil, nil)}, nil
}

// GuardiannVectorIDHashMapOrder is the order a default
// HashMap<VectorId, ...> filled with these (primary key, UUID) ids in list
// order iterates them in (javaHashMapOrder), for cross-engine conformance.
func GuardiannVectorIDHashMapOrder(pks []tuple.Tuple, uuids []tuple.UUID) []int {
	ids := make([]guardiannVectorID, len(pks))
	for i := range pks {
		ids[i] = guardiannVectorID{pk: pks[i], uuid: uuids[i]}
	}
	return javaHashMapOrder(ids)
}

// Insert is Guardiann.insert(transaction, primaryKey, DoubleRealVector, null,
// maintainInTransaction).
func (e *GuardiannEngine) Insert(tx fdb.WritableTransaction, pk tuple.Tuple, vector []float64, maintainInTransaction bool) error {
	return e.g.insert(tx, pk, gVector{data: vector, typ: 2}, nil, maintainInTransaction)
}

// Delete is Guardiann.delete(transaction, primaryKey, DoubleRealVector,
// maintainInTransaction).
func (e *GuardiannEngine) Delete(tx fdb.WritableTransaction, pk tuple.Tuple, vector []float64, maintainInTransaction bool) error {
	return e.g.delete(tx, pk, gVector{data: vector, typ: 2}, maintainInTransaction)
}

// Search is Guardiann.kNearestNeighborsSearch(transaction, k, the default
// SearchConfig, false, DoubleRealVector): the primary keys found, nearest
// first.
func (e *GuardiannEngine) Search(tx fdb.ReadTransaction, k int, vector []float64) ([]tuple.Tuple, error) {
	results, err := e.g.search(tx, k, defaultGuardiannSearchConfig(), gVector{data: vector, typ: 2})
	if err != nil {
		return nil, err
	}
	pks := make([]tuple.Tuple, len(results))
	for i, r := range results {
		pks[i] = r.primaryKey
	}
	return pks, nil
}

// ExecuteDeferredTasks is Guardiann.executeDeferredTasks(transaction,
// numTasks, Long.MAX_VALUE).
func (e *GuardiannEngine) ExecuteDeferredTasks(tx fdb.WritableTransaction, numTasks int) (int, error) {
	return e.g.executeDeferredTasks(tx, numTasks, time.Time{}) // the zero deadline is none
}
