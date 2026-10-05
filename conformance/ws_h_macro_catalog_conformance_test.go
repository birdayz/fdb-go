//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/embedded"
)

// SQL functions are stored in the schema template's metadata
// (MetaData.user_defined_functions) with their parameter names and DEFAULT
// Values. The definitions Go builds from DDL are byte-identical to the ones
// the target stores for the same DDL, and the target calls the functions of a
// template Go stored as it calls its own. Go stores through its catalog
// library: its SQL driver's catalog is on a Go-only keyspace (TODO.md, "Go SQL
// driver stores the relational catalog and user schemas on a Go-only keyspace").
var _ = Describe("WSHMacroCatalogConformance", func() {
	It("stores and reads SQL function definitions as the target does", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		java := NewJavaInvoker()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		db := recordlayer.NewFDBDatabase(sharedDB)
		const body = `CREATE TYPE AS STRUCT st1(y BIGINT, z BIGINT) ` +
			`CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id)) ` +
			`CREATE FUNCTION st1_d(IN y BIGINT, IN z BIGINT DEFAULT 2L) RETURNS st1 RETURN (y, z) ` +
			`CREATE FUNCTION add2(IN a BIGINT, IN b BIGINT DEFAULT 1 + 1) RETURNS BIGINT RETURN a + b ` +
			`CREATE FUNCTION zero() RETURNS BIGINT RETURN 0L ` +
			`CREATE FUNCTION st1_z(IN s TYPE st1) RETURNS BIGINT RETURN s.z ` +
			`CREATE FUNCTION tf(IN lo BIGINT, IN hi BIGINT DEFAULT 10) AS SELECT id, a FROM t WHERE id BETWEEN lo AND hi`
		suffix := strings.ToUpper(strings.ReplaceAll(uuid.New().String()[:8], "-", ""))
		javaName, goName := "WSH_MACRO_J_"+suffix, "WSH_MACRO_G_"+suffix
		drop := func(name string) {
			var dropped struct {
				Dropped bool `json:"dropped"`
			}
			_ = java.InvokeAs(context.Background(), "dropSchemaTemplatePersistentJava", map[string]any{
				"clusterFile": clusterFile, "templateName": name,
			}, &dropped)
		}

		var created struct {
			Created bool `json:"created"`
		}
		Expect(java.InvokeAs(ctx, "createSchemaTemplatePersistentJava", map[string]any{
			"clusterFile": clusterFile, "templateName": javaName, "schemaTemplateBody": body,
		}, &created)).To(Succeed())
		Expect(created.Created).To(BeTrue())
		defer drop(javaName)
		javaFns := loadStoredJavaTemplateMetaData(ctx, db, javaName).GetUserDefinedFunctions()

		goTmpl, err := embedded.BuildSchemaTemplateFromDDLNamed(body, goName)
		Expect(err).NotTo(HaveOccurred())
		goProto, err := goTmpl.Underlying().ToProto()
		Expect(err).NotTo(HaveOccurred())
		goFns := goProto.GetUserDefinedFunctions()
		Expect(javaFns).To(HaveLen(5))
		Expect(goFns).To(HaveLen(len(javaFns)))
		byName := func(fns []*gen.PUserDefinedFunction) map[string]string {
			out := map[string]string{}
			for _, f := range fns {
				b, err := proto.MarshalOptions{Deterministic: true}.Marshal(f)
				Expect(err).NotTo(HaveOccurred())
				name := f.GetUserDefinedMacroFunction().GetFunctionName()
				if name == "" {
					name = f.GetSqlFunction().GetName()
				}
				out[name] = wshCanonicalAliases(string(b))
			}
			return out
		}
		javaBytes, goBytes := byName(javaFns), byName(goFns)
		var defDiffs []string
		for _, name := range sortedStringKeys(javaBytes) {
			jb := javaBytes[name]
			fmt.Fprintf(GinkgoWriter, "WSH-MACRO-DEF %s\n  java=%x\n  go=  %x\n", name, jb, goBytes[name])
			if goBytes[name] != jb {
				defDiffs = append(defDiffs, name)
				for side, b := range map[string]string{"java": jb, "go": goBytes[name]} {
					var f gen.PUserDefinedFunction
					Expect(proto.Unmarshal([]byte(b), &f)).To(Succeed())
					fmt.Fprintf(GinkgoWriter, "WSH-MACRO-DIFF %s %s\n%s\n", name, side, prototext.Format(&f))
				}
			}
		}
		Expect(defDiffs).To(BeEmpty(), "stored definitions differ")

		cat, err := catalog.OpenRecordLayerStoreCatalog()
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			return nil, cat.SchemaTemplateCatalog().CreateTemplate(catalog.NewFDBTransaction(rtx), goTmpl)
		})
		Expect(err).NotTo(HaveOccurred())
		defer drop(goName)
		run := func(template, q string) string {
			var out map[string]any
			if err := java.InvokeAs(ctx, "runOnExistingTemplateJava", map[string]any{
				"clusterFile": clusterFile, "templateName": template,
				"setupSqls": []string{`INSERT INTO t VALUES (1, 10), (2, 20), (12, 120)`}, "querySql": q,
			}, &out); err != nil {
				return "ERROR " + err.Error()
			}
			return fmt.Sprint(out["rows"])
		}
		var failures []string
		for _, q := range []string{
			`SELECT st1_d(4) FROM t WHERE id = 1`,
			`SELECT st1_d(4, 5) FROM t WHERE id = 1`,
			`SELECT st1_d(y => 4) FROM t WHERE id = 1`,
			`SELECT add2(a) FROM t WHERE id = 1`,
			`SELECT add2(a => a, b => 5) FROM t WHERE id = 1`,
			`SELECT zero() FROM t WHERE id = 1`,
			`SELECT st1_z(st1_d(7)) FROM t WHERE id = 1`,
			`SELECT * FROM tf(1) ORDER BY id`,
			`SELECT * FROM tf(lo => 2, hi => 20) ORDER BY id`,
		} {
			own, overGo := run(javaName, q), run(goName, q)
			fmt.Fprintf(GinkgoWriter, "WSH-MACRO %s\n  own=%s\n  over-go=%s\n", q, own, overGo)
			if strings.HasPrefix(own, "ERROR") || own != overGo {
				failures = append(failures, fmt.Sprintf("%s: over its own template %s, over Go's %s", q, own, overGo))
			}
		}
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})
})

var wshAliasPattern = regexp.MustCompile(`c[0-9a-f]{8}_[0-9a-f]{4}_[0-9a-f]{4}_[0-9a-f]{4}_[0-9a-f]{12}`)

// wshCanonicalAliases renames the generated parameter correlations, unique per
// DDL run in either engine, by order of first appearance (same length, so the
// enclosing length prefixes are unchanged).
func wshCanonicalAliases(b string) string {
	seen := map[string]string{}
	return wshAliasPattern.ReplaceAllStringFunc(b, func(id string) string {
		if c, ok := seen[id]; ok {
			return c
		}
		c := fmt.Sprintf("c%036d", len(seen))
		seen[id] = c
		return c
	})
}
