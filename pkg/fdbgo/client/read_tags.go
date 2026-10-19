package client

import "math/rand/v2"

// readTagSampleRate is CLIENT_KNOBS->READ_TAG_SAMPLE_RATE (ClientKnobs.cpp:294).
const readTagSampleRate = 0.01

// readTagSampling overrides the sample rate and its random source (tests only).
type readTagSampling struct {
	rate float64
	draw func() float64 // nil: math/rand/v2
}

func withReadTagSampling(rate float64, draw func() float64) Option {
	return func(o *openOptions) { o.readTagSampling = &readTagSampling{rate: rate, draw: draw} }
}

// sampleReadTags is DatabaseContext::sampleReadTags (NativeAPI.actor.cpp:2080-2083):
// no draw at a nonpositive rate, otherwise random01() <= rate.
func (db *database) sampleReadTags() bool {
	s := db.readTags
	if s.rate <= 0 {
		return false
	}
	if s.draw != nil {
		return s.draw() <= s.rate
	}
	return rand.Float64() <= s.rate
}
