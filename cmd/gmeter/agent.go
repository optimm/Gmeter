package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/optimm/Gmeter/internal/agent"
)

// tokenEnv is the environment variable that holds the agents' password, on
// the agents and on the controller alike. It is not a flag: a flag's value is
// in the process list for anyone on the machine to read.
const tokenEnv = "GMETER_TOKEN"

// defaultAgentListen keeps an agent unreachable from the network until it is
// asked to be.
const defaultAgentListen = "127.0.0.1:7070"

// runAgent implements `gmeter agent [-listen ADDR]`: it serves jobs from a
// controller until ctx ends. Exit code: 0 stopped, 1 cannot listen, 2 bad
// usage or no usable password.
func runAgent(ctx context.Context, args []string, _, stderr io.Writer) int {
	fs := flag.NewFlagSet("gmeter agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", defaultAgentListen, "address to take jobs on; the default is reachable only from this machine, use e.g. 0.0.0.0:7070 for other machines")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: %s=PASSWORD gmeter agent [-listen ADDR]\n\n"+
			"Runs the jobs a controller (gmeter -agents, gmeter replay -agents) sends, one at a time,\n"+
			"for callers that know PASSWORD. The password and the traffic are not encrypted:\n"+
			"run agents on your own machines, on a network you trust.\n\n", tokenEnv)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "gmeter agent: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	var mu sync.Mutex
	logf := func(format string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(stderr, "%s gmeter agent: %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, a...))
	}
	srv, err := agent.NewServer(os.Getenv(tokenEnv), logf)
	if err != nil {
		fmt.Fprintf(stderr, "gmeter agent: set %s: %v\n", tokenEnv, err)
		return 2
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintf(stderr, "gmeter agent: cannot listen on %s: %v\n", *listen, err)
		return 1
	}
	addr := ln.Addr().(*net.TCPAddr)
	logf("listening on %s (protocol version %d)", addr, agent.Version)
	if !addr.IP.IsLoopback() {
		logf("reachable from the network: the password and the traffic are not encrypted, so only do this on a network you trust")
	}

	hs := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second, // no WriteTimeout: the answer comes when the run is over
		ErrorLog:          log.New(stderr, "gmeter agent: ", 0),
	}
	served := make(chan error, 1)
	go func() { served <- hs.Serve(ln) }()
	select {
	case <-ctx.Done():
		hs.Close() // ends a running job too: its controller is told the connection broke
		<-served
		logf("stopped")
		return 0
	case err := <-served:
		fmt.Fprintf(stderr, "gmeter agent: %v\n", err)
		return 1
	}
}
