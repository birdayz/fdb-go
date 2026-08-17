package logical

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// LogicalInlineValues is a multi-row VALUES table source in a FROM clause.
// Its collection is the exact literal array Java lowers directly to an
// ExplodeExpression: every array element is one named record row.
//
// This is deliberately distinct from both LogicalValues (the legacy
// single-row, text-only SELECT-without-FROM seed) and LogicalUnnest (a lateral
// correlated array access). Conflating either shape with an inline table would
// give it the wrong cardinality or make it participate in lateral-unnest
// gather/collision rules.
type LogicalInlineValues struct {
	Alias      string
	Binding    string
	collection values.Value
	resultType values.ExactTypeHandle
}

// NewInlineValues constructs an exact literal-table source. alias is the
// source's query-block correlation (an authored inline-table alias, or a
// parser-minted private alias when SQL omitted one).
func NewInlineValues(alias string, collection values.Value) (*LogicalInlineValues, error) {
	if alias == "" {
		return nil, fmt.Errorf("inline VALUES source requires a non-empty correlation alias")
	}
	if collection == nil {
		return nil, fmt.Errorf("inline VALUES source collection is nil")
	}
	array, ok := collection.Type().(*values.ArrayType)
	if !ok || array.ElementType == nil {
		return nil, fmt.Errorf("inline VALUES source requires an exact array collection, got %v", collection.Type())
	}
	row, ok := array.ElementType.(*values.RecordType)
	if !ok {
		return nil, fmt.Errorf("inline VALUES source array element is not a record row: %v", array.ElementType)
	}
	resultType, err := values.SnapshotExactType(row)
	if err != nil {
		return nil, fmt.Errorf("inline VALUES result row: %w", err)
	}
	return &LogicalInlineValues{
		Alias:      alias,
		collection: collection,
		resultType: resultType,
	}, nil
}

func (*LogicalInlineValues) Children() []LogicalOperator { return []LogicalOperator{} }

func (v *LogicalInlineValues) Explain(indent string) string {
	return fmt.Sprintf("%sInlineValues(%s AS %s)", indent, values.ExplainValue(v.collection), v.Alias)
}

func (v *LogicalInlineValues) CollectionValue() values.Value { return v.collection }

func (v *LogicalInlineValues) ResultType() values.Type { return v.resultType.Type() }

// FindOwnerInlineValues resolves one visible inline VALUES source in the
// current logical FROM scope. CTE bodies are separate scopes, so a CTE exposes
// only its Main here. Duplicate aliases are ambiguous and deliberately return
// nil instead of selecting whichever leaf the traversal happens to visit
// first.
func FindOwnerInlineValues(op LogicalOperator, alias string) *LogicalInlineValues {
	if op == nil || alias == "" {
		return nil
	}
	var found *LogicalInlineValues
	ambiguous := false
	var walk func(LogicalOperator)
	walk = func(current LogicalOperator) {
		if current == nil || ambiguous {
			return
		}
		switch typed := current.(type) {
		case *LogicalInlineValues:
			if !strings.EqualFold(typed.Alias, alias) {
				return
			}
			if found != nil && found != typed {
				ambiguous = true
				return
			}
			found = typed
		case *LogicalCTE:
			walk(typed.Main)
		default:
			for _, child := range current.Children() {
				walk(child)
			}
		}
	}
	walk(op)
	if ambiguous {
		return nil
	}
	return found
}
