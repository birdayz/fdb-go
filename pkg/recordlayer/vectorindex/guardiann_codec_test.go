package vectorindex

import (
	"encoding/hex"
	"fmt"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/vectorcodec"
)

// Golden values produced by the Java 4.14.2.0 GuardiANN classes
// (StorageAdapter, the tasks' valueTuple, RunningStats, RandomHelpers).
func TestGuardiannCodecsMatchJava(t *testing.T) {
	t.Parallel()
	u1 := mustUUID("0123456789abcdef8877665544332211")
	u2 := mustUUID("fedcba98765432101122334455667788")
	half := gVector{data: []float64{1.0, 0.5, -2.0}, typ: vectorcodec.TypeHalf}
	vid := guardiannVectorID{pk: tuple.Tuple{"zone1", int64(7)}, uuid: u1}
	stats := runningStatsOf(0.5).add(1.5).add(4.25)
	cref := guardiannClusterRef{clusterID: u2, centroid: half}
	for name, c := range map[string]struct {
		got  []byte
		want string
	}{
		"primary":          {vectorRefValue(guardiannVectorRef{id: vid, vector: half, primary: true}, gVector.encode), "300123456789abcdef887766554433221114260100ff3c00ff3800ffc000ff00"},
		"underrep":         {vectorRefValue(guardiannVectorRef{id: vid, vector: half, primary: true, underrep: true, collapsed: true}, gVector.encode), "300123456789abcdef88776655443322111501270100ff3c00ff3800ffc000ff00"},
		"replicated":       {vectorRefValue(guardiannVectorRef{id: vid, vector: half, priority: 0.93}, gVector.encode), "300123456789abcdef88776655443322111502260100ff3c00ff3800ffc000ff0021bfedc28f5c28f5c3"},
		"clustermeta":      {clusterMetaValue(guardiannClusterMetadata{id: u2, numUnderrep: 1, numReplicated: 2, stats: stats, states: 3, maxEverPrimary: 5}), "1501150205150321c000aaaaaaaaaaaa21c01e2aaaaaaaaaac21c0110000000000000015031505"},
		"clustermetaEmpty": {clusterMetaValue(guardiannClusterMetadata{id: u2, stats: runningStatsIdentity()}), "1414051421800000000000000021800000000000000021000fffffffffffff001414"},
		"accessInfo":       {tuple.Tuple{int64(-1), nil}.Pack(), "13fe00"},
		"vectorMetadata":   {tuple.Tuple{u1, nil}.Pack(), "300123456789abcdef887766554433221100"},
		"taskSplit":        {(&guardiannTask{kind: taskSplitMerge, id: u1, targets: []tuple.UUID{u2}, centroid: half, nearest: []guardiannClusterRef{cref}}).valueTuple(gVector.encode).Pack(), "1430fedcba987654321011223344556677880100ff3c00ff3800ffc000ff00050530fedcba987654321011223344556677880100ff3c00ff3800ffc000ff000000"},
		"taskReassign":     {(&guardiannTask{kind: taskReassign, id: u1, targets: []tuple.UUID{u2}, centroid: half, causes: []tuple.UUID{u1}, nearest: []guardiannClusterRef{cref}}).valueTuple(gVector.encode).Pack(), "150130fedcba987654321011223344556677880100ff3c00ff3800ffc000ff0005300123456789abcdef887766554433221100050530fedcba987654321011223344556677880100ff3c00ff3800ffc000ff000000"},
		"taskCollapse":     {(&guardiannTask{kind: taskCollapse, id: u1, targets: []tuple.UUID{u2}, centroid: half}).valueTuple(gVector.encode).Pack(), "150330fedcba987654321011223344556677880100ff3c00ff3800ffc000ff00"},
		"taskBounce":       {(&guardiannTask{kind: taskBounce, id: u1, targets: []tuple.UUID{u2}, dependents: []tuple.UUID{u1}, finalKind: taskReassign}).valueTuple(gVector.encode).Pack(), "15020530fedcba987654321011223344556677880005300123456789abcdef88776655443322110002524541535349474e00"},
	} {
		if got := hex.EncodeToString(c.got); got != c.want {
			t.Errorf("%s = %s, want %s", name, got, c.want)
		}
	}
	removed, err := stats.remove(1.5)
	if err != nil || fmt.Sprint(removed) != fmt.Sprint(guardiannRunningStats{2, 2.3749999999999996, 7.031250000000002, 4.25}) {
		t.Errorf("remove = %v %v", removed, err)
	}
	if got := stats.combine(runningStatsOf(9.0).add(-1.0)); fmt.Sprint(got) != fmt.Sprint(guardiannRunningStats{5, 2.8499999999999996, 61.95, 9.0}) {
		t.Errorf("combine = %v", got)
	}
	if got := signatureUUID(half).String(); got != "d7b93505-094d-8103-9287-af13c88599cb" {
		t.Errorf("signature = %s", got)
	}
	if got := randomUUIDFrom(&splittableRandom{seed: 42, gamma: goldenGamma}).String(); got != "bdd73226-2feb-4e95-a8ef-e333b266f103" {
		t.Errorf("randomUuid = %s", got)
	}
	r := &splittableRandom{seed: 7, gamma: goldenGamma}
	if got := fmt.Sprint(r.nextInt(10), r.nextInt(16), r.nextInt(1000000007), r.nextInt(3)); got != "6 2 784385053 0" {
		t.Errorf("nextInt = %s", got)
	}
	if got := javaNameUUID(tuple.Tuple{int64(1)}.Pack()).String(); got != "00bd7d1d-053d-322e-899c-11cd273c44de" {
		t.Errorf("nameUUID = %s", got)
	}
	if got := newSplittableRandomForUUID(u1).nextLong(); got != -5948919466763955995 {
		t.Errorf("random(UUID) = %d", got)
	}
	// Every task round-trips through its value tuple.
	for _, task := range []*guardiannTask{
		{kind: taskSplitMerge, id: u1, targets: []tuple.UUID{u2}, centroid: half, nearest: []guardiannClusterRef{cref}},
		{kind: taskBounce, id: u1, targets: []tuple.UUID{u2}, dependents: []tuple.UUID{u1}, finalKind: taskReassign},
	} {
		back, err := taskFromTuples(tuple.Tuple{task.id}, task.valueTuple(gVector.encode), decodeGVector)
		if err != nil || hex.EncodeToString(back.valueTuple(gVector.encode).Pack()) != hex.EncodeToString(task.valueTuple(gVector.encode).Pack()) {
			t.Errorf("task %d round trip: %v", task.kind, err)
		}
	}
}

func mustUUID(h string) tuple.UUID {
	b, err := hex.DecodeString(h)
	if err != nil {
		panic(err)
	}
	return tuple.UUID(b)
}
