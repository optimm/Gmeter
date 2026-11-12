package find

import (
	"context"
	"time"

	"github.com/optimm/Gmeter/internal/pacer"
	"github.com/optimm/Gmeter/internal/runner"
)

// RunTrial runs one trial at rate: a warm-up stage, then a measured stage at the
// same rate, and judges the measured stage against slo. The warm-up takes the
// cold-start cost (new connections, cold caches) so that it does not count
// against the server. base supplies the URL, request and workers; its load
// profile, seed and progress callback are replaced.
//
// If ctx is cancelled the trial is abandoned with ctx.Err(), not judged: a
// verdict computed from half a trial would be a wrong answer.
func RunTrial(ctx context.Context, base runner.Config, slo SLO, warmup, measure time.Duration, seed uint64, rate float64) (Verdict, Observation, error) {
	cfg := base
	cfg.Rate, cfg.Duration, cfg.Progress, cfg.Seed = 0, 0, nil, seed
	cfg.Stages = nil
	if warmup > 0 {
		cfg.Stages = append(cfg.Stages, pacer.Stage{Duration: warmup, From: rate, To: rate})
	}
	cfg.Stages = append(cfg.Stages, pacer.Stage{Duration: measure, From: rate, To: rate})

	res, err := runner.Run(ctx, cfg)
	if err != nil {
		return Fail, Observation{}, err
	}
	if err := ctx.Err(); err != nil {
		return Fail, Observation{}, err
	}
	v, o := Evaluate(slo, rate, res.Stages[len(res.Stages)-1], res)
	return v, o, nil
}
