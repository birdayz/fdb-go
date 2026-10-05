//go:build bazelrunfiles

package conformance_test

// Index-shape helpers shared by the conformance_test D11 stored-bytes check
// (index_ddl_metadata_conformance_test.go) and the RFC-257 WS-J corpus oracle
// (ws_j_index_fidelity_conformance_test.go, in rfc257_oracle_test). Both targets
// list this file in srcs, so a helper change is compiled and exercised by both.

import (
	"context"
	"fmt"
	"sort"

	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/catalog"
)

// clearProto2Defaults recursively clears proto2 optional fields that are
// explicitly set to their default value. This normalizes "not set" vs
// "explicitly set to default" so that JSON comparison is stable across
// Go (never sets defaults) and Java (materializes defaults on roundtrip).
func clearProto2Defaults(m protoreflect.Message) {
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList() && fd.Kind() == protoreflect.MessageKind:
			// Recurse into each message in repeated message fields
			list := v.List()
			for i := 0; i < list.Len(); i++ {
				clearProto2Defaults(list.Get(i).Message())
			}
		case fd.IsMap() && fd.MapValue().Kind() == protoreflect.MessageKind:
			// Recurse into message values in map fields
			v.Map().Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
				clearProto2Defaults(mv.Message())
				return true
			})
		case fd.IsList(), fd.IsMap():
			// skip non-message collections
		case fd.Kind() == protoreflect.MessageKind:
			clearProto2Defaults(v.Message())
		case fd.HasPresence() && fd.Cardinality() != protoreflect.Required:
			// Only clear optional fields — required fields must stay set.
			def := fd.Default()
			switch fd.Kind() {
			case protoreflect.EnumKind:
				if v.Enum() == def.Enum() {
					m.Clear(fd)
				}
			case protoreflect.BytesKind:
				if string(v.Bytes()) == string(def.Bytes()) {
					m.Clear(fd)
				}
			default:
				if v.Interface() == def.Interface() {
					m.Clear(fd)
				}
			}
		}
		return true
	})
}

// normalizedIndexOptions renders an index's stored options as sorted
// "key=value" strings for order-insensitive comparison.
func normalizedIndexOptions(idx *gen.Index) []string {
	out := make([]string, 0, len(idx.GetOptions()))
	for _, o := range idx.GetOptions() {
		out = append(out, o.GetKey()+"="+o.GetValue())
	}
	sort.Strings(out)
	return out
}

// normalizedProto clones m and clears proto2 optionals explicitly set to
// their default — Java materializes defaults Go leaves unset; the bytes are
// otherwise identical.
func normalizedProto[M proto.Message](m M) M {
	cp := proto.Clone(m).(M)
	clearProto2Defaults(cp.ProtoReflect())
	return cp
}

// indexShapeKey renders an index's full stored SHAPE — name, type, options,
// root expression and predicate — as one deterministic string. Two indexes
// compare equal iff every dimension this test claims to compare is equal, so a
// map keyed by name and valued by this string turns the per-index comparison
// into a SET comparison that cannot be satisfied by a subset.
func indexShapeKey(idx *gen.Index) string {
	root, err := proto.MarshalOptions{Deterministic: true}.Marshal(
		normalizedProto(idx.GetRootExpression()))
	Expect(err).NotTo(HaveOccurred())
	var pred []byte
	if p := idx.GetPredicate(); p != nil {
		pred, err = proto.MarshalOptions{Deterministic: true}.Marshal(normalizedProto(p))
		Expect(err).NotTo(HaveOccurred())
	}
	return fmt.Sprintf("name=%s type=%s options=%v root=%x predicate=%x",
		idx.GetName(), idx.GetType(), normalizedIndexOptions(idx), root, pred)
}

// assertIndexSetEquality checks that the set of indexes GO persists equals the
// set JAVA persists, index for index. Both directions are asserted — a missing
// index and an unexpected extra one each fail. A surplus index is metadata a
// Java app opening the stored template would have to maintain without ever
// having declared it.
func assertIndexSetEquality(shapeName string, expected []string, javaIdx, goIdx map[string]*gen.Index) {
	expectedSet := make(map[string]struct{}, len(expected))
	for _, n := range expected {
		expectedSet[n] = struct{}{}
	}

	// Java persists exactly the declared set — no companions, no surplus.
	javaSorted := make([]string, 0, len(javaIdx))
	for n := range javaIdx {
		javaSorted = append(javaSorted, n)
	}
	sort.Strings(javaSorted)
	wantSorted := append([]string(nil), expected...)
	sort.Strings(wantSorted)
	Expect(javaSorted).To(Equal(wantSorted),
		"%s: Java's stored index set is not the expected set", shapeName)

	// Go persists the expected set with IDENTICAL shapes...
	for name := range expectedSet {
		g, ok := goIdx[name]
		Expect(ok).To(BeTrue(), "%s: Go persisted no index %s (has %v) — an expected index "+
			"that is missing must fail just as loudly as an unexpected extra one",
			shapeName, name, sortedKeys(goIdx))
		Expect(indexShapeKey(g)).To(Equal(indexShapeKey(javaIdx[name])),
			"%s: index %s shape diverges from Java's stored shape", shapeName, name)
	}

	// ...and NOTHING else.
	for name := range goIdx {
		_, ok := expectedSet[name]
		Expect(ok).To(BeTrue(),
			"%s: Go persisted an UNEXPECTED index %q that Java does not persist "+
				"(Java has %v, Go has %v)",
			shapeName, name, sortedKeys(javaIdx), sortedKeys(goIdx))
	}
}

func sortedKeys(m map[string]*gen.Index) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func indexNames(indexes []*gen.Index) []string {
	out := make([]string, 0, len(indexes))
	for _, idx := range indexes {
		out = append(out, idx.GetName())
	}
	sort.Strings(out)
	return out
}

// loadStoredJavaTemplateBytes returns the RAW stored RecordMetaDataProto.MetaData
// bytes for templateName from the shared catalog subspace Java wrote
// (catalog.OpenRecordLayerStoreCatalog — the Java-wire-compatible
// (NULL, NULL, int64(0)) keyspace; ListTemplates' META_DATA column is the
// stored proto).
func loadStoredJavaTemplateBytes(ctx context.Context, db *recordlayer.FDBDatabase, templateName string) []byte {
	cat, openErr := catalog.OpenRecordLayerStoreCatalog()
	Expect(openErr).NotTo(HaveOccurred())
	var stored []byte
	_, runErr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		tx := catalog.NewFDBTransaction(rtx)
		rs, listErr := cat.SchemaTemplateCatalog().ListTemplates(tx)
		if listErr != nil {
			return nil, listErr
		}
		for rs.Next() {
			name, nameErr := rs.String(1)
			if nameErr != nil {
				return nil, nameErr
			}
			if name != templateName {
				continue
			}
			raw, rawErr := rs.Bytes(3)
			if rawErr != nil {
				return nil, rawErr
			}
			stored = raw
		}
		return nil, rs.Err()
	})
	Expect(runErr).NotTo(HaveOccurred())
	Expect(stored).NotTo(BeNil(), "template %s not found in the shared catalog", templateName)
	return stored
}

// loadStoredJavaTemplateMetaData unmarshals the bytes loadStoredJavaTemplateBytes
// reads. It consumes the stored bytes directly, never Java's object model
// round-tripped through Go's, so a Go deserialization bug cannot mask a
// divergence.
func loadStoredJavaTemplateMetaData(ctx context.Context, db *recordlayer.FDBDatabase, templateName string) *gen.MetaData {
	md := &gen.MetaData{}
	Expect(proto.Unmarshal(loadStoredJavaTemplateBytes(ctx, db, templateName), md)).To(Succeed(),
		"unmarshal stored MetaData for %s", templateName)
	return md
}
