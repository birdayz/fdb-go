package logical

import (
	"slices"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// CTERegistry is a persistent lexical registry. Extending it never changes the
// environment retained by an earlier declaration, including names absent there.
type CTERegistry struct {
	parent   *CTERegistry
	producer *CTEProducer
}

func (r CTERegistry) With(p *CTEProducer) CTERegistry {
	extended := r
	extended.parent = &r
	extended.producer = p
	return extended
}

// Lookup compares captured, normalized identifier segments when supplied.
// Only legacy string-only callers use a flattened, case-insensitive name.
func (r CTERegistry) Lookup(name string, path ...string) *CTEProducer {
	for p := &r; p != nil; p = p.parent {
		if p.producer == nil {
			continue
		}
		if path != nil {
			if len(path) > 0 && slices.Equal(p.producer.namePath, path) {
				return p.producer
			}
		} else if strings.EqualFold(p.producer.name, name) {
			return p.producer
		}
	}
	return nil
}

// BodyScope selects a producer's defining frame without publishing it. Prepared
// declarations retain their frame; constructor inputs are interpreted in the
// lexical frame that introduced them in this reader's persistent registry.
func (r CTERegistry) BodyScope(producer *CTEProducer) CTERegistry {
	defining := producer.defining
	if !producer.prepared {
		for frame := &r; frame != nil; frame = frame.parent {
			if frame.producer == producer {
				if frame.parent != nil {
					defining = *frame.parent
				}
				break
			}
		}
	}
	if producer.recursive {
		return defining.With(producer)
	}
	return defining
}

func (r CTERegistry) Names() []string {
	var names []string
	for p := &r; p != nil; p = p.parent {
		if p.producer != nil {
			names = append(names, p.producer.name)
		}
	}
	return names
}

// CTEIdentity identifies a declaration across immutable lowering snapshots.
type CTEIdentity struct{ token byte }

// CTEProducer owns one declaration. After preparation its definition and
// defining registry are immutable; envelopes and scans share this same record.
// Consumer correlation identifiers belong to scans, never to this record.
type CTEProducer struct {
	identity       *CTEIdentity
	scanBinding    values.CorrelationIdentifier
	insertBinding  values.CorrelationIdentifier
	name           string
	namePath       []string
	body           LogicalOperator
	columnAliases  []string
	recursive      bool
	traversalOrder TraversalOrder
	defining       CTERegistry
	prepared       bool
}

func (p *CTEProducer) Identity() *CTEIdentity                      { return p.identity }
func (p *CTEProducer) ScanBinding() values.CorrelationIdentifier   { return p.scanBinding }
func (p *CTEProducer) InsertBinding() values.CorrelationIdentifier { return p.insertBinding }
func (p *CTEProducer) Name() string                                { return p.name }
func (p *CTEProducer) NamePath() []string                          { return slices.Clone(p.namePath) }
func (p *CTEProducer) Body() LogicalOperator                       { return p.body }
func (p *CTEProducer) ColumnAliases() []string                     { return slices.Clone(p.columnAliases) }
func (p *CTEProducer) Recursive() bool                             { return p.recursive }
func (p *CTEProducer) TraversalOrder() TraversalOrder              { return p.traversalOrder }
func (p *CTEProducer) DefiningRegistry() CTERegistry               { return p.defining }

type CTEOption struct {
	columns                  []string
	namePath                 []string
	traversal                TraversalOrder
	hasColumns, hasTraversal bool
}

// CTENamePath captures the declaration's normalized name and qualifier without
// flattening quoted dots. Synthetic declarations default to one literal name.
func CTENamePath(path ...string) CTEOption {
	return CTEOption{namePath: slices.Clone(path)}
}

func CTEColumns(names ...string) CTEOption {
	return CTEOption{columns: slices.Clone(names), hasColumns: true}
}

func CTETraversal(order TraversalOrder) CTEOption {
	return CTEOption{traversal: order, hasTraversal: true}
}

// PrepareCTE builds a declaration once, before publishing it. Recursive bodies
// receive a private self binding during construction, matching Java's temporary
// scan registration. A failed build never escapes into the caller's registry.
func PrepareCTE(name string, recursive bool, defining CTERegistry,
	build func(CTERegistry) (LogicalOperator, error), options ...CTEOption,
) (*CTEProducer, error) {
	p := newCTEProducer(name, nil, recursive, options...)
	p.defining = defining
	bodyRegistry := defining
	if recursive {
		bodyRegistry = defining.With(p)
	}
	body, err := build(bodyRegistry)
	if err != nil {
		return nil, err
	}
	p.body = body
	BindCTESources(body, bodyRegistry)
	p.prepared = true
	return p, nil
}

func newCTEProducer(name string, body LogicalOperator, recursive bool, options ...CTEOption) *CTEProducer {
	p := &CTEProducer{name: name, namePath: []string{name}, body: body, recursive: recursive, identity: &CTEIdentity{}}
	if recursive {
		// Java QueryVisitor.handleRecursiveNamedQuery scopes these named
		// bindings to each recursive execution; declaration identity is separate.
		p.scanBinding = values.NamedCorrelationIdentifier(name + "forScan")
		p.insertBinding = values.NamedCorrelationIdentifier(name + "forInsert")
	}
	for _, option := range options {
		if option.namePath != nil {
			p.namePath = slices.Clone(option.namePath)
		}
		if option.hasColumns {
			p.columnAliases = slices.Clone(option.columns)
		}
		if option.hasTraversal {
			p.traversalOrder = option.traversal
		}
	}
	return p
}

// ScanSource records both a selected producer and a physical lookup. The zero
// value is reserved for constructor input not yet adapted by BindCTESources.
type ScanSource struct {
	resolved bool
	producer *CTEProducer
}

func (s ScanSource) Resolved() bool           { return s.resolved }
func (s ScanSource) Producer() *CTEProducer   { return s.producer }
func PhysicalScanSource() ScanSource          { return ScanSource{resolved: true} }
func CTEScanSource(p *CTEProducer) ScanSource { return ScanSource{resolved: true, producer: p} }

// ResolveScan is a read-only source lookup. Captured physical ownership cannot
// fall through to a later, same-named declaration. Unbound constructor inputs
// use this caller's registry without publishing it into a shared expression;
// only construction's BindCTESources seals that choice.
func ResolveScan(scan *LogicalScan, registry CTERegistry) *CTEProducer {
	if scan.Source.resolved {
		return scan.Source.producer
	}
	return registry.Lookup(scan.Table, scan.TablePath...)
}

// BindCTESources seals source ownership during construction, before the graph
// is shared. Prepared definitions are never revisited under a consumer's
// registry. Read-side lookup uses ResolveScan and does not publish bindings.
func BindCTESources(op LogicalOperator, registry CTERegistry) {
	if op == nil {
		return
	}
	switch node := op.(type) {
	case *LogicalScan:
		if !node.Source.resolved {
			node.Source = CTEScanSource(ResolveScan(node, registry))
		}
		return
	case *LogicalCTE:
		p := node.CTEProducer
		if !p.prepared {
			p.defining = registry
			bodyRegistry := registry
			if p.recursive {
				bodyRegistry = registry.With(p)
			}
			BindCTESources(p.body, bodyRegistry)
			p.prepared = true
		}
		BindCTESources(node.Main, registry.With(p))
		return
	}
	for _, child := range op.Children() {
		BindCTESources(child, registry)
	}
	var scalar []ScalarSubquery
	var correlated []CorrelatedScalarSubquery
	var existential []ExistsSubquery
	switch node := op.(type) {
	case *LogicalFilter:
		scalar, correlated, existential = node.ScalarSubqueries, node.CorrelatedScalarSubqueries, node.ExistsSubqueries
	case *LogicalProject:
		scalar, correlated = node.ScalarSubqueries, node.CorrelatedScalarSubqueries
	case *LogicalAggregate:
		scalar, existential = node.HavingScalarSubqueries, node.HavingExistsSubqueries
	case *LogicalJoin:
		existential = node.OnExistsSubqueries
	}
	for _, edge := range scalar {
		BindCTESources(edge.Plan, registry)
	}
	for _, edge := range correlated {
		BindCTESources(edge.InnerPlan, registry)
	}
	for _, edge := range existential {
		BindCTESources(edge.Plan, registry)
	}
}

// ReferencesCTE follows selected identities, including retained dependencies.
func ReferencesCTE(op LogicalOperator, producer *CTEProducer) bool {
	return ReferencesCTEInScope(op, producer, CTERegistry{})
}

// ReferencesCTEInScope also resolves unbound constructor inputs in the reader's
// lexical scope. It never seals scan ownership or prepares declarations.
func ReferencesCTEInScope(op LogicalOperator, producer *CTEProducer, registry CTERegistry) bool {
	seen := make(map[*CTEProducer]bool)
	var walk func(LogicalOperator, CTERegistry) bool
	walk = func(current LogicalOperator, scope CTERegistry) bool {
		if current == nil {
			return false
		}
		if scan, ok := current.(*LogicalScan); ok {
			selected := ResolveScan(scan, scope)
			if selected == producer {
				return true
			}
			if selected != nil && !seen[selected] {
				seen[selected] = true
				return walk(selected.Body(), scope.BodyScope(selected))
			}
			return false
		}
		if cte, ok := current.(*LogicalCTE); ok {
			return walk(cte.Main, scope.With(cte.CTEProducer))
		}
		for _, child := range current.Children() {
			if walk(child, scope) {
				return true
			}
		}
		for _, child := range AttachedPlans(current) {
			if walk(child, scope) {
				return true
			}
		}
		return false
	}
	return walk(op, registry)
}

// FindVisibleScan resolves a FROM qualifier without entering a definition.
func FindVisibleScan(op LogicalOperator, alias string) *LogicalScan {
	if op == nil {
		return nil
	}
	if scan, ok := op.(*LogicalScan); ok {
		name := scan.Alias
		if name == "" {
			name = scan.Table
		}
		if strings.EqualFold(name, alias) {
			return scan
		}
		return nil
	}
	if cte, ok := op.(*LogicalCTE); ok {
		return FindVisibleScan(cte.Main, alias)
	}
	for _, child := range op.Children() {
		if scan := FindVisibleScan(child, alias); scan != nil {
			return scan
		}
	}
	return nil
}

// WithBody creates an immutable lowering snapshot. Its declaration identity,
// aliases, recursive bindings and captured registry survive the body rewrite.
func (p *CTEProducer) WithBody(body LogicalOperator) *CTEProducer {
	copy := *p
	copy.body = body
	return &copy
}
