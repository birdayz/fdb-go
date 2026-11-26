package yamsql

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"gopkg.in/yaml.v3"
)

const (
	numericManifestPath = "testdata/semantic/numeric-v1.json"
	numericYAMLPath     = "testdata/semantic/numeric-v1.yaml"
)

type (
	numericInput    struct{ Name, Bits, Literal string }
	numericManifest struct {
		Version                       int
		Catalog, Functions, Producers []string
		Inputs                        []numericInput
		Answers                       map[string][]string
		Outside                       []string
		Oracle                        string
	}
)

func loadNumericManifest() (*numericManifest, error) {
	data, err := os.ReadFile(numericManifestPath)
	if err != nil {
		return nil, err
	}
	var m numericManifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("trailing manifest data: %v", err)
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *numericManifest) validate() error { return m.validateCatalog(values.ScalarFunctionNames()) }

func (m *numericManifest) validateCatalog(liveCatalog []string) error {
	if m.Version != 1 || m.Oracle == "" || len(m.Outside) == 0 {
		return fmt.Errorf("missing envelope version, oracle or scope")
	}
	if !reflect.DeepEqual(m.Catalog, liveCatalog) {
		return fmt.Errorf("catalog drift or empty inventory")
	}
	if !reflect.DeepEqual(m.Functions, []string{"FLOOR", "CEIL", "CEILING", "ROUND", "POWER", "POW"}) || !reflect.DeepEqual(m.Producers, []string{"literal", "driver-text-transport", "stored-column"}) {
		return fmt.Errorf("required axes changed")
	}
	catalog := make(map[string]bool)
	for _, name := range m.Catalog {
		catalog[name] = true
	}
	for _, fn := range m.Functions {
		if !catalog[fn] {
			return fmt.Errorf("required function %s is not registered", fn)
		}
	}
	wantInputs := []numericInput{{"negative-zero", "8000000000000000", "-0.0"}, {"positive-zero", "0000000000000000", "0.0"}, {"negative-quarter", "bfd0000000000000", "-0.25"}, {"positive-quarter", "3fd0000000000000", "0.25"}, {"whole-three", "4008000000000000", "3.0"}}
	if !reflect.DeepEqual(m.Inputs, wantInputs) {
		return fmt.Errorf("required input classes changed")
	}
	ops := []string{"FLOOR", "CEIL", "ROUND", "POWER"}
	if len(m.Answers) != len(ops) {
		return fmt.Errorf("oracle operator population changed")
	}
	for i, op := range ops {
		answers := m.Answers[op]
		if len(answers) != len(m.Inputs) {
			return fmt.Errorf("missing answers for %s", op)
		}
		for _, bits := range answers {
			if _, err := tagged("float64", bits).decode(); err != nil {
				return err
			}
		}
		for _, other := range ops[:i] {
			if reflect.DeepEqual(answers, m.Answers[other]) {
				return fmt.Errorf("indistinguishable operators %s and %s", op, other)
			}
		}
	}
	return nil
}

func (m *numericManifest) scenario() *Scenario {
	s := &Scenario{Name: "numeric-v1", SchemaTemplate: "CREATE TABLE numbers (id BIGINT, x DOUBLE, PRIMARY KEY (id))"}
	for i, input := range m.Inputs {
		s.Setup = append(s.Setup, fmt.Sprintf("INSERT INTO numbers VALUES (%d, %s)", i+1, input.Literal))
	}
	for _, producer := range m.Producers {
		for i, input := range m.Inputs {
			expression := input.Literal
			var args []Scalar
			switch producer {
			case "driver-text-transport":
				expression = "?"
				args = []Scalar{tagged("float64", input.Bits)}
			case "stored-column":
				expression = "x"
			}
			suffix := fmt.Sprintf(" FROM numbers WHERE id = %d", i+1)
			rows := [][]Scalar{{tagged("float64", input.Bits)}}
			s.Tests = append(s.Tests, Test{ID: "producer/" + producer + "/" + input.Name, Query: "SELECT " + expression + suffix, Args: args, ExactRows: &rows, ColumnTypes: []string{"DOUBLE"}})
			for _, fn := range m.Functions {
				op := fn
				switch fn {
				case "CEILING":
					op = "CEIL"
				case "POW":
					op = "POWER"
				}
				fnArg := expression
				if op == "POWER" {
					fnArg += ", 3"
				}
				fnArgs := append([]Scalar(nil), args...)
				fnArgs = append(fnArgs, args...)
				expected := [][]Scalar{{tagged("float64", input.Bits), tagged("float64", m.Answers[op][i])}}
				s.Tests = append(s.Tests, Test{ID: fn + "/" + producer + "/" + input.Name, Query: "SELECT " + expression + ", " + fn + "(" + fnArg + ")" + suffix, Args: fnArgs, ExactRows: &expected, ColumnTypes: []string{"DOUBLE", "DOUBLE"}})
			}
		}
	}
	return s
}

// NumericScenarioForTest returns the strict-loaded committed scenario only after
// checking it against the finite manifest's deterministic expansion.
func NumericScenarioForTest(t *testing.T) *Scenario {
	t.Helper()
	m, err := loadNumericManifest()
	if err != nil {
		t.Fatal(err)
	}
	expected := m.scenario()
	data, err := yaml.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	matches, err := os.ReadDir("testdata/semantic")
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	for _, entry := range matches {
		if strings.HasSuffix(entry.Name(), ".yaml") {
			files++
		}
	}
	if files != 1 {
		t.Fatalf("expected 1 generated YAML file, got %d", files)
	}
	committed, err := os.ReadFile(numericYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(committed, data) {
		t.Fatal("numeric-v1.yaml is stale; regenerate from numericManifest.scenario")
	}
	s, err := Load(numericYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	a, err := digestScenario(expected)
	if err != nil {
		t.Fatal(err)
	}
	b, err := digestScenario(s)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("strict YAML round trip changed scenario")
	}
	return s
}

// CheckNumericResultForTest checks live, private outcomes, never aggregate counts.
type (
	numericCellStatus struct{ ID, Status string }
	numericReport     struct {
		Required, Exercised, Validated, Prerequisites int
		Cells                                         []numericCellStatus
		Unclassified                                  []string
	}
)

func CheckNumericResultForTest(t *testing.T, s *Scenario, r *Result) {
	t.Helper()
	m, err := loadNumericManifest()
	if err != nil {
		t.Fatal(err)
	}
	report, err := numericCoverage(m, s, r)
	t.Logf("numeric-v1: required=%d exercised=%d validated=%d prerequisites=%d catalog=%d unclassified=%d", report.Required, report.Exercised, report.Validated, report.Prerequisites, len(m.Catalog), len(report.Unclassified))
	t.Logf("unclassified scalar registrations: %v", report.Unclassified)
	for _, cell := range report.Cells {
		t.Logf("cell %s: %s", cell.ID, cell.Status)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func numericCoverage(m *numericManifest, s *Scenario, r *Result) (numericReport, error) {
	var report numericReport
	if err := m.validate(); err != nil {
		return report, err
	}
	expected := m.scenario()
	required := make(map[string]bool)
	for _, test := range expected.Tests {
		required[test.ID] = true
		report.Cells = append(report.Cells, numericCellStatus{test.ID, "not-exercised"})
		if strings.HasPrefix(test.ID, "producer/") {
			report.Prerequisites++
		} else {
			report.Required++
		}
	}
	classified := make(map[string]bool)
	for _, fn := range m.Functions {
		classified[fn] = true
	}
	for _, name := range m.Catalog {
		if !classified[name] {
			report.Unclassified = append(report.Unclassified, name)
		}
	}
	if s == nil {
		return report, fmt.Errorf("missing executed scenario")
	}
	seen := make(map[string]bool)
	for _, test := range s.Tests {
		if !required[test.ID] || seen[test.ID] {
			return report, fmt.Errorf("unknown or duplicate cell %s", test.ID)
		}
		seen[test.ID] = true
		parts := strings.Split(test.ID, "/")
		var bits string
		for _, input := range m.Inputs {
			if input.Name == parts[2] {
				bits = input.Bits
			}
		}
		if err := witnessNumericProducer(test, parts[1], bits); err != nil {
			return report, fmt.Errorf("%s: %w", test.ID, err)
		}
	}
	if len(seen) != len(required) {
		return report, fmt.Errorf("missing cells")
	}
	gotDigest, err := digestScenario(s)
	if err != nil {
		return report, err
	}
	expectedDigest, err := digestScenario(expected)
	if err != nil {
		return report, err
	}
	if gotDigest != expectedDigest {
		return report, fmt.Errorf("executed input does not match generated envelope")
	}
	outcomes, err := r.successfulStatements(expected)
	if err != nil {
		return report, err
	}
	passed := make(map[string]bool)
	for i, test := range expected.Tests {
		passed[test.ID] = outcomes[i]
	}
	var missing []string
	for i, test := range expected.Tests {
		cell := &report.Cells[i]
		cell.Status = "exercised-failed"
		if passed[test.ID] {
			cell.Status = "validated"
		}
		if strings.HasPrefix(test.ID, "producer/") {
			continue
		}
		report.Exercised++
		parts := strings.Split(test.ID, "/")
		if passed[test.ID] && !passed["producer/"+parts[1]+"/"+parts[2]] {
			cell.Status = "missing-prerequisite"
		}
		if cell.Status == "validated" {
			report.Validated++
		} else {
			missing = append(missing, test.ID)
		}
	}
	if report.Validated != report.Required {
		return report, fmt.Errorf("not validated: %v", missing)
	}
	return report, nil
}

func TestNumericCoverageEvidence(t *testing.T) {
	t.Parallel()
	m, err := loadNumericManifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*Scenario, *Result)
		want   int
	}{
		{"complete", func(_ *Scenario, _ *Result) {}, 90},
		{"failed cell", func(_ *Scenario, r *Result) { r.outcomes[1] = false }, 89},
		{"failed prerequisite", func(_ *Scenario, r *Result) { r.outcomes[0] = false }, 84},
		{"short outcomes", func(_ *Scenario, r *Result) { r.outcomes = r.outcomes[1:] }, 0},
		{"missing success with clean failures", func(_ *Scenario, r *Result) { r.outcomes = nil; r.TestsPass = 105 }, 0},
		{"changed assertions", func(s *Scenario, _ *Result) { s.Tests[1].ColumnTypes = nil }, 0},
		{"appended duplicate", func(s *Scenario, _ *Result) { s.Tests = append(s.Tests, s.Tests[0]) }, 0},
		{"removed cell", func(s *Scenario, _ *Result) { s.Tests = s.Tests[:104] }, 0},
		{"unknown ID", func(s *Scenario, _ *Result) { s.Tests[0].ID = "unknown/literal/negative-zero" }, 0},
		{"wrong witness", func(s *Scenario, _ *Result) { s.Tests[1].Query = "SELECT x, CEIL(x) FROM numbers WHERE id = 1" }, 0},
		{"changed ID", func(s *Scenario, _ *Result) { s.Tests[1].ID = s.Tests[2].ID }, 0},
		{"changed digest", func(_ *Scenario, r *Result) { r.scenarioDigest[0] ^= 1 }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := m.scenario()
			digest, err := digestScenario(s)
			if err != nil {
				t.Fatal(err)
			}
			r := &Result{scenarioDigest: digest, outcomes: make([]bool, 105)}
			for i := range r.outcomes {
				r.outcomes[i] = true
			}
			tc.change(s, r)
			report, err := numericCoverage(m, s, r)
			validated := report.Validated
			counted := 0
			for _, cell := range report.Cells {
				if !strings.HasPrefix(cell.ID, "producer/") && cell.Status == "validated" {
					counted++
				}
			}
			if counted != validated {
				t.Fatalf("report disagrees with verdict: %+v", report)
			}
			if validated != tc.want || (err == nil) != (tc.want == 90) {
				t.Fatalf("validated=%d err=%v", validated, err)
			}
		})
	}
}

func TestNumericManifestGuard(t *testing.T) {
	t.Parallel()
	m, err := loadNumericManifest()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	changes := []struct {
		name   string
		mutate func(*numericManifest)
	}{
		{"version", func(m *numericManifest) { m.Version = 0 }},
		{"catalog empty", func(m *numericManifest) { m.Catalog = nil }},
		{"catalog duplicate", func(m *numericManifest) { m.Catalog[0] = m.Catalog[1] }},
		{"function removal", func(m *numericManifest) { m.Functions = m.Functions[1:] }},
		{"producer removal", func(m *numericManifest) { m.Producers = m.Producers[1:] }},
		{"input removal", func(m *numericManifest) { m.Inputs = m.Inputs[1:] }},
		{"oracle absent", func(m *numericManifest) { delete(m.Answers, "ROUND") }},
		{"oracle short", func(m *numericManifest) { m.Answers["ROUND"] = nil }},
		{"oracle malformed", func(m *numericManifest) { m.Answers["ROUND"][0] = "x" }},
		{"operator collision", func(m *numericManifest) { m.Answers["ROUND"] = m.Answers["CEIL"] }},
		{"scope absent", func(m *numericManifest) { m.Outside = nil }},
	}
	for _, tc := range changes {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var copy numericManifest
			if err := json.Unmarshal(data, &copy); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&copy)
			if err := copy.validate(); err == nil {
				t.Fatal("accepted broken envelope")
			}
		})
	}
}

var updateNumericEnvelope = flag.Bool("update-numeric-envelope", false, "regenerate numeric-v1.yaml from the reviewed manifest")

func TestNumericEnvelopeArtifact(t *testing.T) {
	t.Parallel()
	if *updateNumericEnvelope {
		m, err := loadNumericManifest()
		if err != nil {
			t.Fatal(err)
		}
		data, err := yaml.Marshal(m.scenario())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(numericYAMLPath, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := NumericScenarioForTest(t)
	if len(s.Tests) != 105 {
		t.Fatalf("expected 105 statements, got %d", len(s.Tests))
	}
	var ids []string
	for _, test := range s.Tests {
		if !strings.HasPrefix(test.ID, "producer/") {
			ids = append(ids, test.ID)
		}
	}
	sort.Strings(ids)
	want := strings.Fields(requiredNumericIDs)
	sort.Strings(want)
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("required 90-cell denominator changed: got %v", ids)
	}
}

// This literal denominator is independent of the generator's axis iteration.
const requiredNumericIDs = `
FLOOR/literal/negative-zero
FLOOR/literal/positive-zero
FLOOR/literal/negative-quarter
FLOOR/literal/positive-quarter
FLOOR/literal/whole-three
FLOOR/driver-text-transport/negative-zero
FLOOR/driver-text-transport/positive-zero
FLOOR/driver-text-transport/negative-quarter
FLOOR/driver-text-transport/positive-quarter
FLOOR/driver-text-transport/whole-three
FLOOR/stored-column/negative-zero
FLOOR/stored-column/positive-zero
FLOOR/stored-column/negative-quarter
FLOOR/stored-column/positive-quarter
FLOOR/stored-column/whole-three
CEIL/literal/negative-zero
CEIL/literal/positive-zero
CEIL/literal/negative-quarter
CEIL/literal/positive-quarter
CEIL/literal/whole-three
CEIL/driver-text-transport/negative-zero
CEIL/driver-text-transport/positive-zero
CEIL/driver-text-transport/negative-quarter
CEIL/driver-text-transport/positive-quarter
CEIL/driver-text-transport/whole-three
CEIL/stored-column/negative-zero
CEIL/stored-column/positive-zero
CEIL/stored-column/negative-quarter
CEIL/stored-column/positive-quarter
CEIL/stored-column/whole-three
CEILING/literal/negative-zero
CEILING/literal/positive-zero
CEILING/literal/negative-quarter
CEILING/literal/positive-quarter
CEILING/literal/whole-three
CEILING/driver-text-transport/negative-zero
CEILING/driver-text-transport/positive-zero
CEILING/driver-text-transport/negative-quarter
CEILING/driver-text-transport/positive-quarter
CEILING/driver-text-transport/whole-three
CEILING/stored-column/negative-zero
CEILING/stored-column/positive-zero
CEILING/stored-column/negative-quarter
CEILING/stored-column/positive-quarter
CEILING/stored-column/whole-three
ROUND/literal/negative-zero
ROUND/literal/positive-zero
ROUND/literal/negative-quarter
ROUND/literal/positive-quarter
ROUND/literal/whole-three
ROUND/driver-text-transport/negative-zero
ROUND/driver-text-transport/positive-zero
ROUND/driver-text-transport/negative-quarter
ROUND/driver-text-transport/positive-quarter
ROUND/driver-text-transport/whole-three
ROUND/stored-column/negative-zero
ROUND/stored-column/positive-zero
ROUND/stored-column/negative-quarter
ROUND/stored-column/positive-quarter
ROUND/stored-column/whole-three
POWER/literal/negative-zero
POWER/literal/positive-zero
POWER/literal/negative-quarter
POWER/literal/positive-quarter
POWER/literal/whole-three
POWER/driver-text-transport/negative-zero
POWER/driver-text-transport/positive-zero
POWER/driver-text-transport/negative-quarter
POWER/driver-text-transport/positive-quarter
POWER/driver-text-transport/whole-three
POWER/stored-column/negative-zero
POWER/stored-column/positive-zero
POWER/stored-column/negative-quarter
POWER/stored-column/positive-quarter
POWER/stored-column/whole-three
POW/literal/negative-zero
POW/literal/positive-zero
POW/literal/negative-quarter
POW/literal/positive-quarter
POW/literal/whole-three
POW/driver-text-transport/negative-zero
POW/driver-text-transport/positive-zero
POW/driver-text-transport/negative-quarter
POW/driver-text-transport/positive-quarter
POW/driver-text-transport/whole-three
POW/stored-column/negative-zero
POW/stored-column/positive-zero
POW/stored-column/negative-quarter
POW/stored-column/positive-quarter
POW/stored-column/whole-three
`

func TestNumericRequiredCatalogMembership(t *testing.T) {
	t.Parallel()
	m, err := loadNumericManifest()
	if err != nil {
		t.Fatal(err)
	}
	var removed []string
	for _, name := range m.Catalog {
		if name != "CEILING" {
			removed = append(removed, name)
		}
	}
	m.Catalog = removed
	if err := m.validateCatalog(removed); err == nil || !strings.Contains(err.Error(), "CEILING is not registered") {
		t.Fatalf("missing required function accepted: %v", err)
	}
}
