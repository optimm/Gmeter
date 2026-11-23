package report

import (
	"github.com/optimm/Gmeter/internal/runner"
)

// JourneyInfo is the journey-specific part of a run: the scenario's counters
// and the journey-specific failure kinds, carried under the "journey" key of
// the summary and of RunInfo.
type JourneyInfo struct {
	Name  string `json:"name"` // the scenario's name
	Steps int    `json:"steps"` // the scenario's steps

	SessionsStarted   uint64            `json:"sessions_started"`
	SessionsCompleted uint64            `json:"sessions_completed"`
	SessionsAborted   uint64            `json:"sessions_aborted"`

	// Failures counts the journey-specific failures by kind: "render",
	// "check" and "extraction".
	Failures map[string]uint64 `json:"failures,omitempty"`

	// JourneyLatency is the distribution of each session's start-to-last-step
	// time, or nil if no session ran.
	JourneyLatency *Latency `json:"journey_latency,omitempty"`
}

// journeyOf reads the journey stats of a result into a JourneyInfo.
func journeyOf(j *runner.JourneyResult) *JourneyInfo {
	if j == nil {
		return nil
	}
	info := &JourneyInfo{
		Name:              j.Name,
		Steps:             j.Steps,
		SessionsStarted:   j.SessionsStarted,
		SessionsCompleted: j.SessionsCompleted,
		SessionsAborted:   j.SessionsAborted,
		Failures:          j.Failures,
	}
	if j.JourneyLatencies != nil && j.JourneyLatencies.Count() > 0 {
		lat := digest(j.JourneyLatencies)
		info.JourneyLatency = &lat
	}
	return info
}


