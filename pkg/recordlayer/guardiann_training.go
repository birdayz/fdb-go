package recordlayer

import (
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// addToStatsIfNecessary is Insert.addToStatsIfNecessary. Samples stay in the
// untrained coordinate space until a rotated, negated centroid is established.
func (g *guardiann) addToStatsIfNecessary(tx fdb.WritableTransaction, random *splittableRandom, info *guardiannAccessInfoValue, vector gVector) error {
	if !g.config.useRaBitQ || info.negatedCentroid != nil {
		return nil
	}
	if random.nextDouble() < g.config.sampleVectorStatsProbability {
		if err := g.appendSample(tx, random, 1, vector.data); err != nil {
			return err
		}
	}
	if !(random.nextDouble() < g.config.maintainStatsProbability) {
		return nil
	}
	samples := g.sub(gSubSamples)
	r, err := fdb.PrefixRange(samples.Bytes())
	if err != nil {
		return err
	}
	kvs, err := tx.Snapshot().GetRange(r, fdb.RangeOptions{Limit: g.config.sampleBatchSize, Reverse: true, Mode: fdb.StreamingModeIterator}).GetSliceWithError()
	if err != nil {
		return err
	}
	count := int64(0)
	var sum []float64
	for _, kv := range kvs {
		key, err := samples.Unpack(kv.Key)
		if err != nil {
			return err
		}
		value, err := tuple.Unpack(kv.Value)
		if err != nil {
			return err
		}
		if len(key) != 2 || len(value) != 1 {
			return &RecordCoreError{Message: "invalid GuardiANN sample tuple"}
		}
		n, ok := key[0].(int64)
		raw, bytesOK := value[0].([]byte)
		if !ok || n < 0 || n > 1<<31-1 || !bytesOK {
			return &RecordCoreError{Message: "invalid GuardiANN sample count or vector"}
		}
		v, err := decodeGVector(raw)
		if err != nil {
			return err
		}
		if v.typ != 2 || len(v.data) != g.config.numDimensions {
			return &RecordCoreError{Message: "invalid GuardiANN sample vector type or dimensions"}
		}
		if sum == nil {
			sum = append([]float64(nil), v.data...)
		} else {
			for i, x := range v.data {
				sum[i] += x
			}
		}
		count += n
		if err := tx.AddReadConflictKey(kv.Key); err != nil {
			return err
		}
		tx.Clear(kv.Key)
	}
	if count == 0 {
		return nil
	}
	if err := g.appendSample(tx, random, count, sum); err != nil {
		return err
	}
	if count >= int64(g.config.statsThreshold) {
		seed := random.nextLong()
		for i := range sum {
			sum[i] *= -1 / float64(count)
		}
		centroid := newFhtKacRotator(seed, g.config.numDimensions, 10).apply(sum)
		g.writeAccessInfo(tx, &guardiannAccessInfoValue{rotatorSeed: seed, negatedCentroid: centroid})
		tx.ClearRange(r)
	}
	return nil
}

func (g *guardiann) appendSample(tx fdb.WritableTransaction, random *splittableRandom, count int64, vector []float64) error {
	id, err := g.randomUUID(random)
	if err != nil {
		return fmt.Errorf("GuardiANN sample identity: %w", err)
	}
	tx.Set(fdb.Key(g.sub(gSubSamples).Pack(tuple.Tuple{count, id})), tuple.Tuple{serializeVector(vector)}.Pack())
	return nil
}
