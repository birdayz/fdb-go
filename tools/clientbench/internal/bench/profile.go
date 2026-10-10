package bench

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
)

func StartProfile(mode, dir string) (func() error, error) {
	if mode == "none" {
		return func() error { return nil }, nil
	}
	name := mode + ".pprof"
	if mode == "trace" {
		name = "trace.out"
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	switch mode {
	case "cpu":
		if err := pprof.StartCPUProfile(f); err != nil {
			f.Close()
			return nil, err
		}
		return func() error { pprof.StopCPUProfile(); return f.Close() }, nil
	case "trace":
		if err := trace.Start(f); err != nil {
			f.Close()
			return nil, err
		}
		return func() error { trace.Stop(); return f.Close() }, nil
	case "heap":
		return func() error { runtime.GC(); return errors.Join(pprof.WriteHeapProfile(f), f.Close()) }, nil
	case "mutex":
		previous := runtime.SetMutexProfileFraction(1)
		return func() error {
			runtime.SetMutexProfileFraction(previous)
			return errors.Join(pprof.Lookup("mutex").WriteTo(f, 0), f.Close())
		}, nil
	case "block":
		runtime.SetBlockProfileRate(1)
		return func() error {
			runtime.SetBlockProfileRate(0)
			return errors.Join(pprof.Lookup("block").WriteTo(f, 0), f.Close())
		}, nil
	default:
		f.Close()
		return nil, fmt.Errorf("unknown profile %q", mode)
	}
}
