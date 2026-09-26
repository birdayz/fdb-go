package embedded

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query"
	"fdb.dev/pkg/relational/core/query/logical"
)

const orderByExactMetadataDDL = `
	CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))
	CREATE TABLE emp (id BIGINT, name STRING, dept_id BIGINT, salary BIGINT, PRIMARY KEY (id))
	CREATE TABLE dept (id BIGINT, name STRING, PRIMARY KEY (id))
	CREATE TABLE scores (id BIGINT, player STRING, game STRING, score BIGINT, PRIMARY KEY (id))
	CREATE TYPE AS STRUCT nst (sk BIGINT, co BIGINT)
	CREATE TABLE ts (id BIGINT, n nst, PRIMARY KEY (id))
	CREATE TABLE items_t (id BIGINT, items nst ARRAY, PRIMARY KEY (id))
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
	producer := logical.NewCTE("D", nil, nil, false).CTEProducer
	registry := logical.CTERegistry{}.With(producer)
	cteTypes := map[*logical.CTEProducer]*values.RecordType{producer: cteType}
	newProject := func(
		input logical.LogicalOperator,
		ref logical.ColumnRef,
		computed bool,
	) *logical.LogicalProject {
		logical.BindCTESources(input, registry)
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
		types    map[*logical.CTEProducer]*values.RecordType
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
			types: map[*logical.CTEProducer]*values.RecordType{producer: duplicateOutputType},
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
// carries an array literal with a NULL element, whose exact type has a NULLABLE
// element that semantic.Column cannot carry losslessly (it has the container's
// nullability and no nullable-element bit, and its reverse bridge forces every
// element NOT NULL), so the exact derivation declines the whole source —
// including the plain column `a` a reference actually reads.
//
// The specimen has moved three times, each time because the shape it used
// started publishing. It was a nested-WITH comma-join body, underivable only
// because a join-bodied CTE's schema was guessed from its FROM legs by name;
// then a body that named `dup` twice, withheld by a uniqueness gate — which was
// itself the silent bind it claimed to prevent, since the declined CTE fell to
// the ON-only class and a read of `dup` bound one duplicate or the other (a
// repeated name is published now and its reader reports 42702; repeatedNameCTE
// below resolves); then a join body carrying a catalog STRUCT column, declined
// only because the bridge refused every record not literally named RECORD while
// semantic.Column had carried a record's name in StructTypeName all along (a
// nominal record is published under its name now). What is left that genuinely
// cannot be advertised is a row the semantic column model cannot state; the
// element bit that would state this one is booked in TODO.md ("An array literal
// with a NULL element cannot be read through a CTE or derived table"), and
// closing it moves the specimen a fourth time.
const underivableCTE = `WITH c2 AS (
		SELECT [x.id, NULL] AS s, x.id AS a FROM ts AS x, t AS y WHERE x.id = y.id
	) `

// repeatedNameCTE is the body underivableCTE used to be: it names `dup` twice.
// It publishes as stated, so a computed key or projection over its unambiguous
// column `a` resolves exactly as over any other join-bodied CTE, and only a
// read that spells `dup` meets the ambiguity check.
const repeatedNameCTE = `WITH c2 AS (
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

// The pair below is the other half of the two StaysLoud pins that follow: the
// repeated-name body that used to be their specimen now publishes, so a
// computed key and a computed projection over its unambiguous column resolve.
// Without these, retiring the uniqueness gate would be pinned only from the
// "still declines" side.
func TestOrderByExactMetadata_ComputedKeyOverRepeatedNameCTEResolves(t *testing.T) {
	t.Parallel()
	_, md := newLoggingGenerator(t, orderByExactMetadataDDL, &captureLogger{})
	op, err := NewPlanVisitor(md).VisitQuery(parseQuery(t,
		repeatedNameCTE+`SELECT a FROM c2 ORDER BY a + 1`))
	if err != nil {
		t.Fatalf("VisitQuery: %v", err)
	}
	for _, sort := range logicalSorts(op) {
		if len(sort.Keys) == 1 && sort.Keys[0].Expr == "a + 1" {
			if sort.Keys[0].Value == nil {
				t.Fatal("computed key over a repeated-name CTE has no resolved Value; " +
					"the body publishes its row, repeated name included, and `a` is unambiguous")
			}
			return
		}
	}
	t.Fatal("computed ORDER BY key not found")
}

func TestOrderByExactMetadata_ComputedProjectionOverRepeatedNameCTEResolves(t *testing.T) {
	t.Parallel()
	_, md := newLoggingGenerator(t, orderByExactMetadataDDL, &captureLogger{})
	op, err := NewPlanVisitor(md).VisitQuery(parseQuery(t,
		repeatedNameCTE+`SELECT a + 1 AS b FROM c2 ORDER BY a`))
	if err != nil {
		t.Fatalf("VisitQuery: %v", err)
	}
	found := false
	for _, project := range logicalProjects(op) {
		if len(project.IsComputed) != 1 || !project.IsComputed[0] {
			continue
		}
		found = true
		if len(project.ProjectedValues) == 0 || project.ProjectedValues[0] == nil {
			t.Fatal("computed projection over a repeated-name CTE has no resolved Value")
		}
	}
	if !found {
		t.Fatal("computed CTE projection not found")
	}
	if ref, _, translateErr := query.TranslateToCascadesWithError(op, md); ref == nil || translateErr != nil {
		t.Fatalf("computed projection translation = ref %v, err %v; want a translated reference", ref, translateErr)
	}
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

func TestOrderByExactMetadata_SelectAliasRetainsOutputOwner(t *testing.T) {
	t.Parallel()
	_, md := newLoggingGenerator(t, orderByExactMetadataDDL, &captureLogger{})
	for _, sql := range []string{
		`SELECT p.id AS z, q.id AS "P.ID" FROM t p, t q GROUP BY p.id, q.id AS "P.ID" ORDER BY z, q.id DESC`,
		`SELECT p.id AS z, q.id AS "P.ID" FROM t p, t q GROUP BY p.id, q.id ORDER BY z, q.id DESC`,
		`SELECT p.id AS z, q.id AS "P.ID" FROM t p, t q GROUP BY p.id, q.id AS z ORDER BY z, q.id DESC`,
		`SELECT p.id AS id, q.id AS v FROM t p, t q GROUP BY p.id, q.id AS id ORDER BY id, q.id DESC`,
	} {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()
			op, err := NewPlanVisitor(md).VisitQuery(parseQuery(t, sql))
			if err != nil {
				t.Fatal(err)
			}
			projection := findProjection(op)
			sort := findSort(op)
			if projection == nil || len(projection.ProjectedValues) != 2 || sort == nil || len(sort.Keys) != 2 {
				t.Fatalf("missing two-column output/sort: %s", op.Explain(""))
			}
			key := sort.Keys[0]
			if key.Pos != 0 || key.Value != projection.ProjectedValues[0] || key.Value == projection.ProjectedValues[1] || !key.AggregateOutputValueExact {
				t.Fatalf("SELECT alias lost output slot zero: key=%+v, outputs=%v", key, projection.ProjectedValues)
			}
			field, ok := values.AsFieldValue(key.Value)
			if !ok || !reflect.DeepEqual(field.Path().Ordinals(), []int{0}) {
				t.Fatalf("SELECT alias value = %v, want source field ordinal [0]", key.Value)
			}
			owner, ok := values.AsQuantifiedObjectValue(field.ChildValue())
			if !ok || owner.Correlation().Name() != "P" {
				t.Fatalf("SELECT alias source = %v, want P", field.ChildValue())
			}
			shell, err := buildLogicalPlanForSelectWithCatalog(parseSelect(t, sql), md, defaultEmbeddedSchema)
			if err != nil {
				t.Fatal(err)
			}
			shellProjection, shellSort := findProjection(shell), findSort(shell)
			if shellProjection == nil || len(shellProjection.ProjectedValues) != 2 || shellSort == nil || len(shellSort.Keys) != 2 {
				t.Fatalf("missing shell output/sort: %s", shell.Explain(""))
			}
			shellKey := shellSort.Keys[0]
			if shellKey.Pos != 0 || shellKey.Value != shellProjection.ProjectedValues[0] || shellKey.Value == shellProjection.ProjectedValues[1] || !shellKey.AggregateOutputValueExact {
				t.Fatalf("shell SELECT alias lost output slot zero: key=%+v, outputs=%v", shellKey, shellProjection.ProjectedValues)
			}
			untyped, err := NewPlanVisitor(nil).VisitQueryBody(parseQuery(t, sql).QueryExpressionBody())
			if err != nil {
				t.Fatal(err)
			}
			for _, candidate := range []logical.LogicalOperator{untyped, buildLogicalPlanForSelect(parseSelect(t, sql))} {
				untypedSort := findSort(candidate)
				if untypedSort == nil || len(untypedSort.Keys) != 2 {
					t.Fatalf("missing untyped sort: %s", candidate.Explain(""))
				}
				first, second := untypedSort.Keys[0], untypedSort.Keys[1]
				if first.Pos != 1 || !first.HasAggregateOutputOrdinal || first.AggregateOutputOrdinal != 0 || second.Pos != 0 {
					t.Fatalf("untyped sort lost SELECT ownership or rebound qualified source: %+v", untypedSort.Keys)
				}
			}
		})
	}
}

func TestOrderByExactMetadata_GroupAliasKeepsSourceOwner(t *testing.T) {
	t.Parallel()
	_, md := newLoggingGenerator(t, orderByExactMetadataDDL, &captureLogger{})
	const sql = `SELECT p.id AS "Q.ID", q.id AS v FROM t p, t q GROUP BY p.id, q.id AS g ORDER BY g, p.id DESC`
	visitor, err := NewPlanVisitor(md).VisitQuery(parseQuery(t, sql))
	if err != nil {
		t.Fatal(err)
	}
	shell, err := buildLogicalPlanForSelectWithCatalog(parseSelect(t, sql), md, defaultEmbeddedSchema)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []logical.LogicalOperator{visitor, shell} {
		sort := findSort(op)
		if sort == nil || len(sort.Keys) != 2 {
			t.Fatalf("missing two-key sort: %s", op.Explain(""))
		}
		for i, source := range []string{"Q", "P"} {
			key := sort.Keys[i]
			field, ok := values.AsFieldValue(key.Value)
			var path []int
			if ok {
				path = field.Path().Ordinals()
			}
			if !key.AggregateOutputValueExact || !ok || !reflect.DeepEqual(path, []int{0}) {
				t.Fatalf("sort key %d lost its exact source field: %+v, value %T path %v", i, key, key.Value, path)
			}
			owner, ok := values.AsQuantifiedObjectValue(field.ChildValue())
			if !ok || owner.Correlation().Name() != source {
				t.Fatalf("sort key %d source = %v, want %s", i, field.ChildValue(), source)
			}
		}
	}
}

func TestOrderByExactMetadata_AmbiguousOutputAlias(t *testing.T) {
	t.Parallel()
	_, md := newLoggingGenerator(t, orderByExactMetadataDDL, &captureLogger{})
	for _, sql := range []string{
		`SELECT id AS z, v AS z FROM t GROUP BY id, v ORDER BY z`,
		`SELECT id AS id, v AS id FROM t GROUP BY id, v ORDER BY id`,
		`SELECT id AS z, v AS z FROM t ORDER BY z`,
		`SELECT id AS z, v AS z FROM t ORDER BY z, z`,
		`SELECT id AS z, v AS z FROM t GROUP BY id, v ORDER BY z, z`,
		`SELECT id AS id, v AS id FROM t ORDER BY id`,
		`SELECT id, v AS id FROM t GROUP BY id, v ORDER BY id`,
		`SELECT id+0 AS z, v AS z FROM t GROUP BY id, v ORDER BY z`,
		`SELECT id+0 AS z, v AS z FROM t ORDER BY z`,
	} {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()
			_, visitorErr := NewPlanVisitor(md).VisitQuery(parseQuery(t, sql))
			_, shellErr := buildLogicalPlanForSelectWithCatalog(parseSelect(t, sql), md, defaultEmbeddedSchema)
			for _, err := range []error{visitorErr, shellErr} {
				var diagnostic *api.Error
				if !errors.As(err, &diagnostic) || diagnostic.Code != api.ErrCodeAmbiguousColumn {
					t.Fatalf("ambiguous output alias error = %v, want 42702", err)
				}
				name, _, _, _ := splitColumnRef(parseSelect(t, sql).orderBy[0].rawExpr)
				if diagnostic.Message != "Ambiguous alias "+name {
					t.Fatalf("diagnostic = %q, want Java alias diagnostic for %s", diagnostic.Message, name)
				}
			}
		})
	}
}

func TestOrderByExactMetadata_PlainInheritedNameIsQualified(t *testing.T) {
	t.Parallel()
	_, md := newLoggingGenerator(t, orderByExactMetadataDDL, &captureLogger{})
	for _, tc := range []struct {
		sql     string
		slot    int
		ordinal int
	}{
		{`SELECT id, v AS id FROM t ORDER BY id`, 1, 1},
		{`SELECT v, id AS v FROM t ORDER BY v`, 1, 0},
		{`SELECT ts.n.sk, ts.id AS sk FROM ts ORDER BY sk`, 1, 0},
		{`SELECT t.*, t.id AS v FROM t ORDER BY v`, 2, 0},
		{`SELECT t.id AS v, t.* FROM t ORDER BY v`, 0, 0},
		{`SELECT sk, co AS sk FROM items_t, items_t.items AS x ORDER BY sk`, 1, 1},
		{`SELECT "_0", "_1" AS "_0" FROM VALUES (9, 1), (3, 2) AS v ("_0", "_1") ORDER BY "_0"`, 1, 1},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			t.Parallel()
			visitor, err := NewPlanVisitor(md).VisitQuery(parseQuery(t, tc.sql))
			if err != nil {
				t.Fatal(err)
			}
			shell, err := buildLogicalPlanForSelectWithCatalog(parseSelect(t, tc.sql), md, defaultEmbeddedSchema)
			if err != nil {
				t.Fatal(err)
			}
			for _, op := range []logical.LogicalOperator{visitor, shell} {
				proj, sort := findProjection(op), findSort(op)
				if proj == nil || len(proj.ProjectedValues) <= tc.slot || sort == nil || len(sort.Keys) != 1 {
					t.Fatalf("missing output or sort: %s", op.Explain(""))
				}
				key := sort.Keys[0]
				if key.Value != proj.ProjectedValues[tc.slot] {
					t.Fatalf("ORDER BY alias selected %v, want output slot %d", key.Value, tc.slot)
				}
				field, ok := values.AsFieldValue(key.Value)
				if !ok || !reflect.DeepEqual(field.Path().Ordinals(), []int{tc.ordinal}) {
					t.Fatalf("ORDER BY alias field = %v, want source ordinal %d", key.Value, tc.ordinal)
				}
			}
		})
	}
}

func TestOrderByExactMetadata_OutputAliasCardinality(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, order string
		names       []string
		position    int
		matches     int
	}{
		{"absent", "z", []string{"X", "Y"}, 0, 0},
		{"unique", "z", []string{"X", "Z"}, 2, 1},
		{"ambiguous", "z", []string{"Z", "Z"}, 0, 2},
		{"qualified", "t.z", []string{"Z", "Z"}, 0, 0},
		{"computed", "z+1", []string{"Z", "Z"}, 0, 0},
		{"numeric", "1", []string{"Z", "Z"}, 0, 0},
		{"quoted", `"T.Z"`, []string{"T.Z", "T.Z"}, 0, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sq := parseSelect(t, "SELECT id FROM t ORDER BY "+tc.order)
			position, matches := selectOutputAliasPosition(sq.orderBy[0].rawExpr, tc.names)
			if position != tc.position || matches != tc.matches {
				t.Fatalf("alias binding = (%d,%d), want (%d,%d)", position, matches, tc.position, tc.matches)
			}
		})
	}
}

func TestPositionalSortColumnIdentity(t *testing.T) {
	t.Parallel()
	a := exactFlatGroupKey(t, "A", "ID")
	b := exactFlatGroupKey(t, "B", "ID")
	for _, tc := range []struct {
		name        string
		left, right values.Value
		positions   []bool
		computed    bool
		mismatch    bool
		want        api.ErrorCode
	}{
		{name: "distinct_sources_same_label", left: a, right: b, positions: []bool{true, true}},
		{name: "same_source_different_positions", left: a, right: a, positions: []bool{true, true}, want: api.ErrCodeColumnAlreadyExists},
		{name: "named_then_positional", left: a, right: a, positions: []bool{false, true}, want: api.ErrCodeColumnAlreadyExists},
		{name: "positional_then_named", left: a, right: a, positions: []bool{true, false}, want: api.ErrCodeColumnAlreadyExists},
		{name: "named_only_is_parser_owned", left: a, right: a, positions: []bool{false, false}},
		{name: "expressions_are_not_column_duplicates", left: a, right: a, positions: []bool{true, true}, computed: true},
		{name: "unresolved_is_not_label_identity", left: a, positions: []bool{true, true}},
		{name: "lost_key_is_not_silently_accepted", left: a, right: b, positions: []bool{true, true}, mismatch: true, want: api.ErrCodeInternalError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sq := &selectQuery{selectClassification: selectClassification{
				selectSlots: []selectOutputSlot{{column: &projCol{}}, {column: &projCol{}}},
				orderBy:     []orderByClause{{pos: 1, colName: "ID", bare: "ID"}, {pos: 2, colName: "ID", bare: "ID"}},
			}}
			if tc.computed {
				sq.selectSlots[1].column = nil
			}
			if tc.mismatch {
				sq.orderBy = sq.orderBy[:1]
			}
			sort := &logical.LogicalSort{Keys: []logical.SortKey{{Value: tc.left}, {Value: tc.right}}}
			err := validatePositionalSortColumns(sort, sq, tc.positions)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var diagnostic *api.Error
			if !errors.As(err, &diagnostic) || diagnostic.Code != tc.want {
				t.Fatalf("error = %v, want SQLSTATE %s", err, tc.want)
			}
		})
	}
}

func FuzzPositionalSortColumnIdentity(f *testing.F) {
	f.Add([]byte{128, 129})
	f.Add([]byte{128, 128})
	f.Add([]byte{0, 128, 130, 131})
	f.Fuzz(func(t *testing.T, input []byte) {
		t.Parallel()
		if len(input) == 0 || len(input) > 32 {
			return
		}
		sq := &selectQuery{}
		sort := &logical.LogicalSort{}
		positional := make([]bool, len(input))
		wantDuplicate := false
		for i, code := range input {
			correlation := "A"
			if code&1 != 0 {
				correlation = "B"
			}
			ordinal := int(code>>1) & 1
			bound := nestedGroupKey(t, correlation, []string{"SK", "CO"}[ordinal], ordinal)
			sq.selectSlots = append(sq.selectSlots, selectOutputSlot{column: &projCol{bound: bound}})
			positional[i] = code&128 != 0
			position := 0
			if positional[i] {
				position = i + 1
			}
			sq.orderBy = append(sq.orderBy, orderByClause{pos: position, colName: "ID", bare: "ID"})
			sort.Keys = append(sort.Keys, logical.SortKey{Expr: "ID", Value: bound})
			for j := 0; j < i; j++ {
				if (positional[i] || positional[j]) && code&3 == input[j]&3 {
					wantDuplicate = true
				}
			}
		}
		err := validatePositionalSortColumns(sort, sq, positional)
		if !wantDuplicate {
			if err != nil {
				t.Fatal(err)
			}
			return
		}
		var diagnostic *api.Error
		if !errors.As(err, &diagnostic) || diagnostic.Code != api.ErrCodeColumnAlreadyExists {
			t.Fatalf("same source and field ordinal did not reject: %v", err)
		}
	})
}

func TestOrderByExactMetadata_UnnamedSourceAlias(t *testing.T) {
	t.Parallel()
	_, md := newLoggingGenerator(t, orderByExactMetadataDDL, &captureLogger{})
	for _, sql := range []string{
		`SELECT "_0", "_1" AS "_0" FROM VALUES (9, 1), (3, 2) ORDER BY "_0"`,
		`SELECT "_0", 99 AS "_0" FROM VALUES (42) ORDER BY "_0"`,
		`SELECT *, 99 AS "_0" FROM VALUES (42) ORDER BY "_0"`,
		`SELECT x, 99 AS x FROM items_t, items_t.items AS x ORDER BY x, x`,
		`SELECT x.x, 99 AS x FROM items_t, items_t.items AS x ORDER BY x, x`,
	} {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()
			_, visitorErr := NewPlanVisitor(md).VisitQuery(parseQuery(t, sql))
			_, shellErr := buildLogicalPlanForSelectWithCatalog(parseSelect(t, sql), md, defaultEmbeddedSchema)
			for _, err := range []error{visitorErr, shellErr} {
				var diagnostic *api.Error
				if !errors.As(err, &diagnostic) || diagnostic.Code != api.ErrCodeAmbiguousColumn || diagnostic.Message != "Ambiguous alias "+strings.ToUpper(parseSelect(t, sql).orderBy[0].bare) {
					t.Fatalf("output alias = %v, want 42702 / Ambiguous alias %s", err, parseSelect(t, sql).orderBy[0].bare)
				}
			}
		})
	}
}

func TestOrderByExactMetadata_UnionNamedDuplicates(t *testing.T) {
	t.Parallel()
	_, md := newLoggingGenerator(t, orderByExactMetadataDDL, &captureLogger{})
	for _, tc := range []struct {
		sql  string
		code api.ErrorCode
	}{
		{`SELECT * FROM t p, t q UNION ALL SELECT * FROM t p, t q ORDER BY id`, api.ErrCodeAmbiguousColumn},
		{`SELECT * FROM t p, t q UNION ALL SELECT * FROM t p, t q ORDER BY id, id`, api.ErrCodeAmbiguousColumn},
		{`SELECT * FROM t UNION ALL SELECT * FROM t ORDER BY id, id`, api.ErrCodeColumnAlreadyExists},
		{`SELECT * FROM t UNION ALL SELECT id, v FROM t ORDER BY id, id`, api.ErrCodeColumnAlreadyExists},
		{`SELECT * FROM t UNION ALL SELECT * FROM t ORDER BY missing, missing`, api.ErrCodeUndefinedColumn},
		{`SELECT * FROM t UNION ALL SELECT * FROM t ORDER BY id, v`, ""},
		{`SELECT id FROM t UNION ALL SELECT id FROM t ORDER BY id, id`, api.ErrCodeColumnAlreadyExists},
		{`SELECT id AS z FROM t UNION ALL SELECT id AS z FROM t ORDER BY z, z`, api.ErrCodeColumnAlreadyExists},
		{`SELECT id FROM t UNION ALL SELECT id FROM t ORDER BY missing, missing`, api.ErrCodeUndefinedColumn},
		{`SELECT id, v FROM t UNION ALL SELECT id, v FROM t ORDER BY id, v`, ""},
		{`SELECT id AS "T.V", v FROM t UNION ALL SELECT id AS "T.V", v FROM t ORDER BY "T.V", t.v`, ""},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			t.Parallel()
			visitor, visitorErr := NewPlanVisitor(md).VisitQuery(parseQuery(t, tc.sql))
			shell, shellErr := buildLogicalPlanForQueryBodyWithCatalog(parseQuery(t, tc.sql).QueryExpressionBody(), md)
			for _, err := range []error{visitorErr, shellErr} {
				if tc.code == "" {
					if err != nil {
						t.Fatal(err)
					}
					for _, op := range []logical.LogicalOperator{visitor, shell} {
						sort := findSort(op)
						if sort == nil || len(sort.Keys) != 2 || sort.Keys[0].Pos != 1 || sort.Keys[1].Pos != 2 {
							t.Fatalf("UNION output keys = %#v, want slots 1,2", sort)
						}
					}
					continue
				}
				var diagnostic *api.Error
				if !errors.As(err, &diagnostic) || diagnostic.Code != tc.code {
					t.Fatalf("union ORDER = %v, want %s", err, tc.code)
				}
			}
		})
	}
}

func TestOrderByExactMetadata_UnionDuplicateLabelsRemainLegal(t *testing.T) {
	t.Parallel()
	_, md := newLoggingGenerator(t, orderByExactMetadataDDL, &captureLogger{})
	for _, tc := range []struct {
		sql       string
		positions []int
	}{
		{`SELECT * FROM t p, t q UNION ALL SELECT * FROM t p, t q`, nil},
		{`SELECT * FROM t p, t q UNION ALL SELECT * FROM t p, t q ORDER BY 1, 3`, []int{1, 3}},
		{`SELECT p.id AS a, q.id AS b FROM t p, t q UNION ALL SELECT p.id AS a, q.id AS b FROM t p, t q ORDER BY a, b`, []int{1, 2}},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			t.Parallel()
			visitor, err := NewPlanVisitor(md).VisitQuery(parseQuery(t, tc.sql))
			if err != nil {
				t.Fatal(err)
			}
			shell, err := buildLogicalPlanForQueryBodyWithCatalog(parseQuery(t, tc.sql).QueryExpressionBody(), md)
			if err != nil {
				t.Fatal(err)
			}
			for _, op := range []logical.LogicalOperator{visitor, shell} {
				sort := findSort(op)
				if tc.positions == nil {
					if sort != nil {
						t.Fatalf("unexpected sort: %v", sort)
					}
					continue
				}
				if sort == nil || len(sort.Keys) != len(tc.positions) {
					t.Fatalf("sort keys = %#v", sort)
				}
				for i, want := range tc.positions {
					if sort.Keys[i].Pos != want {
						t.Fatalf("key %d owner %d want %d", i, sort.Keys[i].Pos, want)
					}
				}
			}
		})
	}
}

func TestOrderByExactMetadata_QuotedAliasIdentity(t *testing.T) {
	t.Parallel()
	_, md := newLoggingGenerator(t, orderByExactMetadataDDL, &captureLogger{})
	for _, union := range []bool{false, true} {
		for _, tc := range []struct {
			order     string
			positions []int
			code      api.ErrorCode
		}{
			{`"x"`, []int{1}, ""},
			{`"X"`, []int{2}, ""},
			{`"x", "X"`, []int{1, 2}, ""},
			{`"X", "x"`, []int{2, 1}, ""},
			{`x`, []int{2}, ""},
			{`"x", "x"`, nil, api.ErrCodeColumnAlreadyExists},
			{`"X", x`, nil, api.ErrCodeColumnAlreadyExists},
			{`"missing", "missing"`, nil, api.ErrCodeUndefinedColumn},
		} {
			statement := `SELECT id AS "x", v AS "X" FROM t`
			if union {
				statement += ` UNION ALL ` + statement
			}
			statement += " ORDER BY " + tc.order
			t.Run(statement, func(t *testing.T) {
				t.Parallel()
				visitor, visitorErr := NewPlanVisitor(md).VisitQuery(parseQuery(t, statement))
				shell, shellErr := buildLogicalPlanForQueryBodyWithCatalog(parseQuery(t, statement).QueryExpressionBody(), md)
				for i, op := range []logical.LogicalOperator{visitor, shell} {
					err := []error{visitorErr, shellErr}[i]
					if tc.code != "" {
						var diagnostic *api.Error
						if !errors.As(err, &diagnostic) || diagnostic.Code != tc.code {
							t.Fatalf("ORDER = %v, want %s", err, tc.code)
						}
						continue
					}
					if err != nil {
						t.Fatal(err)
					}
					sort := findSort(op)
					if sort == nil || len(sort.Keys) != len(tc.positions) {
						t.Fatalf("sort = %#v", sort)
					}
					for j, want := range tc.positions {
						if union {
							if sort.Keys[j].Pos != want {
								t.Fatalf("key %d owner %d want %d", j, sort.Keys[j].Pos, want)
							}
						} else {
							projection := findProjection(op)
							if projection == nil || len(projection.ProjectedValues) < want || sort.Keys[j].Value != projection.ProjectedValues[want-1] {
								t.Fatalf("key %d does not own projected value %d", j, want)
							}
						}
					}
				}
			})
		}
	}
}
