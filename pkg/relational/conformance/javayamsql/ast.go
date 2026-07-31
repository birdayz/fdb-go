package javayamsql

import "fmt"

// BlockKind names the seven top-level document keys Java's Block.parse
// dispatches on. Anything else is "Cannot recognize the type of block".
type BlockKind string

// The recognised block keys.
const (
	BlockOptions           BlockKind = "options"
	BlockSchemaTemplate    BlockKind = "schema_template"
	BlockSetup             BlockKind = "setup"
	BlockTest              BlockKind = "test_block"
	BlockInclude           BlockKind = "include"
	BlockTransactionSetups BlockKind = "transaction_setups"
	BlockCopy              BlockKind = "copy_block"
)

// File is one parsed `.yamsql` file.
type File struct {
	// Path is the corpus-root-relative path, which is also the string an
	// `include:` uses to name this file.
	Path   string
	Blocks []*Block
}

// InertDirective is a key the corpus writes that the Java parser never reads.
//
// These are not errors — Java probes its option maps with containsKey and
// leaves anything else alone — but they are always defects in the corpus,
// because the author plainly expected the key to do something. `supported_version`
// written directly under `test_block:` instead of under its `options:` is the
// canonical example: it gates nothing at all.
type InertDirective struct {
	// Where names the construct that ignored the key, e.g. "test_block".
	Where string
	Key   string
	Line  int
}

func (d InertDirective) String() string {
	return fmt.Sprintf("%s key %q at line %d", d.Where, d.Key, d.Line)
}

// Block is a single YAML document region. Exactly one of the typed payload
// pointers is non-nil, selected by Kind.
type Block struct {
	Kind BlockKind
	Line int

	// Inert lists keys within this block that Java's parser silently ignores.
	Inert []InertDirective

	Options           *OptionsBlock
	SchemaTemplate    *SchemaTemplateBlock
	Setup             *SetupBlock
	Test              *TestBlock
	Include           *IncludeBlock
	TransactionSetups *TransactionSetupsBlock
	Copy              *CopyBlock
}

// OptionsBlock is the file preamble. Java requires it to be document 0 of a
// top-level file; an included file carrying one is a parse error unless that
// file is itself the entry point.
type OptionsBlock struct {
	SupportedVersion  *Version
	RequiredClusters  *int64
	ConnectionOptions []Entry
}

// SchemaTemplateBlock holds one or more DDL variants. The plain-string form
// yields a single variant covering all versions.
type SchemaTemplateBlock struct {
	Variants []SchemaTemplateVariant
	// ListForm records whether the source used the list-of-variants syntax,
	// which is the form subject to overlap/coverage validation.
	ListForm bool
}

// SchemaTemplateVariant is one version-gated `CREATE SCHEMA TEMPLATE` body.
type SchemaTemplateVariant struct {
	Line       int
	Definition string
	AtLeast    *Version
	LessThan   *Version
}

// SetupBlock is the manual `setup:` block: a connect target plus ordered steps.
type SetupBlock struct {
	Connect           *Value
	Steps             []*Command
	ConnectionOptions []Entry
}

// TestBlock is a named group of tests plus the knobs controlling how they run.
type TestBlock struct {
	Name    string
	Connect *Value
	Preset  string
	Options TestBlockOptions
	Tests   []*Test
}

// TestBlockOptions mirrors TestBlock.TestBlockOptions' YAML surface. Pointers
// distinguish "absent" from "set to the default", which matters because Java
// layers preset < options-map < execution-context.
type TestBlockOptions struct {
	Mode                 string
	Repetition           *int64
	Seed                 *int64
	CheckCache           *bool
	ConnectionLifecycle  string
	StatementType        string
	SupportedVersion     *Version
	ConnectionOptions    []Entry
	InitialVersionUnused bool
}

// IncludeBlock names another corpus file. Resource is root-relative, exactly as
// written; Java feeds it straight to ClassLoader.getResourceAsStream.
type IncludeBlock struct {
	Resource string
	// Resolved is the parsed target, populated by [Corpus.ParseFile]. It is nil
	// when includes were not followed.
	Resolved *File
}

// TransactionSetupsBlock maps names to reusable setup SQL, in source order.
type TransactionSetupsBlock struct {
	Setups []NamedSetup
}

// NamedSetup is one `transaction_setups` entry.
type NamedSetup struct {
	Name string
	SQL  string
	Line int
}

// CopyBlock is a cross-cluster COPY. export_limit and import_chunk_size live on
// the block itself, not inside the source/dest maps.
type CopyBlock struct {
	Source          CopyEndpoint
	Dest            CopyEndpoint
	ExportLimit     *int64
	ImportChunkSize *int64
}

// CopyEndpoint is one side of a [CopyBlock].
type CopyEndpoint struct {
	Cluster *int64
	Path    string
}

// CommandKind names the three command keys Java's Command.parse dispatches on.
type CommandKind string

// The recognised command keys. Note the two non-query ones contain a space.
const (
	CommandQuery              CommandKind = "query"
	CommandLoadSchemaTemplate CommandKind = "load schema template"
	CommandSetSchemaState     CommandKind = "set schema state"
)

// Command is one executable step: a query with its configs, or a metadata
// command carrying an opaque payload.
type Command struct {
	Kind CommandKind
	Line int

	// Query is the raw query text, parameter-injection segments included.
	Query string
	// Segments are the `!!…!!` parameter injections found in Query, in order.
	Segments []Segment
	// Payload is the value of a load-schema-template / set-schema-state command.
	Payload string
	// Configs are the checks attached to a query command. Empty means Java
	// substitutes an implicit noChecks config.
	Configs []*Config
}

// ImplicitNoChecks reports whether Java would attach a synthetic `noChecks`
// config to this command because it declares none of its own.
func (c *Command) ImplicitNoChecks() bool {
	return c.Kind == CommandQuery && len(c.Configs) == 0
}

// Test is one entry of a test_block's `tests:` list: a command plus its configs.
type Test struct {
	Line    int
	Command *Command
}

// ConfigKind names a query-config directive.
type ConfigKind string

// The config keys QueryConfig.parseConfig accepts from YAML.
const (
	ConfigResult                 ConfigKind = "result"
	ConfigUnorderedResult        ConfigKind = "unorderedResult"
	ConfigExplain                ConfigKind = "explain"
	ConfigExplainContains        ConfigKind = "explainContains"
	ConfigCount                  ConfigKind = "count"
	ConfigError                  ConfigKind = "error"
	ConfigPlanHash               ConfigKind = "planHash"
	ConfigMaxRows                ConfigKind = "maxRows"
	ConfigSupportedVersion       ConfigKind = "supported_version"
	ConfigInitialVersionAtLeast  ConfigKind = "initialVersionAtLeast"
	ConfigInitialVersionLessThan ConfigKind = "initialVersionLessThan"
	ConfigSetup                  ConfigKind = "setup"
	ConfigSetupReference         ConfigKind = "setupReference"
	ConfigDebugger               ConfigKind = "debugger"
	ConfigResultMetadata         ConfigKind = "resultMetadata"
)

// ConfigNoChecks is the implicit config Java attaches to a query with no
// configs of its own. It is never written in a corpus file, so it is not in
// configKinds and parsing a literal `noChecks:` is an error, exactly as in Java.
const ConfigNoChecks ConfigKind = "noChecks"

// Config is one check attached to a query.
type Config struct {
	Kind ConfigKind
	Line int

	// Raw is the directive's value exactly as parsed, always non-nil.
	Raw *Value

	// Rows is populated for result/unorderedResult when the value is a list of
	// row mappings. It stays nil for `result:` with a null or `!ignore` value,
	// both of which Java treats specially rather than as a row list.
	Rows []Row

	// Version is populated for supported_version and initialVersion*.
	Version *Version
	// ErrorCode is the resolved 5-character SQLSTATE for an `error:` config.
	ErrorCode string
	// Number is populated for count, planHash and maxRows.
	Number *int64
	// Text is populated for explain, explainContains, setup, setupReference
	// and debugger.
	Text string
}

// IsResultConfig reports membership of QueryConfig.RESULT_CONFIGS — the set
// that, once seen, forbids any later non-result config.
func (k ConfigKind) IsResultConfig() bool {
	switch k {
	case ConfigError, ConfigCount, ConfigResult, ConfigUnorderedResult, ConfigResultMetadata:
		return true
	default:
		return false
	}
}

// IsVersionDependentConfig reports membership of
// QueryConfig.VERSION_DEPENDENT_RESULT_CONFIGS.
func (k ConfigKind) IsVersionDependentConfig() bool {
	return k == ConfigInitialVersionAtLeast || k == ConfigInitialVersionLessThan
}

// IsResultConsumingConfig reports membership of
// QueryConfig.RESULT_CONSUMING_CONFIGS — the configs that actually draw rows,
// and so the ones `resultMetadata` requires at least one of.
func (k ConfigKind) IsResultConsumingConfig() bool {
	switch k {
	case ConfigError, ConfigCount, ConfigResult, ConfigUnorderedResult:
		return true
	default:
		return false
	}
}

// Row is one expected result row: an ordered list of cells.
type Row struct {
	Line  int
	Cells []Cell
}

// Cell is one expected column value within a [Row].
//
// The by-name/positional split is Java's Matchers.matchMap, and it is entirely
// driven by whether the mapping entry's VALUE is YAML null:
//
//	{ID: 10}        → by name "ID", expecting 10
//	{10}            → positional column 1, expecting 10   (value is null, key is the expectation)
//	{ID: !null}     → by name "ID", expecting SQL NULL    (!null is a tag, not YAML null)
//	{!pos 2: 10}    → positional column 2, expecting 10
//
// See [Value.IsNull] for why the third case is not the second.
type Cell struct {
	Line int
	// Key is the mapping key node, which carries the !pos tag when present.
	Key *Value
	// Val is the mapping value node, always non-nil; YAML null when elided.
	Val *Value
	// Pos is the explicit 1-based column from a `!pos n` key, else nil.
	Pos *int64
}

// Positional reports whether this cell is matched by column number rather than
// by column name.
func (c Cell) Positional() bool { return c.Val.IsNull() || c.Pos != nil }

// Expected returns the value this cell asserts: the mapping value, or the key
// itself when the value is YAML null.
func (c Cell) Expected() *Value {
	if c.Val.IsNull() {
		return c.Key
	}
	return c.Val
}

// ColumnName returns the column name for a by-name cell. It is meaningless for
// a positional cell and reports ok=false there.
func (c Cell) ColumnName() (string, bool) {
	if c.Positional() {
		return "", false
	}
	return c.Key.AsString()
}

// ColumnIndex returns the 1-based column this cell matches, given its ordinal
// position in the row. An explicit `!pos` overrides the ordinal.
func (c Cell) ColumnIndex(ordinal int) int64 {
	if c.Pos != nil {
		return *c.Pos
	}
	return int64(ordinal)
}

// SegmentKind distinguishes a parameter-injection segment's shape.
type SegmentKind uint8

// Segment shapes.
const (
	// SegmentLiteral is a bound literal: `!! 10 !!`.
	SegmentLiteral SegmentKind = iota
	// SegmentUnbound is a generator that a Random binds at execution:
	// `!! !r [2, 9] !!`.
	SegmentUnbound
)

// Segment is one `!!…!!` parameter injection inside a query string.
//
// Phase 0 represents these; binding and substitution belong to execution.
type Segment struct {
	// Raw is the full segment including both `!!` delimiters, which is the
	// exact substring Java replaces when adapting the query.
	Raw string
	// Body is the mini-YAML between the delimiters.
	Body string
	// Value is Body parsed with the parameter-injection tag set.
	Value *Value
	Kind  SegmentKind
	// Offset is Raw's byte offset within the query string.
	Offset int
}
