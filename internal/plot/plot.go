// Package plot turns a results file (see package results) into one HTML page of
// charts: latency over time by both clocks, load over time, the latency
// distribution, and what failed. The page has no outside dependencies, so it
// can be attached to a ticket or opened offline.
package plot

import (
	"errors"
	"io"
	"strconv"
	"time"

	"github.com/optimm/Gmeter/internal/hist"
	"github.com/optimm/Gmeter/internal/results"
	"github.com/optimm/Gmeter/internal/runner"
)

// Stats summarizes a set of latencies, in milliseconds.
type Stats struct {
	Count uint64  `json:"count"`
	Mean  float64 `json:"mean"`
	P50   float64 `json:"p50"`
	P90   float64 `json:"p90"`
	P99   float64 `json:"p99"`
	P999  float64 `json:"p999"`
	Max   float64 `json:"max"`
}

// Bucket is one slice of the run's time, holding the requests that were due in it.
type Bucket struct {
	Start     float64 `json:"start"`  // seconds from the start of the run
	Due       uint64  `json:"due"`    // requests due in it
	OK        uint64  `json:"ok"`     // of them, answered
	Failed    uint64  `json:"failed"` // of them, failed
	Corrected Stats   `json:"corrected"`
	Service   Stats   `json:"service"`
}

// Point is one point of the latency distribution: a fraction of requests and
// the latency under which that fraction finished, by each clock.
type Point struct {
	Quantile  float64 `json:"q"`
	Corrected float64 `json:"corrected"`
	Service   float64 `json:"service"`
}

// Data is everything the page draws.
type Data struct {
	Title         string            `json:"title"`
	Span          float64           `json:"span"` // seconds, to the last request's due time
	BucketSeconds float64           `json:"bucket_seconds"`
	Total         uint64            `json:"total"`
	OK            uint64            `json:"ok"`
	Failed        uint64            `json:"failed"`
	Buckets       []Bucket          `json:"buckets"`
	Corrected     Stats             `json:"corrected"`
	Service       Stats             `json:"service"`
	Curve         []Point           `json:"curve"`
	Failures      map[string]uint64 `json:"failures"`
	Statuses      map[string]uint64 `json:"statuses"`
}

// quantiles are the points of the distribution curve.
var quantiles = []float64{0, 0.25, 0.5, 0.75, 0.9, 0.95, 0.99, 0.995, 0.999, 0.9999, 1}

// timed reports whether a request has a latency worth recording: it got an
// answer, or it timed out, which is the time it waited. Other failures end at
// once, and counting them would make a dead server look fast.
func timed(s runner.Sample) bool { return s.Failure == "" || s.Failure == "timeout" }

type tally struct{ corrected, service *hist.Histogram }

func (t *tally) add(s runner.Sample) {
	if t.corrected == nil {
		t.corrected, t.service = hist.New(), hist.New()
	}
	t.corrected.Record(s.Finished - s.Due)
	t.service.Record(s.Finished - s.Sent)
}

func stats(h *hist.Histogram) Stats {
	if h == nil || h.Count() == 0 {
		return Stats{}
	}
	return Stats{Count: h.Count(), Mean: ms(h.Mean()), P50: ms(h.Quantile(0.5)), P90: ms(h.Quantile(0.9)),
		P99: ms(h.Quantile(0.99)), P999: ms(h.Quantile(0.999)), Max: ms(h.Max())}
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// Summarize reads a results file twice, first for how long the run was, then to
// sort its requests into at most maxBuckets slices of that time. It keeps one
// histogram per slice, not the requests, so a file of any size takes bounded
// memory.
func Summarize(f io.ReadSeeker, maxBuckets int) (*Data, error) {
	if maxBuckets < 1 {
		return nil, errors.New("plot: want at least one bucket")
	}
	var last time.Duration
	var n uint64
	if err := results.Read(f, func(s runner.Sample) {
		n++
		last = max(last, s.Due)
	}); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, errors.New("the file has no requests in it")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	width := max((last+time.Microsecond)/time.Duration(maxBuckets), time.Millisecond)
	d := &Data{
		Span:          last.Seconds(),
		BucketSeconds: width.Seconds(),
		Buckets:       make([]Bucket, int(last/width)+1),
		Failures:      map[string]uint64{},
		Statuses:      map[string]uint64{},
	}
	for i := range d.Buckets {
		d.Buckets[i].Start = (time.Duration(i) * width).Seconds()
	}
	tallies := make([]tally, len(d.Buckets))
	var all tally
	err := results.Read(f, func(s runner.Sample) {
		i := min(int(s.Due/width), len(d.Buckets)-1)
		b := &d.Buckets[i]
		d.Total++
		b.Due++
		if s.Failure != "" {
			d.Failed++
			b.Failed++
			d.Failures[s.Failure]++
		} else {
			d.OK++
			b.OK++
			status := s.Code
			if s.Status != 0 {
				status = strconv.Itoa(s.Status)
			}
			d.Statuses[status]++
		}
		if timed(s) {
			tallies[i].add(s)
			all.add(s)
		}
	})
	if err != nil {
		return nil, err
	}
	for i := range d.Buckets {
		d.Buckets[i].Corrected, d.Buckets[i].Service = stats(tallies[i].corrected), stats(tallies[i].service)
	}
	d.Corrected, d.Service = stats(all.corrected), stats(all.service)
	if all.corrected != nil {
		for _, q := range quantiles {
			d.Curve = append(d.Curve, Point{Quantile: q, Corrected: ms(all.corrected.Quantile(q)), Service: ms(all.service.Quantile(q))})
		}
	}
	return d, nil
}
