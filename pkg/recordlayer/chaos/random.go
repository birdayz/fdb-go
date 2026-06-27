package chaos

import (
	"sort"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// RandomConfig controls the random operation generator.
type RandomConfig struct {
	// Seed for the PRNG. Same seed = same operations = same result.
	Seed uint64

	// NumOps is the total number of operations to execute.
	NumOps int

	// Faults controls fault injection (nil = no faults).
	Faults *FaultConfig

	// VerifyEvery controls how often Verify() is called.
	// Default: 50 (every 50 operations).
	VerifyEvery int

	// MaxPKs bounds the primary key range [0, MaxPKs) for higher overwrite rates.
	// Default: 50.
	MaxPKs int64

	// Weights overrides the default operation weights. Nil uses defaults.
	Weights *OpWeights
}

// OpWeights controls the relative probability of each operation type.
type OpWeights struct {
	SaveNew        int // Save with a PK not in the model
	SaveOverwrite  int // Save with a PK already in the model
	DeleteExisting int // Delete a PK that exists in the model
	DeleteMissing  int // Delete a PK that does NOT exist in the model
	DeleteAll      int // Delete all records
}

// defaultWeights returns the standard operation weights.
func defaultWeights() *OpWeights {
	return &OpWeights{
		SaveNew:        30,
		SaveOverwrite:  20,
		DeleteExisting: 20,
		DeleteMissing:  5,
		DeleteAll:      1,
	}
}

// opType identifies a random operation.
type opType int

const (
	opSaveNew opType = iota
	opSaveOverwrite
	opDeleteExisting
	opDeleteMissing
	opDeleteAll
)

// RunRandom executes a random sequence of operations against the store,
// comparing against the model periodically. Same seed = same sequence = same result.
//
// Returns the final Scenario so callers can inspect model state, fault logs, etc.
func RunRandom(t testing.TB, realDB fdb.Database, metadata *recordlayer.RecordMetaData, cfg RandomConfig) *Scenario {
	t.Helper()

	// Apply defaults.
	if cfg.VerifyEvery <= 0 {
		cfg.VerifyEvery = 50
	}
	if cfg.MaxPKs <= 0 {
		cfg.MaxPKs = 50
	}
	if cfg.NumOps <= 0 {
		cfg.NumOps = 100
	}
	weights := cfg.Weights
	if weights == nil {
		weights = defaultWeights()
	}
	faults := cfg.Faults
	if faults == nil {
		faults = FaultsNone
	}

	// Build the weighted operation table.
	totalWeight := weights.SaveNew + weights.SaveOverwrite +
		weights.DeleteExisting + weights.DeleteMissing + weights.DeleteAll
	if totalWeight <= 0 {
		t.Fatal("chaos: RunRandom: total weight must be > 0")
	}

	// Create scenario.
	var opts []Option
	opts = append(opts, WithSeed(cfg.Seed), WithFaults(faults))
	s := NewScenario(t, realDB, metadata, opts...)

	// Track the next PK to use for "new" saves. This counter only goes up,
	// guaranteeing the PK is fresh (never been used before).
	nextNewPK := int64(0)

	for i := 0; i < cfg.NumOps; i++ {
		op := pickOp(s, weights, totalWeight)

		// If we want an "existing" operation but the model is empty,
		// fall back to a new save.
		if (op == opSaveOverwrite || op == opDeleteExisting) && len(s.model.Records) == 0 {
			op = opSaveNew
		}
		// If we want a "missing" delete but ALL PKs in range are taken,
		// just delete an existing one instead.
		if op == opDeleteMissing && int64(len(s.model.Records)) >= cfg.MaxPKs {
			op = opDeleteExisting
		}

		switch op {
		case opSaveNew:
			// Pick a PK not in the model.
			pk := nextNewPK % cfg.MaxPKs
			nextNewPK++
			// If this PK happens to already exist (wrapped around), it
			// becomes an overwrite — that's fine, still valid.
			price := s.Rng.Int32N(10000)
			qty := s.Rng.Int32N(100)
			s.SaveRecord(&gen.Order{
				OrderId:  proto.Int64(pk),
				Price:    proto.Int32(price),
				Quantity: proto.Int32(qty),
			})

		case opSaveOverwrite:
			// Pick a random existing PK from the model.
			pk := pickExistingPK(s)
			price := s.Rng.Int32N(10000)
			qty := s.Rng.Int32N(100)
			s.SaveRecord(&gen.Order{
				OrderId:  proto.Int64(pk),
				Price:    proto.Int32(price),
				Quantity: proto.Int32(qty),
			})

		case opDeleteExisting:
			pk := pickExistingPK(s)
			s.DeleteRecord(tuple.Tuple{pk})

		case opDeleteMissing:
			// Pick a PK not in the model.
			pk := pickMissingPK(s, cfg.MaxPKs)
			s.DeleteRecord(tuple.Tuple{pk})

		case opDeleteAll:
			s.DeleteAllRecords()
		}

		if (i+1)%cfg.VerifyEvery == 0 {
			s.Verify()
		}
	}

	// Final verification.
	s.Verify()

	t.Logf("chaos: RunRandom completed %d ops (seed=%d, maxPKs=%d, faults=%d)",
		cfg.NumOps, cfg.Seed, cfg.MaxPKs, len(s.FaultLog()))

	return s
}

// pickOp selects a random operation based on weights.
func pickOp(s *Scenario, w *OpWeights, totalWeight int) opType {
	r := s.Rng.IntN(totalWeight)

	r -= w.SaveNew
	if r < 0 {
		return opSaveNew
	}
	r -= w.SaveOverwrite
	if r < 0 {
		return opSaveOverwrite
	}
	r -= w.DeleteExisting
	if r < 0 {
		return opDeleteExisting
	}
	r -= w.DeleteMissing
	if r < 0 {
		return opDeleteMissing
	}
	return opDeleteAll
}

// pickExistingPK returns a random primary key from the model's current records.
// The model must be non-empty. Uses sorted PK list for deterministic selection
// (Go map iteration order is non-deterministic).
func pickExistingPK(s *Scenario) int64 {
	pks := sortedModelPKs(s)
	return pks[s.Rng.IntN(len(pks))]
}

// sortedModelPKs returns all PKs in the model, sorted for deterministic selection.
func sortedModelPKs(s *Scenario) []int64 {
	pks := make([]int64, 0, len(s.model.Records))
	for _, rec := range s.model.Records {
		pks = append(pks, rec.PrimaryKey[0].(int64))
	}
	sort.Slice(pks, func(i, j int) bool { return pks[i] < pks[j] })
	return pks
}

// pickMissingPK returns a random PK in [0, maxPKs) that is NOT in the model.
// Caller must ensure len(model.Records) < maxPKs.
func pickMissingPK(s *Scenario, maxPKs int64) int64 {
	for {
		pk := s.Rng.Int64N(maxPKs)
		if !s.model.Has(tuple.Tuple{pk}) {
			return pk
		}
	}
}
