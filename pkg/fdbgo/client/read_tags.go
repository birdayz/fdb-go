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

// readTagSample is one native request's sampling outcome. C++ draws once per
// request it constructs and loadBalance sends that request to every
// alternative and hedge (LoadBalance.actor.h:446,662,689), so every attempt
// of one request carries the same value.
type readTagSample struct {
	present bool
	tags    []byte // the encoded TagSet; an empty set is present with no bytes
}

// sampleReadTags draws for a request built from the transaction's current
// read tags (trState->options.readTags).
func (tx *Transaction) sampleReadTags() readTagSample {
	if tx.db == nil || !tx.db.sampleReadTags() {
		return readTagSample{}
	}
	return readTagSample{present: true, tags: encodeTagSet(tx.readTags)}
}

// sampleTagSet draws for a request carrying an already captured set (a watch's parameters->tags).
func (db *database) sampleTagSet(tags []byte) readTagSample {
	if !db.sampleReadTags() {
		return readTagSample{}
	}
	return readTagSample{present: true, tags: tags}
}

func (s readTagSample) apply(has *bool, tags *[]byte) {
	if s.present {
		*has, *tags = true, s.tags
	}
}

// readTagRetry reports whether a read retrying after err builds a new request
// (C++'s own read loop: wrong_shard_server, all_alternatives_failed) and so
// draws again. A reply timeout or a maybe-delivered failure is retried inside
// loadBalance with the same request, so it keeps its draw.
func readTagRetry(err error) bool {
	return !isReplyTimeout(err) && !isMaybeDelivered(err)
}
