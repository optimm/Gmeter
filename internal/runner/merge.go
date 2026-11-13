package runner

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	"github.com/optimm/Gmeter/internal/hist"
)

// Merge adds up the results of several generators that each ran a part of one
// test (the same profile at a share of the rate, or a share of a trace) into
// the result of the whole test, as if one generator had sent everything.
//
// Distributions are merged as histograms, never by combining percentiles: the
// p99 of two generators is not the average or the larger of their p99s. That
// is why every window must carry its Latency (Config.WindowHistograms). The
// inputs are not changed.
//
// Windows are lined up by index, which is by time when the generators started
// together; a stage's rate is the sum of the generators' rates.
func Merge(results ...*Result) (*Result, error) {
	if len(results) == 0 {
		return nil, errors.New("runner: nothing to merge")
	}
	first := results[0]
	for i, r := range results {
		if len(r.Stages) != len(first.Stages) {
			return nil, fmt.Errorf("runner: result %d has %d stages, result 1 has %d: they are not parts of one test", i+1, len(r.Stages), len(first.Stages))
		}
		for j, st := range r.Stages {
			if st.Stage.Duration != first.Stages[j].Stage.Duration {
				return nil, fmt.Errorf("runner: result %d's stage %d ran for %v, result 1's for %v: they are not parts of one test", i+1, j+1, st.Stage.Duration, first.Stages[j].Stage.Duration)
			}
		}
		if (r.Phases == nil) != (first.Phases == nil) {
			return nil, fmt.Errorf("runner: result %d has phases and result 1 not, or the other way round", i+1)
		}
		for j, w := range r.Timeline {
			if w.Latency == nil {
				return nil, fmt.Errorf("runner: result %d window %d has no latency distribution, so it cannot be merged (set Config.WindowHistograms)", i+1, j+1)
			}
		}
	}

	m := &Result{
		Statuses:  map[int]uint64{},
		Codes:     map[string]uint64{},
		Errors:    map[string]uint64{},
		Protocols: map[string]uint64{},
		Corrected: hist.New(),
		Service:   hist.New(),
		TimerLag:  hist.New(),
	}
	changes := map[[2]int]uint64{}
	for _, r := range results {
		m.Elapsed = max(m.Elapsed, r.Elapsed)
		m.Scheduled += r.Scheduled
		m.Dropped += r.Dropped
		m.Late += r.Late
		addCounts(m.Statuses, r.Statuses)
		addCounts(m.Codes, r.Codes)
		addCounts(m.Errors, r.Errors)
		addCounts(m.Protocols, r.Protocols)
		m.Corrected.Merge(r.Corrected)
		m.Service.Merge(r.Service)
		m.TimerLag.Merge(r.TimerLag)
		m.Logged.Compared += r.Logged.Compared
		for _, c := range r.Logged.Changes {
			changes[[2]int{c.Logged, c.Got}] += c.Count
		}
	}
	for pair, c := range changes {
		m.Logged.Changes = append(m.Logged.Changes, StatusChange{Logged: pair[0], Got: pair[1], Count: c})
	}
	slices.SortFunc(m.Logged.Changes, func(a, b StatusChange) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.Logged, b.Logged), cmp.Compare(a.Got, b.Got))
	})

	m.Timeline = mergeWindows(results)
	m.Stages = mergeStages(results)
	m.Labels = mergeLabels(results)
	if first.Phases != nil {
		m.Phases = make([]PhaseResult, len(first.Phases))
		for i, p := range first.Phases {
			m.Phases[i] = PhaseResult{Phase: p.Phase, Samples: hist.New()}
		}
		for _, r := range results {
			for i, p := range r.Phases {
				m.Phases[i].Samples.Merge(p.Samples)
			}
		}
	}
	return m, nil
}

func addCounts[K comparable](dst, src map[K]uint64) {
	for k, v := range src {
		dst[k] += v
	}
}

// mergeWindows lines the windows up by index: the i-th window of every
// generator covers the same second when they started together.
func mergeWindows(results []*Result) []Window {
	var n int
	for _, r := range results {
		n = max(n, len(r.Timeline))
	}
	out := make([]Window, n)
	for i := range out {
		w := Window{Latency: hist.New()}
		for _, r := range results {
			if i >= len(r.Timeline) {
				continue
			}
			x := r.Timeline[i]
			w.End = max(w.End, x.End)
			w.Interval = max(w.Interval, x.Interval)
			w.Partial = w.Partial || x.Partial
			w.Scheduled += x.Scheduled
			w.Responses += x.Responses
			w.Failed += x.Failed
			w.RPS += x.RPS
			w.Latency.Merge(x.Latency)
		}
		w.P50, w.P99, w.Max = w.Latency.Quantile(0.50), w.Latency.Quantile(0.99), w.Latency.Max()
		out[i] = w
	}
	return out
}

// mergeStages adds up each stage; every generator ran a share of its rate.
func mergeStages(results []*Result) []StageResult {
	out := make([]StageResult, len(results[0].Stages))
	for i, st := range results[0].Stages {
		m := StageResult{Stage: st.Stage, Corrected: hist.New(), Service: hist.New()}
		m.Stage.From, m.Stage.To = 0, 0
		var sums phaseSums
		phases := false
		for _, r := range results {
			x := r.Stages[i]
			m.Stage.From += x.Stage.From
			m.Stage.To += x.Stage.To
			m.Scheduled += x.Scheduled
			m.Dropped += x.Dropped
			m.Failed += x.Failed
			m.BadStatus += x.BadStatus
			m.Corrected.Merge(x.Corrected)
			m.Service.Merge(x.Service)
			if x.Phases != nil {
				phases = true
				sums.addTotals(x.Phases)
			}
		}
		if phases {
			m.Phases = sums.totalsOf(results[0].Stages[i].Phases)
		}
		out[i] = m
	}
	return out
}

// mergeLabels adds up the labels of the same name, busiest first.
func mergeLabels(results []*Result) []LabelResult {
	byName := map[string]*LabelResult{}
	sums := map[string]*phaseSums{}
	var names []string
	for _, r := range results {
		for _, l := range r.Labels {
			m, ok := byName[l.Label]
			if !ok {
				m = &LabelResult{Label: l.Label, Corrected: hist.New(), Service: hist.New()}
				byName[l.Label] = m
				sums[l.Label] = &phaseSums{}
				names = append(names, l.Label)
			}
			m.Scheduled += l.Scheduled
			m.Failed += l.Failed
			m.BadStatus += l.BadStatus
			m.LoggedCompared += l.LoggedCompared
			m.LoggedDiffer += l.LoggedDiffer
			m.Corrected.Merge(l.Corrected)
			m.Service.Merge(l.Service)
			if l.Phases != nil {
				sums[l.Label].addTotals(l.Phases)
				m.Phases = l.Phases // a template for the phase names; replaced below
			}
			if l.Wait != nil {
				if m.Wait == nil {
					m.Wait = hist.New()
				}
				m.Wait.Merge(l.Wait)
			}
		}
	}
	out := make([]LabelResult, 0, len(names))
	for _, name := range names {
		m := byName[name]
		if m.Phases != nil {
			m.Phases = sums[name].totalsOf(m.Phases)
		}
		out = append(out, *m)
	}
	slices.SortFunc(out, func(a, b LabelResult) int {
		return cmp.Or(cmp.Compare(b.Scheduled, a.Scheduled), cmp.Compare(a.Label, b.Label))
	})
	return out
}
