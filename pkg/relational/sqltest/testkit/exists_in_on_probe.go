package testkit

// Adversarial probes around EXISTS-in-ON (RFC-154 Phase 2a) — feature-edge
// stress: multiple EXISTS conjuncts, EXISTS-in-ON alongside WHERE-EXISTS,
// uncorrelated EXISTS, and a 3-way join with EXISTS-in-ON. Each asserts a
// hand-computed row set; a crash / planner error / wrong rows here is a bug to
// fix (or a clean rejection to pin).

func SortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
