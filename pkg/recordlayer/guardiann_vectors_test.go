package recordlayer

import (
	"encoding/hex"
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/vectorcodec"
)

// Java 4.14.2.0 values produced by testdata/GuardiannVectors.java.
func TestGuardiannVectorCoordinates(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		metric            VectorMetric
		encoded, returned string
		distance          float64
	}{
		{VectorMetricEuclidean, "0340218a6f8ff36398bfd24f6f0e0ad5b63fc34edb3de0b770fb7a", "023fee714ee8af20254000392c4f941c064007fca06b941fd5", 0.8784059169417581},
		{VectorMetricCosine, "033fcf2fd2fc8859b7bfab001c1538fc2f3fa40a3a849a8436fe9e", "023fd185e01893a3833fe16f5bdf7c97fd3fe9e3d66a0701dc", 0.0022804748709671363},
		{VectorMetricInnerProduct, "0340218a6f8ff36398bfd24f6f0e0ad5b63fc34edb3de0b770fb7a", "023fee714ee8af20254000392c4f941c064007fca06b941fd5", -0.6142015225408546},
		{VectorMetricEuclideanSquare, "0340218a6f8ff36398bfd24f6f0e0ad5b63fc34edb3de0b770fb7a", "023fee714ee8af20254000392c4f941c064007fca06b941fd5", 0.7715969549182908},
	} {
		cfg := defaultGuardiannConfig(3)
		cfg.useRaBitQ, cfg.metric = true, tc.metric
		c, err := newGuardiannVectorCodec(cfg, &guardiannAccessInfoValue{rotatorSeed: 42, negatedCentroid: []float64{-0.25, 0.5, -0.75}})
		if err != nil {
			t.Fatal(err)
		}
		plain := vectorcodec.SerializeHalf([]float64{1, 2, 3})
		v, err := c.decode(plain)
		if err != nil {
			t.Fatal(err)
		}
		encoded := c.encode(v)
		if got := hex.EncodeToString(encoded); got != tc.encoded {
			t.Fatalf("metric %d encoded %s want %s", tc.metric, got, tc.encoded)
		}
		v, err = c.decode(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(c.encode(v)); got != tc.encoded {
			t.Fatalf("encoded vector was quantized twice: %s", got)
		}
		returned := c.toClientCoordinates(v)
		if got := hex.EncodeToString(returned.encode()); got != tc.returned {
			t.Errorf("metric %d returned %s want %s", tc.metric, got, tc.returned)
		}
		q, err := c.toStoredCoordinates(gVector{data: []float64{1.5, 2.5, 3.5}, typ: 2})
		if err != nil {
			t.Fatal(err)
		}
		for _, operands := range [][2]gVector{{q, v}, {v, q}} {
			got, err := c.distance(operands[0], operands[1])
			if err != nil || math.Abs(got-tc.distance) > 1e-14 {
				t.Errorf("metric %d distance %.17g want %.17g: %v", tc.metric, got, tc.distance, err)
			}
		}
		untrained, err := newGuardiannVectorCodec(cfg, &guardiannAccessInfoValue{rotatorSeed: -1})
		if err != nil {
			t.Fatal(err)
		}
		v, err = untrained.decode(plain)
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(untrained.encode(v)); got != hex.EncodeToString(plain) {
			t.Fatalf("untrained HALF precision changed: %s", got)
		}
	}
}
