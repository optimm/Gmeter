package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/optimm/Gmeter/internal/runner"
)

// pingTimeout bounds how long Distribute waits for an agent to say how it is.
var pingTimeout = 10 * time.Second

// Distribute runs j on the agents, each a share of it (see Job.Split), and
// returns their results merged: the result of j as if one generator had sent
// everything.
//
// Nothing is sent to any agent until every share has been checked here and
// every agent has said that it is free and speaks this protocol, so a test
// that cannot run everywhere runs nowhere. Once running, the first agent to
// fail stops the others, and so does cancelling ctx: a part of a test is not
// the test, so there is then no result.
//
// The agents start together if j.StartIn leaves enough time for every share to
// reach its agent; the windows of the timeline line up only as well as that.
func Distribute(ctx context.Context, agents []*Client, j Job) (*runner.Result, error) {
	jobs, err := j.Split(len(agents))
	if err != nil {
		return nil, err
	}
	for i := range jobs {
		jobs[i].Version = Version
		cfg, err := jobs[i].Config()
		if err == nil {
			err = cfg.Validate()
		}
		if err != nil {
			return nil, fmt.Errorf("the share of agent %s: %w", agents[i].base, err)
		}
	}
	if err := ready(ctx, agents); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	results := make([]*runner.Result, len(agents))
	var (
		wg    sync.WaitGroup
		once  sync.Once
		first error
	)
	for i, a := range agents {
		wg.Go(func() {
			res, err := a.Run(runCtx, jobs[i])
			if err != nil {
				// The first to fail records why before it stops the others, so an
				// error of one it stopped is never taken for the cause.
				once.Do(func() { first = err })
				stop()
				return
			}
			results[i] = res
		})
	}
	wg.Wait()
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case first != nil:
		return nil, first
	}
	return runner.Merge(results...)
}

// ready checks, all at once, that every agent answers, speaks this protocol and
// is free.
func ready(parent context.Context, agents []*Client) error {
	ctx, cancel := context.WithTimeout(parent, pingTimeout)
	defer cancel()
	errs := make([]error, len(agents))
	var wg sync.WaitGroup
	for i, a := range agents {
		wg.Go(func() {
			st, err := a.Ping(ctx)
			switch {
			case err != nil && ctx.Err() != nil: // when parent is done, Distribute says so instead
				errs[i] = fmt.Errorf("agent %s did not answer within %v", a.base, pingTimeout)
			case err != nil:
				errs[i] = err
			case st.Version != Version:
				errs[i] = fmt.Errorf("agent %s speaks protocol version %d and this controller %d: run the same gmeter build everywhere", a.base, st.Version, Version)
			case st.Busy:
				errs[i] = fmt.Errorf("agent %s is busy running another test", a.base)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}
