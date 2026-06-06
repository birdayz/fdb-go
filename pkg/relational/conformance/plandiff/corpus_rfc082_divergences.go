package plandiff

// RFC-082 cross-engine divergence annotations (generated, then triaged).
//
// Each entry documents a corpus query whose Go vs Java behaviour is a
// legitimate divergence rather than a parity bug:
//   - JavaErrorsGoCorrect: Java's planner/parser rejects a shape Go supports
//     (LIMIT/OFFSET, in-memory sort, GROUP BY/HAVING/DISTINCT the Cascades
//     front-end of fdb-relational 4.11.1.0 cannot plan, ORDER BY in a
//     subquery, parenthesised/CASE boolean WHERE predicates). Go's rows are
//     SQL-correct; column labels are not wire format (see CLAUDE.md
//     "query reach is not the hard line").
//   - JavaSucceedsGoRejects: Go has a tracked capability gap (UUID equality
//     predicates, EXISTS under OR / multiple EXISTS, derived-table/CTE alias
//     resolution corners) and rejects rather than returning wrong rows.
//   - BothErrorMessagesDrift: both engines reject; only the message wording
//     differs (pinned by a cause-specific substring).
//
// NOTE FOR REVIEWERS: the JavaErrorsGoCorrect set asserts Go is *correct*, not
// merely more permissive. Two generated candidates were withheld as suspected
// Go-too-lenient bugs (boolean = int comparison, CAST LONG->BOOLEAN) and are
// tracked in RFC-082 for a fix-or-accept decision rather than annotated here.
var rfc082Divergences = map[string]Divergence{
	"add_int_overflow_rejected":                  {Direction: DivergenceBothErrorMessagesDrift, Reason: "RFC-082: both engines reject; cosmetic message wording differs", GoErrorContains: "integer overflow"},
	"agg_min_max_with_filter":                    {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: Cascades planner could not plan query", GoExpectedRows: [][]any{{float64(1), float64(20), float64(20)}, {float64(2), float64(30), float64(40)}}},
	"aggregate_group_by_having":                  {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: Cascades planner could not plan query", GoExpectedRows: [][]any{{float64(2), float64(120)}}},
	"arith_bigint_add_overflow":                  {Direction: DivergenceBothErrorMessagesDrift, Reason: "RFC-082: both engines reject; cosmetic message wording differs", GoErrorContains: "integer overflow"},
	"arith_bigint_sub_underflow":                 {Direction: DivergenceBothErrorMessagesDrift, Reason: "RFC-082: both engines reject; cosmetic message wording differs", GoErrorContains: "integer overflow"},
	"bare_bool_where_rejected":                   {Direction: DivergenceBothErrorMessagesDrift, Reason: "RFC-082: both engines reject; cosmetic message wording differs", GoErrorContains: "Cascades planner could not plan query"},
	"bigint_literal_overflow_probe":              {Direction: DivergenceBothErrorMessagesDrift, Reason: "RFC-082: both engines reject; cosmetic message wording differs", GoErrorContains: "Cascades planner could not plan query"},
	"cast_string_to_bigint_in_order_by":          {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: Cascades planner could not plan query", GoExpectedRows: [][]any{{float64(2), "2"}, {float64(1), "10"}, {float64(3), "100"}}},
	"cte_basic_with_aggregate":                   {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: order by is not supported in subquery", GoExpectedRows: [][]any{{float64(1), float64(30)}, {float64(2), float64(30)}}},
	"cte_join_aggregate":                         {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: order by is not supported in subquery", GoExpectedRows: [][]any{{float64(1), float64(30), float64(30)}, {float64(2), float64(30), float64(30)}}},
	"derived_table_projection_alias":             {Direction: DivergenceJavaSucceedsGoRejects, Reason: "RFC-082: Go capability gap (tracked); Go rejects rather than returning wrong rows", GoErrorContains: "column \"S\" does not exist"},
	"distinct_on_join":                           {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: Cascades planner could not plan query", GoExpectedRows: [][]any{{"X"}, {"Y"}}},
	"double_negative":                            {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: Cascades planner could not plan query", GoExpectedRows: [][]any{{float64(1), float64(-3.14)}, {float64(2), float64(2.71)}}},
	"error_ambiguous_column_join":                {Direction: DivergenceBothErrorMessagesDrift, Reason: "RFC-082: both engines reject; cosmetic message wording differs", GoErrorContains: "column reference \"NAME\" is ambiguous"},
	"error_delete_nonexistent_table":             {Direction: DivergenceBothErrorMessagesDrift, Reason: "RFC-082: both engines reject; cosmetic message wording differs", GoErrorContains: "DML Cascades translation failed"},
	"error_duplicate_order_by":                   {Direction: DivergenceBothErrorMessagesDrift, Reason: "RFC-082: both engines reject; cosmetic message wording differs", GoErrorContains: "duplicate column \"V\" in ORDER BY"},
	"error_group_by_violation":                   {Direction: DivergenceBothErrorMessagesDrift, Reason: "RFC-082: both engines reject; cosmetic message wording differs", GoErrorContains: "column \"ID\" must appear in the GROUP BY clause or be used in an aggregate function"},
	"error_insert_arity_too_few":                 {Direction: DivergenceBothErrorMessagesDrift, Reason: "RFC-082: both engines reject; cosmetic message wording differs", GoErrorContains: "INSERT column list has 3 columns but VALUES has 1"},
	"error_insert_arity_too_many":                {Direction: DivergenceBothErrorMessagesDrift, Reason: "RFC-082: both engines reject; cosmetic message wording differs", GoErrorContains: "INSERT column list has 2 columns but VALUES has 5"},
	"error_not_null_violation":                   {Direction: DivergenceBothErrorMessagesDrift, Reason: "RFC-082: both engines reject; cosmetic message wording differs", GoErrorContains: "NULL value in column \"NAME\" violates NOT NULL constraint"},
	"error_type_mismatch_in_list":                {Direction: DivergenceBothErrorMessagesDrift, Reason: "RFC-082: both engines reject; cosmetic message wording differs", GoErrorContains: "The operands of a comparison operator are not compatible."},
	"error_undefined_column_where":               {Direction: DivergenceBothErrorMessagesDrift, Reason: "RFC-082: both engines reject; cosmetic message wording differs", GoErrorContains: "column \"NONEXISTENT\" does not exist"},
	"error_undefined_table_from":                 {Direction: DivergenceBothErrorMessagesDrift, Reason: "RFC-082: both engines reject; cosmetic message wording differs", GoErrorContains: "table \"NOSUCHTABLE\" does not exist"},
	"error_unknown_qualifier_select":             {Direction: DivergenceBothErrorMessagesDrift, Reason: "RFC-082: both engines reject; cosmetic message wording differs", GoErrorContains: "column reference with qualifier \"X\" cannot be resolved"},
	"error_unknown_qualifier_where":              {Direction: DivergenceBothErrorMessagesDrift, Reason: "RFC-082: both engines reject; cosmetic message wording differs", GoErrorContains: "column reference with qualifier \"X\" cannot be resolved"},
	"error_update_nonexistent_col":               {Direction: DivergenceBothErrorMessagesDrift, Reason: "RFC-082: both engines reject; cosmetic message wording differs", GoErrorContains: "update field \"NONEXISTENT\" not found in descriptor"},
	"exists_and_not_exists":                      {Direction: DivergenceJavaSucceedsGoRejects, Reason: "RFC-082: Go capability gap (tracked); Go rejects rather than returning wrong rows", GoErrorContains: "Cascades planner could not plan query"},
	"exists_or_outer_predicate":                  {Direction: DivergenceJavaSucceedsGoRejects, Reason: "RFC-082: Go capability gap (tracked); Go rejects rather than returning wrong rows", GoErrorContains: "EXISTS within an OR (disjunction) is not supported"},
	"exists_or_predicate":                        {Direction: DivergenceJavaSucceedsGoRejects, Reason: "RFC-082: Go capability gap (tracked); Go rejects rather than returning wrong rows", GoErrorContains: "EXISTS within an OR (disjunction) is not supported"},
	"exists_over_cte_outer_with_probe":           {Direction: DivergenceJavaSucceedsGoRejects, Reason: "RFC-082: Go capability gap (tracked); Go rejects rather than returning wrong rows", GoErrorContains: "BIG"},
	"exists_three_anded":                         {Direction: DivergenceJavaSucceedsGoRejects, Reason: "RFC-082: Go capability gap (tracked); Go rejects rather than returning wrong rows", GoErrorContains: "Cascades planner could not plan query"},
	"exists_two_anded":                           {Direction: DivergenceJavaSucceedsGoRejects, Reason: "RFC-082: Go capability gap (tracked); Go rejects rather than returning wrong rows", GoErrorContains: "Cascades planner could not plan query"},
	"exists_with_aggregate":                      {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: Cascades planner could not plan query", GoExpectedRows: [][]any{{float64(10), float64(1)}, {float64(20), float64(1)}}},
	"join_aggregate_having":                      {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: Cascades planner could not plan query", GoExpectedRows: [][]any{{"A", float64(6)}, {"B", float64(5)}}},
	"left_outer_join_basic":                      {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: Attempting to query non existing column T_LOJ_02.REF", GoExpectedRows: [][]any{{float64(1), "alpha"}, {float64(2), "beta"}, {float64(3), nil}}},
	"limit_clause_rejected":                      {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: LIMIT clause is not supported.", GoExpectedRows: [][]any{{float64(1)}, {float64(2)}}},
	"mul_int_overflow_rejected":                  {Direction: DivergenceBothErrorMessagesDrift, Reason: "RFC-082: both engines reject; cosmetic message wording differs", GoErrorContains: "integer overflow"},
	"negate_min_int64_overflow":                  {Direction: DivergenceBothErrorMessagesDrift, Reason: "RFC-082: both engines reject; cosmetic message wording differs", GoErrorContains: "integer overflow"},
	"nested_derived_arithmetic_2deep":            {Direction: DivergenceJavaSucceedsGoRejects, Reason: "RFC-082: Go capability gap (tracked); Go rejects rather than returning wrong rows", GoErrorContains: "column \"DOUBLED\" does not exist"},
	"nested_derived_arithmetic_projection":       {Direction: DivergenceJavaSucceedsGoRejects, Reason: "RFC-082: Go capability gap (tracked); Go rejects rather than returning wrong rows", GoErrorContains: "column \"DOUBLED\" does not exist"},
	"nested_derived_double_where":                {Direction: DivergenceJavaSucceedsGoRejects, Reason: "RFC-082: Go capability gap (tracked); Go rejects rather than returning wrong rows", GoErrorContains: "column \"X\" does not exist"},
	"oby_desc_with_ties":                         {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: Cascades planner could not plan query", GoExpectedRows: [][]any{{float64(2), float64(20)}, {float64(4), float64(20)}, {float64(1), float64(10)}, {float64(3), float64(10)}, {float64(5), float64(10)}}},
	"oby_multi_col_mixed_dir":                    {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: Cascades planner could not plan query", GoExpectedRows: [][]any{{float64(1), float64(1), float64(30)}, {float64(2), float64(1), float64(10)}, {float64(4), float64(2), float64(40)}, {float64(3), float64(2), float64(20)}}},
	"oby_null_default_ordering":                  {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: Cascades planner could not plan query", GoExpectedRows: [][]any{{float64(2), nil}, {float64(4), nil}, {float64(1), float64(10)}, {float64(5), float64(20)}, {float64(3), float64(30)}}},
	"offset_clause_rejected":                     {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: OFFSET clause is not supported.", GoExpectedRows: [][]any{{float64(2)}, {float64(3)}}},
	"order_by_arith_unindexed_probe":             {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: Cascades planner could not plan query", GoExpectedRows: [][]any{{float64(1)}, {float64(2)}}},
	"order_by_case_expr":                         {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: Cascades planner could not plan query", GoExpectedRows: [][]any{{float64(4)}, {float64(3)}, {float64(2)}, {float64(1)}}},
	"order_by_expression":                        {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: Cascades planner could not plan query", GoExpectedRows: [][]any{{float64(1), float64(-3)}, {float64(3), float64(-2)}, {float64(2), float64(-1)}}},
	"order_by_null_first":                        {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: Cascades planner could not plan query", GoExpectedRows: [][]any{{float64(2), nil}, {float64(4), nil}, {float64(3), float64(5)}, {float64(1), float64(10)}}},
	"order_by_pk_asc_desc_mixed_rejected":        {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: Cascades planner could not plan query", GoExpectedRows: [][]any{{float64(1), float64(2)}, {float64(1), float64(1)}}},
	"order_by_string_desc":                       {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: Cascades planner could not plan query", GoExpectedRows: [][]any{{float64(2), "cherry"}, {float64(3), "banana"}, {float64(1), "apple"}}},
	"order_by_two_columns_asc_desc":              {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: Cascades planner could not plan query", GoExpectedRows: [][]any{{float64(1), float64(1), float64(30)}, {float64(3), float64(1), float64(10)}, {float64(4), float64(2), float64(40)}, {float64(2), float64(2), float64(20)}}},
	"recursive_cte_basic":                        {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: order by is not supported in subquery", GoExpectedRows: [][]any{{float64(1)}, {float64(2)}, {float64(3)}, {float64(4)}}},
	"scalar_subquery_in_projection":              {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: syntax error:\nSELECT id, (SELECT MAX(v) FROM T_SSQ_01) AS max_v FROM T_SSQ_01 ORDER BY id\n            ^^^^^^", GoExpectedRows: [][]any{{float64(1), float64(30)}, {float64(2), float64(30)}, {float64(3), float64(30)}}},
	"scalar_subquery_in_where":                   {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: syntax error:\nSELECT id FROM T_SSQ_02 WHERE v > (SELECT MIN(v) FROM T_SSQ_02) ORDER BY id\n                                   ^^^^^^", GoExpectedRows: [][]any{{float64(2)}, {float64(3)}}},
	"scalar_subquery_with_aggregate":             {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: syntax error:\nSELECT id, v, v - (SELECT AVG(v) FROM T_SSA_01) AS diff FROM T_SSA_01 ORDER BY id\n                   ^^^^^^", GoExpectedRows: [][]any{{float64(1), float64(10), float64(-10)}, {float64(2), float64(20), float64(0)}, {float64(3), float64(30), float64(10)}}},
	"select_limit_zero":                          {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: LIMIT clause is not supported.", GoExpectedRows: [][]any{}},
	"string_concat_via_plus":                     {Direction: DivergenceJavaSucceedsGoRejects, Reason: "RFC-082: Go capability gap (tracked); Go rejects rather than returning wrong rows", GoErrorContains: "string + string"},
	"string_ordering":                            {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: Cascades planner could not plan query", GoExpectedRows: [][]any{{float64(2), "apple"}, {float64(1), "banana"}, {float64(3), "cherry"}, {float64(4), "date"}}},
	"sub_int_overflow_rejected":                  {Direction: DivergenceBothErrorMessagesDrift, Reason: "RFC-082: both engines reject; cosmetic message wording differs", GoErrorContains: "integer overflow"},
	"sum_over_string_rejected":                   {Direction: DivergenceBothErrorMessagesDrift, Reason: "RFC-082: both engines reject; cosmetic message wording differs", GoErrorContains: "cannot aggregate non-numeric value of type string"},
	"two_not_exists_anded":                       {Direction: DivergenceJavaSucceedsGoRejects, Reason: "RFC-082: Go capability gap (tracked); Go rejects rather than returning wrong rows", GoErrorContains: "Cascades planner could not plan query"},
	"undefined_column":                           {Direction: DivergenceBothErrorMessagesDrift, Reason: "RFC-082: both engines reject; cosmetic message wording differs", GoErrorContains: "column \"NO_SUCH_COL\" does not exist"},
	"undefined_table":                            {Direction: DivergenceBothErrorMessagesDrift, Reason: "RFC-082: both engines reject; cosmetic message wording differs", GoErrorContains: "table \"NO_SUCH_TABLE\" does not exist"},
	"union_all_outer_where_filter_count":         {Direction: DivergenceJavaSucceedsGoRejects, Reason: "RFC-082: Go capability gap (tracked); Go rejects rather than returning wrong rows", GoErrorContains: "Cascades planner could not plan query"},
	"union_all_three_branches_outer_where_count": {Direction: DivergenceJavaSucceedsGoRejects, Reason: "RFC-082: Go capability gap (tracked); Go rejects rather than returning wrong rows", GoErrorContains: "Cascades planner could not plan query"},
	"update_undefined_column_rejected":           {Direction: DivergenceBothErrorMessagesDrift, Reason: "RFC-082: both engines reject; cosmetic message wording differs", GoErrorContains: "update field \"NO_SUCH_COL\" not found in descriptor"},
	"uuid_count_filtered":                        {Direction: DivergenceJavaSucceedsGoRejects, Reason: "RFC-082: Go capability gap (tracked); Go rejects rather than returning wrong rows", GoErrorContains: "Cascades planner could not plan query"},
	"uuid_eq_multi_row":                          {Direction: DivergenceJavaSucceedsGoRejects, Reason: "RFC-082: Go capability gap (tracked); Go rejects rather than returning wrong rows", GoErrorContains: "Cascades planner could not plan query"},
	"uuid_neq":                                   {Direction: DivergenceJavaSucceedsGoRejects, Reason: "RFC-082: Go capability gap (tracked); Go rejects rather than returning wrong rows", GoErrorContains: "Cascades planner could not plan query"},
	"uuid_where_equality":                        {Direction: DivergenceJavaSucceedsGoRejects, Reason: "RFC-082: Go capability gap (tracked); Go rejects rather than returning wrong rows", GoErrorContains: "Cascades planner could not plan query"},
	"where_case_returns_bool_probe":              {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: expected BooleanValue but got PickValue", GoExpectedRows: [][]any{{float64(2)}}},
	"where_nested_paren_and":                     {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: expected BooleanValue but got RecordConstructorValue", GoExpectedRows: [][]any{{float64(1)}, {float64(2)}}},
	"where_paren_top_level_rejected":             {Direction: DivergenceJavaErrorsGoCorrect, Reason: "RFC-082: Go-only read-side extension; Java rejects: expected BooleanValue but got RecordConstructorValue", GoExpectedRows: [][]any{{float64(1)}}},
}

// rfc082ClearStale lists corpus entries whose PRE-EXISTING inline Divergence
// annotation went stale while conformance ran ungated (Go's behaviour drifted
// from the pinned divergence). They are cleared back to no-annotation so the
// RFC-082 regression lock re-classifies them (conform, or known-red) — the
// honest outcome rather than a silently-wrong pinned annotation.
var rfc082ClearStale = map[string]bool{
	"scalar_subq_in_having":                        true,
	"scalar_subq_after_update_with_subq_rhs":       true,
	"scalar_subq_after_delete_with_subq_threshold": true,
	"scalar_subq_with_secondary_index_max":         true,
	"group_by_null_bucket":                         true,
	"groupby_null_key_bigint":                      true,
	"distinct_count":                               true,
}

// ApplyRFC082Divergences sets the RFC-082 divergence annotation on any corpus
// entry that carries one and is not already annotated inline, and clears stale
// pre-existing annotations (rfc082ClearStale) so the regression lock
// re-classifies them. Called by the cross-engine RunSql harness after building
// the corpus.
func ApplyRFC082Divergences(corpus []RunQuery) {
	for i := range corpus {
		if rfc082ClearStale[corpus[i].Name] {
			corpus[i].Divergence = nil
			continue
		}
		if corpus[i].Divergence != nil {
			continue
		}
		if d, ok := rfc082Divergences[corpus[i].Name]; ok {
			dd := d
			corpus[i].Divergence = &dd
		}
	}
}
