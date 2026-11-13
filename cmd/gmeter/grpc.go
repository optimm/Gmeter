package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/optimm/Gmeter/internal/grpcload"
	"github.com/optimm/Gmeter/internal/pacer"
	"github.com/optimm/Gmeter/internal/report"
	"github.com/optimm/Gmeter/internal/runner"
)

// runGRPC implements `gmeter grpc`: call one unary gRPC method at a rate or a
// profile. Exit code: 0 ran (also when interrupted, with a partial report), 1
// failed while writing the report, 2 bad usage, or a method that cannot be
// called (unreachable, unknown, streaming, or a request that does not fit it).
func runGRPC(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gmeter grpc", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		method    = fs.String("call", "", "the method to call, as package.Service/Method (required); it must be unary")
		data      = fs.String("data", "", "the request, as JSON in the protobuf mapping (default the empty message); prefix with @ to read it from a file")
		protoset  = fs.String("protoset", "", "a FileDescriptorSet with the method's schema (protoc --descriptor_set_out=FILE --include_imports); without it the server is asked, by server reflection")
		plaintext = fs.Bool("plaintext", false, "connect without TLS (HTTP/2 over cleartext), for a server that does not use TLS")
		conns     = fs.Int("conns", 1, "connections to spread the calls over; gRPC sends every call of a connection over it at once, so one connection can be a bottleneck of its own")
		rate      = fs.Float64("rate", 100, "target calls per second (open loop)")
		duration  = fs.Duration("duration", 10*time.Second, "how long to keep scheduling calls")
		stagesIn  = fs.String("stages", "", `load profile instead of -rate and -duration: comma-separated DURATION@RATE (constant) or DURATION@FROM->TO (ramp)`)
	)
	rf := addCommonFlags(fs, "even", 50)
	fs.Lookup("H").Usage = `metadata "key: value" sent with every call (repeatable)`
	fs.Lookup("insecure").Usage = "do not verify the server's TLS certificate; the run then measures a server it has not checked"
	fs.Lookup("cacert").Usage = "PEM file with the certificate authorities to trust, instead of the system's"
	fs.Lookup("workers").Usage = "max calls in flight"
	fs.Lookup("timeout").Usage = "per-call timeout; also how long connecting and reading the schema may take"
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: gmeter grpc -call package.Service/Method [flags] HOST:PORT\n\n"+
			"Calls one unary gRPC method at a rate (or a profile) with one request, and reports the\n"+
			"latency from each call's intended start, by status code. Flags must come before the address.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	usage := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "gmeter grpc: "+format+"\n", a...)
		return 2
	}
	switch n := fs.NArg(); {
	case n == 0:
		fmt.Fprintln(stderr, "gmeter grpc: missing the server's address, HOST:PORT")
		fs.Usage()
		return 2
	case n > 1:
		return usage("expected exactly one address but got %d arguments (flags must come before the address)", n)
	}
	addr := fs.Arg(0)
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	switch {
	case strings.Contains(addr, "://"):
		return usage("give the server as host:port, not a URL, got %q (-plaintext says whether it uses TLS)", addr)
	case *method == "":
		return usage("-call is required: the method to call, as package.Service/Method")
	case *conns < 1:
		return usage("-conns must be at least 1, got %d", *conns)
	case *conns > *rf.workers:
		return usage("-conns %d is more than -workers %d: at most %d calls are in flight, so a connection would sit idle", *conns, *rf.workers, *rf.workers)
	case *plaintext && (*rf.insecure || *rf.cacert != ""):
		return usage("-plaintext connects without TLS, so -insecure and -cacert have nothing to do")
	}
	stages, err := profileFlags(set, *rate, *duration, *stagesIn)
	if err != nil {
		return usage("%v", err)
	}
	arrival, seed, err := rf.arrival()
	if err != nil {
		return usage("%v", err)
	}
	md := metadata.MD{}
	for name, values := range rf.headers {
		key := strings.ToLower(name)
		if strings.HasPrefix(key, "grpc-") {
			return usage("-H %s: metadata starting with grpc- is reserved for gRPC itself", key)
		}
		md.Append(key, values...)
	}
	tlsConfig, err := rf.tlsConfig()
	if err != nil {
		return usage("%v", err)
	}
	opts := grpcload.Options{Addr: addr, Method: *method, Plaintext: *plaintext, TLS: tlsConfig, Conns: *conns, Metadata: md}
	if *data != "" {
		if opts.Data, err = readBody(*data); err != nil {
			return usage("cannot read -data: %v", err)
		}
	}
	if *protoset != "" {
		if opts.Protoset, err = os.ReadFile(*protoset); err != nil {
			return usage("-protoset: %v", err)
		}
	}

	dialCtx, cancel := context.WithTimeout(ctx, *rf.timeout)
	caller, err := grpcload.Dial(dialCtx, opts)
	cancel()
	if err != nil {
		msg := strings.TrimPrefix(err.Error(), "grpc: ")
		switch {
		case strings.Contains(msg, "certificate"):
			msg += "\n  trust its certificate authority with -cacert FILE, or skip verification with -insecure"
		case strings.Contains(msg, "TLS handshake"):
			msg += "\n  if the server does not use TLS, try -plaintext"
		}
		return usage("%s", msg)
	}
	defer caller.Close()

	cfg := runner.Config{
		URL:         addr,
		Stages:      stages,
		Arrivals:    arrival,
		Seed:        seed,
		Workers:     *rf.workers,
		Timeout:     *rf.timeout,
		Spin:        *rf.spin,
		SampleEvery: sampleEvery,
		Call:        caller.Call,
	}
	if !*rf.quiet {
		fmt.Fprintf(stderr, "gmeter: gRPC %s on %s%s%s, over %s, up to %d in flight\n",
			caller.Method(), addr, profileText(stages), arrivalsText(arrival, seed), count(*conns, "connection"), cfg.Workers)
		cfg.Progress = progressTo(stderr)
	}
	rf.noteTLS(stderr)

	started := time.Now().UTC()
	res, code := rf.execute(ctx, cfg, nil, "gmeter grpc", stderr, func(s string) string { return s })
	if res == nil {
		return code
	}
	totalDuration, expected := pacer.Totals(stages)
	summary := report.Summarize(res, targetRate(expected, totalDuration))
	summary.Run = report.RunInfo{
		URL:             addr,
		Method:          caller.Method(),
		Workers:         cfg.Workers,
		DurationSeconds: totalDuration.Seconds(),
		Profile:         profileSpec(stages),
		Arrivals:        arrival.String(),
		Seed:            seed,
		TimeoutSeconds:  cfg.Timeout.Seconds(),
		SpinSeconds:     cfg.Spin.Seconds(),
		StartedAt:       started.Format(time.RFC3339),
		Protocol:        "gRPC",
	}
	return emit(summary, *rf.jsonOut, stdout, stderr)
}
