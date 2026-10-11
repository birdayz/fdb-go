package bench

import "fmt"

// Native libfdb_c does not expose these counters through its public C API.
// Unobserved is distinct from an observed zero.
type ClientCounters struct {
	Observation string           `json:"observation"`
	Values      map[string]int64 `json:"values,omitempty"`
}

func CounterDelta(before, after ClientCounters) (ClientCounters, error) {
	if before.Observation == "unobserved" && after.Observation == "unobserved" {
		return ClientCounters{Observation: "unobserved"}, nil
	}
	if before.Observation != "observed" || after.Observation != "observed" {
		return ClientCounters{}, fmt.Errorf("inconsistent counter observation: %q -> %q", before.Observation, after.Observation)
	}
	delta := ClientCounters{Observation: "observed", Values: make(map[string]int64, len(after.Values))}
	if len(before.Values) != len(after.Values) {
		return delta, fmt.Errorf("counter population changed")
	}
	for name, value := range after.Values {
		previous, ok := before.Values[name]
		if !ok || previous < 0 || value < previous {
			return delta, fmt.Errorf("invalid counter %s: %d -> %d (previous observed=%t)", name, previous, value, ok)
		}
		delta.Values[name] = value - previous
	}
	for _, name := range []string{"grv_cache_hits", "read_versions_completed", "grv_successful_batch_replies"} {
		if _, ok := delta.Values[name]; !ok {
			return delta, fmt.Errorf("required counter %s unobserved", name)
		}
	}
	if delta.Values["grv_cache_hits"] != 0 {
		return delta, fmt.Errorf("GRV cache used in uncached workload: %d hits", delta.Values["grv_cache_hits"])
	}
	return delta, nil
}

func ValidateGRVWorkload(delta ClientCounters, workload string, completed int) error {
	if delta.Observation == "unobserved" {
		return nil
	}
	versions, versionsObserved := delta.Values["read_versions_completed"]
	batches, batchesObserved := delta.Values["grv_successful_batch_replies"]
	if delta.Observation != "observed" || !versionsObserved || !batchesObserved {
		return fmt.Errorf("missing GRV workload counters")
	}
	if workload == "ryw" {
		if versions != 0 || batches != 0 {
			return fmt.Errorf("local RYW issued GRV: versions=%d batches=%d", versions, batches)
		}
	} else if versions < int64(completed) || batches <= 0 {
		return fmt.Errorf("remote workload missing GRV evidence: completed=%d versions=%d batches=%d", completed, versions, batches)
	}
	return nil
}
