package hist

import (
	"testing"
	"time"
	"unsafe"
)

// feed records the same skewed samples into any number of sinks.
func feed(n int, sinks ...interface{ Record(time.Duration) }) {
	for i := 1; i <= n; i++ {
		d := time.Duration(i*i%1_000_003) * time.Microsecond
		for _, s := range sinks {
			s.Record(d)
		}
	}
}

func sameSummary(t *testing.T, got, want *Histogram) {
	t.Helper()
	if got.Count() != want.Count() || got.Min() != want.Min() || got.Max() != want.Max() || got.Mean() != want.Mean() {
		t.Fatalf("summary differs: count %d/%d min %v/%v max %v/%v mean %v/%v",
			got.Count(), want.Count(), got.Min(), want.Min(), got.Max(), want.Max(), got.Mean(), want.Mean())
	}
	for _, q := range []float64{0.5, 0.9, 0.99, 0.999} {
		if got.Quantile(q) != want.Quantile(q) {
			t.Errorf("Quantile(%v) = %v, want %v", q, got.Quantile(q), want.Quantile(q))
		}
	}
}

func TestRecorderReadsBackWhatAPlainHistogramWouldHold(t *testing.T) {
	rec, plain := NewRecorder(), New()
	feed(50_000, rec, plain)

	got := New()
	rec.AddTo(got)

	sameSummary(t, got, plain)
}

func TestRecorderAddToAccumulatesAcrossRecorders(t *testing.T) {
	a, b, plain := NewRecorder(), NewRecorder(), New()
	feed(10_000, a, plain)
	feed(20_000, b, plain)

	got := New()
	a.AddTo(got)
	b.AddTo(got)

	sameSummary(t, got, plain)
}

func TestEmptyRecorderAddsNothing(t *testing.T) {
	got := New()
	got.Record(7 * time.Millisecond)
	NewRecorder().AddTo(got)
	if got.Count() != 1 || got.Min() != 7*time.Millisecond || got.Max() != 7*time.Millisecond {
		t.Fatalf("an empty recorder changed the destination: count=%d min=%v max=%v", got.Count(), got.Min(), got.Max())
	}
}

// The point of Recorder: a live display reads it while the worker that owns it
// keeps recording. Run under -race this fails if any field is accessed
// without synchronisation.
func TestRecorderCanBeReadWhileItsOwnerIsWriting(t *testing.T) {
	const n = 200_000
	rec := NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range n {
			rec.Record(time.Duration(1+i%1000) * time.Microsecond)
		}
	}()

	var last uint64
	for finished := false; !finished; {
		select {
		case <-done:
			finished = true
		default:
		}
		snap := New()
		rec.AddTo(snap)
		if snap.Count() < last {
			t.Fatalf("Count went backwards: %d then %d", last, snap.Count())
		}
		last = snap.Count()
		if snap.Count() > 0 {
			if p := snap.Quantile(0.5); p < snap.Min() || p > snap.Max() {
				t.Fatalf("median %v outside [min %v, max %v] in a live snapshot", p, snap.Min(), snap.Max())
			}
		}
	}

	final := New()
	rec.AddTo(final)
	if final.Count() != n {
		t.Fatalf("final Count = %d, want %d", final.Count(), n)
	}
}

func TestDeltaHoldsOnlyTheSamplesRecordedSinceTheEarlierSnapshot(t *testing.T) {
	rec := NewRecorder()
	for range 1000 {
		rec.Record(time.Millisecond) // old, fast samples
	}
	earlier := New()
	rec.AddTo(earlier)

	for i := 1; i <= 50; i++ {
		rec.Record(time.Duration(i) * time.Millisecond) // new samples: 1..50ms
	}
	later := New()
	rec.AddTo(later)

	d := later.Delta(earlier)

	if d.Count() != 50 {
		t.Fatalf("Delta Count = %d, want 50", d.Count())
	}
	// Median of 1..50ms is 25ms; the 1000 old 1ms samples must not drag it down.
	if p := d.Quantile(0.5); p < 25*time.Millisecond || p > 26*time.Millisecond {
		t.Fatalf("Delta median = %v, want ~25ms", p)
	}
	// Max is recovered from bucket edges: never below, and within 0.2% of, the true 50ms.
	if m := d.Max(); m < 50*time.Millisecond || m > time.Duration(float64(50*time.Millisecond)*1.002) {
		t.Fatalf("Delta Max = %v, want ~50ms", m)
	}
}

func TestDeltaOfIdenticalSnapshotsIsEmpty(t *testing.T) {
	rec := NewRecorder()
	feed(1000, rec)
	a, b := New(), New()
	rec.AddTo(a)
	rec.AddTo(b)

	d := b.Delta(a)
	if d.Count() != 0 || d.Quantile(0.99) != 0 || d.Max() != 0 {
		t.Fatalf("expected an empty delta, got count=%d p99=%v max=%v", d.Count(), d.Quantile(0.99), d.Max())
	}
}

// The padding in Recorder is a measured fix: with neighbouring Recorders
// sharing cache lines, parallel Record was ~2.2x slower (see
// BenchmarkRecorderRecordParallel). A benchmark cannot guard that in CI, so
// pin the layout instead: the fields every Record writes must have at least a
// full cache line of padding on each side.
func TestRecorderHotFieldsAreIsolatedOnTheirOwnCacheLines(t *testing.T) {
	var r Recorder
	if lead := unsafe.Offsetof(r.sum); lead < cacheLine {
		t.Errorf("only %d bytes before the hot fields, want >= %d", lead, cacheLine)
	}
	if tail := unsafe.Sizeof(r) - unsafe.Offsetof(r.max) - unsafe.Sizeof(r.max); tail < cacheLine {
		t.Errorf("only %d bytes after the hot fields, want >= %d", tail, cacheLine)
	}
}
