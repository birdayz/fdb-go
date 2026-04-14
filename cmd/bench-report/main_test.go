package main

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/gomega"
)

func TestParseBenchFile(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	content := `goos: linux
goarch: amd64
cpu: AMD Ryzen 9 3900X
BenchmarkSaveRecord-24    518    2176230 ns/op    3136 B/op    101 allocs/op
BenchmarkLoadRecord-24    2880    410904 ns/op    2906 B/op    91 allocs/op
PASS
`
	dir := t.TempDir()
	f := filepath.Join(dir, "bench.txt")
	os.WriteFile(f, []byte(content), 0o644)

	results, err := parseBenchFile(f)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(results).To(HaveLen(2))
	g.Expect(results["BenchmarkSaveRecord-24"].NsPerOp).To(Equal(2176230.0))
	g.Expect(results["BenchmarkSaveRecord-24"].BytesPerOp).To(Equal(3136.0))
	g.Expect(results["BenchmarkSaveRecord-24"].AllocsPerOp).To(Equal(101.0))
	g.Expect(results["BenchmarkSaveRecord-24"].Iterations).To(Equal(518))
	g.Expect(results["BenchmarkLoadRecord-24"].NsPerOp).To(Equal(410904.0))
}

func TestParseBenchFile_NoBytesAllocs(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	content := `BenchmarkSimple-4    1000    5000 ns/op
`
	dir := t.TempDir()
	f := filepath.Join(dir, "bench.txt")
	os.WriteFile(f, []byte(content), 0o644)

	results, err := parseBenchFile(f)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(results).To(HaveLen(1))
	g.Expect(results["BenchmarkSimple-4"].NsPerOp).To(Equal(5000.0))
	g.Expect(results["BenchmarkSimple-4"].BytesPerOp).To(Equal(0.0))
	g.Expect(results["BenchmarkSimple-4"].AllocsPerOp).To(Equal(0.0))
}

func TestCompare_Regression(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	old := map[string]*benchResult{
		"BenchmarkFoo-24": {Name: "BenchmarkFoo-24", NsPerOp: 1000, AllocsPerOp: 5},
	}
	new := map[string]*benchResult{
		"BenchmarkFoo-24": {Name: "BenchmarkFoo-24", NsPerOp: 1200, AllocsPerOp: 7}, // +20%, allocs changed
	}

	comps := compare(old, new)
	g.Expect(comps).To(HaveLen(1))
	g.Expect(comps[0].Status).To(Equal("slower"))
	g.Expect(comps[0].DeltaPct).To(BeNumerically("~", 20.0, 0.1))
}

func TestCompare_Improvement(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	old := map[string]*benchResult{
		"BenchmarkFoo-24": {Name: "BenchmarkFoo-24", NsPerOp: 1000, AllocsPerOp: 10},
	}
	new := map[string]*benchResult{
		"BenchmarkFoo-24": {Name: "BenchmarkFoo-24", NsPerOp: 800, AllocsPerOp: 8}, // -20%, allocs changed
	}

	comps := compare(old, new)
	g.Expect(comps).To(HaveLen(1))
	g.Expect(comps[0].Status).To(Equal("faster"))
	g.Expect(comps[0].DeltaPct).To(BeNumerically("~", -20.0, 0.1))
}

func TestCompare_Noise(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	old := map[string]*benchResult{
		"BenchmarkFoo-24": {Name: "BenchmarkFoo-24", NsPerOp: 1000, AllocsPerOp: 5},
	}
	new := map[string]*benchResult{
		"BenchmarkFoo-24": {Name: "BenchmarkFoo-24", NsPerOp: 1090, AllocsPerOp: 5}, // +9% < 10% threshold
	}

	comps := compare(old, new)
	g.Expect(comps).To(HaveLen(1))
	g.Expect(comps[0].Status).To(Equal("~"))
}

func TestCompare_TimingOnlyNoise(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	// Large timing delta but identical alloc count = VM noise, not regression
	old := map[string]*benchResult{
		"BenchmarkFoo-24": {Name: "BenchmarkFoo-24", NsPerOp: 1000, AllocsPerOp: 5, BytesPerOp: 100},
	}
	new := map[string]*benchResult{
		"BenchmarkFoo-24": {Name: "BenchmarkFoo-24", NsPerOp: 1500, AllocsPerOp: 5, BytesPerOp: 100}, // +50% but same allocs
	}

	comps := compare(old, new)
	g.Expect(comps).To(HaveLen(1))
	g.Expect(comps[0].Status).To(Equal("~"))
}

func TestCompare_BytesChangedAllocsUnchanged(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	// Bytes changed but allocs unchanged = encoding/buffer variance, not regression
	old := map[string]*benchResult{
		"BenchmarkFoo-24": {Name: "BenchmarkFoo-24", NsPerOp: 1000, AllocsPerOp: 5, BytesPerOp: 57345},
	}
	new := map[string]*benchResult{
		"BenchmarkFoo-24": {Name: "BenchmarkFoo-24", NsPerOp: 1200, AllocsPerOp: 5, BytesPerOp: 57346}, // +20%, bytes differ by 1
	}

	comps := compare(old, new)
	g.Expect(comps).To(HaveLen(1))
	g.Expect(comps[0].Status).To(Equal("~"))
}

func TestCompare_NewBenchmark(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	old := map[string]*benchResult{}
	new := map[string]*benchResult{
		"BenchmarkNew-24": {Name: "BenchmarkNew-24", NsPerOp: 500},
	}

	comps := compare(old, new)
	g.Expect(comps).To(HaveLen(1))
	g.Expect(comps[0].Status).To(Equal("new"))
}

func TestCompare_RemovedBenchmark(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	old := map[string]*benchResult{
		"BenchmarkOld-24": {Name: "BenchmarkOld-24", NsPerOp: 500},
	}
	new := map[string]*benchResult{}

	comps := compare(old, new)
	g.Expect(comps).To(HaveLen(1))
	g.Expect(comps[0].Status).To(Equal("removed"))
}

func TestFormatMarkdown_RegressionWarning(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	comps := []comparison{
		{
			Name: "BenchmarkFoo-24", OldNs: 1000, NewNs: 1200, DeltaPct: 20.0,
			OldAllocs: 5, NewAllocs: 7, Status: "slower",
		},
	}
	md := formatMarkdown(comps)
	g.Expect(md).To(ContainSubstring(marker))
	g.Expect(md).To(ContainSubstring("regressions detected"))
	g.Expect(md).To(ContainSubstring("**+20.0%**"))
}

func TestFormatMarkdown_NoRegression(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	comps := []comparison{
		{Name: "BenchmarkFoo-24", OldNs: 1000, NewNs: 1020, DeltaPct: 2.0, Status: "~"},
	}
	md := formatMarkdown(comps)
	g.Expect(md).To(ContainSubstring("No significant"))
	g.Expect(md).NotTo(ContainSubstring("regressions"))
	g.Expect(md).To(ContainSubstring("Threshold: +/-10%"))
}

func TestStripBenchPrefix(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	g.Expect(stripBenchPrefix("BenchmarkSaveRecord-24")).To(Equal("SaveRecord"))
	g.Expect(stripBenchPrefix("BenchmarkFoo")).To(Equal("Foo"))
	g.Expect(stripBenchPrefix("BenchmarkBar-abc")).To(Equal("Bar-abc")) // non-numeric suffix kept
}

func TestFmtNs(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	g.Expect(fmtNs(500)).To(Equal("500.0ns"))
	g.Expect(fmtNs(5000)).To(Equal("5.00us"))
	g.Expect(fmtNs(5000000)).To(Equal("5.00ms"))
	g.Expect(fmtNs(5000000000)).To(Equal("5.00s"))
}
