package mcpserver

import (
	"strings"
	"testing"

	"github.com/optimm/Gmeter/internal/report"
)

func TestResultOfUsesTheGeneratorsOwnReason(t *testing.T) {
	const reason = "queue of 498ms with 5 workers can be explained only by the generator"
	sum := report.Summary{
		Statuses: map[int]uint64{},
		Errors:   map[string]uint64{},
		Warnings: []string{reason},
		Generator: report.Generator{
			Limited: true,
			Reason:  reason,
		},
	}

	got := resultOf(sum)

	if !got.GeneratorLimited || got.Verdict != "generator_limited" || got.GeneratorReason != reason {
		t.Fatalf("generator verdict = %v/%q, reason %q, want generator_limited and %q", got.GeneratorLimited, got.Verdict, got.GeneratorReason, reason)
	}
	if !strings.Contains(got.Summary, reason) || strings.Contains(got.Summary, "timer lag p99") {
		t.Fatalf("summary does not give the generator's reason: %q", got.Summary)
	}
}

func TestResultOfDoesNotMarkASoundRunLimited(t *testing.T) {
	sum := report.Summary{
		Statuses: map[int]uint64{200: 100},
		Errors:   map[string]uint64{},
		Warnings: []string{},
		Generator: report.Generator{
			TimerLagP99Ms: 0.1,
		},
	}

	got := resultOf(sum)

	if got.GeneratorLimited || got.GeneratorReason != "" || got.Verdict != "ok" {
		t.Fatalf("sound run was marked limited: %+v", got)
	}
}
