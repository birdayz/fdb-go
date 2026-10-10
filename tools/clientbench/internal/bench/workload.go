package bench

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

type KV struct{ Key, Value []byte }

type Transaction interface {
	Get([]byte) ([]byte, error)
	Set([]byte, []byte)
	Range([]byte, []byte, int) ([]KV, error)
	ReadVersion() (int64, error)
}

type Database interface {
	Transact(readOnly bool, fn func(Transaction) error) error
	Local(fn func(Transaction) error) error
	Counters() ClientCounters
	Close()
}

type Dataset struct {
	Points          map[int]KV
	Range           []KV
	RangeEnd        []byte
	Writes          [][]byte
	Initial         []byte
	Warmup, Measure [][]byte
	BatchWrites     [][][]byte
	BatchInitial    [][]byte
	BatchValueBytes int
}

func Payload(size, worker, index int, phase byte) []byte {
	value := make([]byte, size)
	for i := range value {
		value[i] = byte((i*31 + 17) % 251)
	}
	if size >= 17 {
		value[0] = phase
		binary.LittleEndian.PutUint64(value[1:9], uint64(worker))
		binary.LittleEndian.PutUint64(value[9:17], uint64(index))
	}
	return value
}

func Prepare(prefix string, workers, warmup, iterations int) Dataset {
	d := Dataset{Points: make(map[int]KV), Initial: Payload(100, 0, 0, 'i')}
	for _, size := range []int{100, 1000, 10000} {
		d.Points[size] = KV{[]byte(fmt.Sprintf("%spoint/%05d", prefix, size)), Payload(size, 0, size, 'p')}
	}
	for i := range 100 {
		value := make([]byte, 8)
		binary.LittleEndian.PutUint64(value, uint64(i))
		d.Range = append(d.Range, KV{[]byte(fmt.Sprintf("%srange/%04d", prefix, i)), value})
	}
	d.RangeEnd = []byte(prefix + "range0")
	for i := range workers {
		d.Writes = append(d.Writes, []byte(fmt.Sprintf("%swrite/%04d", prefix, i)))
	}
	for i := range warmup {
		d.Warmup = append(d.Warmup, Payload(100, i%workers, i, 'w'))
	}
	for i := range iterations {
		d.Measure = append(d.Measure, Payload(100, i%workers, i, 'm'))
	}
	return d
}

// PrepareBatchSet50 pregenerates worker-local keys and operands so none of
// their allocation or formatting enters the measured phase.
func (d *Dataset) PrepareBatchSet50(prefix string) {
	d.prepareBatchSet50(prefix, 100)
}

func batchValueBytes(workload string) int {
	switch workload {
	case "batchset50":
		return 100
	case "batchset50x1000":
		return 1000
	default:
		return 0
	}
}

func (d *Dataset) prepareBatchSet50(prefix string, valueBytes int) {
	d.BatchValueBytes = valueBytes
	for worker := range d.Writes {
		keys := make([][]byte, 50)
		for slot := range keys {
			keys[slot] = []byte(fmt.Sprintf("%sbatchset50/%04d/%04d", prefix, worker, slot))
		}
		d.BatchWrites = append(d.BatchWrites, keys)
		d.BatchInitial = append(d.BatchInitial, batchSet50Payload(valueBytes, worker, 0, 'i'))
	}
	for i := range d.Warmup {
		d.Warmup[i] = batchSet50Payload(valueBytes, i%len(d.Writes), i, 'w')
	}
	for i := range d.Measure {
		d.Measure[i] = batchSet50Payload(valueBytes, i%len(d.Writes), i, 'm')
	}
}

func batchSet50Payload(valueBytes, worker, index int, phase byte) []byte {
	out := make([]byte, 50*valueBytes)
	for slot := range 50 {
		value := out[slot*valueBytes : (slot+1)*valueBytes]
		copy(value, Payload(valueBytes, worker, index, phase))
		binary.LittleEndian.PutUint64(value[17:25], uint64(slot))
	}
	return out
}

func CheckValue(actual, expected []byte) error {
	if !bytes.Equal(actual, expected) {
		return fmt.Errorf("value mismatch: got %d bytes, expected exact %d bytes", len(actual), len(expected))
	}
	return nil
}

func CheckRange(rows, expected []KV) error {
	if len(rows) != len(expected) {
		return fmt.Errorf("range count %d, want %d", len(rows), len(expected))
	}
	for i := range rows {
		if !bytes.Equal(rows[i].Key, expected[i].Key) || !bytes.Equal(rows[i].Value, expected[i].Value) {
			return fmt.Errorf("range key/value mismatch at row %d", i)
		}
	}
	return nil
}

func (d Dataset) Seed(db Database) error {
	return db.Transact(false, func(tx Transaction) error {
		for _, size := range []int{100, 1000, 10000} {
			kv := d.Points[size]
			tx.Set(kv.Key, kv.Value)
		}
		for _, kv := range d.Range {
			tx.Set(kv.Key, kv.Value)
		}
		for _, key := range d.Writes {
			tx.Set(key, d.Initial)
		}
		for worker, keys := range d.BatchWrites {
			for slot, key := range keys {
				tx.Set(key, d.BatchInitial[worker][slot*d.BatchValueBytes:(slot+1)*d.BatchValueBytes])
			}
		}
		return nil
	})
}

func (d Dataset) ValidateSeed(db Database) error {
	return db.Transact(true, func(tx Transaction) error {
		for _, size := range []int{100, 1000, 10000} {
			kv := d.Points[size]
			got, err := tx.Get(kv.Key)
			if err != nil {
				return err
			}
			if err := CheckValue(got, kv.Value); err != nil {
				return err
			}
		}
		rows, err := tx.Range(d.Range[0].Key, d.RangeEnd, 101)
		if err != nil {
			return err
		}
		if err := CheckRange(rows, d.Range); err != nil {
			return err
		}
		for _, key := range d.Writes {
			got, err := tx.Get(key)
			if err != nil {
				return err
			}
			if err := CheckValue(got, d.Initial); err != nil {
				return err
			}
		}
		for worker, keys := range d.BatchWrites {
			for slot, key := range keys {
				got, err := tx.Get(key)
				if err != nil {
					return err
				}
				if err := CheckValue(got, d.BatchInitial[worker][slot*d.BatchValueBytes:(slot+1)*d.BatchValueBytes]); err != nil {
					return fmt.Errorf("seed batch worker %d slot %d: %w", worker, slot, err)
				}
			}
		}
		return nil
	})
}

func (d Dataset) Operation(db Database, workload string, worker int, payload []byte) (attempts int, err error) {
	fn := func(tx Transaction) error {
		attempts++
		size := 100
		switch workload {
		case "get1000", "read1000":
			size = 1000
		case "get10000":
			size = 10000
		}
		switch workload {
		case "get100", "get1000", "get10000", "read1000":
			kv := d.Points[size]
			got, err := tx.Get(kv.Key)
			if err != nil {
				return err
			}
			return CheckValue(got, kv.Value)
		case "range10", "range100":
			count, end := 100, d.RangeEnd
			if workload == "range10" {
				count, end = 10, d.Range[10].Key
			}
			rows, err := tx.Range(d.Range[0].Key, end, count+1)
			if err != nil {
				return err
			}
			return CheckRange(rows, d.Range[:count])
		case "grv":
			version, err := tx.ReadVersion()
			if err != nil {
				return err
			}
			if version <= 0 {
				return fmt.Errorf("invalid read version %d", version)
			}
			return nil
		case "batchset50", "batchset50x1000":
			valueBytes := batchValueBytes(workload)
			if d.BatchValueBytes != valueBytes || len(d.BatchWrites) != len(d.Writes) || len(d.BatchWrites[worker]) != 50 || len(payload) != 50*valueBytes {
				return fmt.Errorf("%s requires 50 prepared keys and 50 %d-byte values", workload, valueBytes)
			}
			for slot, key := range d.BatchWrites[worker] {
				tx.Set(key, payload[slot*valueBytes:(slot+1)*valueBytes])
			}
			return nil
		case "set100", "ryw":
			tx.Set(d.Writes[worker], payload)
			if workload == "ryw" {
				got, err := tx.Get(d.Writes[worker])
				if err != nil {
					return err
				}
				return CheckValue(got, payload)
			}
			return nil
		default:
			return fmt.Errorf("unknown workload %q", workload)
		}
	}
	if workload == "ryw" {
		err = db.Local(fn)
	} else {
		err = db.Transact(workload == "read1000" || workload == "grv", fn)
	}
	return attempts, err
}

func (d Dataset) ValidateWrites(db Database, workload string, warmup, measured int) error {
	if size := batchValueBytes(workload); size > 0 && size != d.BatchValueBytes {
		return fmt.Errorf("%s dataset has wrong value size %d", workload, d.BatchValueBytes)
	}
	return db.Transact(true, func(tx Transaction) error {
		for worker, key := range d.Writes {
			want := d.Initial
			if workload == "set100" {
				for i := worker; i < warmup; i += len(d.Writes) {
					want = d.Warmup[i]
				}
				for i := worker; i < measured; i += len(d.Writes) {
					want = d.Measure[i]
				}
			}
			got, err := tx.Get(key)
			if err != nil {
				return err
			}
			if err := CheckValue(got, want); err != nil {
				return fmt.Errorf("persisted worker %d: %w", worker, err)
			}
		}
		if batchValueBytes(workload) > 0 {
			for worker, keys := range d.BatchWrites {
				want := d.BatchInitial[worker]
				for i := worker; i < warmup; i += len(d.Writes) {
					want = d.Warmup[i]
				}
				for i := worker; i < measured; i += len(d.Writes) {
					want = d.Measure[i]
				}
				for slot, key := range keys {
					got, err := tx.Get(key)
					if err != nil {
						return err
					}
					if err := CheckValue(got, want[slot*d.BatchValueBytes:(slot+1)*d.BatchValueBytes]); err != nil {
						return fmt.Errorf("persisted batch worker %d slot %d: %w", worker, slot, err)
					}
				}
			}
		}
		return nil
	})
}
