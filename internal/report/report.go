// Package report turns a runner.Result into something to read (text) or to
// store and diff later (JSON).
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/optimm/Gmeter/internal/hist"
	"github.com/optimm/Gmeter/internal/runner"
)

// Summary is the stable, serialisable view of a run. Latencies are in
// milliseconds so the JSON is easy to eyeball and to diff.
type Summary struct {
	ElapsedSeconds float64 `json:"elapsed_seconds"`
	TargetRPS      float64 `json:"target_rps"`
	AchievedRPS    float64 `json:"achieved_rps"` // responses per second of wall time

	Scheduled uint64 `json:"scheduled"` // dispatched to workers
	Dropped   uint64 `json:"dropped"`   // owed but never dispatched
	Responses uint64 `json:"responses"` // got an HTTP response, any status
	OK        uint64 `json:"ok"`        // responses with status 200-399
	BadStatus uint64 `json:"bad_status"`
	Failed    uint64 `json:"failed"` // transport failures: timeout, refused, ...

	Statuses map[int]uint64    `json:"statuses"`
	Errors   map[string]uint64 `json:"errors"`

	// Corrected is measured from each request's intended send time (the honest
	// number). Service is measured from when it was actually sent (what a
	// closed-loop tester would have reported).
	Corrected Latency `json:"latency_corrected"`
	Service   Latency `json:"latency_service"`
}

// Latency is a histogram digest in milliseconds.
type Latency struct {
	Count  uint64  `json:"count"`
	MinMs  float64 `json:"min_ms"`
	MeanMs float64 `json:"mean_ms"`
	P50Ms  float64 `json:"p50_ms"`
	P90Ms  float64 `json:"p90_ms"`
	P99Ms  float64 `json:"p99_ms"`
	P999Ms float64 `json:"p999_ms"`
	MaxMs  float64 `json:"max_ms"`
}

// Summarize digests a run. targetRPS is the rate the run was asked to hold.
func Summarize(res *runner.Result, targetRPS float64) Summary {
	s := Summary{
		ElapsedSeconds: res.Elapsed.Seconds(),
		TargetRPS:      targetRPS,
		Scheduled:      res.Scheduled,
		Dropped:        res.Dropped,
		Statuses:       res.Statuses,
		Errors:         res.Errors,
		Corrected:      digest(res.Corrected),
		Service:        digest(res.Service),
	}
	for code, n := range res.Statuses {
		s.Responses += n
		if code >= 200 && code < 400 {
			s.OK += n
		}
	}
	s.BadStatus = s.Responses - s.OK
	for _, n := range res.Errors {
		s.Failed += n
	}
	if s.ElapsedSeconds > 0 {
		s.AchievedRPS = float64(s.Responses) / s.ElapsedSeconds
	}
	return s
}

func digest(h *hist.Histogram) Latency {
	return Latency{
		Count:  h.Count(),
		MinMs:  ms(h.Min()),
		MeanMs: ms(h.Mean()),
		P50Ms:  ms(h.Quantile(0.50)),
		P90Ms:  ms(h.Quantile(0.90)),
		P99Ms:  ms(h.Quantile(0.99)),
		P999Ms: ms(h.Quantile(0.999)),
		MaxMs:  ms(h.Max()),
	}
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// WriteJSON writes the summary as indented JSON.
func (s Summary) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

// WriteText writes a human-readable report.
func (s Summary) WriteText(w io.Writer) error {
	var b strings.Builder

	fmt.Fprintf(&b, "requests   %d scheduled   %d ok   %d bad status   %d failed   %d dropped\n",
		s.Scheduled, s.OK, s.BadStatus, s.Failed, s.Dropped)
	fmt.Fprintf(&b, "achieved   %.1f rps (target %g) over %.2fs\n", s.AchievedRPS, s.TargetRPS, s.ElapsedSeconds)
	if len(s.Statuses) > 0 {
		codes := sortedKeys(s.Statuses)
		parts := make([]string, len(codes))
		for i, c := range codes {
			parts[i] = fmt.Sprintf("%d: %d", c, s.Statuses[c])
		}
		fmt.Fprintf(&b, "status     %s\n", strings.Join(parts, "   "))
	}
	if len(s.Errors) > 0 {
		kinds := sortedKeys(s.Errors)
		parts := make([]string, len(kinds))
		for i, k := range kinds {
			parts[i] = fmt.Sprintf("%s: %d", k, s.Errors[k])
		}
		fmt.Fprintf(&b, "errors     %s\n", strings.Join(parts, "   "))
	}

	b.WriteString("\nlatency, corrected (from each request's intended send time)\n")
	writeLatency(&b, s.Corrected)
	b.WriteString("latency, service (from when the request was actually sent)\n")
	writeLatency(&b, s.Service)

	_, err := io.WriteString(w, b.String())
	return err
}

func writeLatency(b *strings.Builder, l Latency) {
	if l.Count == 0 {
		b.WriteString("  no samples\n")
		return
	}
	d := func(m float64) string { return formatDuration(time.Duration(m * float64(time.Millisecond))) }
	fmt.Fprintf(b, "  p50 %s   p90 %s   p99 %s   p99.9 %s   max %s\n",
		d(l.P50Ms), d(l.P90Ms), d(l.P99Ms), d(l.P999Ms), d(l.MaxMs))
}

func sortedKeys[K int | string](m map[K]uint64) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// formatDuration picks a readable unit: ns, µs, ms or s.
func formatDuration(d time.Duration) string {
	switch {
	case d < time.Microsecond:
		return fmt.Sprintf("%dns", d.Nanoseconds())
	case d < time.Millisecond:
		return fmt.Sprintf("%.0fµs", float64(d)/float64(time.Microsecond))
	case d < time.Second:
		return fmt.Sprintf("%.1fms", ms(d))
	default:
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
}
