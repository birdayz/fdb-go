//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"
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

// Boolean macro bodies (comparisons, AND/OR/NOT, IS, IN, BETWEEN, LIKE,
// searched CASE) are stored byte-identical to the target's definitions for
// the same DDL, and the target calls a template Go stored as it calls its
// own. EXISTS is the exception: the target stores its ExistsValue without
// the subquery and cannot call it, so Go refuses the definition.
var _ = Describe("WSHBooleanMacroConformance", func() {
	It("stores boolean macro bodies as the target does", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		java := NewJavaInvoker()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		db := recordlayer.NewFDBDatabase(sharedDB)
		cat, err := catalog.OpenRecordLayerStoreCatalog()
		Expect(err).NotTo(HaveOccurred())
		var failures []string
		const prefix = `CREATE TYPE AS ENUM mood ('HAPPY', 'SAD') CREATE TYPE AS STRUCT st(m mood) ` +
			`CREATE TABLE t (id BIGINT, a BIGINT, s STRING, b BOOLEAN, p st, PRIMARY KEY (id)) `
		for i, tc := range []struct{ fn, call string }{
			{`CREATE FUNCTION f(IN a BIGINT) RETURNS BOOLEAN RETURN a > 5`, `f(a)`},
			{`CREATE FUNCTION f(IN a BIGINT) RETURNS BOOLEAN RETURN a IN (1, 2, 3)`, `f(a)`},
			{`CREATE FUNCTION f(IN a BIGINT) RETURNS BOOLEAN RETURN a NOT IN (1, 2)`, `f(a)`},
			{`CREATE FUNCTION f(IN a BIGINT, IN b BIGINT) RETURNS BOOLEAN RETURN a IN (b, 2)`, `f(a, id)`},
			{`CREATE FUNCTION f(IN a BIGINT) RETURNS BOOLEAN RETURN a BETWEEN 1 AND 5`, `f(a)`},
			{`CREATE FUNCTION f(IN a BIGINT) RETURNS BOOLEAN RETURN a NOT BETWEEN 1 AND 5`, `f(a)`},
			{`CREATE FUNCTION f(IN a BIGINT) RETURNS STRING RETURN CASE WHEN a > 1 THEN 'x' ELSE 'y' END`, `f(a)`},
			{`CREATE FUNCTION f(IN a BIGINT) RETURNS BIGINT RETURN CASE WHEN a > 1 THEN 1 WHEN a > 0 THEN 2 END`, `f(a)`},
			{`CREATE FUNCTION f(IN s STRING) RETURNS BOOLEAN RETURN s LIKE 'a%'`, `f(s)`},
			{`CREATE FUNCTION f(IN s STRING) RETURNS BOOLEAN RETURN s NOT LIKE 'a!%%' ESCAPE '!'`, `f(s)`},
			{`CREATE FUNCTION f(IN b BOOLEAN) RETURNS BOOLEAN RETURN b IS TRUE`, `f(b)`},
			{`CREATE FUNCTION f(IN b BOOLEAN) RETURNS BOOLEAN RETURN b IS NOT FALSE`, `f(b)`},
			{`CREATE FUNCTION f(IN a BIGINT, IN s STRING) RETURNS BOOLEAN RETURN a >= 2 AND NOT (s = 'q') OR s IS NULL`, `f(a, s)`},
			{`CREATE FUNCTION f(IN x INTEGER, IN y BIGINT) RETURNS BOOLEAN RETURN x > y`, `f(1, a)`},
			{`CREATE FUNCTION f(IN x TYPE st) RETURNS BOOLEAN RETURN x.m = 'HAPPY'`, `f(p)`},
			{`CREATE FUNCTION f(IN a BIGINT) RETURNS BOOLEAN RETURN a > 2.5`, `f(a)`},
			{`CREATE FUNCTION f(IN a BIGINT) RETURNS BOOLEAN RETURN a IS DISTINCT FROM 3`, `f(a)`},
			{`CREATE FUNCTION f(IN a INTEGER) RETURNS BOOLEAN RETURN a IN (1, 2)`, `f(1)`},
			{`CREATE FUNCTION f(IN a BOOLEAN, IN b BOOLEAN) RETURNS BOOLEAN RETURN a AND b`, `f(b, b)`},
			{`CREATE FUNCTION f(IN a BIGINT, IN b BOOLEAN) RETURNS BIGINT RETURN CASE WHEN b THEN a ELSE 5 END`, `f(a, b)`},
			{`CREATE FUNCTION f(IN a BIGINT) RETURNS DOUBLE RETURN CASE WHEN a < 3 THEN 1 ELSE 2.5 END`, `f(a)`},
			{`CREATE FUNCTION f(IN a BIGINT) RETURNS BOOLEAN RETURN (a > 1) = (a < 9)`, `f(a)`},
			{`CREATE FUNCTION f(IN x INTEGER, IN b BOOLEAN) RETURNS INTEGER RETURN CASE WHEN b THEN x ELSE 5 END`, `f(NULL, b)`},
			{`CREATE FUNCTION f(IN b BOOLEAN) RETURNS BOOLEAN RETURN CASE WHEN b THEN b ELSE TRUE END`, `f(b)`},
			{`CREATE FUNCTION f(IN a BIGINT) RETURNS BOOLEAN RETURN NOT a IN (1, 2) OR a IS NOT NULL AND a <> 3`, `f(a)`},
		} {
			fn := tc.fn
			body := prefix + fn
			suffix := strings.ToUpper(strings.ReplaceAll(uuid.New().String()[:8], "-", ""))
			javaName, goName := fmt.Sprintf("WSH_BOOL_J%d_%s", i, suffix), fmt.Sprintf("WSH_BOOL_G%d_%s", i, suffix)
			var created struct {
				Created bool `json:"created"`
			}
			Expect(java.InvokeAs(ctx, "createSchemaTemplatePersistentJava", map[string]any{
				"clusterFile": clusterFile, "templateName": javaName, "schemaTemplateBody": body,
			}, &created)).To(Succeed(), fn)
			javaBody := probeFn(loadStoredJavaTemplateMetaData(ctx, db, javaName).GetUserDefinedFunctions())
			goTmpl, err := embedded.BuildSchemaTemplateFromDDLNamed(body, goName)
			Expect(err).NotTo(HaveOccurred(), fn)
			goProto, err := goTmpl.Underlying().ToProto()
			Expect(err).NotTo(HaveOccurred())
			goBody := probeFn(goProto.GetUserDefinedFunctions())
			fmt.Fprintf(GinkgoWriter, "WSH-BOOL %s\n  java=%s\n  go=  %s\n", fn, javaBody, goBody)
			if goBody != javaBody {
				failures = append(failures, "stored body differs: "+fn)
			}
			_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				return nil, cat.SchemaTemplateCatalog().CreateTemplate(catalog.NewFDBTransaction(rtx), goTmpl)
			})
			Expect(err).NotTo(HaveOccurred())
			call := "SELECT id, " + tc.call + " FROM t ORDER BY id"
			own, overGo := wshBoolRun(ctx, java, clusterFile, javaName, call), wshBoolRun(ctx, java, clusterFile, goName, call)
			fmt.Fprintf(GinkgoWriter, "  %s\n  own=%s\n  over-go=%s\n", call, own, overGo)
			if strings.HasPrefix(own, "ERROR") || own != overGo {
				failures = append(failures, fmt.Sprintf("%s: over its own template %s, over Go's %s", call, own, overGo))
			}
			wshBoolDrop(clusterFile, java, javaName)
			wshBoolDrop(clusterFile, java, goName)
		}

		const exists = `CREATE FUNCTION f(IN a BIGINT) RETURNS BOOLEAN RETURN EXISTS (SELECT * FROM t WHERE id = a)`
		name := "WSH_BOOL_EXISTS_" + strings.ToUpper(strings.ReplaceAll(uuid.New().String()[:8], "-", ""))
		var created struct {
			Created bool `json:"created"`
		}
		Expect(java.InvokeAs(ctx, "createSchemaTemplatePersistentJava", map[string]any{
			"clusterFile": clusterFile, "templateName": name, "schemaTemplateBody": prefix + exists,
		}, &created)).To(Succeed())
		defer wshBoolDrop(clusterFile, java, name)
		Expect(wshBoolRun(ctx, java, clusterFile, name, "SELECT id, f(a) FROM t")).To(ContainSubstring("Missing binding"),
			"the target stores EXISTS without its subquery and cannot call it")
		_, err = embedded.BuildSchemaTemplateFromDDLNamed(prefix+exists, name+"_G")
		Expect(err).To(MatchError(ContainSubstring("EXISTS cannot be persisted in a function body")))
		Expect(failures).To(BeEmpty(), strings.Join(failures, "\n"))
	})
})

func probeFn(fns []*gen.PUserDefinedFunction) string {
	if len(fns) != 1 {
		return fmt.Sprintf("%d functions", len(fns))
	}
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(fns[0])
	Expect(err).NotTo(HaveOccurred())
	var f gen.PUserDefinedFunction
	Expect(proto.Unmarshal([]byte(wshCanonicalAliases(string(b))), &f)).To(Succeed())
	return strings.Join(strings.Fields(prototext.Format(f.GetUserDefinedMacroFunction().GetBody())), " ")
}

func wshBoolRun(ctx context.Context, java *JavaInvoker, clusterFile, template, q string) string {
	var out map[string]any
	if err := java.InvokeAs(ctx, "runOnExistingTemplateJava", map[string]any{
		"clusterFile": clusterFile, "templateName": template,
		"setupSqls": []string{`INSERT INTO t VALUES (1, 1, 'abc', TRUE, ('HAPPY')), (2, 7, 'a%x', FALSE, ('SAD')), (3, NULL, NULL, NULL, NULL)`},
		"querySql":  q,
	}, &out); err != nil {
		return "ERROR " + err.Error()
	}
	return fmt.Sprint(out["rows"])
}

func wshBoolDrop(clusterFile string, java *JavaInvoker, name string) {
	var dropped struct {
		Dropped bool `json:"dropped"`
	}
	_ = java.InvokeAs(context.Background(), "dropSchemaTemplatePersistentJava", map[string]any{
		"clusterFile": clusterFile, "templateName": name,
	}, &dropped)
}
