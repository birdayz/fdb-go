// Package chaos provides model-based chaos testing for the FDB Record Layer.
//
// The framework maintains an in-memory model (a simple map) that shadows
// the real FDB store. Operations are applied to both; after each operation,
// the framework verifies they agree. Disagreement = bug.
//
// Faults (commit-unknown, conflicts, timeouts) are injected via ChaosTransactor,
// which wraps fdb.Transactor. Seeded PRNG ensures reproducibility.
package chaos

import (
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/recordlayer"
)

// StoreModel is a trivially simple in-memory shadow of the record store.
// It tracks which records exist and their content. It IS the specification:
// if the real store disagrees with the model, the store has a bug.
type StoreModel struct {
	Records  map[string]*ModelRecord // pk.Pack() -> record
	metadata *recordlayer.RecordMetaData

	// CountUpdates tracks cumulative insert+update events per grouping key
	// for COUNT_UPDATES indexes. Key: indexName + ":" + packedGroupingKey.
	// COUNT_UPDATES ignores deletes and never decrements.
	CountUpdates map[string]int64

	// MaxEver tracks the maximum value ever seen per (indexName, groupingKey).
	// Key: indexName + ":" + packedGroupingKey. EVER semantics: only ratchets up,
	// individual deletes are no-ops. Reset on DeleteAll (store clears index data).
	MaxEver map[string]int64

	// MinEver tracks the minimum value ever seen per (indexName, groupingKey).
	// Key: indexName + ":" + packedGroupingKey. EVER semantics: only ratchets down,
	// individual deletes are no-ops. Reset on DeleteAll (store clears index data).
	MinEver map[string]int64

	// minEverInitialized tracks which keys have been set at least once,
	// because the zero value of int64 is a valid MIN_EVER value.
	minEverInitialized map[string]bool
}

// ModelRecord tracks a single record in the model.
type ModelRecord struct {
	PrimaryKey tuple.Tuple
	TypeName   string
	Message    proto.Message
}

// NewStoreModel creates a new empty model.
func NewStoreModel(metadata *recordlayer.RecordMetaData) *StoreModel {
	return &StoreModel{
		Records:            make(map[string]*ModelRecord),
		metadata:           metadata,
		CountUpdates:       make(map[string]int64),
		MaxEver:            make(map[string]int64),
		MinEver:            make(map[string]int64),
		minEverInitialized: make(map[string]bool),
	}
}

// Save adds or overwrites a record in the model.
// Extracts the record type name and primary key from the proto message
// using the metadata's record type definitions.
func (m *StoreModel) Save(msg proto.Message) {
	typeName := string(msg.ProtoReflect().Descriptor().Name())
	rt := m.metadata.GetRecordType(typeName)
	if rt == nil {
		panic("chaos: unknown record type: " + typeName)
	}

	pkValues, err := rt.PrimaryKey.Evaluate(nil, msg)
	if err != nil {
		panic("chaos: pk evaluation failed: " + err.Error())
	}

	pk := make(tuple.Tuple, len(pkValues[0]))
	for i, v := range pkValues[0] {
		pk[i] = v
	}

	key := string(pk.Pack())

	// Track index-specific model state.
	for _, idx := range m.metadata.GetAllIndexes() {
		if !m.indexAppliesToType(idx, typeName) {
			continue
		}

		switch idx.Type {
		case recordlayer.IndexTypeCountUpdates:
			groupingKeys := m.evaluateGroupingKeys(idx, msg)
			for _, gk := range groupingKeys {
				cuKey := idx.Name + ":" + gk
				m.CountUpdates[cuKey]++
			}
		case recordlayer.IndexTypeMaxEverLong:
			entries := m.evaluateMinMaxEntries(idx, msg)
			for _, e := range entries {
				k := idx.Name + ":" + e.groupKey
				if cur, ok := m.MaxEver[k]; !ok || e.value > cur {
					m.MaxEver[k] = e.value
				}
			}
		case recordlayer.IndexTypeMinEverLong:
			entries := m.evaluateMinMaxEntries(idx, msg)
			for _, e := range entries {
				k := idx.Name + ":" + e.groupKey
				if !m.minEverInitialized[k] || e.value < m.MinEver[k] {
					m.MinEver[k] = e.value
					m.minEverInitialized[k] = true
				}
			}
		}
	}

	m.Records[key] = &ModelRecord{
		PrimaryKey: pk,
		TypeName:   typeName,
		Message:    proto.Clone(msg),
	}
}

// Delete removes a record from the model. No-op if not found.
func (m *StoreModel) Delete(pk tuple.Tuple) {
	delete(m.Records, string(pk.Pack()))
}

// DeleteAll removes all records from the model.
// Resets EVER tracking too — the store's DeleteAllRecords clears index data.
func (m *StoreModel) DeleteAll() {
	m.Records = make(map[string]*ModelRecord)
	m.CountUpdates = make(map[string]int64)
	m.MaxEver = make(map[string]int64)
	m.MinEver = make(map[string]int64)
	m.minEverInitialized = make(map[string]bool)
}

// Count returns the total number of records in the model.
func (m *StoreModel) Count() int64 {
	return int64(len(m.Records))
}

// Has returns true if a record with the given PK exists in the model.
func (m *StoreModel) Has(pk tuple.Tuple) bool {
	_, ok := m.Records[string(pk.Pack())]
	return ok
}

// indexAppliesToType checks if an index applies to a given record type.
func (m *StoreModel) indexAppliesToType(idx *recordlayer.Index, typeName string) bool {
	for _, ridx := range m.metadata.GetIndexesForRecordType(typeName) {
		if ridx.Name == idx.Name {
			return true
		}
	}
	for _, uidx := range m.metadata.GetUniversalIndexes() {
		if uidx.Name == idx.Name {
			return true
		}
	}
	return false
}

// minMaxModelEntry holds a grouping key and value for MIN/MAX_EVER model tracking.
type minMaxModelEntry struct {
	groupKey string // packed grouping tuple
	value    int64
}

// evaluateMinMaxEntries evaluates a MIN/MAX_EVER index expression against a message
// and returns (groupingKey, value) pairs. Skips nil values and negative values.
func (m *StoreModel) evaluateMinMaxEntries(idx *recordlayer.Index, msg proto.Message) []minMaxModelEntry {
	gke, ok := idx.RootExpression.(*recordlayer.GroupingKeyExpression)
	if !ok {
		return nil
	}
	groupingCount := gke.GetGroupingCount()
	tuples, err := gke.Evaluate(nil, msg)
	if err != nil {
		return nil
	}
	var result []minMaxModelEntry
	for _, values := range tuples {
		if groupingCount >= len(values) {
			continue // no aggregated column
		}
		rawValue := values[groupingCount]
		if rawValue == nil {
			continue
		}
		var val int64
		switch v := rawValue.(type) {
		case int64:
			val = v
		case int32:
			val = int64(v)
		case int:
			val = int64(v)
		default:
			continue
		}
		if val < 0 {
			continue // negative values rejected by the maintainer
		}
		gk := string(tuple.Tuple{}.Pack())
		if groupingCount > 0 {
			t := make(tuple.Tuple, groupingCount)
			for j := 0; j < groupingCount && j < len(values); j++ {
				t[j] = values[j]
			}
			gk = string(t.Pack())
		}
		result = append(result, minMaxModelEntry{groupKey: gk, value: val})
	}
	return result
}

// evaluateGroupingKeys evaluates an index's grouping expression against a message
// and returns the packed grouping key strings. For COUNT_UPDATES tracking.
func (m *StoreModel) evaluateGroupingKeys(idx *recordlayer.Index, msg proto.Message) []string {
	gke, ok := idx.RootExpression.(*recordlayer.GroupingKeyExpression)
	if !ok {
		return nil
	}
	groupingCount := gke.GetGroupingCount()
	if groupingCount == 0 {
		// Ungrouped — single empty key
		return []string{string(tuple.Tuple{}.Pack())}
	}
	// Evaluate the whole key, then take the first groupingCount columns.
	tuples, err := gke.Evaluate(nil, msg)
	if err != nil {
		return nil
	}
	keys := make([]string, len(tuples))
	for i, values := range tuples {
		t := make(tuple.Tuple, groupingCount)
		for j := 0; j < groupingCount && j < len(values); j++ {
			t[j] = values[j]
		}
		keys[i] = string(t.Pack())
	}
	return keys
}
