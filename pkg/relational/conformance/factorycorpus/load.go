package factorycorpus

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"fdb.dev/pkg/relational/conformance/javayamsql"
	"fdb.dev/pkg/relational/conformance/yamsql"
)

// Scenario is one committed factory scenario: its provenance header, the
// reconstructed logical scenario (schema, setup, tests with frozen rows), and
// the parsed yamsql document triple it executes as.
type Scenario struct {
	// Path is the family file the scenario was loaded from.
	Path string
	// Header is the scenario's provenance block.
	Header Header
	// Doc is the logical scenario reconstructed from the yamsql blocks: the
	// internal carrier the writer marshals and the determinism gate compares
	// against regenerated candidates. Row cells are Go values (int64, float64,
	// float32, string, bool, nil) decoded from the typed yamsql cells.
	Doc *yamsql.Scenario
	// Blocks is the scenario's parsed (schema_template, setup, test_block)
	// triple with its `connect:` indices REBASED to 1, so the scenario executes
	// alone through the javayamsql runner exactly as it would inside the whole
	// family file.
	Blocks []*javayamsql.Block
}

// SubFile returns the scenario as a standalone parsed yamsql file, the unit
// the runner executes. The path names the family file and the scenario so a
// failure locates both.
func (s *Scenario) SubFile() *javayamsql.File {
	return &javayamsql.File{Path: s.Path + "#" + s.Header.Name, Blocks: s.Blocks}
}

// FamilyFile is one committed `.yamsql` family file.
type FamilyFile struct {
	Path string
	// Family is the file-level family key; every scenario's feature vector
	// maps onto it.
	Family    string
	Scenarios []*Scenario
}

// Load reads one committed family file.
//
// Parsing is NOT re-implemented: the yamsql structure goes through
// javayamsql.Parse, the same strict parser that gates the vendored Java
// corpus, so the factory corpus inherits every block/config/tag check that
// parser grew. What this loader adds is the provenance contract and the
// cross-checks binding provenance to documents — a header describing a
// different scenario than the one under it is worse than no header, because
// the census would report it with total confidence.
func Load(path string) (*FamilyFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return LoadBytes(path, data)
}

// LoadBytes is Load over bytes already in hand. path is used only for the
// family/file-name cross-check and for error context; nothing is read from
// disk.
//
// It exists so a re-emitted family can be parsed back WITHOUT being written
// first: cmd/factory-rebless verifies that marshalling preserved every frozen
// row before it commits any bytes to the tree, and a check that has to write
// the file in order to verify it is not a check, it is a rollback.
func LoadBytes(path string, data []byte) (*FamilyFile, error) {
	parsed, err := javayamsql.Parse(filepath.Base(path), data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	fileKeys, headers, err := scanProvenance(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	fv, ok := fileKeys["format-version"]
	if !ok {
		return nil, fmt.Errorf("%s: missing file-level format-version", path)
	}
	if v, err := strconv.Atoi(fv); err != nil || v != FormatVersion {
		return nil, fmt.Errorf("%s: format-version %q, this build understands %d", path, fv, FormatVersion)
	}
	family, ok := fileKeys["family"]
	if !ok {
		return nil, fmt.Errorf("%s: missing file-level family key", path)
	}
	if got := FamilyFileName(family); got != filepath.Base(path) {
		return nil, fmt.Errorf("%s: family %q maps to file name %s", path, family, got)
	}

	if len(parsed.Blocks) != 3*len(headers) {
		return nil, fmt.Errorf("%s: %d yamsql blocks for %d provenance blocks; every scenario is a (schema_template, setup, test_block) triple",
			path, len(parsed.Blocks), len(headers))
	}

	out := &FamilyFile{Path: path, Family: family}
	for i, h := range headers {
		blocks := parsed.Blocks[3*i : 3*i+3]
		sc, err := buildScenario(path, h, blocks, i+1)
		if err != nil {
			return nil, fmt.Errorf("%s: scenario %s: %w", path, h.Name, err)
		}
		if got := FamilyOf(h.FeatureVector); got != family {
			return nil, fmt.Errorf("%s: scenario %s has family %s, file is %s", path, h.Name, got, family)
		}
		out.Scenarios = append(out.Scenarios, sc)
	}
	return out, nil
}

// scanProvenance splits the file's comment lines into the file-level keys
// (before the first scenario block) and one ordered Header per `# scenario:`
// block.
func scanProvenance(src string) (map[string]string, []Header, error) {
	fileKeys := map[string]string{}
	var headers []Header
	var block []string
	inScenario := false
	flush := func() error {
		if !inScenario {
			return nil
		}
		h, err := ParseHeader(block)
		if err != nil {
			return err
		}
		headers = append(headers, h)
		block, inScenario = nil, false
		return nil
	}
	for _, line := range strings.Split(src, "\n") {
		if !strings.HasPrefix(line, "#") {
			if err := flush(); err != nil {
				return nil, nil, err
			}
			continue
		}
		m := headerKeyLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if m[1] == scenarioKey {
			if err := flush(); err != nil {
				return nil, nil, err
			}
			inScenario = true
		}
		if inScenario {
			block = append(block, line)
			continue
		}
		if prev, dup := fileKeys[m[1]]; dup {
			return nil, nil, fmt.Errorf("file-level key %q appears twice (%q then %q)", m[1], prev, strings.TrimSpace(m[2]))
		}
		fileKeys[m[1]] = strings.TrimSpace(m[2])
	}
	if err := flush(); err != nil {
		return nil, nil, err
	}
	return fileKeys, headers, nil
}

// buildScenario checks one document triple against its provenance and
// reconstructs the logical scenario.
func buildScenario(path string, h Header, blocks []*javayamsql.Block, connectIdx int) (*Scenario, error) {
	if blocks[0].Kind != javayamsql.BlockSchemaTemplate ||
		blocks[1].Kind != javayamsql.BlockSetup ||
		blocks[2].Kind != javayamsql.BlockTest {
		return nil, fmt.Errorf("document triple is (%s, %s, %s), want (schema_template, setup, test_block)",
			blocks[0].Kind, blocks[1].Kind, blocks[2].Kind)
	}
	for _, blk := range blocks {
		if len(blk.Inert) > 0 {
			return nil, fmt.Errorf("inert directive %s: the writer never emits keys the runner ignores", blk.Inert[0])
		}
	}

	st := blocks[0].SchemaTemplate
	if st.ListForm || len(st.Variants) != 1 {
		return nil, fmt.Errorf("schema_template must be a single plain-string variant")
	}
	doc := &yamsql.Scenario{Name: h.Name, SchemaTemplate: st.Variants[0].Definition}

	setup := blocks[1].Setup
	if err := checkConnect(setup.Connect, connectIdx, "setup"); err != nil {
		return nil, err
	}
	if len(setup.ConnectionOptions) > 0 {
		return nil, fmt.Errorf("setup carries connection_options the writer never emits")
	}
	for _, step := range setup.Steps {
		if step.Kind != javayamsql.CommandQuery {
			return nil, fmt.Errorf("setup step is %q; the factory emits only queries", step.Kind)
		}
		if len(step.Segments) > 0 {
			return nil, fmt.Errorf("setup step carries a parameter injection")
		}
		doc.Setup = append(doc.Setup, step.Query)
	}

	tb := blocks[2].Test
	if tb.Name != h.Name {
		return nil, fmt.Errorf("test_block name %q != provenance scenario %q", tb.Name, h.Name)
	}
	if tb.Preset != "single_repetition_ordered" {
		return nil, fmt.Errorf("preset %q, the writer emits single_repetition_ordered", tb.Preset)
	}
	if err := checkConnect(tb.Connect, connectIdx, "test_block"); err != nil {
		return nil, err
	}
	if len(tb.Tests) == 0 {
		return nil, fmt.Errorf("scenario has no tests")
	}
	for i, t := range tb.Tests {
		tc, err := buildTest(t)
		if err != nil {
			return nil, fmt.Errorf("tests[%d]: %w", i, err)
		}
		doc.Tests = append(doc.Tests, tc)
	}

	return &Scenario{
		Path:   path,
		Header: h,
		Doc:    doc,
		Blocks: rebase(blocks),
	}, nil
}

func checkConnect(v *javayamsql.Value, want int, where string) error {
	if v == nil {
		return fmt.Errorf("%s has no connect index; Java would resolve it against every registration in the file", where)
	}
	n, ok := v.AsInt()
	if !ok || n != int64(want) {
		return fmt.Errorf("%s connect is %v, want %d (its own schema registration)", where, v, want)
	}
	return nil
}

// buildTest converts one parsed test into the internal carrier: exactly one
// result-consuming config, by-name cells, typed values.
func buildTest(t *javayamsql.Test) (yamsql.Test, error) {
	var out yamsql.Test
	cmd := t.Command
	if cmd.Kind != javayamsql.CommandQuery {
		return out, fmt.Errorf("command is %q, want query", cmd.Kind)
	}
	if len(cmd.Segments) > 0 {
		return out, fmt.Errorf("query carries a parameter injection")
	}
	out.Query = cmd.Query
	if len(cmd.Configs) != 1 {
		return out, fmt.Errorf("%d configs, the writer emits exactly one result config", len(cmd.Configs))
	}
	cfg := cmd.Configs[0]
	switch cfg.Kind {
	case javayamsql.ConfigResult:
	case javayamsql.ConfigUnorderedResult:
		out.Unordered = true
	default:
		return out, fmt.Errorf("config %q, want result or unorderedResult", cfg.Kind)
	}
	if cfg.Raw.IsNull() || cfg.Raw.TagName() != "" {
		return out, fmt.Errorf("result value must be an explicit row list (`[]` for an empty result)")
	}
	out.Rows = [][]any{}
	for i, row := range cfg.Rows {
		var cells []any
		for j, cell := range row.Cells {
			if cell.Positional() {
				return out, fmt.Errorf("rows[%d][%d] is positional; the writer emits by-name cells", i, j)
			}
			name, _ := cell.ColumnName()
			if i == 0 {
				out.Columns = append(out.Columns, name)
			} else {
				if j >= len(out.Columns) || out.Columns[j] != name {
					return out, fmt.Errorf("rows[%d][%d] is named %q, row 0 names column %d %q", i, j, name, j, columnAt(out.Columns, j))
				}
			}
			v, err := cellValue(cell.Expected())
			if err != nil {
				return out, fmt.Errorf("rows[%d][%d] (%s): %w", i, j, name, err)
			}
			cells = append(cells, v)
		}
		if i > 0 && len(cells) != len(out.Columns) {
			return out, fmt.Errorf("rows[%d] has %d cells, row 0 has %d", i, len(cells), len(out.Columns))
		}
		out.Rows = append(out.Rows, cells)
	}
	return out, nil
}

func columnAt(cols []string, j int) string {
	if j < len(cols) {
		return cols[j]
	}
	return "<none>"
}

// cellValue decodes one typed yamsql cell into the Go value the writer would
// have frozen it from. The mapping is the inverse of valueScalar, and only the
// spellings the writer emits are accepted — a cell in any other form did not
// come from the factory and must not be silently coerced.
func cellValue(v *javayamsql.Value) (any, error) {
	switch v.TagName() {
	case javayamsql.TagNull:
		return nil, nil
	case javayamsql.TagFloat:
		u := v.Untag()
		switch u.Kind {
		case javayamsql.KindFloat:
			return float32(u.Float), nil
		case javayamsql.KindInt:
			return float32(u.Int), nil
		}
		return nil, fmt.Errorf("!f wraps %s, want a number", u.Kind)
	case "":
	default:
		return nil, fmt.Errorf("tag %s is not one the factory writer emits", v.TagName())
	}
	switch v.Kind {
	case javayamsql.KindBool:
		return v.Bool, nil
	case javayamsql.KindInt:
		return v.Int, nil
	case javayamsql.KindFloat:
		if math.IsNaN(v.Float) || math.IsInf(v.Float, 0) {
			return nil, fmt.Errorf("non-finite float expectation")
		}
		return v.Float, nil
	case javayamsql.KindString:
		return v.Str, nil
	}
	return nil, fmt.Errorf("cell kind %s is not one the factory writer emits", v.Kind)
}

// rebase copies the triple with its connect indices set to 1, the value they
// must hold when the scenario runs as a standalone file with a single schema
// registration.
func rebase(blocks []*javayamsql.Block) []*javayamsql.Block {
	one := &javayamsql.Value{Kind: javayamsql.KindInt, Int: 1}

	setupBlk := *blocks[1]
	setup := *setupBlk.Setup
	setup.Connect = one
	setupBlk.Setup = &setup

	testBlk := *blocks[2]
	tb := *testBlk.Test
	tb.Connect = one
	testBlk.Test = &tb

	return []*javayamsql.Block{blocks[0], &setupBlk, &testBlk}
}

// LoadDir reads every committed family file in dir and returns the flattened,
// dedup-checked scenario list, sorted by (file, position).
//
// An empty directory is an ERROR, not an empty result: every caller here is a
// gate, and a gate over zero files is green forever.
func LoadDir(dir string) ([]*Scenario, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.yamsql"))
	if err != nil {
		return nil, fmt.Errorf("glob %s: %w", dir, err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no *.yamsql under %s: a corpus gate over an empty corpus passes vacuously", dir)
	}
	sort.Strings(matches)
	var out []*Scenario
	seenKey := map[string]string{}
	seenName := map[string]string{}
	for _, m := range matches {
		f, err := Load(m)
		if err != nil {
			return nil, err
		}
		for _, sc := range f.Scenarios {
			id := m + "#" + sc.Header.Name
			if prev, dup := seenName[sc.Header.Name]; dup {
				return nil, fmt.Errorf("%s and %s both commit scenario %s", prev, id, sc.Header.Name)
			}
			seenName[sc.Header.Name] = id
			if prev, dup := seenKey[sc.Header.DedupKey]; dup {
				return nil, fmt.Errorf("%s and %s share dedup key %s: the corpus is committing the same (feature vector, plan shape) point twice, which is volume without coverage",
					prev, id, sc.Header.DedupKey)
			}
			seenKey[sc.Header.DedupKey] = id
			out = append(out, sc)
		}
	}
	return out, nil
}

// TestdataDir is the corpus directory relative to this package.
const TestdataDir = "testdata"
