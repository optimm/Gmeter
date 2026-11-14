package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/optimm/Gmeter/internal/mcpserver"
)

// mcpInput is where `gmeter mcp` reads the client's messages. A variable so that
// a test of run can replace it.
var mcpInput io.Reader = os.Stdin

// listFlag is a flag that can be given several times, each with a comma-separated
// list, and collects all of them.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error {
	for _, item := range strings.Split(v, ",") {
		*l = append(*l, strings.TrimSpace(item))
	}
	return nil
}

// runMCP implements `gmeter mcp`: it serves Gmeter's tools to an AI agent over
// the Model Context Protocol, on stdin and stdout, until the client goes away or
// ctx ends. Stdout carries the protocol and nothing else, so everything meant for
// the operator goes to stderr. Exit code: 0 stopped, 1 failed, 2 bad usage.
func runMCP(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gmeter mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var allow listFlag
	fs.Var(&allow, "allow", "a host the agent may load besides loopback: a name, *.example.com, an IP, a CIDR range such as 10.0.0.0/8, or * for any host; repeat it or separate with commas")
	maxRate := fs.Float64("max-rate", mcpserver.DefaultMaxRate, "the most requests per second the agent may ask for")
	maxDuration := fs.Duration("max-duration", mcpserver.DefaultMaxDuration, "the longest a run, or one trial of a search, may last")
	maxWorkers := fs.Int("max-workers", mcpserver.DefaultMaxWorkers, "the most requests in flight, which is also connections, the agent may ask for")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: gmeter mcp [flags]\n\n"+
			"Serves Gmeter to an AI agent over the Model Context Protocol, on stdin and stdout:\n"+
			"tools to load test, find the capacity of a service, check a latency target and\n"+
			"compare runs. Start it from an MCP client's configuration, not by hand.\n\n"+
			"An agent can target only loopback unless you allow more, and its load is capped.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "gmeter mcp: unexpected argument %q: it takes only flags\n", fs.Arg(0))
		return 2
	}
	policy := mcpserver.Policy{Allow: allow, MaxRate: *maxRate, MaxDuration: *maxDuration, MaxWorkers: *maxWorkers}
	if err := policy.Validate(); err != nil {
		fmt.Fprintf(stderr, "gmeter mcp: %v\n", err)
		return 2
	}

	fmt.Fprintf(stderr, "gmeter mcp %s: serving on stdin and stdout; %s\n", versionString(), policy.Describe())
	srv := mcpserver.New(mcpserver.Options{Version: versionString(), Policy: policy})
	err := srv.Run(ctx, &mcp.IOTransport{Reader: io.NopCloser(stdin), Writer: nopWriteCloser{stdout}})
	// The client going away, or being told to stop, is how this normally ends.
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "closed") {
		fmt.Fprintf(stderr, "gmeter mcp: %v\n", err)
		return 1
	}
	return 0
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
