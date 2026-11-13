package hist

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDumpRoundTripsThroughJSONExactly(t *testing.T) {
	h := New()
	feed(50_000, h)

	raw, err := json.Marshal(h.Dump())
	if err != nil {
		t.Fatal(err)
	}
	var d Dump
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	got, err := FromDump(d)
	if err != nil {
		t.Fatal(err)
	}

	sameSummary(t, got, h)
}

func TestDumpOnlyHoldsNonEmptyBuckets(t *testing.T) {
	h := New()
	h.Record(time.Millisecond)
	h.Record(time.Millisecond)
	h.Record(5 * time.Millisecond)

	d := h.Dump()

	if len(d.Buckets) != 2 {
		t.Fatalf("Dump has %d buckets, want 2 (the histogram has 31,744)", len(d.Buckets))
	}
	var total uint64
	for _, b := range d.Buckets {
		total += b[1]
	}
	if total != 3 {
		t.Fatalf("bucket counts add up to %d, want 3", total)
	}
}

func TestEmptyHistogramDumpsAsAnEmptyListAndComesBackUsable(t *testing.T) {
	d := New().Dump()
	if d.Buckets == nil {
		t.Fatal("Buckets must be an empty list, not nil, so the JSON is [] and not null")
	}

	h, err := FromDump(d)
	if err != nil {
		t.Fatal(err)
	}
	if h.Count() != 0 || h.Min() != 0 || h.Max() != 0 {
		t.Fatalf("restored empty histogram has count=%d min=%v max=%v", h.Count(), h.Min(), h.Max())
	}
	h.Record(5 * time.Millisecond) // must still track min/max correctly afterwards
	if h.Min() != 5*time.Millisecond || h.Max() != 5*time.Millisecond {
		t.Fatalf("after one Record: min=%v max=%v, want 5ms/5ms", h.Min(), h.Max())
	}
}

func TestFromDumpRefusesDumpsItCannotTrust(t *testing.T) {
	good := func() Dump {
		h := New()
		h.Record(time.Millisecond)
		return h.Dump()
	}
	cases := map[string]func(*Dump){
		"different precision":  func(d *Dump) { d.PrecisionBits = 7 },
		"bucket index too big": func(d *Dump) { d.Buckets[0][0] = bucketCount },
		"min above max":        func(d *Dump) { d.MinNs, d.MaxNs = 9, 3 },
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			d := good()
			corrupt(&d)
			if _, err := FromDump(d); err == nil {
				t.Fatal("FromDump accepted a corrupt dump")
			}
		})
	}
}

func TestFractionAboveCountsTheShareOfSlowerSamples(t *testing.T) {
	h := New()
	for i := 1; i <= 100; i++ {
		h.Record(time.Duration(i) * time.Millisecond)
	}

	if got := h.FractionAbove(50 * time.Millisecond); got != 0.5 {
		t.Errorf("FractionAbove(50ms) = %v, want 0.5 (51..100ms are slower)", got)
	}
	if got := h.FractionAbove(100 * time.Millisecond); got != 0 {
		t.Errorf("FractionAbove(max) = %v, want 0", got)
	}
	if got := h.FractionAbove(0); got != 1 {
		t.Errorf("FractionAbove(0) = %v, want 1", got)
	}
	if got := New().FractionAbove(time.Second); got != 0 {
		t.Errorf("FractionAbove on an empty histogram = %v, want 0", got)
	}
}

func TestAHistogramEncodesAsItsDumpAndComesBackExactly(t *testing.T) {
	h := New()
	feed(50_000, h)

	raw, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var d Dump // the encoding is the dump, readable by anything that reads dumps
	if err := json.Unmarshal(raw, &d); err != nil || d.PrecisionBits != sigBits || len(d.Buckets) == 0 {
		t.Fatalf("a histogram encodes as %.80s..., want its dump (%v)", raw, err)
	}
	var got *Histogram
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	sameSummary(t, got, h)
}

func TestDecodingAHistogramRefusesADumpItCannotTrust(t *testing.T) {
	h := New()
	h.Record(time.Millisecond)
	d := h.Dump()
	d.PrecisionBits = 7
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var got *Histogram
	if err := json.Unmarshal(raw, &got); err == nil {
		t.Fatal("decoded a dump of another bucket layout")
	}
}
