# ORDER alias phase correction — verification pending

Supersedes ordinary inherited-name eligibility in tree e83b606c1d79a24a1bf08a5c35c9302f0777b77a.
Actual prior Graefe/independent NAK and Torvalds ACK are retained in ../alias-ambiguity-reviews/.
Ordinary source identifiers retain qualification: only authored AS aliases participate.
Grouped pull-up clears qualifiers: inherited grouped output names still participate.
Named ORDER duplicates are validated after semantic resolution; ambiguity wins.

Live Java 4.14.2.0 probes (35 GROUP-ALIAS-PROBE records in the accompanying log)
pin ordinary/grouped, nested, and duplicate diagnostic precedence. Two previous
Go test oracles incorrectly accepted ambiguous grouped output names. Their EXACT
queries remain tested for 42702 / Ambiguous alias A. Positive row/slot assertions
remain using ORDER BY 1 or ORDER BY m.a, not dropped or relaxed.

The earlier alias-owner-duplicate-mutant-inherited-names result is INVALID as
correctness evidence: its sole failure was the incorrect ordinary inherited-name
expectation. Do not count that mutation. Other earlier mutations remain scoped
to their original trees and are not claimed rerun here.

Three final-phase mutations were each verified applied, compiled, killed by test
assertions and restored (see accompanying script, JSON and logs): ordinary alias
eligibility, early named-duplicate rejection, and ignoring valid-name duplicate
errors. Initial early-duplicate attempt failed gofumpt at build time; it executed
zero tests and is NOT credited as a killed mutation. The second, formatted attempt
is the compiled test failure recorded here.

Latest full affected-package execution: embedded + sqldriver, two uncached targets
passed; population {"run": 9115, "passed": 9115, "skip": 0, "failed": 0}. Full final suite, race, fuzz, just test,
ABBA stress and implementation delta review remain pending. No whole-upgrade
completion claim; WS-B–K remain open. No commit/push/merge authorization.

Superseded by ../alias-source-closure/README.md: AS-only eligibility is false for unnamed VALUES sources; this file records the earlier candidate, not current semantics.
