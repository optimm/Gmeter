package mcpserver

import (
	"crypto/rand"
	"encoding/hex"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/optimm/Gmeter/internal/report"
)

// Options configures the server.
type Options struct {
	// Version is the Gmeter version, reported to the client.
	Version string
	// Policy says what an agent may target and how much load it may ask for.
	Policy Policy
}

// New returns an MCP server with Gmeter's tools. Serve it with Run on a
// transport, such as mcp.StdioTransport.
func New(opts Options) *mcp.Server {
	if opts.Version == "" {
		opts.Version = "dev"
	}
	s := &service{policy: opts.Policy.withDefaults(), runs: newRunStore()}
	srv := mcp.NewServer(&mcp.Implementation{Name: "gmeter", Title: "Gmeter load tester", Version: opts.Version}, &mcp.ServerOptions{
		Instructions: instructions,
	})
	s.addTools(srv)
	return srv
}

// instructions is what the client shows the model about the server as a whole.
const instructions = `Gmeter load-tests HTTP and unary gRPC services and reports honest latency.

Latency is measured from when each request was due, not when it was sent, so a server that stalls cannot hide
the requests that queued behind the stall (coordinated omission). Read "corrected" latency as what users saw
and "service" latency as what a closed-loop tester would have reported.

If a result says generator_limited, the load generator could not keep time and the numbers are not the
server's: do not draw conclusions from the latency, run it again with a lower rate or fewer workers.

By default only loopback targets are allowed, with ceilings on rate, duration and workers; a refusal says
which flag the operator can use to change it. Ask the operator, never work around a refusal.`

// service holds what the tools share.
type service struct {
	policy Policy
	runs   *runStore
}

// runStore keeps the summaries of the runs this server did, so that a later call
// can refer to them by id.
type runStore struct {
	mu   sync.Mutex
	runs map[string]report.Summary
	ids  []string // in the order they were made
}

func newRunStore() *runStore { return &runStore{runs: map[string]report.Summary{}} }

// add stores a summary and returns the id it can be fetched by.
func (r *runStore) add(s report.Summary) string {
	var b [4]byte
	rand.Read(b[:])
	id := "run-" + hex.EncodeToString(b[:])
	r.mu.Lock()
	r.runs[id] = s
	r.ids = append(r.ids, id)
	r.mu.Unlock()
	return id
}

func (r *runStore) get(id string) (report.Summary, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.runs[id]
	return s, ok
}
