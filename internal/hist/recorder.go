package hist

import (
	"math"
	"sync/atomic"
	"time"
)

// Recorder is a histogram with one writer goroutine and any number of
// concurrent readers.
//
// A plain Histogram cannot be read while it is being written. Recorder keeps
// every field in an atomic so a live display can take snapshots (AddTo) while
// the worker that owns the Recorder keeps recording, with no lock on the hot
// path. Record must only ever be called from one goroutine at a time: with a
// single writer, min and max can be maintained with a load and a store
// instead of a compare-and-swap loop, and every counter update is an
// uncontended atomic add.
//
// Ordering is what keeps snapshots coherent. Record publishes min, max and sum
// before it increments the bucket count, and AddTo reads the bucket counts
// before min, max and sum. So any sample that a snapshot counted is already
// reflected in its min, max and sum (they may include one in-flight sample
// that the counts do not yet show, which is harmless).
type Recorder struct {
	counts        []atomic.Uint64
	sum, min, max atomic.Uint64
}

// NewRecorder returns an empty Recorder.
func NewRecorder() *Recorder {
	r := &Recorder{counts: make([]atomic.Uint64, bucketCount)}
	r.min.Store(math.MaxUint64)
	return r
}

// Record adds one sample. Call it from a single goroutine only.
func (r *Recorder) Record(d time.Duration) {
	v := clamp(d)
	if v < r.min.Load() {
		r.min.Store(v)
	}
	if v > r.max.Load() {
		r.max.Store(v)
	}
	r.sum.Add(v)
	r.counts[index(v)].Add(1) // last, see the type comment
}

// AddTo adds everything recorded so far into dst. It is safe to call while
// another goroutine is recording; dst is not shared and need not be safe.
func (r *Recorder) AddTo(dst *Histogram) {
	var n uint64
	for i := range r.counts {
		if c := r.counts[i].Load(); c != 0 {
			dst.counts[i] += c
			n += c
		}
	}
	if n == 0 {
		return
	}
	dst.total += n
	dst.sum += r.sum.Load()
	dst.min = min(dst.min, r.min.Load())
	dst.max = max(dst.max, r.max.Load())
}
