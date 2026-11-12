package find

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/optimm/Gmeter/internal/report"
	"github.com/optimm/Gmeter/internal/runner"
)

// SLO is what a trial has to meet: a latency quantile within a limit, and an
// error budget. Latency is the corrected latency, measured from each request's
// intended send time, so queueing in front of a slow server counts.
type SLO struct {
	Quantile     float64       // 0.99 for p99
	Latency      time.Duration // the limit at that quantile
	MaxErrorRate float64       // failed requests plus bad statuses, as a fraction of those sent
}

// ParseSLO reads a target such as "p99=50ms" or "p99.9=200ms". It leaves
// MaxErrorRate for the caller to set.
func ParseSLO(s string) (SLO, error) {
	q, limit, ok := strings.Cut(strings.TrimSpace(s), "=")
	q = strings.TrimSpace(q)
	if !ok || len(q) < 2 || (q[0] != 'p' && q[0] != 'P') {
		return SLO{}, fmt.Errorf("invalid target %q: want a latency quantile and a limit, such as p99=50ms", s)
	}
	pct, err := strconv.ParseFloat(q[1:], 64)
	if err != nil || math.IsNaN(pct) || pct <= 0 || pct >= 100 {
		return SLO{}, fmt.Errorf("invalid quantile %q in %q: want a percentile between 0 and 100, such as p99 or p99.9", q, s)
	}
	d, err := time.ParseDuration(strings.TrimSpace(limit))
	if err != nil || d <= 0 {
		return SLO{}, fmt.Errorf("invalid latency limit %q in %q: want a positive duration such as 50ms", limit, s)
	}
	return SLO{Quantile: math.Round(pct/100*1e9) / 1e9, Latency: d}, nil // rounded: 99.9/100 is 0.9990000000000001
}

// Label is the quantile as people write it: p99, p99.9.
func (s SLO) Label() string {
	return "p" + strconv.FormatFloat(s.Quantile*100, 'g', 6, 64)
}

func (s SLO) String() string { return fmt.Sprintf("%s ≤ %v", s.Label(), s.Latency) }

// Observation is what one trial measured, for reporting.
type Observation struct {
	Rate      float64
	Scheduled uint64
	Observed  time.Duration // corrected latency at the SLO's quantile
	ErrorRate float64
	Reasons   []string // why the trial failed, or was not trusted; empty on a clean pass

	// Phases is where the measured stage's time went, or nil if phases were not
	// recorded or no request was answered.
	Phases *TrialPhases
}

// TrialPhases is the mean time an answered request of a trial spent in each
// phase, in milliseconds. A phase only some requests had (opening a connection:
// dns, connect and tls together) counts as zero for the others, so the phases
// add up to the mean corrected latency.
type TrialPhases struct {
	QueueMs    float64 `json:"queue_ms"`
	ConnectMs  float64 `json:"connect_ms"` // dns + connect + tls
	SendMs     float64 `json:"send_ms"`
	WaitMs     float64 `json:"wait_ms"`
	TransferMs float64 `json:"transfer_ms"`
}

// Evaluate judges one trial from the results of its measured stage (the
// warm-up is not part of it) and of the whole run.
//
// A trial fails if the latency target is missed, the error budget is blown, or
// requests were left unsent. It passes only if the generator also kept time: a
// trial that looks fine while the generator's timers ran late is Limited,
// because the generator, not the server, may have set the numbers. A missed
// target stays a failure whatever the generator did, since corrected latency
// is honest either way.
func Evaluate(slo SLO, rate float64, st runner.StageResult, res *runner.Result) (Verdict, Observation) {
	o := Observation{Rate: rate, Scheduled: st.Scheduled, Phases: trialPhases(st.Phases)}
	if st.Scheduled == 0 {
		o.Reasons = []string{"no requests were sent"}
		return Fail, o
	}
	o.ErrorRate = float64(st.Failed+st.BadStatus) / float64(st.Scheduled)
	if st.Corrected.Count() > 0 {
		o.Observed = st.Corrected.Quantile(slo.Quantile)
	}

	if st.Dropped > 0 {
		o.Reasons = append(o.Reasons, fmt.Sprintf("%d requests were never sent", st.Dropped))
	}
	if o.ErrorRate > slo.MaxErrorRate {
		o.Reasons = append(o.Reasons, fmt.Sprintf("errors %.1f%% over the %.1f%% budget", o.ErrorRate*100, slo.MaxErrorRate*100))
	}
	if o.Observed > slo.Latency {
		o.Reasons = append(o.Reasons, fmt.Sprintf("%s is %s, over the %s target", slo.Label(), report.FormatDuration(o.Observed), slo.Latency))
	}
	if len(o.Reasons) > 0 {
		return Fail, o
	}

	if lag := res.TimerLag.Quantile(0.99); lag > report.TimerLagWarn {
		o.Reasons = []string{fmt.Sprintf("generator timer lag p99 is %s, so this result may be the generator's and not the server's", report.FormatDuration(lag))}
		return Limited, o
	}
	return Pass, o
}

// trialPhases is the mean time per answered request in each phase of a stage,
// or nil if it has no phases or no answered request.
func trialPhases(totals []runner.PhaseTotal) *TrialPhases {
	var answered uint64
	for _, t := range totals {
		if t.Phase == "queue" { // every answered request has one
			answered = t.Requests
		}
	}
	if answered == 0 {
		return nil
	}
	p := &TrialPhases{}
	for _, t := range totals {
		ms := float64(t.Total) / float64(answered) / float64(time.Millisecond)
		switch t.Phase {
		case "queue":
			p.QueueMs = ms
		case "dns", "connect", "tls":
			p.ConnectMs += ms
		case "send":
			p.SendMs = ms
		case "wait":
			p.WaitMs = ms
		case "transfer":
			p.TransferMs = ms
		}
	}
	return p
}

// list is the phases in the order a request goes through them, with their names.
func (p TrialPhases) list() [5]struct {
	name string
	ms   float64
} {
	type phase = struct {
		name string
		ms   float64
	}
	return [5]phase{{"queue", p.QueueMs}, {"connect", p.ConnectMs}, {"send", p.SendMs}, {"wait", p.WaitMs}, {"transfer", p.TransferMs}}
}

// afterSent is the mean time a request took once Gmeter had sent it.
func (p TrialPhases) afterSent() float64 { return p.ConnectMs + p.SendMs + p.WaitMs + p.TransferMs }
