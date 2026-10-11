package bench

import "testing"

func counters(hits, versions, batches int64) ClientCounters {
	return ClientCounters{Observation: "observed", Values: map[string]int64{
		"grv_cache_hits":               hits,
		"read_versions_completed":      versions,
		"grv_successful_batch_replies": batches,
	}}
}

func TestCounterEvidence(t *testing.T) {
	t.Parallel()
	before, after := counters(0, 50, 50), counters(0, 150, 75)
	delta, err := CounterDelta(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if delta.Values["read_versions_completed"] != 100 || delta.Values["grv_successful_batch_replies"] != 25 {
		t.Fatalf("bad delta: %+v", delta)
	}
	if err := ValidateGRVWorkload(delta, "get100", 100); err != nil {
		t.Fatal(err)
	}
	if err := ValidateGRVWorkload(delta, "ryw", 100); err == nil {
		t.Fatal("RYW network traffic accepted")
	}
	if err := ValidateGRVWorkload(counters(0, 0, 0), "ryw", 100); err != nil {
		t.Fatal(err)
	}
	if err := ValidateGRVWorkload(counters(0, 0, 0), "get100", 100); err == nil {
		t.Fatal("vacuous remote read accepted")
	}
	if err := ValidateGRVWorkload(counters(0, 99, 25), "get100", 100); err == nil {
		t.Fatal("missing read version accepted")
	}
	if err := ValidateGRVWorkload(counters(0, 100, 0), "get100", 100); err == nil {
		t.Fatal("zero real GRV replies accepted")
	}
	if err := ValidateGRVWorkload(ClientCounters{}, "ryw", 100); err == nil {
		t.Fatal("missing observation treated as zero")
	}
}

func TestCounterVacuityAndCacheGuards(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		before, after ClientCounters
	}{
		{"cache-hit", counters(0, 0, 0), counters(1, 100, 100)},
		{"backwards", counters(0, 100, 100), counters(0, 99, 100)},
		{"empty", ClientCounters{Observation: "observed"}, ClientCounters{Observation: "observed"}},
		{"missing", counters(0, 0, 0), ClientCounters{Observation: "observed", Values: map[string]int64{"grv_cache_hits": 0}}},
		{"mixed", counters(0, 0, 0), ClientCounters{Observation: "unobserved"}},
		{"unset", ClientCounters{}, ClientCounters{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := CounterDelta(tc.before, tc.after); err == nil {
				t.Fatal("invalid evidence accepted")
			}
		})
	}
	delta, err := CounterDelta(ClientCounters{Observation: "unobserved"}, ClientCounters{Observation: "unobserved"})
	if err != nil || delta.Observation != "unobserved" || len(delta.Values) != 0 {
		t.Fatalf("unobserved changed to synthetic counters: %+v %v", delta, err)
	}
	if err := ValidateGRVWorkload(delta, "get100", 100); err != nil {
		t.Fatal(err)
	}
}
