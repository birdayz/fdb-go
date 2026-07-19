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
	"bytes"
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
	// Exec is an authoring alias for Query for non-query DML steps
	// (`- exec: UPDATE …`). Load normalizes it into Query, so every
	// downstream consumer reads Query only. Exactly one of query/exec may
	// be set. HISTORY: this field did not exist for the corpus's first
	// months — the YAML unmarshaller silently DROPPED `exec:` keys, so
	// every such stanza ran as an EMPTY statement that "succeeded" without
	// executing any DML, and the scenario's post-DML expectations verified
	// nothing. That silent hole is why Load now rejects unknown fields.
	Exec string `yaml:"exec"`
	// Rowcount, if set on a non-query statement, asserts the driver-reported
	// affected-row count. Pointer so an explicit `rowcount: 0` is
	// distinguishable from absent. (Silently ignored before the same fix.)
	Rowcount *int64 `yaml:"rowcount"`
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
	// Columns asserts the result's COLUMN NAMES, in order. Without it a
	// scenario proves only the values, so a query that returns the right
	// rows under the wrong column names passes — the exact shape of the
	// scalar-aggregate and LIMIT-through-projection alias bugs, one of
	// which was a live Go-vs-Java divergence (RFC-082 select_count_alias).
	// Comparison is case-insensitive: the engines agree on identity, not on
	// display case.
	Columns []string `yaml:"columns"`

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
//
// Decoding is STRICT: an unknown field is a load error, never a silent no-op.
// The corpus shipped for months with `exec:`/`rowcount:` stanzas the struct
// didn't have — the unmarshaller dropped the keys, the "statements" ran empty,
// and the scenarios' post-DML assertions verified nothing while green.
func Load(path string) (*Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var s Scenario
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if s.SchemaTemplate == "" {
		return nil, fmt.Errorf("%s: schema_template is required", path)
	}
	if s.Name == "" {
		s.Name = strings.TrimSuffix(filepath.Base(path), ".yaml")
	}
	for i := range s.Tests {
		t := &s.Tests[i]
		switch {
		case t.Query != "" && t.Exec != "":
			return nil, fmt.Errorf("%s: tests[%d]: query and exec are mutually exclusive", path, i)
		case t.Query == "" && t.Exec == "":
			return nil, fmt.Errorf("%s: tests[%d]: one of query/exec is required", path, i)
		case t.Exec != "":
			if IsQuery(t.Exec) {
				return nil, fmt.Errorf("%s: tests[%d]: exec is for non-query statements; use query: for %q", path, i, t.Exec)
			}
			if len(t.Rows) != 0 {
				return nil, fmt.Errorf("%s: tests[%d]: exec cannot expect rows", path, i)
			}
			t.Query, t.Exec = t.Exec, ""
		}
		if t.Rowcount != nil && IsQuery(t.Query) {
			return nil, fmt.Errorf("%s: tests[%d]: rowcount is only valid on non-query statements", path, i)
		}
		if t.Rowcount != nil && t.EffectiveErrorCode() != "" {
			return nil, fmt.Errorf("%s: tests[%d]: rowcount and error assertions are mutually exclusive", path, i)
		}
		// Assertions the runner can only honor on the Query path must not
		// ride on a DML statement — the non-query branch returns before the
		// plan/row diffing, so they would be silently ignored (the exact
		// dropped-assertion class this loader exists to kill).
		if !IsQuery(t.Query) {
			if t.PlanContains != "" || t.PlanNotContains != "" {
				return nil, fmt.Errorf("%s: tests[%d]: plan assertions are only valid on queries", path, i)
			}
			if t.Unordered {
				return nil, fmt.Errorf("%s: tests[%d]: unordered is only valid on queries", path, i)
			}
		}
	}
	return &s, nil
}
