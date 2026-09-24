package recordlayer

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// carryBaseIndex is the stored side of the classification tests: every field
// the comparison reads is set, so a change in any one of them is visible.
func carryBaseIndex() *gen.Index {
	return &gen.Index{
		RecordType:          []string{"T", "U"},
		Name:                proto.String("ix"),
		RootExpression:      Concat(Field("a"), Literal(int32(5))).ToKeyExpression(),
		SubspaceKey:         tuple.Tuple{"ix"}.Pack(),
		LastModifiedVersion: proto.Int32(3),
		Type:                proto.String(IndexTypeValue),
		Options: []*gen.Index_Option{
			{Key: proto.String("k1"), Value: proto.String("v1")},
			{Key: proto.String("k2"), Value: proto.String("v2")},
		},
		AddedVersion: proto.Int32(2),
		Predicate:    &gen.Predicate{ConstantPredicate: &gen.ConstantPredicate{Value: gen.ConstantPredicate_TRUE.Enum()}},
	}
}

// setLegacyIndexType and setValueExpression set Index's deprecated fields,
// which these tests drive because Java still reads them (Index.java:198-218).
func setLegacyIndexType(p *gen.Index, t gen.Index_Type) {
	m := p.ProtoReflect()
	m.Set(m.Descriptor().Fields().ByName("index_type"), protoreflect.ValueOfEnum(t.Number()))
}

func setValueExpression(p *gen.Index, value KeyExpression) {
	m := p.ProtoReflect()
	m.Set(m.Descriptor().Fields().ByName("value_expression"), protoreflect.ValueOfMessage(value.ToKeyExpression().ProtoReflect()))
}

func classify(t *testing.T, stored, rebuilt *gen.Index) (IndexCarryClass, string) {
	t.Helper()
	class, field, err := ClassifyIndexCarry(stored, rebuilt)
	if err != nil {
		t.Fatalf("ClassifyIndexCarry: %v", err)
	}
	return class, field
}

// One field changed at a time, over every field of the Index descriptor: the
// three assigned fields change nothing, the others each make the index CHANGED
// and are named. The table must name every descriptor field, so a field a proto
// sync adds fails here until its class is decided.
func TestClassifyIndexCarry_EachFieldAlone(t *testing.T) {
	t.Parallel()
	cases := map[protoreflect.Name]struct {
		mutate func(*gen.Index)
		want   IndexCarryClass
	}{
		"record_type": {func(p *gen.Index) { p.RecordType = []string{"T"} }, IndexChanged},
		// A legacy INDEX enum is a VALUE index whose options are the enum's (none),
		// so it differs from the base in its options.
		"index_type":            {func(p *gen.Index) { setLegacyIndexType(p, gen.Index_INDEX) }, IndexChanged},
		"name":                  {func(p *gen.Index) { p.Name = proto.String("iy") }, IndexChanged},
		"root_expression":       {func(p *gen.Index) { p.RootExpression = Concat(Field("b"), Literal(int32(5))).ToKeyExpression() }, IndexChanged},
		"subspace_key":          {func(p *gen.Index) { p.SubspaceKey = tuple.Tuple{int64(7)}.Pack() }, IndexEquivalent},
		"last_modified_version": {func(p *gen.Index) { p.LastModifiedVersion = proto.Int32(9) }, IndexEquivalent},
		"value_expression":      {func(p *gen.Index) { setValueExpression(p, Field("v")) }, IndexChanged},
		// Not a type Java wraps in a grouping, which would change the root too.
		"type":          {func(p *gen.Index) { p.Type = proto.String(IndexTypeVersion) }, IndexChanged},
		"options":       {func(p *gen.Index) { p.Options[1].Value = proto.String("other") }, IndexChanged},
		"added_version": {func(p *gen.Index) { p.AddedVersion = nil }, IndexEquivalent},
		"predicate": {func(p *gen.Index) {
			p.Predicate = &gen.Predicate{ConstantPredicate: &gen.ConstantPredicate{Value: gen.ConstantPredicate_FALSE.Enum()}}
		}, IndexChanged},
	}
	// The field whose difference a folded field shows up in.
	namedAs := map[protoreflect.Name]string{
		"index_type":       "options",
		"value_expression": "root_expression",
	}
	fields := (&gen.Index{}).ProtoReflect().Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		name := fields.Get(i).Name()
		c, ok := cases[name]
		if !ok {
			t.Errorf("Index field %s has no case: decide its class and add it", name)
			continue
		}
		t.Run(string(name), func(t *testing.T) {
			rebuilt := carryBaseIndex()
			c.mutate(rebuilt)
			class, field := classify(t, carryBaseIndex(), rebuilt)
			want := ""
			if c.want != IndexEquivalent {
				want = string(name)
				if n, ok := namedAs[name]; ok {
					want = n
				}
			}
			if class != c.want || field != want {
				t.Fatalf("class %s naming %q, want %s naming %q", class, field, c.want, want)
			}
		})
	}
	if len(cases) != fields.Len() {
		t.Errorf("%d cases for %d Index fields", len(cases), fields.Len())
	}
}

// What each side's reading folds, it folds on both: the pairs below are the
// same index as Java's Index(proto) reads it.
func TestClassifyIndexCarry_ReadAsJavaReadsIt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name            string
		stored, rebuilt func() *gen.Index
	}{
		{"legacy UNIQUE enum is type value with unique=true, its stored option list unread", func() *gen.Index {
			p := carryBaseIndex()
			p.Type = nil
			setLegacyIndexType(p, gen.Index_UNIQUE)
			p.Options = []*gen.Index_Option{{Key: proto.String("ignored"), Value: proto.String("x")}}
			return p
		}, func() *gen.Index {
			p := carryBaseIndex()
			p.Options = []*gen.Index_Option{{Key: proto.String(IndexOptionUnique), Value: proto.String("true")}}
			return p
		}},
		{"absent type is value", func() *gen.Index {
			p := carryBaseIndex()
			p.Type = nil
			return p
		}, carryBaseIndex},
		{"option order is not compared", func() *gen.Index {
			p := carryBaseIndex()
			p.Options[0], p.Options[1] = p.Options[1], p.Options[0]
			return p
		}, carryBaseIndex},
		{"value_expression folds into keyWithValue(concat(root, value), root size)", func() *gen.Index {
			p := carryBaseIndex()
			p.RootExpression = Field("a").ToKeyExpression()
			setValueExpression(p, Concat(Field("b"), Field("c")))
			return p
		}, func() *gen.Index {
			p := carryBaseIndex()
			p.RootExpression = KeyWithValue(Concat(Field("a"), Field("b"), Field("c")), 1).ToKeyExpression()
			return p
		}},
		{"a value_expression of no columns leaves the root alone", func() *gen.Index {
			p := carryBaseIndex()
			setValueExpression(p, EmptyKey())
			return p
		}, carryBaseIndex},
		{"a COUNT root that is not a grouping is compared as Java's grouping", func() *gen.Index {
			p := carryBaseIndex()
			p.Type = proto.String(IndexTypeCount)
			p.RootExpression = Field("a").ToKeyExpression()
			return p
		}, func() *gen.Index {
			p := carryBaseIndex()
			p.Type = proto.String(IndexTypeCount)
			// GroupingKeyExpression(root, root's column size): Java's wrap.
			p.RootExpression = &gen.KeyExpression{Grouping: &gen.Grouping{
				WholeKey: Field("a").ToKeyExpression(), GroupedCount: proto.Int32(1),
			}}
			return p
		}},
		{"record types are a set", func() *gen.Index {
			p := carryBaseIndex()
			p.RecordType = []string{"U", "T"}
			return p
		}, carryBaseIndex},
		{"unknown fields and extensions are not compared", func() *gen.Index {
			p := carryBaseIndex()
			var raw []byte
			raw = protowire.AppendTag(raw, 50, protowire.VarintType)
			raw = protowire.AppendVarint(raw, 1)
			raw = protowire.AppendTag(raw, 1500, protowire.BytesType)
			raw = protowire.AppendBytes(raw, []byte("ext"))
			p.ProtoReflect().SetUnknown(raw)
			return p
		}, carryBaseIndex},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if class, field := classify(t, c.stored(), c.rebuilt()); class != IndexEquivalent {
				t.Fatalf("class %s naming %q, want EQUIVALENT", class, field)
			}
		})
	}
}

// The widening arm is the validator's, one way: a stored long_value read against
// a rebuilt int_value of the same number is WIDENED; the reverse, and a
// different number, are CHANGED; a widened root beside another change is CHANGED
// naming that change.
func TestClassifyIndexCarry_LiteralCarrierWidening(t *testing.T) {
	t.Parallel()
	withLiteral := func(lit KeyExpression) *gen.Index {
		p := carryBaseIndex()
		p.RootExpression = Concat(Field("a"), lit).ToKeyExpression()
		return p
	}
	for _, c := range []struct {
		name            string
		stored, rebuilt *gen.Index
		want            IndexCarryClass
		field           string
	}{
		{"long to int", withLiteral(Literal(int64(5))), withLiteral(Literal(int32(5))), IndexWidened, "root_expression"},
		{"int to long", withLiteral(Literal(int32(5))), withLiteral(Literal(int64(5))), IndexChanged, "root_expression"},
		{"long to a different int", withLiteral(Literal(int64(5))), withLiteral(Literal(int32(6))), IndexChanged, "root_expression"},
		{"widened beside a predicate change", withLiteral(Literal(int64(5))), func() *gen.Index {
			p := withLiteral(Literal(int32(5)))
			p.Predicate = nil
			return p
		}(), IndexChanged, "predicate"},
		{"widened beside a record-type change", withLiteral(Literal(int64(5))), func() *gen.Index {
			p := withLiteral(Literal(int32(5)))
			p.RecordType = []string{"T"}
			return p
		}(), IndexChanged, "record_type"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if class, field := classify(t, c.stored, c.rebuilt); class != c.want || field != c.field {
				t.Fatalf("class %s naming %q, want %s naming %q", class, field, c.want, c.field)
			}
		})
	}
}

// A side that does not load is not classified: an absent root is refused with
// Java's text.
func TestClassifyIndexCarry_UnreadableSideIsAnError(t *testing.T) {
	t.Parallel()
	stored := carryBaseIndex()
	stored.RootExpression = nil
	_, _, err := ClassifyIndexCarry(stored, carryBaseIndex())
	var dErr *KeyExpressionDeserializationError
	if !errors.As(err, &dErr) || !strings.Contains(err.Error(), "Exactly one root must be specified for an index") {
		t.Fatalf("err = %v, want the absent-root refusal", err)
	}
	if !strings.HasPrefix(err.Error(), "stored index ix:") {
		t.Fatalf("err = %v, want it to name the stored side", err)
	}
}
