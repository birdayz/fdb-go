package recordlayer

import (
	"math"

	"fdb.dev/pkg/rabitq"
	"fdb.dev/pkg/recordlayer/vectorcodec"
)

// guardiannVectorCodec owns one operation's coordinate system. Plain stored
// vectors predate training; encoded vectors are already in trained coordinates.
type guardiannVectorCodec struct {
	transform *hnswTransform
	quantizer *rabitq.Quantizer
	// quantizerErr is why a trained RaBitQ quantizer cannot be constructed.
	// Java constructs it (Primitives.quantizer) only at an insert of a new
	// key, a search, a task's write and a task body past its no-op exits, so
	// the codec defers the refusal to those points (requireQuantizer); what
	// never constructs it (a delete enqueuing nothing, an empty drain) runs.
	quantizerErr error
	config       guardiannConfig
}

func newGuardiannVectorCodec(config guardiannConfig, info *guardiannAccessInfoValue) *guardiannVectorCodec {
	c := &guardiannVectorCodec{config: config}
	if info == nil || info.negatedCentroid == nil {
		return c
	}
	c.transform = &hnswTransform{
		rotator:          newFhtKacRotator(info.rotatorSeed, config.numDimensions, 10),
		negatedCentroid:  info.negatedCentroid,
		normalizeVectors: config.metric == VectorMetricCosine,
	}
	if config.useRaBitQ {
		if !rabitq.ValidNumExBits(config.raBitQNumExBits) {
			c.quantizerErr = &IllegalArgumentError{Message: "RaBitQ encodes 1 to 8 extra bits"}
			return c
		}
		c.quantizer = rabitq.NewQuantizer(rabitq.Metric(config.metric), config.raBitQNumExBits)
	}
	return c
}

// requireQuantizer is Java's Primitives.quantizer construction: it refuses
// where RaBitQuantizer's constructor throws.
func (c *guardiannVectorCodec) requireQuantizer() error { return c.quantizerErr }

func (c *guardiannVectorCodec) toStoredCoordinates(v gVector) (gVector, error) {
	if c.transform == nil || v.typ == rabitq.TypeByte {
		return v, nil
	}
	transform := *c.transform
	if transform.normalizeVectors {
		var norm float64
		for _, x := range v.data {
			norm += x * x
		}
		if !(norm > 0) || math.IsInf(norm, 0) {
			return gVector{}, &IllegalArgumentError{Message: "vector has an L2 norm of infinite, not a number, or 0"}
		}
		// RealVector.normalize preserves the input precision before rotation.
		var err error
		v, err = decodeGVector(vectorcodec.SerializeAs(v.typ, normalizeFloat64s(v.data)))
		if err != nil {
			return gVector{}, err
		}
		transform.normalizeVectors = false
	}
	return gVector{data: transform.apply(v.data), typ: vectorcodec.TypeDouble}, nil
}

func (c *guardiannVectorCodec) toClientCoordinates(v gVector) gVector {
	if c.transform == nil {
		return v
	}
	return gVector{data: c.transform.invertedApply(v.data), typ: vectorcodec.TypeDouble}
}

func (c *guardiannVectorCodec) decode(raw []byte) (gVector, error) {
	if len(raw) > 0 && raw[0] == rabitq.TypeByte {
		q := rabitq.NewQuantizer(rabitq.Metric(c.config.metric), c.config.raBitQNumExBits)
		if !rabitq.ValidNumExBits(c.config.raBitQNumExBits) {
			return gVector{}, &IllegalArgumentError{Message: "RaBitQ encodes 1 to 8 extra bits"}
		}
		data, err := q.Decode(raw, c.config.numDimensions)
		if err != nil {
			return gVector{}, err
		}
		return gVector{data: data, typ: rabitq.TypeByte, encoded: append([]byte(nil), raw...)}, nil
	}
	v, err := decodeGVector(raw)
	if err != nil {
		return gVector{}, err
	}
	return c.toStoredCoordinates(v)
}

func (c *guardiannVectorCodec) encode(v gVector) []byte {
	if c.quantizer == nil || v.typ == rabitq.TypeByte {
		return v.encode()
	}
	return c.quantizer.Encode(v.data)
}

// RaBitDistanceEstimator estimates only when exactly one operand is encoded.
func (c *guardiannVectorCodec) distance(a, b gVector) (float64, error) {
	d := javaMetricDistance(a.data, b.data, c.config.metric)
	if c.quantizer != nil {
		var err error
		switch {
		case a.typ != rabitq.TypeByte && b.typ == rabitq.TypeByte:
			d, err = c.quantizer.Distance(a.data, b.encoded, c.config.numDimensions)
		case a.typ == rabitq.TypeByte && b.typ != rabitq.TypeByte:
			d, err = c.quantizer.Distance(b.data, a.encoded, c.config.numDimensions)
		}
		if err == nil && (a.typ == rabitq.TypeByte) != (b.typ == rabitq.TypeByte) {
			switch c.config.metric {
			case VectorMetricEuclidean:
				d = math.Sqrt(math.Max(0, d))
			case VectorMetricEuclideanSquare:
				d = math.Max(0, d)
			}
		}
		if err != nil || math.IsNaN(d) || math.IsInf(d, 0) {
			return 0, &IllegalArgumentError{Message: "distance is infinite or not a number"}
		}
	}
	return d, nil
}
