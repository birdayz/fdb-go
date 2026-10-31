//go:build !linux

package recordlayer

import "time"

// -1 in the diagnostic output means that process CPU time was unavailable.
func largeContextCPUTime() time.Duration { return -1 }
