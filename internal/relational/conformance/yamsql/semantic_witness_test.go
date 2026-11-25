package yamsql

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/core/parser"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"github.com/antlr4-go/antlr/v4"
)

func walkSemanticTree(tree antlr.Tree, visit func(antlr.Tree)) {
	visit(tree)
	for _, child := range tree.GetChildren() {
		walkSemanticTree(child, visit)
	}
}

// witnessNumericProducer checks both projected input and function input leaves.
// It deliberately handles only the generated envelope's single-source queries;
// it is not a classifier for arbitrary SQL or runtime executor binding.
func witnessNumericProducer(test Test, producer string, bits string) error {
	root, err := parser.Parse(test.Query)
	if err != nil {
		return err
	}
	var projections []*antlrgen.SelectExpressionElementContext
	walkSemanticTree(root, func(tree antlr.Tree) {
		if p, ok := tree.(*antlrgen.SelectExpressionElementContext); ok {
			projections = append(projections, p)
		}
	})
	expectedColumns := 1
	if !strings.HasPrefix(test.ID, "producer/") {
		expectedColumns = 2
	}
	if len(projections) != expectedColumns {
		return fmt.Errorf("unexpected projection count %d", len(projections))
	}
	expected, err := tagged("float64", bits).decode()
	if err != nil {
		return err
	}
	args, err := decodeScalars(test.Args)
	if err != nil {
		return err
	}
	if producer == "driver-text-transport" {
		if len(args) != expectedColumns {
			return fmt.Errorf("argument count %d", len(args))
		}
		for _, arg := range args {
			if !exactValueEqual(expected, arg) {
				return fmt.Errorf("argument representation mismatch")
			}
		}
	} else if len(args) != 0 {
		return fmt.Errorf("non-transport producer has arguments")
	}

	for i, projection := range projections {
		atom, err := numericAtom(projection.Expression())
		if err != nil {
			return err
		}
		if i == 1 {
			wrapper, ok := atom.(*antlrgen.FunctionCallExpressionAtomContext)
			if !ok {
				return fmt.Errorf("result projection must be a scalar call")
			}
			call, ok := wrapper.FunctionCall().(*antlrgen.ScalarFunctionCallContext)
			if !ok {
				return fmt.Errorf("result must use scalar-function syntax")
			}
			fn := strings.Split(test.ID, "/")[0]
			name := call.ScalarFunctionName()
			if name.GetStart() != name.GetStop() || name.GetStart().GetText() != fn {
				return fmt.Errorf("function spelling does not witness %s", fn)
			}
			fnArgs := call.FunctionArgs().AllFunctionArg()
			arity := 1
			if fn == "POWER" || fn == "POW" {
				arity = 2
			}
			if len(fnArgs) != arity {
				return fmt.Errorf("wrong function arity")
			}
			if arity == 2 {
				exponent, err := numericAtom(fnArgs[1].Expression())
				if err != nil {
					return err
				}
				c, ok := exponent.(*antlrgen.ConstantExpressionAtomContext)
				if !ok {
					return fmt.Errorf("exponent must be constant 3")
				}
				d, ok := c.Constant().(*antlrgen.DecimalConstantContext)
				if !ok {
					return fmt.Errorf("exponent must be positive integer 3")
				}
				token := d.DecimalLiteral().DECIMAL_LITERAL()
				if token == nil || token.GetSymbol().GetText() != "3" {
					return fmt.Errorf("exponent must be integer 3")
				}
			}
			atom, err = numericAtom(fnArgs[0].Expression())
			if err != nil {
				return err
			}
		}
		switch producer {
		case "driver-text-transport":
			if _, ok := atom.(*antlrgen.PreparedStatementParameterAtomContext); !ok {
				return fmt.Errorf("producer is not a parameter atom")
			}
		case "stored-column":
			node, ok := atom.(*antlrgen.FullColumnNameExpressionAtomContext)
			if !ok {
				return fmt.Errorf("producer is not a column atom")
			}
			// Inspect the identifier tokens, not concatenated expression text.
			id := node.FullColumnName().FullId()
			if id.GetStart().GetText() != "x" || id.GetStart() != id.GetStop() {
				return fmt.Errorf("unexpected stored-column identifier")
			}
		case "literal":
			node, ok := atom.(*antlrgen.ConstantExpressionAtomContext)
			if !ok {
				return fmt.Errorf("producer is not a constant atom")
			}
			var literal antlrgen.IDecimalLiteralContext
			negative := false
			switch constant := node.Constant().(type) {
			case *antlrgen.DecimalConstantContext:
				literal = constant.DecimalLiteral()
			case *antlrgen.NegativeDecimalConstantContext:
				literal = constant.DecimalLiteral()
				negative = true
			default:
				return fmt.Errorf("producer is not a decimal constant")
			}
			token := literal.REAL_LITERAL()
			if token == nil {
				return fmt.Errorf("literal does not have a DOUBLE lexical form")
			}
			value, err := strconv.ParseFloat(token.GetSymbol().GetText(), 64)
			if err != nil {
				return err
			}
			if negative {
				value = math.Copysign(value, -1)
			}
			if !exactValueEqual(expected, value) {
				return fmt.Errorf("literal representation mismatch")
			}
		default:
			return fmt.Errorf("unknown producer %q", producer)
		}
	}
	return nil
}

func TestNumericProducerWitness(t *testing.T) {
	t.Parallel()
	m, err := loadNumericManifest()
	if err != nil {
		t.Fatal(err)
	}
	s := m.scenario()
	for _, test := range s.Tests {
		parts := strings.Split(test.ID, "/")
		var bits string
		for _, input := range m.Inputs {
			if input.Name == parts[2] {
				bits = input.Bits
			}
		}
		if err := witnessNumericProducer(test, parts[1], bits); err != nil {
			t.Fatalf("%s: %v", test.ID, err)
		}
	}
	// Each negative control changes a submitted statement or decoded argument,
	// keeping its claimed cell identity unchanged.
	base := s.Tests[1]
	cases := []struct {
		name           string
		test           Test
		producer, bits string
	}{
		{"swapped label", base, "stored-column", "8000000000000000"},
		{"wrong input", base, "literal", "0000000000000000"},
		{"unknown producer", base, "unknown", "8000000000000000"},
	}
	bound := Test{ID: "ROUND/driver-text-transport/negative-zero", Query: "SELECT ?, ROUND(?) FROM numbers WHERE id = 1", Args: []Scalar{tagged("float64", "8000000000000000"), tagged("float64", "0000000000000000")}}
	cases = append(cases, struct {
		name           string
		test           Test
		producer, bits string
	}{"swapped bound function input", bound, "driver-text-transport", "8000000000000000"})
	alias := Test{ID: "CEILING/stored-column/negative-zero", Query: "SELECT x, CEIL(x) FROM numbers WHERE id = 1"}
	duplicate := Test{ID: "FLOOR/stored-column/negative-zero", Query: "SELECT FLOOR(x), FLOOR(x) FROM numbers WHERE id = 1"}
	for _, test := range []Test{alias, duplicate} {
		if err := witnessNumericProducer(test, "stored-column", "8000000000000000"); err == nil {
			t.Fatalf("accepted false function projection: %s", test.Query)
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := witnessNumericProducer(tc.test, tc.producer, tc.bits); err == nil {
				t.Fatal("accepted false producer witness")
			}
		})
	}
}

func numericAtom(expression antlrgen.IExpressionContext) (antlrgen.IExpressionAtomContext, error) {
	predicated, ok := expression.(*antlrgen.PredicatedExpressionContext)
	if !ok || predicated.Predicate() != nil {
		return nil, fmt.Errorf("expected unpredicated producer expression")
	}
	return predicated.ExpressionAtom(), nil
}

func TestNumericWitnessRejectsWrongStatements(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, query, fn, producer, bits, want string
		args                                  []Scalar
	}{
		{"wrong exponent", "SELECT x, POWER(x, 2) FROM numbers", "POWER", "stored-column", "8000000000000000", "exponent must be integer 3", nil},
		{"missing exponent", "SELECT x, POWER(x) FROM numbers", "POWER", "stored-column", "8000000000000000", "wrong function arity", nil},
		{"real exponent", "SELECT x, POWER(x, 3.0) FROM numbers", "POWER", "stored-column", "8000000000000000", "exponent must be integer 3", nil},
		{"bound exponent", "SELECT x, POWER(x, ?) FROM numbers", "POWER", "stored-column", "8000000000000000", "exponent must be constant 3", nil},
		{"negative exponent", "SELECT x, POWER(x, -3) FROM numbers", "POWER", "stored-column", "8000000000000000", "exponent must be positive integer 3", nil},
		{"integer literal", "SELECT 3, FLOOR(3) FROM numbers", "FLOOR", "literal", "4008000000000000", "DOUBLE lexical form", nil},
		{"extra projection", "SELECT x, x, FLOOR(x) FROM numbers", "FLOOR", "stored-column", "8000000000000000", "projection count", nil},
		{"one argument", "SELECT ?, FLOOR(x) FROM numbers", "FLOOR", "driver-text-transport", "8000000000000000", "argument count", []Scalar{tagged("float64", "8000000000000000")}},
		{"column instead of parameter", "SELECT ?, FLOOR(x) FROM numbers", "FLOOR", "driver-text-transport", "8000000000000000", "not a parameter atom", []Scalar{tagged("float64", "8000000000000000"), tagged("float64", "8000000000000000")}},
		{"args on literal", "SELECT -0.0, FLOOR(-0.0) FROM numbers", "FLOOR", "literal", "8000000000000000", "non-transport producer has arguments", []Scalar{tagged("float64", "8000000000000000")}},
		{"wrong column", "SELECT y, FLOOR(y) FROM numbers", "FLOOR", "stored-column", "8000000000000000", "stored-column identifier", nil},
		{"predicate", "SELECT x, FLOOR(x) IS TRUE FROM numbers", "FLOOR", "stored-column", "8000000000000000", "unpredicated producer", nil},
		{"arithmetic", "SELECT x + 0, FLOOR(x) FROM numbers", "FLOOR", "stored-column", "8000000000000000", "not a column atom", nil},
		{"not a call", "SELECT x, x FROM numbers", "FLOOR", "stored-column", "8000000000000000", "must be a scalar call", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			test := Test{ID: tc.fn + "/" + tc.producer + "/probe", Query: tc.query, Args: tc.args}
			err := witnessNumericProducer(test, tc.producer, tc.bits)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}
