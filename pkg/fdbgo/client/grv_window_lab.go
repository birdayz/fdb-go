package client

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"
)

// Lab switches (docs/labs/single-network-goroutine), off by default:
// FDBGO_EXP_GRV_WINDOW_SCALE=s multiplies the GRV batch window (half the
// smoothed GRV latency) by s; 0 sends every GRV request at once.
// FDBGO_EXP_GRV_WINDOW_LOG=1 prints the window and latency samples once a second.
var (
	grvWindowScale = func() float64 {
		if v, err := strconv.ParseFloat(os.Getenv("FDBGO_EXP_GRV_WINDOW_SCALE"), 64); err == nil && v >= 0 {
			return v
		}
		return 1
	}()
	grvWindowLog = os.Getenv("FDBGO_EXP_GRV_WINDOW_LOG") == "1"
)

type grvWindowLabState struct {
	mu         sync.Mutex
	last       time.Time
	n          int
	sumElapsed time.Duration
	sumWindow  time.Duration
}

var grvLab grvWindowLabState

func (b *grvBatcher) labBatchTime() time.Duration {
	if grvWindowScale == 1 {
		return b.batchTime
	}
	return time.Duration(float64(b.batchTime) * grvWindowScale)
}

// labObserveLocked runs under b.mu.
func (b *grvBatcher) labObserveLocked(elapsed time.Duration) {
	if !grvWindowLog {
		return
	}
	s := &grvLab
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	s.sumElapsed += elapsed
	s.sumWindow += b.labBatchTime()
	if now := time.Now(); now.Sub(s.last) >= time.Second {
		if s.n > 0 {
			fmt.Fprintf(os.Stderr, "grvwindow n=%d mean_latency_us=%.1f mean_window_us=%.1f\n",
				s.n, float64(s.sumElapsed)/float64(s.n)/1e3, float64(s.sumWindow)/float64(s.n)/1e3)
		}
		s.last, s.n, s.sumElapsed, s.sumWindow = now, 0, 0, 0
	}
}
