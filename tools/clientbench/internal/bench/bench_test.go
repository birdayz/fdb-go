package bench

import (
	"bytes"
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPreparedDataset(t *testing.T) {
	t.Parallel()
	d := Prepare("clientbench/test/", 4, 9, 17)
	if len(d.Range) != 100 || len(d.Writes) != 4 || len(d.Warmup) != 9 || len(d.Measure) != 17 {
		t.Fatal("wrong dataset population")
	}
	for _, size := range []int{100, 1000, 10000} {
		if len(d.Points[size].Value) != size {
			t.Fatalf("point size %d", size)
		}
	}
	for i, row := range d.Range {
		if len(row.Value) != 8 {
			t.Fatalf("range %d size", i)
		}
		if i > 0 && bytes.Compare(d.Range[i-1].Key, row.Key) >= 0 {
			t.Fatal("range keys not strictly ordered")
		}
	}
	if bytes.Compare(d.Range[99].Key, d.RangeEnd) >= 0 {
		t.Fatal("range end excludes data")
	}
	for i, value := range d.Measure {
		if len(value) != 100 || bytes.Equal(value, d.Initial) {
			t.Fatal("write is not a changing 100-byte overwrite")
		}
		if i > 0 && bytes.Equal(value, d.Measure[i-1]) {
			t.Fatal("duplicate write payload")
		}
		if i < len(d.Warmup) && bytes.Equal(value, d.Warmup[i]) {
			t.Fatal("warmup equals measured payload")
		}
	}
	for i, key := range d.Writes {
		for j, other := range d.Writes {
			if i != j && bytes.Equal(key, other) {
				t.Fatal("workers share a write key")
			}
		}
	}
}

func TestExactChecksRejectCorruption(t *testing.T) {
	t.Parallel()
	d := Prepare("clientbench/test/", 1, 0, 1)
	if err := CheckValue(d.Initial, append([]byte(nil), d.Initial...)); err != nil {
		t.Fatal(err)
	}
	if err := CheckValue(d.Initial, make([]byte, 100)); err == nil {
		t.Fatal("length-only point check")
	}
	if err := CheckValue(nil, d.Initial); err == nil {
		t.Fatal("missing value accepted")
	}
	if err := CheckRange(d.Range, d.Range); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"empty", "short", "extra", "key", "value", "order"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			rows := append([]KV(nil), d.Range...)
			switch kind {
			case "empty":
				rows = nil
			case "short":
				rows = rows[:99]
			case "extra":
				rows = append(rows, rows[99])
			case "key":
				rows[0].Key = []byte("wrong")
			case "value":
				rows[0].Value = []byte("badvalue")
			case "order":
				rows[0], rows[1] = rows[1], rows[0]
			}
			if CheckRange(rows, d.Range) == nil {
				t.Fatal("corruption accepted")
			}
		})
	}
}

func TestPhaseRoundRobinAndRetries(t *testing.T) {
	t.Parallel()
	p := RunPhase(17, 4, func(i, w int) (int, error) {
		if w != i%4 {
			t.Errorf("worker %d for index %d", w, i)
		}
		return 2, nil
	})
	if p.Completed != 17 || p.Errors != 0 || p.Retries != 17 || p.WallNS <= 0 {
		t.Fatalf("bad phase: %+v", p)
	}
	for i, s := range p.Samples {
		if !s.Attempted || s.Index != i || s.Worker != i%4 || s.LatencyNS <= 0 || s.StartOffsetNS < 0 {
			t.Fatalf("bad sample: %+v", s)
		}
	}
}

func TestFailureIsRecordedWithoutPadding(t *testing.T) {
	t.Parallel()
	failure := errors.New("terminal failure")
	p := RunPhase(10, 1, func(i, w int) (int, error) {
		if i == 2 {
			return 3, failure
		}
		return 1, nil
	})
	if p.Completed != 3 || p.Errors != 1 || p.Retries != 2 || !errors.Is(p.Samples[2].Err, failure) {
		t.Fatalf("bad failed phase: %+v", p)
	}
	path := filepath.Join(t.TempDir(), "samples.csv")
	if err := WriteSamples(path, "measure", p.Samples); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 || rows[3][6] != "terminal failure" || rows[3][7] != "error" {
		t.Fatalf("failed CSV: %v", rows)
	}
	if err := WriteSamples(path, "measure", p.Samples); err == nil {
		t.Fatal("overwrote samples")
	}
}

func TestConfigGuards(t *testing.T) {
	t.Parallel()
	base := Config{ClusterFile: "cluster", Output: "output", AllowWrites: true, Prefix: "clientbench/v1/", Iterations: 1, Warmup: 0, Workers: 1, TimeoutMS: 30000, RetryLimit: 10, Workload: "get100", Profile: "none"}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Config){func(c *Config) { c.AllowWrites = false }, func(c *Config) { c.Prefix = "" }, func(c *Config) { c.Iterations = 0 }, func(c *Config) { c.Warmup = -1 }, func(c *Config) { c.Workers = 0 }, func(c *Config) { c.TimeoutMS = 0 }, func(c *Config) { c.RetryLimit = -1 }, func(c *Config) { c.Workload = "wrong" }, func(c *Config) { c.Profile = "wrong" }} {
		cfg := base
		mutate(&cfg)
		if cfg.Validate() == nil {
			t.Fatal("invalid config accepted")
		}
	}
}
