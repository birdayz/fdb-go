//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// The key-function registry against the JVM's (RFC-257 WS-J, ws-j-design.md
// 4d): each core function's argument bounds, column size and null result, and
// FunctionKeyExpression.create's and fromProto's refusals.

// javaCoreKeyFunctions is the measured name set of the target's core registry
// (the factories in com.apple.foundationdb.record.metadata.expressions). Go
// also registers collate_icu, which the target's ICU module
// (fdb-record-layer-icu, not on this JVM's classpath) registers: its
// CollateFunctionKeyExpressionICU extends CollateFunctionKeyExpression, so its
// bounds, column size and null are collate_jre's, compared below.
var javaCoreKeyFunctions = []string{
	"add", "bitand", "bitmap_bit_position", "bitmap_bucket_offset", "bitnot", "bitor", "bitxor",
	"cardinality", "collate_jre", "div", "divide", "mod", "mul", "multiply",
	"order_asc_nulls_first", "order_asc_nulls_last", "order_desc_nulls_first", "order_desc_nulls_last",
	"sub", "subtract",
}

var _ = Describe("RFC-257 key functions: the registry and create's refusals as Java's", func() {
	It("registers every core function with the target's bounds, column size and null", func() {
		var rows []struct {
			Name       string `json:"name"`
			Factory    string `json:"factory"`
			Min        int    `json:"min"`
			Max        int    `json:"max"`
			ColumnSize int    `json:"columnSize"`
			NullResult string `json:"nullResult"`
		}
		Expect(NewJavaInvoker().InvokeAs(context.Background(), "keyFunctionRegistryJava", map[string]any{}, &rows)).To(Succeed())
		var core []string
		for _, r := range rows {
			GinkgoWriter.Printf("KEYFUNCTION %s factory=%s min=%d max=%d columns=%d null=%s\n", r.Name, r.Factory, r.Min, r.Max, r.ColumnSize, r.NullResult)
			if !strings.HasPrefix(r.Factory, "com.apple.foundationdb.record.metadata.expressions.") {
				continue
			}
			core = append(core, r.Name)
			spec, ok := recordlayer.LookupFunction(r.Name)
			Expect(ok).To(BeTrue(), "Go does not register the target's %s", r.Name)
			Expect([]int{spec.MinArguments, spec.MaxArguments, spec.ColumnSize}).To(Equal([]int{r.Min, r.Max, r.ColumnSize}), r.Name)
			Expect(spec.NullIsNonUnique).To(Equal(r.NullResult == "NULL"), "%s returns %s for null arguments", r.Name, r.NullResult)
		}
		sort.Strings(core)
		Expect(core).To(Equal(javaCoreKeyFunctions), "the target's core registry moved")
		icu, ok := recordlayer.LookupFunction("collate_icu")
		Expect(ok).To(BeTrue())
		jre, _ := recordlayer.LookupFunction("collate_jre")
		Expect([]any{icu.MinArguments, icu.MaxArguments, icu.ColumnSize, icu.NullIsNonUnique}).To(
			Equal([]any{jre.MinArguments, jre.MaxArguments, jre.ColumnSize, jre.NullIsNonUnique}))
		_, ok = recordlayer.LookupFunction("get_versionstamp_incarnation")
		Expect(ok).To(BeFalse(), "a key function the target's registry lacks")
	})

	field := func(name string) *gen.KeyExpression {
		return &gen.KeyExpression{Field: &gen.Field{FieldName: proto.String(name), FanType: gen.Field_SCALAR.Enum(), NullInterpretation: gen.Field_NOT_UNIQUE.Enum()}}
	}
	then := func(children ...*gen.KeyExpression) *gen.KeyExpression {
		return &gen.KeyExpression{Then: &gen.Then{Child: children}}
	}
	empty := &gen.KeyExpression{Empty: &gen.Empty{}}
	const (
		invalidExpression = "com.apple.foundationdb.record.metadata.expressions.KeyExpression$InvalidExpressionException"
		deserialization   = "com.apple.foundationdb.record.metadata.expressions.KeyExpression$DeserializationException"
		notDefined        = "Function not defined"
		badArity          = "Invalid number of arguments provided to function"
	)
	for _, c := range []struct {
		name      string
		args      *gen.KeyExpression
		wantError string // Java's create and load message; "" accepts
	}{
		{"add", then(field("order_id"), field("price")), ""},
		{"add", field("order_id"), badArity},
		{"add", then(field("order_id"), field("price"), field("quantity")), badArity},
		{"sub", field("order_id"), ""},
		{"bitnot", then(field("order_id"), field("price")), badArity},
		{"collate_jre", empty, badArity},
		{"collate_jre", then(field("order_id"), field("price"), field("quantity")), ""},
		{"cardinality", then(field("order_id"), field("price")), badArity},
		{"order_desc_nulls_last", then(field("order_id"), field("price")), badArity},
		{"get_versionstamp_incarnation", empty, notDefined},
		{"no_such_function", field("order_id"), notDefined},
	} {
		It(fmt.Sprintf("create and load %s over %d columns", c.name, argColumns(c.args)), func() {
			fn := &gen.Function{Name: proto.String(c.name), Arguments: c.args}
			fnBytes, err := proto.Marshal(fn)
			Expect(err).NotTo(HaveOccurred())
			var java struct {
				CreateClass string `json:"createClass"`
				CreateError string `json:"createError"`
				LoadClass   string `json:"loadClass"`
				LoadError   string `json:"loadError"`
			}
			Expect(NewJavaInvoker().InvokeAs(context.Background(), "functionKeyExpressionVerdictsJava", map[string]any{
				"functionProto": BytesToIntArray(fnBytes),
			}, &java)).To(Succeed())
			GinkgoWriter.Printf("FUNCTIONVERDICT %s create=%s %q load=%s %q\n", c.name, java.CreateClass, java.CreateError, java.LoadClass, java.LoadError)
			if c.wantError == "" {
				Expect([]string{java.CreateClass, java.LoadClass}).To(Equal([]string{"", ""}))
			} else {
				Expect([]string{java.CreateClass, java.CreateError}).To(Equal([]string{invalidExpression, c.wantError}))
				Expect([]string{java.LoadClass, java.LoadError}).To(Equal([]string{deserialization, c.wantError}))
			}

			// In code: Build returns the refusal create threw.
			args, err := recordlayer.KeyExpressionFromProto(c.args)
			Expect(err).NotTo(HaveOccurred())
			builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
			builder.AddIndex("Order", recordlayer.NewIndex("fn_idx", recordlayer.FunctionExpr(c.name, args)))
			_, buildErr := builder.Build()
			var keyErr *recordlayer.KeyExpressionError
			if c.wantError == "" {
				Expect(errors.As(buildErr, &keyErr) && (keyErr.Message == notDefined || keyErr.Message == badArity)).To(BeFalse(), "%v", buildErr)
			} else {
				Expect(errors.As(buildErr, &keyErr)).To(BeTrue(), "%v", buildErr)
				Expect(keyErr.Message).To(Equal(c.wantError))
			}

			// Loaded: an arity refusal is Java's; an unknown name loads in Go
			// only (DIVERGENCES.md, a function only a Java module registers).
			_, loadErr := recordlayer.KeyExpressionFromProto(&gen.KeyExpression{Function: fn})
			switch c.wantError {
			case "", notDefined:
				Expect(loadErr).NotTo(HaveOccurred())
			default:
				var deserErr *recordlayer.KeyExpressionDeserializationError
				Expect(errors.As(loadErr, &deserErr)).To(BeTrue(), "%v", loadErr)
				Expect(deserErr.Message).To(Equal(c.wantError))
			}
		})
	}
})

func argColumns(e *gen.KeyExpression) int {
	switch {
	case e.GetEmpty() != nil:
		return 0
	case e.GetThen() != nil:
		return len(e.GetThen().GetChild())
	default:
		return 1
	}
}
