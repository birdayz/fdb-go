package seedcorpus

import (
	"bytes"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"path"
	"strconv"
	"strings"

	"fdb.dev/internal/relational/conformance/plandiff"
)

const currentSchemaVersion = 1

//go:embed testdata
var embeddedCorpusFS embed.FS

// CorpusLoadError reports a failure reading, parsing, or validating a JSON corpus.
type CorpusLoadError struct {
	Path   string
	Detail string
	Err    error
}

func (e *CorpusLoadError) Error() string {
	switch {
	case e.Path != "" && e.Err != nil:
		return fmt.Sprintf("seedcorpus: load %s: %s: %v", e.Path, e.Detail, e.Err)
	case e.Path != "":
		return fmt.Sprintf("seedcorpus: load %s: %s", e.Path, e.Detail)
	case e.Err != nil:
		return fmt.Sprintf("seedcorpus: load: %s: %v", e.Detail, e.Err)
	default:
		return fmt.Sprintf("seedcorpus: load: %s", e.Detail)
	}
}

func (e *CorpusLoadError) Unwrap() error { return e.Err }

// CorpusEncodeError reports a failure serializing corpus entries to JSON.
type CorpusEncodeError struct {
	Entry  string
	Detail string
	Err    error
}

func (e *CorpusEncodeError) Error() string {
	switch {
	case e.Entry != "" && e.Err != nil:
		return fmt.Sprintf("seedcorpus: encode %q: %s: %v", e.Entry, e.Detail, e.Err)
	case e.Entry != "":
		return fmt.Sprintf("seedcorpus: encode %q: %s", e.Entry, e.Detail)
	case e.Err != nil:
		return fmt.Sprintf("seedcorpus: encode: %s: %v", e.Detail, e.Err)
	default:
		return fmt.Sprintf("seedcorpus: encode: %s", e.Detail)
	}
}

func (e *CorpusEncodeError) Unwrap() error { return e.Err }

// CellCodecError reports an invalid or unsupported typed cell value in GoExpectedRows.
type CellCodecError struct {
	Type   string
	Detail string
	Err    error
}

func (e *CellCodecError) Error() string {
	switch {
	case e.Type != "" && e.Err != nil:
		return fmt.Sprintf("cell %s: %s: %v", e.Type, e.Detail, e.Err)
	case e.Type != "":
		return fmt.Sprintf("cell %s: %s", e.Type, e.Detail)
	case e.Err != nil:
		return fmt.Sprintf("cell: %s: %v", e.Detail, e.Err)
	default:
		return fmt.Sprintf("cell: %s", e.Detail)
	}
}

func (e *CellCodecError) Unwrap() error { return e.Err }

type jsonCorpusFile struct {
	SchemaVersion int            `json:"schema_version"`
	Entries       []jsonRunQuery `json:"entries"`
}

type jsonRunQuery struct {
	Name           string          `json:"name"`
	SchemaTemplate string          `json:"schema_template"`
	SetupSqls      []string        `json:"setup_sqls"`
	Query          string          `json:"query"`
	Divergence     *jsonDivergence `json:"divergence,omitempty"`
}

type jsonDivergence struct {
	Direction       plandiff.DivergenceDirection `json:"direction"`
	Reason          string                       `json:"reason"`
	GoExpectedRows  [][]jsonCell                 `json:"go_expected_rows"`
	GoErrorContains string                       `json:"go_error_contains,omitempty"`
}

// jsonCell preserves exact Go dynamic types and IEEE-754 float bits across JSON.
type jsonCell struct {
	Val any
}

func (c jsonCell) MarshalJSON() ([]byte, error) {
	switch v := c.Val.(type) {
	case nil:
		return []byte("null"), nil
	case bool:
		if v {
			return []byte("true"), nil
		}
		return []byte("false"), nil
	case string:
		return marshalNoEscape(v)
	case []any:
		if v == nil {
			return []byte(`{"type":"nil_slice"}`), nil
		}
		cells := make([]jsonCell, len(v))
		for i, elem := range v {
			cells[i] = jsonCell{Val: elem}
		}
		return marshalNoEscape(cells)
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || (v == 0 && math.Signbit(v)) {
			return marshalNoEscape(struct {
				Type string `json:"type"`
				Bits string `json:"bits"`
			}{
				Type: "float64",
				Bits: fmt.Sprintf("0x%016x", math.Float64bits(v)),
			})
		}
		return marshalNoEscape(struct {
			Type  string  `json:"type"`
			Value float64 `json:"value"`
		}{
			Type:  "float64",
			Value: v,
		})
	case float32:
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) || (v == 0 && math.Signbit(float64(v))) {
			return marshalNoEscape(struct {
				Type string `json:"type"`
				Bits string `json:"bits"`
			}{
				Type: "float32",
				Bits: fmt.Sprintf("0x%08x", math.Float32bits(v)),
			})
		}
		return marshalNoEscape(struct {
			Type  string  `json:"type"`
			Value float32 `json:"value"`
		}{
			Type:  "float32",
			Value: v,
		})
	case int64:
		return marshalNoEscape(struct {
			Type  string `json:"type"`
			Value int64  `json:"value"`
		}{
			Type:  "int64",
			Value: v,
		})
	case int32:
		return marshalNoEscape(struct {
			Type  string `json:"type"`
			Value int32  `json:"value"`
		}{
			Type:  "int32",
			Value: v,
		})
	case int:
		return marshalNoEscape(struct {
			Type  string `json:"type"`
			Value int    `json:"value"`
		}{
			Type:  "int",
			Value: v,
		})
	case uint64:
		return marshalNoEscape(struct {
			Type  string `json:"type"`
			Value uint64 `json:"value"`
		}{
			Type:  "uint64",
			Value: v,
		})
	case uint32:
		return marshalNoEscape(struct {
			Type  string `json:"type"`
			Value uint32 `json:"value"`
		}{
			Type:  "uint32",
			Value: v,
		})
	case []byte:
		if v == nil {
			return []byte(`{"type":"bytes","value":null}`), nil
		}
		return marshalNoEscape(struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		}{
			Type:  "bytes",
			Value: base64.StdEncoding.EncodeToString(v),
		})
	default:
		return nil, &CellCodecError{
			Type:   fmt.Sprintf("%T", c.Val),
			Detail: "unsupported cell Go type",
		}
	}
}

func (c *jsonCell) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return &CellCodecError{Detail: "empty JSON cell"}
	}
	switch trimmed[0] {
	case 'n':
		if bytes.Equal(trimmed, []byte("null")) {
			c.Val = nil
			return nil
		}
		return &CellCodecError{Detail: fmt.Sprintf("invalid cell token %q", string(trimmed))}
	case 't', 'f':
		var b bool
		if err := json.Unmarshal(trimmed, &b); err != nil {
			return &CellCodecError{Type: "bool", Detail: "invalid boolean cell", Err: err}
		}
		c.Val = b
		return nil
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return &CellCodecError{Type: "string", Detail: "invalid string cell", Err: err}
		}
		c.Val = s
		return nil
	case '[':
		var elems []jsonCell
		if err := json.Unmarshal(trimmed, &elems); err != nil {
			return &CellCodecError{Type: "slice", Detail: "invalid array cell", Err: err}
		}
		out := make([]any, len(elems))
		for i, elem := range elems {
			out[i] = elem.Val
		}
		c.Val = out
		return nil
	case '{':
		return c.unmarshalTaggedObject(trimmed)
	default:
		return &CellCodecError{
			Detail: fmt.Sprintf("unexpected cell JSON token %q (numbers must use typed wrapper)", string(trimmed)),
		}
	}
}

func (c *jsonCell) unmarshalTaggedObject(trimmed []byte) error {
	var raw struct {
		Type  *string         `json:"type"`
		Value json.RawMessage `json:"value"`
		Bits  *string         `json:"bits"`
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return &CellCodecError{Detail: "invalid typed cell object", Err: err}
	}
	if err := ensureEOF(dec); err != nil {
		return &CellCodecError{Detail: "trailing tokens in typed cell object", Err: err}
	}
	if raw.Type == nil || *raw.Type == "" {
		return &CellCodecError{Detail: `typed cell missing required "type" field`}
	}

	typeName := *raw.Type
	hasValue := len(bytes.TrimSpace(raw.Value)) > 0

	switch typeName {
	case "nil_slice":
		if raw.Bits != nil {
			return &CellCodecError{Type: typeName, Detail: `unexpected "bits" field`}
		}
		if hasValue {
			return &CellCodecError{Type: typeName, Detail: `unexpected "value" field`}
		}
		c.Val = []any(nil)
		return nil
	case "float64":
		if raw.Bits != nil {
			if hasValue {
				return &CellCodecError{Type: typeName, Detail: `specify either "value" or "bits", not both`}
			}
			bits, err := parseHexBits(*raw.Bits, typeName, 64)
			if err != nil {
				return err
			}
			c.Val = math.Float64frombits(bits)
			return nil
		}
		v, err := decodeStrictValue[float64](raw.Value, typeName)
		if err != nil {
			return err
		}
		c.Val = v
		return nil
	case "float32":
		if raw.Bits != nil {
			if hasValue {
				return &CellCodecError{Type: typeName, Detail: `specify either "value" or "bits", not both`}
			}
			bits, err := parseHexBits(*raw.Bits, typeName, 32)
			if err != nil {
				return err
			}
			c.Val = math.Float32frombits(uint32(bits))
			return nil
		}
		v, err := decodeStrictValue[float32](raw.Value, typeName)
		if err != nil {
			return err
		}
		c.Val = v
		return nil
	case "int64":
		if raw.Bits != nil {
			return &CellCodecError{Type: typeName, Detail: `unexpected "bits" field`}
		}
		v, err := decodeStrictValue[int64](raw.Value, typeName)
		if err != nil {
			return err
		}
		c.Val = v
		return nil
	case "int32":
		if raw.Bits != nil {
			return &CellCodecError{Type: typeName, Detail: `unexpected "bits" field`}
		}
		v, err := decodeStrictValue[int32](raw.Value, typeName)
		if err != nil {
			return err
		}
		c.Val = v
		return nil
	case "int":
		if raw.Bits != nil {
			return &CellCodecError{Type: typeName, Detail: `unexpected "bits" field`}
		}
		v, err := decodeStrictValue[int](raw.Value, typeName)
		if err != nil {
			return err
		}
		c.Val = v
		return nil
	case "uint64":
		if raw.Bits != nil {
			return &CellCodecError{Type: typeName, Detail: `unexpected "bits" field`}
		}
		v, err := decodeStrictValue[uint64](raw.Value, typeName)
		if err != nil {
			return err
		}
		c.Val = v
		return nil
	case "uint32":
		if raw.Bits != nil {
			return &CellCodecError{Type: typeName, Detail: `unexpected "bits" field`}
		}
		v, err := decodeStrictValue[uint32](raw.Value, typeName)
		if err != nil {
			return err
		}
		c.Val = v
		return nil
	case "bytes":
		if raw.Bits != nil {
			return &CellCodecError{Type: typeName, Detail: `unexpected "bits" field`}
		}
		trimmedVal := bytes.TrimSpace(raw.Value)
		if len(trimmedVal) == 0 {
			return &CellCodecError{Type: typeName, Detail: `missing "value" field`}
		}
		if bytes.Equal(trimmedVal, []byte("null")) {
			c.Val = []byte(nil)
			return nil
		}
		encoded, err := decodeStrictValue[string](trimmedVal, typeName)
		if err != nil {
			return err
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return &CellCodecError{Type: typeName, Detail: "invalid base64 value", Err: err}
		}
		if decoded == nil {
			decoded = []byte{}
		}
		c.Val = decoded
		return nil
	default:
		return &CellCodecError{Type: typeName, Detail: "unknown typed cell type"}
	}
}

func decodeStrictValue[T any](raw json.RawMessage, typeName string) (T, error) {
	var zero T
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return zero, &CellCodecError{Type: typeName, Detail: `missing "value" field`}
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return zero, &CellCodecError{Type: typeName, Detail: `unexpected null "value" field`}
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var out T
	if err := dec.Decode(&out); err != nil {
		return zero, &CellCodecError{Type: typeName, Detail: `invalid "value" field`, Err: err}
	}
	if err := ensureEOF(dec); err != nil {
		return zero, &CellCodecError{Type: typeName, Detail: `trailing tokens in "value" field`, Err: err}
	}
	return out, nil
}

func parseHexBits(s, typeName string, bitSize int) (uint64, error) {
	hexDigits, ok := strings.CutPrefix(s, "0x")
	if !ok {
		return 0, &CellCodecError{Type: typeName, Detail: fmt.Sprintf("bits %q must have 0x prefix", s)}
	}
	wantLen := bitSize / 4
	if len(hexDigits) != wantLen {
		return 0, &CellCodecError{Type: typeName, Detail: fmt.Sprintf("bits %q must have %d hex digits", s, wantLen)}
	}
	v, err := strconv.ParseUint(hexDigits, 16, bitSize)
	if err != nil {
		return 0, &CellCodecError{Type: typeName, Detail: fmt.Sprintf("invalid hex bits %q", s), Err: err}
	}
	return v, nil
}

func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func ensureEOF(dec *json.Decoder) error {
	var extra any
	err := dec.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("unexpected trailing JSON token")
}

func validDivergenceDirection(d plandiff.DivergenceDirection) bool {
	switch d {
	case plandiff.DivergenceJavaErrorsGoCorrect,
		plandiff.DivergenceJavaWrongRowsGoCorrect,
		plandiff.DivergenceJavaIntermittentGoCorrect,
		plandiff.DivergenceBothErrorMessagesDrift,
		plandiff.DivergenceJavaSucceedsGoRejects,
		plandiff.DivergenceUnorderedRowOrderDiffers:
		return true
	default:
		return false
	}
}

// LoadEmbedded loads the JSON corpus embedded from testdata/.
func LoadEmbedded() ([]plandiff.RunQuery, error) {
	return LoadFromFS(embeddedCorpusFS, "testdata")
}

// LoadFromFS walks dir inside fsys in lexical order and decodes every .json shard.
func LoadFromFS(fsys fs.FS, dir string) ([]plandiff.RunQuery, error) {
	if fsys == nil {
		return nil, &CorpusLoadError{Path: dir, Detail: "nil filesystem"}
	}
	cleanDir := path.Clean(strings.TrimSpace(dir))
	if dir == "" {
		cleanDir = "."
	}

	var files []string
	err := fs.WalkDir(fsys, cleanDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return &CorpusLoadError{Path: p, Detail: "walk corpus directory", Err: walkErr}
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		files = append(files, p)
		return nil
	})
	if err != nil {
		var loadErr *CorpusLoadError
		if errors.As(err, &loadErr) {
			return nil, err
		}
		return nil, &CorpusLoadError{Path: cleanDir, Detail: "walk corpus directory", Err: err}
	}
	if len(files) == 0 {
		return nil, &CorpusLoadError{Path: cleanDir, Detail: "no JSON corpus files found"}
	}

	out := make([]plandiff.RunQuery, 0)
	for _, filePath := range files {
		raw, err := fs.ReadFile(fsys, filePath)
		if err != nil {
			return nil, &CorpusLoadError{Path: filePath, Detail: "read corpus file", Err: err}
		}
		entries, err := decodeCorpusFile(filePath, raw)
		if err != nil {
			return nil, err
		}
		out = append(out, entries...)
	}
	return out, nil
}

func decodeCorpusFile(filePath string, raw []byte) ([]plandiff.RunQuery, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()

	var cf jsonCorpusFile
	if err := dec.Decode(&cf); err != nil {
		return nil, &CorpusLoadError{Path: filePath, Detail: "decode JSON", Err: err}
	}
	if err := ensureEOF(dec); err != nil {
		return nil, &CorpusLoadError{Path: filePath, Detail: "trailing data after JSON document", Err: err}
	}
	if cf.SchemaVersion != currentSchemaVersion {
		return nil, &CorpusLoadError{
			Path:   filePath,
			Detail: fmt.Sprintf("unsupported schema_version %d (want %d)", cf.SchemaVersion, currentSchemaVersion),
		}
	}
	if cf.Entries == nil {
		return nil, &CorpusLoadError{Path: filePath, Detail: `missing or null "entries" array`}
	}

	out := make([]plandiff.RunQuery, len(cf.Entries))
	for i, jq := range cf.Entries {
		rq, err := decodeRunQuery(filePath, i, jq)
		if err != nil {
			return nil, err
		}
		out[i] = rq
	}
	return out, nil
}

func decodeRunQuery(filePath string, idx int, jq jsonRunQuery) (plandiff.RunQuery, error) {
	if jq.Name == "" {
		return plandiff.RunQuery{}, &CorpusLoadError{
			Path:   filePath,
			Detail: fmt.Sprintf(`entry[%d] missing required "name"`, idx),
		}
	}
	if jq.Query == "" {
		return plandiff.RunQuery{}, &CorpusLoadError{
			Path:   filePath,
			Detail: fmt.Sprintf(`entry[%d] (%s) missing required "query"`, idx, jq.Name),
		}
	}

	var setupSQLs []string
	if jq.SetupSqls != nil {
		setupSQLs = make([]string, len(jq.SetupSqls))
		copy(setupSQLs, jq.SetupSqls)
	}

	var div *plandiff.Divergence
	if jq.Divergence != nil {
		if !validDivergenceDirection(jq.Divergence.Direction) {
			return plandiff.RunQuery{}, &CorpusLoadError{
				Path:   filePath,
				Detail: fmt.Sprintf("entry[%d] (%s) has unknown divergence direction %q", idx, jq.Name, jq.Divergence.Direction),
			}
		}
		var expectedRows [][]any
		if jq.Divergence.GoExpectedRows != nil {
			expectedRows = make([][]any, len(jq.Divergence.GoExpectedRows))
			for rIdx, row := range jq.Divergence.GoExpectedRows {
				if row == nil {
					continue
				}
				decodedRow := make([]any, len(row))
				for cIdx, cell := range row {
					decodedRow[cIdx] = cell.Val
				}
				expectedRows[rIdx] = decodedRow
			}
		}
		div = &plandiff.Divergence{
			Reason:          jq.Divergence.Reason,
			Direction:       jq.Divergence.Direction,
			GoExpectedRows:  expectedRows,
			GoErrorContains: jq.Divergence.GoErrorContains,
		}
	}

	return plandiff.RunQuery{
		Name:           jq.Name,
		SetupSqls:      setupSQLs,
		Query:          jq.Query,
		SchemaTemplate: jq.SchemaTemplate,
		Divergence:     div,
	}, nil
}

// EncodeJSON serializes entries into the canonical versioned JSON corpus format.
func EncodeJSON(entries []plandiff.RunQuery) ([]byte, error) {
	cf := jsonCorpusFile{
		SchemaVersion: currentSchemaVersion,
		Entries:       make([]jsonRunQuery, len(entries)),
	}
	for i, entry := range entries {
		jq, err := encodeRunQuery(entry)
		if err != nil {
			return nil, err
		}
		cf.Entries[i] = jq
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cf); err != nil {
		return nil, &CorpusEncodeError{Detail: "encode JSON", Err: err}
	}
	return buf.Bytes(), nil
}

func encodeRunQuery(entry plandiff.RunQuery) (jsonRunQuery, error) {
	var setupSQLs []string
	if entry.SetupSqls != nil {
		setupSQLs = make([]string, len(entry.SetupSqls))
		copy(setupSQLs, entry.SetupSqls)
	}

	var div *jsonDivergence
	if entry.Divergence != nil {
		if !validDivergenceDirection(entry.Divergence.Direction) {
			return jsonRunQuery{}, &CorpusEncodeError{
				Entry:  entry.Name,
				Detail: fmt.Sprintf("unknown divergence direction %q", entry.Divergence.Direction),
			}
		}
		var expectedRows [][]jsonCell
		if entry.Divergence.GoExpectedRows != nil {
			expectedRows = make([][]jsonCell, len(entry.Divergence.GoExpectedRows))
			for rIdx, row := range entry.Divergence.GoExpectedRows {
				if row == nil {
					continue
				}
				encodedRow := make([]jsonCell, len(row))
				for cIdx, cell := range row {
					encodedRow[cIdx] = jsonCell{Val: cell}
				}
				expectedRows[rIdx] = encodedRow
			}
		}
		div = &jsonDivergence{
			Direction:       entry.Divergence.Direction,
			Reason:          entry.Divergence.Reason,
			GoExpectedRows:  expectedRows,
			GoErrorContains: entry.Divergence.GoErrorContains,
		}
	}

	return jsonRunQuery{
		Name:           entry.Name,
		SchemaTemplate: entry.SchemaTemplate,
		SetupSqls:      setupSQLs,
		Query:          entry.Query,
		Divergence:     div,
	}, nil
}
