---
name: Use gomega assertions in all new tests
description: All new tests must use gomega assertions (Expect, Eventually) even in non-Ginkgo tests. No manual if/t.Fatal patterns.
type: feedback
---

Use gomega assertions in ALL new tests, even non-Ginkgo `testing.T` tests.

- `Expect(err).ToNot(HaveOccurred())` instead of `if err != nil { t.Fatal(err) }`
- `Expect(len(kvs)).To(Equal(100))` instead of manual length checks
- `Eventually(func() int { ... }).Should(BeNumerically(">", 1))` for polling/waiting
- Register gomega with `RegisterTestingT(t)` at the start of each test function

**Why:** Consistent, readable assertions. `Eventually` is cleaner than hand-rolled poll loops. Gomega is already a dependency (used by Ginkgo in recordlayer tests).

**How to apply:** Any new test file or test function. Don't rewrite existing tests unless actively modifying them.
