package conformance_test

import (
	"bytes"
	"os"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

// The Java server's launcher must point FDB Java at the pinned libfdb_c in its
// runfiles; the probes check the running JVM actually maps it.
func TestConformanceServerPassesPinnedLibfdbC(t *testing.T) {
	t.Parallel()
	r, err := runfiles.New()
	if err != nil {
		t.Fatal(err)
	}
	launcher, err := r.Rlocation("_main/conformance/conformance_server")
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(launcher)
	if err != nil {
		t.Fatal(err)
	}
	const flag = "-DFDB_LIBRARY_PATH_FDB_C=${JAVA_RUNFILES}/"
	i := bytes.Index(script, []byte(flag))
	if i < 0 {
		t.Fatalf("launcher %s does not pass %s", launcher, flag)
	}
	rest := script[i+len(flag):]
	end := bytes.IndexAny(rest, "' \"\n")
	if end <= 0 {
		t.Fatalf("launcher %s: no runfiles path after %s", launcher, flag)
	}
	lib, err := r.Rlocation(string(rest[:end]))
	if err != nil {
		t.Fatalf("runfiles %s: %v", rest[:end], err)
	}
	if fi, err := os.Stat(lib); err != nil || fi.Size() == 0 {
		t.Fatalf("pinned libfdb_c %s missing or empty: %v", lib, err)
	}
}
