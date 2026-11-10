package recordlayer

import (
	"errors"
	"math"
	"reflect"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

func TestPartitionKeyLayoutLogicalAndPhysical(t *testing.T) {
	t.Parallel()
	for _, sourceFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "ID_SOURCE", true: "SOURCE_ID"}[sourceFirst], func(t *testing.T) {
			t.Parallel()
			b := partitionTestBuilder()
			logical := []KeyExpression{Field("order_id"), Field("coord_x")}
			if sourceFirst {
				logical[0], logical[1] = logical[1], logical[0]
			}
			b.GetRecordType("Order").SetRecordTypeKey("ORDERS").SetPrimaryKey(Concat(append([]KeyExpression{RecordTypeKey(), PartitionExpr(Field("coord_x"), 10)}, logical...)...))
			md, err := b.Build()
			if err != nil {
				t.Fatal(err)
			}
			rt := md.GetRecordType("Order")
			layout, err := DescribePartitionKey(rt)
			if err != nil || layout == nil {
				t.Fatalf("layout = %v, %v", layout, err)
			}
			if layout.PhysicalPrimaryKey() != rt.PrimaryKey || !proto.Equal(layout.LogicalPrimaryKey().ToKeyExpression(), Concat(logical...).ToKeyExpression()) {
				t.Fatal("lost physical/logical AST")
			}
			if got := layout.LogicalToPhysicalOrdinals(); !reflect.DeepEqual(got, []int{2, 3}) {
				t.Fatalf("logical positions = %v", got)
			}
			components := layout.Components()
			if len(components) != 4 {
				t.Fatalf("components = %v", components)
			}
			for i, component := range components {
				wantKind, wantLogical := PartitionKeyDeclared, i-2
				if i == 0 {
					wantKind, wantLogical = PartitionKeyRecordType, -1
				}
				if i == 1 {
					wantKind, wantLogical = PartitionKeyCoordinate, -1
				}
				if component.Kind != wantKind || component.PhysicalOrdinal != i || component.LogicalOrdinal != wantLogical || !keyExpressionEquals(component.Expression, NormalizeKeyForPositions(rt.PrimaryKey)[i]) {
					t.Fatalf("component %d = %+v", i, component)
				}
			}
			components[0].Kind = PartitionKeyDeclared
			if layout.Components()[0].Kind != PartitionKeyRecordType {
				t.Fatal("caller mutated layout")
			}
			wantSource := 1
			if sourceFirst {
				wantSource = 0
			}
			if layout.SourceLogicalOrdinal() != wantSource || layout.SourceField().Number() != 6 {
				t.Fatalf("source ordinal/number = %d/%d", layout.SourceLogicalOrdinal(), layout.SourceField().Number())
			}
			for _, tc := range []struct{ x, p int64 }{{-11, -2}, {-10, -1}, {-1, -1}, {0, 0}, {9, 0}, {10, 1}, {21, 2}, {math.MinInt64, -922337203685477581}, {math.MaxInt64, 922337203685477580}} {
				key := tuple.Tuple{int64(7), tc.x}
				if sourceFirst {
					key[0], key[1] = key[1], key[0]
				}
				physical, err := layout.PhysicalKey(key)
				want := append(tuple.Tuple{"ORDERS", tc.p}, key...)
				if err != nil || !reflect.DeepEqual(physical, want) {
					t.Fatalf("route %v = %v, %v; want %v", key, physical, err, want)
				}
				physical[2] = int64(999)
				if key[0] == int64(999) {
					t.Fatal("physical tuple aliases input")
				}
			}
			for _, key := range []tuple.Tuple{nil, {int64(1)}, {int64(1), int64(2), int64(3)}, {nil, int64(1)}, {int64(1), nil}, {"bad", "bad"}, {int32(1), int32(2)}} {
				_, err := layout.PhysicalKey(key)
				var routing *PartitionRoutingError
				if !errors.As(err, &routing) {
					t.Fatalf("bad key %v: %v", key, err)
				}
			}
			if plain, err := DescribePartitionKey(md.GetRecordType("Customer")); plain != nil || err != nil {
				t.Fatalf("plain layout = %v, %v", plain, err)
			}
		})
	}
}

func TestPartitionIndexLogicalProjectionAndPhysicalOrdinals(t *testing.T) {
	t.Parallel()
	b := partitionTestBuilder()
	p := PartitionExpr(Field("coord_x"), 10)
	b.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), p, Field("order_id"), Field("coord_x")))
	b.AddIndex("Order", NewIndex("value", Concat(p, Field("order_id"))))
	b.AddIndex("Order", NewIndex("cover", KeyWithValue(Concat(p, Field("order_id"), Field("price")), 2)))
	b.AddIndex("Order", NewIndex("cover_key_only_partition", KeyWithValue(Concat(p, Field("price")), 1)))
	b.AddIndex("Order", NewIndex("nested", Concat(p, Nest("flower", Concat(Field("type"), Field("color"))))))
	b.AddIndex("Order", NewIndex("unique", Concat(p, Field("coord_x"), Literal(int64(42)))).SetUnique())
	b.AddIndex("Order", NewTextIndex("text", GroupBy(Nest("flower", Field("type")), p, Field("quantity"))))
	b.AddIndex("Order", NewTextIndex("text_only_partition", GroupBy(Nest("flower", Field("type")), p)))
	md, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	layout, err := DescribePartitionKey(md.GetRecordType("Order"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		logical  KeyExpression
		ordinals []int
	}{
		{"value", Field("order_id"), []int{2, 0, 1, 3}},
		{"cover", KeyWithValue(Concat(Field("order_id"), Field("price")), 1), []int{2, 0, 1, 3}},
		{"cover_key_only_partition", KeyWithValue(Field("price"), 0), []int{1, 0, 2, 3}},
		{"nested", Nest("flower", Concat(Field("type"), Field("color"))), []int{3, 0, 4, 5}},
		{"unique", Concat(Field("coord_x"), Literal(int64(42))), []int{3, 0, 4, 1}},
		{"text", GroupBy(Nest("flower", Field("type")), Field("quantity")), []int{3, 0, 4, 5}},
		{"text_only_partition", Ungrouped(Nest("flower", Field("type"))), []int{2, 0, 3, 4}},
	} {
		projection, err := layout.ProjectIndex(md.GetIndex(tc.name))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !proto.Equal(projection.LogicalExpression().ToKeyExpression(), tc.logical.ToKeyExpression()) {
			t.Fatalf("%s projection = %v, want %v", tc.name, projection.LogicalExpression().ToKeyExpression(), tc.logical.ToKeyExpression())
		}
		if projection.PhysicalIndex() != md.GetIndex(tc.name) {
			t.Fatal("replaced physical index")
		}
		if got := projection.PrimaryKeyEntryOrdinals(); !reflect.DeepEqual(got, tc.ordinals) {
			t.Fatalf("%s entry positions = %v, want %v", tc.name, got, tc.ordinals)
		}
		if got := projection.LogicalPrimaryKeyEntryOrdinals(); !reflect.DeepEqual(got, tc.ordinals[2:]) {
			t.Fatalf("%s logical PK entry positions = %v", tc.name, got)
		}
		components := projection.Components()
		if len(components) != len(NormalizeKeyForPositions(md.GetIndex(tc.name).RootExpression)) || components[0].Kind != PartitionKeyCoordinate || components[0].LogicalOrdinal != -1 {
			t.Fatalf("%s physical index components = %v", tc.name, components)
		}
		for i, component := range components {
			if component.PhysicalOrdinal != i || component.LogicalOrdinal != i-1 || i > 0 && component.Kind != PartitionKeyDeclared {
				t.Fatalf("%s component %d = %+v", tc.name, i, component)
			}
		}
		components[0].Kind = PartitionKeyDeclared
		if projection.Components()[0].Kind != PartitionKeyCoordinate {
			t.Fatal("caller mutated index components")
		}
		positions := projection.PrimaryKeyEntryOrdinals()
		positions[0] = 999
		if projection.PrimaryKeyEntryOrdinals()[0] == 999 {
			t.Fatal("caller mutated entry mapping")
		}
	}
	if !md.GetIndex("unique").IsUnique() {
		t.Fatal("lost unique metadata")
	}
}

func TestPartitionKeyLayoutRejectsMalformedMetadata(t *testing.T) {
	t.Parallel()
	md, err := partitionTestBuilder().Build()
	if err != nil {
		t.Fatal(err)
	}
	rt := *md.GetRecordType("Order")
	for _, key := range []KeyExpression{
		Concat(PartitionExpr(Field("coord_x"), 10), Field("coord_x")),
		Concat(RecordTypeKey(), Field("order_id"), PartitionExpr(Field("coord_x"), 10), Field("coord_x")),
		Concat(RecordTypeKey(), PartitionExpr(Field("coord_x"), 0), Field("coord_x")),
		Concat(RecordTypeKey(), PartitionExpr(Field("price"), 10), Field("price")),
		Concat(RecordTypeKey(), FunctionExpr(FunctionNamePartitionV1, Concat(Field("coord_x"), Literal(int64(10)))), Field("coord_x")),
	} {
		rt.PrimaryKey = key
		layout, err := DescribePartitionKey(&rt)
		var typed *PartitionMetadataError
		if layout != nil || !errors.As(err, &typed) {
			t.Fatalf("malformed PK downgraded: %v, %v", layout, err)
		}
	}
	if _, err := DescribePartitionKey(nil); err == nil {
		t.Fatal("nil record type accepted")
	}
	layout, err := DescribePartitionKey(md.GetRecordType("Order"))
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []*Index{
		nil,
		NewIndex("no_prefix", Field("price")),
		NewIndex("wrong_width", Concat(PartitionExpr(Field("coord_x"), 20), Field("price"))),
		NewIndex("invalid_inherited_argument", Concat(FunctionExpr(FunctionNamePartitionV1, Concat(FieldWithNullStandin("coord_x", FanTypeNone, NullStandinNotNull), Literal(int64(10)))), Field("price"))),
	} {
		_, err := layout.ProjectIndex(index)
		var typed *PartitionMetadataError
		if !errors.As(err, &typed) {
			t.Fatalf("malformed index = %v", err)
		}
	}
	for _, positions := range [][]int{{0}, {-1, 3, -1, -1}, {-1, -2, -1, -1}, {0, 0, -1, -1}} {
		idx := NewIndex("bad_mapping", Concat(PartitionExpr(Field("coord_x"), 10), Field("price")))
		idx.primaryKeyComponentPositions = positions
		_, err := layout.ProjectIndex(idx)
		var typed *PartitionMetadataError
		if !errors.As(err, &typed) {
			t.Fatalf("mapping %v: %v", positions, err)
		}
	}
	idx := NewIndex("untrimmed", Concat(PartitionExpr(Field("coord_x"), 10), Field("price")))
	projection, err := layout.ProjectIndex(idx)
	if err != nil || !reflect.DeepEqual(projection.PrimaryKeyEntryOrdinals(), []int{2, 3, 4, 5}) {
		t.Fatalf("untrimmed mapping = %v, %v", projection, err)
	}
	if !keyExpressionEquals(partitionLogicalConcat(nil), EmptyKey()) {
		t.Fatal("empty projection is not EmptyKey")
	}
	plainWithPartitionIndex := *md.GetRecordType("Customer")
	plainWithPartitionIndex.indexes = []*Index{NewIndex("bad_plain", Concat(PartitionExpr(Field("customer_id"), 10), Field("name")))}
	var malformed *PartitionMetadataError
	if layout, err := DescribePartitionKey(&plainWithPartitionIndex); layout != nil || !errors.As(err, &malformed) {
		t.Fatalf("partition index downgraded to plain: %v, %v", layout, err)
	}
	missingDescriptor := *md.GetRecordType("Order")
	missingDescriptor.Descriptor = nil
	if _, err := DescribePartitionKey(&missingDescriptor); err == nil {
		t.Fatal("missing partition descriptor accepted")
	}
	for _, empty := range []*PartitionKeyLayout{nil, {}} {
		var typed *PartitionMetadataError
		if _, err := empty.PhysicalKey(nil); !errors.As(err, &typed) {
			t.Fatalf("uninitialized physical-key layout: %v", err)
		}
		if _, err := empty.ProjectIndex(idx); !errors.As(err, &typed) {
			t.Fatalf("uninitialized index layout: %v", err)
		}
	}
	if (&PartitionKeyLayout{}).LogicalToPhysicalOrdinals() != nil || (&PartitionIndexLayout{}).LogicalPrimaryKeyEntryOrdinals() != nil {
		t.Fatal("uninitialized mappings must be empty")
	}
}

func TestPartitionLogicalSuffixKeepsSurvivingTrees(t *testing.T) {
	t.Parallel()
	nested := Nest("flower", Concat(Field("type"), Field("color")))
	for _, tc := range []struct {
		expression KeyExpression
		count      int
		want       KeyExpression
		valid      bool
	}{
		{nested, 0, nested, true},
		{Field("order_id"), -1, nil, false},
		{Field("order_id"), 2, nil, false},
		{Field("order_id"), 1, EmptyKey(), true},
		{Concat(Field("order_id"), Field("coord_x")), 2, EmptyKey(), true},
		{nested, 1, nil, false},
		{Concat(Field("coord_x"), nested), 2, nil, false},
		{GroupBy(Field("price"), Field("coord_x")), 2, nil, false},
		{GroupBy(Field("price"), nested), 1, nil, false},
		{KeyWithValue(nested, 2), 1, nil, false},
		{KeyWithValue(Concat(Field("coord_x"), nested), 1), 1, KeyWithValue(nested, 0), true},
	} {
		got, valid := partitionLogicalSuffix(tc.expression, tc.count)
		if valid != tc.valid || valid && !keyExpressionEquals(got, tc.want) {
			t.Fatalf("suffix(%v, %d) = %v/%v, want %v/%v", tc.expression.ToKeyExpression(), tc.count, got, valid, tc.want, tc.valid)
		}
	}
}
