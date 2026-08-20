package embedded

import (
	"reflect"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/core/query"
	"fdb.dev/pkg/relational/core/query/logical"
)

const orderByExactMetadataDDL = `
	CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))
	CREATE TABLE emp (id BIGINT, name STRING, dept_id BIGINT, salary BIGINT, PRIMARY KEY (id))
	CREATE TABLE dept (id BIGINT, name STRING, PRIMARY KEY (id))
	CREATE TABLE scores (id BIGINT, player STRING, game STRING, score BIGINT, PRIMARY KEY (id))
`

func logicalSorts(op logical.LogicalOperator) []*logical.LogicalSort {
	var result []*logical.LogicalSort
	var walk func(logical.LogicalOperator)
	walk = func(candidate logical.LogicalOperator) {
		if candidate == nil {
			return
		}
		if sort, ok := candidate.(*logical.LogicalSort); ok {
			result = append(result, sort)
		}
		for _, child := range candidate.Children() {
			walk(child)
		}
	}
	walk(op)
	return result
}

func logicalProjects(op logical.LogicalOperator) []*logical.LogicalProject {
	var result []*logical.LogicalProject
	var walk func(logical.LogicalOperator)
	walk = func(candidate logical.LogicalOperator) {
		if candidate == nil {
			return
		}
		if project, ok := candidate.(*logical.LogicalProject); ok {
			result = append(result, project)
		}
		for _, child := range candidate.Children() {
			walk(child)
		}
	}
	walk(op)
	return result
}

func TestOrderByExactMetadata_LiftedUnionKeepsSegmentsAndBindsOutputOrdinal(t *testing.T) {
	t.Parallel()
	_, md := newLoggingGenerator(t, orderByExactMetadataDDL, &captureLogger{})
	op, err := NewPlanVisitor(md).VisitQuery(parseQuery(t,
		`SELECT id AS "A.B" FROM t UNION ALL SELECT id AS right_id FROM t ORDER BY "A.B" DESC`))
	if err != nil {
		t.Fatalf("VisitQuery: %v", err)
	}

	var unionSort *logical.LogicalSort
	for _, sort := range logicalSorts(op) {
		if _, ok := sort.Input.(*logical.LogicalUnion); ok {
			unionSort = sort
			break
		}
	}
	if unionSort == nil || len(unionSort.Keys) != 1 {
		t.Fatalf("union ORDER BY = %#v, want one lifted key", unionSort)
	}
	key := unionSort.Keys[0]
	if !key.BareRef || key.Bare != "A.B" || key.Qualified || key.Qualifier != "" ||
		!reflect.DeepEqual(key.Segs, []string{"A.B"}) {
		t.Fatalf("lifted structured key = %+v, want quoted one-segment A.B", key)
	}
	if key.Pos != 1 {
		t.Fatalf("lifted key output ordinal = %d, want 1", key.Pos)
	}
	if key.Value != nil {
		t.Fatalf("lifted union key Value = %v, want ordinal-only metadata", key.Value)
	}
}

func TestOrderByExactMetadata_UnderivableCTEUsesBuiltResultType(t *testing.T) {
	t.Parallel()
	_, md := newLoggingGenerator(t, orderByExactMetadataDDL, &captureLogger{})
	tests := []struct {
		name            string
		sql             string
		wantCorrelation string
		wantField       string
	}{
		{
			name: "nested_cte_shadow",
			sql: `WITH c2(a) AS (
				WITH t AS (SELECT x.v AS id FROM t AS x, t AS y WHERE x.id = y.id)
				SELECT id FROM t WHERE id <= 2
			) SELECT a FROM c2 ORDER BY a`,
			wantCorrelation: "C2",
			wantField:       "A",
		},
		{
			name: "join_bodied_cte",
			sql: `WITH eng_dept AS (SELECT id FROM dept WHERE name = 'Engineering'),
				eng_emp AS (SELECT name FROM emp AS e, eng_dept AS ed WHERE e.dept_id = ed.id)
			SELECT name FROM eng_emp ORDER BY name`,
			wantCorrelation: "ENG_EMP",
			wantField:       "NAME",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			op, err := NewPlanVisitor(md).VisitQuery(parseQuery(t, test.sql))
			if err != nil {
				t.Fatalf("VisitQuery: %v", err)
			}
			var matchedSort, matchedProjection values.FieldValue
			for _, sort := range logicalSorts(op) {
				if len(sort.Keys) != 1 || sort.Keys[0].Value == nil {
					continue
				}
				field, ok := values.AsFieldValue(sort.Keys[0].Value)
				if ok && field.DisplayName() == test.wantField {
					matchedSort = field
					break
				}
			}
			for _, project := range logicalProjects(op) {
				for _, projected := range project.ProjectedValues {
					field, ok := values.AsFieldValue(projected)
					if !ok || field.DisplayName() != test.wantField {
						continue
					}
					owner, ownerOK := values.AsQuantifiedObjectValue(field.ChildValue())
					if ownerOK && owner.Correlation().Name() == test.wantCorrelation {
						matchedProjection = field
						break
					}
				}
			}
			if matchedSort == nil {
				t.Fatalf("no exact ORDER BY field %q in logical plan", test.wantField)
			}
			if matchedProjection == nil {
				t.Fatalf("no exact projected CTE field %q rooted at %q", test.wantField, test.wantCorrelation)
			}
			owner, ok := values.AsQuantifiedObjectValue(matchedSort.ChildValue())
			if !ok {
				t.Fatalf("ORDER BY owner = %T, want exact QOV", matchedSort.ChildValue())
			}
			if got := owner.Correlation().Name(); got != test.wantCorrelation {
				t.Fatalf("ORDER BY owner = %q, want %q", got, test.wantCorrelation)
			}
			if got := matchedSort.Path().Ordinals(); !reflect.DeepEqual(got, []int{0}) {
				t.Fatalf("ORDER BY path = %v, want [0]", got)
			}
			if got := matchedProjection.Path().Ordinals(); !reflect.DeepEqual(got, []int{0}) {
				t.Fatalf("projection path = %v, want [0]", got)
			}
			ref, _, translateErr := query.TranslateToCascadesWithError(op, md)
			if translateErr != nil || ref == nil {
				t.Fatalf("exact CTE projection did not translate: ref=%v err=%v", ref, translateErr)
			}
		})
	}
}

func TestExactCTEProjection_QualifiedDirectJoinLegUsesBuiltResultType(t *testing.T) {
	t.Parallel()

	cteType := values.NewRecordType("D_OUTPUT", false, []values.Field{
		{Name: "X", FieldType: values.NotNullInt, Ordinal: 0},
	})
	cteFieldsBefore := append([]values.Field(nil), cteType.Fields...)
	cteTypes := map[string]*values.RecordType{"D": cteType}
	newProject := func(
		input logical.LogicalOperator,
		ref logical.ColumnRef,
		computed bool,
	) *logical.LogicalProject {
		project := logical.NewProject(input, []string{"D.X"}, []string{""})
		project.ProjectionRefs = []logical.ColumnRef{ref}
		project.IsComputed = []bool{computed}
		return project
	}
	qualifiedX := logical.ColumnRef{
		Present: true, Bare: "X", Qualifier: "D", Qualified: true,
	}
	directJoin := func(kind logical.JoinKind) *logical.LogicalJoin {
		return logical.NewJoin(
			logical.NewScan("D", "D"),
			logical.NewScan("EEV", "EEV"),
			kind, "")
	}

	input := directJoin(logical.JoinInner)
	project := newProject(input, qualifiedX, false)
	refBefore := project.ProjectionRefs[0]
	if err := bindExactCTEProjection(project, cteTypes); err != nil {
		t.Fatalf("bind qualified direct CTE join leg: %v", err)
	}
	if project.Input != input || project.ProjectionRefs[0] != refBefore {
		t.Fatal("exact CTE projection binding mutated its logical source")
	}
	if !reflect.DeepEqual(cteType.Fields, cteFieldsBefore) {
		t.Fatal("exact CTE projection binding mutated its built result authority")
	}
	if len(project.ProjectedValues) != 1 {
		t.Fatalf("projected values = %v, want one exact CTE field", project.ProjectedValues)
	}
	field, ok := values.AsFieldValue(project.ProjectedValues[0])
	if !ok {
		t.Fatalf("projected value = %T, want exact FieldValue", project.ProjectedValues[0])
	}
	owner, ok := values.AsQuantifiedObjectValue(field.ChildValue())
	if !ok || owner.Correlation().Name() != "D" || !owner.FlowedType().Equals(cteType) {
		t.Fatalf("projected owner = %T/%v, want D with exact built CTE type %s",
			field.ChildValue(), field.ChildValue(), cteType)
	}
	if got := field.Path().Ordinals(); !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("projected path = %v, want [0]", got)
	}

	// A malformed/external logical type can still present duplicate labels.
	// Keep the binder's unique-field guard pinned without asking NewRecordType
	// to admit that invalid type as a normal construction.
	duplicateOutputType := &values.RecordType{
		RecordName: "D_DUPLICATE",
		Fields: []values.Field{
			{Name: "X", FieldType: values.NotNullInt, Ordinal: 0},
			{Name: "X", FieldType: values.NotNullInt, Ordinal: 1},
		},
	}
	tests := []struct {
		name     string
		input    logical.LogicalOperator
		ref      logical.ColumnRef
		computed bool
		types    map[string]*values.RecordType
	}{
		{
			name:  "unqualified",
			input: directJoin(logical.JoinInner),
			ref:   logical.ColumnRef{Present: true, Bare: "X"},
			types: cteTypes,
		},
		{
			name:     "computed",
			input:    directJoin(logical.JoinInner),
			ref:      qualifiedX,
			computed: true,
			types:    cteTypes,
		},
		{
			name:  "outer_join",
			input: directJoin(logical.JoinLeft),
			ref:   qualifiedX,
			types: cteTypes,
		},
		{
			name: "wrapped_join_leg",
			input: logical.NewJoin(
				&logical.LogicalFilter{Input: logical.NewScan("D", "D")},
				logical.NewScan("EEV", "EEV"), logical.JoinInner, ""),
			ref:   qualifiedX,
			types: cteTypes,
		},
		{
			name: "colliding_qualifier",
			input: logical.NewJoin(
				logical.NewScan("D", "D"),
				logical.NewScan("EEV", "D"), logical.JoinInner, ""),
			ref:   qualifiedX,
			types: cteTypes,
		},
		{
			name: "table_name_hidden_by_alias",
			input: logical.NewJoin(
				logical.NewScan("D", "RENAMED"),
				logical.NewScan("EEV", "EEV"), logical.JoinInner, ""),
			ref:   qualifiedX,
			types: cteTypes,
		},
		{
			name:  "foreign_non_cte_leg",
			input: directJoin(logical.JoinInner),
			ref: logical.ColumnRef{
				Present: true, Bare: "X", Qualifier: "EEV", Qualified: true,
			},
			types: cteTypes,
		},
		{
			name:  "missing_field",
			input: directJoin(logical.JoinInner),
			ref: logical.ColumnRef{
				Present: true, Bare: "MISSING", Qualifier: "D", Qualified: true,
			},
			types: cteTypes,
		},
		{
			name:  "duplicate_output_field",
			input: directJoin(logical.JoinInner),
			ref:   qualifiedX,
			types: map[string]*values.RecordType{"D": duplicateOutputType},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			declined := newProject(test.input, test.ref, test.computed)
			if err := bindExactCTEProjection(declined, test.types); err != nil {
				t.Fatalf("bindExactCTEProjection: %v", err)
			}
			if len(declined.ProjectedValues) != 0 {
				t.Fatalf("declined projection values = %v, want unresolved/loud", declined.ProjectedValues)
			}
		})
	}
}

// underivableCTE is a CTE body that BUILDS but cannot be PUBLISHED: its row
// carries the name `dup` twice, which is ambiguous by construction, so
// complete-schema-or-decline withholds the whole source — including the
// unambiguous column `a` a reference actually reads.
//
// The specimen used to be a nested-WITH comma-join body, which was underivable
// only because a join-bodied CTE's schema was guessed from its FROM legs by
// name. That guess is retired (the row now comes from the built body), so the
// shape publishes and resolves — a duplicate output name is what is left that
// genuinely cannot be advertised.
const underivableCTE = `WITH c2 AS (
		SELECT x.v AS dup, y.v AS dup, x.id AS a FROM t AS x, t AS y WHERE x.id = y.id
	) `

// TestOrderByExactMetadata_ComputedKeyOverDerivableCTEResolves is the other
// half of the pair below, and the half that MOVED. A computed ORDER BY key is
// not inherently unresolvable — it is unresolvable when its SOURCE row is not
// known. The nested-WITH comma-join specimen used to be exactly that, and now
// publishes its built row, so `a + 1` over it computes a real Value.
//
// Without this arm, the retirement is only pinned from the "still declines"
// side, and a regression that stopped resolving computed keys altogether would
// keep every StaysLoud pin green.
func TestOrderByExactMetadata_ComputedKeyOverDerivableCTEResolves(t *testing.T) {
	t.Parallel()
	_, md := newLoggingGenerator(t, orderByExactMetadataDDL, &captureLogger{})
	op, err := NewPlanVisitor(md).VisitQuery(parseQuery(t, `WITH c2(a) AS (
		WITH t AS (SELECT x.v AS id FROM t AS x, t AS y WHERE x.id = y.id)
		SELECT id FROM t WHERE id <= 2
	) SELECT a FROM c2 ORDER BY a + 1`))
	if err != nil {
		t.Fatalf("VisitQuery: %v", err)
	}
	for _, sort := range logicalSorts(op) {
		if len(sort.Keys) == 1 && sort.Keys[0].Expr == "a + 1" {
			if sort.Keys[0].Value == nil {
				t.Fatal("computed key over a derivable CTE has no resolved Value; " +
					"the body's built row is what makes it computable")
			}
			return
		}
	}
	t.Fatal("computed ORDER BY key not found")
}

func TestOrderByExactMetadata_UnderivableCTEComputedKeyStaysLoud(t *testing.T) {
	t.Parallel()
	_, md := newLoggingGenerator(t, orderByExactMetadataDDL, &captureLogger{})
	op, err := NewPlanVisitor(md).VisitQuery(parseQuery(t,
		underivableCTE+`SELECT a FROM c2 ORDER BY a + 1`))
	if err != nil {
		t.Fatalf("VisitQuery: %v", err)
	}
	for _, sort := range logicalSorts(op) {
		if len(sort.Keys) == 1 && sort.Keys[0].Expr == "a + 1" {
			if sort.Keys[0].Value != nil || sort.Keys[0].Pos != 0 {
				t.Fatalf("computed underivable key gained identity metadata: %+v", sort.Keys[0])
			}
			return
		}
	}
	t.Fatal("computed ORDER BY key not found")
}

func TestOrderByExactMetadata_UnderivableCTEComputedProjectionStaysLoud(t *testing.T) {
	t.Parallel()
	_, md := newLoggingGenerator(t, orderByExactMetadataDDL, &captureLogger{})
	op, err := NewPlanVisitor(md).VisitQuery(parseQuery(t,
		underivableCTE+`SELECT a + 1 AS b FROM c2 ORDER BY a`))
	if err != nil {
		t.Fatalf("VisitQuery: %v", err)
	}
	found := false
	for _, project := range logicalProjects(op) {
		if len(project.IsComputed) != 1 || !project.IsComputed[0] {
			continue
		}
		found = true
		if len(project.ProjectedValues) > 0 && project.ProjectedValues[0] != nil {
			t.Fatalf("computed underivable projection gained identity metadata: %v", project.ProjectedValues[0])
		}
	}
	if !found {
		t.Fatal("computed CTE projection not found")
	}
	ref, _, translateErr := query.TranslateToCascadesWithError(op, md)
	if ref != nil || translateErr == nil ||
		!strings.Contains(translateErr.Error(), "projection slot 0 has no resolved Value") {
		t.Fatalf("computed projection translation = ref %v, err %v; want loud unresolved slot", ref, translateErr)
	}
}

func TestOrderByExactMetadata_DerivedDuplicateNamesUsePhysicalInputContract(t *testing.T) {
	t.Parallel()
	_, md := newLoggingGenerator(t, orderByExactMetadataDDL, &captureLogger{})
	plan, _, err := PlanRecordQueryWithSubqueries(
		`SELECT y FROM (
			SELECT id AS x, SUM(score) AS x, id AS y
			FROM scores GROUP BY id ORDER BY 2 DESC LIMIT 1
		) d`, md, nil)
	if err != nil {
		t.Fatalf("PlanRecordQueryWithSubqueries: %v", err)
	}
	projection, ok := plan.(*plans.RecordQueryProjectionPlan)
	if !ok {
		t.Fatalf("plan = %T, want RecordQueryProjectionPlan", plan)
	}
	projected := projection.GetProjections()
	if len(projected) != 1 {
		t.Fatalf("projection width = %d, want 1", len(projected))
	}
	field, ok := values.AsFieldValue(projected[0])
	if !ok {
		t.Fatalf("projected value = %T, want exact FieldValue", projected[0])
	}
	root, ok := values.AsQuantifiedObjectValue(field.ChildValue())
	if !ok {
		t.Fatalf("projected root = %T, want exact QOV", field.ChildValue())
	}
	quantifiers := projection.GetQuantifiers()
	if len(quantifiers) != 1 {
		t.Fatalf("projection quantifier count = %d, want 1", len(quantifiers))
	}
	input, err := quantifiers[0].RequireFlowedObjectValue()
	if err != nil {
		t.Fatalf("projection input QOV: %v", err)
	}
	if !root.FlowedType().Equals(input.FlowedType()) {
		t.Fatalf("projected root type = %s, input type = %s", root.FlowedType(), input.FlowedType())
	}
	children := projection.GetChildren()
	if len(children) != 1 {
		t.Fatalf("projection child count = %d, want 1", len(children))
	}
	childLayout, err := children[0].ProvidedOutputLayout()
	if err != nil {
		t.Fatalf("projection child layout: %v", err)
	}
	if root != childLayout.Carrier() {
		t.Fatal("projected field is not rooted at the selected child's exact layout carrier")
	}
	inputRecord, ok := input.FlowedType().(*values.RecordType)
	if !ok || len(inputRecord.Fields) != 3 {
		t.Fatalf("projection input type = %v, want three-field record", input.FlowedType())
	}
	if got := []string{inputRecord.Fields[0].Name, inputRecord.Fields[1].Name, inputRecord.Fields[2].Name}; !reflect.DeepEqual(got, []string{"X", "X_2", "Y"}) {
		t.Fatalf("physical input names = %v, want [X X_2 Y]", got)
	}
	if got := field.Path().Ordinals(); !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("projected path = %v, want [2]", got)
	}
	if !field.ResultType().Equals(values.NullableLong) {
		t.Fatalf("projected leaf type = %s, want nullable LONG", field.ResultType())
	}
}

func TestOrderByExactMetadata_DerivedDelimitedLowercaseNameUsesPhysicalOrdinal(t *testing.T) {
	t.Parallel()
	_, md := newLoggingGenerator(t, orderByExactMetadataDDL, &captureLogger{})
	plan, _, err := PlanRecordQueryWithSubqueries(
		`SELECT "a.b" FROM (SELECT v AS "a.b" FROM t) AS s ORDER BY "a.b" DESC`, md, nil)
	if err != nil {
		t.Fatalf("PlanRecordQueryWithSubqueries: %v", err)
	}

	var sortPlan *plans.RecordQueryInMemorySortPlan
	var walk func(plans.RecordQueryPlan)
	walk = func(candidate plans.RecordQueryPlan) {
		if candidate == nil || sortPlan != nil {
			return
		}
		if sort, ok := candidate.(*plans.RecordQueryInMemorySortPlan); ok {
			sortPlan = sort
			return
		}
		for _, child := range candidate.GetChildren() {
			walk(child)
		}
	}
	walk(plan)
	if sortPlan == nil {
		t.Fatalf("plan = %T/%s, want an in-memory sort", plan, plan.Explain())
	}
	keys := sortPlan.GetSortKeys()
	if len(keys) != 1 {
		t.Fatalf("sort key count = %d, want 1", len(keys))
	}
	key, ok := values.AsFieldValue(keys[0].ValueExpr)
	if !ok {
		t.Fatalf("sort key = %T, want exact FieldValue", keys[0].ValueExpr)
	}
	children := sortPlan.GetChildren()
	if len(children) != 1 {
		t.Fatalf("sort child count = %d, want 1", len(children))
	}
	inputLayout, err := children[0].ProvidedOutputLayout()
	if err != nil {
		t.Fatalf("sort input layout: %v", err)
	}
	if key.ChildValue() != inputLayout.Carrier() {
		t.Fatalf("sort key owner = %v, want exact input carrier %p",
			key.ChildValue(), inputLayout.Carrier())
	}
	if got := key.Path().Ordinals(); !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("sort key path = %v, want [0]", got)
	}
	// THE QUOTED ALIAS KEEPS ITS CASE. `AS "a.b"` names the derived column
	// a.b, and both the sort key's display name and the slot key it renders
	// carry that spelling — the correlation prefix S is the source ALIAS,
	// which is a different domain and stays folded.
	//
	// This asserted A.B / S.A.B#0 while the output-name authority folded, and
	// that fold is what made a quoted alias unreachable by its own name.
	if key.DisplayName() != "a.b" || keys[0].Field != "S.a.b#0" {
		t.Fatalf("physical sort key = %q/%q, want a.b/S.a.b#0",
			key.DisplayName(), keys[0].Field)
	}
	if !key.ResultType().Equals(values.NullableLong) {
		t.Fatalf("sort key type = %s, want nullable LONG", key.ResultType())
	}
}

func TestOrderByExactMetadata_DerivedRecordIdentityIsNotNameNormalization(t *testing.T) {
	t.Parallel()
	_, md := newLoggingGenerator(t, orderByExactMetadataDDL, &captureLogger{})
	plan, _, err := PlanRecordQueryWithSubqueries(
		`SELECT id FROM (SELECT * FROM scores) d`, md, nil)
	if err != nil {
		t.Fatalf("ordinary derived-table projection: %v", err)
	}
	if plan == nil {
		t.Fatal("ordinary derived-table projection returned no plan")
	}
}

func TestOrderByExactMetadata_PositionalBindingCannotBeOverwrittenByText(t *testing.T) {
	t.Parallel()
	_, md := newLoggingGenerator(t, orderByExactMetadataDDL, &captureLogger{})
	op, err := NewPlanVisitor(md).VisitQuery(parseQuery(t,
		`SELECT score + 0 AS id, id AS y FROM (SELECT score, id FROM scores) d ORDER BY 2 LIMIT 3`))
	if err != nil {
		t.Fatalf("VisitQuery: %v", err)
	}

	var projection *logical.LogicalProject
	for _, candidate := range logicalProjects(op) {
		if len(candidate.ProjectedValues) == 2 && len(candidate.Aliases) == 2 &&
			candidate.Aliases[0] == "ID" && candidate.Aliases[1] == "Y" {
			projection = candidate
			break
		}
	}
	if projection == nil {
		t.Fatal("select-list projection not found")
	}
	var sort *logical.LogicalSort
	for _, candidate := range logicalSorts(op) {
		if len(candidate.Keys) == 1 && candidate.Keys[0].Expr == "ID" {
			sort = candidate
			break
		}
	}
	if sort == nil {
		t.Fatal("ORDER BY 2 key not found")
	}
	if sort.Keys[0].Pos != 0 {
		t.Fatalf("resolved positional key retained Pos %d, want 0", sort.Keys[0].Pos)
	}
	if sort.Keys[0].Value != projection.ProjectedValues[1] {
		t.Fatalf("ORDER BY 2 Value = %v, want exact select-list slot 1 %v",
			sort.Keys[0].Value, projection.ProjectedValues[1])
	}
	if sort.Keys[0].Value == projection.ProjectedValues[0] {
		t.Fatal("later ID text/alias mapping overwrote ORDER BY 2 with select-list slot 0")
	}
	field, ok := values.AsFieldValue(sort.Keys[0].Value)
	if !ok {
		t.Fatalf("ORDER BY 2 Value = %T, want exact FieldValue", sort.Keys[0].Value)
	}
	if got := field.Path().Ordinals(); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("ORDER BY 2 path = %v, want derived input ordinal [1]", got)
	}
}
