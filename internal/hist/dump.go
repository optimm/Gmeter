package hist

import (
	"encoding/json"
	"fmt"
	"time"
)

// Dump is the serialisable form of a Histogram: only the non-empty buckets,
// so a typical run is a few kilobytes of JSON rather than 31,744 counters.
//
// Keeping the full distribution (not just p50/p99) is what lets runs be
// merged and compared later. Percentiles cannot be averaged or combined, but
// histograms can simply be added.
type Dump struct {
	// PrecisionBits identifies the bucket layout. A dump from a build with a
	// different layout cannot be read back, and FromDump refuses it.
	PrecisionBits int         `json:"precision_bits"`
	SumNs         uint64      `json:"sum_ns"`
	MinNs         uint64      `json:"min_ns"`
	MaxNs         uint64      `json:"max_ns"`
	Buckets       [][2]uint64 `json:"buckets"` // [bucket index, count], ascending, non-empty only
}

// Dump returns the serialisable form of h. Buckets is never nil, so it
// encodes as [] rather than null.
func (h *Histogram) Dump() Dump {
	d := Dump{
		PrecisionBits: sigBits,
		SumNs:         h.sum,
		MinNs:         uint64(h.Min()),
		MaxNs:         h.max,
		Buckets:       [][2]uint64{},
	}
	for i, c := range h.counts {
		if c != 0 {
			d.Buckets = append(d.Buckets, [2]uint64{uint64(i), c})
		}
	}
	return d
}

// FromDump rebuilds a Histogram. It rejects dumps with a different bucket
// layout, out-of-range bucket indices, or min above max, rather than
// silently producing wrong percentiles.
func FromDump(d Dump) (*Histogram, error) {
	if d.PrecisionBits != sigBits {
		return nil, fmt.Errorf("hist: dump uses %d precision bits, this build uses %d", d.PrecisionBits, sigBits)
	}
	h := New()
	for _, b := range d.Buckets {
		if b[0] >= bucketCount {
			return nil, fmt.Errorf("hist: dump has bucket index %d, but there are only %d buckets", b[0], bucketCount)
		}
		h.counts[b[0]] += b[1]
		h.total += b[1]
	}
	if h.total == 0 {
		return h, nil
	}
	if d.MinNs > d.MaxNs {
		return nil, fmt.Errorf("hist: dump has min %dns above max %dns", d.MinNs, d.MaxNs)
	}
	h.sum, h.min, h.max = d.SumNs, d.MinNs, d.MaxNs
	return h, nil
}

// MarshalJSON encodes h as its Dump, so that anything holding a histogram (a
// run's result sent from one machine to another) keeps the whole distribution.
func (h *Histogram) MarshalJSON() ([]byte, error) {
	return json.Marshal(h.Dump())
}

// UnmarshalJSON reads a Dump into h, refusing one FromDump would refuse.
func (h *Histogram) UnmarshalJSON(raw []byte) error {
	var d Dump
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	got, err := FromDump(d)
	if err != nil {
		return err
	}
	*h = *got
	return nil
}

// FractionAbove returns the share of samples that are slower than v, from 0
// to 1. It counts whole buckets above the one holding v, so samples within
// ~0.1% of v (the same bucket) are not counted as above it.
func (h *Histogram) FractionAbove(v time.Duration) float64 {
	if h.total == 0 {
		return 0
	}
	var above uint64
	for i := index(clamp(v)) + 1; i < len(h.counts); i++ {
		above += h.counts[i]
	}
	return float64(above) / float64(h.total)
}
