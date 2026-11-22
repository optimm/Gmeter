package runner

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/optimm/Gmeter/internal/hist"
	"github.com/optimm/Gmeter/internal/pacer"
)

// Journey is a compiled stateful scenario: one session, run by a worker per
// job. The journey package compiles a scenario file into it; see
// internal/journey.
//
// A session starts at the job's scheduled time. Its steps run in order, one
// request at a time, all still due at their own times: a step's due time is
// the previous step's finish plus its Think. Latency is measured from those
// due times, so a step is charged for itself and the generator's hand-off
// after the previous one, not for the session's whole wait.
type Journey struct {
	Name  string
	Steps []JourneyStep
}

// JourneyStep is one step of a journey session.
type JourneyStep struct {
	Label string // the step's name; its tally is Result.Labels by this name
	Method string // the step's HTTP method, as the runner sends it
	Think  time.Duration

	// Render builds the request for one session before anything is sent: the
	// method, the target (a path, so it never reaches into another host), the
	// headers and the body. vars is the session's variable map. A non-nil
	// error aborts the session before the request: it is not sent.
	Render func(vars map[string]any) (Method string, Target string, Header http.Header, Body []byte, err error)

	// Check reports whether the response satisfies the step; a non-nil error
	// is the session's check failure, counted in the journey's failures. The
	// latency still counts: the server did take that long to answer.
	Check func(vars map[string]any, status int, header http.Header, body []byte) error

	// Extract updates vars from the response. A non-nil error is an
	// extraction failure and ends the session.
	Extract func(vars map[string]any, header http.Header, body []byte) error

	// ContinueOnCheckFailure says the session carries on after a failed
	// check instead of ending at that step.
	ContinueOnCheckFailure bool
}

// JourneyResult is what a journey run did: counts of sessions, and the
// distribution of how long each session took, from its scheduled start to its
// final step's finish.
type JourneyResult struct {
	Name  string
	Steps int

	SessionsStarted   uint64
	SessionsCompleted uint64
	SessionsAborted   uint64

	// Failures counts the journey-specific failures by kind: "render",
	// "check" and "extraction". Transport failures are in Result.Errors, as
	// for a plain run.
	Failures map[string]uint64

	// JourneyLatencies is the per-session distribution of start-to-final-step
	// time; nil if no session ran.
	JourneyLatencies *hist.Histogram
}

// journeyStats accumulates a JourneyResult from the workers, which all write
// it concurrently.
type journeyStats struct {
	mu        sync.Mutex
	completed uint64
	aborted   uint64
	failures  map[string]uint64
	durations *hist.Histogram
}

func newJourneyStats() *journeyStats {
	return &journeyStats{failures: map[string]uint64{}, durations: hist.New()}
}

func (s *journeyStats) complete() {
	s.mu.Lock()
	s.completed++
	s.mu.Unlock()
}

func (s *journeyStats) abort() {
	s.mu.Lock()
	s.aborted++
	s.mu.Unlock()
}

func (s *journeyStats) fail(kind string) {
	s.mu.Lock()
	s.failures[kind]++
	s.mu.Unlock()
}

func (s *journeyStats) duration(d time.Duration) {
	s.mu.Lock()
	s.durations.Record(d)
	s.mu.Unlock()
}

func (s *journeyStats) result(jn *Journey, started uint64) *JourneyResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &JourneyResult{
		Name: jn.Name, Steps: len(jn.Steps),
		SessionsStarted:   started,
		SessionsCompleted: s.completed,
		SessionsAborted:   s.aborted,
		Failures:          s.failures,
		JourneyLatencies:  s.durations,
	}
}

// journeyBodyLimit bounds how much of a response a journey check or extract
// sees: a larger body is a check failure, not unbounded memory.
const journeyBodyLimit = 1 << 20

// journeyTally returns the shared tally of one step's label.
func (w *worker) journeyTally(name string) *partTally { return w.jt[name] }

// doJourney runs one session: the job is a session start, and the worker runs
// the journey's steps one at a time, recording each step as a labelled
// request. Steps share the run's machinery, so a journey's per-step latency,
// failures and phases are the run's, by step label.
func (w *worker) doJourney(ctx context.Context, j job) {
	jn := w.cfg.Journey
	var lastFinished time.Time
	defer func() {
		if len(jn.Steps) > 0 && !lastFinished.IsZero() {
			w.jstats.duration(lastFinished.Sub(j.at))
		}
	}()
	vars := map[string]any{"session": j.seq}
	var finished time.Time // the finish of the step before the one being run
	for i, st := range jn.Steps {
		due := j.at
		if i > 0 {
			due = finished.Add(st.Think) // after the last step finished
		}
		if err := pacer.WaitUntil(ctx, due, 0); err != nil {
			w.jstats.abort()
			return
		}
		method, target, header, body, err := st.Render(vars)
		if err != nil {
			w.journeyFail(j, st.Label) // the step failed to render; the session aborts
			w.jstats.fail("render")
			w.jstats.abort()
			return
		}
		w.journeyTally(st.Label).sched()
		status, proto, respHeader, respBody, sent, fin, split, err := w.doJourneyStep(ctx, method, target, header, body, due)
		finished = fin
		lastFinished = fin
		if err != nil {
			kind := classify(err)
			if ctx.Err() != nil {
				kind = "canceled"
			}
			w.t.errors[kind]++
			w.live.failed.Add(1)
			w.stages[j.stage].fail()
			w.journeyTally(st.Label).fail()
			w.jstats.abort()
			return
		}
		corrected, service := finished.Sub(due), finished.Sub(sent)
		w.t.corrected.Record(corrected)
		w.t.service.Record(service)
		w.stages[j.stage].record(corrected, service)
		w.journeyTally(st.Label).record(corrected, service)
		w.t.statuses[status]++
		w.t.protocols[proto]++
		w.live.completed.Add(1)
		if status < 200 || status >= 400 {
			w.stages[j.stage].bad()
			w.journeyTally(st.Label).bad()
		}
		if w.phases != nil {
			w.phases.record(split)
			w.stages[j.stage].recordPhases(split)
			w.journeyTally(st.Label).recordLabelPhases(split)
		}
		checkFailed := false
		if st.Check != nil {
			if err := st.Check(vars, status, respHeader, respBody); err != nil {
				checkFailed = true
				w.journeyTally(st.Label).fail()
				w.stages[j.stage].fail()
				w.jstats.fail("check")
				w.jstats.abort()
				if !st.ContinueOnCheckFailure {
					return
				}
			}
		}
		if st.Extract != nil && !checkFailed {
			if err := st.Extract(vars, respHeader, respBody); err != nil {
				w.journeyTally(st.Label).fail()
				w.stages[j.stage].fail()
				w.jstats.fail("extraction")
				w.jstats.abort()
				return
			}
		}
		w.emitJourneyStep(j, st.Label, due, sent, finished, status)
	}
	w.jstats.complete()
}

// doJourneyStep sends one step's request and returns its outcome: status and
// protocol of the response, its headers and body (capped), the finish time
// and, when phases are on, the step's phase split.
func (w *worker) doJourneyStep(ctx context.Context, method, target string, header http.Header, body []byte, due time.Time) (int, string, http.Header, []byte, time.Time, time.Time, phaseTimes, error) {
	reqCtx, cancel := context.WithTimeout(ctx, w.cfg.Timeout)
	defer cancel()
	var payload io.Reader
	if len(body) > 0 {
		payload = bytes.NewReader(body)
	}
	if w.cfg.base != "" && !strings.Contains(target, "://") {
		target = w.cfg.base + target // the step's path is relative to the base URL
	}
	req, err := http.NewRequestWithContext(reqCtx, method, target, payload)
	if err != nil { // validated by the compiler; defensive only
		return 0, "", nil, nil, time.Time{}, time.Time{}, phaseTimes{}, fmt.Errorf("journey step %q: invalid request: %w", target, err)
	}
	req.Header = w.cfg.Header.Clone()
	for name, values := range header {
		req.Header.Del(name) // a name the step gives replaces the global one
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	var clock *phaseClock
	if w.phases != nil {
		clock = newPhaseClock(time.Now)
		reqCtx = httptrace.WithClientTrace(reqCtx, clock.trace())
	}
	sent := time.Now()
	resp, err := w.client.Do(req)
	var status int
	var proto string
	var respHeader http.Header
	var respBody []byte
	if err == nil {
		status, proto = resp.StatusCode, resp.Proto
		respHeader = resp.Header
		respBody, _ = io.ReadAll(io.LimitReader(resp.Body, journeyBodyLimit+1))
		resp.Body.Close()
	}
	finished := time.Now()
	var split phaseTimes
	if clock != nil {
		split = clock.split(due, sent, finished)
	}
	return status, proto, respHeader, respBody, sent, finished, split, err
}

// journeyFail counts a step as failed: nothing was sent or answered, so the
// session ends at the step.
func (w *worker) journeyFail(j job, label string) {
	w.stages[j.stage].fail()
	w.journeyTally(label).fail()
}

// emitJourneyStep hands the step's outcome to Config.Sink, as emit does for
// plain requests: the label is the step's name.
func (w *worker) emitJourneyStep(j job, label string, due, sent, finished time.Time, status int) {
	if w.cfg.Sink == nil {
		return
	}
	w.cfg.Sink(Sample{
		Seq:      j.seq,
		Stage:    j.stage,
		Label:    label,
		Due:      due.Sub(w.start),
		Sent:     sent.Sub(w.start),
		Finished: finished.Sub(w.start),
		Status:   status,
	})
}

// journeyLabels builds Result.Labels for a journey run: one row per step,
// busiest first, from the shared tallies of the steps' labels.
func journeyLabels(jn *Journey, jt map[string]*partTally, phases bool) []LabelResult {
	out := make([]LabelResult, 0, len(jn.Steps))
	for _, st := range jn.Steps {
		t := jt[st.Label]
		if t == nil { // a step no session reached
			t = &partTally{}
		}
		r := LabelResult{
			Label:     st.Label,
			Scheduled: t.scheduled,
			Failed:    t.failed,
			BadStatus: t.badStatus,
			Corrected: t.corrected,
			Service:   t.service,
		}
		if phases {
			r.Phases = t.phases.totals()
			r.Wait = t.wait
			if r.Wait == nil {
				r.Wait = hist.New()
			}
		}
		if r.Corrected == nil {
			r.Corrected, r.Service = hist.New(), hist.New()
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b LabelResult) int {
		return cmp.Or(cmp.Compare(b.Scheduled, a.Scheduled), cmp.Compare(a.Label, b.Label))
	})
	return out
}
