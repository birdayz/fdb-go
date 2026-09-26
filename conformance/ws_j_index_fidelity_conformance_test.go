//go:build bazelrunfiles

package conformance_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/antlr4-go/antlr/v4"
	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/plandiff"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/keyspace"
	"fdb.dev/pkg/relational/core/metadata"
	"fdb.dev/pkg/relational/core/parser"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
)

// Live-JVM oracle for RFC-257 WS-J index-definition fidelity, driven by the
// TARGET release's own corpus rather than hand-picked shapes.
//
// conformance/testdata/rfc257/java_index_templates.json is every schema template
// in the 4.14.2.0 yaml-tests corpus that declares an index (harvested by
// rfcs/257-java-upgrade-audit/ws-j-oracle/harvest.py from the Java checkout at
// the release tag; the file records tag and commit). For each template the real
// Java engine persists it into the shared catalog, Go reads the RAW stored bytes
// back, and Go's own DDL front end builds the identical text under the identical
// template name. The comparison is per index and per stored field, and it is
// deliberately stricter than the D11 shape key (index_ddl_metadata_conformance_
// test.go): record_type, subspace_key and the version fields are compared too,
// and every root/predicate difference is classified as either a STRICT byte
// difference (Go and Java disagree only on whether a proto2 default is present)
// or a REAL one (they disagree after clearProto2Defaults).
//
// The JAVA outcome of every run is pinned: acceptance with the index count and a
// digest of the whole canonical stored MetaData, or the rejection's SQLSTATE and
// class (wsj_java_pins.json; the Java roots of the hand-written shapes are printed
// as WSJ-JAVA-ROOT lines). That is the target contract the WS-J design ports. The
// GO outcome is pinned as each run's CLASS (wsj_go_classes.json), so a run that
// stops being equal reddens here and a run the port makes equal is a reviewed pin
// update; the per-index differences behind a class are recorded in the WSJ lines.
type wsjTemplate struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Form    string `json:"form"`
	Variant string `json:"variant"`
	Body    string `json:"body"`
}

type wsjCorpus struct {
	Source struct {
		Tag    string `json:"tag"`
		Commit string `json:"commit"`
	} `json:"source"`
	// Census is harvest.py's statement census: every `create [unique|vector]
	// index` occurrence in the corpus, reconciled against the harvested bodies
	// and the comment lines that mention the phrase.
	Census struct {
		Occurrences      int `json:"occurrences"`
		InTemplateBodies int `json:"in_template_bodies"`
		OnCommentLines   []struct {
			File string `json:"file"`
			Line int    `json:"line"`
		} `json:"on_comment_lines"`
		HarvestedStatements int `json:"harvested_statements"`
		VariantDuplicates   int `json:"variant_duplicates"`
	} `json:"census"`
	Templates []wsjTemplate `json:"templates"`
}

// wsjQuantifierAliasRE matches the per-plan quantifier alias a target message
// embeds, which the render replaces with <alias>.
var wsjQuantifierAliasRE = regexp.MustCompile(`\bq[0-9a-f]{8}_[0-9a-f]{4}_[0-9a-f]{4}_[0-9a-f]{4}_[0-9a-f]{12}\b`)

var wsjRandomAliasRE = regexp.MustCompile(`c[0-9a-f]{8}_[0-9a-f]{4}_[0-9a-f]{4}_[0-9a-f]{4}_[0-9a-f]{12}`)

// wsjIndexStatementRE counts index statements in a harvested body the way
// harvest.py's INDEX_RE does; the spec re-counts so a hand-edited corpus file
// cannot keep a stale census.
var wsjIndexStatementRE = regexp.MustCompile(`(?i)\bcreate\s+(unique\s+|vector\s+)?index\b`)

// loadWSJJavaPins reads the measured target outcome of every run from
// testdata/rfc257/wsj_java_pins.json (id -> "OK <indexes> <digest>" or
// "ERROR <SQLSTATE> <exception class>").
func loadWSJJavaPins() map[string]string {
	return loadWSJPinFile("wsj_java_pins.json")
}

// loadWSJPinFile reads one id -> string pin file from testdata/rfc257, staged as
// runfiles data.
func loadWSJPinFile(name string) map[string]string {
	r, err := runfiles.New()
	Expect(err).NotTo(HaveOccurred())
	p, err := r.Rlocation("_main/conformance/testdata/rfc257/" + name)
	Expect(err).NotTo(HaveOccurred())
	raw, err := os.ReadFile(p)
	Expect(err).NotTo(HaveOccurred(), "%s must be staged as runfiles data", name)
	pins := map[string]string{}
	Expect(json.Unmarshal(raw, &pins)).To(Succeed())
	Expect(pins).NotTo(BeEmpty(), "%s holds no pins", name)
	return pins
}

func loadWSJCorpus() wsjCorpus {
	r, err := runfiles.New()
	Expect(err).NotTo(HaveOccurred())
	p, err := r.Rlocation("_main/conformance/testdata/rfc257/java_index_templates.json")
	Expect(err).NotTo(HaveOccurred())
	raw, err := os.ReadFile(p)
	Expect(err).NotTo(HaveOccurred(), "the harvested corpus must be staged as runfiles data")
	var c wsjCorpus
	Expect(json.Unmarshal(raw, &c)).To(Succeed())
	return c
}

func (t wsjTemplate) id() string {
	id := fmt.Sprintf("%s:%d", t.File, t.Line)
	if t.Variant != "" {
		id += "[" + t.Variant + "]"
	}
	return id
}

// wsjIndexDims lists every stored Index field on which go and java disagree.
// A root/predicate difference is "root~" when it vanishes under
// clearProto2Defaults (presence-only) and "root" when it does not.
func wsjIndexDims(g, j *gen.Index) []string {
	var dims []string
	gt, jt := append([]string(nil), g.GetRecordType()...), append([]string(nil), j.GetRecordType()...)
	sort.Strings(gt)
	sort.Strings(jt)
	if strings.Join(gt, ",") != strings.Join(jt, ",") {
		dims = append(dims, fmt.Sprintf("record_type(go=%v java=%v)", gt, jt))
	}
	if g.GetType() != j.GetType() {
		dims = append(dims, fmt.Sprintf("type(go=%q java=%q)", g.GetType(), j.GetType()))
	}
	if go_, ja := normalizedIndexOptions(g), normalizedIndexOptions(j); strings.Join(go_, ",") != strings.Join(ja, ",") {
		dims = append(dims, fmt.Sprintf("options(go=%v java=%v)", go_, ja))
	} else if gk, jk := wsjOptionKeyOrder(g), wsjOptionKeyOrder(j); strings.Join(gk, ",") != strings.Join(jk, ",") {
		// The same option set in a different stored ORDER is still different
		// bytes: Java stores its ImmutableMap's insertion order
		// (RecordLayerIndex.Builder.setOption, Index.toProto :661-663).
		dims = append(dims, fmt.Sprintf("options-order(go=%v java=%v)", gk, jk))
	}
	msgDim := func(name string, gm, jm proto.Message) {
		if proto.Equal(gm, jm) {
			return
		}
		if proto.Equal(normalizedProto(gm), normalizedProto(jm)) {
			dims = append(dims, name+"~")
			return
		}
		dims = append(dims, name)
	}
	msgDim("root", g.GetRootExpression(), j.GetRootExpression())
	msgDim("predicate", g.GetPredicate(), j.GetPredicate())
	if string(g.GetSubspaceKey()) != string(j.GetSubspaceKey()) {
		dims = append(dims, fmt.Sprintf("subspace_key(go=%x java=%x)", g.GetSubspaceKey(), j.GetSubspaceKey()))
	}
	if g.GetAddedVersion() != j.GetAddedVersion() || g.GetLastModifiedVersion() != j.GetLastModifiedVersion() {
		dims = append(dims, fmt.Sprintf("versions(go=%d/%d java=%d/%d)",
			g.GetAddedVersion(), g.GetLastModifiedVersion(), j.GetAddedVersion(), j.GetLastModifiedVersion()))
	}
	// Every stored field not compared above — the deprecated index_type and
	// value_expression, extensions and unknown fields — is compared
	// generically, so a field this list forgot still reports.
	if other := wsjOtherIndexFields(g, j); len(other) > 0 {
		dims = append(dims, "other("+strings.Join(other, ",")+")")
	}
	return dims
}

func wsjOptionKeyOrder(i *gen.Index) []string {
	var out []string
	for _, o := range i.GetOptions() {
		out = append(out, o.GetKey())
	}
	return out
}

var wsjComparedIndexFields = map[protoreflect.Name]bool{
	"record_type": true, "type": true, "options": true, "root_expression": true,
	"predicate": true, "subspace_key": true, "added_version": true,
	"last_modified_version": true, "name": true,
}

func wsjOtherIndexFields(g, j *gen.Index) []string {
	strip := func(i *gen.Index) *gen.Index {
		c := proto.Clone(i).(*gen.Index)
		m := c.ProtoReflect()
		fields := m.Descriptor().Fields()
		for k := 0; k < fields.Len(); k++ {
			if wsjComparedIndexFields[fields.Get(k).Name()] {
				m.Clear(fields.Get(k))
			}
		}
		return c
	}
	gs, js := strip(g), strip(j)
	if proto.Equal(gs, js) {
		return nil
	}
	var out []string
	fields := gs.ProtoReflect().Descriptor().Fields()
	for k := 0; k < fields.Len(); k++ {
		fd := fields.Get(k)
		gm, jm := gs.ProtoReflect(), js.ProtoReflect()
		if gm.Has(fd) != jm.Has(fd) || !gm.Get(fd).Equal(jm.Get(fd)) {
			out = append(out, string(fd.Name()))
		}
	}
	if string(gs.ProtoReflect().GetUnknown()) != string(js.ProtoReflect().GetUnknown()) {
		out = append(out, "unknown-fields")
	}
	if len(out) == 0 {
		out = append(out, "extensions")
	}
	return out
}

// wsjCanonical returns a copy of md with the two differences WS-J declares as
// not ported (ws-j-design.md section 4) removed, and nothing else:
//   - record_types sorted by name (Java's order is HashMap iteration order);
//   - anonymous "__type__<id>" descriptor messages renamed IN PLACE by the path
//     that reaches them from a named message ("__anon__<owner>.<field>"), since
//     Java draws a random UUID per build (ProtoUtils.java:96-98);
//   - within each maximal run of CONSECUTIVE anonymous messages, those messages
//     ordered by their canonical name. Java emits a table's types from a TreeSet
//     keyed by type name (TypeRepository.java:269-271), so the relative order of
//     two anonymous types is the order of their random names, and measured to
//     move between two builds of one template (in-predicate.yamsql:20). Their
//     position relative to NAMED types does not move — every anonymous name
//     shares the "__type__" prefix — so no named message is reordered.
//
// Descriptor message ORDER, union field NUMBERS, record-type keys, field
// numbers, index protos and versions are untouched: those are the semantic
// content the comparison is for, and message order is what Java's
// FileDescriptorSerializer emits (table-visit order, a TreeSet per table), which
// the port matches rather than hides.
func wsjCanonical(md *gen.MetaData) *gen.MetaData {
	c := proto.Clone(md).(*gen.MetaData)
	sort.SliceStable(c.RecordTypes, func(a, b int) bool {
		return c.RecordTypes[a].GetName() < c.RecordTypes[b].GetName()
	})
	fdp := c.GetRecords()
	if fdp == nil {
		return c
	}
	short := func(typeName string) string {
		if i := strings.LastIndex(typeName, "."); i >= 0 {
			return typeName[i+1:]
		}
		return typeName
	}
	byName := map[string]*descriptorpb.DescriptorProto{}
	for _, m := range fdp.GetMessageType() {
		byName[m.GetName()] = m
	}
	isAnon := func(name string) bool { return strings.HasPrefix(name, "__type__") }
	anonAt := map[*descriptorpb.DescriptorProto]bool{}
	rename := map[string]string{}
	var walk func(owner string, m *descriptorpb.DescriptorProto)
	walk = func(owner string, m *descriptorpb.DescriptorProto) {
		fields := append([]*descriptorpb.FieldDescriptorProto(nil), m.GetField()...)
		sort.Slice(fields, func(a, b int) bool { return fields[a].GetNumber() < fields[b].GetNumber() })
		for _, f := range fields {
			target := short(f.GetTypeName())
			if !isAnon(target) {
				continue
			}
			if _, done := rename[target]; done {
				continue
			}
			canon := "__anon__" + owner + "." + f.GetName()
			rename[target] = canon
			if tm, ok := byName[target]; ok {
				walk(canon, tm)
			}
		}
	}
	var named []string
	for n := range byName {
		if !isAnon(n) {
			named = append(named, n)
		}
	}
	sort.Strings(named)
	for _, n := range named {
		walk(n, byName[n])
	}
	for _, m := range fdp.GetMessageType() {
		wasAnon := isAnon(m.GetName())
		if canon, ok := rename[m.GetName()]; ok {
			m.Name = proto.String(canon)
		}
		if wasAnon {
			anonAt[m] = true
		}
		for _, f := range m.GetField() {
			tn := f.GetTypeName()
			if canon, ok := rename[short(tn)]; ok {
				f.TypeName = proto.String(strings.TrimSuffix(tn, short(tn)) + canon)
			}
		}
	}
	msgs := fdp.MessageType
	for i := 0; i < len(msgs); {
		if !anonAt[msgs[i]] {
			i++
			continue
		}
		j := i
		for j < len(msgs) && anonAt[msgs[j]] {
			j++
		}
		run := msgs[i:j]
		sort.SliceStable(run, func(a, b int) bool { return run[a].GetName() < run[b].GetName() })
		i = j
	}
	return c
}

// wsjRemoveCompanions returns a copy of md without the named RFC-209 companion
// indexes and with the metadata versions they took given back. Go adds a
// companion together with its owner, and the metadata builder gives every added
// index its own version, so each companion occupies one version slot: every
// index version above a slot, and the metadata version, move down by one per
// slot below them. A companion whose added and last-modified versions differ
// does not occupy one slot, so it is not removed and the run stays an index
// divergence.
func wsjRemoveCompanions(md *gen.MetaData, companions map[string]bool) *gen.MetaData {
	c := proto.Clone(md).(*gen.MetaData)
	var slots []int32
	kept := c.Indexes[:0]
	for _, i := range c.Indexes {
		if companions[i.GetName()] && i.GetAddedVersion() == i.GetLastModifiedVersion() {
			slots = append(slots, i.GetAddedVersion())
			continue
		}
		kept = append(kept, i)
	}
	c.Indexes = kept
	below := func(v int32, inclusive bool) int32 {
		n := int32(0)
		for _, s := range slots {
			if s < v || (inclusive && s == v) {
				n++
			}
		}
		return n
	}
	for _, i := range c.Indexes {
		if i.AddedVersion != nil {
			i.AddedVersion = proto.Int32(i.GetAddedVersion() - below(i.GetAddedVersion(), false))
		}
		if i.LastModifiedVersion != nil {
			i.LastModifiedVersion = proto.Int32(i.GetLastModifiedVersion() - below(i.GetLastModifiedVersion(), false))
		}
	}
	if c.Version != nil {
		c.Version = proto.Int32(c.GetVersion() - below(c.GetVersion(), true))
	}
	return c
}

// wsjMetaDataFieldDiff names the top-level MetaData fields that differ,
// marking a field "~" when the difference vanishes under clearProto2Defaults.
func wsjMetaDataFieldDiff(g, j *gen.MetaData) []string {
	var out []string
	gm, jm := g.ProtoReflect(), j.ProtoReflect()
	fields := gm.Descriptor().Fields()
	for k := 0; k < fields.Len(); k++ {
		fd := fields.Get(k)
		if gm.Has(fd) == jm.Has(fd) && gm.Get(fd).Equal(jm.Get(fd)) {
			continue
		}
		gc, jc := proto.Clone(g).(*gen.MetaData), proto.Clone(j).(*gen.MetaData)
		gr, jr := gc.ProtoReflect(), jc.ProtoReflect()
		for q := 0; q < fields.Len(); q++ {
			if q != k {
				gr.Clear(fields.Get(q))
				jr.Clear(fields.Get(q))
			}
		}
		if proto.Equal(normalizedProto(gc), normalizedProto(jc)) {
			out = append(out, string(fd.Name())+"~")
		} else {
			out = append(out, string(fd.Name()))
		}
	}
	if string(gm.GetUnknown()) != string(jm.GetUnknown()) {
		out = append(out, "unknown-fields")
	}
	if len(out) == 0 {
		out = append(out, "extensions-or-order")
	}
	return out
}

// wsjStructureDetail renders the order of stored record types, descriptor
// messages and indexes on both sides, plus every per-name difference in a
// record type or descriptor message, compactly enough for one log line.
func wsjStructureDetail(g, j *gen.MetaData) string {
	var b strings.Builder
	rtNames := func(md *gen.MetaData) []string {
		var out []string
		for _, rt := range md.GetRecordTypes() {
			out = append(out, rt.GetName())
		}
		return out
	}
	msgNames := func(md *gen.MetaData) []string {
		var out []string
		for _, m := range md.GetRecords().GetMessageType() {
			out = append(out, m.GetName())
		}
		return out
	}
	idxNames := func(md *gen.MetaData) []string {
		var out []string
		for _, i := range md.GetIndexes() {
			out = append(out, i.GetName())
		}
		return out
	}
	if gn, jn := rtNames(g), rtNames(j); strings.Join(gn, ",") != strings.Join(jn, ",") {
		fmt.Fprintf(&b, "record_types-order(go=%v java=%v) ", gn, jn)
	}
	if gn, jn := msgNames(g), msgNames(j); strings.Join(gn, ",") != strings.Join(jn, ",") {
		fmt.Fprintf(&b, "messages-order(go=%v java=%v) ", gn, jn)
	}
	if gn, jn := idxNames(g), idxNames(j); strings.Join(gn, ",") != strings.Join(jn, ",") {
		fmt.Fprintf(&b, "indexes-order(go=%v java=%v) ", gn, jn)
	}
	jidx := map[string]*gen.Index{}
	for _, i := range j.GetIndexes() {
		jidx[i.GetName()] = i
	}
	for _, gi := range g.GetIndexes() {
		ji, ok := jidx[gi.GetName()]
		if !ok {
			continue
		}
		if !proto.Equal(normalizedProto(gi.GetRootExpression()), normalizedProto(ji.GetRootExpression())) {
			fmt.Fprintf(&b, "root %s(go={%v} java={%v}) ", gi.GetName(), gi.GetRootExpression(), ji.GetRootExpression())
		}
		if !proto.Equal(normalizedProto(gi.GetPredicate()), normalizedProto(ji.GetPredicate())) {
			fmt.Fprintf(&b, "predicate %s(go={%v} java={%v}) ", gi.GetName(), gi.GetPredicate(), ji.GetPredicate())
		}
	}
	jrt := map[string]*gen.RecordType{}
	for _, rt := range j.GetRecordTypes() {
		jrt[rt.GetName()] = rt
	}
	for _, grt := range g.GetRecordTypes() {
		if jr, ok := jrt[grt.GetName()]; ok && !proto.Equal(grt, jr) {
			fmt.Fprintf(&b, "record_type %s(go={%v} java={%v}) ", grt.GetName(), grt, jr)
		}
	}
	jmsg := map[string]proto.Message{}
	for _, m := range j.GetRecords().GetMessageType() {
		jmsg[m.GetName()] = m
	}
	for _, gm := range g.GetRecords().GetMessageType() {
		jm, ok := jmsg[gm.GetName()]
		if !ok {
			fmt.Fprintf(&b, "message %s go-only ", gm.GetName())
			continue
		}
		if !proto.Equal(gm, jm) {
			fmt.Fprintf(&b, "message %s(go={%v} java={%v}) ", gm.GetName(), gm, jm)
		}
	}
	gr, jr := proto.Clone(g.GetRecords()), proto.Clone(j.GetRecords())
	if gr != nil && jr != nil {
		for _, r := range []proto.Message{gr, jr} {
			m := r.ProtoReflect()
			m.Clear(m.Descriptor().Fields().ByName("message_type"))
		}
		if !proto.Equal(gr, jr) {
			fmt.Fprintf(&b, "records-non-message(go={%v} java={%v}) ", gr, jr)
		}
	}
	return strings.TrimSpace(b.String())
}

// wsjJavaDigest fingerprints everything Java stored, not only the index shapes:
// the canonical MetaData (wsjCanonical) marshalled deterministically, so index
// versions and subspace keys, the metadata version, record-type keys, union
// field numbers and descriptor message order are all under the pin. The stored
// bytes also carry the per-run random template name (the records file
// descriptor is named after it), which is replaced by a placeholder of the same
// length so the length prefixes around it do not move either; and the
// correlation aliases Java draws per build inside stored function and view
// plans ("c" + a UUID with underscores, measured to move between two builds of
// in-predicate, user-defined-macro-function-tests and valid-identifiers), each
// replaced by a same-length placeholder numbered in order of first appearance.
func wsjJavaDigest(md *gen.MetaData, templateName string) string {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(wsjCanonical(md))
	Expect(err).NotTo(HaveOccurred())
	b = bytes.ReplaceAll(b, []byte(templateName), bytes.Repeat([]byte("T"), len(templateName)))
	seen := map[string]string{}
	b = wsjRandomAliasRE.ReplaceAllFunc(b, func(a []byte) []byte {
		if r, ok := seen[string(a)]; ok {
			return []byte(r)
		}
		r := fmt.Sprintf("c%0*d", len(a)-1, len(seen))
		seen[string(a)] = r
		return []byte(r)
	})
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum[:6])
}

func wsjTrim(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// wsjShapes are hand-written index shapes aimed at the generator arms the corpus
// reaches thinly or not at all: field-path tries mixing nested and top-level
// columns (FieldValueTrieNode.computeTrieForValues), non-adjacent references to
// one parent, key/covering splits without and with ORDER BY
// (MaterializedViewIndexGenerator.splitKeyFromValue), ordering functions on
// nested leaves, and the stored width of literals
// (ValueToKeyExpressionVisitor.visitLiteralValue stores the literal's own Java
// object).
var wsjShapes = []struct{ name, body string }{
	{"nested_then_top", wsjStructT + `create index ix as select s.x, ts from t order by s.x, ts`},
	{"top_then_nested", wsjStructT + `create index ix as select ts, s.x from t order by ts, s.x`},
	{"nested_siblings", wsjStructT + `create index ix as select s.x, s.y from t order by s.x, s.y`},
	{"nested_siblings_reordered", wsjStructT + `create index ix as select s.x, s.y from t order by s.y, s.x`},
	{"nested_disconnected", wsjStructT + `create index ix as select s.x, ts, s.y from t order by s.x, ts, s.y`},
	{"nested_then_top_no_order", wsjStructT + `create index ix as select s.x, ts from t`},
	{"top_pair_no_order", wsjStructT + `create index ix as select ts, id from t`},
	{"nested_covering", wsjStructT + `create index ix as select s.x, ts from t order by s.x`},
	{"nested_desc", wsjStructT + `create index ix as select s.x, ts from t order by s.x desc, ts`},
	{"nested_nulls_last", wsjStructT + `create index ix as select s.x, s.y from t order by s.x asc nulls last, s.y`},
	{"grouped_by_nested", wsjStructT + `create index ix as select s.x, count(*) from t group by s.x`},
	{"sum_grouped_by_nested_and_top", wsjStructT + `create index ix as select s.x, ts, sum(s.y) from t group by s.x, ts`},
	{"deep_nesting", `create type as struct st_in(a bigint, b bigint) create type as struct st_out(i st_in, c bigint) ` +
		`create table t(id bigint, o st_out, primary key(id)) ` +
		`create index ix as select o.i.a, o.c, o.i.b from t order by o.i.a, o.c, o.i.b`},
	{"deep_nesting_shared", `create type as struct st_in(a bigint, b bigint) create type as struct st_out(i st_in, c bigint) ` +
		`create table t(id bigint, o st_out, primary key(id)) ` +
		`create index ix as select o.i.a, o.i.b, o.c from t order by o.i.a, o.i.b, o.c`},
	{"literal_int_arith", `create table t(id bigint, d bigint, primary key(id)) create index ix as select d + 1 from t order by d + 1`},
	{"literal_long_arith", `create table t(id bigint, d bigint, primary key(id)) create index ix as select d + 3000000000 from t order by d + 3000000000`},
	{"literal_double_arith", `create table t(id bigint, c double, primary key(id)) create index ix as select c * 1.5 from t order by c * 1.5`},
	{"literal_int_bitand", `create table t(id bigint, d bigint, primary key(id)) create index ix as select d & 7 from t order by d & 7`},
	{"bitmap_bucket_offset_value", `create table t(id bigint, primary key(id)) create index ix as select bitmap_bucket_offset(id) from t order by bitmap_bucket_offset(id)`},
	{"literal_int_int_column", `create table t(id bigint, i integer, primary key(id)) create index ix as select i + 1 from t order by i + 1`},
	{"on_source_nested", wsjStructT + `create index ix on t(s.x)`},
	{"on_source_include", wsjStructT + `create index ix on t(ts) include (id)`},
	{"unnest_comma", `create table t4(id bigint, col2 bigint, col4 bigint array, primary key(id)) ` +
		`create index ix as select t.col2, "e" from t4 as t, t.col4 as "e" order by t.col2, "e"`},
	{"unnest_derived", `create type as struct it(k string) create table t6(id bigint, a bigint, c it array, primary key(id)) ` +
		`create index ix as select a, ek.k from t6, (select k from t6.c) as ek order by a, ek.k`},
	{"unnest_same_array_twice", `create table t4(id bigint, col4 bigint array, primary key(id)) ` +
		`create index ix as select "e1", "e2" from t4 as t, t.col4 as "e1", t.col4 as "e2" order by "e1", "e2"`},
	{"enum_value_index", `create type as enum color('RED', 'GREEN') create table t(id bigint, c color, primary key(id)) ` +
		`create index ix as select c from t order by c`},
	{"array_no_unnest", `create table t4(id bigint, col4 bigint array, primary key(id)) create index ix as select col4 from t4 order by col4`},
	{"multi_table_order", `create table a(id bigint, x bigint, primary key(id)) create table b(id bigint, y bigint, primary key(id)) ` +
		`create table c(id bigint, z bigint, primary key(id)) create index ia as select x from a order by x ` +
		`create index ib as select y from b order by y create index ia2 as select x, id from a order by x, id`},
	// A literal of one numeric type against a column of another: the literal keeps its
	// own carrier whatever the column (INT into FLOAT and DOUBLE arithmetic, DOUBLE
	// into INTEGER arithmetic).
	{"literal_int_float_column", `create table t(id bigint, f float, primary key(id)) create index ix as select f + 1 from t order by f + 1`},
	{"literal_int_double_column", `create table t(id bigint, c double, primary key(id)) create index ix as select c * 2 from t order by c * 2`},
	{"literal_double_int_column", `create table t(id bigint, i integer, primary key(id)) create index ix as select i * 1.5 from t order by i * 1.5`},
	// A table whose quoted name holds a dot and a dollar sign: what name each engine
	// stores for the record type and the index's record_type list.
	{"dotted_table_name", `create table "foo.table$nested"(id bigint, v bigint, primary key(id)) ` +
		`create index ix as select v from "foo.table$nested" order by v`},
	// Literal spellings ParseHelpers.parseDecimal types differently (:68-104):
	// the f/d/h suffixes on a decimal, l/i on an integer, and a negated literal.
	{"literal_float_suffix", `create table t(id bigint, f float, primary key(id)) create index ix as select f + 1.5f from t order by f + 1.5f`},
	{"literal_double_suffix", `create table t(id bigint, c double, primary key(id)) create index ix as select c + 1.5d from t order by c + 1.5d`},
	{"literal_half_suffix", `create table t(id bigint, c double, primary key(id)) create index ix as select c + 1.5h from t order by c + 1.5h`},
	{"literal_long_suffix", `create table t(id bigint, d bigint, primary key(id)) create index ix as select d + 1l from t order by d + 1l`},
	{"literal_int_suffix", `create table t(id bigint, d bigint, primary key(id)) create index ix as select d + 1i from t order by d + 1i`},
	{"literal_negative_int", `create table t(id bigint, d bigint, primary key(id)) create index ix as select d + -1 from t order by d + -1`},
	{"literal_negative_double", `create table t(id bigint, c double, primary key(id)) create index ix as select c * -1.5 from t order by c * -1.5`},
	{"literal_constant_subexpression", `create table t(id bigint, d bigint, primary key(id)) create index ix as select d + (1 + 2) from t order by d + (1 + 2)`},
	// The INT literal's edges and the first LONG: the carrier is int_value up to
	// 2147483647 and down to -2147483648, long_value from 2147483648.
	{"literal_int_max", `create table t(id bigint, d bigint, primary key(id)) create index ix as select d + 2147483647 from t order by d + 2147483647`},
	{"literal_long_min_above_int", `create table t(id bigint, d bigint, primary key(id)) create index ix as select d + 2147483648 from t order by d + 2147483648`},
	{"literal_int_min", `create table t(id bigint, d bigint, primary key(id)) create index ix as select d + -2147483648 from t order by d + -2147483648`},
	// Cross-type: a FLOAT literal over a DOUBLE column, a LONG literal over an INTEGER column.
	{"literal_float_double_column", `create table t(id bigint, c double, primary key(id)) create index ix as select c + 1.5f from t order by c + 1.5f`},
	{"literal_long_int_column", `create table t(id bigint, i integer, primary key(id)) create index ix as select i + 3000000000 from t order by i + 3000000000`},
	// The top-node check (DdlVisitor.java:274, the view plan must be a
	// LogicalSortExpression) runs BEFORE IndexSpec.collect (generate(),
	// MaterializedViewIndexGenerator.java:95-97): an ORDER BY key that is not
	// projected, alone and on a join IndexSpec also refuses; and the two Go top
	// shapes outside the mapped Project/Sort pair, LIMIT and an EXISTS predicate.
	{"top_order_key_not_projected", `create table t(id bigint, a bigint, b bigint, primary key(id)) create index ix as select a from t order by b`},
	{"top_order_key_not_projected_join", `create table a(id bigint, x bigint, primary key(id)) create table b(id bigint, y bigint, primary key(id)) ` +
		`create index ix as select a.x from a, b where a.id = b.id order by b.y`},
	{"top_limit", `create table t(id bigint, a bigint, primary key(id)) create index ix as select a from t order by a limit 5`},
	{"top_exists", `create table t(id bigint, a bigint, primary key(id)) create table u(id bigint, primary key(id)) ` +
		`create index ix as select a from t where exists (select 1 from u where u.id = t.id) order by a`},
	// IndexSpec's structural admission (IndexSpec.java:166, 255-270, 387-433):
	// nested unnests, a derived table without an unnest, a predicate on the
	// inner and on the outer select, HAVING, and a join with and without an
	// unnest leg (the join message comes first, :258-262).
	{"unnest_nested", `create type as struct si(v bigint array) create table t(id bigint, a si array, primary key(id)) ` +
		`create index ix as select "b" from t as r, r.a as "x", "x".v as "b" order by "b"`},
	{"unnest_nested_derived", `create type as struct si(v bigint array) create table t(id bigint, a si array, primary key(id)) ` +
		`create index ix as select sq."b" from t as r, (select "b" from r.a as "x", "x".v as "b") as sq order by sq."b"`},
	{"derived_no_unnest", `create table t(id bigint, a bigint, primary key(id)) create index ix as select d.a from (select a from t) as d order by d.a`},
	{"unnest_inner_predicate", `create table t4(id bigint, col2 bigint, col4 bigint array, primary key(id)) ` +
		`create index ix as select sq."e" from t4 as r, (select "e" from r.col4 as "e" where "e" > 1) as sq order by sq."e"`},
	{"unnest_outer_predicate", `create table t4(id bigint, col2 bigint, col4 bigint array, primary key(id)) ` +
		`create index ix as select sq."e" from t4 as r, (select "e" from r.col4 as "e") as sq where r.col2 > 1 order by sq."e"`},
	{"having", `create table t(id bigint, a bigint, primary key(id)) create index ix as select a, count(*) from t group by a having count(*) > 1`},
	{"join_plain", `create table a(id bigint, x bigint, primary key(id)) create table b(id bigint, y bigint, primary key(id)) ` +
		`create index ix as select a.x from a, b where a.id = b.id order by a.x`},
	{"join_with_unnest", `create table t4(id bigint, col2 bigint, col4 bigint array, primary key(id)) create table b(id bigint, y bigint, primary key(id)) ` +
		`create index ix as select r.col2, "e" from t4 as r, r.col4 as "e", b order by r.col2, "e"`},
	// Enums: the index-predicate side of #4624 (enum-distinct-from-function.
	// yamsql:26-28), an enum on a table that moves when its index is declared,
	// two enums sharing a value name, and an enum no table references (Java
	// stores only the types a table's closure reaches, FileDescriptorSerializer.
	// java:118-140).
	{"enum_predicate_index", `create type as enum mood('JOYFUL', 'HAPPY', 'SAD') create table t(id bigint, m mood, primary key(id)) ` +
		`create index ix as select id from t where m = 'HAPPY' order by id`},
	{"enum_on_moved_table", `create type as enum color('RED', 'GREEN') create table a(id bigint, c color, primary key(id)) ` +
		`create table b(id bigint, y bigint, primary key(id)) create index ia as select c from a order by c`},
	{"enums_sharing_value_name", `create type as enum e1('X', 'Y') create type as enum e2('Y', 'Z') ` +
		`create table t(id bigint, a e1, b e2, primary key(id)) create index ix as select a, b from t order by a, b`},
	{"enum_unreferenced", `create type as enum unused('Q') create table t(id bigint, x bigint, primary key(id)) ` +
		`create index ix as select x from t order by x`},
	// Legacy versus tuple extremum storage (ExtremumEverStorage.java:76).
	{"extremum_ever_legacy", `create table t1(id bigint, col1 bigint, col2 bigint, primary key(id)) ` +
		`create index mn as select min_ever(col2) from t1 group by col1 with attributes LEGACY_EXTREMUM_EVER ` +
		`create index mx as select max_ever(col2) from t1 group by col1 with attributes LEGACY_EXTREMUM_EVER`},
	{"extremum_ever_tuple", `create table t1(id bigint, col1 bigint, col2 bigint, primary key(id)) ` +
		`create index mn as select min_ever(col2) from t1 group by col1 create index mx as select max_ever(col2) from t1 group by col1`},
}

const wsjStructT = `create type as struct sc(x bigint, y bigint) create table t(id bigint, s sc, ts bigint, primary key(id)) `

// wsjUnsplittable are the corpus templates Go's grammar cannot parse, so no
// clause boundary exists to derive isolation or view-free bodies from. It is
// pinned so that a grammar change which makes one parseable fails loudly here
// (its derived runs then need pins) instead of silently growing the run set.
var wsjUnsplittable = map[string]bool{
	"guardiann-semantic-search.yamsql:32":      true,
	"isolation-level-snapshot.yamsql:79":       true,
	"schema-template-stored-queries.yamsql:32": true,
	"vector-engine-preference.yamsql:40":       true,
}

// wsjRun is one template body driven through both engines.
type wsjRun struct {
	id      string
	class   string
	javaPin string
	goPin   string // Go's outcome: "OK <digest of its canonical MetaData>" or "ERROR <code> <digest of its message>"
	lines   []string
}

// wsjShortDigest is a short content digest of a (scrubbed) text, for pinning
// Go's rejection messages without carrying their text in the pin file.
func wsjShortDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum[:6])
}

// wsjClause is one CREATE clause of a schema-template body, located through the
// TYPED parse tree (TemplateClauseContext) and addressed by its code-point span
// in the body. ANTLR intervals index CODE POINTS, not bytes, so every slice is
// over runes; a byte slice would cut a body with non-ASCII identifiers
// mid-clause.
type wsjClause struct {
	kind  string // "index", "view", "function" or "other" (type, enum, table)
	name  string // declared name for an index, view or function (wsjIdent)
	label string // the declared name as written, for run ids: two index
	// names that resolve alike under case folding stay distinct in a
	// case-sensitive template
	from, to int
	refs     map[string]bool // every identifier the clause mentions (wsjIdent)
}

// wsjParsedTemplate is one CREATE SCHEMA TEMPLATE body split into its clauses
// (see the parse function below, whose ok is false when Go's grammar cannot parse
// the body: that rejection is then the recorded Go outcome, and there is no clause
// boundary to cut at).
type wsjParsedTemplate struct {
	runes   []rune
	clauses []wsjClause
	tail    string // text after the last clause: the template WITH OPTIONS clause
}

// wsjIdent is an identifier as the relational layer resolves it: a
// double-quoted identifier keeps its case, anything else is upper-cased.
func wsjIdent(u antlrgen.IUidContext) string {
	if u == nil {
		return ""
	}
	if uc, ok := u.(*antlrgen.UidContext); ok && uc.DOUBLE_QUOTE_ID() != nil {
		return strings.Trim(uc.GetText(), `"`)
	}
	return strings.ToUpper(u.GetText())
}

func wsjFullIdent(f antlrgen.IFullIdContext) string {
	if f == nil {
		return ""
	}
	var parts []string
	for _, u := range f.AllUid() {
		parts = append(parts, wsjIdent(u))
	}
	return strings.Join(parts, ".")
}

func wsjParseTemplate(body string) (wsjParsedTemplate, bool) {
	const prefix = "CREATE SCHEMA TEMPLATE wsj_split "
	root, err := parser.Parse(prefix + body)
	if err != nil {
		return wsjParsedTemplate{}, false
	}
	off := len([]rune(prefix))
	out := wsjParsedTemplate{runes: []rune(body)}
	var stmt *antlrgen.CreateSchemaTemplateStatementContext
	var find func(antlr.Tree)
	find = func(n antlr.Tree) {
		if stmt != nil {
			return
		}
		if c, ok := n.(*antlrgen.CreateSchemaTemplateStatementContext); ok {
			stmt = c
			return
		}
		for i := 0; i < n.GetChildCount(); i++ {
			find(n.GetChild(i))
		}
	}
	find(root)
	if stmt == nil {
		return wsjParsedTemplate{}, false
	}
	last := 0
	for _, tc := range stmt.AllTemplateClause() {
		c := wsjClause{
			kind: "other",
			from: tc.GetStart().GetStart() - off,
			to:   tc.GetStop().GetStop() - off + 1,
			refs: map[string]bool{},
		}
		if id := tc.IndexDefinition(); id != nil {
			c.kind = "index"
			switch d := id.(type) {
			case *antlrgen.IndexAsSelectDefinitionContext:
				c.name, c.label = wsjIdent(d.GetIndexName()), d.GetIndexName().GetText()
			case *antlrgen.IndexOnSourceDefinitionContext:
				c.name, c.label = wsjIdent(d.GetIndexName()), d.GetIndexName().GetText()
			case *antlrgen.VectorIndexDefinitionContext:
				c.name, c.label = wsjIdent(d.GetIndexName()), d.GetIndexName().GetText()
			}
		} else if v := tc.ViewDefinition(); v != nil {
			c.kind = "view"
			c.name = wsjFullIdent(v.(*antlrgen.ViewDefinitionContext).GetViewName())
		} else if f := tc.SqlInvokedFunction(); f != nil {
			c.kind = "function"
			c.name = wsjFullIdent(f.FunctionSpecification().GetSchemaQualifiedRoutineName())
		}
		var walk func(antlr.Tree)
		walk = func(n antlr.Tree) {
			if u, ok := n.(*antlrgen.UidContext); ok {
				c.refs[wsjIdent(u)] = true
			}
			for i := 0; i < n.GetChildCount(); i++ {
				walk(n.GetChild(i))
			}
		}
		walk(tc)
		delete(c.refs, c.name)
		out.clauses = append(out.clauses, c)
		last = c.to
	}
	out.tail = strings.TrimSpace(string(out.runes[last:]))
	return out, true
}

func (t wsjParsedTemplate) text(c wsjClause) string { return string(t.runes[c.from:c.to]) }

// assemble joins the kept clauses in their original order, then the tail.
func (t wsjParsedTemplate) assemble(keep func(int, wsjClause) bool) string {
	var parts []string
	for i, c := range t.clauses {
		if keep(i, c) {
			parts = append(parts, t.text(c))
		}
	}
	if t.tail != "" {
		parts = append(parts, t.tail)
	}
	return strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
}

// withDependencies is the set of VIEW and FUNCTION clause positions the clauses
// in seed mention, closed transitively (a view over a function over a view).
func (t wsjParsedTemplate) withDependencies(seed map[int]bool) map[int]bool {
	byName := map[string]int{}
	for i, c := range t.clauses {
		if c.kind == "view" || c.kind == "function" {
			byName[c.name] = i
		}
	}
	keep := map[int]bool{}
	var add func(int)
	add = func(i int) {
		if keep[i] {
			return
		}
		keep[i] = true
		for r := range t.clauses[i].refs {
			if j, ok := byName[r]; ok {
				add(j)
			}
		}
	}
	for i := range seed {
		add(i)
	}
	return keep
}

// wsjStripViewsAndFunctions returns body with every CREATE VIEW and CREATE
// FUNCTION clause removed, together with every index clause that depends on one
// of them (directly or through another view or function), so the derived body
// measures the remaining indexes instead of failing on a dangling reference.
// cutIndexes names the dependent indexes removed. ok is false when there is no
// view or function clause, or when Go's grammar cannot parse the body.
func wsjStripViewsAndFunctions(body string) (derived string, cutIndexes []string, ok bool) {
	t, parsed := wsjParseTemplate(body)
	if !parsed {
		return body, nil, false
	}
	hasVF := false
	for _, c := range t.clauses {
		if c.kind == "view" || c.kind == "function" {
			hasVF = true
		}
	}
	if !hasVF {
		return body, nil, false
	}
	dependent := map[int]bool{}
	for i, c := range t.clauses {
		if c.kind != "index" {
			continue
		}
		deps := t.withDependencies(map[int]bool{i: true})
		if len(deps) > 1 {
			dependent[i] = true
			cutIndexes = append(cutIndexes, c.name)
		}
	}
	derived = t.assemble(func(i int, c wsjClause) bool {
		return c.kind == "other" || (c.kind == "index" && !dependent[i])
	})
	return derived, cutIndexes, true
}

// wsjIsolateIndexes returns, for a body with at least two index clauses, one
// body per index: every type, enum and table clause, that one index, the views
// and functions it depends on, and the template options. A template one engine
// rejects hides every index it declares; isolation measures each of them. The
// derivation is structural, never conditioned on either engine's outcome, so the
// set of runs (and therefore of pins) does not move when Go's behaviour does.
func wsjIsolateIndexes(body string) (names, bodies []string) {
	t, parsed := wsjParseTemplate(body)
	if !parsed {
		return nil, nil
	}
	var idx []int
	for i, c := range t.clauses {
		if c.kind == "index" {
			idx = append(idx, i)
		}
	}
	if len(idx) < 2 {
		return nil, nil
	}
	for _, k := range idx {
		deps := t.withDependencies(map[int]bool{k: true})
		names = append(names, t.clauses[k].label)
		bodies = append(bodies, t.assemble(func(i int, c wsjClause) bool {
			return c.kind == "other" || deps[i]
		}))
	}
	return names, bodies
}

var _ = Describe("WS-J index-definition fidelity oracle", func() {
	It("records every target-corpus index template on both engines", func() {
		ctx := context.Background()
		java := NewJavaInvoker()
		db := recordlayer.NewFDBDatabase(sharedDB)
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())

		corpus := loadWSJCorpus()
		Expect(corpus.Source.Tag).To(Equal("4.14.2.0"), "the harvest must come from the target tag")
		// Population guards: a harvest that silently lost templates would make
		// every row below vacuously agree. The census reconciles the harvest with
		// every index-statement occurrence in the corpus (harvest.py refuses to
		// write when one is unaccounted for); the re-count below keeps a
		// hand-edited corpus file from carrying a stale census.
		Expect(corpus.Templates).To(HaveLen(81))
		Expect(corpus.Census.Occurrences).To(Equal(360))
		Expect(corpus.Census.Occurrences).To(Equal(corpus.Census.InTemplateBodies+len(corpus.Census.OnCommentLines)),
			"every corpus occurrence is in a harvested body or on a comment line")
		recount := 0
		for _, t := range corpus.Templates {
			recount += len(wsjIndexStatementRE.FindAllStringIndex(t.Body, -1))
		}
		Expect(recount).To(Equal(corpus.Census.HarvestedStatements))
		// harvest.py derives variant_duplicates as harvested - in_bodies, so the sum
		// cannot fail; what can is the corpus growing a versioned template, whose
		// variants repeat statements. None does at 4.14.2.0.
		Expect(corpus.Census.VariantDuplicates).To(Equal(0), "a versioned template entered the corpus: its variants repeat index statements")
		Expect(corpus.Census.HarvestedStatements).To(Equal(corpus.Census.InTemplateBodies))
		javaPins := loadWSJJavaPins()

		seq := 0
		goBuiltTwice := 0
		run := func(id, body string) wsjRun {
			seq++
			r := wsjRun{id: id}
			templateName := fmt.Sprintf("WSJ%03d_%s", seq, uuid.New().String()[:8])
			// Both engines echo the template name into some messages, and the
			// target echoes a per-plan quantifier alias ("q" + a UUID with
			// underscores) into some rejections; both are per-run random, so they
			// are replaced before anything is printed.
			scrub := func(msg string) string {
				return wsjQuantifierAliasRE.ReplaceAllString(strings.ReplaceAll(msg, templateName, "<T>"), "<alias>")
			}
			var created struct {
				Created bool `json:"created"`
			}
			jerr := java.InvokeAs(ctx, "createSchemaTemplatePersistentJava", map[string]any{
				"clusterFile":        clusterFile,
				"templateName":       templateName,
				"schemaTemplateBody": body,
			}, &created)

			goTmpl, goErr := embedded.BuildSchemaTemplateFromDDLNamed(body, templateName)
			goSide := "OK"
			if goErr != nil {
				var ae *api.Error
				if errors.As(goErr, &ae) {
					goSide = fmt.Sprintf("ERROR %s %q", string(ae.Code), wsjTrim(scrub(ae.Message)))
					r.goPin = fmt.Sprintf("ERROR %s %s", string(ae.Code), wsjShortDigest(scrub(ae.Message)))
				} else {
					goSide = fmt.Sprintf("ERROR %q", wsjTrim(scrub(goErr.Error())))
					r.goPin = "ERROR " + wsjShortDigest(scrub(goErr.Error()))
				}
			} else {
				// Go's own outcome is pinned beside its class: the digest of the WHOLE
				// canonical MetaData Go stores, so a change inside a run whose class
				// does not move (a column type inside a metadata-diverge run, a root
				// inside an index-diverge one) still reddens. The digest is sound only
				// if a build is a function of its input, so every accepted body is
				// built twice here and the two stored byte strings must be equal.
				first, firstErr := goTmpl.Underlying().ToProto()
				Expect(firstErr).NotTo(HaveOccurred())
				again, againErr := embedded.BuildSchemaTemplateFromDDLNamed(body, templateName)
				Expect(againErr).NotTo(HaveOccurred(), "a second Go build of %s", id)
				second, secondErr := again.Underlying().ToProto()
				Expect(secondErr).NotTo(HaveOccurred())
				fb, fbErr := proto.MarshalOptions{Deterministic: true}.Marshal(first)
				Expect(fbErr).NotTo(HaveOccurred())
				sb, sbErr := proto.MarshalOptions{Deterministic: true}.Marshal(second)
				Expect(sbErr).NotTo(HaveOccurred())
				Expect(bytes.Equal(fb, sb)).To(BeTrue(), "two Go builds of %s stored different bytes", id)
				r.goPin = "OK " + wsjJavaDigest(first, templateName)
				goBuiltTwice++
			}

			if jerr != nil {
				var je *JavaError
				if errors.As(jerr, &je) {
					r.javaPin = fmt.Sprintf("ERROR %s %s", je.SQLState, je.ExceptionClass)
					r.lines = append(r.lines, fmt.Sprintf("WSJ %s java=ERROR %s %s %q go=%s",
						id, je.SQLState, je.ExceptionClass, wsjTrim(scrub(je.Message)), goSide))
				} else {
					r.javaPin = "ERROR " + wsjTrim(scrub(jerr.Error()))
					r.lines = append(r.lines, fmt.Sprintf("WSJ %s java=ERROR %q go=%s", id, wsjTrim(scrub(jerr.Error())), goSide))
				}
				r.class = "both-reject"
				if goErr == nil {
					r.class = "java-reject-go-accept"
				}
				return r
			}
			Expect(created.Created).To(BeTrue())

			javaMD := loadStoredJavaTemplateMetaData(ctx, db, templateName)
			var dropped struct {
				Dropped bool `json:"dropped"`
			}
			_ = java.InvokeAs(ctx, "dropSchemaTemplatePersistentJava", map[string]any{
				"clusterFile":  clusterFile,
				"templateName": templateName,
			}, &dropped)
			r.javaPin = fmt.Sprintf("OK %d %s", len(javaMD.GetIndexes()), wsjJavaDigest(javaMD, templateName))
			// Java's stored roots for every hand-written shape it accepts,
			// whatever Go does: the port's unit tests pin these verbatim.
			if strings.HasPrefix(id, "shape:") {
				for _, i := range javaMD.GetIndexes() {
					r.lines = append(r.lines, fmt.Sprintf("WSJ-JAVA-ROOT %s %s type=%s options=%v root={%v} predicate={%v}",
						id, i.GetName(), i.GetType(), normalizedIndexOptions(i), i.GetRootExpression(), i.GetPredicate()))
				}
			}

			if goErr != nil {
				r.class = "java-accept-go-reject"
				r.lines = append(r.lines, fmt.Sprintf("WSJ %s java=OK(%d) go=%s", id, len(javaMD.GetIndexes()), goSide))
				return r
			}
			goMD, protoErr := goTmpl.Underlying().ToProto()
			Expect(protoErr).NotTo(HaveOccurred())

			javaIdx := map[string]*gen.Index{}
			for _, i := range javaMD.GetIndexes() {
				javaIdx[i.GetName()] = i
			}
			goIdx := map[string]*gen.Index{}
			for _, i := range goMD.GetIndexes() {
				goIdx[i.GetName()] = i
			}
			// Go-only indexes: a verified RFC-209 group-existence companion of an
			// index the target also stores is a declared Go extension; anything
			// else is an index divergence.
			var extra []string
			companions := map[string]bool{}
			nonCompanionExtra := 0
			for _, name := range sortedKeys(goIdx) {
				if _, ok := javaIdx[name]; ok {
					continue
				}
				owner, isCompanion := strings.CutSuffix(name, recordlayer.GroupCountCompanionSuffix)
				if jo, ok := javaIdx[owner]; isCompanion && ok {
					ok2, why := isGroupExistenceCompanionOf(goIdx[name], jo, goMD.GetVersion())
					if ok2 {
						extra = append(extra, name+"(companion)")
						companions[name] = true
						continue
					}
					extra = append(extra, name+"(NOT-companion: "+why+")")
				} else {
					extra = append(extra, name)
				}
				nonCompanionExtra++
			}
			// Everything below compares the target's metadata with Go's as it
			// would be without the companions: each companion takes one metadata
			// version when its owner is added, so removing it gives back that
			// version (wsjRemoveCompanions). The raw bytes are still reported.
			goCmp := goMD
			if len(companions) > 0 {
				goCmp = wsjRemoveCompanions(goMD, companions)
			}
			goCmpIdx := map[string]*gen.Index{}
			for _, i := range goCmp.GetIndexes() {
				goCmpIdx[i.GetName()] = i
			}
			var equal int
			var diffs, missing []string
			for _, name := range sortedKeys(javaIdx) {
				g, ok := goCmpIdx[name]
				if !ok {
					missing = append(missing, name)
					continue
				}
				if dims := wsjIndexDims(g, javaIdx[name]); len(dims) > 0 {
					diffs = append(diffs, name+":"+strings.Join(dims, "+"))
				} else {
					equal++
				}
			}
			mdBytes := "md-bytes-equal"
			jb, jErr := proto.MarshalOptions{Deterministic: true}.Marshal(javaMD)
			Expect(jErr).NotTo(HaveOccurred())
			gb, gErr := proto.MarshalOptions{Deterministic: true}.Marshal(goMD)
			Expect(gErr).NotTo(HaveOccurred())
			if string(jb) != string(gb) {
				mdBytes = "md-bytes-differ(" + strings.Join(wsjMetaDataFieldDiff(goMD, javaMD), ",") + ")"
			}
			mdCanonicalEqual := proto.Equal(wsjCanonical(goCmp), wsjCanonical(javaMD))
			switch {
			case mdBytes == "md-bytes-equal":
			case mdCanonicalEqual:
				mdBytes += " md-canonical-equal"
			default:
				mdBytes += " md-canonical-differ(" + strings.Join(wsjMetaDataFieldDiff(wsjCanonical(goCmp), wsjCanonical(javaMD)), ",") + ")"
			}
			// The class is decided by the WHOLE stored metadata, not the index
			// comparison alone: a run whose indexes all agree while its record-type
			// keys, union numbers, versions or descriptor order differ after
			// canonicalisation is a wire divergence, and is classed as one. A run
			// that is equal once its verified companions are removed has its own
			// class, so the equal class means byte-for-byte what the target stores
			// (up to wsjCanonical).
			switch {
			case len(diffs) > 0 || len(missing) > 0 || nonCompanionExtra > 0:
				r.class = "both-accept-index-diverge"
			case !mdCanonicalEqual:
				r.class = "both-accept-metadata-diverge"
			case len(companions) > 0:
				r.class = "both-accept-companion"
			default:
				r.class = "both-accept-equal"
			}
			r.lines = append(r.lines, fmt.Sprintf("WSJ %s java=OK(%d) go=OK(%d) equal=%d diff=%v missing=%v extra=%v version(go=%d java=%d) %s class=%s",
				id, len(javaMD.GetIndexes()), len(goMD.GetIndexes()), equal, diffs, missing, extra,
				goMD.GetVersion(), javaMD.GetVersion(), mdBytes, r.class))
			if mdBytes != "md-bytes-equal" {
				r.lines = append(r.lines, "WSJ-DETAIL "+id+" "+wsjStructureDetail(goMD, javaMD))
			}
			return r
		}

		javaOutcome := map[string]string{}
		goClass := map[string]string{}
		tally := map[string]int{}
		var lines []string
		outcomes := 0
		record := func(r wsjRun) {
			javaOutcome[r.id] = r.javaPin
			Expect(r.goPin).NotTo(BeEmpty(), "run %s has no Go outcome", r.id)
			goClass[r.id] = r.class + " " + r.goPin
			tally[r.class]++
			lines = append(lines, r.lines...)
			outcomes++
		}
		unsplittable := map[string]bool{}
		for _, t := range corpus.Templates {
			record(run(t.id(), t.Body))
			if _, parsed := wsjParseTemplate(t.Body); !parsed {
				unsplittable[t.id()] = true
				continue
			}
			// Derived runs are STRUCTURAL: whenever the body has a VIEW or
			// FUNCTION clause, and for every index of a multi-index body, whatever
			// either engine did with the full body.
			if derived, cut, ok := wsjStripViewsAndFunctions(t.Body); ok {
				if len(cut) > 0 {
					lines = append(lines, fmt.Sprintf("WSJ-CUT %s{-views-functions} dependent indexes %v", t.id(), cut))
				}
				record(run(t.id()+"{-views-functions}", derived))
			}
			names, bodies := wsjIsolateIndexes(t.Body)
			for k := range names {
				record(run(t.id()+"{index:"+names[k]+"}", bodies[k]))
			}
		}
		for _, sh := range wsjShapes {
			record(run("shape:"+sh.name, sh.body))
		}
		for _, l := range lines {
			fmt.Fprintln(GinkgoWriter, l)
		}
		fmt.Fprintf(GinkgoWriter, "WSJ-TALLY %v\n", tally)
		fmt.Fprintf(GinkgoWriter, "WSJ-UNSPLITTABLE %v\n", sortedBoolKeys(unsplittable))
		for _, id := range sortedStringKeys(javaOutcome) {
			fmt.Fprintf(GinkgoWriter, "WSJ-JAVA %q: %q,\n", id, javaOutcome[id])
		}
		Expect(sortedBoolKeys(unsplittable)).To(Equal(sortedBoolKeys(wsjUnsplittable)),
			"the templates Go's grammar cannot split moved: a newly parseable template needs pins for its derived runs")
		// Every run is pinned, and every pin is run: a pin without a run is a
		// template the harvest lost, a run without a pin an unreviewed one.
		Expect(sortedStringKeys(javaOutcome)).To(Equal(sortedStringKeys(javaPins)),
			"the set of runs must equal the set of pinned Java outcomes")
		for id, got := range javaOutcome {
			Expect(got).To(Equal(javaPins[id]), "Java outcome for %s moved", id)
		}
		// The ratchet: every run's class AND Go's own outcome are pinned
		// (wsj_go_classes.json, "<class> OK <digest>" or "<class> ERROR <code>
		// <message digest>"), so a run that stops being both-accept-equal reddens
		// here even where no unit golden covers it, a change inside a class that
		// does not move reddens too, and a run that becomes equal is a reviewed pin
		// update. Every Go-accepted body was built twice and stored equal bytes.
		fmt.Fprintf(GinkgoWriter, "WSJ-GO-BUILT-TWICE %d\n", goBuiltTwice)
		Expect(goBuiltTwice).To(BeNumerically(">", 300), "the double-build determinism check ran over the accepted runs")
		for _, id := range sortedStringKeys(goClass) {
			fmt.Fprintf(GinkgoWriter, "WSJ-CLASS %q: %q,\n", id, goClass[id])
		}
		classPins := loadWSJPinFile("wsj_go_classes.json")
		Expect(sortedStringKeys(goClass)).To(Equal(sortedStringKeys(classPins)),
			"the set of runs must equal the set of pinned classes")
		var moved []string
		for _, id := range sortedStringKeys(goClass) {
			if goClass[id] != classPins[id] {
				moved = append(moved, fmt.Sprintf("%s: %s -> %s", id, classPins[id], goClass[id]))
			}
		}
		Expect(moved).To(BeEmpty(), "run classes or Go outcomes moved from their pins")
		wsjLines := 0
		for _, l := range lines {
			if strings.HasPrefix(l, "WSJ ") {
				wsjLines++
			}
		}
		Expect(wsjLines).To(Equal(outcomes), "every run must produce exactly one outcome line")
		Expect(javaOutcome).To(HaveLen(outcomes), "run ids are unique")
		Expect(outcomes).To(BeNumerically(">=", len(corpus.Templates)+len(wsjShapes)))
	})
})

func sortedBoolKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedStringKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The query-side half of the literal-width finding. Java's bitmap bucketing
// functions exist only as ArithmeticValue _LI and _II operators
// (ArithmeticValue.java:515-522): the entry size is an INT literal
// (SemanticAnalyzer BITMAP_DEFAULT_ENTRY_SIZE), and over an INTEGER column the
// result is INT with int32 multiplyExact/subtractExact. The bit operators
// likewise pick an INT or LONG lane from their operands, and operand types with
// no lane (DOUBLE, STRING) have no physical operator at all. Java's result type,
// value or rejection is PINNED per probe (wsjQueryPins, set-equal to the
// probes); the Go line is recorded for the port.
var wsjQueryPins = map[string]string{
	"bit_position_int":        "OK [INTEGER] [[2345]]",
	"bit_position_int_min":    "ERROR XXXXX ArithmeticException \"integer overflow\"",
	"bitand_array_int":        "ERROR XX000 SemanticException \"The argument to an arithmetic operator expecting an argument of a primitive type, is invoked with an argument of a complex type, e.g. an array or a record.\"",
	"bitand_cast_null_int":    "OK [BIGINT] [[NULL]]",
	"bitand_double_int":       "ERROR XX000 VerifyException \"unable to encapsulate arithmetic operation due to type mismatch(es)\"",
	"bitand_enum_int":         "ERROR XX000 SemanticException \"The argument to an arithmetic operator expecting an argument of a primitive type, is invoked with an argument of a complex type, e.g. an array or a record.\"",
	"bitand_int_literal":      "OK [INTEGER] [[1]]",
	"bitand_int_long_column":  "OK [BIGINT] [[12345]]",
	"bitand_int_long_literal": "OK [BIGINT] [[4096]]",
	"bitand_long_literal":     "OK [BIGINT] [[1]]",
	"bitand_null_int":         "ERROR XX000 VerifyException \"unable to encapsulate arithmetic operation due to type mismatch(es)\"",
	"bitand_string_int":       "ERROR XX000 VerifyException \"unable to encapsulate arithmetic operation due to type mismatch(es)\"",
	"bitand_uuid_int":         "ERROR XX000 SemanticException \"The argument to an arithmetic operator expecting an argument of a primitive type, is invoked with an argument of a complex type, e.g. an array or a record.\"",
	"bitor_int_literal":       "OK [INTEGER] [[12347]]",
	"bitxor_int_literal":      "OK [INTEGER] [[12346]]",
	"bucket_number_int":       "ERROR 0AF00 RelationalException \"Unsupported operator bitmap_bucket_number\"",
	"bucket_offset_double":    "ERROR XX000 VerifyException \"unable to encapsulate arithmetic operation due to type mismatch(es)\"",
	"bucket_offset_enum":      "ERROR XX000 SemanticException \"The argument to an arithmetic operator expecting an argument of a primitive type, is invoked with an argument of a complex type, e.g. an array or a record.\"",
	"bucket_offset_int":       "OK [INTEGER] [[10000]]",
	"bucket_offset_int_min":   "ERROR XXXXX ArithmeticException \"integer overflow\"",
	"bucket_offset_long":      "OK [BIGINT] [[10000]]",
	"bucket_offset_long_min":  "ERROR XXXXX ArithmeticException \"long overflow\"",
	"bucket_offset_string":    "ERROR XX000 VerifyException \"unable to encapsulate arithmetic operation due to type mismatch(es)\"",
	"mod_int_literal":         "OK [INTEGER] [[4]]",
}

// wsjValue renders one result value exactly: an integral number prints all its
// digits (the Java runner's JSON decodes numbers as float64, exact to 2^53), any
// other float its shortest round-trip form at its own width.
func wsjValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return strconv.FormatInt(x, 10)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int:
		return strconv.Itoa(x)
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'g', -1, 32)
	default:
		return fmt.Sprintf("%v", x)
	}
}

func wsjRows(rows [][]any) string {
	var out []string
	for _, r := range rows {
		var cells []string
		for _, v := range r {
			cells = append(cells, wsjValue(v))
		}
		out = append(out, "["+strings.Join(cells, " ")+"]")
	}
	return "[" + strings.Join(out, " ") + "]"
}

var _ = Describe("WS-J literal-width query oracle", func() {
	It("pins bitmap and bit-operator result types, lanes and overflow edges", func() {
		ctx := context.Background()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "ws_j_"+uuid.New().String())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		clusterFilePath := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFilePath)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFilePath)

		schema := "CREATE TABLE T (id BIGINT, i INTEGER, l BIGINT, d DOUBLE, s STRING, PRIMARY KEY (id))"
		setup := []string{
			"INSERT INTO T VALUES (1, -2147483648, -9223372036854775808, -1.5, 'a'), (2, 12345, 12345, 12345.75, 'b'), " +
				"(3, 2147483647, 9223372036854775807, 1.5, 'c')",
		}
		probes := []struct{ name, sql string }{
			{"bucket_offset_int", "SELECT bitmap_bucket_offset(i) FROM T WHERE id = 2"},
			{"bucket_offset_long", "SELECT bitmap_bucket_offset(l) FROM T WHERE id = 2"},
			{"bit_position_int", "SELECT bitmap_bit_position(i) FROM T WHERE id = 2"},
			{"bucket_offset_int_min", "SELECT bitmap_bucket_offset(i) FROM T WHERE id = 1"},
			{"bucket_offset_long_min", "SELECT bitmap_bucket_offset(l) FROM T WHERE id = 1"},
			{"bit_position_int_min", "SELECT bitmap_bit_position(i) FROM T WHERE id = 1"},
			{"bitand_int_literal", "SELECT i & 1 FROM T WHERE id = 2"},
			{"bitand_long_literal", "SELECT l & 1 FROM T WHERE id = 2"},
			{"bitor_int_literal", "SELECT i | 2 FROM T WHERE id = 2"},
			{"bitxor_int_literal", "SELECT i ^ 3 FROM T WHERE id = 2"},
			{"bitand_int_long_literal", "SELECT i & 3000000000 FROM T WHERE id = 2"},
			{"bitand_int_long_column", "SELECT i & l FROM T WHERE id = 2"},
			{"bitand_double_int", "SELECT d & 1 FROM T WHERE id = 2"},
			{"bitand_string_int", "SELECT s & 1 FROM T WHERE id = 2"},
			{"bucket_offset_double", "SELECT bitmap_bucket_offset(d) FROM T WHERE id = 2"},
			{"bucket_offset_string", "SELECT bitmap_bucket_offset(s) FROM T WHERE id = 2"},
			{"bucket_number_int", "SELECT bitmap_bucket_number(i) FROM T WHERE id = 2"},
			{"mod_int_literal", "SELECT i % 7 FROM T WHERE id = 2"},
		}
		render := func(r plandiff.RunResult) string {
			if r.Err != nil {
				var je *plandiff.JavaError
				if errors.As(r.Err, &je) {
					return fmt.Sprintf("ERROR %s %s %q", je.SQLState, je.ExceptionClass, wsjTrim(je.Message))
				}
				var ge *api.Error
				if errors.As(r.Err, &ge) {
					return fmt.Sprintf("ERROR %s %q", string(ge.Code), wsjTrim(ge.Message))
				}
				return fmt.Sprintf("ERROR %q", wsjTrim(r.Err.Error()))
			}
			var types []string
			for _, c := range r.Rows.Columns {
				types = append(types, c.Type)
			}
			return fmt.Sprintf("OK %v %s", types, wsjRows(r.Rows.Rows))
		}
		got := map[string]string{}
		for _, p := range probes {
			jr := javaRunner.RunWithSetup(ctx, schema, setup, p.sql)
			gr := goRunner.RunWithSetup(ctx, schema, setup, p.sql)
			got[p.name] = render(jr)
			fmt.Fprintf(GinkgoWriter, "WSJQ %s java=%s go=%s\n", p.name, render(jr), render(gr))
		}
		// Lane resolution's FIRST check: an operand that is not primitive (ENUM, UUID,
		// ARRAY, RECORD) is a SemanticException before any lane is looked up
		// (ArithmeticValue.java:215-220), and a NULL operand has no lane. Its own
		// schema, because Go's DDL cannot declare the enum yet (WS-J F6).
		complexSchema := "CREATE TYPE AS ENUM color ('RED', 'GREEN') " +
			"CREATE TABLE C (id BIGINT, e color, u UUID, a BIGINT ARRAY, n BIGINT, PRIMARY KEY (id))"
		complexSetup := []string{"INSERT INTO C VALUES (1, 'RED', '123e4567-e89b-12d3-a456-426614174000', [1, 2], 3)"}
		complexProbes := []struct{ name, sql string }{
			{"bitand_enum_int", "SELECT e & 1 FROM C WHERE id = 1"},
			{"bitand_uuid_int", "SELECT u & 1 FROM C WHERE id = 1"},
			{"bitand_array_int", "SELECT a & 1 FROM C WHERE id = 1"},
			{"bitand_null_int", "SELECT NULL & 1 FROM C WHERE id = 1"},
			{"bitand_cast_null_int", "SELECT CAST(NULL AS BIGINT) & 1 FROM C WHERE id = 1"},
			{"bucket_offset_enum", "SELECT bitmap_bucket_offset(e) FROM C WHERE id = 1"},
		}
		for _, p := range complexProbes {
			jr := javaRunner.RunWithSetup(ctx, complexSchema, complexSetup, p.sql)
			gr := goRunner.RunWithSetup(ctx, complexSchema, complexSetup, p.sql)
			got[p.name] = render(jr)
			fmt.Fprintf(GinkgoWriter, "WSJQ %s java=%s go=%s\n", p.name, render(jr), render(gr))
		}
		for _, name := range sortedStringKeys(got) {
			fmt.Fprintf(GinkgoWriter, "WSJQ-JAVA %q: %q,\n", name, got[name])
		}
		Expect(got).To(HaveLen(len(probes)+len(complexProbes)), "probe names are unique")
		Expect(sortedStringKeys(got)).To(Equal(sortedStringKeys(wsjQueryPins)), "every probe is pinned, every pin probed")
		for name, v := range got {
			Expect(v).To(Equal(wsjQueryPins[name]), "Java result for %s moved", name)
		}
	})
})

// Whether the target SERVES a GROUP BY over nested struct fields from an
// aggregate index. Go stores such an index since the field-path trie fix, and
// its planner does not use it (TestAggregateIndexResidual_RecordTypedGrouping
// KeyIsUnreachable guards that, because RFC-248's residual ordinal rewrite has
// no rows pin over a leaf-expanded grouping key). The Java tree decides whether
// that is parity or a missing plan.
var _ = Describe("WS-J nested-grouping aggregate index plan oracle", func() {
	It("records both engines' plans for GROUP BY over nested fields", func() {
		ctx := context.Background()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "ws_j_plan_"+uuid.New().String())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		java := plandiff.NewJavaEngineHTTP(javaBaseURL(srv), env.ClusterFile)
		goEngine := plandiff.NewGoEngine()
		schema := "CREATE TYPE AS STRUCT ADDR (city STRING, zip BIGINT) " +
			"CREATE TABLE T_S (id BIGINT, home ADDR, cat STRING, v BIGINT, PRIMARY KEY (id)) " +
			"CREATE INDEX cnt_home_cat AS SELECT COUNT(*) FROM T_S GROUP BY home.city, home.zip, cat " +
			"CREATE INDEX sum_home_cat AS SELECT SUM(v) FROM T_S GROUP BY home.city, home.zip, cat " +
			"CREATE INDEX cnt_cat AS SELECT COUNT(*) FROM T_S GROUP BY cat"
		reads := []string{
			"SELECT cat, COUNT(*) FROM T_S GROUP BY cat",
			"SELECT home.city, home.zip, cat, COUNT(*) FROM T_S GROUP BY home.city, home.zip, cat",
			"SELECT home.city, home.zip, cat, COUNT(*) FROM T_S WHERE cat = 'x' GROUP BY home.city, home.zip, cat",
			"SELECT home.city, home.zip, cat, SUM(v) FROM T_S WHERE home.city = 'a' GROUP BY home.city, home.zip, cat",
			"SELECT home, cat, COUNT(*) FROM T_S GROUP BY home, cat",
		}
		// Measured Java trees (4.14.2.0). The target serves a GROUP BY over nested
		// LEAF fields from the aggregate index, with a grouping-key residual as a
		// FILTER on the leaf ordinal above the scan, and cannot plan a RECORD-typed
		// grouping key at all.
		wantJava := map[string]string{
			"SELECT cat, COUNT(*) FROM T_S GROUP BY cat":                                                               "AISCAN(CNT_CAT <,> BY_GROUP -> [_0: KEY:[0], _1: VALUE:[0]]) | MAP (_._0 AS CAT, _._1 AS _1)",
			"SELECT home.city, home.zip, cat, COUNT(*) FROM T_S GROUP BY home.city, home.zip, cat":                     "AISCAN(CNT_HOME_CAT <,> BY_GROUP -> [_0: KEY:[0], _1: KEY:[1], _2: KEY:[2], _3: VALUE:[0]]) | MAP (_._0 AS CITY, _._1 AS ZIP, _._2 AS CAT, _._3 AS _3)",
			"SELECT home.city, home.zip, cat, COUNT(*) FROM T_S WHERE cat = 'x' GROUP BY home.city, home.zip, cat":     "AISCAN(CNT_HOME_CAT <,> BY_GROUP -> [_0: KEY:[0], _1: KEY:[1], _2: KEY:[2], _3: VALUE:[0]]) | FILTER _._2 EQUALS promote(@c20 AS STRING) | MAP (_._0 AS CITY, _._1 AS ZIP, _._2 AS CAT, _._3 AS _3)",
			"SELECT home.city, home.zip, cat, SUM(v) FROM T_S WHERE home.city = 'a' GROUP BY home.city, home.zip, cat": "AISCAN(SUM_HOME_CAT [EQUALS promote(@c22 AS STRING)] BY_GROUP -> [_0: KEY:[0], _1: KEY:[1], _2: KEY:[2], _3: VALUE:[0]]) | MAP (_._0 AS CITY, _._1 AS ZIP, _._2 AS CAT, _._3 AS _3)",
			"SELECT home, cat, COUNT(*) FROM T_S GROUP BY home, cat":                                                   "ERROR plandiff: java UnableToPlanException: Cascades planner could not plan query",
		}
		n := 0
		for i, q := range reads {
			query := plandiff.Query{Name: fmt.Sprintf("wsj_nested_%d", i), SQL: q, SchemaTemplate: schema}
			jr := java.Plan(ctx, query)
			gr := goEngine.Plan(ctx, query)
			render := func(r plandiff.PlanResult) string {
				if r.Err != nil {
					return "ERROR " + wsjTrim(r.Err.Error())
				}
				return strings.Join(strings.Fields(r.Tree), " ")
			}
			fmt.Fprintf(GinkgoWriter, "WSJP %q\n  java=%s\n  go=%s\n", q, render(jr), render(gr))
			Expect(render(jr)).To(Equal(wantJava[q]), "Java plan for %s moved", q)
			n++
		}
		Expect(n).To(Equal(len(reads)))
		Expect(wantJava).To(HaveLen(len(reads)))
	})
})

// Whether each engine SERVES a query from a VALUE index whose key holds a nested
// scalar leaf. F1 made Go store such an index as the target does; Go's candidate
// construction then drops every index whose root reaches a nested leaf
// (plan_context_builder.go, index_expansion.go, match_candidate_index.go), so it
// plans a scan where the target uses the index. Both engines' trees are pinned:
// the Go pins are the gap WS-J section 3.5 closes, and move with it.
var _ = Describe("WS-J nested-leaf value index plan oracle", func() {
	It("records both engines' plans over value indexes on nested leaf fields", func() {
		ctx := context.Background()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "ws_j_vplan_"+uuid.New().String())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		java := plandiff.NewJavaEngineHTTP(javaBaseURL(srv), env.ClusterFile)
		// Go's PHYSICAL plan, through EXPLAIN on the SQL runner (plandiff's Go
		// engine explains the logical plan only).
		clusterFilePath := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFilePath)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFilePath)
		renderGo := func(r plandiff.RunResult) string {
			if r.Err != nil {
				return "ERROR " + wsjTrim(r.Err.Error())
			}
			if len(r.Rows.Rows) == 0 || len(r.Rows.Rows[0]) == 0 {
				return "EMPTY"
			}
			return fmt.Sprint(r.Rows.Rows[0][0])
		}
		schema := "CREATE TYPE AS STRUCT SC (x BIGINT, y BIGINT) " +
			"CREATE TABLE T (id BIGINT, s SC, ts BIGINT, PRIMARY KEY (id)) " +
			"CREATE INDEX nested_then_top AS SELECT s.x, ts FROM T ORDER BY s.x, ts " +
			"CREATE INDEX nested_y AS SELECT s.y FROM T ORDER BY s.y"
		reads := []string{
			"SELECT s.x, ts FROM T ORDER BY s.x, ts",
			"SELECT id FROM T WHERE s.x = 5",
			"SELECT id, ts FROM T WHERE s.x = 5 AND ts > 3",
			"SELECT s.y FROM T ORDER BY s.y",
			"SELECT id FROM T WHERE s.y > 2",
			"SELECT * FROM T WHERE s.x = 5",
		}
		// Measured (4.14.2.0): the target serves every read from the nested-leaf
		// index, covering where the projection allows; Go scans and sorts.
		wantJava := map[string]string{
			"SELECT s.x, ts FROM T ORDER BY s.x, ts":        "COVERING(NESTED_THEN_TOP <,> -> [ID: KEY:[3], TS: KEY:[1], S: [X: KEY:[0]]]) | MAP (_.S.X AS X, _.TS AS TS)",
			"SELECT id FROM T WHERE s.x = 5":                "COVERING(NESTED_THEN_TOP [EQUALS promote(@c9 AS LONG)] -> [ID: KEY:[3], TS: KEY:[1], S: [X: KEY:[0]]]) | MAP (_.ID AS ID)",
			"SELECT id, ts FROM T WHERE s.x = 5 AND ts > 3": "COVERING(NESTED_THEN_TOP [EQUALS promote(@c11 AS LONG), [GREATER_THAN promote(@c15 AS LONG)]] -> [ID: KEY:[3], TS: KEY:[1], S: [X: KEY:[0]]]) | MAP (_.ID AS ID, _.TS AS TS)",
			"SELECT s.y FROM T ORDER BY s.y":                "COVERING(NESTED_Y <,> -> [ID: KEY:[2], S: [Y: KEY:[0]]]) | MAP (_.S.Y AS Y)",
			"SELECT id FROM T WHERE s.y > 2":                "COVERING(NESTED_Y [[GREATER_THAN promote(@c9 AS LONG)]] -> [ID: KEY:[2], S: [Y: KEY:[0]]]) | MAP (_.ID AS ID)",
			"SELECT * FROM T WHERE s.x = 5":                 "ISCAN(NESTED_THEN_TOP [EQUALS promote(@c9 AS LONG)])",
		}
		wantGo := map[string]string{
			"SELECT s.x, ts FROM T ORDER BY s.x, ts":        "Project([_current.S#1.X#0, _current.TS#2], InMemorySort([_current.S#1.X#0 ASC, _current.TS#2 ASC], Scan(T)))",
			"SELECT id FROM T WHERE s.x = 5":                "Project([_current.ID#0], PredicatesFilter(Scan(T), [1 preds]))",
			"SELECT id, ts FROM T WHERE s.x = 5 AND ts > 3": "Project([_current.ID#0, _current.TS#2], PredicatesFilter(Scan(T), [2 preds]))",
			"SELECT s.y FROM T ORDER BY s.y":                "Project([_current.S#1.Y#1], InMemorySort([_current.S#1.Y#1 ASC], Scan(T)))",
			"SELECT id FROM T WHERE s.y > 2":                "Project([_current.ID#0], PredicatesFilter(Scan(T), [1 preds]))",
			"SELECT * FROM T WHERE s.x = 5":                 "PredicatesFilter(Scan(T), [1 preds])",
		}
		render := func(r plandiff.PlanResult) string {
			if r.Err != nil {
				return "ERROR " + wsjTrim(r.Err.Error())
			}
			return strings.Join(strings.Fields(r.Tree), " ")
		}
		gotJava, gotGo := map[string]string{}, map[string]string{}
		for i, q := range reads {
			query := plandiff.Query{Name: fmt.Sprintf("wsj_vnested_%d", i), SQL: q, SchemaTemplate: schema}
			gotJava[q], gotGo[q] = render(java.Plan(ctx, query)), renderGo(goRunner.RunWithSetup(ctx, schema, nil, "EXPLAIN "+q))
			fmt.Fprintf(GinkgoWriter, "WSJV %q\n  java=%s\n  go=%s\n", q, gotJava[q], gotGo[q])
		}
		for _, q := range reads {
			fmt.Fprintf(GinkgoWriter, "WSJV-PIN %q:\n  java %q\n  go %q\n", q, gotJava[q], gotGo[q])
		}
		Expect(gotJava).To(HaveLen(len(reads)), "reads are unique")
		Expect(sortedStringKeys(wantJava)).To(Equal(sortedStringKeys(gotJava)), "every read has a Java pin")
		Expect(sortedStringKeys(wantGo)).To(Equal(sortedStringKeys(gotGo)), "every read has a Go pin")
		for _, q := range reads {
			Expect(gotJava[q]).To(Equal(wantJava[q]), "Java plan for %s moved", q)
			Expect(gotGo[q]).To(Equal(wantGo[q]), "Go plan for %s moved", q)
		}
	})
})

// "Go stores, Java plans". The target plans over the STORED key expressions
// (KeyExpressionExpansionVisitor turns a stored literal into a Value of the
// stored width), so a template Go persisted with long_value literals may not
// match the target's own INT-typed query Values, or may fail to encapsulate
// where the target has no (LONG, LONG) operator (the bitmap functions have only
// _LI and _II). Each read runs twice in the target: over the template Go
// stored through its catalog library at the Java-compatible catalog subspace,
// and over the identical body the target stored itself (the control). Before
// the literal-carrier fix every bitmap read over the Go-stored template failed
// to encapsulate; the two must now answer identically. A third copy stored
// through the Go SQL driver is pinned invisible to the target.
var _ = Describe("WS-J Go-stored template planned by the target", func() {
	It("records the target's plans and rows over literal-bearing indexes Go stored", func() {
		ctx := context.Background()
		java := NewJavaInvoker()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		clusterFilePath := writeClusterFileToTemp(clusterFile)
		defer os.Remove(clusterFilePath)

		body := "CREATE TABLE T1 (id BIGINT, category STRING, PRIMARY KEY (id)) " +
			"CREATE INDEX agg_bucket AS SELECT bitmap_bucket_offset(id) FROM T1 ORDER BY bitmap_bucket_offset(id) " +
			"CREATE INDEX bm AS SELECT bitmap_construct_agg(bitmap_bit_position(id)), category, bitmap_bucket_offset(id) " +
			"FROM T1 GROUP BY category, bitmap_bucket_offset(id) " +
			"CREATE TABLE T2 (id BIGINT, d BIGINT, PRIMARY KEY (id)) " +
			"CREATE INDEX dmask AS SELECT d & 1 FROM T2 ORDER BY d & 1 " +
			"CREATE INDEX dplus AS SELECT d + 1 FROM T2 ORDER BY d + 1"
		driverName := "WSJ_DRV_" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")
		goName := "WSJ_GO_" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")
		javaName := "WSJ_JAVA_" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")

		// (a) Through the Go SQL driver: stored on the driver's Go-only keyspace
		// (TODO.md, "Go SQL driver stores the relational catalog ... on a Go-only
		// keyspace"), so the target cannot see it at all. Pinned below; it reddens
		// when the driver moves to the Java layout.
		sysDB, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s", clusterFilePath))
		Expect(err).NotTo(HaveOccurred())
		defer sysDB.Close()
		_, err = sysDB.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA TEMPLATE %s %s", driverName, body))
		Expect(err).NotTo(HaveOccurred(), "Go stores the template through its SQL driver")
		defer func() { _, _ = sysDB.ExecContext(context.Background(), "DROP SCHEMA TEMPLATE IF EXISTS "+driverName) }()
		var driverRead map[string]any
		driverErr := java.InvokeAs(ctx, "runOnExistingTemplateJava", map[string]any{
			"clusterFile": clusterFile, "templateName": driverName, "setupSqls": []string{}, "querySql": "SELECT id FROM T2",
		}, &driverRead)
		var driverJE *JavaError
		Expect(errors.As(driverErr, &driverJE)).To(BeTrue(), "the target reading a driver-stored template")
		Expect(driverJE.SQLState).To(Equal("42F55"),
			"the target cannot see a template the Go SQL driver stored (Go-only keyspace); if this moved, "+
				"the driver's catalog layout changed and the TODO.md entry must be revisited")
		fmt.Fprintf(GinkgoWriter, "WSJG driver-stored template seen by the target: %s %q\n", driverJE.SQLState, wsjTrim(driverJE.Message))

		// (b) The same Go-built metadata stored through Go's catalog LIBRARY at the
		// Java-compatible (NULL, NULL, 0) subspace: the literal-width question.
		goTmpl, err := embedded.BuildSchemaTemplateFromDDLNamed(body, goName)
		Expect(err).NotTo(HaveOccurred())
		cat, err := catalog.OpenRecordLayerStoreCatalog()
		Expect(err).NotTo(HaveOccurred())
		db := recordlayer.NewFDBDatabase(sharedDB)
		_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			return nil, cat.SchemaTemplateCatalog().CreateTemplate(catalog.NewFDBTransaction(rtx), goTmpl)
		})
		Expect(err).NotTo(HaveOccurred(), "Go stores the template through its catalog library")
		defer func() {
			var dropped struct {
				Dropped bool `json:"dropped"`
			}
			_ = java.InvokeAs(context.Background(), "dropSchemaTemplatePersistentJava", map[string]any{
				"clusterFile": clusterFile, "templateName": goName,
			}, &dropped)
		}()
		// (c) The same metadata with every INT literal rewritten to long_value, the
		// width a Go build before the literal-carrier fix persisted.
		longName := "WSJ_LONG_" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")
		longProto, err := goTmpl.Underlying().ToProto()
		Expect(err).NotTo(HaveOccurred())
		rewritten := wsjWidenIntLiterals(longProto.ProtoReflect())
		Expect(rewritten).To(BeNumerically(">", 0), "the long_value variant must actually rewrite a literal")
		fmt.Fprintf(GinkgoWriter, "WSJG long_value variant rewrote %d literals\n", rewritten)
		longMD, err := recordlayer.RecordMetaDataFromProto(longProto)
		Expect(err).NotTo(HaveOccurred())
		longTmpl, err := metadata.NewRecordLayerSchemaTemplateWithVersion(longName, longMD, goTmpl.Version())
		Expect(err).NotTo(HaveOccurred())
		// The build path refuses a key the target cannot plan (the lane check,
		// ws-j-design.md section 3.2): bitmap_bucket_offset over (LONG, LONG).
		_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			return nil, cat.SchemaTemplateCatalog().CreateTemplate(catalog.NewFDBTransaction(rtx), longTmpl)
		})
		var laneErr *api.Error
		Expect(errors.As(err, &laneErr)).To(BeTrue(), "%v", err)
		Expect(string(laneErr.Code)).To(Equal("42F59"))
		Expect(laneErr.Message).To(ContainSubstring("bitmap_bucket_offset has no lane for operand types (LONG, LONG)"))
		// So the variant is what a tenant an earlier Go build served holds: a
		// raw write of its catalog row, past the build path.
		longBytes, err := proto.Marshal(longProto)
		Expect(err).NotTo(HaveOccurred())
		catalogMD, err := catalog.BuildCatalogMetaData()
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetSubspace(catalog.DefaultCatalogSubspace()).
				SetMetaDataProvider(catalogMD).Open()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Templates{
				TEMPLATE_NAME: proto.String(longName), TEMPLATE_VERSION: proto.Int32(int32(goTmpl.Version())), META_DATA: longBytes,
			})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred(), "the long_value variant, written raw")
		defer func() {
			var dropped struct {
				Dropped bool `json:"dropped"`
			}
			_ = java.InvokeAs(context.Background(), "dropSchemaTemplatePersistentJava", map[string]any{
				"clusterFile": clusterFile, "templateName": longName,
			}, &dropped)
		}()
		var created struct {
			Created bool `json:"created"`
		}
		Expect(java.InvokeAs(ctx, "createSchemaTemplatePersistentJava", map[string]any{
			"clusterFile": clusterFile, "templateName": javaName, "schemaTemplateBody": body,
		}, &created)).To(Succeed())
		defer func() {
			var dropped struct {
				Dropped bool `json:"dropped"`
			}
			_ = java.InvokeAs(context.Background(), "dropSchemaTemplatePersistentJava", map[string]any{
				"clusterFile": clusterFile, "templateName": javaName,
			}, &dropped)
		}()

		setup := []string{
			"INSERT INTO T1 VALUES (1, 'a'), (20005, 'a'), (30002, 'b')",
			"INSERT INTO T2 VALUES (1, 4), (2, 5), (3, 7)",
		}
		reads := []string{
			"EXPLAIN SELECT bitmap_bucket_offset(id) FROM T1 ORDER BY bitmap_bucket_offset(id)",
			"SELECT bitmap_bucket_offset(id) FROM T1 ORDER BY bitmap_bucket_offset(id)",
			"EXPLAIN SELECT id FROM T2 ORDER BY d & 1",
			"SELECT id, d & 1 FROM T2 ORDER BY d & 1, id",
			"EXPLAIN SELECT id FROM T2 ORDER BY d + 1",
			"SELECT d + 1 FROM T2 ORDER BY d + 1",
			"EXPLAIN SELECT bitmap_construct_agg(bitmap_bit_position(id)) AS b, category, bitmap_bucket_offset(id) AS o " +
				"FROM T1 GROUP BY category, bitmap_bucket_offset(id)",
			// Index-served equalities over the literal-bearing arithmetic indexes.
			"EXPLAIN SELECT id FROM T2 WHERE d & 1 = 1",
			"SELECT id FROM T2 WHERE d & 1 = 1",
			"EXPLAIN SELECT id FROM T2 WHERE d + 1 = 5",
			"SELECT id FROM T2 WHERE d + 1 = 5",
			// Reads of T1 that no bitmap expression serves: whether a stored key the
			// target cannot expand fails every query over its table (the target's
			// candidate expansion catches only UnsupportedOperationException,
			// MatchCandidateExpansion.java:101-131) or only the ones that would use it.
			"SELECT id FROM T1 WHERE id = 20005",
			"EXPLAIN SELECT id FROM T1 WHERE id = 20005",
			"SELECT category FROM T1 ORDER BY id",
			"SELECT count(*) FROM T1",
		}
		render := func(sqlText string, raw map[string]any, err error) string {
			if err != nil {
				var je *JavaError
				if errors.As(err, &je) {
					return fmt.Sprintf("ERROR %s %s %q", je.SQLState, je.ExceptionClass, wsjTrim(je.Message))
				}
				return fmt.Sprintf("ERROR %q", wsjTrim(err.Error()))
			}
			rows, _ := raw["rows"].([]any)
			if strings.HasPrefix(sqlText, "EXPLAIN") && len(rows) > 0 {
				if first, ok := rows[0].([]any); ok && len(first) > 0 {
					return fmt.Sprintf("OK EXPLAIN %q", fmt.Sprint(first[0]))
				}
			}
			return fmt.Sprintf("OK %v", rows)
		}
		// The target's answer over the template IT stored, measured. Over the
		// Go-stored template the answer must be identical: before the literal
		// width fix, every bitmap row there was XX000 "unable to encapsulate
		// arithmetic operation due to type mismatch(es)", because Go stored the
		// injected 10000 entry size as long_value where Java stores int_value.
		javaPins := map[string]string{
			reads[0]:  `OK EXPLAIN "ISCAN(AGG_BUCKET <,>) | MAP ((_.ID) bitmap_bucket_offset 10000 AS _0)"`,
			reads[1]:  `OK [[0] [20000] [30000]]`,
			reads[2]:  `ERROR 0AF00 UnableToPlanException "Cascades planner could not plan query"`,
			reads[3]:  `ERROR 0AF00 UnableToPlanException "Cascades planner could not plan query"`,
			reads[4]:  `ERROR 0AF00 UnableToPlanException "Cascades planner could not plan query"`,
			reads[5]:  `ERROR 0AF00 UnableToPlanException "Cascades planner could not plan query"`,
			reads[6]:  `OK EXPLAIN "AISCAN(BM <,> BY_GROUP -> [_0: KEY:[0], _1: KEY:[1], _2: VALUE:[0]]) | MAP (_._2 AS B, _._0 AS CATEGORY, _._1 AS O)"`,
			reads[7]:  `OK EXPLAIN "COVERING(DMASK [EQUALS promote(@c7 AS LONG)] -> [ID: KEY:[2]]) | MAP (_.ID AS ID)"`,
			reads[8]:  `OK [[2] [3]]`,
			reads[9]:  `OK EXPLAIN "COVERING(DPLUS [EQUALS promote(@c9 AS LONG)] -> [ID: KEY:[2]]) | MAP (_.ID AS ID)"`,
			reads[10]: `OK [[1]]`,
			reads[11]: `OK [[20005]]`,
			reads[12]: `OK EXPLAIN "SCAN([IS T1, EQUALS promote(@c7 AS LONG)]) | MAP (_.ID AS ID)"`,
			reads[13]: `OK [[a] [a] [b]]`,
			reads[14]: `OK [[3]]`,
		}
		// The target over the template as a Go build before the literal-carrier fix
		// stored it: every INT literal carried as long_value. This is what a tenant
		// created before the upgrade holds, and the control that shows which reads
		// tell the two widths apart.
		// Measured (4.14.2.0): every bitmap read fails to encapsulate (the bitmap
		// functions have only _LI and _II operators), and the two equalities are no
		// longer sargable on their index (the stored LONG literal does not match the
		// query's INT Value), so they scan the whole index and filter: the same rows,
		// at full-index cost. The ORDER BY reads cannot be planned over either width.
		encapsulate := `ERROR XX000 VerifyException "unable to encapsulate arithmetic operation due to type mismatch(es)"`
		longPins := map[string]string{
			reads[0]:  encapsulate,
			reads[1]:  encapsulate,
			reads[2]:  javaPins[reads[2]],
			reads[3]:  javaPins[reads[3]],
			reads[4]:  javaPins[reads[4]],
			reads[5]:  javaPins[reads[5]],
			reads[6]:  encapsulate,
			reads[7]:  `OK EXPLAIN "ISCAN(DMASK <,>) | FILTER _.D & @c7 EQUALS promote(@c7 AS LONG) | MAP (_.ID AS ID)"`,
			reads[8]:  javaPins[reads[8]],
			reads[9]:  `OK EXPLAIN "ISCAN(DPLUS <,>) | FILTER _.D + @c7 EQUALS promote(@c9 AS LONG) | MAP (_.ID AS ID)"`,
			reads[10]: javaPins[reads[10]],
			// Every read of T1 fails, the four that no bitmap expression serves
			// included: the target expands every index of the queried type into a
			// match candidate, and the stored bitmap key's LONG entry size has no
			// lane, so the VerifyException escapes candidate expansion (which
			// catches only UnsupportedOperationException) and fails the query.
			reads[11]: encapsulate,
			reads[12]: encapsulate,
			reads[13]: encapsulate,
			reads[14]: encapsulate,
		}
		gotJava, gotGo, gotLong := map[string]string{}, map[string]string{}, map[string]string{}
		for _, q := range reads {
			var onGo, onJava, onLong map[string]any
			goErr := java.InvokeAs(ctx, "runOnExistingTemplateJava", map[string]any{
				"clusterFile": clusterFile, "templateName": goName, "setupSqls": setup, "querySql": q,
			}, &onGo)
			javaErr := java.InvokeAs(ctx, "runOnExistingTemplateJava", map[string]any{
				"clusterFile": clusterFile, "templateName": javaName, "setupSqls": setup, "querySql": q,
			}, &onJava)
			longErr := java.InvokeAs(ctx, "runOnExistingTemplateJava", map[string]any{
				"clusterFile": clusterFile, "templateName": longName, "setupSqls": setup, "querySql": q,
			}, &onLong)
			overGo, overJava, overLong := render(q, onGo, goErr), render(q, onJava, javaErr), render(q, onLong, longErr)
			fmt.Fprintf(GinkgoWriter, "WSJG %q\n  over-go-stored=%s\n  over-java-stored=%s\n  over-long-value=%s\n", q, overGo, overJava, overLong)
			gotJava[q], gotGo[q], gotLong[q] = overJava, overGo, overLong
		}
		for _, q := range reads {
			fmt.Fprintf(GinkgoWriter, "WSJG-PIN %q\n  java=%q\n  long=%q\n", q, gotJava[q], gotLong[q])
		}
		Expect(gotJava).To(HaveLen(len(reads)), "reads are unique")
		Expect(sortedStringKeys(javaPins)).To(Equal(sortedStringKeys(gotJava)), "one pin per read")
		Expect(sortedStringKeys(longPins)).To(Equal(sortedStringKeys(gotLong)), "one long_value pin per read")
		discriminating := 0
		for _, q := range reads {
			Expect(gotJava[q]).To(Equal(javaPins[q]), "the target over its own template: %s", q)
			Expect(gotGo[q]).To(Equal(gotJava[q]), "the target over the Go-stored template must answer as over its own: %s", q)
			Expect(gotLong[q]).To(Equal(longPins[q]), "the target over the long_value template: %s", q)
			if gotLong[q] != gotJava[q] {
				discriminating++
			}
		}
		fmt.Fprintf(GinkgoWriter, "WSJG discriminating=%d of %d\n", discriminating, len(reads))
		Expect(discriminating).To(Equal(9), "the reads that tell an int_value template from a long_value one")
	})
})

// What the target does when a long-only arithmetic key function
// (LongArithmethicFunctionKeyExpression, reading its operands with
// Key.Evaluated.getNullableLong, Key.java:579-582) is maintained over a
// NON-integer operand: a DOUBLE column, a DOUBLE literal, a FLOAT column. The
// DDL is accepted by both engines (the lowering carries no lane); the question is
// the first insert and what the index then serves. getNullableLong accepts any
// java.lang.Number through Number.longValue(), so the source reading is "no
// error, the operands truncate toward zero"; this measures it.
var _ = Describe("WS-J long arithmetic key functions over non-integer operands", func() {
	It("records the target's insert and index reads over DOUBLE and FLOAT operands", func() {
		ctx := context.Background()
		java := NewJavaInvoker()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		type probe struct {
			name, body string
			setup      []string
			reads      []string
		}
		probes := []probe{
			{
				"double_plus_double", "CREATE TABLE T (id BIGINT, d DOUBLE, n BIGINT, PRIMARY KEY (id)) " +
					"CREATE INDEX arith_d AS SELECT d + d FROM T ORDER BY d + d",
				[]string{"INSERT INTO T VALUES (1, 1.5, 2), (2, -1.5, 3), (3, 2.0, 4)"},
				[]string{
					"SELECT id FROM T",
					"EXPLAIN SELECT id FROM T WHERE d + d = 2",
					"SELECT id FROM T WHERE d + d = 2",
					"SELECT id FROM T WHERE d + d = 3",
					"EXPLAIN SELECT d + d FROM T ORDER BY d + d",
					"SELECT d + d FROM T ORDER BY d + d",
					// Coverage of a function key: the entry holds d + d and the
					// primary key, not d. Does the target serve the function value, or
					// the operand, from the entry, or fetch the record?
					"EXPLAIN SELECT d + d FROM T WHERE d + d = 2",
					"EXPLAIN SELECT d FROM T WHERE d + d = 2",
				},
			},
			{
				"double_times_literal", "CREATE TABLE T (id BIGINT, c DOUBLE, PRIMARY KEY (id)) " +
					"CREATE INDEX ix AS SELECT c * 1.5 FROM T ORDER BY c * 1.5",
				[]string{"INSERT INTO T VALUES (1, 2.0), (2, 1.5)"},
				[]string{
					"EXPLAIN SELECT id FROM T WHERE c * 1.5 = 2",
					"SELECT id FROM T WHERE c * 1.5 = 2",
					"SELECT id FROM T WHERE c * 1.5 = 3",
				},
			},
			{
				"float_plus_int", "CREATE TABLE T (id BIGINT, f FLOAT, PRIMARY KEY (id)) " +
					"CREATE INDEX ix AS SELECT f + 1 FROM T ORDER BY f + 1",
				[]string{"INSERT INTO T VALUES (1, 1.5f)"},
				[]string{
					"EXPLAIN SELECT id FROM T WHERE f + 1 = 2",
					"SELECT id FROM T WHERE f + 1 = 2",
					"EXPLAIN SELECT id FROM T WHERE f + 1 = 2.5",
					"SELECT id FROM T WHERE f + 1 = 2.5",
				},
			},
			// An INTEGER operand at the edge of its lane: the index computes the key in
			// long arithmetic (LongArithmethicFunctionKeyExpression), the query's i + 1
			// is an INT-lane ArithmeticValue that overflows at 2147483647.
			{
				"int_max_plus_one", "CREATE TABLE T (id BIGINT, i INTEGER, PRIMARY KEY (id)) " +
					"CREATE INDEX ix AS SELECT i + 1 FROM T ORDER BY i + 1",
				[]string{"INSERT INTO T VALUES (1, 2147483647), (2, 5)"},
				[]string{
					"EXPLAIN SELECT id FROM T WHERE i + 1 = 2147483648",
					"SELECT id FROM T WHERE i + 1 = 2147483648",
					"EXPLAIN SELECT id FROM T WHERE i + 1 = 6",
					"SELECT id FROM T WHERE i + 1 = 6",
					"SELECT i + 1 FROM T WHERE id = 1",
					"SELECT id FROM T WHERE i + 1 > 0 ORDER BY id",
					// The function value itself, projected in the index's order: does
					// the target read it from the index entry (2147483648, a LONG the
					// INT-typed expression cannot produce) or recompute it per record?
					"EXPLAIN SELECT i + 1 FROM T ORDER BY i + 1",
					"SELECT i + 1 FROM T ORDER BY i + 1",
					// The same coverage question at the INT edge.
					"EXPLAIN SELECT i + 1 FROM T WHERE i + 1 = 6",
					"SELECT i + 1 FROM T WHERE i + 1 = 6",
					"EXPLAIN SELECT i FROM T WHERE i + 1 = 6",
					"SELECT i FROM T WHERE i + 1 = 6",
				},
			},
		}
		// Measured (4.14.2.0, two runs identical). Every insert succeeds; an index-served
		// equality returns no row even where the record's own value matches (the
		// index holds the truncated LONG sum, the query compares a DOUBLE).
		want := map[string]string{
			`double_plus_double "SELECT id FROM T"`:                                "OK [[2] [1] [3]]",
			`double_plus_double "EXPLAIN SELECT id FROM T WHERE d + d = 2"`:        "PLAN COVERING(ARITH_D [EQUALS promote(@c9 AS DOUBLE)] -> [ID: KEY:[2]]) | MAP (_.ID AS ID)",
			`double_plus_double "SELECT id FROM T WHERE d + d = 2"`:                "OK []",
			`double_plus_double "SELECT id FROM T WHERE d + d = 3"`:                "OK []",
			`double_plus_double "EXPLAIN SELECT d + d FROM T ORDER BY d + d"`:      "PLAN ISCAN(ARITH_D <,>) | MAP (_.D + _.D AS _0)",
			`double_plus_double "SELECT d + d FROM T ORDER BY d + d"`:              "OK [[-3] [3] [4]]",
			`double_times_literal "EXPLAIN SELECT id FROM T WHERE c * 1.5 = 2"`:    "PLAN COVERING(IX [EQUALS promote(@c9 AS DOUBLE)] -> [ID: KEY:[2]]) | MAP (_.ID AS ID)",
			`double_times_literal "SELECT id FROM T WHERE c * 1.5 = 2"`:            "OK []",
			`double_times_literal "SELECT id FROM T WHERE c * 1.5 = 3"`:            "OK []",
			`float_plus_int "EXPLAIN SELECT id FROM T WHERE f + 1 = 2"`:            "PLAN COVERING(IX [EQUALS promote(@c9 AS FLOAT)] -> [ID: KEY:[2]]) | MAP (_.ID AS ID)",
			`float_plus_int "SELECT id FROM T WHERE f + 1 = 2"`:                    "OK []",
			`float_plus_int "EXPLAIN SELECT id FROM T WHERE f + 1 = 2.5"`:          "PLAN ISCAN(IX <,>) | FILTER promote(_.F + @c7 AS DOUBLE) EQUALS promote(@c9 AS DOUBLE) | MAP (_.ID AS ID)",
			`float_plus_int "SELECT id FROM T WHERE f + 1 = 2.5"`:                  "OK [[1]]",
			`int_max_plus_one "EXPLAIN SELECT id FROM T WHERE i + 1 = 2147483648"`: "PLAN ISCAN(IX <,>) | FILTER promote(_.I + @c7 AS LONG) EQUALS promote(@c9 AS LONG) | MAP (_.ID AS ID)",
			`int_max_plus_one "SELECT id FROM T WHERE i + 1 = 2147483648"`:         "ERROR XXXXX ArithmeticException \"integer overflow\"",
			`int_max_plus_one "EXPLAIN SELECT id FROM T WHERE i + 1 = 6"`:          "PLAN COVERING(IX [EQUALS promote(@c9 AS INT)] -> [ID: KEY:[2]]) | MAP (_.ID AS ID)",
			`int_max_plus_one "SELECT id FROM T WHERE i + 1 = 6"`:                  "OK [[2]]",
			`int_max_plus_one "SELECT i + 1 FROM T WHERE id = 1"`:                  "ERROR XXXXX ArithmeticException \"integer overflow\"",
			`int_max_plus_one "SELECT id FROM T WHERE i + 1 > 0 ORDER BY id"`:      "ERROR XXXXX ArithmeticException \"integer overflow\"",
			`int_max_plus_one "EXPLAIN SELECT i + 1 FROM T ORDER BY i + 1"`:        "ERROR 0AF00 UnableToPlanException \"Cascades planner could not plan query\"",
			`int_max_plus_one "SELECT i + 1 FROM T ORDER BY i + 1"`:                "ERROR 0AF00 UnableToPlanException \"Cascades planner could not plan query\"",
			// Coverage of a function key, measured: an index-served equality fetches
			// the record and recomputes the function value from it; the value in
			// the entry is never read, and neither is an operand.
			`double_plus_double "EXPLAIN SELECT d + d FROM T WHERE d + d = 2"`: "PLAN ISCAN(ARITH_D [EQUALS promote(@c11 AS DOUBLE)]) | MAP (_.D + _.D AS _0)",
			`double_plus_double "EXPLAIN SELECT d FROM T WHERE d + d = 2"`:     "PLAN ISCAN(ARITH_D [EQUALS promote(@c9 AS DOUBLE)]) | MAP (_.D AS D)",
			`int_max_plus_one "EXPLAIN SELECT i + 1 FROM T WHERE i + 1 = 6"`:   "PLAN ISCAN(IX [EQUALS promote(@c11 AS INT)]) | MAP (_.I + @c9 AS _0)",
			`int_max_plus_one "SELECT i + 1 FROM T WHERE i + 1 = 6"`:           "OK [[6]]",
			`int_max_plus_one "EXPLAIN SELECT i FROM T WHERE i + 1 = 6"`:       "PLAN ISCAN(IX [EQUALS promote(@c9 AS INT)]) | MAP (_.I AS I)",
			`int_max_plus_one "SELECT i FROM T WHERE i + 1 = 6"`:               "OK [[5]]",
		}
		got := map[string]string{}
		render := func(raw map[string]any, err error) string {
			if err != nil {
				var je *JavaError
				if errors.As(err, &je) {
					return fmt.Sprintf("ERROR %s %s %q", je.SQLState, je.ExceptionClass, wsjTrim(je.Message))
				}
				return fmt.Sprintf("ERROR %q", wsjTrim(err.Error()))
			}
			rows, _ := raw["rows"].([]any)
			if cols, _ := raw["columns"].([]any); len(rows) > 0 && len(cols) > 0 {
				if c0, ok := cols[0].(map[string]any); ok && strings.EqualFold(fmt.Sprint(c0["name"]), "PLAN") {
					if first, ok := rows[0].([]any); ok && len(first) > 0 {
						return "PLAN " + fmt.Sprint(first[0])
					}
				}
			}
			var rs [][]any
			for _, r := range rows {
				if row, ok := r.([]any); ok {
					rs = append(rs, row)
				}
			}
			return "OK " + wsjRows(rs)
		}
		// Go's answer to the same reads. Go writes the same truncated entries (the
		// F2b entry test below), so an index-served equality over a DOUBLE comparand
		// would miss the row exactly as the target's does; these pins say whether Go
		// serves each read from the index and what it answers.
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "ws_j_nonint_"+uuid.New().String())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		clusterFilePath := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFilePath)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFilePath)
		renderGo := func(r plandiff.RunResult) string {
			if r.Err != nil {
				var ge *api.Error
				if errors.As(r.Err, &ge) {
					return fmt.Sprintf("ERROR %s %q", string(ge.Code), wsjTrim(ge.Message))
				}
				return fmt.Sprintf("ERROR %q", wsjTrim(r.Err.Error()))
			}
			if len(r.Rows.Columns) > 0 && strings.EqualFold(r.Rows.Columns[0].Name, "PLAN") && len(r.Rows.Rows) > 0 {
				return "PLAN " + fmt.Sprint(r.Rows.Rows[0][0])
			}
			return "OK " + wsjRows(r.Rows.Rows)
		}
		gotGo := map[string]string{}
		n := 0
		for _, p := range probes {
			for _, q := range p.reads {
				key := fmt.Sprintf("%s %q", p.name, q)
				gotGo[key] = renderGo(goRunner.RunWithSetup(ctx, p.body, p.setup, q))
				fmt.Fprintf(GinkgoWriter, "WSJN-GO %s -> %s\n", key, gotGo[key])
			}
			name := "WSJN_" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")
			var created struct {
				Created bool `json:"created"`
			}
			cerr := java.InvokeAs(ctx, "createSchemaTemplatePersistentJava", map[string]any{
				"clusterFile": clusterFile, "templateName": name, "schemaTemplateBody": p.body,
			}, &created)
			if cerr != nil {
				fmt.Fprintf(GinkgoWriter, "WSJN %s ddl=%s\n", p.name, render(nil, cerr))
				n++
				continue
			}
			for _, q := range p.reads {
				var out map[string]any
				rerr := java.InvokeAs(ctx, "runOnExistingTemplateJava", map[string]any{
					"clusterFile": clusterFile, "templateName": name, "setupSqls": p.setup, "querySql": q,
				}, &out)
				key := fmt.Sprintf("%s %q", p.name, q)
				got[key] = render(out, rerr)
				fmt.Fprintf(GinkgoWriter, "WSJN %s -> %s\n", key, got[key])
			}
			var dropped struct {
				Dropped bool `json:"dropped"`
			}
			_ = java.InvokeAs(ctx, "dropSchemaTemplatePersistentJava", map[string]any{
				"clusterFile": clusterFile, "templateName": name,
			}, &dropped)
			n++
		}
		Expect(n).To(Equal(len(probes)))
		Expect(sortedStringKeys(got)).To(Equal(sortedStringKeys(want)), "every read is pinned, every pin read")
		for k, v := range got {
			Expect(v).To(Equal(want[k]), "target answer for %s moved", k)
		}
		for _, k := range sortedStringKeys(gotGo) {
			fmt.Fprintf(GinkgoWriter, "WSJN-GO-PIN %q: %q,\n", k, gotGo[k])
		}
		Expect(sortedStringKeys(gotGo)).To(Equal(sortedStringKeys(wsjNonIntGoPins)), "every Go read is pinned, every pin read")
		for k, v := range gotGo {
			Expect(v).To(Equal(wsjNonIntGoPins[k]), "Go answer for %s moved", k)
		}
	})
})

// wsjWidenIntLiterals rewrites every key-expression Value carrying int_value to
// carry the same number as long_value, in place, and returns how many it rewrote.
func wsjWidenIntLiterals(m protoreflect.Message) int {
	n := 0
	if v, ok := m.Interface().(*gen.Value); ok && v.IntValue != nil {
		wide := int64(v.GetIntValue())
		v.IntValue, v.LongValue = nil, &wide
		n++
	}
	m.Range(func(fd protoreflect.FieldDescriptor, val protoreflect.Value) bool {
		if fd.Kind() != protoreflect.MessageKind && fd.Kind() != protoreflect.GroupKind {
			return true
		}
		switch {
		case fd.IsList():
			l := val.List()
			for i := 0; i < l.Len(); i++ {
				n += wsjWidenIntLiterals(l.Get(i).Message())
			}
		case fd.IsMap():
			if fd.MapValue().Kind() == protoreflect.MessageKind {
				val.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
					n += wsjWidenIntLiterals(mv.Message())
					return true
				})
			}
		default:
			n += wsjWidenIntLiterals(val.Message())
		}
		return true
	})
	return n
}

// wsjNonIntGoPins is Go's answer to each read of the non-integer-operand oracle.
// Go's candidate bridge declines every arithmetic function key
// (keyExpressionFlatColumnDescriptors), so each read is a scan over the record's
// own values: `d + d = 3` and `c * 1.5 = 3` return row 1 where the target, serving
// them from its truncated entries, returns none (DIVERGENCES.md, "long-arithmetic
// index over a non-integer operand"). When Go ports function-key matching (WS-J
// section 3.5), these reads must stay scans: the entries do not hold the value of
// the query expression, so no index over them is a valid match.
var wsjNonIntGoPins = map[string]string{
	"int_max_plus_one \"EXPLAIN SELECT id FROM T WHERE i + 1 = 2147483648\"": "PLAN Project([_current.ID#0], PredicatesFilter(Scan(T), [1 preds]))",
	"int_max_plus_one \"SELECT id FROM T WHERE i + 1 = 2147483648\"":         "ERROR 22003 \"integer overflow\"",
	"int_max_plus_one \"EXPLAIN SELECT id FROM T WHERE i + 1 = 6\"":          "PLAN Project([_current.ID#0], PredicatesFilter(Scan(T), [1 preds]))",
	"int_max_plus_one \"SELECT id FROM T WHERE i + 1 = 6\"":                  "ERROR 22003 \"integer overflow\"",
	"int_max_plus_one \"SELECT i + 1 FROM T WHERE id = 1\"":                  "ERROR 22003 \"integer overflow\"",
	"int_max_plus_one \"SELECT id FROM T WHERE i + 1 > 0 ORDER BY id\"":      "ERROR 22003 \"integer overflow\"",
	"double_plus_double \"EXPLAIN SELECT d + d FROM T ORDER BY d + d\"":      "PLAN Project([(_current.D#1 + _current.D#1)], InMemorySort([(_current.D#1 + _current.D#1) ASC], Scan(T)))",
	"double_plus_double \"EXPLAIN SELECT id FROM T WHERE d + d = 2\"":        "PLAN Project([_current.ID#0], PredicatesFilter(Scan(T), [1 preds]))",
	"double_plus_double \"SELECT d + d FROM T ORDER BY d + d\"":              "OK [[-3] [3] [4]]",
	"double_plus_double \"SELECT id FROM T WHERE d + d = 2\"":                "OK []",
	"double_plus_double \"SELECT id FROM T WHERE d + d = 3\"":                "OK [[1]]",
	"double_plus_double \"SELECT id FROM T\"":                                "OK [[1] [2] [3]]",
	"double_times_literal \"EXPLAIN SELECT id FROM T WHERE c * 1.5 = 2\"":    "PLAN Project([_current.ID#0], PredicatesFilter(Scan(T), [1 preds]))",
	"double_times_literal \"SELECT id FROM T WHERE c * 1.5 = 2\"":            "OK []",
	"double_times_literal \"SELECT id FROM T WHERE c * 1.5 = 3\"":            "OK [[1]]",
	"float_plus_int \"EXPLAIN SELECT id FROM T WHERE f + 1 = 2\"":            "PLAN Project([_current.ID#0], PredicatesFilter(Scan(T), [1 preds]))",
	"float_plus_int \"SELECT id FROM T WHERE f + 1 = 2\"":                    "OK []",
	"float_plus_int \"EXPLAIN SELECT id FROM T WHERE f + 1 = 2.5\"":          "PLAN Project([_current.ID#0], PredicatesFilter(Scan(T), [1 preds]))",
	"float_plus_int \"SELECT id FROM T WHERE f + 1 = 2.5\"":                  "OK [[1]]",
	"int_max_plus_one \"EXPLAIN SELECT i + 1 FROM T ORDER BY i + 1\"":        "PLAN Project([(_current.I#1 + 1)], InMemorySort([(_current.I#1 + 1) ASC], Scan(T)))",
	"int_max_plus_one \"SELECT i + 1 FROM T ORDER BY i + 1\"":                "ERROR 22003 \"integer overflow\"",
	"double_plus_double \"EXPLAIN SELECT d + d FROM T WHERE d + d = 2\"":     "PLAN Project([(_current.D#1 + _current.D#1)], PredicatesFilter(Scan(T), [1 preds]))",
	"double_plus_double \"EXPLAIN SELECT d FROM T WHERE d + d = 2\"":         "PLAN Project([_current.D#1], PredicatesFilter(Scan(T), [1 preds]))",
	"int_max_plus_one \"EXPLAIN SELECT i + 1 FROM T WHERE i + 1 = 6\"":       "PLAN Project([(_current.I#1 + 1)], PredicatesFilter(Scan(T), [1 preds]))",
	"int_max_plus_one \"SELECT i + 1 FROM T WHERE i + 1 = 6\"":               "ERROR 22003 \"integer overflow\"",
	"int_max_plus_one \"EXPLAIN SELECT i FROM T WHERE i + 1 = 6\"":           "PLAN Project([_current.I#1], PredicatesFilter(Scan(T), [1 preds]))",
	"int_max_plus_one \"SELECT i FROM T WHERE i + 1 = 6\"":                   "ERROR 22003 \"integer overflow\"",
}

// F2b across engines, byte for byte. The target maintains a long-arithmetic index
// over DOUBLE operands by reading each operand with getNullableLong (truncating
// toward zero, NaN to 0, saturating), and an INT literal as int_value. Here the
// target inserts rows through SQL and the Go record layer saves the same rows into
// the SAME store (opened at the store prefix the target resolves), and the raw
// entries of both indexes are compared row for row: -1.5, 2.9999 and NaN, the two
// infinities and 2^63 (which saturate and overflow the sum), and the INT literal of
// `n + 1`. The same values narrowed to FLOAT go into a FLOAT column f, whose
// operands the target reads through the float arm of getNullableLong: `f + f`, and
// `f + 1.5f`, a FLOAT literal inside long arithmetic (the position the widening arm
// of 3.2 admits a double_value -> float_value move at). The target then answers an
// index-served read over the Go-written rows.
var _ = Describe("WS-J F2b index entries written by both engines", func() {
	It("stores identical entries and the target serves Go-written rows", func() {
		ctx := context.Background()
		java := NewJavaInvoker()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		name := "WSJF2B_" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")
		body := "CREATE TABLE T (id BIGINT, d DOUBLE, n BIGINT, f FLOAT, PRIMARY KEY (id)) " +
			"CREATE INDEX ARITH_D AS SELECT d + d FROM T ORDER BY d + d " +
			"CREATE INDEX NPLUS AS SELECT n + 1 FROM T ORDER BY n + 1 " +
			"CREATE INDEX ARITH_F AS SELECT f + f FROM T ORDER BY f + f " +
			"CREATE INDEX FPLUS AS SELECT f + 1.5f FROM T ORDER BY f + 1.5f"
		var created struct {
			Created bool `json:"created"`
		}
		Expect(java.InvokeAs(ctx, "createSchemaTemplatePersistentJava", map[string]any{
			"clusterFile": clusterFile, "templateName": name, "schemaTemplateBody": body,
		}, &created)).To(Succeed())
		defer func() {
			var dropped struct {
				Dropped bool `json:"dropped"`
			}
			_ = java.InvokeAs(context.Background(), "dropSchemaTemplatePersistentJava", map[string]any{
				"clusterFile": clusterFile, "templateName": name,
			}, &dropped)
		}()
		var store struct {
			DbPath      string `json:"dbPath"`
			SchemaName  string `json:"schemaName"`
			StorePrefix []int  `json:"storePrefix"`
		}
		Expect(java.InvokeAs(ctx, "wsjOpenStoreJava", map[string]any{"clusterFile": clusterFile, "templateName": name}, &store)).To(Succeed())
		defer func() {
			var dropped struct {
				Dropped bool `json:"dropped"`
			}
			_ = java.InvokeAs(context.Background(), "wsjDropDatabaseJava", map[string]any{"clusterFile": clusterFile, "dbPath": store.DbPath}, &dropped)
		}()

		// Each row writes d into D and f into F. The first six give both columns
		// one value; the ±∞ and 2^63 rows among them fail on ARITH_D first, so the
		// last four keep d finite so the FLOAT arm meets ±∞ and 2^63 on its own, and
		// the last is a large FLOAT that stays in range (3e18, doubled to 6e18).
		type f2bRow struct{ d, f any }
		rows := []f2bRow{
			{-1.5, -1.5},
			{2.9999, 2.9999},
			{"NaN", "NaN"},
			{"Infinity", "Infinity"},
			{"-Infinity", "-Infinity"},
			{9.223372036854775807e18, 9.223372036854775807e18},
			{0.0, "Infinity"},
			{0.0, "-Infinity"},
			{0.0, 9.223372036854775807e18},
			{0.0, 3e18},
		}
		goDouble := func(d any) float64 {
			switch v := d.(type) {
			case float64:
				return v
			case string:
				switch v {
				case "NaN":
					return math.NaN()
				case "Infinity":
					return math.Inf(1)
				}
				return math.Inf(-1)
			}
			panic(fmt.Sprint(d))
		}
		var javaRows [][]any
		for i, r := range rows {
			javaRows = append(javaRows, []any{i + 1, r.d, (i + 1) * 10, r.f})
		}
		var inserted struct {
			Outcomes []string `json:"outcomes"`
		}
		Expect(java.InvokeAs(ctx, "wsjInsertDoublesJava", map[string]any{
			"clusterFile": clusterFile, "dbPath": store.DbPath, "schemaName": store.SchemaName, "rows": javaRows,
		}, &inserted)).To(Succeed())
		Expect(inserted.Outcomes).To(HaveLen(len(rows)))

		// The Go record layer writes the same rows, ids 101.., into the same store.
		cat, err := catalog.OpenRecordLayerStoreCatalog()
		Expect(err).NotTo(HaveOccurred())
		db := recordlayer.NewFDBDatabase(sharedDB)
		var goOutcomes []string
		for i, r := range rows {
			_, serr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				tmpl, err := cat.SchemaTemplateCatalog().LoadSchemaTemplate(catalog.NewFDBTransaction(rtx), name)
				if err != nil {
					return nil, err
				}
				md := tmpl.(*metadata.RecordLayerSchemaTemplate).Underlying()
				prefix := make([]byte, len(store.StorePrefix))
				for j, b := range store.StorePrefix {
					prefix[j] = byte(b)
				}
				rs, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(subspace.FromBytes(prefix)).Open()
				if err != nil {
					return nil, err
				}
				rt := md.GetRecordType("T")
				msg := dynamicpb.NewMessage(rt.Descriptor)
				fields := rt.Descriptor.Fields()
				msg.Set(fields.ByName("ID"), protoreflect.ValueOfInt64(int64(i+101)))
				msg.Set(fields.ByName("D"), protoreflect.ValueOfFloat64(goDouble(r.d)))
				msg.Set(fields.ByName("N"), protoreflect.ValueOfInt64(int64((i+101)*10)))
				msg.Set(fields.ByName("F"), protoreflect.ValueOfFloat32(float32(goDouble(r.f))))
				_, err = rs.SaveRecord(msg)
				return nil, err
			})
			if serr != nil {
				goOutcomes = append(goOutcomes, "ERROR "+wsjTrim(serr.Error()))
			} else {
				goOutcomes = append(goOutcomes, "OK")
			}
		}

		entries := func(index string) map[int64]wsjEntry {
			return wsjIndexEntriesByID(ctx, java, clusterFile, store.DbPath, store.SchemaName, index)
		}
		arith, nplus := entries("ARITH_D"), entries("NPLUS")
		arithF, fplus := entries("ARITH_F"), entries("FPLUS")
		render := func(m map[int64]wsjEntry, id int64) string {
			e, ok := m[id]
			if !ok {
				return "-"
			}
			return e.value + "/" + e.raw
		}
		var got []string
		for i, r := range rows {
			d := r.d
			label := fmt.Sprintf("d=%v", r.d)
			if fmt.Sprint(r.f) != fmt.Sprint(r.d) {
				label = fmt.Sprintf("d=%v f=%v", r.d, r.f)
			}
			jid, gid := int64(i+1), int64(i+101)
			je, jok := arith[jid]
			ge, gok := arith[gid]
			line := fmt.Sprintf("d=%v java=%s go=%s arith_d java=%s/%s go=%s/%s", d, inserted.Outcomes[i], goOutcomes[i],
				map[bool]string{true: je.value, false: "-"}[jok], je.raw, map[bool]string{true: ge.value, false: "-"}[gok], ge.raw)
			fmt.Fprintf(GinkgoWriter, "WSJF2B %s\n", line)
			got = append(got, fmt.Sprintf("%s java=%s arith_d=%s raw=%s arith_f=%s fplus=%s", label, inserted.Outcomes[i],
				map[bool]string{true: je.value, false: "-"}[jok], je.raw, render(arithF, jid), render(fplus, jid)))
			Expect(render(arithF, gid)).To(Equal(render(arithF, jid)), "the Go-written ARITH_F entry of f=%v has the target's value and raw bytes", d)
			Expect(render(fplus, gid)).To(Equal(render(fplus, jid)), "the Go-written FPLUS entry of f=%v has the target's value and raw bytes", d)
			Expect(goOutcomes[i] == "OK").To(Equal(inserted.Outcomes[i] == "OK"), "both engines accept or both refuse the row with d=%v", d)
			Expect(gok).To(Equal(jok), "both engines wrote an ARITH_D entry for d=%v, or neither did", d)
			if jok {
				Expect(ge.raw).To(Equal(je.raw), "the Go-written entry of d=%v has the target's raw bytes", d)
				Expect(ge.value).To(Equal(je.value))
			}
			if inserted.Outcomes[i] == "OK" {
				Expect(nplus[gid].value).To(Equal(strconv.Itoa((i+101)*10+1)), "n + 1 over the int_value literal")
				Expect(nplus[jid].value).To(Equal(strconv.Itoa((i+1)*10 + 1)))
				Expect(nplus[gid].raw).To(Equal(hex.EncodeToString(tuple.Tuple{int64((i+101)*10 + 1)}.Pack())),
					"the raw NPLUS entry Go wrote is the tuple encoding of n + 1")
				Expect(nplus[jid].raw).To(Equal(hex.EncodeToString(tuple.Tuple{int64((i+1)*10 + 1)}.Pack())),
					"the raw NPLUS entry the target wrote is the tuple encoding of n + 1")
			}
		}
		// The target serves an index read over a Go-written row.
		var served, plan map[string]any
		Expect(java.InvokeAs(ctx, "wsjQueryJava", map[string]any{
			"clusterFile": clusterFile, "dbPath": store.DbPath, "schemaName": store.SchemaName,
			"querySql": "SELECT id FROM T WHERE n + 1 = 1011",
		}, &served)).To(Succeed())
		Expect(java.InvokeAs(ctx, "wsjQueryJava", map[string]any{
			"clusterFile": clusterFile, "dbPath": store.DbPath, "schemaName": store.SchemaName,
			"querySql": "EXPLAIN SELECT id FROM T WHERE n + 1 = 1011",
		}, &plan)).To(Succeed())
		fmt.Fprintf(GinkgoWriter, "WSJF2B served=%v plan=%v\n", served["rows"], plan["rows"])
		Expect(fmt.Sprint(served["rows"])).To(Equal("[[101]]"), "the target reads the Go-written row through NPLUS")
		Expect(fmt.Sprint(plan["rows"])).To(ContainSubstring("NPLUS"))
		for _, l := range got {
			fmt.Fprintf(GinkgoWriter, "WSJF2B-PIN %q,\n", l)
		}
		// Measured (4.14.2.0): the target truncates each DOUBLE and FLOAT operand toward
		// zero (-1.5 -> -1, 2.9999 -> 2) and NaN to 0, the FLOAT literal 1.5f to 1 inside
		// long arithmetic, and an infinity or 2^63 saturates to Long.MIN/MAX_VALUE, so
		// addExact overflows and the insert fails; the Go record layer stores the same
		// entries (asserted per row above) and refuses the same rows.
		want := []string{
			"d=-1.5 java=OK arith_d=-2 raw=13fd arith_f=-2/13fd fplus=0/14",
			"d=2.9999 java=OK arith_d=4 raw=1504 arith_f=4/1504 fplus=3/1503",
			"d=NaN java=OK arith_d=0 raw=14 arith_f=0/14 fplus=1/1501",
			"d=Infinity java=ERROR XXXXX ArithmeticException long overflow arith_d=- raw= arith_f=- fplus=-",
			"d=-Infinity java=ERROR XXXXX ArithmeticException long overflow arith_d=- raw= arith_f=- fplus=-",
			"d=9.223372036854776e+18 java=ERROR XXXXX ArithmeticException long overflow arith_d=- raw= arith_f=- fplus=-",
			"d=0 f=Infinity java=ERROR XXXXX ArithmeticException long overflow arith_d=- raw= arith_f=- fplus=-",
			"d=0 f=-Infinity java=ERROR XXXXX ArithmeticException long overflow arith_d=- raw= arith_f=- fplus=-",
			"d=0 f=9.223372036854776e+18 java=ERROR XXXXX ArithmeticException long overflow arith_d=- raw= arith_f=- fplus=-",
			"d=0 f=3e+18 java=OK arith_d=0 raw=14 arith_f=5999999768401543168/1c5344480000000000 fplus=2999999884200771585/1c29a2240000000001",
		}
		Expect(got).To(Equal(want))
	})

	// The rows above that meet an infinity or 2^63 fail on addExact, so the
	// saturated operand itself never reaches an entry there, and a saturation to
	// the wrong sign would fail the same way. `d + 0` and `f + 0` cannot overflow,
	// so their entries ARE the saturated operand: both engines write them, and the
	// raw bytes are compared.
	It("stores the saturated operand itself where the sum cannot overflow", func() {
		ctx := context.Background()
		java := NewJavaInvoker()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		name := "WSJSAT_" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")
		body := "CREATE TABLE T (id BIGINT, d DOUBLE, n BIGINT, f FLOAT, PRIMARY KEY (id)) " +
			"CREATE INDEX DZERO AS SELECT d + 0 FROM T ORDER BY d + 0 " +
			"CREATE INDEX FZERO AS SELECT f + 0 FROM T ORDER BY f + 0"
		var created struct {
			Created bool `json:"created"`
		}
		Expect(java.InvokeAs(ctx, "createSchemaTemplatePersistentJava", map[string]any{
			"clusterFile": clusterFile, "templateName": name, "schemaTemplateBody": body,
		}, &created)).To(Succeed())
		defer func() {
			var dropped struct {
				Dropped bool `json:"dropped"`
			}
			_ = java.InvokeAs(context.Background(), "dropSchemaTemplatePersistentJava", map[string]any{
				"clusterFile": clusterFile, "templateName": name,
			}, &dropped)
		}()
		var store struct {
			DbPath      string `json:"dbPath"`
			SchemaName  string `json:"schemaName"`
			StorePrefix []int  `json:"storePrefix"`
		}
		Expect(java.InvokeAs(ctx, "wsjOpenStoreJava", map[string]any{"clusterFile": clusterFile, "templateName": name}, &store)).To(Succeed())
		defer func() {
			var dropped struct {
				Dropped bool `json:"dropped"`
			}
			_ = java.InvokeAs(context.Background(), "wsjDropDatabaseJava", map[string]any{"clusterFile": clusterFile, "dbPath": store.DbPath}, &dropped)
		}()
		// d and f per row; the FLOAT column narrows its value (3e38 fits a float).
		rows := []struct{ d, f any }{
			{"Infinity", "Infinity"},
			{"-Infinity", "-Infinity"},
			{9.223372036854775807e18, 9.223372036854775807e18},
			{-9.3e18, -9.3e18},
			{1e300, 3e38},
			{-1e300, -3e38},
			{"NaN", "NaN"},
			{-1.5, -1.5},
		}
		value := func(v any) float64 {
			if s, ok := v.(string); ok {
				switch s {
				case "NaN":
					return math.NaN()
				case "Infinity":
					return math.Inf(1)
				}
				return math.Inf(-1)
			}
			return v.(float64)
		}
		var javaRows [][]any
		for i, r := range rows {
			javaRows = append(javaRows, []any{i + 1, r.d, i + 1, r.f})
		}
		var inserted struct {
			Outcomes []string `json:"outcomes"`
		}
		Expect(java.InvokeAs(ctx, "wsjInsertDoublesJava", map[string]any{
			"clusterFile": clusterFile, "dbPath": store.DbPath, "schemaName": store.SchemaName, "rows": javaRows,
		}, &inserted)).To(Succeed())
		cat, err := catalog.OpenRecordLayerStoreCatalog()
		Expect(err).NotTo(HaveOccurred())
		db := recordlayer.NewFDBDatabase(sharedDB)
		var goOutcomes []string
		for i, r := range rows {
			_, serr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				tmpl, err := cat.SchemaTemplateCatalog().LoadSchemaTemplate(catalog.NewFDBTransaction(rtx), name)
				if err != nil {
					return nil, err
				}
				md := tmpl.(*metadata.RecordLayerSchemaTemplate).Underlying()
				prefix := make([]byte, len(store.StorePrefix))
				for j, b := range store.StorePrefix {
					prefix[j] = byte(b)
				}
				rs, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(subspace.FromBytes(prefix)).Open()
				if err != nil {
					return nil, err
				}
				rt := md.GetRecordType("T")
				msg := dynamicpb.NewMessage(rt.Descriptor)
				fields := rt.Descriptor.Fields()
				msg.Set(fields.ByName("ID"), protoreflect.ValueOfInt64(int64(i+101)))
				msg.Set(fields.ByName("D"), protoreflect.ValueOfFloat64(value(r.d)))
				msg.Set(fields.ByName("N"), protoreflect.ValueOfInt64(int64(i+101)))
				msg.Set(fields.ByName("F"), protoreflect.ValueOfFloat32(float32(value(r.f))))
				_, err = rs.SaveRecord(msg)
				return nil, err
			})
			if serr != nil {
				goOutcomes = append(goOutcomes, "ERROR "+wsjTrim(serr.Error()))
			} else {
				goOutcomes = append(goOutcomes, "OK")
			}
		}
		dzero := wsjIndexEntriesByID(ctx, java, clusterFile, store.DbPath, store.SchemaName, "DZERO")
		fzero := wsjIndexEntriesByID(ctx, java, clusterFile, store.DbPath, store.SchemaName, "FZERO")
		var got []string
		for i, r := range rows {
			jid, gid := int64(i+1), int64(i+101)
			got = append(got, fmt.Sprintf("d=%v f=%v java=%s go=%s dzero=%s/%s fzero=%s/%s", r.d, r.f, inserted.Outcomes[i], goOutcomes[i],
				dzero[jid].value, dzero[jid].raw, fzero[jid].value, fzero[jid].raw))
			Expect(dzero[gid]).To(Equal(dzero[jid]), "the Go-written DZERO entry of d=%v is the target's", r.d)
			Expect(fzero[gid]).To(Equal(fzero[jid]), "the Go-written FZERO entry of f=%v is the target's", r.f)
		}
		for _, l := range got {
			fmt.Fprintf(GinkgoWriter, "WSJSAT-PIN %q,\n", l)
		}
		// Measured (4.14.2.0): Number.longValue() saturates an infinity or an
		// out-of-range value to Long.MAX_VALUE or Long.MIN_VALUE by sign, and NaN to 0.
		want := []string{
			"d=Infinity f=Infinity java=OK go=OK dzero=9223372036854775807/1c7fffffffffffffff fzero=9223372036854775807/1c7fffffffffffffff",
			"d=-Infinity f=-Infinity java=OK go=OK dzero=-9223372036854775808/0c7fffffffffffffff fzero=-9223372036854775808/0c7fffffffffffffff",
			"d=9.223372036854776e+18 f=9.223372036854776e+18 java=OK go=OK dzero=9223372036854775807/1c7fffffffffffffff fzero=9223372036854775807/1c7fffffffffffffff",
			"d=-9.3e+18 f=-9.3e+18 java=OK go=OK dzero=-9223372036854775808/0c7fffffffffffffff fzero=-9223372036854775808/0c7fffffffffffffff",
			"d=1e+300 f=3e+38 java=OK go=OK dzero=9223372036854775807/1c7fffffffffffffff fzero=9223372036854775807/1c7fffffffffffffff",
			"d=-1e+300 f=-3e+38 java=OK go=OK dzero=-9223372036854775808/0c7fffffffffffffff fzero=-9223372036854775808/0c7fffffffffffffff",
			"d=NaN f=NaN java=OK go=OK dzero=0/14 fzero=0/14",
			"d=-1.5 f=-1.5 java=OK go=OK dzero=-1/13fe fzero=-1/13fe",
		}
		Expect(got).To(Equal(want))
	})
})

// wsjEntry is one index entry: its decoded value (for a pin's readability) and
// the RAW bytes of everything before the primary key, as lowercase hex.
type wsjEntry struct{ value, raw string }

// wsjIndexEntriesByID reads an index of a store wsjOpenStoreJava kept, through the
// target, keyed by the id of the row. The key below the index subspace is
// pack(value) || pack(0) || pack(id), so the raw bytes strip the primary key's own
// encoding, checked to be there, and keep the rest byte for byte.
func wsjIndexEntriesByID(ctx context.Context, java *JavaInvoker, clusterFile, dbPath, schemaName, index string) map[int64]wsjEntry {
	var out struct {
		Entries []struct {
			Tuple string `json:"tuple"`
			Hex   string `json:"hex"`
		} `json:"entries"`
	}
	Expect(java.InvokeAs(ctx, "wsjIndexEntriesJava", map[string]any{
		"clusterFile": clusterFile, "dbPath": dbPath, "schemaName": schemaName, "indexName": index,
	}, &out)).To(Succeed())
	byID := map[int64]wsjEntry{}
	for _, e := range out.Entries {
		// "(<value>, <record type key>, <id>)": the indexed value, then the
		// primary key, which the relational layer prefixes with T's type key 0.
		inner := strings.TrimSuffix(strings.TrimPrefix(e.Tuple, "("), ")")
		parts := strings.Split(inner, ", ")
		Expect(len(parts)).To(BeNumerically(">=", 3), e.Tuple)
		Expect(parts[len(parts)-2]).To(Equal("0"), "the record type key of T: %s", e.Tuple)
		id, perr := strconv.ParseInt(parts[len(parts)-1], 10, 64)
		Expect(perr).NotTo(HaveOccurred(), e.Tuple)
		pkHex := hex.EncodeToString(tuple.Tuple{int64(0), id}.Pack())
		Expect(e.Hex).To(HaveSuffix(pkHex), "the raw entry of id %d ends in its primary key: %s", id, e.Hex)
		byID[id] = wsjEntry{value: strings.Join(parts[:len(parts)-2], ", "), raw: strings.TrimSuffix(e.Hex, pkHex)}
	}
	return byID
}

// What the target does with a stored index whose option list names a key twice
// (Index.java:253-266 rebuilds the options through a Guava ImmutableMap.Builder,
// which refuses a repeated key), and what Go's loader does with the same index:
// each case is given to the JVM step and, inside stored metadata, to
// recordlayer.RecordMetaDataFromProto, and the two outcomes are compared, so a
// drift of either message reddens this spec. The first case is also the input
// of the record-layer pin TestIndexOptionOrder_FromProtoRefusesDuplicateKey.
const wsjDuplicateOptionGoMessage = "Multiple entries with same key: unique=false and unique=true"

// wsjGoDuplicateOptionOutcome loads metadata holding one index with the given
// options through Go's proto loader and renders the outcome the way the JVM step
// renders its own: "ERROR java.lang.IllegalArgumentException <message>" for a
// DuplicateIndexOptionError, "OK" when the index loads.
func wsjGoDuplicateOptionOutcome(options [][]string) string {
	b := recordlayer.NewRecordMetaDataBuilder()
	b.SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	built, err := b.Build()
	Expect(err).NotTo(HaveOccurred())
	md, err := built.ToProto()
	Expect(err).NotTo(HaveOccurred())
	md.Version = proto.Int32(1)
	idx := &gen.Index{
		Name: proto.String("WSJ_DUP"), RecordType: []string{"Order"},
		RootExpression: recordlayer.Field("order_id").ToKeyExpression(),
		Type:           proto.String("value"), AddedVersion: proto.Int32(1), LastModifiedVersion: proto.Int32(1),
	}
	for _, kv := range options {
		idx.Options = append(idx.Options, &gen.Index_Option{Key: proto.String(kv[0]), Value: proto.String(kv[1])})
	}
	md.Indexes = append(md.Indexes, idx)
	_, err = recordlayer.RecordMetaDataFromProto(md)
	if err == nil {
		return "OK"
	}
	var dup *recordlayer.DuplicateIndexOptionError
	if errors.As(err, &dup) {
		return "ERROR java.lang.IllegalArgumentException " + dup.Error()
	}
	return "ERROR go " + err.Error()
}

var _ = Describe("WS-J duplicate index option", func() {
	It("records the target's refusal of a stored index that repeats an option key", func() {
		java := NewJavaInvoker()
		cases := []struct {
			name    string
			options [][]string
			want    string
		}{
			{
				"go_pin_input",
				[][]string{{"unique", "true"}, {"x", "1"}, {"unique", "false"}},
				"ERROR java.lang.IllegalArgumentException " + wsjDuplicateOptionGoMessage,
			},
			{
				"adjacent",
				[][]string{{"a", "1"}, {"a", "2"}},
				"ERROR java.lang.IllegalArgumentException Multiple entries with same key: a=2 and a=1",
			},
			{
				"no_duplicate",
				[][]string{{"unique", "true"}, {"x", "1"}},
				"OK {unique=true, x=1}",
			},
		}
		for _, c := range cases {
			var out struct {
				Outcome string `json:"outcome"`
			}
			Expect(java.InvokeAs(context.Background(), "wsjDuplicateIndexOptionJava", map[string]any{"options": c.options}, &out)).To(Succeed())
			goOutcome := wsjGoDuplicateOptionOutcome(c.options)
			fmt.Fprintf(GinkgoWriter, "WSJD %s %q go=%q\n", c.name, out.Outcome, goOutcome)
			Expect(out.Outcome).To(Equal(c.want), "the target's outcome for %s", c.name)
			// Go's loader refuses the same inputs with the same message; a loaded
			// index is "OK" in Go, whose options map has no rendering to compare.
			if strings.HasPrefix(c.want, "OK ") {
				Expect(goOutcome).To(Equal("OK"), "Go loads %s", c.name)
			} else {
				Expect(goOutcome).To(Equal(out.Outcome), "Go's loader matches the target for %s", c.name)
			}
		}
	})
})

// A stored Index proto read by both engines: Java's `new Index(proto)` (the
// constructor every stored index goes through) and Go's loader. The deprecated
// value_expression is folded into the root, a stored index without an added
// version is read as added at version 1, the deprecated index_type supplies
// the type and options, and a RANK/COUNT/SUM root that is not a grouping is
// wrapped in one; the root each engine then maintains is compared as a proto.
var _ = Describe("WS-J stored index protos read as Java reads them", func() {
	It("reads each stored shape to the same root, versions and type", func() {
		java := NewJavaInvoker()
		f := recordlayer.Field
		kx := func(e recordlayer.KeyExpression) *gen.KeyExpression { return e.ToKeyExpression() }
		cases := []struct {
			name string
			idx  *gen.Index
			// buildRefusal, when set, is the KeyExpression.InvalidExpressionException
			// both engines' meta-data validators then refuse the read index with:
			// the read is compared on Java's side, and Go's build here.
			buildRefusal string
		}{
			{"value expression over a field root, no added version", &gen.Index{
				RootExpression: kx(f("price")), ValueExpression: kx(f("quantity")),
				Type: proto.String("value"), LastModifiedVersion: proto.Int32(3),
			}, ""},
			{"value expression over a concatenated root", &gen.Index{
				RootExpression: kx(recordlayer.Concat(f("price"), f("order_id"))), ValueExpression: kx(recordlayer.Concat(f("quantity"), f("price"))),
				Type: proto.String("value"), AddedVersion: proto.Int32(2), LastModifiedVersion: proto.Int32(3),
			}, ""},
			{"a value expression of no columns", &gen.Index{
				RootExpression: kx(f("price")), ValueExpression: kx(recordlayer.EmptyKey()),
				Type: proto.String("value"), AddedVersion: proto.Int32(1), LastModifiedVersion: proto.Int32(1),
			}, ""},
			{"index_type UNIQUE with a value expression", &gen.Index{
				RootExpression: kx(f("order_id")), ValueExpression: kx(f("price")),
				IndexType: gen.Index_UNIQUE.Enum(), LastModifiedVersion: proto.Int32(2),
			}, ""},
			// Java's Index(proto) wraps it with every column grouped
			// (Index.java:206-213), which its COUNT validator then refuses.
			{"a COUNT root that is not a grouping", &gen.Index{
				RootExpression: kx(f("price")), Type: proto.String("count"),
				AddedVersion: proto.Int32(1), LastModifiedVersion: proto.Int32(1),
			}, "index type does not support non-group fields; use COUNT_NOT_NULL"},
			// index_type beside a stored option list: Java takes the type and
			// options from index_type and ignores the list (Index.java:198-204),
			// the branch the carry rule's comparison relies on.
			{"index_type UNIQUE beside an option list", &gen.Index{
				RootExpression: kx(f("order_id")), IndexType: gen.Index_UNIQUE.Enum(),
				Options: []*gen.Index_Option{
					{Key: proto.String("unique"), Value: proto.String("false")},
					{Key: proto.String("x"), Value: proto.String("1")},
				},
				AddedVersion: proto.Int32(1), LastModifiedVersion: proto.Int32(2),
			}, ""},
		}
		for _, c := range cases {
			c.idx.Name = proto.String("WSJ_IDX")
			c.idx.RecordType = []string{"Order"}
			encoded, err := proto.Marshal(c.idx)
			Expect(err).NotTo(HaveOccurred())
			var out struct {
				Outcome             string `json:"outcome"`
				Root                []int  `json:"root"`
				AddedVersion        int    `json:"addedVersion"`
				LastModifiedVersion int    `json:"lastModifiedVersion"`
				Type                string `json:"type"`
				Options             string `json:"options"`
			}
			ints := make([]int, len(encoded))
			for i, b := range encoded {
				ints[i] = int(b)
			}
			Expect(java.InvokeAs(context.Background(), "wsjIndexFromProtoJava", map[string]any{"indexProto": ints}, &out)).To(Succeed())
			Expect(out.Outcome).To(Equal("OK"), c.name)
			javaRoot := &gen.KeyExpression{}
			rootBytes := make([]byte, len(out.Root))
			for i, b := range out.Root {
				rootBytes[i] = byte(b)
			}
			Expect(proto.Unmarshal(rootBytes, javaRoot)).To(Succeed())

			b := recordlayer.NewRecordMetaDataBuilder()
			b.SetRecords(gen.File_record_layer_demo_proto)
			b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
			b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
			b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
			built, err := b.Build()
			Expect(err).NotTo(HaveOccurred())
			md, err := built.ToProto()
			Expect(err).NotTo(HaveOccurred())
			md.Version = proto.Int32(3)
			md.Indexes = append(md.Indexes, c.idx)
			loaded, err := recordlayer.RecordMetaDataFromProto(md)
			if c.buildRefusal != "" {
				Expect(javaRoot.GetGrouping()).NotTo(BeNil(), "%s: Java reads the root as a grouping", c.name)
				Expect(javaRoot.GetGrouping().GetGroupedCount()).To(Equal(int32(1)), "%s: every column grouped", c.name)
				// Java's build of the same meta-data refuses it with this class
				// and text: conformance_test's "Index validation at build, as
				// Java builds", "a count over a bare field".
				var keyErr *recordlayer.KeyExpressionError
				Expect(errors.As(err, &keyErr)).To(BeTrue(), "%s: Go: %T %v", c.name, err, err)
				Expect(keyErr.Message).To(Equal(c.buildRefusal), c.name)
				continue
			}
			Expect(err).NotTo(HaveOccurred(), c.name)
			goIdx := loaded.GetIndex("WSJ_IDX")
			goRoot := goIdx.RootExpression.ToKeyExpression()
			fmt.Fprintf(GinkgoWriter, "WSJIX %s java=%v go=%v added=%d/%d type=%s/%s\n", c.name,
				javaRoot, goRoot, out.AddedVersion, goIdx.AddedVersion, out.Type, goIdx.Type)
			Expect(proto.Equal(goRoot, javaRoot)).To(BeTrue(), "%s: Go maintains root %v, Java %v", c.name, goRoot, javaRoot)
			Expect(goIdx.AddedVersion).To(Equal(out.AddedVersion), c.name)
			Expect(goIdx.LastModifiedVersion).To(Equal(out.LastModifiedVersion), c.name)
			Expect(goIdx.Type).To(Equal(out.Type), c.name)
			// Java renders its option map with Map.toString, "{k=v, k2=v2}";
			// compared as maps (Go's option order is its own concern, 4b).
			javaOptions := map[string]string{}
			if inner := strings.TrimSuffix(strings.TrimPrefix(out.Options, "{"), "}"); inner != "" {
				for _, kv := range strings.Split(inner, ", ") {
					k, v, ok := strings.Cut(kv, "=")
					Expect(ok).To(BeTrue(), "%s: Java option %q", c.name, kv)
					javaOptions[k] = v
				}
			}
			goOptions := map[string]string{}
			for k, v := range goIdx.Options {
				goOptions[k] = v
			}
			fmt.Fprintf(GinkgoWriter, "WSJIX %s options java=%s go=%v\n", c.name, out.Options, goOptions)
			Expect(goOptions).To(Equal(javaOptions), "%s: options", c.name)
		}
	})

	// A stored subspace key must pack exactly one non-null item
	// (Index.decodeSubspaceKey and normalizeSubspaceKey, Index.java:80-97); Go
	// used to fall back to the index's name for each of these.
	It("refuses each malformed stored subspace key as Java does", func() {
		java := NewJavaInvoker()
		type result struct {
			name, java, javaWant, goClass, goWant string
			goErr                                 error
		}
		var results []result
		for _, c := range []struct {
			name, javaWant, goClass, goWant string
			key                             []byte
		}{
			{"present and empty", "ERROR com.apple.foundationdb.record.RecordCoreException subspace key must encode a single item tuple", "core", "subspace key must encode a single item tuple", []byte{}},
			{"two items", "ERROR com.apple.foundationdb.record.RecordCoreException subspace key must encode a single item tuple", "core", "subspace key must encode a single item tuple", tuple.Tuple{"a", "b"}.Pack()},
			{"a null item", "ERROR com.apple.foundationdb.record.RecordCoreArgumentException Index subspace key cannot be null", "argument", "Index subspace key cannot be null", tuple.Tuple{nil}.Pack()},
			// Java builds the options with the type, before the root and the key
			// (Index.java:198-221), so a repeated option wins over an empty key.
			{"a repeated option beside an empty key", "ERROR java.lang.IllegalArgumentException Multiple entries with same key: a=2 and a=1", "duplicate", "Multiple entries with same key: a=2 and a=1", []byte{}},
			// The root is read before the key (Index.java:205), and an absent
			// root is refused (KeyExpression.java:404-405); Go skipped it.
			{"an absent root beside an empty key", "ERROR com.apple.foundationdb.record.metadata.expressions.KeyExpression$DeserializationException Exactly one root must be specified for an index", "root", "Exactly one root must be specified for an index", []byte{}},
		} {
			idx := &gen.Index{
				Name: proto.String("WSJ_IDX"), RecordType: []string{"Order"},
				RootExpression: recordlayer.Field("price").ToKeyExpression(), Type: proto.String("value"),
				AddedVersion: proto.Int32(1), LastModifiedVersion: proto.Int32(1), SubspaceKey: c.key,
			}
			if c.goClass == "duplicate" {
				idx.Options = []*gen.Index_Option{{Key: proto.String("a"), Value: proto.String("1")}, {Key: proto.String("a"), Value: proto.String("2")}}
			}
			if c.goClass == "root" {
				idx.RootExpression = nil
			}
			encoded, err := proto.Marshal(idx)
			Expect(err).NotTo(HaveOccurred())
			ints := make([]int, len(encoded))
			for i, b := range encoded {
				ints[i] = int(b)
			}
			var out struct {
				Outcome string `json:"outcome"`
			}
			Expect(java.InvokeAs(context.Background(), "wsjIndexFromProtoJava", map[string]any{"indexProto": ints}, &out)).To(Succeed())

			b := recordlayer.NewRecordMetaDataBuilder()
			b.SetRecords(gen.File_record_layer_demo_proto)
			b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
			b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
			b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
			built, err := b.Build()
			Expect(err).NotTo(HaveOccurred())
			md, err := built.ToProto()
			Expect(err).NotTo(HaveOccurred())
			md.Version = proto.Int32(3)
			md.Indexes = append(md.Indexes, idx)
			// Go reads the marshalled bytes, as a stored template is read, so
			// the presence of an empty key is what Go's unmarshaller keeps.
			stored, err := proto.Marshal(md)
			Expect(err).NotTo(HaveOccurred())
			var decoded gen.MetaData
			Expect(proto.Unmarshal(stored, &decoded)).To(Succeed())
			_, goErr := recordlayer.RecordMetaDataFromProto(&decoded)
			fmt.Fprintf(GinkgoWriter, "WSJIXK %s java=%s go=%v\n", c.name, out.Outcome, goErr)
			results = append(results, result{c.name, out.Outcome, c.javaWant, c.goClass, c.goWant, goErr})
		}
		// Every shape is measured on both engines before any is asserted.
		for _, r := range results {
			Expect(r.java).To(Equal(r.javaWant), r.name)
			Expect(r.goErr).To(HaveOccurred(), r.name)
			var core *recordlayer.RecordCoreError
			var arg *recordlayer.RecordCoreArgumentError
			var dup *recordlayer.DuplicateIndexOptionError
			switch r.goClass {
			case "root":
				var rootErr *recordlayer.KeyExpressionDeserializationError
				Expect(errors.As(r.goErr, &rootErr)).To(BeTrue(), "%s: %v (%T), want Java's DeserializationException", r.name, r.goErr, r.goErr)
				Expect(rootErr.Message).To(HavePrefix(r.goWant), r.name)
				// The root is refused before the key is read: the chain's
				// RecordCoreErrors are the MetaDataProtoDeserializationException
				// (a MetaDataException, and so a RecordCoreException) and the
				// root's refusal (Java's DeserializationException), and no key
				// refusal.
				var pde *recordlayer.MetaDataProtoDeserializationError
				Expect(errors.As(r.goErr, &pde)).To(BeTrue(), r.name)
				Expect(recordlayer.RecordCoreMessages(r.goErr)).To(Equal([]string{pde.Error(), rootErr.Message}), "%s: the root is refused before the key is read", r.name)
			case "duplicate":
				Expect(errors.As(r.goErr, &dup)).To(BeTrue(), "%s: %v (%T), want a DuplicateIndexOptionError", r.name, r.goErr, r.goErr)
				Expect(dup.Error()).To(Equal(r.goWant), r.name)
			case "core":
				Expect(errors.As(r.goErr, &core)).To(BeTrue(), "%s: %v (%T), want a RecordCoreError", r.name, r.goErr, r.goErr)
				Expect(core.Message).To(Equal(r.goWant), r.name)
			case "argument":
				Expect(errors.As(r.goErr, &arg)).To(BeTrue(), "%s: %v (%T), want a RecordCoreArgumentError", r.name, r.goErr, r.goErr)
				Expect(arg.Message).To(Equal(r.goWant), r.name)
				Expect(arg.IndexName).To(Equal("WSJ_IDX"), r.name)
			}
		}
	})
})

// A bit or bitmap operator over an operand type with no lane, in an index key.
// The target builds the index's Values through ArithmeticValue.encapsulate at the
// DDL clause, so it refuses each such index when the template is created, with
// the query path's outcome (XX000: a VerifyException for a primitive with no row
// in the operator table, a SemanticException for a non-primitive operand,
// ArithmeticValue.java:213-231). Go's key generator builds these keys without
// consulting the lane table (ddl/generator.go, the ArithmeticValue and bit
// ScalarFunctionValue arms), so its DDL stored a key the target can never plan
// (ws-j-design.md section 3.2, "the lane check"): eight shapes stored, the two
// STRUCT shapes refused by the metadata build with another message. The
// generator now runs encapsulate's checks at the clause (encapsulateLane), and
// Go's outcome is asserted equal to the target's on SQLSTATE and message: Go
// renders an error as ERROR <code> "<message>" with no exception class, so the
// target's class is not compared.
var _ = Describe("WS-J bit and bitmap index keys over an operand with no lane", func() {
	encapsulate := `ERROR XX000 VerifyException "unable to encapsulate arithmetic operation due to type mismatch(es)"`
	complexArg := `ERROR XX000 SemanticException "The argument to an arithmetic operator expecting an argument of a primitive type, is invoked with an argument of a complex type, e.g. an array or a record."`
	withoutClass := regexp.MustCompile(`^ERROR (\S+) \S+ `)
	for _, c := range []struct{ name, body, want string }{
		{"bitand_double", `create table t(id bigint, v double, primary key(id)) create index ix as select v & 1 from t order by v & 1`, encapsulate},
		{"bitand_float", `create table t(id bigint, v float, primary key(id)) create index ix as select v & 1 from t order by v & 1`, encapsulate},
		{"bitand_string", `create table t(id bigint, v string, primary key(id)) create index ix as select v & 1 from t order by v & 1`, encapsulate},
		{"bitand_boolean", `create table t(id bigint, v boolean, primary key(id)) create index ix as select v & 1 from t order by v & 1`, encapsulate},
		{"bitand_struct", `create type as struct s1(x bigint) create table t(id bigint, v s1, primary key(id)) create index ix as select v & 1 from t order by v & 1`, complexArg},
		{"bucket_offset_double", `create table t(id bigint, v double, primary key(id)) create index ix as select bitmap_bucket_offset(v) from t order by bitmap_bucket_offset(v)`, encapsulate},
		{"bucket_offset_float", `create table t(id bigint, v float, primary key(id)) create index ix as select bitmap_bucket_offset(v) from t order by bitmap_bucket_offset(v)`, encapsulate},
		{"bucket_offset_string", `create table t(id bigint, v string, primary key(id)) create index ix as select bitmap_bucket_offset(v) from t order by bitmap_bucket_offset(v)`, encapsulate},
		{"bucket_offset_boolean", `create table t(id bigint, v boolean, primary key(id)) create index ix as select bitmap_bucket_offset(v) from t order by bitmap_bucket_offset(v)`, encapsulate},
		{"bucket_offset_struct", `create type as struct s1(x bigint) create table t(id bigint, v s1, primary key(id)) create index ix as select bitmap_bucket_offset(v) from t order by bitmap_bucket_offset(v)`, complexArg},
		// Two faults: a lane-less index key BEFORE a later table the target
		// refuses. The target registers every table before it generates any
		// index (ws-j-design.md section 4), so the table's fault is reported.
		{"bitand_double_then_bad_table", `create table t(id bigint, v double, primary key(id)) create index ix as select v & 1 from t order by v & 1 create table z(id bigint, x nosuchtype, primary key(id))`, `ERROR 42F18 RelationalException "could not find type 'NOSUCHTYPE'"`},
	} {
		It("the target refuses the DDL: "+c.name, func() {
			ctx := context.Background()
			java := NewJavaInvoker()
			clusterFile, err := sharedContainer.ClusterFile(ctx)
			Expect(err).NotTo(HaveOccurred())
			clusterFilePath := writeClusterFileToTemp(clusterFile)
			defer os.Remove(clusterFilePath)
			suffix := strings.ReplaceAll(uuid.New().String()[:8], "-", "")
			name := "WSJLANE_" + suffix
			var created struct {
				Created bool `json:"created"`
			}
			javaErr := java.InvokeAs(ctx, "createSchemaTemplatePersistentJava", map[string]any{
				"clusterFile": clusterFile, "templateName": name, "schemaTemplateBody": c.body,
			}, &created)
			if javaErr == nil {
				var dropped struct {
					Dropped bool `json:"dropped"`
				}
				_ = java.InvokeAs(context.Background(), "dropSchemaTemplatePersistentJava", map[string]any{
					"clusterFile": clusterFile, "templateName": name,
				}, &dropped)
			}
			target := "OK"
			var je *JavaError
			if errors.As(javaErr, &je) {
				target = fmt.Sprintf("ERROR %s %s %q", je.SQLState, je.ExceptionClass, je.Message)
			} else if javaErr != nil {
				target = "ERROR " + javaErr.Error()
			}
			goOutcome := func() string {
				sysDB, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s", clusterFilePath))
				if err != nil {
					return "ERROR " + err.Error()
				}
				defer sysDB.Close()
				goName := "WSJLANE_GO_" + suffix
				defer func() { _, _ = sysDB.ExecContext(context.Background(), "DROP SCHEMA TEMPLATE IF EXISTS "+goName) }()
				if _, err := sysDB.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+goName+" "+c.body); err != nil {
					var ae *api.Error
					if errors.As(err, &ae) {
						return fmt.Sprintf("ERROR %s %q", ae.Code, ae.Message)
					}
					return "ERROR " + err.Error()
				}
				return "OK"
			}()
			fmt.Fprintf(GinkgoWriter, "WSJLANE %s target=%s go=%s\n", c.name, target, goOutcome)
			Expect(target).To(Equal(c.want), "the target's outcome for %s", c.name)
			Expect(goOutcome).To(Equal(withoutClass.ReplaceAllString(c.want, "ERROR $1 ")), "Go's outcome for %s, the target's", c.name)
		})
	}
})

// Shape (d) (ws-j-design.md section 4c) refuses stored metadata whose message
// named UnionDescriptor is not the union found when the pre-upgrade Go loader,
// taking that message for the union, would have loaded the same bytes: its loop
// made a record type of a message-typed field, every such record type has a
// stored primary key, and no index covers a record type the loop did not make.
// The target's DDL creates such a message: a table or STRUCT named
// "UnionDescriptor" one of whose columns the loop frames (a column typed by a
// table, or one of any type named _X for a table X), every framed column naming
// a table, and no index on a table it does not frame. This
// measures that the target creates and stores each shape, and pins what Go does
// with the target's stored template and with the same DDL through its own
// driver: refused with 0A000 (the declared residual of shape (d)) when the
// replayed pre-upgrade loader loads it, and loaded when it does not.
var _ = Describe("WS-J a column typed by a table", func() {
	for _, c := range []struct {
		name, body string
	}{
		{"table", `CREATE TABLE A (id BIGINT, PRIMARY KEY (id)) CREATE TABLE "UnionDescriptor" (id BIGINT, a A, PRIMARY KEY (id))`},
		{"struct", `CREATE TABLE A (id BIGINT, PRIMARY KEY (id)) CREATE TYPE AS STRUCT "UnionDescriptor" (a A) CREATE TABLE T (id BIGINT, s "UnionDescriptor", PRIMARY KEY (id))`},
		// A STRUCT nested in a STRUCT: the holder is two levels below the table.
		{"struct nested in a struct", `CREATE TABLE A (id BIGINT, PRIMARY KEY (id)) CREATE TYPE AS STRUCT "UnionDescriptor" (a A) CREATE TYPE AS STRUCT W (u "UnionDescriptor") CREATE TABLE T (id BIGINT, w W, PRIMARY KEY (id))`},
		// A STRUCT or UUID column made a record type with no stored primary key,
		// and an index covers a table the loop did not make: the pre-upgrade loader
		// refused each, so nothing was framed.
		{"table with a STRUCT column", `CREATE TYPE AS STRUCT S (x BIGINT) CREATE TABLE "UnionDescriptor" (id BIGINT, s S, PRIMARY KEY (id))`},
		{"table with a UUID column", `CREATE TABLE "UnionDescriptor" (id BIGINT, u UUID, PRIMARY KEY (id))`},
		{"table with a table-typed column and an index", `CREATE TABLE A (id BIGINT, PRIMARY KEY (id)) CREATE TABLE "UnionDescriptor" (id BIGINT, a A, PRIMARY KEY (id)) CREATE INDEX I AS SELECT id FROM "UnionDescriptor" ORDER BY id`},
		// The replay's two other named shapes: a column of any type named _X for a
		// table X, which the pre-upgrade loop framed by its name; and a table named
		// "UUID" beside a UnionDescriptor table with a UUID column, which the
		// replay refuses though the pre-upgrade load failed on a check it does not
		// replay (the safe side).
		{"table with a column named for a table", `CREATE TABLE A (id BIGINT, PRIMARY KEY (id)) CREATE TABLE "UnionDescriptor" (id BIGINT, "_A" BIGINT, PRIMARY KEY (id))`},
		{"a UUID table beside a UUID column", `CREATE TABLE "UUID" (id BIGINT, PRIMARY KEY (id)) CREATE TABLE "UnionDescriptor" (id BIGINT, u UUID, PRIMARY KEY (id))`},
	} {
		It("the target creates it: "+c.name, func() {
			ctx := context.Background()
			java := NewJavaInvoker()
			clusterFile, err := sharedContainer.ClusterFile(ctx)
			Expect(err).NotTo(HaveOccurred())
			clusterFilePath := writeClusterFileToTemp(clusterFile)
			defer os.Remove(clusterFilePath)
			db := recordlayer.NewFDBDatabase(sharedDB)
			suffix := strings.ReplaceAll(uuid.New().String()[:8], "-", "")
			name := "WSJTT_" + suffix
			var created struct {
				Created bool `json:"created"`
			}
			Expect(java.InvokeAs(ctx, "createSchemaTemplatePersistentJava", map[string]any{
				"clusterFile": clusterFile, "templateName": name, "schemaTemplateBody": c.body,
			}, &created)).To(Succeed(), "the target creates %q", c.body)
			Expect(created.Created).To(BeTrue())
			defer func() {
				var dropped struct {
					Dropped bool `json:"dropped"`
				}
				_ = java.InvokeAs(context.Background(), "dropSchemaTemplatePersistentJava", map[string]any{
					"clusterFile": clusterFile, "templateName": name,
				}, &dropped)
			}()
			javaMD := loadStoredJavaTemplateMetaData(ctx, db, name)
			var legacy *descriptorpb.DescriptorProto
			for _, m := range javaMD.GetRecords().GetMessageType() {
				if m.GetName() == "UnionDescriptor" {
					legacy = m
				}
			}
			Expect(legacy).NotTo(BeNil(), "the target stores a message named UnionDescriptor")
			if strings.Contains(c.body, `"_A"`) {
				// The relational layer keeps a single leading underscore
				// (ProtoUtils.toProtoBufCompliantName), so the column is stored
				// under the name the pre-upgrade loop framed by.
				Expect(legacy.GetField()).To(ContainElement(WithTransform(
					func(f *descriptorpb.FieldDescriptorProto) string { return f.GetName() }, Equal("_A"))),
					"the stored UnionDescriptor has a field named _A")
			} else {
				// The target stores a column's message type by name, leaving the kind to
				// be resolved when the file is built.
				Expect(legacy.GetField()).To(ContainElement(WithTransform(
					func(f *descriptorpb.FieldDescriptorProto) string { return f.GetTypeName() }, Not(BeEmpty()))),
					"the stored UnionDescriptor has a message-typed field")
			}

			cat, err := catalog.OpenRecordLayerStoreCatalog()
			Expect(err).NotTo(HaveOccurred())
			_, targetErr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				return cat.SchemaTemplateCatalog().LoadSchemaTemplate(catalog.NewFDBTransaction(rtx), name)
			})

			goErr := func() error {
				sysDB, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s", clusterFilePath))
				if err != nil {
					return err
				}
				defer sysDB.Close()
				goName := "WSJTT_GO_" + suffix
				defer func() { _, _ = sysDB.ExecContext(context.Background(), "DROP SCHEMA TEMPLATE IF EXISTS "+goName) }()
				_, err = sysDB.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+goName+" "+c.body)
				return err
			}()
			fmt.Fprintf(GinkgoWriter, "WSJTT %s target-stored=%v go-driver=%v\n", c.name, targetErr, goErr)
			// Both are served as the target serves them: Go reads the stored
			// template as the target does, and its driver stores the template it
			// built. (Before the owner's ruling on pre-release data, RFC-257 item
			// 9, Go refused the shapes marked refused, which the pre-upgrade Go
			// loader could have framed by the message named UnionDescriptor.)
			Expect(targetErr).NotTo(HaveOccurred(), "the target's stored template")
			Expect(goErr).NotTo(HaveOccurred(), "Go's driver-built template")
		})
	}
})

// A SQL table or STRUCT named "UnionDescriptor" is a data message of that name,
// beside the union the relational builder names RecordTypeUnion. The target creates
// and serves such a template, and so does Go. Both halves run through the
// production paths: Go's catalog library loads the target's stored template
// (LoadSchemaTemplate, which deserializes through RecordMetaDataFromProto), and the
// Go SQL driver creates the same DDL and serves a query over a schema bound to it.
// Both outcomes are taken before either is asserted, so a failure in one half
// cannot hide the other.
var _ = Describe("WS-J a table or struct named UnionDescriptor", func() {
	for _, c := range []struct{ name, body, query string }{
		{"table", `CREATE TABLE "UnionDescriptor" (id BIGINT, PRIMARY KEY (id))`, `SELECT id FROM "UnionDescriptor"`},
		{"struct", `CREATE TYPE AS STRUCT "UnionDescriptor" (x BIGINT) CREATE TABLE T (id BIGINT, s "UnionDescriptor", PRIMARY KEY (id))`, `SELECT id FROM T`},
	} {
		It("loads in both engines: "+c.name, func() {
			ctx := context.Background()
			java := NewJavaInvoker()
			clusterFile, err := sharedContainer.ClusterFile(ctx)
			Expect(err).NotTo(HaveOccurred())
			clusterFilePath := writeClusterFileToTemp(clusterFile)
			defer os.Remove(clusterFilePath)
			db := recordlayer.NewFDBDatabase(sharedDB)
			suffix := strings.ReplaceAll(uuid.New().String()[:8], "-", "")
			name := "WSJUD_" + suffix
			var created struct {
				Created bool `json:"created"`
			}
			Expect(java.InvokeAs(ctx, "createSchemaTemplatePersistentJava", map[string]any{
				"clusterFile": clusterFile, "templateName": name, "schemaTemplateBody": c.body,
			}, &created)).To(Succeed())
			Expect(created.Created).To(BeTrue())
			defer func() {
				var dropped struct {
					Dropped bool `json:"dropped"`
				}
				_ = java.InvokeAs(context.Background(), "dropSchemaTemplatePersistentJava", map[string]any{
					"clusterFile": clusterFile, "templateName": name,
				}, &dropped)
			}()
			javaMD := loadStoredJavaTemplateMetaData(ctx, db, name)
			Expect(javaMD.GetRecords().GetMessageType()).To(ContainElement(WithTransform(
				func(m *descriptorpb.DescriptorProto) string { return m.GetName() }, Equal("UnionDescriptor"))),
				"the target stores a message named UnionDescriptor")

			// The target's template, loaded by Go's catalog library.
			cat, err := catalog.OpenRecordLayerStoreCatalog()
			Expect(err).NotTo(HaveOccurred())
			_, targetErr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				return cat.SchemaTemplateCatalog().LoadSchemaTemplate(catalog.NewFDBTransaction(rtx), name)
			})

			// The same DDL through the Go SQL driver, served over a bound schema.
			goErr := func() error {
				sysDB, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s", clusterFilePath))
				if err != nil {
					return err
				}
				defer sysDB.Close()
				dbPath := "/WSJUD_DB_" + suffix
				goName := "WSJUD_GO_" + suffix
				for _, stmt := range []string{
					"CREATE DATABASE " + dbPath,
					"CREATE SCHEMA TEMPLATE " + goName + " " + c.body,
					"CREATE SCHEMA " + dbPath + "/s WITH TEMPLATE " + goName,
				} {
					if _, err := sysDB.ExecContext(ctx, stmt); err != nil {
						return fmt.Errorf("%s: %w", stmt, err)
					}
				}
				defer func() {
					_, _ = sysDB.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+dbPath)
					_, _ = sysDB.ExecContext(context.Background(), "DROP SCHEMA TEMPLATE IF EXISTS "+goName)
				}()
				conn, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=S", strings.ToUpper(dbPath), clusterFilePath))
				if err != nil {
					return err
				}
				defer conn.Close()
				rows, err := conn.QueryContext(ctx, c.query)
				if err != nil {
					return fmt.Errorf("%s: %w", c.query, err)
				}
				defer rows.Close()
				for rows.Next() {
				}
				return rows.Err()
			}()
			fmt.Fprintf(GinkgoWriter, "WSJUD %s target-stored=%v go-driver=%v\n", c.name, targetErr, goErr)
			Expect(map[string]error{"the target's stored template": targetErr, "Go's driver-built template": goErr}).To(Equal(
				map[string]error{"the target's stored template": nil, "Go's driver-built template": nil}))
		})
	}
})

// A column whose records-file field declares a proto2 default and is unset in
// the stored record: Java's result row reads a message field through
// MessageTuple (getFieldOnMessage: an unset field is null), and a top-level
// SELECT * row is a MessageTuple of the stored message (QueryPlan), so both
// SELECT * and SELECT d read null there, not the declared default. Each engine
// creates the template through its own DDL, its catalog row is rewritten raw
// with the defaults (library code can store such a records file; the DDL
// cannot), a row with d and s unset is inserted through SQL, and each engine
// reads it. Each engine reads its own catalog: the Go driver keeps its catalog
// on a Go-only keyspace (TODO.md, "Go SQL driver stores the relational catalog
// and user schemas on a Go-only keyspace").
var _ = Describe("WS-J an unset field with a declared default reads as the target reads it", func() {
	It("SELECT * and SELECT d", func() {
		ctx := context.Background()
		java := NewJavaInvoker()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		clusterFilePath := writeClusterFileToTemp(clusterFile)
		defer os.Remove(clusterFilePath)
		suffix := strings.ReplaceAll(uuid.New().String()[:8], "-", "")
		body := "create table t(id bigint, d bigint, s string, primary key(id))"
		db := recordlayer.NewFDBDatabase(sharedDB)
		// withDefaults writes template (to, 1) raw into the catalog at ss: the
		// bytes of (from, 1) with D [default = 5] and S [default = "x"]. A name
		// neither engine has loaded, so no cached template of the name hides
		// the rewrite.
		withDefaults := func(ss subspace.Subspace, from, to string) {
			cat, err := catalog.NewRecordLayerStoreCatalog(ss)
			Expect(err).NotTo(HaveOccurred())
			catalogMD, err := catalog.BuildCatalogMetaData()
			Expect(err).NotTo(HaveOccurred())
			_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				p, err := cat.SchemaTemplateCatalog().LoadTemplateProto(catalog.NewFDBTransaction(rtx), from, 1)
				if err != nil {
					return nil, err
				}
				for _, m := range p.GetRecords().GetMessageType() {
					if m.GetName() != "T" {
						continue
					}
					for _, f := range m.GetField() {
						switch f.GetName() {
						case "D":
							f.DefaultValue = proto.String("5")
						case "S":
							f.DefaultValue = proto.String("x")
						}
					}
				}
				b, err := proto.Marshal(p)
				if err != nil {
					return nil, err
				}
				store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetSubspace(ss).SetMetaDataProvider(catalogMD).Open()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Templates{TEMPLATE_NAME: proto.String(to), TEMPLATE_VERSION: proto.Int32(1), META_DATA: b})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
		}
		queries := []string{
			"SELECT * FROM T",
			"SELECT d, s FROM T",
			"SELECT id FROM T WHERE d IS NULL",
			"SELECT id FROM T WHERE d = 5",
		}

		// The target.
		name := "WSJDEF_" + suffix
		var created struct {
			Created bool `json:"created"`
		}
		Expect(java.InvokeAs(ctx, "createSchemaTemplatePersistentJava", map[string]any{
			"clusterFile": clusterFile, "templateName": name, "schemaTemplateBody": body,
		}, &created)).To(Succeed())
		defer func() {
			var dropped struct {
				Dropped bool `json:"dropped"`
			}
			_ = java.InvokeAs(context.Background(), "dropSchemaTemplatePersistentJava", map[string]any{
				"clusterFile": clusterFile, "templateName": name,
			}, &dropped)
		}()
		withDefaults(catalog.DefaultCatalogSubspace(), name, name+"_D")
		defer func() {
			var dropped struct {
				Dropped bool `json:"dropped"`
			}
			_ = java.InvokeAs(context.Background(), "dropSchemaTemplatePersistentJava", map[string]any{
				"clusterFile": clusterFile, "templateName": name + "_D",
			}, &dropped)
		}()
		var store struct {
			DbPath     string `json:"dbPath"`
			SchemaName string `json:"schemaName"`
		}
		Expect(java.InvokeAs(ctx, "wsjOpenStoreJava", map[string]any{"clusterFile": clusterFile, "templateName": name + "_D"}, &store)).To(Succeed())
		defer func() {
			var dropped struct {
				Dropped bool `json:"dropped"`
			}
			_ = java.InvokeAs(context.Background(), "wsjDropDatabaseJava", map[string]any{"clusterFile": clusterFile, "dbPath": store.DbPath}, &dropped)
		}()
		// The defaults are not lost on the way: the template the catalog loads
		// and the meta-data a schema's store opens with both declare them.
		var probe struct {
			CachedHasDefault bool   `json:"cachedHasDefault"`
			CachedDefault    string `json:"cachedDefault"`
			StoreHasDefault  bool   `json:"storeHasDefault"`
		}
		Expect(java.InvokeAs(ctx, "wsjTemplateDefaultsJava", map[string]any{"clusterFile": clusterFile, "templateName": name + "_D"}, &probe)).To(Succeed())
		fmt.Fprintf(GinkgoWriter, "WSJDEFPROBE %+v\n", probe)
		Expect(probe.CachedHasDefault && probe.StoreHasDefault && probe.CachedDefault == "5").To(BeTrue(), "%+v", probe)
		var inserted struct {
			Outcome string `json:"outcome"`
		}
		Expect(java.InvokeAs(ctx, "wsjExecuteJava", map[string]any{
			"clusterFile": clusterFile, "dbPath": store.DbPath, "schemaName": store.SchemaName, "sql": "INSERT INTO T (id) VALUES (1)",
		}, &inserted)).To(Succeed())
		Expect(inserted.Outcome).To(Equal("OK 1"))
		// The stored record, loaded by the core record layer under that
		// meta-data: d is unset and MessageHelpers.getFieldOnMessage, FieldValue's
		// reader, reads the default. A query does not: it reads a copy of the
		// record in the plan's type (QueryResult.fromQueriedRecord), so d is null.
		type recordRead struct {
			HasField          bool   `json:"hasField"`
			HasDefaultValue   bool   `json:"hasDefaultValue"`
			GetFieldOnMessage string `json:"getFieldOnMessage"`
		}
		var rec recordRead
		Expect(java.InvokeAs(ctx, "wsjRecordFieldJava", map[string]any{
			"clusterFile": clusterFile, "dbPath": store.DbPath, "schemaName": store.SchemaName, "templateName": name + "_D", "pk": 1,
		}, &rec)).To(Succeed())
		fmt.Fprintf(GinkgoWriter, "WSJDEFREC %+v\n", rec)
		Expect(rec).To(Equal(recordRead{HasField: false, HasDefaultValue: true, GetFieldOnMessage: "5"}))
		var plan map[string]any
		Expect(java.InvokeAs(ctx, "wsjQueryJava", map[string]any{
			"clusterFile": clusterFile, "dbPath": store.DbPath, "schemaName": store.SchemaName, "querySql": "EXPLAIN SELECT d, s FROM T",
		}, &plan)).To(Succeed())
		Expect(fmt.Sprint(plan["rows"])).To(ContainSubstring("SCAN([IS T]) | MAP (_.D AS D, _.S AS S)"), "a FieldValue over the scanned record")
		var javaRows []string
		for _, q := range queries {
			var target map[string]any
			Expect(java.InvokeAs(ctx, "wsjQueryJava", map[string]any{
				"clusterFile": clusterFile, "dbPath": store.DbPath, "schemaName": store.SchemaName, "querySql": q,
			}, &target)).To(Succeed())
			javaRows = append(javaRows, fmt.Sprint(target["rows"]))
		}

		// Go, through its own driver.
		sysDB, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s", clusterFilePath))
		Expect(err).NotTo(HaveOccurred())
		defer sysDB.Close()
		goName := "WSJDEF_GO_" + suffix
		dbPath := "/WSJDEF_DB_" + suffix
		defer func() {
			_, _ = sysDB.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+dbPath)
			_, _ = sysDB.ExecContext(context.Background(), "DROP SCHEMA TEMPLATE IF EXISTS "+goName)
			_, _ = sysDB.ExecContext(context.Background(), "DROP SCHEMA TEMPLATE IF EXISTS "+goName+"_D")
		}()
		_, err = sysDB.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+goName+" "+body)
		Expect(err).NotTo(HaveOccurred())
		withDefaults(keyspace.New(subspace.Sub()).CatalogSubspace(), goName, goName+"_D")
		for _, stmt := range []string{"CREATE DATABASE " + dbPath, "CREATE SCHEMA " + dbPath + "/s WITH TEMPLATE " + goName + "_D"} {
			_, err := sysDB.ExecContext(ctx, stmt)
			Expect(err).NotTo(HaveOccurred(), stmt)
		}
		conn, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=S", strings.ToUpper(dbPath), clusterFilePath))
		Expect(err).NotTo(HaveOccurred())
		defer conn.Close()
		_, err = conn.ExecContext(ctx, "INSERT INTO T (id) VALUES (1)")
		Expect(err).NotTo(HaveOccurred())
		goRows := func(q string) string {
			rows, err := conn.QueryContext(ctx, q)
			if err != nil {
				return "ERROR " + err.Error()
			}
			defer rows.Close()
			cols, err := rows.Columns()
			Expect(err).NotTo(HaveOccurred())
			var out [][]any
			for rows.Next() {
				vals := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				Expect(rows.Scan(ptrs...)).To(Succeed())
				for i, v := range vals {
					if b, ok := v.([]byte); ok {
						vals[i] = string(b)
					}
				}
				out = append(out, vals)
			}
			Expect(rows.Err()).NotTo(HaveOccurred())
			return fmt.Sprint(out)
		}
		var goOut []string
		for i, q := range queries {
			goOut = append(goOut, goRows(q))
			fmt.Fprintf(GinkgoWriter, "WSJDEFAULT %q java=%s go=%s\n", q, javaRows[i], goOut[i])
		}
		Expect(goOut).To(Equal(javaRows))
	})
})

// Section 4's tests whose v1 the target writes (ws-j-design.md section 4, 4e):
// the target creates v1 through its DDL in the shared catalog, and Go carries
// v2, built by its own DDL, from those stored bytes through its catalog library
// (CreateTemplate at the Java-compatible catalog subspace). The record-type keys
// and union fields are the target's, an unchanged index (a literal-bearing one
// among them: F2's population, EQUIVALENT) is the target's Index message, a new
// index is added above, and the target then serves v2: it creates a schema over
// Go's carried template and plans a read through the new index. Test 5: a v1
// holding a long_value bitmap entry size (written raw, the rows a Go build
// before F2 stored) is carried as CHANGED to the int_value root Go's DDL builds,
// and the target plans the bitmap reads over v2.
var _ = Describe("WS-J a new version carried from the target's template", func() {
	carriedFromJava := func(ctx context.Context, java *JavaInvoker, clusterFile, name string) (*gen.MetaData, *catalog.RecordLayerStoreCatalog, *recordlayer.FDBDatabase) {
		cat, err := catalog.OpenRecordLayerStoreCatalog()
		Expect(err).NotTo(HaveOccurred())
		db := recordlayer.NewFDBDatabase(sharedDB)
		var v1 *gen.MetaData
		_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			var lerr error
			v1, lerr = cat.SchemaTemplateCatalog().LoadTemplateProto(catalog.NewFDBTransaction(rtx), name, 1)
			return nil, lerr
		})
		Expect(err).NotTo(HaveOccurred())
		return v1, cat, db
	}
	byName := func(p *gen.MetaData) (map[string]*gen.RecordType, map[string]*gen.Index, map[string]int32) {
		types, indexes, unions := map[string]*gen.RecordType{}, map[string]*gen.Index{}, map[string]int32{}
		for _, rt := range p.GetRecordTypes() {
			types[rt.GetName()] = rt
		}
		for _, idx := range p.GetIndexes() {
			indexes[idx.GetName()] = idx
		}
		for _, m := range p.GetRecords().GetMessageType() {
			if m.GetName() == "RecordTypeUnion" {
				for _, f := range m.GetField() {
					unions[f.GetTypeName()[strings.LastIndexByte(f.GetTypeName(), '.')+1:]] = f.GetNumber()
				}
			}
		}
		return types, indexes, unions
	}
	It("keeps the target's numbering and indexes, and the target serves the carried version", func() {
		ctx := context.Background()
		java := NewJavaInvoker()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		name := "WSJCARRY_" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")
		// Java's order moves each indexed table to the end in clause order: v1's
		// ib then nplus make it B, A. v2 adds an index on b after them, which
		// moves B to the end again, so a fresh build of v2 numbers A before B;
		// the carry keeps v1's numbering.
		v1Body := "create table a(id bigint, x bigint, n bigint, primary key(id)) " +
			"create table b(id bigint, y bigint, primary key(id)) " +
			"create index ib as select y from b order by y " +
			"create index nplus as select n + 1 from a order by n + 1"
		var created struct {
			Created bool `json:"created"`
		}
		Expect(java.InvokeAs(ctx, "createSchemaTemplatePersistentJava", map[string]any{
			"clusterFile": clusterFile, "templateName": name, "schemaTemplateBody": v1Body,
		}, &created)).To(Succeed())
		defer func() {
			var dropped struct {
				Dropped bool `json:"dropped"`
			}
			_ = java.InvokeAs(context.Background(), "dropSchemaTemplatePersistentJava", map[string]any{"clusterFile": clusterFile, "templateName": name}, &dropped)
		}()
		v1, cat, db := carriedFromJava(ctx, java, clusterFile, name)
		built, err := embedded.BuildSchemaTemplateFromDDLNamed(v1Body+" create index ib2 as select id, y from b order by id, y", name)
		Expect(err).NotTo(HaveOccurred())
		fresh, err := built.Underlying().ToProto()
		Expect(err).NotTo(HaveOccurred())
		freshTypes, _, freshUnions := byName(fresh)
		v1Types, _, v1Unions := byName(v1)
		Expect(proto.Equal(freshTypes["A"].GetExplicitKey(), v1Types["A"].GetExplicitKey()) && freshUnions["A"] == v1Unions["A"]).To(BeFalse(),
			"a fresh build of v2 renumbers A, or the spec cannot tell a carry from none")
		v2, err := metadata.NewRecordLayerSchemaTemplateWithVersion(name, built.Underlying(), 2)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			return nil, cat.SchemaTemplateCatalog().CreateTemplate(catalog.NewFDBTransaction(rtx), v2)
		})
		Expect(err).NotTo(HaveOccurred())
		var stored *gen.MetaData
		_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			var lerr error
			stored, lerr = cat.SchemaTemplateCatalog().LoadTemplateProto(catalog.NewFDBTransaction(rtx), name, 2)
			return nil, lerr
		})
		Expect(err).NotTo(HaveOccurred())
		t1, i1, u1 := byName(v1)
		t2, i2, u2 := byName(stored)
		fmt.Fprintf(GinkgoWriter, "WSJCARRY keys v1=%v v2=%v unions v1=%v v2=%v\n", t1, t2, u1, u2)
		for _, tbl := range []string{"A", "B"} {
			Expect(proto.Equal(t2[tbl].GetExplicitKey(), t1[tbl].GetExplicitKey())).To(BeTrue(), "%s's record-type key", tbl)
			Expect(u2[tbl]).To(Equal(u1[tbl]), "%s's union field", tbl)
		}
		for _, ix := range []string{"IB", "NPLUS"} {
			Expect(proto.Equal(i2[ix], i1[ix])).To(BeTrue(), "%s is EQUIVALENT, carried as the target stored it: %v against %v", ix, i2[ix], i1[ix])
		}
		Expect(i2["IB2"]).NotTo(BeNil())
		Expect(i2["IB2"].GetAddedVersion()).To(BeNumerically(">", v1.GetVersion()))
		Expect(i2["IB2"].GetLastModifiedVersion()).To(Equal(i2["IB2"].GetAddedVersion()))

		// The target serves Go's carried v2.
		var served, plan map[string]any
		setup := []string{"insert into a values (1, 10, 5), (2, 20, 6)", "insert into b values (1, 7), (2, 8)"}
		Expect(java.InvokeAs(ctx, "runOnExistingTemplateJava", map[string]any{
			"clusterFile": clusterFile, "templateName": name, "setupSqls": setup, "querySql": "SELECT id, y FROM b ORDER BY id, y",
		}, &served)).To(Succeed())
		Expect(java.InvokeAs(ctx, "runOnExistingTemplateJava", map[string]any{
			"clusterFile": clusterFile, "templateName": name, "setupSqls": []string{}, "querySql": "EXPLAIN SELECT id, y FROM b ORDER BY id, y",
		}, &plan)).To(Succeed())
		fmt.Fprintf(GinkgoWriter, "WSJCARRY served=%v plan=%v\n", served["rows"], plan["rows"])
		Expect(fmt.Sprint(served["rows"])).To(Equal("[[1 7] [2 8]]"))
		Expect(fmt.Sprint(plan["rows"])).To(ContainSubstring("IB2"))
	})

	It("carries a long_value bitmap entry size as CHANGED, and the target plans the bitmap reads", func() {
		ctx := context.Background()
		java := NewJavaInvoker()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		name := "WSJCARRY5_" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")
		body := "create table t(id bigint, v bigint, primary key(id)) " +
			"create index agg_bucket as select bitmap_bucket_offset(id) from t order by bitmap_bucket_offset(id)"
		built, err := embedded.BuildSchemaTemplateFromDDLNamed(body, name)
		Expect(err).NotTo(HaveOccurred())
		longProto, err := built.Underlying().ToProto()
		Expect(err).NotTo(HaveOccurred())
		Expect(wsjWidenIntLiterals(longProto.ProtoReflect())).To(BeNumerically(">", 0))
		longBytes, err := proto.Marshal(longProto)
		Expect(err).NotTo(HaveOccurred())
		catalogMD, err := catalog.BuildCatalogMetaData()
		Expect(err).NotTo(HaveOccurred())
		db := recordlayer.NewFDBDatabase(sharedDB)
		_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetSubspace(catalog.DefaultCatalogSubspace()).SetMetaDataProvider(catalogMD).Open()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Templates{TEMPLATE_NAME: proto.String(name), TEMPLATE_VERSION: proto.Int32(1), META_DATA: longBytes})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			var dropped struct {
				Dropped bool `json:"dropped"`
			}
			_ = java.InvokeAs(context.Background(), "dropSchemaTemplatePersistentJava", map[string]any{"clusterFile": clusterFile, "templateName": name}, &dropped)
		}()
		v1, cat, _ := carriedFromJava(ctx, java, clusterFile, name)
		v2, err := metadata.NewRecordLayerSchemaTemplateWithVersion(name, built.Underlying(), 2)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			return nil, cat.SchemaTemplateCatalog().CreateTemplate(catalog.NewFDBTransaction(rtx), v2)
		})
		Expect(err).NotTo(HaveOccurred())
		var stored *gen.MetaData
		_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			var lerr error
			stored, lerr = cat.SchemaTemplateCatalog().LoadTemplateProto(catalog.NewFDBTransaction(rtx), name, 2)
			return nil, lerr
		})
		Expect(err).NotTo(HaveOccurred())
		_, i1, _ := byName(v1)
		_, i2, _ := byName(stored)
		Expect(i2["AGG_BUCKET"].GetAddedVersion()).To(Equal(i1["AGG_BUCKET"].GetAddedVersion()))
		Expect(i2["AGG_BUCKET"].GetSubspaceKey()).To(Equal(i1["AGG_BUCKET"].GetSubspaceKey()))
		Expect(i2["AGG_BUCKET"].GetLastModifiedVersion()).To(BeNumerically(">", v1.GetVersion()), "CHANGED: rebuilt when a store opens")
		Expect(wsjWidenIntLiterals(proto.Clone(i2["AGG_BUCKET"]).ProtoReflect())).To(BeNumerically(">", 0), "the carried root holds int_value")

		var served, plan map[string]any
		read := "SELECT bitmap_bucket_offset(id) FROM t ORDER BY bitmap_bucket_offset(id)"
		Expect(java.InvokeAs(ctx, "runOnExistingTemplateJava", map[string]any{
			"clusterFile": clusterFile, "templateName": name, "setupSqls": []string{"insert into t values (1, 1), (3, 1), (10005, 2)"}, "querySql": read,
		}, &served)).To(Succeed())
		Expect(java.InvokeAs(ctx, "runOnExistingTemplateJava", map[string]any{
			"clusterFile": clusterFile, "templateName": name, "setupSqls": []string{}, "querySql": "EXPLAIN " + read,
		}, &plan)).To(Succeed())
		fmt.Fprintf(GinkgoWriter, "WSJCARRY5 served=%v plan=%v\n", served["rows"], plan["rows"])
		Expect(fmt.Sprint(served["rows"])).To(Equal("[[0] [0] [10000]]"))
		Expect(fmt.Sprint(plan["rows"])).To(ContainSubstring("ISCAN(AGG_BUCKET <,>)"))
	})
})
