package hist

import (
	"testing"
	"time"
)

func TestEmptyHistogramReportsZeros(t *testing.T) {
	h := New()
	if h.Count() != 0 {
		t.Fatalf("Count() = %d, want 0", h.Count())
	}
	if got := h.Quantile(0.99); got != 0 {
		t.Fatalf("Quantile(0.99) on empty = %v, want 0", got)
	}
	if h.Min() != 0 || h.Max() != 0 || h.Mean() != 0 {
		t.Fatalf("empty Min/Max/Mean = %v/%v/%v, want all 0", h.Min(), h.Max(), h.Mean())
	}
}

func TestMinMaxMeanAreExact(t *testing.T) {
	h := New()
	for _, d := range []time.Duration{3 * time.Millisecond, 1 * time.Millisecond, 8 * time.Millisecond} {
		h.Record(d)
	}
	if h.Count() != 3 {
		t.Fatalf("Count() = %d, want 3", h.Count())
	}
	if h.Min() != 1*time.Millisecond {
		t.Fatalf("Min() = %v, want 1ms", h.Min())
	}
	if h.Max() != 8*time.Millisecond {
		t.Fatalf("Max() = %v, want 8ms", h.Max())
	}
	if h.Mean() != 4*time.Millisecond {
		t.Fatalf("Mean() = %v, want 4ms", h.Mean())
	}
}

func TestSmallValuesAreRecordedExactly(t *testing.T) {
	h := New()
	h.Record(500 * time.Nanosecond)
	if got := h.Quantile(0.5); got != 500*time.Nanosecond {
		t.Fatalf("Quantile(0.5) = %v, want exactly 500ns", got)
	}
}

func TestQuantilesStayWithinPrecisionBound(t *testing.T) {
	// 1µs..1,000,000µs, one sample each: the q-quantile is exactly q*N µs.
	const n = 1_000_000
	h := New()
	for i := 1; i <= n; i++ {
		h.Record(time.Duration(i) * time.Microsecond)
	}
	for _, q := range []float64{0.5, 0.9, 0.99, 0.999} {
		want := time.Duration(q*n) * time.Microsecond
		got := h.Quantile(q)
		// Reported value is the top of the bucket: never below the truth,
		// and at most 2^-10 (~0.1%) above it. Allow 0.2% to stay robust.
		if got < want {
			t.Errorf("Quantile(%v) = %v, below true value %v", q, got, want)
		}
		if limit := time.Duration(float64(want) * 1.002); got > limit {
			t.Errorf("Quantile(%v) = %v, more than 0.2%% above true value %v", q, got, want)
		}
	}
}

func TestQuantileEdgesReturnExactMinAndMax(t *testing.T) {
	h := New()
	h.Record(123456789 * time.Nanosecond)
	h.Record(987654321 * time.Nanosecond)
	if got := h.Quantile(0); got != 123456789*time.Nanosecond {
		t.Fatalf("Quantile(0) = %v, want exact min", got)
	}
	if got := h.Quantile(1); got != 987654321*time.Nanosecond {
		t.Fatalf("Quantile(1) = %v, want exact max", got)
	}
}

func TestMergeEqualsRecordingEverythingIntoOne(t *testing.T) {
	all, a, b := New(), New(), New()
	for i := 1; i <= 10_000; i++ {
		d := time.Duration(i*i) * time.Microsecond // skewed distribution
		all.Record(d)
		if i%2 == 0 {
			a.Record(d)
		} else {
			b.Record(d)
		}
	}
	a.Merge(b)

	if a.Count() != all.Count() || a.Min() != all.Min() || a.Max() != all.Max() || a.Mean() != all.Mean() {
		t.Fatalf("merged summary differs: count %d/%d min %v/%v max %v/%v mean %v/%v",
			a.Count(), all.Count(), a.Min(), all.Min(), a.Max(), all.Max(), a.Mean(), all.Mean())
	}
	for _, q := range []float64{0.5, 0.9, 0.99, 0.999} {
		if a.Quantile(q) != all.Quantile(q) {
			t.Errorf("Quantile(%v): merged %v, direct %v", q, a.Quantile(q), all.Quantile(q))
		}
	}
}

func TestMergeIntoEmptyKeepsMinAndMax(t *testing.T) {
	src := New()
	src.Record(5 * time.Millisecond)
	dst := New()
	dst.Merge(src)
	if dst.Min() != 5*time.Millisecond || dst.Max() != 5*time.Millisecond {
		t.Fatalf("Min/Max after merge into empty = %v/%v, want 5ms/5ms", dst.Min(), dst.Max())
	}
}

func TestOutOfRangeValuesAreClamped(t *testing.T) {
	h := New()
	h.Record(-5 * time.Second)
	h.Record(24 * time.Hour)
	if h.Count() != 2 {
		t.Fatalf("Count() = %d, want 2 (out-of-range samples must still be counted)", h.Count())
	}
	if h.Min() != 0 {
		t.Fatalf("Min() = %v, want 0 for a negative sample", h.Min())
	}
	if h.Max() != MaxValue {
		t.Fatalf("Max() = %v, want clamp to MaxValue %v", h.Max(), MaxValue)
	}
}

func TestSumIsTheExactTotalOfTheSamples(t *testing.T) {
	h := New()
	if h.Sum() != 0 {
		t.Errorf("empty histogram: Sum = %v, want 0", h.Sum())
	}
	for _, d := range []time.Duration{3 * time.Millisecond, 7 * time.Microsecond, 11 * time.Second, -5} {
		h.Record(d) // a negative sample counts as 0
	}
	if want := 3*time.Millisecond + 7*time.Microsecond + 11*time.Second; h.Sum() != want {
		t.Errorf("Sum = %v, want %v: exact, not rounded to a bucket", h.Sum(), want)
	}
}
