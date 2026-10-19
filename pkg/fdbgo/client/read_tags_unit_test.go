package client

import (
	"math"
	"testing"
)

func TestSampleReadTags(t *testing.T) {
	t.Parallel()
	draws := 0
	scripted := func(v float64) func() float64 {
		return func() float64 { draws++; return v }
	}
	for _, tc := range []struct {
		name  string
		s     readTagSampling
		want  bool
		draws int
	}{
		{"zero value", readTagSampling{}, false, 0},
		{"rate 0 never draws", readTagSampling{rate: 0, draw: scripted(0)}, false, 0},
		{"negative rate never draws", readTagSampling{rate: -1, draw: scripted(0)}, false, 0},
		{"rate 1 always", readTagSampling{rate: 1, draw: scripted(math.Nextafter(1, 0))}, true, 1},
		{"at the rate", readTagSampling{rate: 0.01, draw: scripted(0.01)}, true, 1},
		{"above the rate", readTagSampling{rate: 0.01, draw: scripted(math.Nextafter(0.01, 1))}, false, 1},
	} {
		draws = 0
		db := &database{readTags: tc.s}
		if got := db.sampleReadTags(); got != tc.want || draws != tc.draws {
			t.Errorf("%s: sampled=%v after %d draws, want %v after %d", tc.name, got, draws, tc.want, tc.draws)
		}
	}
	if readTagSampleRate != 0.01 {
		t.Fatalf("READ_TAG_SAMPLE_RATE = %v, want 0.01 (ClientKnobs.cpp:294)", readTagSampleRate)
	}
	if o := applyOptions([]Option{withReadTagSampling(0.5, nil)}); o.readTagSampling == nil || o.readTagSampling.rate != 0.5 {
		t.Fatal("withReadTagSampling did not reach the open options")
	}
}
