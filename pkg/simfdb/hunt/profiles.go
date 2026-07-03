package hunt

import (
	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// A Profile is a named hunting configuration: a schema + workload shape. Rotating profiles
// widens coverage — the kitchen sink stresses all maintainers together, the single-index
// profiles isolate one maintainer at higher throughput (so a per-maintainer bug surfaces
// without the others' noise), and the *-dense profiles shrink the keyspace so overwrite/
// delete/retry churn on a few hot keys is relentless.
type Profile struct {
	Name string
	Cfg  Config
}

// Profiles is the overnight-sweep matrix: every index maintainer in isolation and together,
// across normal and conflict-dense keyspaces. The runner cycles through these per seed so a
// long sweep covers the whole matrix uniformly.
func Profiles() []Profile {
	return []Profile{
		{"kitchen-sink", Config{Metadata: KitchenSinkMetadata}},
		{"kitchen-sink-dense", Config{Metadata: KitchenSinkMetadata, MaxPKs: 8, FaultProb: 0.4}},
		{"value", Config{Metadata: ValueMetadata}},
		{"count-sum", Config{Metadata: CountSumMetadata}},
		{"rank", Config{Metadata: RankMetadata}},
		{"rank-dense", Config{Metadata: RankMetadata, MaxPKs: 6}},
		{"maxever", Config{Metadata: MaxEverMetadata}},
		{"version", Config{Metadata: VersionMetadata}},
		{"covering", Config{Metadata: CoveringMetadata}},
		{"permuted", Config{Metadata: PermutedMetadata}},
	}
}

func newBuilder() *recordlayer.RecordMetaDataBuilder {
	b := recordlayer.NewRecordMetaDataBuilder()
	b.SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	b.SetRecordCountKey(recordlayer.EmptyKey())
	return b
}

func mustBuild(b *recordlayer.RecordMetaDataBuilder) *recordlayer.RecordMetaData {
	md, err := b.Build()
	if err != nil {
		panic("hunt: metadata build: " + err.Error())
	}
	return md
}

// ValueMetadata: record counting + a single VALUE index on price.
func ValueMetadata() *recordlayer.RecordMetaData {
	b := newBuilder()
	b.AddIndex("Order", recordlayer.NewIndex("hunt_v_price", recordlayer.Field("price")))
	return mustBuild(b)
}

// CountSumMetadata: a COUNT (grouped by price) + a SUM (ungrouped) index — the aggregate
// maintainers whose removeCommon* retry optimizations are the classic non-idempotency risk.
func CountSumMetadata() *recordlayer.RecordMetaData {
	b := newBuilder()
	b.AddIndex("Order", recordlayer.NewCountIndex("hunt_cs_count",
		recordlayer.GroupAll(recordlayer.Field("price"))))
	b.AddIndex("Order", recordlayer.NewSumIndex("hunt_cs_sum",
		recordlayer.Ungrouped(recordlayer.Field("price"))))
	return mustBuild(b)
}

// RankMetadata: a VALUE + RANK index. RANK has dual-subspace state (B-tree + RankedSet) that
// can drift under retry — the deepest maintainer to keep consistent.
func RankMetadata() *recordlayer.RecordMetaData {
	b := newBuilder()
	b.AddIndex("Order", recordlayer.NewIndex("hunt_r_price", recordlayer.Field("price")))
	b.AddIndex("Order", recordlayer.NewRankIndex("hunt_r_rank", recordlayer.Field("price")))
	return mustBuild(b)
}

// MaxEverMetadata: a MAX_EVER (ungrouped, long) index — monotone aggregate that must never
// regress even when the record carrying the max is deleted then a retry re-derives.
func MaxEverMetadata() *recordlayer.RecordMetaData {
	b := newBuilder()
	b.AddIndex("Order", recordlayer.NewIndex("hunt_m_price", recordlayer.Field("price")))
	b.AddIndex("Order", recordlayer.NewMaxEverLongIndex("hunt_m_maxever",
		recordlayer.Ungrouped(recordlayer.Field("price"))))
	return mustBuild(b)
}

// VersionMetadata: record versions on + a VERSION index. Exercises the versionstamp path
// (the one that surfaced the API-version fidelity gap) and its cleanup/recreate on retry.
func VersionMetadata() *recordlayer.RecordMetaData {
	b := newBuilder()
	b.SetStoreRecordVersions(true)
	b.AddIndex("Order", recordlayer.NewIndex("hunt_ver_price", recordlayer.Field("price")))
	b.AddIndex("Order", recordlayer.NewVersionIndex("hunt_ver_version", recordlayer.VersionKey()))
	return mustBuild(b)
}

// CoveringMetadata: a VALUE + a covering (KeyWithValue) index — the covering payload must stay
// in lockstep with the record it mirrors.
func CoveringMetadata() *recordlayer.RecordMetaData {
	b := newBuilder()
	b.AddIndex("Order", recordlayer.NewIndex("hunt_c_price", recordlayer.Field("price")))
	b.AddIndex("Order", recordlayer.NewIndex("hunt_c_covering",
		recordlayer.KeyWithValue(
			recordlayer.Concat(recordlayer.Field("price"), recordlayer.Nest("flower", recordlayer.Field("type"))),
			1)))
	return mustBuild(b)
}

// PermutedMetadata: PERMUTED_MAX + PERMUTED_MIN over (group=price, value=quantity). The
// permuted subspace must always reflect the current per-group extremum.
func PermutedMetadata() *recordlayer.RecordMetaData {
	b := newBuilder()
	b.AddIndex("Order", recordlayer.NewPermutedMaxIndex("hunt_p_max",
		recordlayer.GroupBy(recordlayer.Field("price"), recordlayer.Field("quantity")), 1))
	b.AddIndex("Order", recordlayer.NewPermutedMinIndex("hunt_p_min",
		recordlayer.GroupBy(recordlayer.Field("price"), recordlayer.Field("quantity")), 1))
	return mustBuild(b)
}
