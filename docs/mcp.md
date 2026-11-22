# Using it from an AI agent

Back to the [README](../README.md).

## Using it from an AI agent (`gmeter mcp`)

`gmeter mcp` serves Gmeter over the [Model Context Protocol](https://modelcontextprotocol.io), on stdin and stdout, so
an AI agent can load test a service and read an answer it can rely on. It is the Go package above behind four
tools, in the same binary: nothing more to install.

Add it to your client:

```
claude mcp add gmeter -- gmeter mcp
```

or, for a client with a JSON configuration, such as Claude Desktop:

```json
{
  "mcpServers": {
    "gmeter": { "command": "gmeter", "args": ["mcp"] }
  }
}
```

With Docker, use `"command": "docker"` and `"args": ["run", "-i", "--rm", "ghcr.io/optimm/gmeter", "mcp"]`. Inside a
container "loopback" is the container, not your machine, so to reach a service on your machine add
`"-allow", "host.docker.internal"` after `"mcp"` and use that name in the URLs (on Linux also pass
`--add-host=host.docker.internal:host-gateway` to `docker run`).

### The tools

| Tool | What it does |
|---|---|
| `load_test` | Sends a constant or shaped load (`rate` and `duration`, or `stages`) at an HTTP URL or a gRPC method (unary or streaming), and reports the result. |
| `find_capacity` | Finds the highest rate that meets a target such as `p99=50ms`, like `gmeter find`. |
| `check_slo` | Runs a load and answers yes or no against a target, with the reasons it was missed. |
| `compare_runs` | Compares two runs made in the session, by `run_id`, and flags a p99 regression. |

A run's full report, with its latency histograms, can be read as the resource `gmeter://runs/{run_id}`. Runs are kept
only as long as the server process runs.

Every result starts with a `summary` in a sentence or two and a `verdict` of `ok`, `errors` or `generator_limited`, then the
counts, the statuses and errors, and the **corrected** latency (from when each request was due, what users saw) next to
the **service** latency (from when it was sent, what a closed-loop tester would report). `generator_limited` means
Gmeter's own timers ran late, so the latencies describe the generator and not the server; the tool descriptions tell
the agent to run it again, lower, rather than conclude anything from it. `check_slo` and `find_capacity` do not pass
such a run, for the same reason `gmeter find` does not. Long calls send progress updates if the client asks for them.

### What the agent may do

An agent that can send load is not the owner of what it sends it to, so the server starts closed:

- **Targets.** Only loopback (`localhost`, `127.0.0.0/8`, `::1`) unless you allow more with `-allow`: a host name,
  `*.example.com` for subdomains, an IP, a CIDR range such as `10.0.0.0/8`, or `*` for any host. The flag can be
  repeated or take a comma-separated list. A name is resolved first, so a name that points at a forbidden address is
  refused like the address. Cloud metadata addresses (`169.254.0.0/16`, `fd00:ec2::254`, `metadata.google.internal`)
  are refused whatever you allow.
- **Load.** At most `-max-rate` requests per second (default 1000), `-max-duration` per run or per trial of a search
  (default 2m) and `-max-workers` in flight (default 200). A search never goes above `-max-rate`.
- **Refusals say how to change them.** A refused call names the flag, and the server's instructions tell the agent to
  ask you instead of working around it.

```
claude mcp add gmeter -- gmeter mcp -allow staging.example.com -max-rate 500
```

The policy in force is printed to stderr when the server starts. Stdout carries only the protocol.

### What it leaves out

- Replaying a traffic log and mutual TLS are not offered as tools yet, and neither is a gRPC descriptor set: gRPC
  schemas come from server reflection.
- The headers and body an agent passes are sent as given. Gmeter does not record them in a run's report, but they are in
  the agent's own context, so do not give an agent a credential you would not give its transcript.
- The limits stop one call from doing much harm, not many calls from adding up: an agent that asks again and again can
  send as much as it likes in total. Stop the server to stop it.
