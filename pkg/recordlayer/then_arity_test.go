package recordlayer

import (
	"errors"
	"testing"

	"fdb.dev/gen"
)

// TestThenOfFewerThanTwoChildrenIsRefusedWhereJavaThrows pins Java's refusal of
// a Then of fewer than two children (ThenKeyExpression.java:63-65), a
// RecordCoreException thrown where the Then is built. Go's Concat cannot throw,
// so Build returns it as the builder fault recorded at the Concat, in program
// order with every other builder fault. Without it Go stored a one-child Then
// that neither engine's loader reads back ("Then must have at least 2
// children", on load).
func TestThenOfFewerThanTwoChildrenIsRefusedWhereJavaThrows(t *testing.T) {
	t.Parallel()

	const want = "Then must have at least 2 children"
	base := func() *RecordMetaDataBuilder {
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return b
	}
	refused := func(t *testing.T, err error) {
		t.Helper()
		var rc *RecordCoreError
		if !errors.As(err, &rc) || rc.Message != want {
			t.Fatalf("Build: %v (%T), want the RecordCoreException %q", err, err, want)
		}
	}

	for _, c := range []struct {
		name  string
		build func() *RecordMetaDataBuilder
	}{
		{"a one-child primary key", func() *RecordMetaDataBuilder {
			b := base()
			b.GetRecordType("Order").SetPrimaryKey(Concat(Field("order_id")))
			return b
		}},
		{"an empty primary key", func() *RecordMetaDataBuilder {
			b := base()
			b.GetRecordType("Order").SetPrimaryKey(Concat())
			return b
		}},
		{"a one-child index root", func() *RecordMetaDataBuilder {
			return base().AddIndex("Order", NewIndex("idx", Concat(Field("price"))))
		}},
		{"inside a grouping", func() *RecordMetaDataBuilder {
			return base().AddIndex("Order", NewCountIndex("idx", GroupAll(Concat(Field("price")))))
		}},
		{"grouped by a one-child Then", func() *RecordMetaDataBuilder {
			return base().AddIndex("Order", NewSumIndex("idx", GroupBy(Field("quantity"), Concat(Field("price")))))
		}},
		{"a one-child Then inside a nesting", func() *RecordMetaDataBuilder {
			return base().AddIndex("Order", NewIndex("idx", Nest("flower", Concat(Field("type")))))
		}},
		{"flattened into a Then of two", func() *RecordMetaDataBuilder {
			return base().AddIndex("Order", NewIndex("idx", Concat(Concat(Field("price")), Field("quantity"))))
		}},
		{"on a universal index", func() *RecordMetaDataBuilder {
			return base().AddUniversalIndex(NewIndex("idx", Concat(Field("price"))))
		}},
		{"a one-child record count key", func() *RecordMetaDataBuilder {
			return base().SetRecordCountKey(Concat(RecordTypeKey()))
		}},
		// Java threw at the concat, so a later set cannot undo it.
		{"a one-child primary key replaced by a good one", func() *RecordMetaDataBuilder {
			b := base()
			b.GetRecordType("Order").SetPrimaryKey(Concat(Field("order_id")))
			b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			return b
		}},
		{"a one-child record count key replaced by a good one", func() *RecordMetaDataBuilder {
			return base().SetRecordCountKey(Concat(RecordTypeKey())).SetRecordCountKey(RecordTypeKey())
		}},
		// The Then is built before GetRecordType's unknown-type fault, and the
		// key lands on the placeholder type the builder never holds.
		{"handed to an unknown record type after it was built", func() *RecordMetaDataBuilder {
			b := base()
			key := Concat(Field("order_id"))
			b.GetRecordType("Nope").SetPrimaryKey(key)
			return b
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := c.build().Build()
			refused(t, err)
		})
	}

	t.Run("a Then of two builds", func(t *testing.T) {
		t.Parallel()
		if _, err := base().AddIndex("Order", NewIndex("idx", Concat(Field("price"), Field("quantity")))).Build(); err != nil {
			t.Fatalf("Build: %v", err)
		}
	})

	// Program order: the Java program dies at whichever throw comes first.
	t.Run("an earlier builder fault is the one reported", func(t *testing.T) {
		t.Parallel()
		b := base()
		b.AddIndex("Nope", NewIndex("first", Field("price")))
		b.AddIndex("Order", NewIndex("idx", Concat(Field("price"))))
		_, err := b.Build()
		var md *MetaDataError
		if !errors.As(err, &md) || md.Message != "Unknown record type Nope" {
			t.Fatalf("Build: %v (%T), want the earlier MetaDataException", err, err)
		}
	})
	t.Run("a Then built before a later builder fault is the one reported", func(t *testing.T) {
		t.Parallel()
		b := base()
		root := Concat(Field("price"))
		b.AddIndex("Nope", NewIndex("later", Field("price")))
		b.AddIndex("Order", NewIndex("idx", root))
		_, err := b.Build()
		refused(t, err)
	})
}

// TestAddMultiTypeIndexResolvesItsRecordTypesFirst pins Java's order: a Java
// program resolves every record type (getRecordType, RecordMetaDataBuilder.java:
// 986-996) before it calls addMultiTypeIndex (:1177), so a duplicate index name
// beside an unknown record type is the unknown type's fault, and the index is
// not added.
func TestAddMultiTypeIndexResolvesItsRecordTypesFirst(t *testing.T) {
	t.Parallel()
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	b.AddIndex("Order", NewIndex("dup", Field("price")))
	b.AddMultiTypeIndex([]string{"Order", "Nope"}, NewIndex("dup", Field("price")))
	_, err := b.Build()
	var md *MetaDataError
	if !errors.As(err, &md) || md.Message != "Unknown record type Nope" {
		t.Fatalf("Build = %v (%T), want the unknown record type, not the duplicate", err, err)
	}

	b = NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	idx := NewIndex("multi", Field("price"))
	b.AddMultiTypeIndex([]string{"Nope", "Order"}, idx)
	if got := b.GetRecordTypes()["Order"].multiTypeIndexes; len(got) != 0 {
		t.Fatalf("a refused multi-type index was added to Order: %v", got)
	}
}

// TestTextIndexWithAnEmptyTokenizerNameIsRefused pins Java's reading of an
// empty tokenizer name: Index.getOption returns "" for it, not null, and the
// registry knows no tokenizer "" (TextTokenizerRegistryImpl.java:94-104), so
// it is "unrecognized text tokenizer"; an absent name is the default.
func TestTextIndexWithAnEmptyTokenizerNameIsRefused(t *testing.T) {
	t.Parallel()
	build := func(options map[string]string) error {
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		idx := NewTextIndex("text", Field("name"))
		for k, v := range options {
			idx.Options[k] = v
		}
		b.AddIndex("Customer", idx)
		_, err := b.Build()
		return err
	}
	if err := build(nil); err != nil {
		t.Fatalf("absent tokenizer name: %v", err)
	}
	var md *MetaDataError
	if err := build(map[string]string{IndexOptionTextTokenizerName: ""}); !errors.As(err, &md) || md.Message != "unrecognized text tokenizer" {
		t.Fatalf("empty tokenizer name: %v (%T), want Java's MetaDataException", err, err)
	}
}
