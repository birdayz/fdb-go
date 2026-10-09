package recordlayer

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"fdb.dev/gen"
	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
)

func TestVectorEngineIdentity(t *testing.T) {
	t.Parallel()
	idx := func(engine string) *Index {
		i := &Index{Name: "vi", Type: IndexTypeVector, Options: map[string]string{IndexOptionVectorNumDimensions: "3"}}
		if engine != "" {
			i.Options[IndexOptionVectorEngine] = engine
		}
		return i
	}
	for engine, want := range map[string]VectorEngineKind{"": VectorEngineHNSW, "hnsw": VectorEngineHNSW, "GuardiANN": VectorEngineGuardiann} {
		if got, err := VectorEngineOf(idx(engine)); err != nil || got != want {
			t.Errorf("%q: %v %v", engine, got, err)
		}
	}
	var mde *MetaDataError
	if _, err := VectorEngineOf(idx("spann")); !errors.As(err, &mde) {
		t.Errorf("unknown engine: %v", err)
	}
	if err := validateVectorIndexOptions(idx(""), idx("GUARDIANN"), map[string]bool{IndexOptionVectorEngine: true}); !errors.As(err, &mde) {
		t.Errorf("engine change: %v", err)
	}
	sub := subspace.Sub("v")
	m, err := newVectorIndexMaintainer(idx("GUARDIANN"), sub, sub, sub, nil, nil)
	if err != nil || m.engine != VectorEngineGuardiann || m.guardiannConfig.primaryClusterMax != 1000 {
		t.Errorf("GuardiANN maintainer: %v", err)
	}
	// Config's own checks refuse what Java's parseConfig refuses.
	bad := idx("GUARDIANN")
	bad.Options[IndexOptionGuardiannCollapseMinDuplicates] = "1000"
	var iae *IllegalArgumentError
	if _, err := newVectorIndexMaintainer(bad, sub, sub, sub, nil, nil); !errors.As(err, &iae) {
		t.Errorf("collapseMinDuplicates >= primaryClusterMax: %v", err)
	}
	if err := validateVectorIndexOptionsAtBuild(bad); !errors.As(err, &mde) || mde.Message != "incorrect index options" {
		t.Errorf("build-time GuardiANN config check: %v", err)
	}
}

func TestGuardiannChangedOptions(t *testing.T) {
	t.Parallel()
	with := func(kv ...string) *Index {
		i := &Index{Name: "vi", Type: IndexTypeVector, Options: map[string]string{
			IndexOptionVectorNumDimensions: "3", IndexOptionVectorEngine: "GUARDIANN",
		}}
		for n := 0; n < len(kv); n += 2 {
			i.Options[kv[n]] = kv[n+1]
		}
		return i
	}
	old := with()
	var mee *MetaDataEvolutionError
	for _, c := range []struct {
		key, val string
		ok       bool
	}{
		{IndexOptionGuardiannPrimaryClusterMax, "500", false},
		{IndexOptionGuardiannPrimaryClusterMin, "100", true}, // its default: no effective change
		{IndexOptionGuardiannConstructionCentroidEfRingSearch, "7", false},
		{IndexOptionVectorMetric, "COSINE_METRIC", false},
		{IndexOptionGuardiannPrimaryClusterHardMax, "3000", true},
		{IndexOptionGuardiannBounceConcurrency, "3", true},
		{IndexOptionHNSWStatsThreshold, "9", true},
	} {
		changed := map[string]bool{c.key: true}
		err := validateVectorIndexOptions(old, with(c.key, c.val), changed)
		if c.ok {
			if err != nil || len(changed) != 0 {
				t.Errorf("%s: %v, left %v", c.key, err, changed)
			}
		} else if !errors.As(err, &mee) {
			t.Errorf("%s: want immutable-option refusal, got %v", c.key, err)
		}
	}
}

func TestGuardiannNaNOptions(t *testing.T) {
	t.Parallel()
	for _, key := range []string{
		IndexOptionGuardiannMergeMaxEverFraction, IndexOptionGuardiannMinChildFraction,
		IndexOptionGuardiannMaxRelativeImbalance, IndexOptionGuardiannSplitImbalancePenalty,
	} {
		idx := &Index{Name: "vi", Type: IndexTypeVector, Options: map[string]string{
			IndexOptionVectorNumDimensions: "3", IndexOptionVectorEngine: "GUARDIANN", key: "NaN",
		}}
		var md *MetaDataError
		var argument *IllegalArgumentError
		err := validateVectorIndexOptionsAtBuild(idx)
		if !errors.As(err, &md) || !errors.As(err, &argument) {
			t.Errorf("%s: want wrapped IllegalArgumentError, got %v", key, err)
		}
	}
	nan := math.NaN()
	for _, opts := range []VectorIndexScanOptions{
		{GuardiannCandidatePoolFactor: &nan}, {GuardiannSearchDistanceRatioCutoff: &nan},
	} {
		_, err := guardiannSearchConfigOf(opts)
		var argument *IllegalArgumentError
		if !errors.As(err, &argument) {
			t.Errorf("NaN scan option accepted: %v", err)
		}
	}
}

func TestVectorScanOptionsWireEncoding(t *testing.T) {
	t.Parallel()
	no, integer, fraction, ef := false, 7, 1.5, 11
	opts := VectorIndexScanOptions{
		ReturnVectors: &no, EfSearch: &ef,
		GuardiannCandidatePoolFactor: &fraction, GuardiannSearchMaxClusters: &integer,
		GuardiannSearchMinClustersBeforePruning: &integer, GuardiannSearchDistanceRatioCutoff: &fraction,
		GuardiannCentroidEfRingSearch: &integer, GuardiannCentroidEfOutwardSearch: &integer, GuardiannSearchConcurrency: &integer,
	}
	wire, err := opts.ToProto()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := VectorIndexScanOptionsFromProto(wire)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := decoded.ToProto()
	if err != nil || !proto.Equal(wire, roundTrip) {
		t.Fatalf("all-option round trip: %v, %v", roundTrip, err)
	}
	want := map[string]*gen.Value{
		"vectorReturnVectors": {BoolValue: proto.Bool(false)}, "hnswEfSearch": {IntValue: proto.Int32(11)},
		"guardiannCandidatePoolFactor": {DoubleValue: proto.Float64(1.5)}, "guardiannSearchMaxClusters": {IntValue: proto.Int32(7)},
		"guardiannSearchMinClustersBeforePruning": {IntValue: proto.Int32(7)}, "guardiannSearchDistanceRatioCutoff": {DoubleValue: proto.Float64(1.5)},
		"guardiannCentroidEfRingSearch": {IntValue: proto.Int32(7)}, "guardiannCentroidEfOutwardSearch": {IntValue: proto.Int32(7)}, "guardiannSearchConcurrency": {IntValue: proto.Int32(7)},
	}
	if len(wire.OptionEntries) != len(want) {
		t.Fatalf("wire entries=%d, want %d", len(wire.OptionEntries), len(want))
	}
	for _, entry := range wire.OptionEntries {
		if !proto.Equal(entry.Value, want[entry.GetKey()]) {
			t.Fatalf("wrong wire option: %v", entry)
		}
		delete(want, entry.GetKey())
	}
	if len(want) != 0 {
		t.Fatalf("missing options: %v", want)
	}
}

func TestVectorScanOptionsWireRoundTrip(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"vectorReturnVectors", "hnswEfSearch", "guardiannCandidatePoolFactor", "guardiannSearchMaxClusters", "guardiannSearchMinClustersBeforePruning", "guardiannSearchDistanceRatioCutoff", "guardiannCentroidEfRingSearch", "guardiannCentroidEfOutwardSearch", "guardiannSearchConcurrency"} {
		for _, null := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/null=%t", key, null), func(t *testing.T) {
				t.Parallel()
				value := &gen.Value{}
				if !null {
					switch key {
					case "vectorReturnVectors":
						value.BoolValue = proto.Bool(false)
					case "guardiannCandidatePoolFactor", "guardiannSearchDistanceRatioCutoff":
						value.DoubleValue = proto.Float64(0)
					default:
						value.IntValue = proto.Int32(0)
					}
				}
				want := &gen.PVectorIndexScanOptions{OptionEntries: []*gen.PVectorIndexScanOptions_POptionEntry{{Key: proto.String(key), Value: value}}}
				opts, err := VectorIndexScanOptionsFromProto(want)
				if err != nil {
					t.Fatal(err)
				}
				got, err := opts.ToProto()
				if err != nil {
					t.Fatal(err)
				}
				if !proto.Equal(got, want) {
					t.Fatalf("presence lost: got %v want %v", got, want)
				}
				// Decoding owns its fields; changing the input proto cannot change options.
				want.OptionEntries[0].Value.StringValue = proto.String("mutated")
				again, err := opts.ToProto()
				if err != nil || !proto.Equal(got, again) {
					t.Fatal("decoded options alias the input proto")
				}
			})
		}
	}
	empty, err := (VectorIndexScanOptions{}).ToProto()
	if err != nil || len(empty.OptionEntries) != 0 {
		t.Fatalf("zero options emitted entries: %v, %v", empty, err)
	}
}

func TestVectorScanOptionsAliasesAndRejections(t *testing.T) {
	t.Parallel()
	entry := func(key string, value *gen.Value) *gen.PVectorIndexScanOptions_POptionEntry {
		return &gen.PVectorIndexScanOptions_POptionEntry{Key: proto.String(key), Value: value}
	}
	for _, alias := range []string{"vectorReturnVectors", "hnswReturnVectors"} {
		opts, err := VectorIndexScanOptionsFromProto(&gen.PVectorIndexScanOptions{OptionEntries: []*gen.PVectorIndexScanOptions_POptionEntry{entry(alias, &gen.Value{BoolValue: proto.Bool(true)})}})
		if err != nil || opts.ReturnVectors == nil || !*opts.ReturnVectors {
			t.Fatalf("alias %s: %v, %v", alias, opts, err)
		}
		wire, err := opts.ToProto()
		if err != nil || len(wire.OptionEntries) != 1 || wire.OptionEntries[0].GetKey() != "vectorReturnVectors" {
			t.Fatalf("alias emitted noncanonical name: %v %v", wire, err)
		}
		for _, other := range []string{"vectorReturnVectors", "hnswReturnVectors"} {
			_, err := VectorIndexScanOptionsFromProto(&gen.PVectorIndexScanOptions{OptionEntries: []*gen.PVectorIndexScanOptions_POptionEntry{entry(alias, &gen.Value{}), entry(other, &gen.Value{BoolValue: proto.Bool(false)})}})
			var duplicate *RecordCoreError
			if !errors.As(err, &duplicate) || duplicate.IndexOption != "vectorReturnVectors" {
				t.Fatalf("duplicate %s/%s: %v", alias, other, err)
			}
		}
	}
	for _, e := range []*gen.PVectorIndexScanOptions_POptionEntry{
		entry("unknown", &gen.Value{}), nil,
		entry("hnswEfSearch", &gen.Value{LongValue: proto.Int64(1)}),
		entry("vectorReturnVectors", &gen.Value{IntValue: proto.Int32(1)}),
		entry("guardiannCandidatePoolFactor", &gen.Value{FloatValue: proto.Float32(1)}),
		entry("guardiannSearchConcurrency", &gen.Value{BoolValue: proto.Bool(true)}),
		entry("hnswEfSearch", &gen.Value{IntValue: proto.Int32(1), BoolValue: proto.Bool(true)}),
	} {
		if _, err := VectorIndexScanOptionsFromProto(&gen.PVectorIndexScanOptions{OptionEntries: []*gen.PVectorIndexScanOptions_POptionEntry{e}}); err == nil {
			t.Fatalf("accepted malformed entry %v", e)
		}
	}
	if _, err := VectorIndexScanOptionsFromProto(nil); err == nil {
		t.Fatal("accepted nil options")
	}
	tooLarge := int(int64(math.MaxInt32) + 1)
	if _, err := (VectorIndexScanOptions{GuardiannSearchConcurrency: &tooLarge}).ToProto(); err == nil {
		t.Fatal("silently truncated Java Integer")
	}
}

func FuzzVectorScanOptionsProto(f *testing.F) {
	for _, p := range []*gen.PVectorIndexScanOptions{
		{},
		{OptionEntries: []*gen.PVectorIndexScanOptions_POptionEntry{{Key: proto.String("hnswReturnVectors"), Value: &gen.Value{BoolValue: proto.Bool(false)}}}},
		{OptionEntries: []*gen.PVectorIndexScanOptions_POptionEntry{{Key: proto.String("hnswEfSearch"), Value: &gen.Value{IntValue: proto.Int32(0)}}}},
		{OptionEntries: []*gen.PVectorIndexScanOptions_POptionEntry{{Key: proto.String("vectorReturnVectors"), Value: &gen.Value{}}, {Key: proto.String("hnswReturnVectors"), Value: &gen.Value{}}}},
	} {
		data, err := proto.Marshal(p)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var p gen.PVectorIndexScanOptions
		if err := proto.Unmarshal(data, &p); err != nil {
			return
		}
		opts, err := VectorIndexScanOptionsFromProto(&p)
		if err != nil {
			return
		}
		canonical, err := opts.ToProto()
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := VectorIndexScanOptionsFromProto(canonical)
		if err != nil {
			t.Fatal(err)
		}
		again, err := decoded.ToProto()
		if err != nil || !proto.Equal(canonical, again) {
			t.Fatalf("unstable canonical wire form: %v, %v, %v", canonical, again, err)
		}
	})
}
