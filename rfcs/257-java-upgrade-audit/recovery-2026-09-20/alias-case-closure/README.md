# Quoted ORDER alias identity

Repairs independent NAK on tree15c853e0f15c1daed5214da4998308e30c577ed7;
actual Graefe/Torvalds ACKs and independent NAK retained in ../alias-resolved-reviews/.
UNION cardinality now keys normalized labels exactly. Legacy raw protobuf labels
retain relaxed lookup only if exact lookup finds nothing. Both constructors pin
quoted x/X to distinct ordinal owners, mixed orders, unquoted x -> normalized X,
repeated keys42701 and missing-before-duplicate42703. Existing true ambiguity,
quoted dotted versus qualified labels, projection elision and normalization stay
pinned. Ordinary SELECT shared alias lookup and named duplicate comparison had
the same case-folding bug; both now use exact normalized identities. Its test
checks exact projected Value identity (Pos is intentionally consumed/cleared for
plain SELECT), not the UNION-only surviving Pos carrier.

Java SemanticAnalyzer normalization preserves quoted case and lookupAlias uses
Identifier.equals. Its duplicate checker groups by Identifier identity. Java56
probe records PASS, including three indexed positive controls accepting quoted
lower/upper/mixed names. Three discriminating multirow UNION probes explicitly
retain existing RFC-180 extensions: Java sorts only its right leg (QueryVisitor
visitSetQuery) and refuses unavailable ordering with0AF00; Go sorts the combined
result and has an in-memory fallback. DIVERGENCES.md already documents both.
No new exception or relaxation: Java results are recorded exactly, and real-FDB
Go tests assert discriminating complete ordered rows and case-sensitive labels.

Nine applied/compiled/killed/restored mutations include the six prior contract
arms plus quoted UNION identity, ordinary SELECT identity, and duplicate identity.
Initial strict UNION lookup exposed raw protobuf lower-case labels in existing
TestSortNeverSitsOverAProjection_UnionBuilders; exact-first relaxed lookup fixes
those without conflating exact quoted owners. The complete five affected targets
passed uncached after restoring mutations; population {"run": 9906, "passed": 9906, "skip": 0, "failed": 0}.
Full explaindiff golden unchanged. No assertions weakened, no corpus admissions,
no new skips. The prior full suite was cleanly interrupted at NAK and hashes
checked unchanged; it is not credited as a green. Final frozen verification and
delta re-ACKs pending. No commit/push/merge; WS-B–K remain open.

Final verification COMPLETE at code tree `0dedbe1b1083358fb9568a31178084c043fa31e0`. Three implementation ACKs retained in ../alias-case-reviews/. Full93/race15/just-test/fuzz/ABBA passed with frozen hashes unchanged. See verification.json and stress-table.md; TODO.md/RFC257 final appended booking records scope and remaining gates.
