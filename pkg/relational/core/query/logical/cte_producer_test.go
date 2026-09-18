package logical

import (
	"errors"
	"slices"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestPreparedCTEDefiningRegistryAndCapturedAbsence(t *testing.T) {
	t.Parallel()
	prepare := func(name string, registry CTERegistry, body LogicalOperator, options ...CTEOption) *CTEProducer {
		t.Helper()
		producer, err := PrepareCTE(name, false, registry, func(CTERegistry) (LogicalOperator, error) { return body, nil }, options...)
		if err != nil {
			t.Fatal(err)
		}
		return producer
	}
	first := prepare("A", CTERegistry{}, NewScan("PHYSICAL", "P"))
	unrelated := prepare("UNRELATED", CTERegistry{}, NewScan("OTHER", "O"))
	defining := CTERegistry{}.With(first).With(unrelated)
	dependency := NewScan("A", "DEPENDENCY")
	absence := NewScan("LATER", "CAPTURED")
	body := NewJoin(dependency, absence, JoinInner, "")
	aliases := []string{"quoted", "a.b"}
	builds := 0
	producer, err := PrepareCTE("B", false, defining, func(registry CTERegistry) (LogicalOperator, error) {
		builds++
		if registry.Lookup("UNRELATED") != unrelated || registry.Lookup("LATER") != nil {
			t.Fatal("preparation lost the complete defining environment")
		}
		return body, nil
	}, CTEColumns(aliases...))
	if err != nil {
		t.Fatal(err)
	}
	shadow := prepare("A", CTERegistry{}, NewScan("SHADOW", "S"))
	later := prepare("LATER", CTERegistry{}, NewScan("NEW", "N"))
	consumerRegistry := defining.With(producer).With(shadow).With(later)
	left, right := NewScan("B", "LEFT"), NewScan("B", "RIGHT")
	left.Binding, right.Binding = "CONSUMER_LEFT", "CONSUMER_RIGHT"
	for range 2 {
		BindCTESources(NewJoin(left, right, JoinInner, ""), consumerRegistry)
		BindCTESources(body, consumerRegistry)
	}
	if builds != 1 || producer.Body() != body || left.Source.Producer() != producer || right.Source.Producer() != producer {
		t.Fatal("consumers did not retain one prepared producer")
	}
	if left.Binding == right.Binding || left.Binding != "CONSUMER_LEFT" || right.Binding != "CONSUMER_RIGHT" {
		t.Fatal("shared producer changed the consumer identities")
	}
	if dependency.Source.Producer() != first || !absence.Source.Resolved() || absence.Source.Producer() != nil {
		t.Fatal("a consumer shadow or later declaration rebound the producer body")
	}
	if producer.DefiningRegistry().Lookup("A") != first || producer.DefiningRegistry().Lookup("UNRELATED") != unrelated || producer.DefiningRegistry().Lookup("LATER") != nil {
		t.Fatal("producer did not retain its complete defining registry")
	}
	aliases[0] = "changed"
	returned := producer.ColumnAliases()
	returned[1] = "changed"
	if !slices.Equal(producer.ColumnAliases(), []string{"quoted", "a.b"}) {
		t.Fatal("column aliases are writable through a caller's slice")
	}
}

func TestPreparedCTERecursiveScopeAndFailedPublication(t *testing.T) {
	t.Parallel()
	outer, err := PrepareCTE("R", true, CTERegistry{}, func(registry CTERegistry) (LogicalOperator, error) {
		return NewUnion([]LogicalOperator{NewScan("SEED", "S"), NewScan("R", "SELF")}, false), nil
	}, CTEColumns("n"), CTETraversal(TraversalPostOrder))
	if err != nil {
		t.Fatal(err)
	}
	registry := CTERegistry{}.With(outer)
	inner, err := PrepareCTE("R", true, registry, func(local CTERegistry) (LogicalOperator, error) {
		if local.Lookup("R") == outer {
			t.Fatal("recursive preparation did not install its private self binding")
		}
		return NewUnion([]LogicalOperator{NewScan("OTHER_SEED", "S"), NewScan("R", "INNER_SELF")}, false), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ReferencesCTE(outer.Body(), outer) || ReferencesCTE(outer.Body(), inner) || !ReferencesCTE(inner.Body(), inner) || ReferencesCTE(inner.Body(), outer) {
		t.Fatal("recursive self ownership crossed a lexical shadow")
	}
	if outer.Identity() == inner.Identity() {
		t.Fatal("shadowed recursive declarations share a producer identity")
	}
	for _, producer := range []*CTEProducer{outer, inner} {
		if producer.ScanBinding() != values.NamedCorrelationIdentifier("RforScan") ||
			producer.InsertBinding() != values.NamedCorrelationIdentifier("RforInsert") ||
			producer.ScanBinding() == producer.InsertBinding() {
			t.Fatalf("recursive bindings = (%#v, %#v), want Java's distinct named scan/insert bindings", producer.ScanBinding(), producer.InsertBinding())
		}
	}
	if inner.DefiningRegistry().Lookup("R") != outer || registry.Lookup("R") != outer || !outer.Recursive() || outer.TraversalOrder() != TraversalPostOrder {
		t.Fatal("recursive preparation changed its enclosing declaration")
	}
	want := errors.New("preparation failed")
	failed, err := PrepareCTE("R", true, registry, func(CTERegistry) (LogicalOperator, error) { return nil, want })
	if failed != nil || !errors.Is(err, want) || registry.Lookup("R") != outer {
		t.Fatal("failed recursive preparation published a declaration")
	}
}

func TestResolveScanDoesNotPublishConsumerScope(t *testing.T) {
	t.Parallel()
	first := NewCTE("C", NewScan("FIRST", ""), nil, false).CTEProducer
	second := NewCTE("C", NewScan("SECOND", ""), nil, false).CTEProducer
	firstScope, secondScope := CTERegistry{}.With(first), CTERegistry{}.With(second)
	scan := NewScan("C", "CONSUMER")
	for _, test := range []struct {
		registry CTERegistry
		want     *CTEProducer
	}{{firstScope, first}, {secondScope, second}, {CTERegistry{}, nil}} {
		if got := ResolveScan(scan, test.registry); got != test.want {
			t.Fatalf("read-side lookup returned %p, want this caller's producer %p", got, test.want)
		}
		if scan.Source.Resolved() {
			t.Fatal("read-side lookup published a consumer scope into a shared scan")
		}
	}
	// Construction alone seals ownership. Later read-side lookups must honor
	// both a selected producer and a captured physical lookup under shadows.
	BindCTESources(scan, firstScope)
	if !scan.Source.Resolved() || ResolveScan(scan, secondScope) != first || ResolveScan(scan, CTERegistry{}) != first {
		t.Fatal("construction did not retain the selected producer independently of readers")
	}
	physical := NewScan("C", "PHYSICAL")
	BindCTESources(physical, CTERegistry{})
	if !physical.Source.Resolved() || ResolveScan(physical, firstScope) != nil {
		t.Fatal("captured physical ownership fell through to a reader's CTE")
	}
}

func TestResolveScanIdentifierSegments(t *testing.T) {
	t.Parallel()
	literal := NewCTE("S.T", NewScan("CTE_ROWS", ""), nil, false).CTEProducer
	registry := CTERegistry{}.With(literal)
	for _, test := range []struct {
		name string
		path []string
		want *CTEProducer
	}{
		{"quoted_dot", []string{"S.T"}, literal},
		{"schema_qualified", []string{"S", "T"}, nil},
		{"malformed_not_legacy", []string{}, nil},
		{"legacy", nil, literal},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			scan := NewScan("S.T", "", test.path...)
			if got := ResolveScan(scan, registry); got != test.want {
				t.Fatalf("path %q selected %p, want %p", test.path, got, test.want)
			}
			if scan.Source.Resolved() {
				t.Fatal("identifier lookup published a scan binding")
			}
			BindCTESources(scan, registry)
			if !scan.Source.Resolved() || scan.Source.Producer() != test.want {
				t.Fatal("construction captured a different identifier")
			}
			if got := ResolveScan(scan, CTERegistry{}); got != test.want {
				t.Fatal("a later scope changed captured ownership")
			}
		})
	}
}

func TestPreparedCTEIdentifierSegments(t *testing.T) {
	t.Parallel()
	literal := NewCTE("S.T", NewScan("LITERAL_ROWS", ""), nil, false).CTEProducer
	path := []string{"S", "T"}
	option := CTENamePath(path...)
	path[0] = "CHANGED"
	qualified, err := PrepareCTE("S.T", false, CTERegistry{}.With(literal), func(CTERegistry) (LogicalOperator, error) {
		return NewScan("QUALIFIED_ROWS", ""), nil
	}, option)
	if err != nil {
		t.Fatal(err)
	}
	returned := qualified.NamePath()
	returned[0] = "CHANGED"
	snapshot := qualified.WithBody(NewScan("LOWERED_ROWS", ""))
	for _, producer := range []*CTEProducer{qualified, snapshot} {
		if !slices.Equal(producer.NamePath(), []string{"S", "T"}) {
			t.Fatalf("declaration name path changed through a caller's slice: %q", producer.NamePath())
		}
		registry := CTERegistry{}.With(literal).With(producer)
		for _, test := range []struct {
			path []string
			want *CTEProducer
		}{
			{[]string{"S.T"}, literal},
			{[]string{"S", "T"}, producer},
			{[]string{"s", "T"}, nil},
			{[]string{"T"}, nil},
		} {
			scan := NewScan("S.T", "CONSUMER", test.path...)
			if got := ResolveScan(scan, registry); got != test.want {
				t.Fatalf("normalized path %q selected %p, want %p", test.path, got, test.want)
			}
		}
	}
}

func TestCTEBodyScopeIsReadOnlyAndLexical(t *testing.T) {
	t.Parallel()
	for _, prepared := range []bool{false, true} {
		for _, recursive := range []bool{false, true} {
			name := "unprepared"
			if prepared {
				name = "prepared"
			}
			if recursive {
				name += "_recursive"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				first := NewCTE("A", NewScan("FIRST", ""), nil, false).CTEProducer
				shadow := NewCTE("A", NewScan("SHADOW", ""), nil, false).CTEProducer
				defining := CTERegistry{}.With(first)
				producer := NewCTE("C", NewScan("A", ""), nil, recursive).CTEProducer
				if prepared {
					var err error
					producer, err = PrepareCTE("C", recursive, defining, func(CTERegistry) (LogicalOperator, error) {
						return NewScan("A", ""), nil
					})
					if err != nil {
						t.Fatal(err)
					}
				}
				registry := defining.With(producer).With(shadow)
				if prepared {
					// Prepared ownership must not depend on the producer still
					// being introduced in the reader's current registry.
					registry = CTERegistry{}.With(shadow)
				}
				scope := registry.BodyScope(producer)
				if scope.Lookup("A") != first {
					t.Fatal("body inherited a consumer shadow instead of its defining A")
				}
				var self *CTEProducer
				if recursive {
					self = producer
				}
				if scope.Lookup("C") != self {
					t.Fatal("body scope lost recursive self or exposed a non-recursive self")
				}
				if producer.prepared != prepared || (!prepared && producer.defining.Lookup("A") != nil) {
					t.Fatal("body-scope lookup published preparation into its input")
				}
				if registry.Lookup("A") != shadow {
					t.Fatal("body-scope lookup changed the consumer's registry")
				}
			})
		}
	}
}
