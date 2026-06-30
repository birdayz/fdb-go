// Package yamsql is a SQL-level conformance harness for the Go SQL driver.
//
// Each scenario is a YAML file describing a schema template, optional
// setup statements, and a list of queries with expected result rows.
// The runner executes the scenario against sql.Open("fdbsql", ...) and
// diffs actual results against expected rows — any mismatch is a
// correctness regression.
//
// The format is a strict subset of Java's yamsql (fdb-record-layer
// yaml-tests) but uses YAML sequences `[v1, v2]` for rows instead of
// yamsql's `{v1, v2}` flow-map shorthand, so it round-trips through
// standard YAML parsers without custom constructors.
package yamsql

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Scenario is one parsed conformance scenario.
type Scenario struct {
	// Name is the scenario identifier (defaults to the file basename).
	Name string `yaml:"name"`
	// SchemaTemplate is the body of CREATE SCHEMA TEMPLATE — a sequence
	// of DDL statements that define the schema under test. Required.
	SchemaTemplate string `yaml:"schema_template"`
	// Setup is a list of statements executed after schema creation,
	// before any test query. Typically INSERT seeds.
	Setup []string `yaml:"setup"`
	// Tests are the scenario's assertions.
	Tests []Test `yaml:"tests"`
}

// Test is one (query, expected) pair.
type Test struct {
	// Query is the SQL to execute.
	Query string `yaml:"query"`
	// Rows is the expected ordered result set. Each inner sequence is
	// one row of column values. Use nil/~ for SQL NULL.
	Rows [][]any `yaml:"rows"`
	// Unordered, if true, treats Rows as a multiset — rows may be
	// returned in any order.
	Unordered bool `yaml:"unordered"`
	// ErrorCode, if set, asserts that the query fails with an api.Error
	// whose Code matches. When non-empty, Rows is ignored.
	ErrorCode string `yaml:"error_code"`
	// Error is the legacy alias for ErrorCode used by the Java-derived
	// `*_java.yaml` scenarios (`error: "<SQLSTATE>"`). Same SQLSTATE format;
	// EffectiveErrorCode unifies the two so the runner and the coverage
	// classifier treat both key forms identically. Without this, an `error:`
	// pin leaves ErrorCode empty and is mis-classified as a positive result.
	Error string `yaml:"error"`
	// PlanContains, if set, runs EXPLAIN on the query and asserts that
	// the plan output contains this substring. Useful for verifying
	// index scan, covering, sort elimination, etc.
	PlanContains string `yaml:"plan_contains"`
	// PlanNotContains, if set, asserts the plan does NOT contain this
	// substring. Useful for negative assertions (no InMemorySort, no Fetch).
	PlanNotContains string `yaml:"plan_not_contains"`
	// Ansi lists the SQL:2023 Core feature IDs this specific test provides
	// POSITIVE evidence for (RFC-165 Ledger A). The ANSI ledger credits each ID
	// only when THIS test passes (classifyTest == OutcomeSupported), binding the
	// evidence to the exact test that exercises the feature — so a positive tag
	// can never be credited off a sibling test in the same file (the F261-01
	// "Simple CASE credited off searched-CASE" class the audit caught).
	Ansi []string `yaml:"ansi"`
	// AnsiGap lists feature IDs this test pins as UNSUPPORTED (an explicit
	// rejection). Credited only when THIS test is an unsupported-feature pin
	// (classifyTest == OutcomeUnsupported).
	AnsiGap []string `yaml:"ansi_gap"`
}

// EffectiveErrorCode returns the asserted error SQLSTATE, preferring the modern
// `error_code:` key and falling back to the legacy Java `error:` alias. Both the
// runner and the coverage classifier MUST go through this so an `error:` pin is
// never silently treated as a passing result.
func (t Test) EffectiveErrorCode() string {
	if t.ErrorCode != "" {
		return t.ErrorCode
	}
	return t.Error
}

// Load parses a scenario from a YAML file.
func Load(path string) (*Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var s Scenario
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if s.SchemaTemplate == "" {
		return nil, fmt.Errorf("%s: schema_template is required", path)
	}
	if s.Name == "" {
		s.Name = strings.TrimSuffix(filepath.Base(path), ".yaml")
	}
	return &s, nil
}
