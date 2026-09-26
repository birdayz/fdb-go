# WS-J oracle

Live-JVM measurement of index-definition fidelity for RFC-257 WS-J
(`../ws-j-design.md`). Spec: `conformance/ws_j_index_fidelity_conformance_test.go`,
target `//conformance:rfc257_oracle_test`; helpers shared with the D11 check in
`conformance/index_shape_helpers_test.go` (both targets compile it).

1. `harvest.py` — rebuilds `conformance/testdata/rfc257/java_index_templates.json`
   from a Java checkout at the target tag: every `schema_template:` block (and
   versioned `definition:` variant) and every `create schema template` query step
   that declares a `CREATE [UNIQUE|VECTOR] INDEX`, plus a statement CENSUS it refuses
   to write without: every such occurrence in every `.yamsql` file is inside a
   harvested body or on a comment line. At 4.14.2.0 (`fdacd162a`): 81 templates, 352
   statements, 77 files; 360 occurrences = 352 in bodies + 8 on comment lines. (v1's
   figures, 75 / 328 / 71, missed the seven vector-index files; its positive control
   `grep -rli 'create (unique )?index'` is basic regex, where `(` and `?` are literal,
   so it could not have matched as written.)
2. The "WS-J index-definition fidelity oracle" spec runs 479 bodies, all derived
   STRUCTURALLY from the corpus (never from either engine's outcome): 81 corpus
   templates, 21 `{-views-functions}` bodies (views, functions and the indexes that
   depend on them cut along the typed parse tree), 314 `{index:<name>}` isolation
   bodies (one per index of every multi-index template) and 63 hand-written shapes.
   Every Java outcome is pinned in `conformance/testdata/rfc257/wsj_java_pins.json`
   (a digest of the WHOLE canonical stored MetaData, section 0 of the design), with
   set equality, unique run ids, the four grammar-unsplittable templates pinned, and
   the census asserted (`variant_duplicates == 0`). Every run's Go CLASS and Go's OWN
   OUTCOME are pinned too, in `conformance/testdata/rfc257/wsj_go_classes.json` (the
   ratchet: `<class> OK <digest of Go's whole canonical MetaData>` or `<class> ERROR
   <Go SQLSTATE> <digest of its message>`), every body Go accepts is built twice
   in-process and the two stored byte strings asserted equal (`WSJ-GO-BUILT-TWICE`), with a
   `both-accept-companion` class for runs equal once their verified RFC-209
   companions are removed (design section 0). Output line kinds: `WSJ` (one per run,
   with its class), `WSJ-CLASS` (the class pin map),
   `WSJ-DETAIL`, `WSJ-CUT`, `WSJ-JAVA-ROOT`, `WSJ-JAVA` (the pin map), `WSJ-TALLY`,
   `WSJ-UNSPLITTABLE`.
3. The "WS-J literal-width query oracle" spec runs 24 bitmap/bit-operator probes (six
   of them on a second schema with ENUM, UUID and ARRAY columns) and
   pins the target's result type, exact value or rejection of each (`WSJQ`,
   `WSJQ-JAVA`).
4. `evidence-run.txt` — the v1 full-target run (16/16 specs), superseded by
   `evidence-v2.txt` (item 8) for everything v2 claims.
5. `evidence-mutations.txt` — the v1 pin, set and trie-fix mutations, and the v2
   instrument mutations (census floor, unsplittable pin, digest template-name scrub,
   anonymous-run ordering), each shown to redden.
6. The "WS-J Go-stored template planned by the target" spec stores one template
   three ways — through the Go SQL driver, through Go's catalog library at the
   Java-compatible `(NULL, NULL, 0)` catalog subspace, and through Java. The target
   runs 15 reads over the library-stored and the Java-stored copies and over a
   fourth copy, the library-stored metadata with every INT literal rewritten to
   `long_value` (the pre-F2 width); the driver-stored copy gets ONE read, which is
   refused. Nine of the 15 reads are asserted to tell the widths apart: the three
   bitmap reads, the EXPLAINs of the two index-served equalities, and four reads of
   T1 that no bitmap expression serves, which the target also fails over the
   `long_value` copy (a stored key it cannot expand fails every query of its
   table). The driver-stored template is
   pinned invisible to the target (42F55; TODO.md "Go SQL driver stores the
   relational catalog and user schemas on a Go-only keyspace"). The target's
   answers over its own template are pinned, and over the Go-stored one must be
   identical.
7. `evidence-f2.txt` — the literal-carrier-width fix (F2): the Go-stored reads
   before (XX000 "unable to encapsulate arithmetic operation" on every bitmap
   read) and after, the six index-fidelity rows that became byte-equal, and the
   mutations that redden.
8. `evidence-v2.txt` — two uncached runs of the five WS-J specs on the hashed bytes:
   every WS-J line of run 5, the Java pins holding in both, and the five Go-side lines
   that differ between the runs, all explained by F12 (option order).
9. The "WS-J long arithmetic key functions over non-integer operands" spec: 27 pinned
   target answers over `d + d` (DOUBLE), `c * 1.5` and `f + 1` (FLOAT) indexes (inserts
   succeed, index-served equalities return no row) and over `i + 1` on an INTEGER column
   holding 2147483647 (the index serves `= 6` past the overflowing row, per-row reads
   fail with the overflow, and `SELECT i + 1 ... ORDER BY i + 1` is unplannable in the
   target, and six coverage reads, v6), and Go's 27 answers and plans pinned beside them
   (`wsjNonIntGoPins`: Go answers from the record; design section 3.2,
   DIVERGENCES.md).
10. The "WS-J F2b index entries written by both engines" spec: the target and the Go
   record layer write the same rows into ONE store; every entry's raw key bytes (hex)
   are checked to end in its primary key's tuple encoding, the bytes before it are
   compared byte for byte between the two engines' rows and pinned for `d + d`, and the
   `n + 1` entries are compared with the tuple encoding of `n + 1`; the target serves a
   Go-written row through the index. Its second spec (v7) indexes `d + 0` and `f + 0`,
   which cannot overflow, so their entries are the saturated operand itself: ±∞, ±9.3e18,
   ±1e300 and ±3e38f store Long.MAX_VALUE or Long.MIN_VALUE by sign, NaN 0, -1.5 -1,
   byte-equal between the engines (a Go sign-flip mutation reddens only this spec).
11. The "WS-J nested-leaf value index plan oracle" spec: 6 reads over value indexes on
   nested leaves, the target's EXPLAIN and Go's physical EXPLAIN (through the SQL
   runner) pinned (design section 3.5).
12. `evidence-v3.txt` — two uncached passes of every WS-J spec on the hashed bytes of
   the v3 tree, plus the unit and FDB pins run under Bazel; `evidence-mutations.txt`
   gains the v3 mutations (option order, class ratchet, production-path pin, the float
   arm).
13. The "WS-J duplicate index option" spec: the target reads stored Index protos whose
   option list repeats a key and pins its IllegalArgumentException messages, the inputs
   and expected messages of Go's `TestIndexOptionOrder_FromProtoRefusesDuplicateKey`
   (design section 4b).
14. `evidence-v4.txt` — two uncached passes of every WS-J spec on the hashed bytes of
   the v4 tree (every landed WS-J file and test hashed, not only the oracle's), plus the
   unit, JVM and FDB pins run under Bazel; `evidence-mutations.txt` gains the v4 block,
   each mutation with its text and presence count (the widening arm in each direction,
   the rebind option, the CLI flag, the union-name refusal, the first-fault Build, the
   signed readers, the carrier's Go kinds, the digest and double-build pins).
15. `evidence-v5.txt` — two uncached passes of every WS-J spec on the hashed bytes of the v5
   tree (48 files), the JVM records-descriptor spec (23 files, the load order and shape (d)
   among them), and two `just test` runs whose executed sets are listed; `evidence-mutations.txt`
   gains the v5 block (the one setRecords guard and name check, the loader's order and message,
   the ambiguous-legacy-union refusal, the carrier's unknown pairs, the duplicate-option
   message driven through Go's loader, the INT-edge pin, the FLOAT operand arm, and the F12
   revert with how many class pins and digests it moves). The oracle gains four `top_*`
   shapes (479 runs), the INT-edge ordered projection (21 non-integer reads) and FLOAT
   columns in the F2b spec.
16. `evidence-v6.txt` — design v6: two uncached passes of every WS-J spec on the hashed bytes of
   49 files (TODO.md added), the JVM records-descriptor spec (29 shapes: the load order per
   index and for RecordType entries, a record type and a nested message named
   UnionDescriptor that both engines load, and shape (d2) measured on the target), and
   `just test` (43 of 94 executed, all green). The oracle gains the spec "WS-J a table or
   struct named UnionDescriptor" (the target creates both; Go loads the target's stored
   metadata and its own build), six coverage reads of the long-arithmetic probe (an
   index-served equality fetches the record and recomputes, pinned on both engines) and
   four F2b rows where only the FLOAT column saturates or overflows. `evidence-mutations.txt`
   gains the v6 block (the field-type condition, the RecordType refusal, the v5 index order
   re-created, the empty union name and the untyped literal, each red).
17. `evidence-v7.txt` — design v7: two uncached passes of every WS-J spec on the hashed bytes of the same 49 files,
   the JVM records-descriptor spec (31 shapes: v7 adds a legacy UnionDescriptor held by an envelope message and one
   only an unreached message holds, both refused by Go, loaded by the target), and `just test` (43 of 94 executed,
   all green). The spec "WS-J a table or struct named UnionDescriptor" now runs through Go's catalog library and SQL
   driver; the F2b Describe gains the saturated-operand spec (`d + 0`, `f + 0`). `evidence-mutations.txt` gains the
   v7 block.
18. `evidence-v8.txt` — design v8: two uncached passes of every WS-J spec on the hashed bytes of the WS-J and WS-C
   files (66), the JVM records-descriptor spec (35 shapes: v8 adds a legacy UnionDescriptor held by a record type
   and a table-typed column, refused by Go and loaded by the target, and a STRUCT-typed column and an indexed
   table-typed column that both load), and `just test`. New specs: "WS-J stored index protos read as Java reads
   them" (five Index protos with `value_expression`, absent `added_version` and both, read by the JVM and by Go's
   loader), "WS-J a column typed by a table" (five DDL shapes the target creates: two refused by Go's catalog and
   driver with 0A000, three loaded by both), and "WS-J bit and bitmap index keys over an operand with no lane" (ten
   DDL shapes the target refuses at the clause; Go's outcomes recorded). `evidence-mutations.txt` gains the v8 block.
19. `evidence-v9.txt` — design v9: two uncached passes of every WS-J spec, the JVM records-descriptor spec, the WS-C
   revision 5 focus and its 40-run loop, and `just test` (43 of 94 executed, all green), on the hashed bytes of the
   whole change set against HEAD (1602 files, the WS-E design and oracle excepted). Changed specs: "WS-J stored index
   protos read as Java reads them" asserts the options the JVM returns (the `index_type` UNIQUE case's
   `{unique=true}`); "WS-J bit and bitmap index keys over an operand with no lane" pins Go's outcome on this tree
   (`goNow`: eight OK, two XX000); "Java refuses the same records descriptors" asserts the v9 remedy text. The corpus
   tally is unchanged, and the file now quotes it with the `md-canonical-differ` counts. `evidence-mutations.txt`
   gains the v9 block (the v8 unit mutations re-run under Bazel, and three for the v9 pins).
20. `evidence-v10.txt` — design v10: two uncached passes of every WS-J spec (31, four more than v9), the JVM
   records-descriptor spec, the WS-C revision 6 focus and its 40-run loop, and `just test` (41 of 94 executed, all
   green), on the hashed bytes of the whole change set (1626 files, the WS-E design, oracle and running gate
   excepted). New or changed: WSJTT's two shapes the replay names (a BIGINT column named `_A`, a table named "UUID"),
   refused by Go with the SQL remedy; WSJLANE's two-fault shape (a later table's 42F18 in both engines); WSJIX's
   `index_type` beside a stored option list, and its malformed subspace keys, refused by both engines with Java's
   classes and messages. `evidence-mutations.txt` gains the v10 block.
