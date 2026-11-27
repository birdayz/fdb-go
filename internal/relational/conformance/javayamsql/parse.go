// Portions derived from FoundationDB Record Layer (YamlExecutionContext.java,
// QueryCommand.java),
// Copyright 2021-2024 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package javayamsql

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// ParseError locates a parse failure within a corpus file.
type ParseError struct {
	Path string
	Line int
	Err  error
}

func (e *ParseError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("%s:%d: %v", e.Path, e.Line, e.Err)
	}
	return fmt.Sprintf("%s: %v", e.Path, e.Err)
}

func (e *ParseError) Unwrap() error { return e.Err }

// parser carries the state Java keeps on its YamlExecutionContext for the
// duration of one run.
//
// The state is shared across an include graph because Java's context is: a
// `transaction_setups` block inside an included file registers names the
// *including* file can reference afterwards. That also means block order
// matters across files, so includes are expanded inline as they are met rather
// than after the parent finishes.
type parser struct {
	// corpus resolves include targets. Nil means includes are not followed.
	corpus *Corpus
	// txnSetups holds the names registered by transaction_setups blocks so far.
	txnSetups map[string]string
	// ancestors is the include chain used for cycle detection.
	ancestors []string
}

// Parse parses the documents of one `.yamsql` file.
//
// It does not follow `include:` directives — the include target is resolved
// against the corpus root, which Parse has no handle on. Use
// [Corpus.ParseFile] for that.
func Parse(path string, src []byte) (*File, error) {
	p := &parser{txnSetups: map[string]string{}}
	return p.parseFile(path, src, true)
}

// parseFile parses one file's document stream. topLevel is false for an
// included file, which is what makes a stray `options:` block in one an error
// no matter where it sits.
func (p *parser) parseFile(path string, src []byte, topLevel bool) (*File, error) {
	f := &File{Path: path}
	dec := yaml.NewDecoder(strings.NewReader(string(src)))
	for docIdx := 0; ; docIdx++ {
		var node yaml.Node
		err := dec.Decode(&node)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, &ParseError{Path: path, Err: err}
		}
		// A document that is empty (`---` followed by nothing, or the trailing
		// `...`) carries no block. Java's snakeyaml loadAll yields a null for
		// these and Block.parse would reject it; none occur in the corpus, but
		// skipping is the only non-crashing reading.
		if node.Kind == 0 || (node.Kind == yaml.DocumentNode && len(node.Content) == 0) {
			continue
		}
		blk, err := p.parseBlock(&node, docIdx, topLevel)
		if err != nil {
			var pe *ParseError
			if errors.As(err, &pe) {
				if pe.Path == "" {
					pe.Path = path
				}
				return nil, pe
			}
			return nil, &ParseError{Path: path, Err: err}
		}
		if blk.Kind == BlockInclude && p.corpus != nil {
			sub, err := p.resolveInclude(path, blk)
			if err != nil {
				return nil, err
			}
			blk.Include.Resolved = sub
		}
		f.Blocks = append(f.Blocks, blk)
	}
	return f, nil
}

// parseBlock mirrors Block.parse: a document must be a mapping of exactly one
// entry whose key names the block type.
func (p *parser) parseBlock(node *yaml.Node, docIdx int, topLevel bool) (*Block, error) {
	v, err := convertNode(node)
	if err != nil {
		return nil, err
	}
	if v.Kind != KindMap {
		return nil, &ParseError{Line: v.Line, Err: fmt.Errorf("a block must be a map, got %s", v.Kind)}
	}
	if len(v.Map) != 1 {
		return nil, &ParseError{Line: v.Line, Err: fmt.Errorf("a block must be a map of size 1, got %d keys", len(v.Map))}
	}
	e := v.Map[0]
	key, ok := e.Key.AsString()
	if !ok {
		return nil, &ParseError{Line: e.Key.Line, Err: errors.New("block key must be a string")}
	}

	b := &Block{Kind: BlockKind(key), Line: e.Key.Line}
	switch BlockKind(key) {
	case BlockOptions:
		// Block.parse asserts `isTopLevel && blockNumber == 0`. The isTopLevel
		// half is why an included file may never carry file-wide options at
		// all, even as its own first document: the options would silently
		// apply to a run whose preamble was already fixed by the entry file.
		if !topLevel || docIdx != 0 {
			return nil, &ParseError{Line: b.Line, Err: errors.New("file-wide options must be the first block")}
		}
		b.Options, b.Inert, err = parseOptionsBlock(e.Val)
	case BlockSchemaTemplate:
		b.SchemaTemplate, b.Inert, err = parseSchemaTemplateBlock(e.Val)
	case BlockSetup:
		b.Setup, b.Inert, err = p.parseSetupBlock(e.Val)
	case BlockTest:
		b.Test, b.Inert, err = p.parseTestBlock(e.Val)
	case BlockInclude:
		b.Include, err = parseIncludeBlock(e.Val)
	case BlockTransactionSetups:
		b.TransactionSetups, err = p.parseTransactionSetupsBlock(e.Val)
	case BlockCopy:
		b.Copy, b.Inert, err = parseCopyBlock(e.Val)
	default:
		return nil, &ParseError{Line: b.Line, Err: fmt.Errorf("cannot recognize the type of block %q", key)}
	}
	if err != nil {
		return nil, err
	}
	return b, nil
}

// mapping walks v as a mapping, splitting its keys into the ones the named
// construct actually reads and the ones it does not.
//
// Java's block and option parsers probe with containsKey/get and never look for
// leftovers, so a key they do not recognise is not an error — it is silently
// INERT, contributing nothing while looking for all the world like it
// configures something. Rejecting such keys would refuse files Java loads, so
// they are collected instead and surfaced through [Block.Inert]. The parse gate
// pins the exact set, which is what turns a silent upstream typo into a
// reviewable diff.
//
// The four dispatches Java genuinely closes — block key, command key, config
// key, YAML tag — stay hard errors; see their call sites.
func mapping(v *Value, what string, allowed ...string) (map[string]*Value, []InertDirective, error) {
	if v == nil || v.Kind != KindMap {
		return nil, nil, &ParseError{Line: lineOf(v), Err: fmt.Errorf("%s must be a map, got %s", what, kindOf(v))}
	}
	ok := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		ok[a] = true
	}
	out := make(map[string]*Value, len(v.Map))
	var inert []InertDirective
	for _, e := range v.Map {
		k, isStr := e.Key.AsString()
		if !isStr {
			return nil, nil, &ParseError{Line: e.Key.Line, Err: fmt.Errorf("%s key must be a string", what)}
		}
		if !ok[k] {
			inert = append(inert, InertDirective{Where: what, Key: k, Line: e.Key.Line})
			continue
		}
		out[k] = e.Val
	}
	return out, inert, nil
}

func lineOf(v *Value) int {
	if v == nil {
		return 0
	}
	return v.Line
}

// resolveInclude parses an include target inline, sharing this parser's state.
//
// Cycle detection is on the ANCESTOR chain, matching
// YamlReference.YamlResource.assertNotCyclic. Including the same file twice
// from one parent is legal — multiple-same-includes.yamsql does it four times —
// because each include is a distinct resource keyed by its call site, so only
// genuine self-recursion is refused.
func (p *parser) resolveInclude(from string, blk *Block) (*File, error) {
	target := path.Clean(blk.Include.Resource)
	wrap := func(err error) error {
		return &ParseError{Path: from, Line: blk.Line, Err: fmt.Errorf("include %s: %w", blk.Include.Resource, err)}
	}
	for _, a := range append(p.ancestors, from) {
		if a == target {
			chain := append(append([]string{}, p.ancestors...), from, target)
			return nil, wrap(fmt.Errorf("cyclic path detected at: %s", strings.Join(chain, " > ")))
		}
	}
	src, err := fs.ReadFile(p.corpus.FS, target)
	if err != nil {
		return nil, wrap(fmt.Errorf("could not find %q in resources bundle", target))
	}
	saved := p.ancestors
	p.ancestors = append(append([]string{}, saved...), from)
	sub, err := p.parseFile(target, src, false)
	p.ancestors = saved
	if err != nil {
		return nil, wrap(err)
	}
	return sub, nil
}

// transactionSetup mirrors YamlExecutionContext.getTransactionSetup, which
// resolves eagerly while the config is being parsed. That eagerness is the
// whole behaviour of transaction-setup/shouldFail/future-reference.yamsql: a
// setupReference naming a block that appears later in the file is a parse
// error, not a late execution failure.
func (p *parser) transactionSetup(name string) (string, bool) {
	sql, ok := p.txnSetups[name]
	return sql, ok
}

func parseOptionsBlock(v *Value) (*OptionsBlock, []InertDirective, error) {
	m, inert, err := mapping(v, "options", "supported_version", "required_clusters", "connection_options")
	if err != nil {
		return nil, nil, err
	}
	out := &OptionsBlock{}
	if sv, ok := m["supported_version"]; ok {
		if out.SupportedVersion, err = parseVersion(sv); err != nil {
			return nil, nil, err
		}
	}
	if rc, ok := m["required_clusters"]; ok {
		n, isInt := rc.AsInt()
		if !isInt {
			return nil, nil, &ParseError{Line: rc.Line, Err: fmt.Errorf("required_clusters must be an integer, got %s", kindOf(rc))}
		}
		out.RequiredClusters = &n
	}
	if co, ok := m["connection_options"]; ok {
		if out.ConnectionOptions, err = connectionOptions(co); err != nil {
			return nil, nil, err
		}
	}
	return out, inert, nil
}

// connectionOptions keeps the map as ordered entries. Java validates each key
// against the Options.Name enum, which lives in the relational API rather than
// the yaml framework; the names are checked for shape here and left otherwise
// opaque so this package does not fork that enum.
func connectionOptions(v *Value) ([]Entry, error) {
	if v == nil || v.Kind != KindMap {
		return nil, &ParseError{Line: lineOf(v), Err: fmt.Errorf("connection_options must be a map, got %s", kindOf(v))}
	}
	for _, e := range v.Map {
		if _, ok := e.Key.AsString(); !ok {
			return nil, &ParseError{Line: e.Key.Line, Err: errors.New("connection_options key must be a string")}
		}
	}
	return v.Map, nil
}

func parseSchemaTemplateBlock(v *Value) (*SchemaTemplateBlock, []InertDirective, error) {
	out := &SchemaTemplateBlock{}
	var inert []InertDirective
	// SetupBlock.SchemaTemplateBlock.parse branches on `document instanceof
	// List`: a list is the variant form, anything else is coerced to a string.
	if v != nil && v.Kind == KindSeq {
		out.ListForm = true
		if len(v.Seq) == 0 {
			return nil, nil, &ParseError{Line: v.Line, Err: errors.New("schema_template list form must declare at least one variant")}
		}
		for _, raw := range v.Seq {
			m, vi, err := mapping(raw, "schema_template variant", "definition", "initialVersionAtLeast", "initialVersionLessThan")
			if err != nil {
				return nil, nil, err
			}
			inert = append(inert, vi...)
			def, ok := m["definition"]
			if !ok {
				return nil, nil, &ParseError{Line: raw.Line, Err: errors.New("schema_template variant requires a 'definition' key")}
			}
			text, isStr := def.AsString()
			if !isStr {
				return nil, nil, &ParseError{Line: def.Line, Err: fmt.Errorf("schema_template variant definition must be a string, got %s", kindOf(def))}
			}
			variant := SchemaTemplateVariant{Line: raw.Line, Definition: text}
			if av, ok := m["initialVersionAtLeast"]; ok {
				if variant.AtLeast, err = parseVersion(av); err != nil {
					return nil, nil, err
				}
			}
			if lv, ok := m["initialVersionLessThan"]; ok {
				if variant.LessThan, err = parseVersion(lv); err != nil {
					return nil, nil, err
				}
			}
			out.Variants = append(out.Variants, variant)
		}
		if err := validateVariantRanges(out.Variants, v.Line); err != nil {
			return nil, nil, err
		}
		return out, inert, nil
	}

	text, ok := v.AsString()
	if !ok {
		return nil, nil, &ParseError{Line: lineOf(v), Err: fmt.Errorf("schema_template must be a string or a list of variants, got %s", kindOf(v))}
	}
	out.Variants = []SchemaTemplateVariant{{Line: lineOf(v), Definition: text}}
	return out, nil, nil
}

func (p *parser) parseSetupBlock(v *Value) (*SetupBlock, []InertDirective, error) {
	m, inert, err := mapping(v, "setup", "connect", "steps", "options")
	if err != nil {
		return nil, nil, err
	}
	out := &SetupBlock{Connect: m["connect"]}
	if opts, ok := m["options"]; ok {
		om, oi, err := mapping(opts, "setup options", "connection_options", "supported_version")
		if err != nil {
			return nil, nil, err
		}
		inert = append(inert, oi...)
		if x, ok := om["supported_version"]; ok {
			if out.SupportedVersion, err = parseVersion(x); err != nil {
				return nil, nil, err
			}
		}
		if co, ok := om["connection_options"]; ok {
			if out.ConnectionOptions, err = connectionOptions(co); err != nil {
				return nil, nil, err
			}
		}
	}
	steps, ok := m["steps"]
	if !ok || steps.IsNull() {
		return nil, nil, &ParseError{Line: lineOf(v), Err: errors.New("no steps provided in setup block")}
	}
	if steps.Kind != KindSeq {
		return nil, nil, &ParseError{Line: steps.Line, Err: fmt.Errorf("setup steps must be a list, got %s", steps.Kind)}
	}
	for _, step := range steps.Seq {
		// A setup step is a single-command map, wrapped by Java into a
		// one-element list before being handed to Command.parse. It therefore
		// admits no configs.
		if step.Kind != KindMap || len(step.Map) != 1 {
			return nil, nil, &ParseError{Line: step.Line, Err: errors.New("a setup step should be a single command")}
		}
		cmd, err := p.parseCommand(step, nil)
		if err != nil {
			return nil, nil, err
		}
		out.Steps = append(out.Steps, cmd)
	}
	return out, inert, nil
}

func parseIncludeBlock(v *Value) (*IncludeBlock, error) {
	name, ok := v.AsString()
	if !ok {
		return nil, &ParseError{Line: lineOf(v), Err: fmt.Errorf("include must name a resource, got %s", kindOf(v))}
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, &ParseError{Line: lineOf(v), Err: errors.New("include resource name is empty")}
	}
	return &IncludeBlock{Resource: name}, nil
}

func (p *parser) parseTransactionSetupsBlock(v *Value) (*TransactionSetupsBlock, error) {
	if v == nil || v.Kind != KindMap {
		return nil, &ParseError{Line: lineOf(v), Err: fmt.Errorf("transaction_setups must be a map, got %s", kindOf(v))}
	}
	out := &TransactionSetupsBlock{}
	for _, e := range v.Map {
		name, ok := e.Key.AsString()
		if !ok {
			return nil, &ParseError{Line: e.Key.Line, Err: errors.New("transaction setup name must be a string")}
		}
		sql, ok := e.Val.AsString()
		if !ok {
			return nil, &ParseError{Line: e.Val.Line, Err: fmt.Errorf("transaction setup %q must be a string command, got %s", name, kindOf(e.Val))}
		}
		// Registered as we go: later blocks may reference these, earlier ones
		// may not. Duplicate names overwrite, matching snakeyaml's
		// allowDuplicateKeys(true) plus a plain map put.
		p.txnSetups[name] = sql
		out.Setups = append(out.Setups, NamedSetup{Name: name, SQL: sql, Line: e.Key.Line})
	}
	return out, nil
}

func parseCopyBlock(v *Value) (*CopyBlock, []InertDirective, error) {
	m, inert, err := mapping(v, "copy_block", "source", "dest", "export_limit", "import_chunk_size")
	if err != nil {
		return nil, nil, err
	}
	out := &CopyBlock{}
	var si, di []InertDirective
	if out.Source, si, err = copyEndpoint(m["source"], "source"); err != nil {
		return nil, nil, err
	}
	if out.Dest, di, err = copyEndpoint(m["dest"], "dest"); err != nil {
		return nil, nil, err
	}
	inert = append(inert, si...)
	inert = append(inert, di...)
	if out.ExportLimit, err = optionalInt(m["export_limit"], "export_limit"); err != nil {
		return nil, nil, err
	}
	if out.ImportChunkSize, err = optionalInt(m["import_chunk_size"], "import_chunk_size"); err != nil {
		return nil, nil, err
	}
	return out, inert, nil
}

func copyEndpoint(v *Value, what string) (CopyEndpoint, []InertDirective, error) {
	var out CopyEndpoint
	m, inert, err := mapping(v, "copy_block "+what, "cluster", "path")
	if err != nil {
		return out, nil, err
	}
	if out.Cluster, err = optionalInt(m["cluster"], "cluster"); err != nil {
		return out, nil, err
	}
	p, ok := m["path"]
	if !ok {
		return out, nil, &ParseError{Line: lineOf(v), Err: fmt.Errorf("copy_block %s requires a path", what)}
	}
	if out.Path, ok = p.AsString(); !ok {
		return out, nil, &ParseError{Line: p.Line, Err: fmt.Errorf("copy_block %s path must be a string, got %s", what, kindOf(p))}
	}
	return out, inert, nil
}

func optionalInt(v *Value, what string) (*int64, error) {
	if v == nil {
		return nil, nil
	}
	n, ok := v.AsInt()
	if !ok {
		return nil, &ParseError{Line: v.Line, Err: fmt.Errorf("%s must be an integer, got %s", what, kindOf(v))}
	}
	return &n, nil
}
