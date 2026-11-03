package collector

import (
	"runtime"
	"runtime/metrics"
	"syscall"
)

// CollectorBenchCPU is this process's CPU over an interval: getrusage
// (RUSAGE_SELF, every thread; FDB servers are other processes) user and
// system time, and the runtime/metrics GC CPU estimates. The GC figures are
// the runtime's own estimates, updated as GC work completes, and cover only
// part of the rusage total. The interval's CPU includes whatever else runs
// in the process (producers, the collector, under-load query workers).
type CollectorBenchCPU struct {
	UserNs int64 `json:"user_ns"`
	SysNs  int64 `json:"sys_ns"`
	// TotalNs is UserNs + SysNs.
	TotalNs int64 `json:"total_ns"`
	// GCNs is /cpu/classes/gc/total; the mark and pause classes are its parts.
	GCNs          int64 `json:"gc_ns"`
	GCAssistNs    int64 `json:"gc_mark_assist_ns"`
	GCDedicatedNs int64 `json:"gc_mark_dedicated_ns"`
	GCIdleNs      int64 `json:"gc_mark_idle_ns"`
	GCPauseNs     int64 `json:"gc_pause_ns"`
	// CoresUsed is TotalNs per wall nanosecond; Utilization is CoresUsed
	// over the cores the process can run on at once, min(GOMAXPROCS, NumCPU)
	// (whichever is known when the other is zero).
	CoresUsed   float64 `json:"cores_used"`
	Utilization float64 `json:"utilization"`
	// MsPerSpan is TotalNs in milliseconds per exported span.
	MsPerSpan float64 `json:"ms_per_span"`
	// GCFraction is GCNs over TotalNs. GCNs is the runtime's per-P estimate
	// (time GC work held a P, not CPU it was granted), so with GOMAXPROCS >
	// NumCPU, where Ps share CPUs, it overstates GC CPU, and GCFraction with
	// it; a CPU profile gives the actual share.
	GCFraction float64 `json:"gc_fraction"`
	GOMAXPROCS int     `json:"gomaxprocs"`
	NumCPU     int     `json:"num_cpu"`
}

// benchCPUSample is cumulative process CPU at one instant, in nanoseconds.
type benchCPUSample struct {
	user, sys                          int64
	gc, assist, dedicated, idle, pause int64
}

var benchCPUMetrics = [...]string{
	"/cpu/classes/gc/total:cpu-seconds",
	"/cpu/classes/gc/mark/assist:cpu-seconds",
	"/cpu/classes/gc/mark/dedicated:cpu-seconds",
	"/cpu/classes/gc/mark/idle:cpu-seconds",
	"/cpu/classes/gc/pause:cpu-seconds",
}

// readBenchCPU samples the process's cumulative CPU. A figure the platform
// does not provide reads as zero.
func readBenchCPU() benchCPUSample {
	var s benchCPUSample
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) == nil {
		s.user, s.sys = ru.Utime.Nano(), ru.Stime.Nano()
	}
	var samples [len(benchCPUMetrics)]metrics.Sample
	for i, name := range benchCPUMetrics {
		samples[i].Name = name
	}
	metrics.Read(samples[:])
	ns := func(i int) int64 {
		if samples[i].Value.Kind() != metrics.KindFloat64 {
			return 0
		}
		return int64(samples[i].Value.Float64() * 1e9)
	}
	s.gc, s.assist, s.dedicated, s.idle, s.pause = ns(0), ns(1), ns(2), ns(3), ns(4)
	return s
}

// benchCPUDelta is the CPU between two samples over wallNs, with spans
// exported in it.
func benchCPUDelta(prev, cur benchCPUSample, wallNs, spans int64, gomaxprocs, numCPU int) CollectorBenchCPU {
	c := CollectorBenchCPU{
		UserNs: cur.user - prev.user, SysNs: cur.sys - prev.sys,
		GCNs: cur.gc - prev.gc, GCAssistNs: cur.assist - prev.assist, GCDedicatedNs: cur.dedicated - prev.dedicated,
		GCIdleNs: cur.idle - prev.idle, GCPauseNs: cur.pause - prev.pause,
		GOMAXPROCS: gomaxprocs, NumCPU: numCPU,
	}
	c.derive(wallNs, spans)
	return c
}

// add sums o's counters into c and keeps the largest GOMAXPROCS and NumCPU;
// derive recomputes the ratios.
func (c *CollectorBenchCPU) add(o CollectorBenchCPU) {
	c.UserNs += o.UserNs
	c.SysNs += o.SysNs
	c.GCNs += o.GCNs
	c.GCAssistNs += o.GCAssistNs
	c.GCDedicatedNs += o.GCDedicatedNs
	c.GCIdleNs += o.GCIdleNs
	c.GCPauseNs += o.GCPauseNs
	c.GOMAXPROCS = max(c.GOMAXPROCS, o.GOMAXPROCS)
	c.NumCPU = max(c.NumCPU, o.NumCPU)
}

// derive fills TotalNs and the ratios from the counters; a ratio with a zero
// denominator is zero.
func (c *CollectorBenchCPU) derive(wallNs, spans int64) {
	c.TotalNs = c.UserNs + c.SysNs
	c.CoresUsed, c.Utilization, c.MsPerSpan, c.GCFraction = 0, 0, 0, 0
	if wallNs > 0 {
		c.CoresUsed = float64(c.TotalNs) / float64(wallNs)
	}
	if procs := benchCPUProcs(c.GOMAXPROCS, c.NumCPU); procs > 0 {
		c.Utilization = c.CoresUsed / float64(procs)
	}
	if spans > 0 {
		c.MsPerSpan = float64(c.TotalNs) / 1e6 / float64(spans)
	}
	if c.TotalNs > 0 {
		c.GCFraction = float64(c.GCNs) / float64(c.TotalNs)
	}
}

// benchCPUProcs is min(gomaxprocs, numCPU) over the positive ones, or zero.
func benchCPUProcs(gomaxprocs, numCPU int) int {
	switch {
	case gomaxprocs <= 0:
		return max(numCPU, 0)
	case numCPU <= 0:
		return gomaxprocs
	}
	return min(gomaxprocs, numCPU)
}

// benchCPUWindow samples the CPU now and returns its delta since prev.
func benchCPUWindow(prev benchCPUSample, wallNs, spans int64) (CollectorBenchCPU, benchCPUSample) {
	cur := readBenchCPU()
	return benchCPUDelta(prev, cur, wallNs, spans, runtime.GOMAXPROCS(0), runtime.NumCPU()), cur
}
