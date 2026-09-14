//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"
	"os"
	"time"

	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ModFunctionJavaProbe", func() {
	It("distinguishes the Go function extension from Java's remainder operator", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "modfn_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		clusterFile := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFile)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFile)
		const schema = "CREATE TABLE t (id BIGINT, d DOUBLE, PRIMARY KEY (id))"
		setup := []string{"INSERT INTO t VALUES (1, 7.0)"}
		for _, expr := range []string{"d % 0.0", "MOD(d, 0.0)"} {
			query := "SELECT CAST(" + expr + " AS STRING) FROM t"
			javaResult := javaRunner.RunWithSetup(ctx, schema, setup, query)
			goResult := goRunner.RunWithSetup(ctx, schema, setup, query)
			Expect(goResult.Err).NotTo(HaveOccurred(), query)
			Expect(goResult.Rows.Rows).To(Equal([][]any{{"NaN"}}), query)
			if expr == "MOD(d, 0.0)" {
				Expect(javaResult.Err).To(HaveOccurred(), "Java's function admission changed: re-evaluate the extension boundary")
				Expect(javaResult.Err.Error()).To(ContainSubstring("Unsupported operator MOD"))
			} else {
				Expect(javaResult.Err).NotTo(HaveOccurred(), query)
				Expect(javaResult.Rows.Rows).To(Equal([][]any{{"NaN"}}), query)
			}
			fmt.Fprintf(GinkgoWriter, "MOD-FUNCTION-BOUNDARY %s: Go=%v; Java rows=%v err=%v\n", expr, goResult.Rows, javaResult.Rows, javaResult.Err)
		}
	})
})
