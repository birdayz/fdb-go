package javayamsql

import (
	"fmt"
	"sort"
	"strings"
)

// Census counts the directive and tag surface actually exercised by a set of
// parsed files.
//
// It exists so that a version bump fails *describably*. A parse gate alone
// reports "still parses"; it cannot notice that upstream deleted every use of a
// directive, or doubled the query count. The census turns those into a diff.
type Census struct {
	Files   int
	Blocks  map[BlockKind]int
	Queries int
	// Commands counts every command including queries, so
	// Commands[CommandQuery] == Queries.
	Commands map[CommandKind]int
	Configs  map[ConfigKind]int
	// Tags counts custom-tag occurrences anywhere in a file, including inside
	// parameter-injection segments.
	Tags map[string]int
	// Rows and Cells count expected result rows and their cells.
	Rows  int
	Cells int
	// PositionalCells is the subset of Cells matched by column number.
	PositionalCells int
	// Segments counts `!!…!!` parameter injections.
	Segments int
	// Includes counts include blocks (not distinct targets).
	Includes int
}

// NewCensus returns an empty census with its maps ready.
func NewCensus() *Census {
	return &Census{
		Blocks:   map[BlockKind]int{},
		Commands: map[CommandKind]int{},
		Configs:  map[ConfigKind]int{},
		Tags:     map[string]int{},
	}
}

// Add folds one file into the census. Included files are NOT followed; count
// them separately so each file contributes exactly once.
func (c *Census) Add(f *File) {
	c.Files++
	for _, b := range f.Blocks {
		c.Blocks[b.Kind]++
		switch b.Kind {
		case BlockOptions:
			c.countValueTags(versionValue(b.Options.SupportedVersion))
			c.countEntryTags(b.Options.ConnectionOptions)
		case BlockSchemaTemplate:
			for _, v := range b.SchemaTemplate.Variants {
				c.countValueTags(versionValue(v.AtLeast))
				c.countValueTags(versionValue(v.LessThan))
			}
		case BlockSetup:
			c.countEntryTags(b.Setup.ConnectionOptions)
			for _, cmd := range b.Setup.Steps {
				c.addCommand(cmd)
			}
		case BlockTest:
			c.countValueTags(versionValue(b.Test.Options.SupportedVersion))
			c.countEntryTags(b.Test.Options.ConnectionOptions)
			for _, t := range b.Test.Tests {
				c.addCommand(t.Command)
			}
		case BlockInclude:
			c.Includes++
		}
	}
}

func (c *Census) addCommand(cmd *Command) {
	c.Commands[cmd.Kind]++
	if cmd.Kind == CommandQuery {
		c.Queries++
	}
	c.Segments += len(cmd.Segments)
	for _, s := range cmd.Segments {
		c.countValueTags(s.Value)
	}
	for _, cfg := range cmd.Configs {
		c.Configs[cfg.Kind]++
		c.countValueTags(cfg.Raw)
		for _, row := range cfg.Rows {
			c.Rows++
			for _, cell := range row.Cells {
				c.Cells++
				if cell.Positional() {
					c.PositionalCells++
				}
			}
		}
	}
}

// versionValue re-materialises the tag a Version was parsed from, so
// `!current_version` is counted wherever it appears.
func versionValue(v *Version) *Value {
	if v == nil || !v.Current {
		return nil
	}
	return &Value{Kind: KindTagged, Tag: TagCurrentVersion, Inner: &Value{Kind: KindNull}}
}

func (c *Census) countEntryTags(entries []Entry) {
	for _, e := range entries {
		c.countValueTags(e.Key)
		c.countValueTags(e.Val)
	}
}

func (c *Census) countValueTags(v *Value) {
	if v == nil {
		return
	}
	if v.Kind == KindTagged {
		c.Tags[v.Tag]++
		c.countValueTags(v.Inner)
		return
	}
	for _, s := range v.Seq {
		c.countValueTags(s)
	}
	c.countEntryTags(v.Map)
}

// Line renders the census as one stable, greppable line.
func (c *Census) Line() string {
	var b strings.Builder
	fmt.Fprintf(&b, "files=%d blocks{%s} queries=%d commands{%s} configs{%s} tags{%s}",
		c.Files, countsOf(c.Blocks), c.Queries, countsOf(c.Commands),
		countsOf(c.Configs), countsOf(c.Tags))
	fmt.Fprintf(&b, " rows=%d cells=%d positional_cells=%d segments=%d includes=%d",
		c.Rows, c.Cells, c.PositionalCells, c.Segments, c.Includes)
	return b.String()
}

func countsOf[K ~string](m map[K]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[K(k)]))
	}
	return strings.Join(parts, ",")
}
