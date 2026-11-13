package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/optimm/Gmeter/internal/agent"
	"github.com/optimm/Gmeter/internal/results"
	"github.com/optimm/Gmeter/internal/runner"
)

// addAgentsFlag registers -agents, for the commands that can split their test
// among agents: a run and a replay. A capacity search runs here only.
func addAgentsFlag(fs *flag.FlagSet, r *requestFlags) {
	r.agents = fs.String("agents", "", "run the test on these gmeter agents instead of on this machine, comma separated host:port; each sends a share of the load and the report is of them all together. Their password is read from "+tokenEnv+". There is no live progress, and an interrupted test has no report")
}

// agentClients parses -agents, if it was given (nil otherwise), and reads the
// agents' password.
func (r *requestFlags) agentClients(given bool) ([]*agent.Client, error) {
	if !given {
		return nil, nil
	}
	if *r.key != "" {
		return nil, errors.New("-key: the private key would have to be sent to every agent over plain HTTP; run without -agents, or start each agent with the certificate on it")
	}
	if r.results != nil && *r.results != "" {
		return nil, errors.New("-results: not available with -agents, which report only the whole test; run without -agents, or save each agent's own run")
	}
	var addrs []string
	seen := map[string]bool{}
	for _, a := range strings.Split(*r.agents, ",") {
		a = strings.TrimSpace(a)
		switch {
		case a == "":
			return nil, fmt.Errorf("-agents: want a comma-separated list of host:port, got %q", *r.agents)
		case seen[a]:
			return nil, fmt.Errorf("-agents: %s is listed twice; an agent runs one share at a time", a)
		}
		seen[a] = true
		addrs = append(addrs, a)
	}
	token := os.Getenv(tokenEnv)
	if len(token) < agent.MinTokenLength {
		return nil, fmt.Errorf("-agents: set %s to the agents' password, which is at least %d characters long", tokenEnv, agent.MinTokenLength)
	}
	clients := make([]*agent.Client, len(addrs))
	for i, a := range addrs {
		clients[i] = agent.NewClient(a, token)
	}
	return clients, nil
}

// agentsText says in a banner that the test runs on agents.
func agentsText(agents []*agent.Client) string {
	if agents == nil {
		return ""
	}
	return fmt.Sprintf(" across %s (no live progress: the report comes when every agent is done)", count(len(agents), "agent"))
}

// job is cfg as it is sent to agents. A TLS configuration cannot be sent, so
// the flags it was made from are.
func (r *requestFlags) job(cfg runner.Config) (agent.Job, error) {
	j := agent.Job{
		URL:         cfg.URL,
		Method:      cfg.Method,
		Header:      cfg.Header,
		Body:        cfg.Body,
		Stages:      cfg.Stages,
		Offsets:     cfg.Offsets,
		Requests:    cfg.Requests,
		Arrivals:    cfg.Arrivals.String(),
		Seed:        cfg.Seed,
		Workers:     cfg.Workers,
		Timeout:     cfg.Timeout,
		Spin:        cfg.Spin,
		Phases:      cfg.Phases,
		HTTP2:       cfg.HTTP2,
		Insecure:    *r.insecure,
		SampleEvery: cfg.SampleEvery,
		StartIn:     agentStartIn,
	}
	if *r.cacert != "" {
		data, err := os.ReadFile(*r.cacert)
		if err != nil {
			return agent.Job{}, fmt.Errorf("-cacert: %w", err)
		}
		j.CACert = data
	}
	return j, nil
}

// execute runs cfg on this machine, or shared among the agents if there are
// any. Without a result to report it returns nil and the exit code: 2 for a
// configuration that cannot run, 1 for a test on agents that failed or was
// interrupted (a part of a test is not the test, so that has no report).
func (r *requestFlags) execute(ctx context.Context, cfg runner.Config, agents []*agent.Client, name string, stderr io.Writer, scrub func(string) string) (*runner.Result, int) {
	if agents == nil && r.results != nil && *r.results != "" {
		f, err := os.Create(*r.results)
		if err != nil {
			fmt.Fprintf(stderr, "%s: -results: %v\n", name, err)
			return nil, 2
		}
		w := results.NewWriter(f)
		cfg.Sink = w.Add
		defer func() {
			if err := errors.Join(w.Close(), f.Close()); err != nil {
				fmt.Fprintf(stderr, "%s: -results: the file is incomplete: %v\n", name, err)
			}
		}()
	}
	if agents == nil {
		res, err := runner.Run(ctx, cfg)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %s\n", name, scrub(err.Error()))
			return nil, 2
		}
		if ctx.Err() != nil {
			fmt.Fprintln(stderr, "gmeter: interrupted; the report covers the partial run")
		}
		return res, 0
	}

	err := cfg.Validate()
	switch {
	case err != nil:
	case cfg.Workers < len(agents):
		err = fmt.Errorf("-workers %d is fewer than the %d agents: each agent needs at least one", cfg.Workers, len(agents))
	case cfg.Offsets != nil && len(cfg.Offsets) < len(agents):
		err = fmt.Errorf("only %s to replay for %d agents: each agent needs at least one", count(len(cfg.Offsets), "request"), len(agents))
	}
	var j agent.Job
	if err == nil {
		j, err = r.job(cfg)
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %s\n", name, scrub(err.Error()))
		return nil, 2
	}

	res, err := agent.Distribute(ctx, agents, j)
	switch {
	case err == nil:
		return res, 0
	case ctx.Err() != nil:
		fmt.Fprintln(stderr, "gmeter: interrupted; the agents have stopped, and a test run on agents has no partial report")
	default:
		fmt.Fprintf(stderr, "%s: %s\n", name, scrub(err.Error()))
	}
	return nil, 1
}
