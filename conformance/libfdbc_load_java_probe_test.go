package conformance_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// FDB Java swallows a failed load of the configured libfdb_c and falls back to
// the host's, so only the running JVM's mappings prove the pin took effect.
var _ = Describe("LibfdbCLoadJavaProbe", func() {
	It("maps the pinned libfdb_c and no other", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		java, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = java.Close() }()
		var trace map[string]any
		Expect(java.InvokeAs(ctx, "planRuleTrace", map[string]any{
			"clusterFile": clusterFile, "schemaTemplate": "CREATE TABLE T (id BIGINT, PRIMARY KEY (id))",
			"setupSqls": []string{}, "querySql": "SELECT id FROM T", "rules": []string{},
		}, &trace)).To(Succeed(), "one FDB request loads FDB Java's natives")

		r, err := runfiles.New()
		Expect(err).NotTo(HaveOccurred())
		pinned, err := r.Rlocation("libfdb_c_linux_x86_64/file/libfdb_c.so")
		Expect(err).NotTo(HaveOccurred())
		want, err := filepath.EvalSymlinks(pinned)
		Expect(err).NotTo(HaveOccurred())

		// The launcher may fork the JVM, so read every process in its group.
		group := java.serverCmd.Process.Pid
		mapped := map[string]bool{}
		procs, err := os.ReadDir("/proc")
		Expect(err).NotTo(HaveOccurred())
		for _, p := range procs {
			pid, err := strconv.Atoi(p.Name())
			if err != nil {
				continue
			}
			stat, err := os.ReadFile(filepath.Join("/proc", p.Name(), "stat"))
			if err != nil {
				continue
			}
			fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
			if len(fields) < 3 || fields[2] != strconv.Itoa(group) {
				continue
			}
			maps, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "maps"))
			Expect(err).NotTo(HaveOccurred())
			for _, line := range strings.Split(string(maps), "\n") {
				if i := strings.Index(line, "/"); i >= 0 && strings.HasSuffix(line, "/libfdb_c.so") {
					mapped[line[i:]] = true
				}
			}
		}
		Expect(mapped).To(Equal(map[string]bool{want: true}),
			"the JVM must map exactly the pinned libfdb_c (resolved from runfiles %s), no host copy", pinned)
	})
})
