// Package hist implements a fixed-memory latency histogram in the style of
// HdrHistogram.
//
// Values are nanoseconds. Each power-of-two range [2^m, 2^(m+1)) is split into
// 2^sigBits equal-width buckets, so the relative error of any reported
// quantile is at most 2^-sigBits (about 0.1%) regardless of magnitude, while
// the memory footprint stays constant (~250KB) no matter how many samples are
// recorded. Values below 2^sigBits ns are stored exactly.
//
// A Histogram is NOT safe for concurrent use. The intended pattern is one
// Histogram per goroutine (no locks, no atomics on the hot path), merged once
// the writers are done.
package hist

import (
	"math"
	"math/bits"
	"time"
)

const (
	sigBits  = 10
	subCount = 1 << sigBits // buckets per power-of-two range

	// MaxValue is the largest trackable latency (~18 minutes). Larger samples
	// are clamped to it so they are still counted rather than lost.
	MaxValue = time.Duration(1<<40 - 1)

	bucketCount = ((40-1-sigBits)+1)*subCount + subCount
)

// Histogram records durations and answers quantile queries.
type Histogram struct {
	counts []uint64
	total  uint64
	sum    uint64 // nanoseconds
	min    uint64
	max    uint64
}

// New returns an empty Histogram.
func New() *Histogram {
	return &Histogram{counts: make([]uint64, bucketCount), min: math.MaxUint64}
}

// Record adds one sample. Negative durations are recorded as 0 and durations
// above MaxValue are clamped to MaxValue.
func (h *Histogram) Record(d time.Duration) {
	v := clamp(d)
	h.counts[index(v)]++
	h.total++
	h.sum += v
	h.min = min(h.min, v)
	h.max = max(h.max, v)
}

// Count returns the number of recorded samples.
func (h *Histogram) Count() uint64 { return h.total }

// Min returns the exact smallest sample, or 0 if the histogram is empty.
func (h *Histogram) Min() time.Duration {
	if h.total == 0 {
		return 0
	}
	return time.Duration(h.min)
}

// Max returns the exact largest sample, or 0 if the histogram is empty.
func (h *Histogram) Max() time.Duration { return time.Duration(h.max) }

// Sum returns the exact total of all samples.
func (h *Histogram) Sum() time.Duration { return time.Duration(h.sum) }

// Mean returns the exact arithmetic mean, or 0 if the histogram is empty.
func (h *Histogram) Mean() time.Duration {
	if h.total == 0 {
		return 0
	}
	return time.Duration(h.sum / h.total)
}

// Quantile returns the value at quantile q in [0,1]. q<=0 yields the exact
// minimum and q>=1 the exact maximum. Otherwise the result is the upper edge
// of the bucket holding the sample at that rank, so it never understates the
// true value and overstates it by at most ~0.1%.
func (h *Histogram) Quantile(q float64) time.Duration {
	if h.total == 0 {
		return 0
	}
	if q <= 0 {
		return h.Min()
	}
	if q >= 1 {
		return h.Max()
	}
	rank := max(uint64(math.Ceil(q*float64(h.total))), 1) // 1-based
	var seen uint64
	for i, c := range h.counts {
		seen += c
		if seen >= rank {
			// A bucket's upper edge can exceed the real max; never report that.
			return time.Duration(min(highest(i), h.max))
		}
	}
	return h.Max()
}

// Merge adds all samples of o into h. o is left unchanged.
func (h *Histogram) Merge(o *Histogram) {
	if o.total == 0 {
		return
	}
	for i, c := range o.counts {
		h.counts[i] += c
	}
	h.total += o.total
	h.sum += o.sum
	h.min = min(h.min, o.min)
	h.max = max(h.max, o.max)
}

// Delta returns the samples that are in h but not in prev, where prev is an
// earlier snapshot of the same growing stream of samples. It turns two
// cumulative snapshots into "what happened in between", which is how a live
// display gets per-second percentiles without resetting anything.
//
// Min and Max of the result are recovered from bucket edges instead of being
// tracked, so they are accurate to ~0.1%; Max never exceeds h's exact Max.
func (h *Histogram) Delta(prev *Histogram) *Histogram {
	d := New()
	first, last := -1, -1
	for i, c := range h.counts {
		p := prev.counts[i]
		if c <= p {
			continue
		}
		d.counts[i] = c - p
		d.total += c - p
		if first < 0 {
			first = i
		}
		last = i
	}
	if d.total == 0 {
		return d
	}
	d.sum = h.sum - min(prev.sum, h.sum)
	d.min = max(lowest(first), h.min)
	d.max = min(highest(last), h.max)
	return d
}

// clamp converts a duration to a trackable non-negative value.
func clamp(d time.Duration) uint64 {
	switch {
	case d < 0:
		return 0
	case d > MaxValue:
		return uint64(MaxValue)
	default:
		return uint64(d)
	}
}

// index maps a value to its bucket. Values below subCount map to themselves;
// above that, the position is the power-of-two range plus the top sigBits
// bits that follow the leading one.
func index(v uint64) int {
	if v < subCount {
		return int(v)
	}
	shift := bits.Len64(v) - 1 - sigBits
	return (shift+1)*subCount + int(v>>shift) - subCount
}

// lowest returns the smallest value that maps to bucket i.
func lowest(i int) uint64 {
	if i < subCount {
		return uint64(i)
	}
	return uint64(subCount+i%subCount) << (i/subCount - 1)
}

// highest returns the largest value that maps to bucket i.
func highest(i int) uint64 {
	if i < subCount {
		return uint64(i)
	}
	return lowest(i) + (1 << (i/subCount - 1)) - 1
}
