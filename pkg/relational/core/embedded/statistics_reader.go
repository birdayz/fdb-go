package embedded

import (
	"context"
	"database/sql/driver"
	"fmt"
	"sort"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
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
	// ExtraTypes lists collected types the schema no longer declares, sorted.
	// These do NOT refuse — a dropped table leaves an orphan entry and the
	// planner simply never asks for it — but an operator wants to see them.
	ExtraTypes []string
	// perType is the provider input, populated only when Usable.
	perType map[string]float64
}

// statisticsGateInput is every fact the gates decide on, gathered by the I/O
// layer so the decision itself can be exercised without a cluster.
type statisticsGateInput struct {
	// ReadErr is non-nil when the statistics read failed. When set, nothing
	// else in this struct is meaningful.
	ReadErr error
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
	in := statisticsGateInput{DeclaredTypes: make([]string, 0, len(declared))}
	for name := range declared {
		in.DeclaredTypes = append(in.DeclaredTypes, name)
	}

	storeSubspace, err := c.sess.Keyspace.SchemaSubspace(c.sess.DBPath, c.sess.Schema)
	if err != nil {
		in.ReadErr = err
		return decideStatistics(in)
	}

	// Snapshot-only inside ReadStatistics: a planner read must never add a
	// conflict range, or planning could make a transaction retry.
	stats, ok, rErr := recordlayer.ReadStatistics(ctx, c.sess.DB,
		recordlayer.NewStatisticsSubspace(c.sess.Keyspace.StatisticsSubspace()), storeSubspace)
	in.ReadErr, in.Found, in.Stats = rErr, ok, stats
	if rErr == nil && ok {
		v, vErr := readCurrentVersion(ctx, c)
		in.VersionErr, in.CurrentVersion = vErr, v
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

	// GATE 1 — the read.
	if in.ReadErr != nil {
		st.Refusal = StatisticsReadFailed
		return st
	}
	if !in.Found {
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

	st.Usable = true
	st.Refusal = StatisticsOK
	st.perType = perType
	return st
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

// readCurrentVersion reads the cluster's current version for the freshness
// gate. Snapshot semantics: it is a read version, not a write, and adds no
// conflict range.
func readCurrentVersion(ctx context.Context, c *EmbeddedConnection) (int64, error) {
	v, err := c.sess.DB.RunRead(ctx, func(rtx fdb.ReadTransaction) (any, error) {
		return rtx.GetReadVersion().Get()
	})
	if err != nil {
		return 0, err
	}
	n, ok := v.(int64)
	if !ok {
		return 0, fmt.Errorf("read version has type %T", v)
	}
	return n, nil
}
