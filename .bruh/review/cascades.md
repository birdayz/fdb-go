---
lens: cascades
applies: ["pkg/recordlayer/query/**", "pkg/relational/core/**"]
references:
  - "Java Record Layer / Relational 4.14.2.0 in fdb-record-layer/ (gitignored checkout; the reference implementation). Planner: fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/query/plan/cascades/"
  - "DIVERGENCES.md: declared differences from Java"
---
The query planner is a port of Java's Cascades planner; Java 4.14.2.0 is the reference.
Behaviour, plan choice and rule structure follow Java unless DIVERGENCES.md declares
otherwise. Cite the Java class and line for every verdict about planner behaviour.

### cascades/java-fidelity — Rules and plans follow Java
Severity: high
Check: a new or changed rule, property, cost comparison or plan operator has a Java
counterpart that does the same thing; Go-only rules and heuristics are suspect and need a
declared reason; the rule fires in the same planner phase (REWRITING vs PLANNING) as in
Java; match-then-implement order is kept.
Verify: find the Java class (grep fdb-record-layer/) and compare.
Not a finding: a Go structure that differs from Java's but produces the same plans and
properties, with tests that show it.

### cascades/properties — Properties derived, not flagged
Severity: high
Check: ordering, distinctness, correlation, cardinality and similar properties are derived
from the expression tree, as Java's property visitors do, not stored as imperative flags or
guessed from structure (child counts, operator names). Physical wrappers propagate
correlation (getCorrelatedTo) like their Java counterparts.
Verify: show the property computed structurally and the Java visitor that derives it.
Not a finding: caching of a derived property that is invalidated correctly.

### cascades/aliases — Quantifier and table aliases are not confused
Severity: high
Check: code comparing aliases compares the same namespace (quantifier aliases q$N versus
table aliases); predicates reference values the quantifier actually produces; merged rows
keep the qualification downstream predicates expect.
Verify: trace the alias from where it is created to where it is compared.
Not a finding: aliases normalized at a documented boundary.

### cascades/plan-tests — Planner changes pin the plan
Severity: medium
Check: a planner change asserts the plan shape (EXPLAIN in yamsql or sqltest) and not only
the rows, which a fallback plan also returns; the yamsql/explain goldens changed by the
change are explained; plan choice is deterministic across runs.
Verify: name the test that only checks rows, or the golden changed without a reason.
Not a finding: executor-only changes covered by row tests.
