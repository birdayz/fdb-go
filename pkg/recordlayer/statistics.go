package recordlayer

// Collected planner statistics (RFC-236).
//
// Statistics are gathered by an OFFLINE job — the same shape as
// RebalanceSPFreshIndex and OnlineIndexer, which this library already ships —
// and read at plan time behind a per-connection opt-in. The collector is allowed
// to SCAN, which is the whole reason this design works where two earlier ones
// failed: FDB's sampled range-size estimator is accurate above ~100KB and
// returns 0 for a non-empty range below it, so a small table is invisible to it
// exactly when its smallness is the most valuable thing a join-order decision
// could know (measured in estimated_range_size_probe_test.go and
// per_type_size_estimate_probe_test.go).
//
// WHAT IS STORED, AND WHERE IT IS NOT. Statistics live OUTSIDE the record
// store's subspace. That namespace belongs to Java: FDBRecordStoreKeyspace
// defines 0-10 and is @API(UNSTABLE) — it has already grown once past this
// port's original transcription (see constants.go). Taking a prefix inside a
// store is not a question of whether Java reads it; it is a collision waiting
// for an upstream release to claim the number.
//
// A store IS its subspace prefix, whatever produced it — a relational schema, a
// hand-built store, a Java-authored layout — so that prefix is the key. Nothing
// here knows about the SQL layer, and nothing needs to.
//
// FDB TENANTS are separate keyspaces, so a store's prefix is only meaningful
// within its tenant: two tenants may hold byte-identical prefixes for entirely
// different stores. The caller supplies the root, and a tenant-scoped caller
// must supply a root inside that tenant. Getting this backwards makes tenant A's
// statistics describe tenant B's tables, and the symptom is a bad plan rather
// than an error.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// StatisticsFormatVersion is written with every entry so a reader can refuse a
// layout it does not understand rather than misparse one. Bump it when the
// tuple below changes shape; readers reject anything they do not recognise.
const StatisticsFormatVersion = 1

// statisticsHeaderKey names the per-store header entry. It is a distinct tuple
// element rather than a reserved record-type name so it can never collide with
// a real type, whatever a schema calls its tables.
var statisticsHeaderKey = tuple.Tuple{int64(0)}

// RecordTypeStatistic is one collected per-record-type statistic.
//
// Count is COUNTED, not sampled. That is the point of collecting offline: it
// removes the floor, the quantization and the bytes-to-rows conversion that make
// plan-time sampling unusable for small tables.
//
// It is EXACT for a store that is not being mutated under the scan, and only
// then. Collection spans transactions -- it must, since a full scan cannot fit
// in one -- and each batch is a consistent snapshot of its own, not of the run.
// So a record whose PRIMARY KEY MOVES concurrently, from an already-scanned key
// to a later one, is observed by both batches and counted twice; the reverse
// move is observed by neither and counted zero times. Ordinary inserts and
// deletes are not a problem in the same way -- they are simply seen or not,
// which is the usual meaning of "as of collection time" -- but a key move can
// make the total differ from any count the store ever actually had.
//
// This is not fixable by scanning harder. A run-wide snapshot needs one read
// version across the whole store, and FDB expires a read version in 5s -- the
// same limit that forces the batching. The honest statement is therefore the
// narrower one, and it is enough for the purpose: a count that is exact at rest
// and near-exact under churn still separates a 150-row table from a 1,000,000-row
// one, which is the decision this feature exists to inform, and it is a class
// better than the estimator it replaced.
type RecordTypeStatistic struct {
	// Count is the number of records of this type observed by the scan. Exact
	// for a store at rest; see the type comment for what concurrent key moves do.
	Count int64
	// CollectedAtVersion is the read version the count was taken at.
	CollectedAtVersion int64
	// CollectedAtUnixNanos is the collector's wall clock at collection.
	// Used for expiry; see StatisticsReader.
	CollectedAtUnixNanos int64
}

// StoreStatistics is everything collected for one record store.
type StoreStatistics struct {
	// PerType maps record type name to its statistic. A type ABSENT here has
	// no statistic — which is a normal outcome, never an implied zero.
	PerType map[string]RecordTypeStatistic
	// CollectedAtVersion / CollectedAtUnixNanos are the run's own stamps, from
	// the header entry.
	CollectedAtVersion   int64
	CollectedAtUnixNanos int64
}

// StatisticsSubspace addresses collected statistics for record stores under one
// root. The root MUST be outside every record store's subspace, and for a
// tenant-scoped deployment must be inside the tenant.
type StatisticsSubspace struct {
	root subspace.Subspace
}

// NewStatisticsSubspace returns statistics addressing rooted at root.
func NewStatisticsSubspace(root subspace.Subspace) StatisticsSubspace {
	return StatisticsSubspace{root: root}
}

// forStore returns the subspace holding one store's statistics, keyed by that
// store's subspace prefix bytes. Layout-agnostic by construction: a store is
// its prefix, however the prefix was derived.
func (s StatisticsSubspace) forStore(storeSubspace subspace.Subspace) subspace.Subspace {
	return s.root.Sub(storeSubspace.Bytes())
}

// packStatistic encodes one statistic. The format version leads so a reader can
// reject an unknown layout before interpreting anything after it.
func packStatistic(st RecordTypeStatistic) []byte {
	return tuple.Tuple{
		int64(StatisticsFormatVersion),
		st.Count,
		st.CollectedAtVersion,
		st.CollectedAtUnixNanos,
	}.Pack()
}

// unpackStatistic decodes one statistic. ok=false means the bytes are absent,
// malformed, or a format this build does not understand — all of which are
// "no statistic", never a guessed one.
func unpackStatistic(b []byte) (RecordTypeStatistic, bool) {
	if len(b) == 0 {
		return RecordTypeStatistic{}, false
	}
	t, err := tuple.Unpack(b)
	if err != nil || len(t) != 4 {
		return RecordTypeStatistic{}, false
	}
	version, ok := t[0].(int64)
	if !ok || version != StatisticsFormatVersion {
		return RecordTypeStatistic{}, false
	}
	count, ok1 := t[1].(int64)
	ver, ok2 := t[2].(int64)
	nanos, ok3 := t[3].(int64)
	if !ok1 || !ok2 || !ok3 {
		return RecordTypeStatistic{}, false
	}
	// A negative count is not a small table; it is corruption. Refuse rather
	// than hand the cost model a number that would invert its comparisons.
	if count < 0 {
		return RecordTypeStatistic{}, false
	}
	return RecordTypeStatistic{
		Count:                count,
		CollectedAtVersion:   ver,
		CollectedAtUnixNanos: nanos,
	}, true
}

// CollectOptions tunes a collection run.
type CollectOptions struct {
	// BatchSize is the maximum number of records scanned per transaction.
	// Collection is continuation-driven, so a large store costs many small
	// transactions rather than one big one.
	//
	// It is a ROW bound and therefore NOT what keeps a transaction inside FDB's
	// 5s limit: a record can be a hundred bytes or, split across KV pairs,
	// hundreds of kilobytes, so a fixed row count is anywhere from ~100KB to
	// hundreds of megabytes of reading. TimeLimit and ScannedBytesLimit are the
	// bounds that hold whatever the records weigh; a batch ends at whichever of
	// the three comes first.
	BatchSize int
	// MaxRecordsPerType ABORTS the collection as soon as any one record type
	// exceeds this many rows. Nothing is stored, and the report names the type
	// that blew the budget. Zero means no cap.
	//
	// Abort rather than skip-and-continue, because skipping cannot produce a
	// usable outcome. A type with no entry fails the reader's schema-wide
	// completeness gate, so the OTHER types' counts are refused along with it —
	// the old behaviour bought a full scan and an unusable result, which is
	// strictly worse than not collecting. Aborting reaches the same end state
	// having read a bounded prefix, and says which type to look at.
	//
	// It also cannot bound work any other way: collection is a SINGLE PASS over
	// all records (a per-type scan needs a record-type-prefixed primary key,
	// which not every layout has), so there is no way to keep scanning while
	// skipping one type's rows.
	MaxRecordsPerType int64
	// TimeLimit bounds how long ONE scan transaction spends reading before it
	// stops and hands back a continuation. Zero means DefaultCollectTimeLimit.
	//
	// This, not BatchSize, is what keeps a transaction inside FDB's 5s limit.
	// BatchSize bounds ROWS, and a row is not a fixed number of bytes.
	TimeLimit time.Duration
	// ScannedBytesLimit bounds how many bytes ONE scan transaction reads before
	// it stops and hands back a continuation. Zero means
	// DefaultCollectScannedBytesLimit.
	//
	// Both bounds are drivable from a test, and neither is a wall-clock bound:
	// every elapsed-time decision goes through ScanLimiterState.Elapsed, which is
	// env.Since, so TimeLimit runs on the simulation clock when one is installed.
	// An earlier version of this comment claimed TimeLimit was nondeterministic
	// and used that to justify leaving its arm undriven -- the claim was false and
	// the gap it excused was real; both arms now have a test.
	ScannedBytesLimit int64
	// Tags are FDB transaction tags applied to EVERY transaction this collection
	// opens — each scan batch and the replacing write.
	//
	// EmbeddedConnection.CollectStatistics OVERWRITES this with the connection's
	// own tags: at that layer the connection owns them, and a caller passing
	// CollectOptions should not have to know they exist. Callers of this package
	// directly (the fleet fan-out, tests) set it themselves and it is honoured.
	//
	// Threaded explicitly rather than wrapped around the database, because a
	// wrapper has to reconstruct an *FDBDatabase and this package's copy-method
	// gate forbids the field-by-field form for good reason: the first attempt
	// dropped env, which silently swaps a persisted timestamp's seeded clock for
	// the wall clock. A parameter is visible at every call site; a dropped field
	// is visible nowhere.
	Tags []string
}

// DefaultCollectTimeLimit is the per-batch time bound when CollectOptions.TimeLimit
// is zero. It leaves room under FDB's 5s transaction limit for the read-version
// fetch and the commit around the scan itself.
//
// Exported so an operator-facing description of collection can RENDER the bound
// rather than restate it: a help text carrying a hand-typed "3s" goes stale the
// first time this changes, and says so to the one reader who cannot check it.
const DefaultCollectTimeLimit = 3 * time.Second

// DefaultCollectScannedBytesLimit is the per-batch read bound when
// CollectOptions.ScannedBytesLimit is zero. Exported for the same reason as
// DefaultCollectTimeLimit. Generous enough that
// an ordinary batch never reaches it, small enough that a batch of split
// multi-hundred-KB records stops long before the time limit would have to.
const DefaultCollectScannedBytesLimit int64 = 16 << 20

func (o CollectOptions) batchSize() int {
	if o.BatchSize <= 0 {
		return 1000
	}
	return o.BatchSize
}

func (o CollectOptions) timeLimit() time.Duration {
	if o.TimeLimit <= 0 {
		return DefaultCollectTimeLimit
	}
	return o.TimeLimit
}

func (o CollectOptions) scannedBytesLimit() int64 {
	if o.ScannedBytesLimit <= 0 {
		return DefaultCollectScannedBytesLimit
	}
	return o.ScannedBytesLimit
}

// CollectionReport describes one run.
type CollectionReport struct {
	// Collected is the per-type statistics written.
	Collected map[string]RecordTypeStatistic
	// Skipped maps a record type to why it has NO statistic. Present so a
	// caller can tell "not collected" from "collected as zero" — the two are
	// different facts and only one of them is a table with no rows.
	Skipped map[string]string
	// RecordsScanned is the total records read across all types.
	RecordsScanned int64
}

// CollectStatistics scans a record store and writes per-record-type counts into
// stats -- exact for a store at rest; see RecordTypeStatistic for what concurrent
// primary-key moves do to the total.
//
// It is an OFFLINE maintenance job, not a query path: it reads every record. The
// signature mirrors RebalanceSPFreshIndex so it composes with the maintainers
// already in this library.
//
// Counting is a SINGLE PASS over all records, tallying by type, rather than one
// scan per type. That is deliberate: a per-type scan is only cheaper when the
// primary key carries a record-type prefix, and this must work for every store
// layout, including the ones that do not. One pass is uniform and correct
// everywhere.
func CollectStatistics(
	ctx context.Context,
	db *FDBDatabase,
	storeBuilder func(*FDBRecordContext) (*FDBRecordStore, error),
	stats StatisticsSubspace,
	opts CollectOptions,
) (*CollectionReport, error) {
	if db == nil || storeBuilder == nil {
		return nil, fmt.Errorf("CollectStatistics: db and storeBuilder are required")
	}

	counts := make(map[string]int64)
	var scanned int64
	var continuation []byte
	var storeSubspace subspace.Subspace
	var collectedAtVersion int64
	var declaredTypes map[string]*RecordType
	// cappedTypes is the set that blew the cap. It is merged from a per-attempt
	// set after Run returns, exactly like the counters — an earlier revision
	// wrote it inside the closure on the argument that a retry re-derives the
	// same membership, which holds only if the rows do not change underneath.
	// Same discipline, no separate argument to be wrong about.
	cappedTypes := make(map[string]struct{})

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// PER-ATTEMPT accumulators. db.Run RETRIES its closure — that is the
		// whole point of a transactor — and a batch that trips
		// transaction_too_old after tallying most of its rows re-runs from the
		// same continuation and re-reads them. Tallying straight into the
		// durable counters would then add those rows twice.
		//
		// It fails in the worst possible direction: a retry is likeliest on the
		// LONGEST batches, so the inflation lands preferentially on the biggest
		// tables — the ones a join-order decision is most sensitive to — and it
		// is silent, because an inflated count is a perfectly well-formed number
		// that every gate downstream passes through.
		//
		// So each attempt accumulates into its own map, RESET at the top of the
		// closure, and the merge below happens only after Run returns without
		// error, i.e. exactly once per committed batch.
		batchDone := false
		var batchCounts map[string]int64
		var batchScanned int64
		// batchVersion is per-attempt for the same reason the counters are: a
		// retried attempt's reads are DISCARDED, so its read version describes no
		// observation in the finished set. Merging only after Run returns keeps the
		// minimum over versions that actually contributed data.
		var batchVersion int64
		var batchContinuation []byte
		var batchCapped map[string]struct{}
		res, err := db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			if err := applyTags(rtx.Transaction(), opts.Tags); err != nil {
				return nil, err
			}
			batchCounts = make(map[string]int64)
			batchScanned = 0
			batchVersion = 0
			batchContinuation = continuation
			batchCapped = make(map[string]struct{})
			batchDone = false
			store, sErr := storeBuilder(rtx)
			if sErr != nil {
				return nil, sErr
			}
			if storeSubspace == nil {
				storeSubspace = store.Subspace()
			}
			// The outer variables written inside this closure split into two
			// groups with different safety arguments. They are ENUMERATED, not
			// counted, and not left to a grep. A list can be checked against the
			// code by reading it; every attempt here to state the same thing as a
			// number or a command has drifted instead — naming three while seven
			// are written, counting assignments and missing the increments, and two
			// counting commands that each absorbed something they should not have.
			// One of those was a write-only `batch` counter that nothing read; it
			// is deleted now, which is the actual lesson. The arithmetic was never
			// the thing worth pinning; the grouping is.
			//
			// FIVE are the per-attempt accumulators (batchCounts, batchScanned,
			// batchContinuation, batchDone, batchVersion). They are assigned here
			// BECAUSE they are reset here; that reset is the fix.
			//
			// batchVersion joined them when the run stopped being stamped by its
			// LAST batch and started being stamped by its OLDEST. As an overwrite it
			// was idempotent; as a minimum it is an accumulator, and a retried
			// attempt's version describes reads that were discarded. Merging after
			// Run returns keeps the minimum over versions that contributed data.
			//
			// TWO are not accumulators — storeSubspace above and declaredTypes on
			// the next line — and they are safe under retry by IDEMPOTENCE: each is
			// an overwrite with a value the same attempt would produce again. That is
			// a weaker property than the accumulators get, so it is stated rather
			// than glossed.
			//
			// What is checkable by reading is the narrower claim that matters:
			// no DURABLE counter is touched in here. Counters ARE incremented —
			// batchScanned and batchCounts, two lines of the scan loop — but they
			// are the per-attempt locals reset at the top, and the MERGE into the
			// durable counts runs only after Run returns without error. So a retry
			// cannot add anything twice, which is the failure this structure exists
			// to prevent and the one idempotence would not have covered.
			//
			// The merge is what carries that, not the seeding further down: seeding
			// assigns zero to declared types the scan never saw, so it cannot
			// double anything and is not part of this argument.
			// REFUSE UNMODELED SYNTHETIC TYPES HERE, not only at the SQL and fleet
			// boundaries. RecordTypes() deliberately OMITS joined and unnested
			// declarations -- this port carries them opaquely -- so for such metadata
			// the line below returns a PARTIAL view, and a run over it writes a header
			// vouching for a set that can never be complete. The reader below the
			// relational layer has no completeness gate to catch that.
			//
			// The two callers inside this repo already refuse it before opening a
			// store, which is cheaper and says so earlier. This is the check that
			// makes the invariant belong to the COLLECTOR rather than to whoever
			// happens to call it -- the same reason the entry stamps are set by the
			// writer instead of trusted from its caller.
			if md := store.GetRecordMetaData(); md.DeclaresSyntheticRecordTypes() {
				return nil, &SyntheticRecordTypesNotModeledError{
					TypeNames: md.SyntheticRecordTypeNames(),
				}
			}
			declaredTypes = store.GetRecordMetaData().RecordTypes()
			// The read version of the LAST batch stamps the run. Collection
			// spans transactions, so no single version describes all of it;
			// this one bounds how recent the newest reading is.
			//
			// Propagated, not swallowed, and the argument is stronger here than
			// on the read side: this version is PERSISTED. Swallowing it stamps
			// the entry with 0 or with a previous batch's version, the freshness
			// gate then refuses the schema on every plan, and the operator sees
			// a silent refusal instead of the cluster's actual error.
			v, vErr := rtx.ReadTransaction(true).GetReadVersion().Get()
			if vErr != nil {
				return nil, vErr
			}
			// THE OLDEST batch stamps the run, not the newest.
			//
			// Collection spans transactions, so no single version describes all of
			// it. Recording the LAST one dates the set by its most recent reading,
			// which makes the whole set look as fresh as its freshest part -- so a
			// long collection can hold counts already past the freshness bound while
			// the reader treats the set as good for another full window.
			//
			// The oldest is the conservative direction: age is then judged from the
			// stalest observation in the set, so the gate expires it no later than
			// the truth and never later than it should.
			batchVersion = v

			// BOUND THE BATCH BY TIME AND BYTES, NOT ONLY BY ROWS.
			//
			// A row limit alone bounds nothing an FDB transaction cares about: a
			// record can be a hundred bytes or, split across KV pairs, hundreds of
			// kilobytes, so the default 1000-row batch is anywhere from ~100KB to
			// hundreds of megabytes. The large end exceeds the 5s limit, db.Run
			// retries it, and it exceeds the limit again -- so collection fails on
			// exactly the stores whose size makes it worth collecting.
			//
			// FailOnScanLimitReached stays false (the default), so reaching either
			// limit STOPS the cursor with a continuation rather than erroring. The
			// loop below already routes HasStoppedBeforeEnd through that
			// continuation, so a bounded batch resumes in the next transaction and
			// the total stays exact.
			//
			// Built from DefaultExecutePropertiesIn, never a bare struct literal.
			// The zero value gets three separate behaviours by accident: ScanState
			// nil (the seeded-simulation seam -- see ScanState's doc comment, which
			// names the raw-literal case exactly), StreamingModeSmall (the slowest
			// mode, on a job that is nothing but a full-store scan), and snapshot
			// isolation.
			//
			// Snapshot is right here, so it is set EXPLICITLY rather than inherited:
			// a serializable full-store scan adds a read conflict range over the
			// whole store, so any concurrent write would abort a collection that is
			// by design an offline job running against a live store. Counting is a
			// read; it must not make writers fail.
			props := ScanProperties{
				ExecuteProperties: DefaultExecutePropertiesIn(rtx.Env()).
					WithIsolationLevel(IsolationLevelSnapshot).
					WithReturnedRowLimit(opts.batchSize()).
					WithTimeLimit(opts.timeLimit()).
					WithScannedBytesLimit(opts.scannedBytesLimit()),
				CursorStreamingMode: StreamingModeIterator,
			}
			cur := store.ScanRecords(batchContinuation, props)
			defer func() { _ = cur.Close() }()

			for {
				r, cErr := cur.OnNext(ctx)
				if cErr != nil {
					return nil, cErr
				}
				if !r.HasNext() {
					// No next: either the scan is exhausted or the batch limit
					// stopped it. HasStoppedBeforeEnd distinguishes them, and
					// conflating the two would silently truncate the count.
					if !r.HasStoppedBeforeEnd() {
						batchDone = true
						return nil, nil
					}
					c, bErr := r.GetContinuation().ToBytes()
					if bErr != nil {
						return nil, bErr
					}
					if c == nil {
						batchDone = true
					}
					batchContinuation = c
					return nil, nil
				}
				rec := r.GetValue()
				if rec != nil && rec.RecordType != nil {
					name := rec.RecordType.Name
					// The cap has to fire DURING the scan or it bounds nothing.
					// Applying it only to the finished tally — which is what this
					// did — reads and decodes every row of a million-row type and
					// then throws the number away, so the knob documented as
					// limiting work limited only the output.
					//
					// Once a type is over, it is ABANDONED: no further counting,
					// and it lands in Skipped. The count is then meaningless and
					// is not reported, which is the same contract as before —
					// absent, never partial — reached without the work.
					batchCounts[name]++
					if opts.MaxRecordsPerType > 0 &&
						counts[name]+batchCounts[name] > opts.MaxRecordsPerType {
						// Stop here: nothing collected after this point can be
						// used, so reading it is pure cost.
						batchCapped[name] = struct{}{}
						batchScanned++
						return nil, nil
					}
				}
				batchScanned++
			}
		})
		_ = res
		if err != nil {
			return nil, err
		}
		// The batch committed: fold its tally in exactly once.
		for name, c := range batchCounts {
			counts[name] += c
		}
		scanned += batchScanned
		if batchVersion != 0 && (collectedAtVersion == 0 || batchVersion < collectedAtVersion) {
			collectedAtVersion = batchVersion
		}
		for name := range batchCapped {
			cappedTypes[name] = struct{}{}
		}
		continuation = batchContinuation
		if len(cappedTypes) > 0 {
			// Aborted. Store nothing: a partial pass has partial counts for every
			// type it had not finished, and writing those would be worse than the
			// capped type's absence — a wrong number for a table nobody capped.
			break
		}
		if batchDone {
			break
		}
	}

	if len(cappedTypes) > 0 {
		report := &CollectionReport{
			Collected:      map[string]RecordTypeStatistic{},
			Skipped:        map[string]string{},
			RecordsScanned: scanned,
		}
		for name := range cappedTypes {
			report.Skipped[name] = fmt.Sprintf(
				"exceeds MaxRecordsPerType (%d); collection aborted and stored nothing",
				opts.MaxRecordsPerType)
		}
		return report, nil
	}

	// SEED EVERY DECLARED TYPE AT ZERO.
	//
	// Counting only what the scan observed would leave a declared type with no
	// rows ABSENT — and the reader requires every declared type to be present,
	// so ONE empty table would refuse statistics for the whole schema,
	// permanently, until somebody inserted a row. A freshly created schema is
	// mostly empty tables, so the feature would be off exactly where it had just
	// been switched on.
	//
	// An exact 0 from a full scan is as trustworthy as an exact 5, and it is not
	// a hazard downstream: NewCollectedStatistics clamps a count below 1 up to 1,
	// so an empty table costs as a one-row table rather than collapsing every
	// cost above it to zero. ABSENT stays reserved for "not counted" — a capped
	// type — which is a different fact from "counted, and there were none".
	for name := range declaredTypes {
		if _, seen := counts[name]; !seen {
			counts[name] = 0
		}
	}

	// The cap is applied DURING the scan and aborts the run above, so nothing
	// here can be over it — this loop only turns finished tallies into the
	// report.
	report := &CollectionReport{
		Collected:      make(map[string]RecordTypeStatistic),
		Skipped:        map[string]string{},
		RecordsScanned: scanned,
	}
	for name, c := range counts {
		report.Collected[name] = RecordTypeStatistic{
			Count:              c,
			CollectedAtVersion: collectedAtVersion,
		}
	}

	if storeSubspace == nil {
		// Unreachable: the scan loop runs at least once and its first act is to
		// open the store, which sets this; a failure to open returns an error
		// from db.Run instead of arriving here.
		//
		// It is an ERROR rather than the early return it used to be, because that
		// return reported SUCCESS having persisted nothing. That is the same shape
		// as the capped run which used to exit 0 while storing nothing, and
		// automation reads it as a completed refresh. An unreachable branch that
		// fails silently is worth less than one that fails loudly.
		return nil, fmt.Errorf(
			"internal: collection finished without resolving the store subspace, so nothing was persisted")
	}
	nowNanos, wErr := writeStatistics(ctx, db, stats, storeSubspace, report, collectedAtVersion, opts.Tags)
	if wErr != nil {
		return nil, wErr
	}
	// Stamp the returned report from what was actually PERSISTED, so a caller
	// reading report.Collected sees the same instant a reader will.
	for name, st := range report.Collected {
		st.CollectedAtUnixNanos = nowNanos
		report.Collected[name] = st
	}
	return report, nil
}

// writeStatistics replaces a store's statistics atomically: the previous set is
// cleared and the new one written in ONE transaction, so a reader never sees a
// half-updated mixture of two runs. A mixture would be worse than either, since
// counts from different versions are not comparable.
//
// THE TIMESTAMP IS DRAWN HERE, from the DST env rather than time.Now, and that
// is a requirement rather than a style: these bytes are PERSISTED, so a raw
// wall-clock read makes a seeded simulation run unreplayable (RFC-199 Tier 0;
// the seam gate in pkg/docscheck enforces it). Env() is nil-safe and falls back
// to real time in production.
//
// Stamping here rather than in the caller also makes the stamp describe the
// WRITE, which is the instant a reader's freshness check is really about.
func writeStatistics(
	ctx context.Context,
	db *FDBDatabase,
	stats StatisticsSubspace,
	storeSubspace subspace.Subspace,
	report *CollectionReport,
	version int64,
	tags []string,
) (int64, error) {
	target := stats.forStore(storeSubspace)
	var nanos int64
	_, err := db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		tx := rtx.Transaction()
		if err := applyTags(tx, tags); err != nil {
			return nil, err
		}
		// The DST seam, not time.Now: these bytes are persisted.
		nanos = rtx.Env().Now().UnixNano()
		begin, end := target.FDBRangeKeys()
		tx.ClearRange(fdb.KeyRange{Begin: fdb.Key(begin.FDBKey()), End: fdb.Key(end.FDBKey())})
		tx.Set(target.Pack(statisticsHeaderKey), packStatistic(RecordTypeStatistic{
			Count:                int64(len(report.Collected)),
			CollectedAtVersion:   version,
			CollectedAtUnixNanos: nanos,
		}))
		for name, st := range report.Collected {
			// BOTH stamps are set here, not one. The version used to be taken on
			// trust from the report, which held only because the single caller
			// pre-stamped every entry with the same value. Since the reader now
			// requires entry stamps to MATCH the header, a future caller that built
			// a report without pre-stamping would write a set rejected as torn --
			// surfacing to an operator as "not collected" immediately after a
			// successful collection. Stamping here makes the writer, not its
			// callers, responsible for the invariant the reader enforces.
			//
			// Overriding the caller cannot lose information while the reader demands
			// set-wide stamp equality: an entry version differing from the header's
			// is BY DEFINITION a torn set there, so this can only turn a set the
			// reader would reject into one it accepts. Per-entry versions become
			// meaningful only under incremental recollection (RFC-236 §7, out of
			// scope), which would have to relax that equality check first -- the two
			// move together, and this line is where the coupling lives.
			//
			// The range copies, so this never reaches report.Collected: a caller that
			// did not pre-stamp gets back a report saying version 0 while the store
			// holds `version`. Unreachable today -- writeStatistics is unexported and
			// has one caller, which pre-stamps -- and worth stating so the next
			// caller knows the report is not what was written.
			st.CollectedAtVersion = version
			st.CollectedAtUnixNanos = nanos
			tx.Set(target.Pack(tuple.Tuple{name}), packStatistic(st))
		}
		return nil, nil
	})
	return nanos, err
}

// ReadStatistics returns the statistics collected for one store, or ok=false if
// there are none. It is a SNAPSHOT read: a planner read must never add a
// conflict range, or planning could make a transaction retry.
//
// Absent, malformed and unknown-format all return ok=false. There is no partial
// success: a caller gets a usable set or nothing.
func ReadStatistics(
	ctx context.Context,
	db *FDBDatabase,
	stats StatisticsSubspace,
	storeSubspace subspace.Subspace,
) (StoreStatistics, bool, error) {
	out, ok, _, err := ReadStatisticsAt(ctx, db, stats, storeSubspace)
	return out, ok, err
}

// StatisticsReadRefusal names WHY a statistics read produced no usable set.
//
// It exists because a bool cannot: ReadStatisticsAt has seven distinct ways to
// decline, and collapsing them into ok=false made the refusals
// INDISTINGUISHABLE TO TESTS. Two separate checks were found dead that way --
// each pinned by a spec asserting ok==false, each silently superseded by a
// later check that refused the same fixture first, and nothing went red at the
// moment either was superseded. Fixtures cannot fix that: an undecodable entry
// has no stamps BY CONSTRUCTION, so no fixture can make the decode arm the only
// thing able to refuse it.
//
// Asserting the REASON makes the mutation matrix diagonal by construction, and
// means the next arm added cannot inherit an older arm's test.
type StatisticsReadRefusal string

const (
	// StatisticsReadOK — a usable set.
	StatisticsReadOK StatisticsReadRefusal = ""
	// StatisticsReadAbsent — nothing is stored here at all: no header AND no
	// entries. The ordinary "never collected" case, and the ONLY refusal that
	// means the store is empty.
	StatisticsReadAbsent StatisticsReadRefusal = "no statistics stored"
	// StatisticsReadHeaderMissing — per-type entries survive but the header does
	// not. DISTINCT from Absent: something IS stored, and reporting it as
	// absence tells an operator the store is empty while it holds orphaned
	// entries that a collect would clear. The header is what makes a set
	// vouchable, so entries without one are torn, not missing.
	StatisticsReadHeaderMissing StatisticsReadRefusal = "entries present with no header"
	// StatisticsReadUndecodableKey — an entry key this build cannot parse.
	StatisticsReadUndecodableKey StatisticsReadRefusal = "entry key could not be decoded"
	// StatisticsReadUnknownIntegerKey — an integer key that is not THE header;
	// record-type names are strings, so this is corruption or a newer layout.
	StatisticsReadUnknownIntegerKey StatisticsReadRefusal = "unknown integer entry key"
	// StatisticsReadNonStringKey — a key element that is neither the header
	// integer nor a record-type string.
	StatisticsReadNonStringKey StatisticsReadRefusal = "entry key is neither header nor record type"
	// StatisticsReadUndecodableValue — an entry value this build cannot decode.
	StatisticsReadUndecodableValue StatisticsReadRefusal = "entry value could not be decoded"
	// StatisticsReadCountMismatch — more or fewer entries than the header says
	// the write put down.
	StatisticsReadCountMismatch StatisticsReadRefusal = "entry count disagrees with the header"
	// StatisticsReadVersionMismatch — an entry carrying a version that is not
	// the header's, so it came from a different collection run.
	StatisticsReadVersionMismatch StatisticsReadRefusal = "entry version disagrees with the header"
	// StatisticsReadTimestampMismatch — same, for the timestamp. SPLIT from the
	// version case deliberately: they are two arms of one condition, and a
	// single shared reason would leave their specs unable to tell each other
	// apart -- which is the exact confound this type exists to remove. The
	// timestamp arm shipped undriven once already because the only spec covering
	// the pair moved the version.
	StatisticsReadTimestampMismatch StatisticsReadRefusal = "entry timestamp disagrees with the header"
)

// ReadStatisticsAt is ReadStatistics plus the cluster version the read was
// taken at, from the SAME transaction.
//
// The freshness gate needs both, and taking them separately costs a second
// round-trip on every uncached plan — for two numbers that are only meaningful
// relative to each other. Reading them together also removes a real (if narrow)
// window in which the entry could be replaced between the two reads, which
// would compare one run's stamp against a version drawn after another run.
func ReadStatisticsAtWithRefusal(
	ctx context.Context,
	db *FDBDatabase,
	stats StatisticsSubspace,
	storeSubspace subspace.Subspace,
	tags ...string,
) (StoreStatistics, StatisticsReadRefusal, int64, error) {
	target := stats.forStore(storeSubspace)
	var out StoreStatistics
	var found bool
	var readVersion int64
	var refusal StatisticsReadRefusal
	// headerTypeCount is the entry count the WRITE recorded; it is compared with
	// what this read actually assembled. Reset per attempt with everything else.
	var headerTypeCount int64
	// RunRead, not Run: this is on the PLAN path, and Run opens a read-write
	// transaction and pays a commit round-trip for a read that writes nothing.
	//
	// RunRead RETRIES, exactly as Run does, so everything the closure produces is
	// RESET at its top. This is the same hazard CollectStatistics guards against
	// one function away, and it bites differently here: a retry after a
	// concurrent ClearStatistics would otherwise keep attempt 1's entries and
	// found=true while taking attempt 2's read version — stale statistics wearing
	// a fresh stamp, which is precisely what the freshness gate exists to reject.
	_, err := db.RunRead(ctx, func(rtx fdb.ReadTransaction) (any, error) {
		if tErr := applyTagsTo(rtx.Options(), tags); tErr != nil {
			return nil, tErr
		}
		out = StoreStatistics{PerType: make(map[string]RecordTypeStatistic)}
		found = false
		readVersion = 0
		refusal = StatisticsReadOK
		headerTypeCount = 0
		// Propagate rather than swallow: the freshness gate turns a missing
		// version into a refusal either way, but an operator asking WHY wants
		// the cluster's own error, not a generic "no version" sentinel.
		v, vErr := rtx.GetReadVersion().Get()
		if vErr != nil {
			return nil, vErr
		}
		readVersion = v
		begin, end := target.FDBRangeKeys()
		kvs, rErr := rtx.Snapshot().GetRange(
			fdb.KeyRange{Begin: fdb.Key(begin.FDBKey()), End: fdb.Key(end.FDBKey())},
			fdb.RangeOptions{}).GetSliceWithError()
		if rErr != nil {
			return nil, rErr
		}
		for _, kv := range kvs {
			key, uErr := target.Unpack(kv.Key)
			if uErr != nil || len(key) != 1 {
				// Same reasoning as an undecodable VALUE below: a key this build
				// cannot parse is a set it cannot vouch for, and skipping it
				// returns the rest with ok=true — the partial answer the
				// all-or-nothing contract exists to forbid.
				refusal = StatisticsReadUndecodableKey
				return nil, nil
			}
			// The header is discriminated by tuple ELEMENT TYPE, not by a
			// reserved name. A string element is a record type; the integer
			// element is the header, and no record-type name can produce one.
			// A reserved string like "__header__" is reachable: Java-authored
			// metadata may legally declare a type with that exact name, whose
			// per-type write would then overwrite the header — after which the
			// type is missing and completeness can never pass.
			isHeader := false
			if n, isInt := key[0].(int64); isInt {
				if hdr, ok := statisticsHeaderKey[0].(int64); !ok || n != hdr {
					// An integer key that is not THE header cannot be a record
					// type — names are strings — so it is corruption or a newer
					// writer's layout. Skipping it and returning the rest is the
					// same partial answer a malformed value would give.
					refusal = StatisticsReadUnknownIntegerKey
					return nil, nil
				}
				isHeader = true
			}
			name, isStr := key[0].(string)
			if !isHeader && !isStr {
				refusal = StatisticsReadNonStringKey
				return nil, nil
			}
			st, ok := unpackStatistic(kv.Value)
			if !ok {
				// NOT a skip. This function promises all-or-nothing, and skipping
				// one malformed entry while the header stays valid returns a
				// PARTIAL map with ok=true — the shape the completeness gate is
				// built to make impossible, handed to it pre-broken. A caller
				// below the relational layer has no gate at all.
				refusal = StatisticsReadUndecodableValue
				return nil, nil
			}
			if isHeader {
				out.CollectedAtVersion = st.CollectedAtVersion
				out.CollectedAtUnixNanos = st.CollectedAtUnixNanos
				headerTypeCount = st.Count
				found = true
				continue
			}
			out.PerType[name] = st
		}
		return nil, nil
	})
	if err != nil {
		return StoreStatistics{}, StatisticsReadOK, 0, err
	}
	// The HEADER is what makes a set usable. Per-type entries without it are a
	// torn or hand-written state, and the run's own stamps are what expiry is
	// judged on.
	if refusal != StatisticsReadOK {
		// All-or-nothing: a set with an entry this build cannot read is not a
		// usable set, and reporting the rest of it would be a partial answer
		// wearing a complete one's shape.
		return StoreStatistics{}, refusal, readVersion, nil
	}
	if !found {
		// ENTRIES WITHOUT A HEADER ARE TORN, NOT ABSENT. The loop above populates
		// PerType regardless of whether the header turned up, so a range holding
		// per-type keys whose header was deleted lands here with a non-empty map
		// -- and calling that "nothing is stored" is the same absent-versus-stored
		// conflation this reader exists to remove, at the one arm that had not
		// been split yet.
		if len(out.PerType) > 0 {
			return StoreStatistics{}, StatisticsReadHeaderMissing, readVersion, nil
		}
		return StoreStatistics{}, StatisticsReadAbsent, readVersion, nil
	}
	// THE HEADER SAYS HOW MANY PER-TYPE ENTRIES THE WRITE PUT DOWN, so a read
	// that returns a different number returned a DIFFERENT SET than was written.
	// Header and entries are written in one transaction (ClearRange, then the
	// header, then every entry), so on any consistent read they agree.
	//
	// This is the check that makes the header's Count field load-bearing rather
	// than decorative -- it was previously written to durable bytes and never
	// read, which is the shape that lets a value drift wrong without anything
	// noticing. Disagreement is treated exactly like a malformed entry, because
	// it means the same thing: a PARTIAL set, which the completeness gate above
	// this is built to never receive.
	//
	// This reaches an operator as its OWN diagnosis, not as "not collected".
	// An earlier version of this comment argued the collapse was acceptable
	// because "ok=false is the only channel this signature has" -- which stopped
	// being true the moment ReadStatisticsAtWithRefusal landed, and the sentence
	// then held the gap open by justifying it. Absent and torn are opposite
	// facts: one means the store is empty, the other means it is holding
	// something that cannot be vouched for.
	if int64(len(out.PerType)) != headerTypeCount {
		return StoreStatistics{}, StatisticsReadCountMismatch, readVersion, nil
	}
	// EVERY ENTRY MUST CARRY THE HEADER'S OWN STAMPS.
	//
	// writeStatistics puts the header and every entry down in ONE transaction and
	// stamps them all with the same version and nanos, so on any set this library
	// wrote they agree. A disagreement means at least one entry came from a
	// DIFFERENT collection run.
	//
	// The count check above does not catch that: replacing one entry in place
	// leaves the number of keys unchanged, so the set passes as whole while
	// mixing an old count with the header's fresh version -- and the freshness
	// gate, which reads only the header, then judges the stale count fresh. That
	// is the all-or-nothing contract broken in the one direction the gates above
	// cannot see, because every one of them is looking at the header.
	for _, st := range out.PerType {
		if st.CollectedAtVersion != out.CollectedAtVersion {
			return StoreStatistics{}, StatisticsReadVersionMismatch, readVersion, nil
		}
		if st.CollectedAtUnixNanos != out.CollectedAtUnixNanos {
			return StoreStatistics{}, StatisticsReadTimestampMismatch, readVersion, nil
		}
	}
	return out, StatisticsReadOK, readVersion, nil
}

// ClearStatistics removes a store's statistics.
func ClearStatistics(
	ctx context.Context,
	db *FDBDatabase,
	stats StatisticsSubspace,
	storeSubspace subspace.Subspace,
	tags ...string,
) error {
	target := stats.forStore(storeSubspace)
	_, err := db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		if tErr := applyTags(rtx.Transaction(), tags); tErr != nil {
			return nil, tErr
		}
		begin, end := target.FDBRangeKeys()
		rtx.Transaction().ClearRange(fdb.KeyRange{Begin: fdb.Key(begin.FDBKey()), End: fdb.Key(end.FDBKey())})
		return nil, nil
	})
	return err
}

// ReadStatisticsAt is ReadStatisticsAtWithRefusal reduced to a boolean, for
// callers that only need to know whether the set is usable.
//
// Every caller that DISTINGUISHES refusals must use the WithRefusal form. A
// bool cannot tell apart the eight ways this read declines, and FOUR checks were
// found dead precisely because their specs asserted ok==false: a later check
// refused the same fixture first, and the earlier one stopped being exercised
// with nothing going red at the moment it happened.
//
// That applies to callers that REPORT a refusal too, not only to tests. The
// relational gate used this wrapper and therefore told operators "not
// collected" for every torn set -- an earlier draft of this very comment said
// "every caller that ASSERTS on a refusal", and its only non-test caller was
// doing exactly that while not asserting, so the wording excused it.
func ReadStatisticsAt(
	ctx context.Context,
	db *FDBDatabase,
	stats StatisticsSubspace,
	storeSubspace subspace.Subspace,
	tags ...string,
) (StoreStatistics, bool, int64, error) {
	out, refusal, version, err := ReadStatisticsAtWithRefusal(ctx, db, stats, storeSubspace, tags...)
	return out, err == nil && refusal == StatisticsReadOK, version, err
}

// SyntheticRecordTypesNotModeledError reports metadata declaring JOINED or
// UNNESTED record types, which this port carries opaquely rather than modelling.
//
// Collection refuses such a schema before scanning. RecordTypes() omits those
// declarations, so a run would read every record in the store to produce a set
// that is a PARTIAL view of the schema by construction — and would then write a
// header vouching for it. Completeness is undecidable against a partial set, so
// there is no reading of that outcome a planner could safely use.
type SyntheticRecordTypesNotModeledError struct {
	// TypeNames are the synthetic declarations found, so a caller can say which.
	//
	// STORAGE names, like every other name field outside StatisticsStatus. A
	// programmatic consumer matching against metadata needs them in the namespace
	// metadata uses; Error() is where they become human-facing and is therefore
	// where they are decoded.
	TypeNames []string
}

// Error renders the refusal for a human, decoding the declarations to the SQL
// identifiers the operator wrote.
//
// Error() IS the rendering boundary for an error value, so the same rule applies
// here as in the CLI: the field stays in the namespace its programmatic
// consumers need, and only the text a person reads is translated.
//
// Joining raw made ONE synthetic schema print MY$JOINED under `frl stats show`,
// which decodes at its renderer, and MY__1JOINED under `frl stats collect`,
// which surfaces this error -- two spellings of one declaration from one source,
// disagreeing by construction rather than by accident.
func (e *SyntheticRecordTypesNotModeledError) Error() string {
	names := make([]string, len(e.TypeNames))
	for i, n := range e.TypeNames {
		names[i] = ToUserIdentifier(n)
	}
	// Re-sort in the namespace being PRINTED. TypeNames arrives sorted in
	// STORAGE space (SyntheticRecordTypeNames sorts before returning), and the
	// two orders disagree: storage-sorted [A__0B, A__1B] decodes to
	// [A__B, A$B], so a reader scanning for A$B finds it second.
	sort.Strings(names)
	return fmt.Sprintf("metadata declares synthetic record types this port does not model (%s); "+
		"statistics collection refused rather than scanning the store for a set that "+
		"could never be complete", strings.Join(names, ", "))
}
