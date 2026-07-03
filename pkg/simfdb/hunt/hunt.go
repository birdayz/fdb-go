// Package hunt is a brute-force, seed-driven bug hunter for the record layer.
//
// It drives a randomized save/overwrite/delete/deleteAll workload — under a seed-derived
// commit-fault schedule (commit_unknown / not_committed / transaction_too_old, via the Buggify
// points SimFDB's commit reads) — over SimFDB, the deterministic in-memory FDB backend, and
// after every batch checks the store against a chaos.StoreModel with chaos.Verify (the same
// 14-invariant oracle the fixed-seed chaos suite uses over real FDB).
//
// The point is throughput: SimFDB runs in-process with no Docker, so one core replays tens of
// thousands of independent seeds per second. That turns bug hunting into a loop — FuzzHunt
// (coverage-guided) and TestBruteHunt (time-budgeted) sweep an enormous permutation space
// "until a bug drops out", the brute-force complement to chaos.RunRandom (one fixed seed,
// real FDB, Docker). A found bug is a single uint64: Run(seed) replays it exactly and Shrink
// minimizes it. This is RFC-179 Tier 2.
package hunt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"sort"

	"fdb.dev/gen"
	"fdb.dev/pkg/dst"
	fdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/chaos"
	"fdb.dev/pkg/simfdb"

	"google.golang.org/protobuf/proto"
)

// opStream is the PCG stream for the workload PRNG, kept distinct from the Env's own streams
// (randStream feeds persisted-byte sites; buggifyStream feeds fault decisions) so the op
// sequence and the fault schedule are independent yet both reproducible from the one seed.
const opStream = uint64(7)

// Config parameterizes a single Run. The zero value is valid: withDefaults fills a sensible
// brute-force profile (a small keyspace so overwrites and conflicts are frequent, the full
// kitchen-sink index set, faults at FDB's 0.25 Buggify rate).
type Config struct {
	NumOps      int     // operations per run (default 300)
	MaxPKs      int64   // primary-key range [0,MaxPKs); smaller = more overwrite/conflict (default 30)
	VerifyEvery int     // run the oracle every N ops, plus once at the end (default 25)
	FaultProb   float64 // Buggify activation+fire probability for the commit faults (default 0.25; 0 = fault-free)

	// Metadata builds the schema + indexes under test. Nil uses the kitchen-sink schema
	// (VALUE + COUNT + SUM + RANK + MAX_EVER + VERSION + COVERING) so every maintainer is
	// exercised and Verify has maximal teeth.
	Metadata func() *recordlayer.RecordMetaData
}

func (c Config) withDefaults() Config {
	if c.NumOps <= 0 {
		c.NumOps = 300
	}
	if c.MaxPKs <= 0 {
		c.MaxPKs = 30
	}
	if c.VerifyEvery <= 0 {
		c.VerifyEvery = 25
	}
	if c.FaultProb == 0 {
		c.FaultProb = 0.25
	}
	if c.Metadata == nil {
		c.Metadata = KitchenSinkMetadata
	}
	return c
}

// Report is the outcome of one Run: clean, or a reproducible failure keyed by Seed.
type Report struct {
	Seed        uint64
	Ops         int      // operations executed before the run stopped
	FaultsFired int      // commit faults actually injected (0 = happy-path-only this seed)
	Violations  []string // oracle violations found (a store/model divergence)
	Err         string   // an unexpected harness error (an op that should never have failed)
	Fingerprint string   // sha256 of the final persisted keyspace (determinism probe); empty on failure
}

// Failed reports whether this run found a bug — an oracle violation or an unexpected error.
func (r *Report) Failed() bool { return r != nil && (len(r.Violations) > 0 || r.Err != "") }

func (r *Report) String() string {
	if r == nil {
		return "hunt: <nil report>"
	}
	if !r.Failed() {
		return fmt.Sprintf("hunt: seed=%d OK (%d ops, %d faults, fp=%s)", r.Seed, r.Ops, r.FaultsFired, r.Fingerprint)
	}
	s := fmt.Sprintf("hunt: seed=%d FAILED after %d ops (%d faults)", r.Seed, r.Ops, r.FaultsFired)
	if r.Err != "" {
		s += "\n  error: " + r.Err
	}
	for _, v := range r.Violations {
		s += "\n  violation: " + v
	}
	s += fmt.Sprintf("\n  reproduce: hunt.Run(%d, cfg)   // or bazelisk test //pkg/simfdb/hunt --test_arg=-test.run=TestHuntSeed --test_arg=HUNT_SEED=%d", r.Seed, r.Seed)
	return s
}

// Run replays seed deterministically and returns its Report. Never panics on a record-layer
// bug — it captures the divergence in the Report so a driver (fuzzer / brute loop) can decide
// how to surface it. Same (seed, cfg) always yields the same Report.
func Run(seed uint64, cfg Config) *Report {
	cfg = cfg.withDefaults()
	ctx := context.Background()

	env := dst.NewSim(seed)
	if cfg.FaultProb <= 0 {
		env.Buggify = dst.DisabledBuggifier()
	} else {
		env.Buggify.SetProbabilities(cfg.FaultProb, cfg.FaultProb)
	}

	md := cfg.Metadata()
	backend := simfdb.New(env)
	db := recordlayer.NewFDBDatabaseWithBackend(backend).SetEnv(env)
	// A fault-free view of the SAME backend for the oracle, so an injected fault can never
	// perturb verification (mirrors chaos.Scenario's clean-DB split). Read-only verify draws
	// nothing from the shared clock/randomness, so determinism is preserved.
	cleanEnv := &dst.Env{Clock: env.Clock, Random: env.Random, Buggify: dst.DisabledBuggifier()}
	cleanDB := recordlayer.NewFDBDatabaseWithBackend(backend).SetEnv(cleanEnv)

	d := &driver{
		db:      db,
		cleanDB: cleanDB,
		md:      md,
		sub:     subspace.FromBytes(tuple.Tuple{"hunt"}.Pack()),
		model:   chaos.NewStoreModel(md),
	}
	rng := rand.New(rand.NewPCG(seed, opStream))
	rep := &Report{Seed: seed}

	nextNewPK := int64(0)
	for i := 0; i < cfg.NumOps; i++ {
		op := pickOp(rng)
		// Fall back when the model can't satisfy the drawn op: an "existing" op on an empty
		// model becomes a new save; a "missing" delete on a full keyspace becomes an existing
		// delete. Identical to chaos.RunRandom so the two harnesses agree op-for-op.
		if (op == opSaveOverwrite || op == opDeleteExisting) && len(d.model.Records) == 0 {
			op = opSaveNew
		}
		if op == opDeleteMissing && int64(len(d.model.Records)) >= cfg.MaxPKs {
			op = opDeleteExisting
		}

		var err error
		switch op {
		case opSaveNew:
			pk := nextNewPK % cfg.MaxPKs
			nextNewPK++
			err = d.save(ctx, pk, rng.Int32N(10000), rng.Int32N(100))
		case opSaveOverwrite:
			err = d.save(ctx, pickExistingPK(rng, d.model), rng.Int32N(10000), rng.Int32N(100))
		case opDeleteExisting:
			err = d.delete(ctx, pickExistingPK(rng, d.model))
		case opDeleteMissing:
			err = d.delete(ctx, pickMissingPK(rng, d.model, cfg.MaxPKs))
		case opDeleteAll:
			err = d.deleteAll(ctx)
		}
		rep.Ops = i + 1
		if err != nil {
			// On this schema (no UNIQUE index) a save/delete has no legitimate domain error,
			// and faults are retried transparently by db.Run, so any surfaced error is a real
			// retry/idempotency failure — a bug the hunt just caught.
			rep.Err = fmt.Sprintf("op %d %s: %v", i, op, err)
			rep.FaultsFired = env.Buggify.Fired()
			return rep
		}

		if (i+1)%cfg.VerifyEvery == 0 {
			if v := d.verify(ctx); len(v) > 0 {
				rep.Violations = v
				rep.FaultsFired = env.Buggify.Fired()
				return rep
			}
		}
	}

	if v := d.verify(ctx); len(v) > 0 {
		rep.Violations = v
		rep.FaultsFired = env.Buggify.Fired()
		return rep
	}

	rep.FaultsFired = env.Buggify.Fired()
	rep.Fingerprint = d.fingerprint(ctx)
	return rep
}

// driver holds the record-layer plumbing for one run and keeps the shadow model in lockstep
// with the store: mutate through the faulted db, then — only on success — update the model.
type driver struct {
	db      *recordlayer.FDBDatabase
	cleanDB *recordlayer.FDBDatabase
	md      *recordlayer.RecordMetaData
	sub     subspace.Subspace
	model   *chaos.StoreModel
}

func (d *driver) open(rtx *recordlayer.FDBRecordContext) (*recordlayer.FDBRecordStore, error) {
	return recordlayer.NewStoreBuilder().
		SetContext(rtx).
		SetMetaDataProvider(d.md).
		SetSubspace(d.sub).
		CreateOrOpen()
}

func (d *driver) save(ctx context.Context, pk int64, price, qty int32) error {
	rec := &gen.Order{OrderId: proto.Int64(pk), Price: proto.Int32(price), Quantity: proto.Int32(qty)}
	_, err := d.db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := d.open(rtx)
		if err != nil {
			return nil, err
		}
		_, err = store.SaveRecord(rec)
		return nil, err
	})
	if err == nil {
		d.model.Save(rec)
	}
	return err
}

func (d *driver) delete(ctx context.Context, pk int64) error {
	_, err := d.db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := d.open(rtx)
		if err != nil {
			return nil, err
		}
		_, err = store.DeleteRecord(tuple.Tuple{pk})
		return nil, err
	})
	if err == nil {
		d.model.Delete(tuple.Tuple{pk})
	}
	return err
}

func (d *driver) deleteAll(ctx context.Context) error {
	_, err := d.db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := d.open(rtx)
		if err != nil {
			return nil, err
		}
		return nil, store.DeleteAllRecords()
	})
	if err == nil {
		d.model.DeleteAll()
	}
	return err
}

// verify opens the store through the fault-free view and returns any oracle violations as
// strings (empty = the store matches the model on every invariant).
func (d *driver) verify(ctx context.Context) []string {
	res, err := d.cleanDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := d.open(rtx)
		if err != nil {
			return nil, err
		}
		return chaos.Verify(store, d.model), nil
	})
	if err != nil {
		return []string{fmt.Sprintf("verify open failed: %v", err)}
	}
	violations, _ := res.([]chaos.Violation)
	if len(violations) == 0 {
		return nil
	}
	out := make([]string, len(violations))
	for i, v := range violations {
		out[i] = v.String()
	}
	return out
}

// fingerprint hashes the entire persisted keyspace, so two runs of the same seed can be
// asserted byte-identical (the determinism oracle) without shipping the whole dump around.
func (d *driver) fingerprint(ctx context.Context) string {
	h := sha256.New()
	_, _ = d.cleanDB.RunRead(ctx, func(rtx fdb.ReadTransaction) (any, error) {
		kvs := rtx.GetRange(fdb.KeyRange{Begin: fdb.Key{}, End: fdb.Key{0xff}}, fdb.RangeOptions{}).GetSliceOrPanic()
		for _, kv := range kvs {
			// length-prefix so key/value boundaries can't alias across entries
			var lp [8]byte
			putLen(lp[:4], len(kv.Key))
			putLen(lp[4:], len(kv.Value))
			h.Write(lp[:])
			h.Write(kv.Key)
			h.Write(kv.Value)
		}
		return nil, nil
	})
	return hex.EncodeToString(h.Sum(nil))
}

func putLen(b []byte, n int) {
	b[0] = byte(n >> 24)
	b[1] = byte(n >> 16)
	b[2] = byte(n >> 8)
	b[3] = byte(n)
}

// op is one workload action.
type op int

const (
	opSaveNew op = iota
	opSaveOverwrite
	opDeleteExisting
	opDeleteMissing
	opDeleteAll
)

func (o op) String() string {
	switch o {
	case opSaveNew:
		return "saveNew"
	case opSaveOverwrite:
		return "saveOverwrite"
	case opDeleteExisting:
		return "deleteExisting"
	case opDeleteMissing:
		return "deleteMissing"
	default:
		return "deleteAll"
	}
}

// Default operation weights, matching chaos.defaultWeights so the SimFDB brute hunt and the
// real-FDB chaos suite draw the same op mix: overwrite-heavy with rare deleteAll churn.
const (
	wSaveNew        = 30
	wSaveOverwrite  = 20
	wDeleteExisting = 20
	wDeleteMissing  = 5
	wDeleteAll      = 1
	wTotal          = wSaveNew + wSaveOverwrite + wDeleteExisting + wDeleteMissing + wDeleteAll
)

func pickOp(rng *rand.Rand) op {
	r := rng.IntN(wTotal)
	if r -= wSaveNew; r < 0 {
		return opSaveNew
	}
	if r -= wSaveOverwrite; r < 0 {
		return opSaveOverwrite
	}
	if r -= wDeleteExisting; r < 0 {
		return opDeleteExisting
	}
	if r -= wDeleteMissing; r < 0 {
		return opDeleteMissing
	}
	return opDeleteAll
}

func pickExistingPK(rng *rand.Rand, model *chaos.StoreModel) int64 {
	pks := make([]int64, 0, len(model.Records))
	for _, rec := range model.Records {
		pks = append(pks, rec.PrimaryKey[0].(int64))
	}
	sort.Slice(pks, func(i, j int) bool { return pks[i] < pks[j] })
	return pks[rng.IntN(len(pks))]
}

func pickMissingPK(rng *rand.Rand, model *chaos.StoreModel, maxPKs int64) int64 {
	for {
		pk := rng.Int64N(maxPKs)
		if !model.Has(tuple.Tuple{pk}) {
			return pk
		}
	}
}

// KitchenSinkMetadata builds the Order schema with all seven index types active at once
// (VALUE + COUNT + SUM + RANK + MAX_EVER + VERSION + COVERING) plus record counting and
// record versions — the maximal-coverage schema, so a single run exercises every index
// maintainer's fault/retry path and Verify has teeth on all of them.
func KitchenSinkMetadata() *recordlayer.RecordMetaData {
	b := recordlayer.NewRecordMetaDataBuilder()
	b.SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	b.SetRecordCountKey(recordlayer.EmptyKey())
	b.SetStoreRecordVersions(true)
	b.AddIndex("Order", recordlayer.NewIndex("hunt_price_idx", recordlayer.Field("price")))
	b.AddIndex("Order", recordlayer.NewCountIndex("hunt_count",
		recordlayer.GroupAll(recordlayer.Field("price"))))
	b.AddIndex("Order", recordlayer.NewSumIndex("hunt_sum",
		recordlayer.Ungrouped(recordlayer.Field("price"))))
	b.AddIndex("Order", recordlayer.NewRankIndex("hunt_rank", recordlayer.Field("price")))
	b.AddIndex("Order", recordlayer.NewMaxEverLongIndex("hunt_maxever",
		recordlayer.Ungrouped(recordlayer.Field("price"))))
	b.AddIndex("Order", recordlayer.NewVersionIndex("hunt_version", recordlayer.VersionKey()))
	b.AddIndex("Order", recordlayer.NewIndex("hunt_covering",
		recordlayer.KeyWithValue(
			recordlayer.Concat(recordlayer.Field("price"), recordlayer.Nest("flower", recordlayer.Field("type"))),
			1)))
	md, err := b.Build()
	if err != nil {
		panic("hunt: failed to build kitchen-sink metadata: " + err.Error())
	}
	return md
}
