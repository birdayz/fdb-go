package embedded

import (
	"context"
	"database/sql/driver"
	"sort"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/protoname"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/relational/api"
)

// RFC-236's read side: decide whether a schema's collected statistics may be
// planned on, and say why when they may not.
//
// The decision lives in ONE function, decideStatistics, because it has two
// callers whose answers must never disagree. The planner asks it what to plan
// on. `frl stats show` asks it what to tell an operator. Separate
// implementations would eventually have the CLI report "usable" for statistics
// the planner had already refused — the most expensive kind of wrong, because
// it reads as a confirmation.
//
// decideStatistics is PURE: it takes the gathered facts and returns the
// verdict. Everything that touches FDB is in evaluateCollectedStatistics above
// it. That split is what lets a table-driven test drive every refusal arm —
// including the two that need a cluster misbehaving (a failed version read, an
// entry stamped ahead of the cluster) and would otherwise ship untested,
// firing for the first time in front of an operator and reading as a finding
// rather than as an untested branch.

// statisticsLocation returns the two subspaces every statistics operation on
// this connection needs: where THIS schema's entries live, and the root they
// live under.
//
// It exists so the pair is derived in ONE place. Three operations need it —
// collect, clear, and the planner's read — and each deriving it itself is three
// chances to disagree about the database-path convention or the schema's case
// folding. That failure is silent in the worst way: the collector writes where
// the planner never looks, every command reports success, and the only symptom
// is that plans never change.
//
// The fleet fan-out is the one caller that cannot use this, because it has no
// single schema to bind a connection to. It goes through the same two keyspace
// methods, and TestIntegration_Stats_FleetCollectIsReadableByTheConnection is
// what proves the two agree.
func (c *EmbeddedConnection) statisticsLocation() (recordlayer.StatisticsSubspace, subspace.Subspace, error) {
	storeSubspace, err := c.sess.Keyspace.SchemaSubspace(c.sess.DBPath, c.sess.Schema)
	if err != nil {
		return recordlayer.StatisticsSubspace{}, nil, err
	}
	return recordlayer.NewStatisticsSubspace(c.sess.Keyspace.StatisticsSubspace()), storeSubspace, nil
}

// statisticsMaxAgeVersions bounds how stale a collected statistic may be, in
// FDB versions rather than wall-clock nanoseconds.
//
// FDB advances the version by ~1,000,000 per second, so this is ~24 hours.
// Versions are the cluster's own clock: monotone within it, and immune to skew
// between the host that ran the collector and the host planning the query. A
// wall-clock comparison across two machines can make an entry effectively
// immortal, which would quietly defeat this gate.
const statisticsMaxAgeVersions int64 = 24 * 60 * 60 * 1_000_000

// StatisticsRefusal names why collected statistics were not used. The values
// are stable strings because `frl stats show` prints them.
//
// ADDING ONE? Add it to allStatisticsRefusals in statistics_reader_test.go and
// give it a case in decideStatisticsCases. The coverage guard cannot catch a
// constant that is in neither — Go cannot enumerate constants at runtime, so
// the guard's list is hand-maintained — and that gap is not theoretical:
// StatisticsSyntheticTypes was added without either and the guard stayed green.
// This note is here rather than only in the test because this is the line an
// author is looking at when the omission happens.
type StatisticsRefusal string

const (
	// StatisticsOK means every gate passed and the planner will use them.
	StatisticsOK StatisticsRefusal = ""
	// StatisticsNotCollected — nothing has ever been written for this store.
	StatisticsNotCollected StatisticsRefusal = "not collected"
	// StatisticsReadFailed — the read itself errored.
	StatisticsReadFailed StatisticsRefusal = "read failed"
	// StatisticsVersionUnavailable — the freshness gate could not read a
	// current version, so the age is unknown. Unknown age is not fresh.
	StatisticsVersionUnavailable StatisticsRefusal = "cluster version unavailable"
	// StatisticsStampedInFuture — the entry's version is ahead of the
	// cluster's. A restore from backup moves versions backwards, and an entry
	// stamped in the abandoned future would otherwise never expire, which is
	// the one way a freshness gate fails silently rather than loudly.
	StatisticsStampedInFuture StatisticsRefusal = "stamped ahead of the cluster"
	// StatisticsExpired — older than statisticsMaxAgeVersions.
	StatisticsExpired StatisticsRefusal = "expired"
	// StatisticsIncomplete — at least one record type in the schema has no
	// entry. See the completeness note on decideStatistics.
	StatisticsIncomplete StatisticsRefusal = "incomplete"
	// StatisticsEmptySchema — the schema declares no record types, so there is
	// nothing to be complete about and nothing to plan on.
	StatisticsEmptySchema StatisticsRefusal = "schema has no record types"
	// StatisticsSyntheticTypes — the metadata declares joined or unnested
	// synthetic types, which this port carries opaquely and does not model. See
	// decideStatistics for why that makes completeness undecidable here.
	StatisticsSyntheticTypes StatisticsRefusal = "metadata declares unmodeled synthetic record types"
	// StatisticsTorn — statistics exist but were not assembled from a single
	// consistent write: an entry count or stamp disagreeing with the header, or
	// an entry this build cannot decode. DISTINCT from NotCollected because the
	// two are opposite facts -- something IS stored, and it is unusable. See
	// recordlayer.StatisticsReadRefusal for the eight ways a read declines.
	StatisticsTorn StatisticsRefusal = "stored statistics are torn or unreadable"
	// StatisticsAmbiguousNames — two declared types collide across the SQL and
	// storage namespaces, so a lookup by name cannot say which one is meant. See
	// GATE 4 in decideStatistics.
	StatisticsAmbiguousNames StatisticsRefusal = "declared type names are ambiguous across the SQL and storage namespaces"
)

// StatisticsStatus is the full verdict for one schema, including the evidence
// behind it, so an operator sees what the planner saw.
type StatisticsStatus struct {
	// Usable is true exactly when the planner would plan on these.
	Usable bool
	// Refusal is StatisticsOK when Usable, otherwise why not.
	Refusal StatisticsRefusal
	// Found reports whether any statistics exist at all, independent of
	// whether they are usable. "collected but expired" and "never collected"
	// are different operator problems with different fixes.
	Found bool
	// Stats is what was read; zero-valued when Found is false.
	Stats recordlayer.StoreStatistics
	// CurrentVersion is the cluster version the freshness gate compared
	// against; 0 when it was not read.
	CurrentVersion int64
	// AgeVersions is CurrentVersion - Stats.CollectedAtVersion. Meaningful
	// only when the freshness gate ran; negative means stamped in the future.
	AgeVersions int64
	// MaxAgeVersions is the bound in force, so the CLI need not re-derive it.
	MaxAgeVersions int64
	// MissingTypes lists schema record types with no entry, sorted. Empty
	// unless Refusal is StatisticsIncomplete.
	MissingTypes []string
	// SyntheticTypes names the unmodeled joined/unnested types, when those are
	// why the refusal happened. Empty otherwise.
	SyntheticTypes []string
	// ExtraTypes lists collected types the schema no longer declares, sorted.
	// These do NOT refuse — a dropped table leaves an orphan entry and the
	// planner simply never asks for it — but an operator wants to see them.
	ExtraTypes []string
	// ReadRefusal is the record-layer read's own reason when Refusal is
	// StatisticsTorn, so an operator sees WHICH way the set is broken.
	ReadRefusal recordlayer.StatisticsReadRefusal
	// ReadErr is the underlying failure when Refusal is StatisticsReadFailed.
	//
	// Carried because Found is false in that case for a DIFFERENT reason than
	// everywhere else: existence is UNKNOWN, not absent. An operator told
	// "nothing is stored" after a permission or cluster fault collects again,
	// which does not diagnose the fault either.
	ReadErr error
	// AmbiguousTypes is the colliding pair when Refusal is
	// StatisticsAmbiguousNames: two declared names where one is the other's
	// escaped form, so a lookup by either cannot say which table is meant.
	AmbiguousTypes []string
	// perType is the provider input, populated only when Usable.
	perType map[string]float64
}

// statisticsGateInput is every fact the gates decide on, gathered by the I/O
// layer so the decision itself can be exercised without a cluster.
type statisticsGateInput struct {
	// ReadErr is non-nil when the statistics read failed. When set, nothing
	// else in this struct is meaningful.
	ReadErr error
	// ReadRefusal is WHY the read declined, when it did.
	//
	// Found alone cannot tell an ABSENT set from a TORN one -- both give
	// Found=false -- and reporting a torn set as "not collected" is the same
	// absent-versus-failed conflation the reader spent four commits removing.
	// Only StatisticsReadNoHeader means absent.
	ReadRefusal recordlayer.StatisticsReadRefusal
	// Found reports whether an entry exists.
	Found bool
	// Stats is what was read.
	Stats recordlayer.StoreStatistics
	// VersionErr is non-nil when the cluster's current version could not be
	// read. Only consulted when Found.
	VersionErr error
	// CurrentVersion is the cluster's version at gate time.
	CurrentVersion int64
	// DeclaredTypes is the schema's record type names.
	DeclaredTypes []string
	// HasSyntheticTypes reports that the metadata declares joined or unnested
	// types that RecordTypes() omits, so DeclaredTypes is a PARTIAL set.
	HasSyntheticTypes bool
	// SyntheticTypeNames names them, so a refusal can say WHICH type cost the
	// schema its statistics. "Metadata declares unmodeled synthetic types" is
	// otherwise a verdict an operator cannot act on.
	SyntheticTypeNames []string
}

// StatisticsStatus reports what the planner would decide about this
// connection's collected statistics, without planning anything.
//
// It deliberately ignores the PLANNER_STATISTICS opt-in: the flag governs
// whether the planner asks, not whether the data is any good, and an operator
// running `frl stats show` wants to know the data is fresh BEFORE turning the
// flag on. The CLI prints the flag's role separately.
func (c *EmbeddedConnection) StatisticsStatus(ctx context.Context) (StatisticsStatus, error) {
	if c.closed.Load() {
		return StatisticsStatus{}, driver.ErrBadConn
	}
	if c.sess == nil || c.sess.DB == nil || c.sess.Schema == "" {
		return StatisticsStatus{}, api.NewError(api.ErrCodeInvalidParameter,
			"StatisticsStatus requires a connection bound to a schema")
	}
	if err := c.ensureMetaData(ctx); err != nil {
		return StatisticsStatus{}, err
	}
	md := c.cachedMetaData()
	if md == nil {
		return StatisticsStatus{}, api.NewErrorf(api.ErrCodeUndefinedSchema,
			"no metadata for schema %q", c.sess.Schema)
	}
	return evaluateCollectedStatistics(ctx, c, md), nil
}

// evaluateCollectedStatistics gathers the gate inputs from FDB and returns
// decideStatistics' verdict.
//
// It opens no record store. The store's subspace IS the schema subspace, which
// the keyspace already yields — so this read is independent of
// fetchIndexStateSnapshot, including its early return when a schema has no
// indexes at all. That case matters: a join between two index-less tables is
// exactly where cardinality alone decides the order.
func evaluateCollectedStatistics(
	ctx context.Context,
	c *EmbeddedConnection,
	md *recordlayer.RecordMetaData,
) StatisticsStatus {
	declared := md.RecordTypes()
	in := statisticsGateInput{
		DeclaredTypes:     make([]string, 0, len(declared)),
		HasSyntheticTypes: md.DeclaresSyntheticRecordTypes(),
	}
	if in.HasSyntheticTypes {
		in.SyntheticTypeNames = md.SyntheticRecordTypeNames()
	}
	for name := range declared {
		in.DeclaredTypes = append(in.DeclaredTypes, name)
	}

	// Decide BEFORE any I/O when the answer cannot depend on it. Synthetic
	// declarations fix the verdict outright, and reading anyway costs an FDB
	// transaction on every opt-in plan-cache miss — one that may retry or wait on
	// a cluster whose answer is then thrown away.
	if in.HasSyntheticTypes {
		return decideStatistics(in)
	}

	statsSubspace, storeSubspace, err := c.statisticsLocation()
	if err != nil {
		in.ReadErr = err
		return decideStatistics(in)
	}

	// ONE read transaction for both halves. Snapshot-only: a planner read must
	// never add a conflict range, or planning could make a transaction retry.
	// The cluster version comes from the SAME transaction as the entry, so the
	// freshness gate compares two numbers drawn at one instant rather than
	// paying a second round-trip for a pair that is only meaningful together.
	// WithRefusal, not the boolean wrapper. A bool collapses eight read
	// outcomes into one, and this is the site where that collapse reaches an
	// OPERATOR: every torn set -- a count mismatch, a stamp mismatch, an
	// undecodable key or value -- would be reported as "not collected", which is
	// the one thing it is not. Only NoHeader means absent.
	stats, readRefusal, readVersion, rErr := recordlayer.ReadStatisticsAtWithRefusal(
		ctx, c.sess.DB, statsSubspace, storeSubspace, c.statisticsTags()...)
	ok := rErr == nil && readRefusal == recordlayer.StatisticsReadOK
	in.ReadErr, in.Found, in.Stats, in.ReadRefusal = rErr, ok, stats, readRefusal
	if rErr == nil && ok {
		// A zero version is not "the epoch", it is a read that did not happen.
		// Treating it as a real version would make every entry look infinitely
		// stale, which is safe, but reporting WHY is what an operator needs.
		if readVersion == 0 {
			in.VersionErr = &noClusterVersionError{}
		}
		in.CurrentVersion = readVersion
	}
	return decideStatistics(in)
}

// decideStatistics applies the gates in refusal order and returns the verdict.
//
// Every refusal produces the same planning outcome: today's plan, on the cost
// model's constant. That is not timidity, it is the only safe direction. A
// refusal yields LeafScanCardinality = 1e6, larger than almost any real count,
// so one missing type standing beside a real one ranks the missing table as the
// biggest in the schema and drives the join from the wrong side. Half a
// statistic is worse than none, which at least ties.
//
// COMPLETENESS IS SCHEMA-WIDE, not query-wide, and that is deliberate on two
// counts. It is undecidable here — this runs before the planner exists, so
// which types a query will touch is unknown. And it would be insufficient even
// if it were decidable: FullUnorderedScanExpression SUMS per-type cardinalities
// (properties/cost.go), so one absent type inside one scan node yields
// 1e6 + realCount, an inversion BELOW the granularity a per-query gate is even
// defined at. The cost, stated rather than discovered: one uncollected type
// disables statistics for every query in that schema.
func decideStatistics(in statisticsGateInput) StatisticsStatus {
	st := StatisticsStatus{MaxAgeVersions: statisticsMaxAgeVersions}

	// GATE 0 — UNMODELED SYNTHETIC TYPES, refused before anything is read,
	// because no read can repair it.
	//
	// RecordTypes() deliberately omits joined and unnested types: this port
	// carries their declarations opaquely and does not model them. So when they
	// are present, DeclaredTypes is a PARTIAL set, and the completeness gate
	// below would certify a schema as complete after checking a subset of its
	// types — exactly the inversion the gate exists to prevent, arrived at
	// through the gate itself.
	//
	// DeclaresSyntheticRecordTypes' own doc states the rule this obeys: a caller
	// computing over "all record types" for a coverage decision or a count is
	// computing over a set that omits them, and must refuse rather than answer
	// from the partial set. A statistics completeness check is both of those.
	if in.HasSyntheticTypes {
		st.Refusal = StatisticsSyntheticTypes
		st.SyntheticTypes = append([]string(nil), in.SyntheticTypeNames...)
		sort.Strings(st.SyntheticTypes)
		return st
	}

	// GATE 1 — the read.
	if in.ReadErr != nil {
		st.Refusal = StatisticsReadFailed
		st.ReadErr = in.ReadErr
		return st
	}
	if !in.Found {
		// ABSENT and TORN are opposite facts and only one of them means "run
		// collect to gather them". Everything except NoHeader means a set IS
		// stored and cannot be vouched for; calling that "not collected" tells an
		// operator the store is empty when it is holding something broken.
		if in.ReadRefusal != recordlayer.StatisticsReadOK &&
			in.ReadRefusal != recordlayer.StatisticsReadNoHeader {
			st.Refusal = StatisticsTorn
			st.ReadRefusal = in.ReadRefusal
			return st
		}
		st.Refusal = StatisticsNotCollected
		return st
	}
	st.Found = true
	st.Stats = in.Stats

	// GATE 2 — freshness, judged on VERSIONS (see statisticsMaxAgeVersions).
	if in.VersionErr != nil {
		st.Refusal = StatisticsVersionUnavailable
		return st
	}
	st.CurrentVersion = in.CurrentVersion
	st.AgeVersions = in.CurrentVersion - in.Stats.CollectedAtVersion
	if st.AgeVersions < 0 {
		st.Refusal = StatisticsStampedInFuture
		return st
	}
	if st.AgeVersions > statisticsMaxAgeVersions {
		st.Refusal = StatisticsExpired
		return st
	}

	// GATE 3 — completeness over the whole schema. The orphan sweep runs
	// alongside it: an extra entry is reportable but never a refusal.
	perType := make(map[string]float64, len(in.Stats.PerType))
	declared := make(map[string]struct{}, len(in.DeclaredTypes))
	for _, name := range in.DeclaredTypes {
		declared[name] = struct{}{}
		s, present := in.Stats.PerType[name]
		if !present {
			st.MissingTypes = append(st.MissingTypes, name)
			continue
		}
		perType[name] = float64(s.Count)
	}
	for name := range in.Stats.PerType {
		if _, declaredNow := declared[name]; !declaredNow {
			st.ExtraTypes = append(st.ExtraTypes, name)
		}
	}
	sort.Strings(st.MissingTypes)
	sort.Strings(st.ExtraTypes)
	if len(st.MissingTypes) > 0 {
		st.Refusal = StatisticsIncomplete
		return st
	}
	if len(perType) == 0 {
		// No declared types at all. NewCollectedStatistics would floor the
		// store total at 1, which is not a small store but a meaningless one —
		// every cost above it collapses to the same value.
		st.Refusal = StatisticsEmptySchema
		return st
	}

	// GATE 4 — NAME AMBIGUITY ACROSS THE TWO NAMESPACES.
	//
	// The per-type map the provider will be handed is keyed by STORAGE names, and
	// a relational scan asks with the SQL name it was written with, so the
	// provider tries the name as given and then, on a miss, its escaped form --
	// which is right whenever only one of the two can match.
	//
	// The check below runs over the DECLARED names, not that map: ambiguity is a
	// property of what a schema declares, not of what a run happened to collect.
	//
	// Both can. The escaping is not injective ACROSS the namespaces: MY$TABLE is
	// stored as MY__1TABLE, and a table whose SQL name IS MY__1TABLE is stored as
	// MY__01TABLE. With both present, a scan of MY__1TABLE hits the first entry
	// directly and is priced with the OTHER table's count -- the escaped form is
	// never consulted, because the unescaped lookup already succeeded.
	//
	// Nothing downstream can notice: the set is fresh, complete, and internally
	// consistent, and the number returned is a real count of a real table. So the
	// ambiguity is resolved HERE, once, by refusing the set -- rather than by
	// picking a lookup order, which only moves which of the two tables is priced
	// wrong.
	//
	// This is a REFUSAL, not the settled fix. The settled fix is to canonicalise
	// names so the two namespaces never meet at a lookup -- and that is not a
	// statistics-local change, because the identical try-then-escape shape exists
	// for FIELD names in values.go with the same non-injectivity. Escaping table
	// names in the translator would close this instance and leave its twin, while
	// changing what the planner DOES (record-type filters, explain text) rather
	// than what it refuses. Refusing falls back to the cost model's constant and
	// changes no plan that was already right, so it is the safe half to ship now;
	// canonicalisation needs its own RFC covering BOTH sites.
	//
	// The twin's surface is LARGER in the code -- four arms there against two here
	// -- but it was MEASURED before being written up, and it does not reach wrong
	// data from SQL: DDL accepts two columns whose names collide under the
	// escaping, and both still round-trip their own values
	// (TestFDB_FieldNameCollisionAcrossEscaping). The SQL path resolves columns
	// through the descriptor rather than through that fallback chain. So the RFC
	// should scope the twin by what it was shown to do, not by how the code looks.
	if ambiguous, ok := ambiguousStorageName(declared); ok {
		st.Refusal = StatisticsAmbiguousNames
		st.AmbiguousTypes = ambiguous
		return st
	}

	st.Usable = true
	st.Refusal = StatisticsOK
	st.perType = perType
	return st
}

// ambiguousStorageName reports a declared type name that means one table read as
// a STORAGE name and a different one read as a SQL name, which happens exactly
// when some name's escaped form is ALSO a declared name.
//
// Returns the colliding pair, lower name first, so the refusal can say which two
// tables an operator has to rename or quote differently.
//
// Takes the DECLARED set, not the per-type map the completeness loop built.
// Ambiguity is a property of the names a schema declares, not of which of them
// happened to be collected -- and the two coincide only because completeness
// refuses first. Reading the collected map would make this gate's correctness
// depend on the gate above it, which is one reordering away from vacuous, and
// §5 of RFC-236 explicitly floats relaxing completeness to per-query.
func ambiguousStorageName(declared map[string]struct{}) ([]string, bool) {
	var worst []string
	for name := range declared {
		escaped, err := protoname.ToProtoBufCompliantName(name)
		if err != nil || escaped == name {
			continue
		}
		if _, collides := declared[escaped]; !collides {
			continue
		}
		pair := []string{name, escaped}
		// Deterministic across map iteration order: an operator comparing two
		// runs must not see the pair change.
		if worst == nil || pair[0] < worst[0] {
			worst = pair
		}
	}
	return worst, worst != nil
}

// fetchCollectedStatistics returns per-record-type row counts collected by the
// offline collector (RFC-236), or nil to plan on the cost model's constant.
//
// The opt-in flag is the first and cheapest gate; the rest is
// evaluateCollectedStatistics, shared with `frl stats show`.
func (g *cascadesGenerator) fetchCollectedStatistics(
	ctx context.Context,
	md *recordlayer.RecordMetaData,
	popts plannerOptions,
) properties.StatisticsProvider {
	// GATE 0 — opt-in. Also in the plan-cache key, BOTH halves
	// (planner_options.go): with only a render arm, a flag-ON connection whose
	// other options are default renders "", byte-identical to flag-off, and
	// shares its cache entry.
	if !popts.useCollectedStatistics {
		return nil
	}
	c := g.c
	if c == nil || c.sess == nil || c.sess.DB == nil || md == nil {
		return nil
	}
	st := evaluateCollectedStatistics(ctx, c, md)
	if !st.Usable {
		return nil
	}
	return properties.NewCollectedStatistics(st.perType)
}

// noClusterVersionError marks a statistics read whose transaction produced no
// cluster version. Age is then unknown, and unknown age is not fresh.
//
// A struct rather than an errors.New sentinel, because this package's convention
// is typed errors and a sentinel can carry no structured context if this ever
// needs to say WHICH read or WHY.
//
// No caller matches on it today: decideStatistics only asks whether VersionErr is
// nil. Saying otherwise would be a claim about code that does not exist. The
// field stays an error rather than a bool because it ALSO carries real cluster
// errors from the read, and the day something needs to tell those apart from
// this marker, errors.As is what it will reach for -- which a sentinel would
// make no easier and a bool would foreclose.
type noClusterVersionError struct{}

func (e *noClusterVersionError) Error() string {
	return "statistics read produced no cluster version"
}

// statisticsTags returns the connection's FDB transaction tags, which every
// statistics transaction must carry.
//
// Threaded as a parameter rather than wrapped around the database. Wrapping
// means reconstructing an *FDBDatabase, and this repo's copy-method gate
// forbids the field-by-field form precisely because a dropped field is silent —
// the first attempt here dropped env, which swaps a persisted timestamp's
// seeded clock for the wall clock, unreplayably, and only when tags happen to
// be configured. A parameter is visible at every call site.
//
// Why it matters at all: beginTransaction's comment calls itself "the single
// transaction-creation seam in the SQL layer", and statistics work does not go
// through it — collection opens its own transaction per batch. Untagged, the
// heaviest job in the system escapes the cluster's ratekeeper.
func (c *EmbeddedConnection) statisticsTags() []string {
	tags, _ := c.Options().Get(api.OptTransactionTags).([]string)
	return tags
}
