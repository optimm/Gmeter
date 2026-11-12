package pacer

import (
	"math"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestFirstRequestIsDueAtStart(t *testing.T) {
	s, err := New(t0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.At(0); !got.Equal(t0) {
		t.Fatalf("At(0) = %v, want %v", got, t0)
	}
}

func TestRequestsAreSpacedByTheRateInterval(t *testing.T) {
	s, _ := New(t0, 100) // one every 10ms
	if got, want := s.At(1), t0.Add(10*time.Millisecond); !got.Equal(want) {
		t.Fatalf("At(1) = %v, want %v", got, want)
	}
	if got, want := s.At(250), t0.Add(2500*time.Millisecond); !got.Equal(want) {
		t.Fatalf("At(250) = %v, want %v", got, want)
	}
}

func TestScheduleDoesNotDriftWhenRateDoesNotDivideASecond(t *testing.T) {
	// 3 rps => 333,333,333.33ns per request. Accumulating a truncated
	// interval would drift by 1ns per request; computing from the index must not.
	s, _ := New(t0, 3)
	if got, want := s.At(3), t0.Add(time.Second); !got.Equal(want) {
		t.Fatalf("At(3) = %v, want exactly %v", got, want)
	}
	const n = 3_000_000
	got := s.At(n).Sub(t0)
	want := 1_000_000 * time.Second
	if diff := (got - want).Abs(); diff > time.Nanosecond {
		t.Fatalf("At(%d) off by %v, want <=1ns (no accumulated drift)", n, diff)
	}
}

func TestInvalidRatesAreRejected(t *testing.T) {
	for _, rate := range []float64{0, -5, math.NaN(), math.Inf(1)} {
		if _, err := New(t0, rate); err == nil {
			t.Errorf("New(rate=%v) succeeded, want error", rate)
		}
	}
}
