package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// buildMetaWith builds a demo metadata with customizable PK / index /
// version so diff tests can target one axis at a time.
func buildMetaWith(t *testing.T, opts ...func(*recordlayer.RecordMetaDataBuilder)) *recordlayer.RecordMetaData {
	t.Helper()
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	for _, o := range opts {
		o(b)
	}
	md, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return md
}

func TestDiff_Identical(t *testing.T) {
	t.Parallel()
	m1 := buildMetaWith(t)
	m2 := buildMetaWith(t)
	var buf bytes.Buffer
	if err := writeMetaDiff(&buf, m1, m2); err != nil {
		t.Fatalf("writeMetaDiff: %v", err)
	}
	if !strings.Contains(buf.String(), "identical") {
		t.Errorf("expected 'identical' in output, got:\n%s", buf.String())
	}
}

func TestDiff_IndexAdded(t *testing.T) {
	t.Parallel()
	m1 := buildMetaWith(t)
	m2 := buildMetaWith(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
	})
	var buf bytes.Buffer
	if err := writeMetaDiff(&buf, m1, m2); err != nil {
		t.Fatalf("writeMetaDiff: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "INDEXES:") {
		t.Errorf("missing INDEXES section:\n%s", got)
	}
	if !strings.Contains(got, "+ Order$price") {
		t.Errorf("missing '+ Order$price':\n%s", got)
	}
}

func TestDiff_IndexRemoved(t *testing.T) {
	t.Parallel()
	m1 := buildMetaWith(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
	})
	m2 := buildMetaWith(t)
	var buf bytes.Buffer
	if err := writeMetaDiff(&buf, m1, m2); err != nil {
		t.Fatalf("writeMetaDiff: %v", err)
	}
	if !strings.Contains(buf.String(), "- Order$price") {
		t.Errorf("missing '- Order$price':\n%s", buf.String())
	}
}

func TestDiff_IndexTypeChanged(t *testing.T) {
	t.Parallel()
	m1 := buildMetaWith(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
	})
	m2 := buildMetaWith(t, func(b *recordlayer.RecordMetaDataBuilder) {
		// count index requires grouping — Ungrouped() wraps EmptyKeyExpression
		// into a GroupingKeyExpression so the builder accepts it.
		idx := recordlayer.NewCountIndex("Order$price", recordlayer.Ungrouped(&recordlayer.EmptyKeyExpression{}))
		b.AddIndex("Order", idx)
	})
	var buf bytes.Buffer
	if err := writeMetaDiff(&buf, m1, m2); err != nil {
		t.Fatalf("writeMetaDiff: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "~ Order$price") || !strings.Contains(got, "type value -> count") {
		t.Errorf("expected ~ line with type change, got:\n%s", got)
	}
}

func TestDiff_VersionChange(t *testing.T) {
	t.Parallel()
	m1 := buildMetaWith(t)
	m2 := buildMetaWith(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.SetVersion(42)
	})
	var buf bytes.Buffer
	if err := writeMetaDiff(&buf, m1, m2); err != nil {
		t.Fatalf("writeMetaDiff: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "VERSION:") || !strings.Contains(got, "-> 42") {
		t.Errorf("expected VERSION change line, got:\n%s", got)
	}
}

func TestDiffJSON_IdenticalProducesEmptySections(t *testing.T) {
	t.Parallel()
	m1 := buildMetaWith(t)
	m2 := buildMetaWith(t)
	var buf bytes.Buffer
	if err := writeMetaDiffJSON(&buf, m1, m2); err != nil {
		t.Fatalf("writeMetaDiffJSON: %v", err)
	}
	var d map[string]any
	if err := json.Unmarshal(buf.Bytes(), &d); err != nil {
		t.Fatalf("decode: %v\nraw:\n%s", err, buf.String())
	}
	// version key omitted when unchanged (omitempty).
	if _, hasVersion := d["version"]; hasVersion {
		t.Errorf("version key should be omitted when unchanged; got %v", d["version"])
	}
	// Both sections present with empty arrays — scripts can do `.added | length`
	// without null-guards. Crucially NOT nil — the encoder should emit `[]`.
	for _, section := range []string{"record_types", "indexes"} {
		bucket, ok := d[section].(map[string]any)
		if !ok {
			t.Fatalf("missing %s section:\n%s", section, buf.String())
		}
		for _, k := range []string{"added", "removed", "changed"} {
			arr, ok := bucket[k].([]any)
			if !ok {
				t.Errorf("%s.%s not an array (nil would be a scripting footgun):\n%s",
					section, k, buf.String())
			}
			if len(arr) != 0 {
				t.Errorf("%s.%s = %v; want empty", section, k, arr)
			}
		}
	}
}

func TestDiffJSON_IndexAddedAndRemoved(t *testing.T) {
	t.Parallel()
	m1 := buildMetaWith(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
	})
	m2 := buildMetaWith(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.AddIndex("Order", recordlayer.NewIndex("Order$quantity", recordlayer.Field("quantity")))
	})

	var buf bytes.Buffer
	if err := writeMetaDiffJSON(&buf, m1, m2); err != nil {
		t.Fatalf("writeMetaDiffJSON: %v", err)
	}
	var d map[string]any
	if err := json.Unmarshal(buf.Bytes(), &d); err != nil {
		t.Fatalf("decode JSON: %v\nraw:\n%s", err, buf.String())
	}
	idx, ok := d["indexes"].(map[string]any)
	if !ok {
		t.Fatalf("missing indexes key:\n%s", buf.String())
	}
	// added — objects with {name, detail}, mirroring the text output.
	added, _ := idx["added"].([]any)
	if len(added) != 1 {
		t.Fatalf("indexes.added = %v; want one entry", added)
	}
	addedObj, _ := added[0].(map[string]any)
	if addedObj["name"] != "Order$quantity" {
		t.Errorf("indexes.added[0].name = %v; want Order$quantity", addedObj["name"])
	}
	if detail, _ := addedObj["detail"].(string); !strings.Contains(detail, "quantity") {
		t.Errorf("indexes.added[0].detail = %q; want the indexed fields", detail)
	}
	// removed — names only.
	removed, _ := idx["removed"].([]any)
	if len(removed) != 1 || removed[0] != "Order$price" {
		t.Errorf("indexes.removed = %v; want [Order$price]", removed)
	}
}

func TestDiffJSON_VersionBumpEmitsVersion(t *testing.T) {
	t.Parallel()
	m1 := buildMetaWith(t)
	m2 := buildMetaWith(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.SetVersion(7)
	})
	var buf bytes.Buffer
	if err := writeMetaDiffJSON(&buf, m1, m2); err != nil {
		t.Fatalf("writeMetaDiffJSON: %v", err)
	}
	var d map[string]any
	if err := json.Unmarshal(buf.Bytes(), &d); err != nil {
		t.Fatalf("decode JSON: %v\nraw:\n%s", err, buf.String())
	}
	v, ok := d["version"].(map[string]any)
	if !ok {
		t.Fatalf("missing version key:\n%s", buf.String())
	}
	if v["new"].(float64) != 7 {
		t.Errorf("version.new = %v; want 7", v["new"])
	}
}

// TestMetaDiffCmd_TextEndToEnd drives the full cobra command on two
// metadata files. Without a command-level test the only coverage comes
// from writeMetaDiff / writeMetaDiffJSON unit tests — flag wiring,
// argument parsing, and the file-load path stay unexercised.
func TestMetaDiffCmd_TextEndToEnd(t *testing.T) {
	t.Parallel()
	oldPath := writeDiffMetaFile(t, 0)
	newPath := writeDiffMetaFile(t, 0, func(b *recordlayer.RecordMetaDataBuilder) {
		b.AddIndex("Order", recordlayer.NewIndex("Order$added", recordlayer.Field("price")))
	})

	c := newMetaDiffCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{oldPath, newPath})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v\nout:\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{"INDEXES:", "+ Order$added"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

// TestMetaDiffCmd_JSONEndToEnd locks in the -o json contract via the
// command path. Script consumers key off `.indexes.added` etc.
func TestMetaDiffCmd_JSONEndToEnd(t *testing.T) {
	t.Parallel()
	oldPath := writeDiffMetaFile(t, 0)
	newPath := writeDiffMetaFile(t, 0, func(b *recordlayer.RecordMetaDataBuilder) {
		b.AddIndex("Order", recordlayer.NewIndex("Order$added", recordlayer.Field("price")))
	})

	c := newMetaDiffCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{oldPath, newPath, "-o", "json"})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v\nout:\n%s", err, out.String())
	}
	var d map[string]any
	if err := json.Unmarshal(out.Bytes(), &d); err != nil {
		t.Fatalf("decode: %v\nraw:\n%s", err, out.String())
	}
	idx, ok := d["indexes"].(map[string]any)
	if !ok {
		t.Fatalf("missing indexes section:\n%s", out.String())
	}
	added, _ := idx["added"].([]any)
	if len(added) != 1 {
		t.Fatalf("indexes.added = %v; want one entry", added)
	}
	if obj, _ := added[0].(map[string]any); obj["name"] != "Order$added" {
		t.Errorf("indexes.added[0].name = %v; want Order$added", added[0])
	}
}

// TestMetaDiffCmd_RejectsInvalidOutput — output validator is the last
// line of defence against typo'd flag values; verify the command path
// uses it rather than falling through to "text" silently.
func TestMetaDiffCmd_RejectsInvalidOutput(t *testing.T) {
	t.Parallel()
	p := writeDiffMetaFile(t, 0)
	c := newMetaDiffCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{p, p, "-o", "yaml"}) // diff does not support yaml
	if err := c.Execute(); err == nil {
		t.Fatal("expected error for -o yaml")
	}
}

// writeDiffMetaFile exports a metadata snapshot (with optional tweaks)
// to a temp file for end-to-end meta diff tests.
func writeDiffMetaFile(t *testing.T, version int32, opts ...func(*recordlayer.RecordMetaDataBuilder)) string {
	t.Helper()
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	if version != 0 {
		b.SetVersion(int(version))
	}
	for _, o := range opts {
		o(b)
	}
	md, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	path := filepath.Join(t.TempDir(), "meta.pb")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := recordlayer.WriteRecordMetaData(md, f); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// TestDiff_RecordTypePKChange exercises the RECORD TYPES section of
// the text renderer — previously only index changes were exercised,
// so the entire record-types branch in writeMetaDiff (roughly
// 6 statements) was uncovered. PK change on an existing type produces
// a `~ Name: pk changed (old -> new)` line.
func TestDiff_RecordTypePKChange(t *testing.T) {
	t.Parallel()
	m1 := buildMetaWith(t) // Order has pk=order_id by default
	m2 := buildMetaWith(t, func(b *recordlayer.RecordMetaDataBuilder) {
		// Compose with a real field on the Order proto so the builder
		// validates. Quantity is a plain int32 on Order.
		b.GetRecordType("Order").SetPrimaryKey(
			recordlayer.Concat(recordlayer.Field("order_id"),
				recordlayer.Field("quantity")))
	})

	var buf bytes.Buffer
	if err := writeMetaDiff(&buf, m1, m2); err != nil {
		t.Fatalf("writeMetaDiff: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "RECORD TYPES:") {
		t.Errorf("output missing RECORD TYPES section:\n%s", got)
	}
	// The change line must include the old and new PKs so reviewers
	// can spot compatibility breaks at a glance.
	if !strings.Contains(got, "~ Order") ||
		!strings.Contains(got, "order_id") ||
		!strings.Contains(got, "quantity") {
		t.Errorf("expected ~ Order line with both PK fields, got:\n%s", got)
	}
}

// TestDiffIndexes_MultipleSortedAlphabetically proves the name-sort
// comparators inside sortSection actually run. Prior tests diffed
// one-entry buckets, so Go's sort.Slice short-circuited without
// invoking the lambda. If the comparator is ever flipped or removed,
// the assertion on alphabetical ordering across three additions fires.
func TestDiffIndexes_MultipleSortedAlphabetically(t *testing.T) {
	t.Parallel()
	m1 := buildMetaWith(t)
	m2 := buildMetaWith(t, func(b *recordlayer.RecordMetaDataBuilder) {
		// Added in non-alphabetical order so the test fails if sortSection
		// accidentally preserves insertion order.
		b.AddIndex("Order", recordlayer.NewIndex("Order$zeta", recordlayer.Field("price")))
		b.AddIndex("Order", recordlayer.NewIndex("Order$alpha", recordlayer.Field("price")))
		b.AddIndex("Order", recordlayer.NewIndex("Order$mu", recordlayer.Field("price")))
	})
	s := diffIndexes(m1, m2)
	if len(s.Added) != 3 {
		t.Fatalf("Added len = %d; want 3", len(s.Added))
	}
	want := []string{"Order$alpha", "Order$mu", "Order$zeta"}
	for i, w := range want {
		if s.Added[i].Name != w {
			t.Errorf("Added[%d] = %q; want %q (sort broken?)", i, s.Added[i].Name, w)
		}
	}
}

// TestPKFieldsOrUnset covers the three branches of the PK→text helper
// that diff + evolve-check share. Each branch produces the operator-
// visible string that shows up in both text and JSON output — a silent
// regression in the "(unset)" fallback would misreport PK adds /
// removes as bogus "pk changed (field -> field)" lines.
func TestPKFieldsOrUnset(t *testing.T) {
	t.Parallel()

	// nil expression → the sentinel operators look for.
	if got := pkFieldsOrUnset(nil); got != "(unset)" {
		t.Errorf("nil → %q; want (unset)", got)
	}

	// Non-nil but no field names (e.g. empty concat / record-type-key)
	// still must fall through to "(unset)" — otherwise `meta diff` would
	// render an empty-string PK in + / - lines and break jq scripts.
	if got := pkFieldsOrUnset(&recordlayer.EmptyKeyExpression{}); got != "(unset)" {
		t.Errorf("empty expression → %q; want (unset)", got)
	}

	// Single field.
	ke := recordlayer.Field("order_id")
	if got := pkFieldsOrUnset(ke); got != "order_id" {
		t.Errorf("Field(order_id) → %q; want order_id", got)
	}

	// Composite PK — comma-joined, no parens / no spaces (consistent
	// with how other commands render tuples).
	ke = recordlayer.Concat(recordlayer.Field("store"), recordlayer.Field("id"))
	if got := pkFieldsOrUnset(ke); got != "store,id" {
		t.Errorf("Concat(store,id) → %q; want store,id", got)
	}
}

// TestDiffSection_EntryShape verifies the structured diffEntry output
// replaces the text-parsing that splitDiffLine used to do. Each bucket
// must contain entries with Name populated; Detail is optional (empty
// for bare removals).
func TestDiffSection_EntryShape(t *testing.T) {
	t.Parallel()
	m1 := buildMetaWith(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
	})
	m2 := buildMetaWith(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.AddIndex("Order", recordlayer.NewIndex("Order$quantity", recordlayer.Field("quantity")))
	})

	idx := diffIndexes(m1, m2)
	// Expect 1 added (Order$quantity), 1 removed (Order$price), 0 changed.
	if len(idx.Added) != 1 || idx.Added[0].Name != "Order$quantity" {
		t.Errorf("Added = %v; want [Order$quantity]", idx.Added)
	}
	if idx.Added[0].Detail == "" {
		t.Errorf("Added entry missing Detail — was expected to carry type/fields summary")
	}
	if len(idx.Removed) != 1 || idx.Removed[0].Name != "Order$price" {
		t.Errorf("Removed = %v; want [Order$price]", idx.Removed)
	}
	// Removed entries have empty Detail by contract.
	if idx.Removed[0].Detail != "" {
		t.Errorf("Removed entry Detail = %q; want empty", idx.Removed[0].Detail)
	}
	if len(idx.Changed) != 0 {
		t.Errorf("Changed = %v; want empty", idx.Changed)
	}
}

// Matrix regression (RFC-174 Slice 0): every compared index/type
// attribute, when flipped alone, must surface in BOTH the text and the
// JSON output. The old diff compared only index type + expression
// fields and type PKs — a flipped unique option, predicate, or
// record-type key reported "(metadata is identical)", which is
// dangerous for a tool whose stated job is deploy-time sanity.
// (Subspace key is not diffable: Go's Index doesn't model it.)
func TestDiff_AttributeMatrix_SurfacesInTextAndJSON(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string // fieldChange.Field identifier expected in output
		mutateNew func(md *recordlayer.RecordMetaData)
	}{
		{"options", func(md *recordlayer.RecordMetaData) {
			md.GetIndex("Order$price").Options = map[string]string{"unique": "true"}
		}},
		{"predicate", func(md *recordlayer.RecordMetaData) {
			err := md.GetIndex("Order$price").SetPredicateProto(&gen.Predicate{
				ConstantPredicate: &gen.ConstantPredicate{
					Value: gen.ConstantPredicate_TRUE.Enum(),
				},
			})
			if err != nil {
				t.Fatalf("SetPredicateProto: %v", err)
			}
		}},
		{"added_version", func(md *recordlayer.RecordMetaData) {
			md.GetIndex("Order$price").AddedVersion++
		}},
		{"last_modified_version", func(md *recordlayer.RecordMetaData) {
			md.GetIndex("Order$price").LastModifiedVersion++
		}},
		{"since_version", func(md *recordlayer.RecordMetaData) {
			md.RecordTypes()["Order"].SinceVersion = 3
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			withIdx := func(b *recordlayer.RecordMetaDataBuilder) {
				b.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
			}
			oldMD := buildMetaWith(t, withIdx)
			newMD := buildMetaWith(t, withIdx)
			tc.mutateNew(newMD)

			var text bytes.Buffer
			if err := writeMetaDiff(&text, oldMD, newMD); err != nil {
				t.Fatalf("writeMetaDiff: %v", err)
			}
			if strings.Contains(text.String(), "identical") {
				t.Fatalf("%s flip reported identical:\n%s", tc.name, text.String())
			}
			if !strings.Contains(text.String(), tc.name) {
				t.Errorf("text diff does not name the %s change:\n%s", tc.name, text.String())
			}

			var jsonBuf bytes.Buffer
			if err := writeMetaDiffJSON(&jsonBuf, oldMD, newMD); err != nil {
				t.Fatalf("writeMetaDiffJSON: %v", err)
			}
			// The symmetric contract: the changed bucket must carry a
			// {field, old, new} entry naming this attribute.
			if !strings.Contains(jsonBuf.String(), `"field": "`+tc.name+`"`) {
				t.Errorf("JSON diff missing {field:%q} change entry:\n%s", tc.name, jsonBuf.String())
			}
		})
	}
}

// record_type_key flips via the builder (SetRecordTypeKey) — kept out of
// the mutate-the-built-metadata matrix because the key participates in
// Build() validation.
func TestDiff_RecordTypeKeyChange(t *testing.T) {
	t.Parallel()
	oldMD := buildMetaWith(t)
	newMD := buildMetaWith(t, func(b *recordlayer.RecordMetaDataBuilder) {
		b.GetRecordType("Order").SetRecordTypeKey(int64(7))
	})

	var text bytes.Buffer
	if err := writeMetaDiff(&text, oldMD, newMD); err != nil {
		t.Fatalf("writeMetaDiff: %v", err)
	}
	if !strings.Contains(text.String(), "record_type_key") {
		t.Errorf("text diff does not name the record_type_key change:\n%s", text.String())
	}
	var jsonBuf bytes.Buffer
	if err := writeMetaDiffJSON(&jsonBuf, oldMD, newMD); err != nil {
		t.Fatalf("writeMetaDiffJSON: %v", err)
	}
	if !strings.Contains(jsonBuf.String(), `"field": "record_type_key"`) {
		t.Errorf("JSON diff missing record_type_key change:\n%s", jsonBuf.String())
	}
}
