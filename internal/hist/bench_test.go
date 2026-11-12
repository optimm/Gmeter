package hist

import (
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func sample(i int) time.Duration { return time.Duration(i&0xFFFF+1) * time.Microsecond }

func BenchmarkHistogramRecord(b *testing.B) {
	h := New()
	for i := 0; b.Loop(); i++ {
		h.Record(sample(i))
	}
}

func BenchmarkRecorderRecord(b *testing.B) {
	r := NewRecorder()
	for i := 0; b.Loop(); i++ {
		r.Record(sample(i))
	}
}

// Every goroutine records into its OWN Recorder, so there is no logically
// shared data at all. If this scales worse than BenchmarkRecorderRecord at the
// same per-goroutine cost, the Recorders are fighting over cache lines.
func BenchmarkRecorderRecordParallel(b *testing.B) {
	recs := make([]*Recorder, runtime.GOMAXPROCS(0))
	for i := range recs {
		recs[i] = NewRecorder() // small structs allocated back to back
	}
	var next atomic.Int32
	b.RunParallel(func(pb *testing.PB) {
		r := recs[int(next.Add(1)-1)%len(recs)]
		for i := 0; pb.Next(); i++ {
			r.Record(sample(i))
		}
	})
}
