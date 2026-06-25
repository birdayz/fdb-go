package semantic

// Catalog is the semantic analyzer's view of the schema. The
// analyzer looks up tables, columns, and indexes through this
// interface; concrete implementations bridge to `RecordMetaData`
// (for the embedded engine) or to test fixtures.
//
// Mirrors the subset of Java's `SchemaTemplate` that the Go
// SemanticAnalyzer port needs. Keeping it narrow so the seed
// doesn't drag in the full RecordLayer metadata surface — callers
// who need more adapter methods can extend the Table interface
// later.
//
// All lookups take QualifiedName so the analyzer can handle
// schema-qualified references uniformly; concrete impls decide how
// to resolve un-qualified names (walk the search path, default
// schema, etc.).
type Catalog interface {
	// LookupTable returns a Table handle for the given qualified
	// name, or (nil, false) if no such table exists. Name
	// normalization is the caller's job — QualifiedName already
	// carries the case-folded form.
	LookupTable(name QualifiedName) (Table, bool)

	// TableExists reports whether a Table with the given name is
	// registered. Equivalent to LookupTable's second return value;
	// surfaced separately because it's the common fast-path check
	// in existence-gating rules.
	TableExists(name QualifiedName) bool

	// AllTableNames returns every registered table's qualified
	// name. Order is unspecified; callers that need deterministic
	// ordering should sort. Used for INFORMATION_SCHEMA-style
	// reflection and for error messages that enumerate candidate
	// tables.
	AllTableNames() []QualifiedName
}

// Table is the analyzer's view of a single SQL table. Minimal for
// the seed — Name + Columns + Indexes. Richer methods (PK shape,
// fields-of-interest, RecordType bridging) land as the analyzer
// grows.
type Table interface {
	// Name returns the qualified table name.
	Name() QualifiedName

	// Columns returns the table's column definitions in declared
	// order. Empty slice if the table has no columns (a valid state
	// for views / CTEs). Never nil.
	Columns() []Column

	// LookupColumn returns a Column by identifier, matching
	// case-insensitively under SQL rules (the Identifier's
	// normalized form). Returns (Column{}, false) if no match.
	LookupColumn(id Identifier) (Column, bool)

	// Indexes returns the index names defined on this table. The
	// seed returns just names; richer IndexInfo follows once
	// index-pushdown rules need per-index metadata.
	Indexes() []string
}

// Column is the analyzer's view of a table column. Type is a
// placeholder string until the DataType / Type hierarchy port lands
// (Phase 4.0 continuation).
type Column struct {
	// Id is the column name.
	Id Identifier

	// Type is the column's SQL-ish data type as a string (e.g.
	// "INT", "STRING", "BYTES"). Will be replaced with a richer
	// `DataType` once the Type hierarchy is ported.
	Type string

	// Nullable reports whether the column allows NULL values.
	// Matters for NOT-NULL-gated simplifications (x = x → TRUE).
	Nullable bool

	// IsArray reports whether the column is an ARRAY (a repeated proto
	// field). The placeholder Type string carries only the scalar/element
	// kind, so this is the array signal callers need to type the resolved
	// column Value as an ArrayType — e.g. CARDINALITY()'s isArray() check.
	// When true, Type is the ELEMENT type string.
	IsArray bool
}

// InMemoryCatalog is a test-friendly Catalog built from a fixed list
// of tables. Keeps the test surface small — production impls bridge
// to RecordMetaData; tests construct one of these in a line or two.
type InMemoryCatalog struct {
	tables map[string]Table
}

// NewInMemoryCatalog builds a Catalog from the given tables. Table
// names key the map by their canonical String() form so lookups
// are O(1).
func NewInMemoryCatalog(tables ...Table) *InMemoryCatalog {
	c := &InMemoryCatalog{tables: make(map[string]Table, len(tables))}
	for _, t := range tables {
		c.tables[t.Name().String()] = t
	}
	return c
}

// LookupTable implements Catalog.
func (c *InMemoryCatalog) LookupTable(name QualifiedName) (Table, bool) {
	t, ok := c.tables[name.String()]
	return t, ok
}

// TableExists implements Catalog.
func (c *InMemoryCatalog) TableExists(name QualifiedName) bool {
	_, ok := c.tables[name.String()]
	return ok
}

// AllTableNames implements Catalog. Iterates the map — order is
// unspecified.
func (c *InMemoryCatalog) AllTableNames() []QualifiedName {
	out := make([]QualifiedName, 0, len(c.tables))
	for _, t := range c.tables {
		out = append(out, t.Name())
	}
	return out
}

// StaticTable is a test-friendly Table impl backing InMemoryCatalog.
// Production code should implement Table directly (bridging to
// RecordType) rather than use this value-type.
type StaticTable struct {
	TableName    QualifiedName
	TableColumns []Column
	TableIndexes []string
}

// Name implements Table.
func (t *StaticTable) Name() QualifiedName { return t.TableName }

// Columns implements Table; returns a defensive copy so callers
// can't mutate the backing slice.
func (t *StaticTable) Columns() []Column {
	if len(t.TableColumns) == 0 {
		return []Column{}
	}
	out := make([]Column, len(t.TableColumns))
	copy(out, t.TableColumns)
	return out
}

// LookupColumn implements Table — case-insensitive match on
// Identifier.Name.
func (t *StaticTable) LookupColumn(id Identifier) (Column, bool) {
	for _, c := range t.TableColumns {
		if c.Id.EqualsIgnoreQuoting(id) {
			return c, true
		}
	}
	return Column{}, false
}

// Indexes implements Table.
func (t *StaticTable) Indexes() []string {
	if len(t.TableIndexes) == 0 {
		return []string{}
	}
	out := make([]string, len(t.TableIndexes))
	copy(out, t.TableIndexes)
	return out
}
