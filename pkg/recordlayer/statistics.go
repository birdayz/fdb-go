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
// Count is EXACT — obtained by scanning, not sampled. That is the point of
// collecting offline: exactness removes the floor, the quantization and the
// bytes-to-rows conversion that make plan-time sampling unusable for small
// tables.
type RecordTypeStatistic struct {
	// Count is the exact number of records of this type at collection time.
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
	// BatchSize is the number of records scanned per transaction. Collection is
	// continuation-driven, so a large store costs many small transactions
	// rather than one that cannot commit inside FDB's 5s limit.
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
}

func (o CollectOptions) batchSize() int {
	if o.BatchSize <= 0 {
		return 1000
	}
	return o.BatchSize
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

// CollectStatistics scans a record store and writes exact per-record-type
// counts into stats.
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
	capped := make(map[string]string)
	// cappedTypes is the set abandoned mid-scan. It is durable across batches
	// (a type over its cap stays over) and safe under retry: a re-run of the same
	// rows re-derives the same membership.
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
		var batchContinuation []byte
		res, err := db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			batchCounts = make(map[string]int64)
			batchScanned = 0
			batchContinuation = continuation
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
			// FOUR are the per-attempt accumulators (batchCounts, batchScanned,
			// batchContinuation, batchDone). They are assigned here BECAUSE they
			// are reset here; that reset is the fix.
			//
			// THREE are not accumulators — storeSubspace above, declaredTypes on
			// the next line, collectedAtVersion below — and they are safe under
			// retry by IDEMPOTENCE: each is an overwrite with a value the same
			// attempt would produce again. That is a weaker property than the
			// accumulators get, so it is stated rather than glossed.
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
			collectedAtVersion = v

			props := ScanProperties{
				ExecuteProperties: ExecuteProperties{}.WithReturnedRowLimit(opts.batchSize()),
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
						cappedTypes[name] = struct{}{}
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
			Skipped:        capped,
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

	// Apply the cap AFTER counting: a type over its cap is recorded absent, and
	// the reason is kept so a caller is told rather than left to infer it from
	// a missing key.
	report := &CollectionReport{
		Collected:      make(map[string]RecordTypeStatistic),
		Skipped:        capped,
		RecordsScanned: scanned,
	}
	for name, c := range counts {
		report.Collected[name] = RecordTypeStatistic{
			Count:              c,
			CollectedAtVersion: collectedAtVersion,
		}
	}

	if storeSubspace == nil {
		return report, nil
	}
	nowNanos, wErr := writeStatistics(ctx, db, stats, storeSubspace, report, collectedAtVersion)
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
) (int64, error) {
	target := stats.forStore(storeSubspace)
	var nanos int64
	_, err := db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		tx := rtx.Transaction()
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

// ReadStatisticsAt is ReadStatistics plus the cluster version the read was
// taken at, from the SAME transaction.
//
// The freshness gate needs both, and taking them separately costs a second
// round-trip on every uncached plan — for two numbers that are only meaningful
// relative to each other. Reading them together also removes a real (if narrow)
// window in which the entry could be replaced between the two reads, which
// would compare one run's stamp against a version drawn after another run.
func ReadStatisticsAt(
	ctx context.Context,
	db *FDBDatabase,
	stats StatisticsSubspace,
	storeSubspace subspace.Subspace,
) (StoreStatistics, bool, int64, error) {
	target := stats.forStore(storeSubspace)
	var out StoreStatistics
	var found bool
	var readVersion int64
	var malformed bool
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
		out = StoreStatistics{PerType: make(map[string]RecordTypeStatistic)}
		found = false
		readVersion = 0
		malformed = false
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
				continue
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
					continue
				}
				isHeader = true
			}
			name, isStr := key[0].(string)
			if !isHeader && !isStr {
				continue
			}
			st, ok := unpackStatistic(kv.Value)
			if !ok {
				// NOT a skip. This function promises all-or-nothing, and skipping
				// one malformed entry while the header stays valid returns a
				// PARTIAL map with ok=true — the shape the completeness gate is
				// built to make impossible, handed to it pre-broken. A caller
				// below the relational layer has no gate at all.
				malformed = true
				return nil, nil
			}
			if isHeader {
				out.CollectedAtVersion = st.CollectedAtVersion
				out.CollectedAtUnixNanos = st.CollectedAtUnixNanos
				found = true
				continue
			}
			out.PerType[name] = st
		}
		return nil, nil
	})
	if err != nil {
		return StoreStatistics{}, false, 0, err
	}
	// The HEADER is what makes a set usable. Per-type entries without it are a
	// torn or hand-written state, and the run's own stamps are what expiry is
	// judged on.
	if malformed {
		// All-or-nothing: a set with an entry this build cannot read is not a
		// usable set, and reporting the rest of it would be a partial answer
		// wearing a complete one's shape.
		return StoreStatistics{}, false, readVersion, nil
	}
	if !found {
		return StoreStatistics{}, false, readVersion, nil
	}
	return out, true, readVersion, nil
}

// ClearStatistics removes a store's statistics.
func ClearStatistics(
	ctx context.Context,
	db *FDBDatabase,
	stats StatisticsSubspace,
	storeSubspace subspace.Subspace,
) error {
	target := stats.forStore(storeSubspace)
	_, err := db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		begin, end := target.FDBRangeKeys()
		rtx.Transaction().ClearRange(fdb.KeyRange{Begin: fdb.Key(begin.FDBKey()), End: fdb.Key(end.FDBKey())})
		return nil, nil
	})
	return err
}
