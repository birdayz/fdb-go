package embedded

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
)

// RFC-209 §5.2 — the DDL emits a create-if-absent COUNT(*) companion for every
// GROUPED SUM / COUNT(col) aggregate index, and reserves the derived suffix on
// user-supplied index names.
//
// The companion is the only thing that can decide whether a group exists: a
// SUM's stored zero is byte-identical whether the group cancelled to zero or
// was vacated, and an all-NULL group has no SUM entry at all. Without the
// companion the read path (§5.3) has nothing to drive the merge from and must
// fail closed to streaming aggregation.

func buildIndexes(t *testing.T, ddl string) map[string]*recordlayer.Index {
	t.Helper()
	tmpl, err := BuildSchemaTemplateFromDDL(ddl)
	if err != nil {
		t.Fatalf("BuildSchemaTemplateFromDDL: %v", err)
	}
	return tmpl.Underlying().GetAllIndexes()
}

func indexNameList(indexes map[string]*recordlayer.Index) []string {
	out := make([]string, 0, len(indexes))
	for name := range indexes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// TestGroupCountCompanion_EmittedForGroupedSumAndCountCol pins that both owner
// shapes get a companion, that it is a grouped COUNT(*) index (record-layer
// type "count", zero grouped columns) and that it groups by the SAME key the
// owner groups by — matched on the normalized grouping signature, which is what
// the read side matches on, not on the name.
func TestGroupCountCompanion_EmittedForGroupedSumAndCountCol(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		owner string
		ddl   string
	}{
		{"sum", "AI_SUM_G", "CREATE INDEX ai_sum_g AS SELECT SUM(v) FROM ai GROUP BY g"},
		{"count-col", "AI_CNTV_G", "CREATE INDEX ai_cntv_g AS SELECT COUNT(v) FROM ai GROUP BY g"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			indexes := buildIndexes(t,
				"CREATE TABLE ai (pk BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (pk)) "+tc.ddl)

			owner, ok := indexes[tc.owner]
			if !ok {
				t.Fatalf("owner index %s missing; have %v", tc.owner, indexNameList(indexes))
			}
			wantName := recordlayer.GroupCountCompanionName(tc.owner)
			companion, ok := indexes[wantName]
			if !ok {
				t.Fatalf("no group-existence companion emitted for %s.\n  have: %v\n"+
					"Without it the read path has no group-existence source and every "+
					"grouped %s query must fall back to streaming aggregation (RFC-209 5.3(c)).",
					tc.owner, indexNameList(indexes), tc.name)
			}
			if companion.Type != recordlayer.IndexTypeCount {
				t.Fatalf("companion %s has type %q, want %q — only a COUNT(*) index counts "+
					"rows independently of the aggregated value, which is the whole reason "+
					"it can decide existence", wantName, companion.Type, recordlayer.IndexTypeCount)
			}
			cgke, ok := companion.RootExpression.(*recordlayer.GroupingKeyExpression)
			if !ok {
				t.Fatalf("companion %s root is %T, want *GroupingKeyExpression", wantName, companion.RootExpression)
			}
			if cgke.GetGroupedCount() != 0 {
				t.Fatalf("companion %s has %d grouped columns, want 0: a COUNT(*) aggregates "+
					"no column, so its whole key is the grouping", wantName, cgke.GetGroupedCount())
			}
			ogke, ok := owner.RootExpression.(*recordlayer.GroupingKeyExpression)
			if !ok {
				t.Fatalf("owner %s root is %T, want *GroupingKeyExpression", tc.owner, owner.RootExpression)
			}
			ownerSig := recordlayer.GroupingSignature(ogke)
			companionSig := recordlayer.GroupingSignature(cgke)
			if len(ownerSig) == 0 || string(ownerSig) != string(companionSig) {
				t.Fatalf("companion groups by a DIFFERENT key than its owner.\n  owner:     %x\n"+
					"  companion: %x\nThe read side matches structurally on this signature, so a "+
					"mismatch means the companion can never be found and the emission is dead metadata.",
					ownerSig, companionSig)
			}
		})
	}
}

// TestGroupCountCompanion_CreateIfAbsent pins that a user-declared COUNT(*)
// over the same grouping key satisfies the requirement and no duplicate is
// emitted — matching is structural, so the user must not pay for a second index
// maintaining the identical counter.
func TestGroupCountCompanion_CreateIfAbsent(t *testing.T) {
	t.Parallel()
	indexes := buildIndexes(t,
		"CREATE TABLE ai (pk BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
			"CREATE INDEX ai_sum_g AS SELECT SUM(v) FROM ai GROUP BY g "+
			"CREATE INDEX ai_cntv_g AS SELECT COUNT(v) FROM ai GROUP BY g "+
			"CREATE INDEX ai_cnt_g AS SELECT COUNT(*) FROM ai GROUP BY g")

	for _, name := range indexNameList(indexes) {
		if strings.HasSuffix(name, recordlayer.GroupCountCompanionSuffix) {
			t.Fatalf("emitted companion %s although the user already declared AI_CNT_G, a "+
				"COUNT(*) over the same grouping key.\n  have: %v\nMatching is structural: the "+
				"declared index already serves, so this is a duplicate counter maintained on "+
				"every write for nothing.", name, indexNameList(indexes))
		}
	}
	if _, ok := indexes["AI_CNT_G"]; !ok {
		t.Fatalf("the user-declared COUNT(*) vanished; have %v", indexNameList(indexes))
	}
}

// TestGroupCountCompanion_SparseOwnerGetsSparseCompanion pins the divergence
// from the RFC text (§5.2 defines the structural match on the grouping key
// alone): a SPARSE owner's companion must carry the SAME predicate.
//
// A dense COUNT(*) counts rows the sparse owner never indexed, so its key set
// contains groups holding no qualifying row. Driving the merge from it would
// INVENT groups — precisely the over-approximation the companion exists to
// remove, reintroduced through the back door.
func TestGroupCountCompanion_SparseOwnerGetsSparseCompanion(t *testing.T) {
	t.Parallel()

	t.Run("companion carries the predicate", func(t *testing.T) {
		t.Parallel()
		indexes := buildIndexes(t,
			"CREATE TABLE ai (pk BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
				"CREATE INDEX ai_sum_g AS SELECT SUM(v) FROM ai WHERE v > 3 GROUP BY g")
		owner := indexes["AI_SUM_G"]
		companion := indexes[recordlayer.GroupCountCompanionName("AI_SUM_G")]
		if owner == nil || companion == nil {
			t.Fatalf("missing owner or companion; have %v", indexNameList(indexes))
		}
		ownerPred := recordlayer.PredicateSignature(owner)
		if len(ownerPred) == 0 {
			t.Fatalf("the owner index is not sparse — this test no longer covers the sparse " +
				"dimension; fix the fixture, do not relax the assertion")
		}
		companionPred := recordlayer.PredicateSignature(companion)
		if !recordlayer.SamePredicateSignature(ownerPred, companionPred) {
			t.Fatalf("sparse owner's companion carries a DIFFERENT predicate.\n"+
				"  owner:     %x\n  companion: %x\nIt therefore counts a different row "+
				"population, and every group it lists that the owner never saw becomes an "+
				"invented row in the merged answer.", ownerPred, companionPred)
		}
	})

	t.Run("a dense COUNT(*) does not satisfy a sparse owner", func(t *testing.T) {
		t.Parallel()
		indexes := buildIndexes(t,
			"CREATE TABLE ai (pk BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
				"CREATE INDEX ai_sum_g AS SELECT SUM(v) FROM ai WHERE v > 3 GROUP BY g "+
				"CREATE INDEX ai_cnt_g AS SELECT COUNT(*) FROM ai GROUP BY g")
		if _, ok := indexes[recordlayer.GroupCountCompanionName("AI_SUM_G")]; !ok {
			t.Fatalf("create-if-absent accepted the DENSE AI_CNT_G as the sparse owner's "+
				"companion and emitted nothing.\n  have: %v\nThe dense index counts rows "+
				"WHERE v <= 3 too, so its group set is strictly larger than the sparse "+
				"owner's population.", indexNameList(indexes))
		}
	})
}

// TestGroupCountCompanion_NoDuplicatesAcrossRepeatedBuilds pins the reason the
// companion is computed LOCALLY inside Build() and never appended to the
// builder's declared-index list.
//
// parseAsSelectIndexDefinition calls Build() once per AS-SELECT index, against
// the metadata registered so far. A companion appended to the declared list
// would survive into the next Build() and be re-derived on top of itself, so a
// template with N aggregate indexes would accumulate companions quadratically.
// Three AS-SELECT indexes means Build() runs at least three times before the
// final one.
func TestGroupCountCompanion_NoDuplicatesAcrossRepeatedBuilds(t *testing.T) {
	t.Parallel()
	indexes := buildIndexes(t,
		"CREATE TABLE ai (pk BIGINT, g BIGINT, h BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
			"CREATE INDEX ai_sum_g AS SELECT SUM(v) FROM ai GROUP BY g "+
			"CREATE INDEX ai_sum_h AS SELECT SUM(v) FROM ai GROUP BY h "+
			"CREATE INDEX ai_cntv_g AS SELECT COUNT(v) FROM ai GROUP BY g")

	// AI_SUM_G and AI_CNTV_G share a grouping key, so ONE companion serves both;
	// AI_SUM_H needs its own.
	var companions []string
	for _, name := range indexNameList(indexes) {
		if strings.HasSuffix(name, recordlayer.GroupCountCompanionSuffix) {
			companions = append(companions, name)
		}
	}
	if len(companions) != 2 {
		t.Fatalf("expected exactly 2 companions (one per distinct grouping key), got %d: %v\n"+
			"  all indexes: %v\nMore than one per grouping key means Build()'s companions "+
			"leaked into the declared-index list and re-derived on the next parse; fewer "+
			"means a grouping key went unserved.", len(companions), companions, indexNameList(indexes))
	}
	seen := map[string]bool{}
	for _, name := range companions {
		gke := indexes[name].RootExpression.(*recordlayer.GroupingKeyExpression)
		sig := string(recordlayer.GroupingSignature(gke))
		if seen[sig] {
			t.Fatalf("two companions share a grouping signature: %v — create-if-absent did "+
				"not consult the companions emitted earlier in the same Build()", companions)
		}
		seen[sig] = true
	}
}

// TestGroupCountCompanion_NotEmittedWhereItCannotHelp pins the shapes that must
// NOT get a companion. Each would be dead metadata maintained on every write.
func TestGroupCountCompanion_NotEmittedWhereItCannotHelp(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		ddl  string
		why  string
	}{
		{
			"ungrouped sum",
			"CREATE INDEX ai_sum AS SELECT SUM(v) FROM ai",
			"an ungrouped aggregate has exactly one group, which exists whether or " +
				"not the table has rows (RFC-209 5.4)",
		},
		{
			"grouped count-star",
			"CREATE INDEX ai_cnt AS SELECT COUNT(*) FROM ai GROUP BY g",
			"a COUNT(*) index is its own group-existence oracle — a stored zero can " +
				"only be a vacated group, which 5.3(a) drops at scan time",
		},
		{
			"grouped min",
			"CREATE INDEX ai_min AS SELECT MIN(v) FROM ai GROUP BY g",
			"MIN/MAX map to PERMUTED_MIN/PERMUTED_MAX, which keep a real per-record " +
				"entry that the record's deletion removes — exact already",
		},
		{
			"plain value index",
			"CREATE INDEX ai_v AS SELECT v FROM ai ORDER BY v",
			"a value index aggregates nothing",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			indexes := buildIndexes(t,
				"CREATE TABLE ai (pk BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (pk)) "+tc.ddl)
			for _, name := range indexNameList(indexes) {
				if strings.HasSuffix(name, recordlayer.GroupCountCompanionSuffix) {
					t.Fatalf("emitted companion %s for %s: %s", name, tc.name, tc.why)
				}
			}
		})
	}
}

// TestGroupCountCompanion_ReservedSuffixRejectedOnEveryCreatePath pins the
// user-visible DDL restriction on all three index-creation grammars. Without
// it, create-if-absent can collide with a pre-existing user index carrying the
// derived name but a different structure, and the failure surfaces as a
// confusing duplicate-name error at schema-creation time rather than at the
// point the user chose the name.
func TestGroupCountCompanion_ReservedSuffixRejectedOnEveryCreatePath(t *testing.T) {
	t.Parallel()

	const table = "CREATE TABLE ai (pk BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (pk)) "
	for _, tc := range []struct {
		name string
		ddl  string
	}{
		{"as-select", table + "CREATE INDEX ai_x__GROUP_COUNT AS SELECT COUNT(*) FROM ai GROUP BY g"},
		{"on-source", table + "CREATE INDEX ai_x__GROUP_COUNT ON ai (g)"},
		{"lowercase spelling", table + "CREATE INDEX \"ai_x__group_count\" ON ai (g)"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := BuildSchemaTemplateFromDDL(tc.ddl)
			if err == nil {
				t.Fatalf("CREATE accepted a name ending in the reserved suffix %q",
					recordlayer.GroupCountCompanionSuffix)
			}
			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeInvalidName {
				t.Fatalf("wrong error for a reserved index name: %v\nwant SQLSTATE %s "+
					"(invalid name), not a generic template or duplicate-name failure",
					err, api.ErrCodeInvalidName)
			}
		})
	}
}

// TestGroupCountCompanion_ReservedSuffixIsNotRejectedAtLoad pins that the
// restriction applies at CREATE ONLY. A schema written by Java, or by an older
// Go binary, may already contain such a name; rejecting it at load would let a
// read-side extension render an existing Java-written schema unreadable, which
// no read-side extension may do. Companion matching is structural and the name
// carries no semantic weight, so such a name is inert.
func TestGroupCountCompanion_ReservedSuffixIsNotRejectedAtLoad(t *testing.T) {
	t.Parallel()
	tmpl, err := BuildSchemaTemplateFromDDL(
		"CREATE TABLE ai (pk BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (pk)) " +
			"CREATE INDEX ai_sum_g AS SELECT SUM(v) FROM ai GROUP BY g")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// The emitted companion itself carries the reserved suffix. Round-tripping
	// the stored metadata through proto is the load path, and it must not reject.
	stored, err := tmpl.Underlying().ToProto()
	if err != nil {
		t.Fatalf("ToProto: %v", err)
	}
	reloaded, err := recordlayer.RecordMetaDataFromProto(stored)
	if err != nil {
		t.Fatalf("loading a schema containing a reserved index name failed: %v\n"+
			"The reservation must apply at CREATE only — a Java-written schema carrying "+
			"such a name has to open and read normally.", err)
	}
	name := recordlayer.GroupCountCompanionName("AI_SUM_G")
	if _, ok := reloaded.GetAllIndexes()[name]; !ok {
		t.Fatalf("index %s did not survive the metadata round trip; have %v",
			name, indexNameList(reloaded.GetAllIndexes()))
	}
}
