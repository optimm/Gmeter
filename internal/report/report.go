// Package report turns a runner.Result into something to read (text) or to
// store and diff later (JSON).
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/optimm/Gmeter/internal/hist"
	"github.com/optimm/Gmeter/internal/runner"
)

// Summary is the stable, serialisable view of a run. Latencies are in
// milliseconds so the JSON is easy to eyeball and to diff.
type Summary struct {
	// Version is the schema version of the file; ReadSummary refuses any other.
	Version int `json:"version"`
	// Run describes what was run, so two summaries can be checked for being
	// like-for-like. The caller fills it in; Summarize leaves it empty.
	Run RunInfo `json:"run"`

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

	// Generator describes how well Gmeter itself kept time, so you can tell a
	// slow server from a struggling load generator. Warnings explains any
	// reading that crosses a threshold; it is always a list, never null.
	Generator Generator `json:"generator"`
	Warnings  []string  `json:"warnings"`

	// Stages has one entry per stage of the load profile (a single one for a
	// constant rate), always a list. Latencies are attributed by when each
	// request was due, so a step test shows which step a server stopped
	// coping at.
	Stages []StageSummary `json:"stages"`

	// Phases splits the corrected latency of the requests that got a response
	// into where each spent its time; the key is left out when phases were not
	// recorded.
	Phases []PhaseSummary `json:"phases,omitempty"`

	// Endpoints has one entry per endpoint of a replayed trace, busiest first;
	// the key is left out of a run that has none. Its figures add up to the
	// totals above.
	Endpoints []EndpointSummary `json:"endpoints,omitempty"`

	// Timeline has one point per sample window; its points add up to the totals
	// above. Histograms holds the full latency distributions, which is what
	// makes a saved run comparable and mergeable later.
	Timeline   []Point    `json:"timeline"`
	Histograms Histograms `json:"histograms"`
}

// StageSummary is one stage of the load profile.
type StageSummary struct {
	Profile         string  `json:"profile"` // e.g. "30s@100" or "1m@100->500"
	DurationSeconds float64 `json:"duration_seconds"`
	TargetRPS       float64 `json:"target_rps"` // the stage's average rate
	Scheduled       uint64  `json:"scheduled"`
	Dropped         uint64  `json:"dropped"`
	Failed          uint64  `json:"failed"`
	Corrected       Latency `json:"latency_corrected"`
	Service         Latency `json:"latency_service"`
}

// SchemaVersion is bumped whenever the JSON layout changes incompatibly.
const SchemaVersion = 1

// RunInfo records what was run. The URL has any password redacted, and request
// headers are never recorded.
type RunInfo struct {
	URL             string  `json:"url"`
	Method          string  `json:"method"`
	Workers         int     `json:"workers"`
	DurationSeconds float64 `json:"duration_seconds"` // how long requests were scheduled for, in total
	TimeoutSeconds  float64 `json:"timeout_seconds"`
	SpinSeconds     float64 `json:"spin_seconds"` // pacing busy-wait window; 0 = plain timer (also what older files imply)
	Profile         string  `json:"profile"`      // the load profile as stages, e.g. "30s@100,30s@200"; empty in older files
	Arrivals        string  `json:"arrivals"`     // "even" or "poisson"; empty in older files, which were all even
	Seed            uint64  `json:"seed"`         // Poisson arrivals only: the seed that reproduces the arrival times
	StartedAt       string  `json:"started_at"`   // RFC 3339, UTC

	// Replay is set when the run replayed a traffic log instead of sending one
	// request over and over. In that case Method lists the methods in the trace.
	Replay *ReplayInfo `json:"replay,omitempty"`
}

// ReplayInfo says what was replayed. Request bodies and headers from the trace
// are never recorded.
type ReplayInfo struct {
	Source   string  `json:"source"`             // the trace's file name, without its directory
	Format   string  `json:"format"`             // "jsonl" or "clf"
	Requests int     `json:"requests"`           // requests in the trace after the method filter
	Timing   string  `json:"timing"`             // "recorded" (the trace's own times) or "fixed rate"
	Speed    float64 `json:"speed,omitempty"`    // recorded timing only: how much faster than recorded
	Skipped  int     `json:"skipped,omitempty"`  // requests left out because their method was not allowed
	Unparsed int     `json:"unparsed,omitempty"` // access-log lines that were not usable requests
}

// Point is one timeline window. Latencies are corrected, in milliseconds.
type Point struct {
	EndSeconds      float64 `json:"end_s"`
	IntervalSeconds float64 `json:"interval_s"`
	Partial         bool    `json:"partial"` // the closing window, shorter than the others
	Scheduled       uint64  `json:"scheduled"`
	Responses       uint64  `json:"responses"`
	Failed          uint64  `json:"failed"`
	RPS             float64 `json:"rps"`
	P50Ms           float64 `json:"p50_ms"`
	P99Ms           float64 `json:"p99_ms"`
	MaxMs           float64 `json:"max_ms"`
}

// Histograms holds both full latency distributions.
type Histograms struct {
	Corrected hist.Dump `json:"corrected"`
	Service   hist.Dump `json:"service"`
}

// Generator is the load generator's own health.
type Generator struct {
	TimerLagP99Ms float64 `json:"timer_lag_p99_ms"`
	TimerLagMaxMs float64 `json:"timer_lag_max_ms"`
	Late          uint64  `json:"late_dispatches"` // reached more than 1ms after their due time
}

// Thresholds beyond which a run gets a warning. Timers on an idle machine wake
// within microseconds, so a p99 over 5ms means the generator is being starved.
const (
	// TimerLagWarn is the timer-lag p99 above which a run gets a warning.
	TimerLagWarn     = 5 * time.Millisecond
	lateWarnFraction = 0.05
)

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
		Generator: Generator{
			TimerLagP99Ms: ms(res.TimerLag.Quantile(0.99)),
			TimerLagMaxMs: ms(res.TimerLag.Max()),
			Late:          res.Late,
		},
		Warnings:  []string{},
		Version:   SchemaVersion,
		Stages:    stageSummaries(res.Stages),
		Endpoints: endpointSummaries(res.Labels),
		Phases:    phaseSummaries(res.Phases),
		Timeline:  points(res.Timeline),
		Histograms: Histograms{
			Corrected: res.Corrected.Dump(),
			Service:   res.Service.Dump(),
		},
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

	if lag := res.TimerLag.Quantile(0.99); lag > TimerLagWarn {
		s.Warnings = append(s.Warnings, fmt.Sprintf(
			"generator timer lag p99 is %s: Gmeter is struggling to keep time on this machine, so results at this rate may be distorted; lower the rate, free up CPU, or run it somewhere quieter",
			FormatDuration(lag)))
	}
	if frac := s.lateFraction(); frac > lateWarnFraction {
		s.Warnings = append(s.Warnings, fmt.Sprintf(
			"%d of %d requests (%.0f%%) were dispatched more than 1ms after their due time: every worker was busy (raise -workers, or the server is slow) or the generator was starved of CPU; their latency still counts from the due time",
			s.Generator.Late, s.Scheduled, frac*100))
	}
	return s
}

func stageSummaries(stages []runner.StageResult) []StageSummary {
	out := make([]StageSummary, len(stages)) // never nil, so the JSON is [] and not null
	for i, st := range stages {
		out[i] = StageSummary{
			Profile:         st.Stage.String(),
			DurationSeconds: st.Stage.Duration.Seconds(),
			TargetRPS:       (st.Stage.From + st.Stage.To) / 2,
			Scheduled:       st.Scheduled,
			Dropped:         st.Dropped,
			Failed:          st.Failed,
			Corrected:       digest(st.Corrected),
			Service:         digest(st.Service),
		}
	}
	return out
}

func (s Summary) lateFraction() float64 {
	if s.Scheduled == 0 {
		return 0
	}
	return float64(s.Generator.Late) / float64(s.Scheduled)
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
	average := ""
	if len(s.Stages) > 1 {
		average = " on average" // the rate changes during the run
	}
	fmt.Fprintf(&b, "achieved   %.1f rps (target %.6g%s) over %.2fs\n", s.AchievedRPS, s.TargetRPS, average, s.ElapsedSeconds)
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
	fmt.Fprintf(&b, "generator  timer lag p99 %s, max %s   late %d (%.1f%%)\n",
		FormatDuration(time.Duration(s.Generator.TimerLagP99Ms*float64(time.Millisecond))),
		FormatDuration(time.Duration(s.Generator.TimerLagMaxMs*float64(time.Millisecond))),
		s.Generator.Late, s.lateFraction()*100)
	for _, w := range s.Warnings {
		fmt.Fprintf(&b, "warning    %s\n", w)
	}

	b.WriteString("\nlatency, corrected (from each request's intended send time)\n")
	writeLatency(&b, s.Corrected)
	b.WriteString("latency, service (from when the request was actually sent)\n")
	writeLatency(&b, s.Service)
	writePhases(&b, s.Phases)
	writeStages(&b, s.Stages)
	writeEndpoints(&b, s.Endpoints)
	writeEndpointPhases(&b, s.Endpoints)
	writeTimeline(&b, s.Timeline)

	_, err := io.WriteString(w, b.String())
	return err
}

// writeStages prints one row per stage. A single stage would only repeat the
// run's own numbers, so a constant-rate run prints nothing here.
func writeStages(b *strings.Builder, stages []StageSummary) {
	if len(stages) < 2 {
		return
	}
	width := len("stage")
	for _, st := range stages {
		width = max(width, len(st.Profile))
	}
	d := func(m float64) string { return FormatDuration(time.Duration(m * float64(time.Millisecond))) }

	b.WriteString("\nstages (corrected latency, by when each request was due)\n")
	fmt.Fprintf(b, "  %2s  %-*s  %9s  %6s  %9s  %9s  %9s  %9s\n", "#", width, "stage", "scheduled", "failed", "p50", "p90", "p99", "max")
	for i, st := range stages {
		l := st.Corrected
		p50, p90, p99, worst := "-", "-", "-", "-"
		if l.Count > 0 {
			p50, p90, p99, worst = d(l.P50Ms), d(l.P90Ms), d(l.P99Ms), d(l.MaxMs)
		}
		fmt.Fprintf(b, "  %2d  %-*s  %9d  %6d  %9s  %9s  %9s  %9s\n", i+1, width, st.Profile, st.Scheduled, st.Failed, p50, p90, p99, worst)
	}
}

func writeLatency(b *strings.Builder, l Latency) {
	if l.Count == 0 {
		b.WriteString("  no samples\n")
		return
	}
	d := func(m float64) string { return FormatDuration(time.Duration(m * float64(time.Millisecond))) }
	fmt.Fprintf(b, "  p50 %s   p90 %s   p99 %s   p99.9 %s   max %s\n",
		d(l.P50Ms), d(l.P90Ms), d(l.P99Ms), d(l.P999Ms), d(l.MaxMs))
}

func points(ws []runner.Window) []Point {
	ps := make([]Point, len(ws)) // never nil, so the JSON is [] and not null
	for i, w := range ws {
		ps[i] = Point{
			EndSeconds:      w.End.Seconds(),
			IntervalSeconds: w.Interval.Seconds(),
			Partial:         w.Partial,
			Scheduled:       w.Scheduled,
			Responses:       w.Responses,
			Failed:          w.Failed,
			RPS:             w.RPS,
			P50Ms:           ms(w.P50),
			P99Ms:           ms(w.P99),
			MaxMs:           ms(w.Max),
		}
	}
	return ps
}

// ReadSummary parses a summary written by WriteJSON. It refuses anything it
// cannot trust: not JSON, a missing or different schema version, or a
// histogram it cannot rebuild.
func ReadSummary(r io.Reader) (Summary, error) {
	var s Summary
	if err := json.NewDecoder(r).Decode(&s); err != nil {
		return Summary{}, fmt.Errorf("report: not a gmeter summary: %w", err)
	}
	switch {
	case s.Version == 0:
		return Summary{}, fmt.Errorf("report: the file has no schema version; it was not written by this gmeter, so re-run it")
	case s.Version != SchemaVersion:
		return Summary{}, fmt.Errorf("report: the file uses schema version %d but this gmeter reads version %d", s.Version, SchemaVersion)
	}
	if _, err := hist.FromDump(s.Histograms.Corrected); err != nil {
		return Summary{}, fmt.Errorf("report: corrected histogram: %w", err)
	}
	if _, err := hist.FromDump(s.Histograms.Service); err != nil {
		return Summary{}, fmt.Errorf("report: service histogram: %w", err)
	}
	return s, nil
}

// timelineWidth caps the sparklines so a long run still fits a terminal.
const timelineWidth = 60

// writeTimeline plots p99 and rps per window and names the worst window. A
// single window is not a timeline, so short runs print nothing.
func writeTimeline(b *strings.Builder, tl []Point) {
	if len(tl) < 2 {
		return
	}
	p99s := make([]float64, 0, len(tl))
	var rps []float64
	worst, lowest := tl[0], Point{RPS: math.Inf(1)}
	for _, p := range tl {
		p99s = append(p99s, p.P99Ms)
		if p.P99Ms > worst.P99Ms {
			worst = p
		}
		if !p.Partial { // a short closing window would fake a dip in rps
			rps = append(rps, p.RPS)
			if p.RPS < lowest.RPS {
				lowest = p
			}
		}
	}

	fmt.Fprintf(b, "\ntimeline (%.3gs windows)\n", tl[0].IntervalSeconds)
	fmt.Fprintf(b, "  p99  %s   worst %s in the window ending at %.1fs\n",
		sparkline(p99s, timelineWidth, math.Max),
		FormatDuration(time.Duration(worst.P99Ms*float64(time.Millisecond))), worst.EndSeconds)
	if len(rps) >= 2 {
		fmt.Fprintf(b, "  rps  %s   lowest %.0f in the window ending at %.1fs\n",
			sparkline(rps, timelineWidth, math.Min), lowest.RPS, lowest.EndSeconds)
	}
}

var sparkBlocks = []rune("▁▂▃▄▅▆▇█")

// sparkline draws values as bars scaled from 0 to the tallest value. If there
// are more values than width it shrinks them by combining neighbours with
// pick, so the caller chooses what must survive: math.Max keeps a latency
// spike, math.Min keeps a throughput dip.
func sparkline(values []float64, width int, pick func(a, b float64) float64) string {
	if len(values) == 0 {
		return ""
	}
	if width > 0 && len(values) > width {
		group := (len(values) + width - 1) / width
		var shrunk []float64
		for i := 0; i < len(values); i += group {
			v := values[i]
			for _, x := range values[i+1 : min(i+group, len(values))] {
				v = pick(v, x)
			}
			shrunk = append(shrunk, v)
		}
		values = shrunk
	}
	tallest := 0.0
	for _, v := range values {
		tallest = math.Max(tallest, v)
	}
	out := make([]rune, len(values))
	for i, v := range values {
		level := 0
		if tallest > 0 {
			level = int(math.Round(v / tallest * float64(len(sparkBlocks)-1)))
		}
		out[i] = sparkBlocks[max(0, min(level, len(sparkBlocks)-1))]
	}
	return string(out)
}

func sortedKeys[K int | string](m map[K]uint64) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// FormatDuration picks a readable unit: ns, µs, ms or s.
func FormatDuration(d time.Duration) string {
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
