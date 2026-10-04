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
	"strings"

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

// groupingKeyOfProto reconstructs the grouping key expression an index proto
// declares, or nil when the index is not a grouping index at all.
func groupingKeyOfProto(idx *gen.Index) *recordlayer.GroupingKeyExpression {
	expr, err := recordlayer.KeyExpressionFromProto(idx.GetRootExpression())
	if err != nil {
		return nil
	}
	gke, ok := expr.(*recordlayer.GroupingKeyExpression)
	if !ok {
		return nil
	}
	return gke
}

// isGroupExistenceCompanionOf reports whether companion is the RFC-209 §5.2
// group-existence companion of the JAVA-stored owner index: a COUNT(*) index
// (record-layer type "count", zero grouped columns) whose grouping key is
// byte-identical to the grouping half of the owner's, over the same row
// population (same stored predicate).
//
// Anchoring the check on Java's stored owner rather than on Go's own owner is
// deliberate: the interop claim is that a Java app opening the STORED metadata
// sees a plain grouped COUNT index over a grouping key it already understands.
// Deriving the expectation from Go's side would make the assertion agree with
// whatever Go emitted.
//
// The version and option checks are not decoration: a Java app can only see
// this index by opening the STORED metadata (FDBMetaDataStore.loadAndSetCurrent
// builds every index in the proto with no whitelist), and every validating path
// on that side requires 0 < added_version <= last_modified_version <=
// metadata.version. clearWhenZero is the option that decides whether a zeroed
// group leaves a key behind — which IS the group-existence semantics — so Go
// leaving it unset is a claim about Java's behaviour, and it gets pinned.
func isGroupExistenceCompanionOf(companion, javaOwner *gen.Index, metaDataVersion int32) (bool, string) {
	if companion.GetType() != recordlayer.IndexTypeCount {
		return false, fmt.Sprintf("type is %q, want %q — only a COUNT(*) index counts rows "+
			"independently of the aggregated value, and only \"count\" resolves to Java's "+
			"AtomicMutationIndexMaintainer", companion.GetType(), recordlayer.IndexTypeCount)
	}
	cgke := groupingKeyOfProto(companion)
	if cgke == nil {
		return false, "root expression is not a grouping key expression"
	}
	if cgke.GetGroupedCount() != 0 {
		return false, fmt.Sprintf("grouped_count is %d, want 0 — Java's "+
			"AtomicMutationIndexMaintainerFactory rejects a COUNT index with grouped columns",
			cgke.GetGroupedCount())
	}
	ogke := groupingKeyOfProto(javaOwner)
	if ogke == nil {
		return false, fmt.Sprintf("owner %s stores no grouping key expression", javaOwner.GetName())
	}
	want := recordlayer.GroupingSignature(ogke)
	got := recordlayer.GroupingSignature(cgke)
	if len(want) == 0 || len(got) == 0 || string(want) != string(got) {
		return false, fmt.Sprintf("grouping key differs from owner %s's grouping half",
			javaOwner.GetName())
	}
	if !proto.Equal(normalizedProto(companion.GetPredicate()), normalizedProto(javaOwner.GetPredicate())) {
		return false, fmt.Sprintf("predicate differs from owner %s's — a companion over a "+
			"different row population reports groups the owner never indexed",
			javaOwner.GetName())
	}
	added, lastModified := companion.GetAddedVersion(), companion.GetLastModifiedVersion()
	if !(added > 0 && added <= lastModified && lastModified <= metaDataVersion) {
		return false, fmt.Sprintf("version fields are added=%d last_modified=%d against "+
			"metadata version %d, but every validating path on Java's side requires "+
			"0 < added <= last_modified <= metadata.version; an auto-emitted index that "+
			"violates it makes the whole stored template unopenable from Java",
			added, lastModified, metaDataVersion)
	}
	for _, o := range companion.GetOptions() {
		if o.GetKey() == recordlayer.IndexOptionClearWhenZero {
			return false, fmt.Sprintf("carries clearWhenZero=%q. The companion's whole job is "+
				"to distinguish a live group from a vacated one, and clearWhenZero decides "+
				"whether a zeroed group leaves a key behind. Go leaves the option unset and "+
				"drops zero-valued entries at READ time, which is what makes both engines' "+
				"maintainers agree; storing the option changes the write path and this "+
				"assertion must be revisited together with that change.", o.GetValue())
		}
	}
	return true, ""
}

// assertIndexSetEquality is the RFC-209 half of this check: the set of indexes
// GO persists must equal the set JAVA persists, index for index, EXCEPT for
// group-existence companions of indexes that are themselves in the expected
// set. Both directions are asserted — a missing index and an unexpected extra
// one each fail.
//
// A membership check over a list of expected names cannot do this. RFC-209 made
// Go auto-emit an index Java does not persist, and the interop claim rests
// entirely on that surplus being EXACTLY the allowlisted companions: any other
// surplus index is metadata a Java app opening the stored template would have
// to maintain without ever having declared it.
func assertIndexSetEquality(shapeName string, expected []string, javaIdx, goIdx map[string]*gen.Index, goMetaDataVersion int32) {
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

	// ...and NOTHING else, other than allowlisted group-existence companions.
	for name, g := range goIdx {
		if _, ok := expectedSet[name]; ok {
			continue
		}
		owner, isCompanionName := strings.CutSuffix(name, recordlayer.GroupCountCompanionSuffix)
		Expect(isCompanionName).To(BeTrue(),
			"%s: Go persisted an UNEXPECTED index %q that Java does not persist and that is "+
				"not a group-existence companion (Java has %v, Go has %v). Go's stored metadata "+
				"may only be a superset of Java's by exactly the RFC-209 §5.2 companions; an "+
				"intentional new auto-emitted index must be added to this allowlist "+
				"DELIBERATELY, with the interop argument for it, never absorbed silently.",
			shapeName, name, sortedKeys(javaIdx), sortedKeys(goIdx))
		javaOwner, ownerOK := javaIdx[owner]
		Expect(ownerOK).To(BeTrue(),
			"%s: Go persisted %q, whose name claims to be the group-existence companion of "+
				"%q — but %q is not in the expected set. A companion is allowlisted only as a "+
				"companion OF an expected index; add it to this allowlist deliberately.",
			shapeName, name, owner, owner)
		ok, why := isGroupExistenceCompanionOf(g, javaOwner, goMetaDataVersion)
		Expect(ok).To(BeTrue(),
			"%s: Go persisted %q, which carries the companion suffix but is not a "+
				"group-existence companion of %q: %s. A name is not a licence — the allowlist "+
				"tolerates the STRUCTURE, and an index that merely borrows the name is an "+
				"unexpected index.",
			shapeName, name, owner, why)
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
