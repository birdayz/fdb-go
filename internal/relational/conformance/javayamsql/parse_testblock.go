// Portions derived from FoundationDB Record Layer (YamlExecutionContext.java,
// QueryCommand.java),
// Copyright 2021-2024 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package javayamsql

import (
	"errors"
	"fmt"
	"strings"
)

func (p *parser) parseTestBlock(v *Value) (*TestBlock, []InertDirective, error) {
	m, inert, err := mapping(v, "test_block", "name", "connect", "preset", "options", "tests")
	if err != nil {
		return nil, nil, err
	}
	out := &TestBlock{Connect: m["connect"]}
	if n, ok := m["name"]; ok {
		if out.Name, ok = n.AsString(); !ok {
			return nil, nil, &ParseError{Line: n.Line, Err: fmt.Errorf("test_block name must be a string, got %s", kindOf(n))}
		}
	}
	if p, ok := m["preset"]; ok {
		s, isStr := p.AsString()
		if !isStr {
			return nil, nil, &ParseError{Line: p.Line, Err: fmt.Errorf("preset must be a string, got %s", kindOf(p))}
		}
		if !validPresets[s] {
			return nil, nil, &ParseError{Line: p.Line, Err: fmt.Errorf("unknown value for preset: %s", s)}
		}
		out.Preset = s
	}
	if o, ok := m["options"]; ok {
		var oi []InertDirective
		if out.Options, oi, err = parseTestBlockOptions(o); err != nil {
			return nil, nil, err
		}
		inert = append(inert, oi...)
	}
	tests, ok := m["tests"]
	if !ok || tests.IsNull() {
		return nil, nil, &ParseError{Line: lineOf(v), Err: errors.New("tests not found in test_block")}
	}
	if tests.Kind != KindSeq {
		return nil, nil, &ParseError{Line: tests.Line, Err: fmt.Errorf("tests must be a list, got %s", tests.Kind)}
	}
	for _, t := range tests.Seq {
		// Each test is a list whose head is the command and whose tail are the
		// configs (QueryCommand.parse: `arrayList(object).stream().skip(1)`).
		if t.Kind != KindSeq || len(t.Seq) == 0 {
			return nil, nil, &ParseError{Line: t.Line, Err: errors.New("a test must be a non-empty list starting with a command")}
		}
		cmd, err := p.parseCommand(t.Seq[0], t.Seq[1:])
		if err != nil {
			return nil, nil, err
		}
		out.Tests = append(out.Tests, &Test{Line: t.Line, Command: cmd})
	}
	return out, inert, nil
}

var validPresets = map[string]bool{
	"single_repetition_ordered": true, "single_repetition_randomized": true,
	"single_repetition_parallelized": true, "multi_repetition_ordered": true,
	"multi_repetition_randomized": true, "multi_repetition_parallelized": true,
}

var validModes = map[string]bool{"ordered": true, "randomized": true, "parallelized": true}

func parseTestBlockOptions(v *Value) (TestBlockOptions, []InertDirective, error) {
	var out TestBlockOptions
	m, inert, err := mapping(v, "test_block options",
		"mode", "repetition", "seed", "check_cache", "connection_lifecycle",
		"statement_type", "connection_options", "supported_version")
	if err != nil {
		return out, nil, err
	}
	if x, ok := m["mode"]; ok {
		s, isStr := x.AsString()
		if !isStr || !validModes[s] {
			return out, nil, &ParseError{Line: x.Line, Err: fmt.Errorf("unknown value for option mode: %s", kindOf(x))}
		}
		out.Mode = s
	}
	if x, ok := m["repetition"]; ok {
		n, isInt := x.AsInt()
		if !isInt {
			return out, nil, &ParseError{Line: x.Line, Err: fmt.Errorf("repetition must be an integer, got %s", kindOf(x))}
		}
		if n <= 0 {
			return out, nil, &ParseError{Line: x.Line, Err: fmt.Errorf("illegal repetition value %d, should be >= 1", n)}
		}
		out.Repetition = &n
	}
	if x, ok := m["seed"]; ok {
		n, isInt := x.AsInt()
		if !isInt {
			return out, nil, &ParseError{Line: x.Line, Err: fmt.Errorf("seed must be an integer, got %s", kindOf(x))}
		}
		out.Seed = &n
	}
	if x, ok := m["check_cache"]; ok {
		u := x.Untag()
		if u == nil || u.Kind != KindBool {
			return out, nil, &ParseError{Line: x.Line, Err: fmt.Errorf("check_cache must be a boolean, got %s", kindOf(x))}
		}
		b := u.Bool
		out.CheckCache = &b
	}
	if x, ok := m["connection_lifecycle"]; ok {
		s, isStr := x.AsString()
		if !isStr || (s != "test" && s != "block") {
			return out, nil, &ParseError{Line: x.Line, Err: fmt.Errorf("unknown value for option connection_lifecycle: %s", kindOf(x))}
		}
		out.ConnectionLifecycle = s
	}
	if x, ok := m["statement_type"]; ok {
		s, isStr := x.AsString()
		if !isStr || (s != "simple" && s != "prepared") {
			return out, nil, &ParseError{Line: x.Line, Err: fmt.Errorf("unknown value for option statement_type: %s", kindOf(x))}
		}
		out.StatementType = s
	}
	if x, ok := m["supported_version"]; ok {
		if out.SupportedVersion, err = parseVersion(x); err != nil {
			return out, nil, err
		}
	}
	if x, ok := m["connection_options"]; ok {
		if out.ConnectionOptions, err = connectionOptions(x); err != nil {
			return out, nil, err
		}
	}
	return out, inert, nil
}

// parseCommand mirrors Command.parse. head is the single-entry map naming the
// command; rest are the config entries that follow it in the test list.
func (p *parser) parseCommand(head *Value, rest []*Value) (*Command, error) {
	if head == nil || head.Kind != KindMap || len(head.Map) == 0 {
		return nil, &ParseError{Line: lineOf(head), Err: errors.New("a command must be a map")}
	}
	// Java reads firstEntry, so a command map with extra keys silently drops
	// them; reject instead, since that is exactly the drift this gate is for.
	if len(head.Map) != 1 {
		return nil, &ParseError{Line: head.Line, Err: fmt.Errorf("a command must be a map of size 1, got %d keys", len(head.Map))}
	}
	e := head.Map[0]
	key, ok := e.Key.AsString()
	if !ok {
		return nil, &ParseError{Line: e.Key.Line, Err: errors.New("command key must be a string")}
	}

	cmd := &Command{Kind: CommandKind(key), Line: e.Key.Line}
	switch CommandKind(key) {
	case CommandQuery:
		q, isStr := e.Val.AsString()
		if !isStr {
			return nil, &ParseError{Line: e.Val.Line, Err: fmt.Errorf("query must be a string, got %s", kindOf(e.Val))}
		}
		cmd.Query = q
		segs, err := ParseSegments(q, e.Val.Line)
		if err != nil {
			return nil, err
		}
		cmd.Segments = segs
		if cmd.Configs, err = p.parseConfigs(rest); err != nil {
			return nil, err
		}
	case CommandLoadSchemaTemplate, CommandSetSchemaState:
		p, isStr := e.Val.AsString()
		if !isStr {
			return nil, &ParseError{Line: e.Val.Line, Err: fmt.Errorf("%s payload must be a string, got %s", key, kindOf(e.Val))}
		}
		cmd.Payload = p
		if len(rest) > 0 {
			return nil, &ParseError{Line: rest[0].Line, Err: fmt.Errorf("%s takes no query configs", key)}
		}
	default:
		return nil, &ParseError{Line: cmd.Line, Err: fmt.Errorf("could not find command %q", key)}
	}
	return cmd, nil
}

// parseConfigs mirrors QueryConfig.parseConfigs plus validateConfigs.
func (p *parser) parseConfigs(items []*Value) ([]*Config, error) {
	var out []*Config
	// Once a result-ish config is seen, only result-ish configs may follow.
	// resultMetadata is exempt from *arming* the rule but is itself allowed
	// after it, which is why it is in RESULT_CONFIGS yet excluded here.
	requireResults := false

	for _, item := range items {
		if item == nil || item.Kind != KindMap || len(item.Map) == 0 {
			return nil, &ParseError{Line: lineOf(item), Err: errors.New("a query config must be a map")}
		}
		e := item.Map[0]
		key, ok := e.Key.AsString()
		if !ok {
			return nil, &ParseError{Line: e.Key.Line, Err: errors.New("query config key must be a string")}
		}
		kind := ConfigKind(key)
		if !configKinds[kind] {
			return nil, &ParseError{Line: e.Key.Line, Err: fmt.Errorf("%q is not a valid configuration", key)}
		}
		resultOrVersion := kind.IsResultConfig() || kind.IsVersionDependentConfig()
		if requireResults && !resultOrVersion {
			return nil, &ParseError{Line: e.Key.Line, Err: errors.New("only result configurations can follow first result or version specification config")}
		}
		if resultOrVersion && kind != ConfigResultMetadata {
			requireResults = true
		}
		cfg, err := p.parseConfig(kind, e.Key.Line, e.Val)
		if err != nil {
			return nil, err
		}
		out = append(out, cfg)
	}

	if err := validateConfigs(out); err != nil {
		return nil, err
	}
	return out, nil
}

var configKinds = map[ConfigKind]bool{
	ConfigResult: true, ConfigUnorderedResult: true, ConfigExplain: true,
	ConfigExplainContains: true, ConfigCount: true, ConfigError: true,
	ConfigPlanHash: true, ConfigMaxRows: true, ConfigSupportedVersion: true,
	ConfigInitialVersionAtLeast: true, ConfigInitialVersionLessThan: true,
	ConfigSetup: true, ConfigSetupReference: true, ConfigDebugger: true,
	ConfigResultMetadata: true,
}

// validDebuggers is DebuggerImplementation's enum, which QueryConfig resolves
// with valueOf(value.toUpperCase()).
//
// QueryConfig's own javadoc advertises "SANE, VERBOSE, QUIET". That comment is
// stale — no such constants exist — so the enum is the authority here.
var validDebuggers = map[string]bool{
	"insane": true, "sane": true, "recording": true, "repl": true,
}

func (p *parser) parseConfig(kind ConfigKind, line int, val *Value) (*Config, error) {
	if val == nil {
		val = &Value{Kind: KindNull, Line: line}
	}
	c := &Config{Kind: kind, Line: line, Raw: val}
	var err error

	switch kind {
	case ConfigResult, ConfigUnorderedResult:
		if c.Rows, err = parseRows(val); err != nil {
			return nil, err
		}
	case ConfigCount, ConfigPlanHash, ConfigMaxRows:
		n, ok := val.AsInt()
		if !ok {
			return nil, &ParseError{Line: val.Line, Err: fmt.Errorf("%s must be an integer, got %s", kind, kindOf(val))}
		}
		c.Number = &n
	case ConfigError:
		s, ok := val.AsString()
		if !ok {
			return nil, &ParseError{Line: val.Line, Err: fmt.Errorf("error must be a SQLSTATE or ErrorCode name, got %s", kindOf(val))}
		}
		code, ok := resolveErrorCode(s)
		if !ok {
			return nil, &ParseError{Line: val.Line, Err: fmt.Errorf("%q is not a valid ErrorCode enum name or a 5-character SQLSTATE code", s)}
		}
		c.ErrorCode = code
	case ConfigExplain, ConfigExplainContains, ConfigSetup:
		s, ok := val.AsString()
		if !ok {
			return nil, &ParseError{Line: val.Line, Err: fmt.Errorf("%s must be a string, got %s", kind, kindOf(val))}
		}
		c.Text = s
	case ConfigSetupReference:
		name, ok := val.AsString()
		if !ok {
			return nil, &ParseError{Line: val.Line, Err: fmt.Errorf("setupReference must be a string, got %s", kindOf(val))}
		}
		sql, ok := p.transactionSetup(name)
		if !ok {
			return nil, &ParseError{Line: val.Line, Err: fmt.Errorf("transaction setup %s is not defined", name)}
		}
		// Java stores the resolved SQL on the config, not the name, so the
		// reference and the inline form become indistinguishable downstream.
		c.Text = sql
	case ConfigDebugger:
		s, ok := val.AsString()
		if !ok || !validDebuggers[strings.ToLower(s)] {
			return nil, &ParseError{Line: val.Line, Err: fmt.Errorf("unknown debugger implementation %s", kindOf(val))}
		}
		c.Text = s
	case ConfigSupportedVersion, ConfigInitialVersionAtLeast, ConfigInitialVersionLessThan:
		if c.Version, err = parseVersion(val); err != nil {
			return nil, err
		}
	case ConfigResultMetadata:
		// The metadata descriptor is an arbitrarily nested list/map of column
		// name to type; there is no closed key set to check it against, so it
		// stays raw.
	}
	return c, nil
}

// parseRows turns a result config's value into rows.
//
// A null value (`result:`) and an `!ignore` value are both legal and are not
// row lists — Matchers.matchResultSet handles each before it reaches the
// list branch — so both yield no rows rather than an error.
func parseRows(val *Value) ([]Row, error) {
	if val.IsNull() || val.TagName() == TagIgnore {
		return nil, nil
	}
	if val.Kind != KindSeq {
		return nil, &ParseError{Line: val.Line, Err: fmt.Errorf("unknown format of expected result set: %s", kindOf(val))}
	}
	rows := make([]Row, 0, len(val.Seq))
	for _, rv := range val.Seq {
		if rv.Kind != KindMap {
			return nil, &ParseError{Line: rv.Line, Err: fmt.Errorf("unknown format of expected result set: row is %s", kindOf(rv))}
		}
		row := Row{Line: rv.Line, Cells: make([]Cell, 0, len(rv.Map))}
		for _, e := range rv.Map {
			// Matchers.valueElseKey rejects a `null: null` entry outright: with
			// both sides absent there is nothing left to expect, and the user
			// almost certainly meant the !null matcher.
			if e.Key.IsNull() && e.Val.IsNull() {
				return nil, &ParseError{Line: e.Key.Line, Err: errors.New("encountered YAML-style 'null' which is not supported, consider using '!null' instead")}
			}
			cell := Cell{Line: e.Key.Line, Key: e.Key, Val: e.Val}
			if e.Key.TagName() == TagPos {
				n, _ := e.Key.AsInt() // shape already enforced by validateTag
				cell.Pos = &n
			}
			row.Cells = append(row.Cells, cell)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// validateConfigs mirrors QueryConfig.validateConfigs.
//
// Note what is deliberately absent: Java's "ERROR config should be the last
// config specified" is an assertion inside QueryCommand.executeInternal, not a
// parse-time rule, so enforcing it here would reject files Java loads happily.
func validateConfigs(configs []*Config) error {
	for i, c := range configs {
		if i > 0 && c.Kind == ConfigSupportedVersion {
			return &ParseError{Line: c.Line, Err: errors.New("supported_version must be the first config in a query (after the query itself)")}
		}
	}
	hasMetadata, hasConsumer := false, false
	for _, c := range configs {
		if c.Kind == ConfigResultMetadata {
			hasMetadata = true
		}
		if c.Kind.IsResultConsumingConfig() {
			hasConsumer = true
		}
	}
	if hasMetadata && !hasConsumer {
		return &ParseError{
			Line: configs[0].Line,
			Err:  errors.New("resultMetadata requires at least one of result, unorderedResult, error, or count to be present"),
		}
	}
	return validateVersionCoverage(configs)
}
