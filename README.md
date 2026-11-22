<p align="center">
  <img src="docs/assets/gmeter-banner.jpg" alt="Gmeter: a gopher in a chef's hat at a load-testing console, sending a stream of requests to a rack of servers. HTTP load testing in Go." width="100%">
</p>

# Gmeter

An agent-friendly, open-loop load tester for HTTP and gRPC, in Go, that reports **honest latency** and tells you
when the generator itself is the problem. It runs from the command line, from a Go test, and as an MCP server
for an AI agent.

Website: [optimm.github.io/Gmeter](https://optimm.github.io/Gmeter/)

Most load testers send a request, wait for the reply, then send the next one. When the server stalls, they
stop sending, so the requests that *would have* arrived during the stall are never measured and the p99 looks
fine while real users suffer. This is **coordinated omission**.

Gmeter schedules requests by the clock instead. Request *n* is due at `start + n / rate` no matter what the
server is doing, and latency is measured from that **intended** send time.


## Install

```bash
brew install optimm/tap/gmeter                      # macOS and Linux, with Homebrew
go install github.com/optimm/Gmeter/cmd/gmeter@latest  # or with Go 1.26 or newer
gmeter -version
```

Prebuilt archives for macOS, Linux and Windows (amd64 and arm64) are on the
[releases page](https://github.com/optimm/Gmeter/releases); the Windows ones are `.zip` files holding `gmeter.exe`.

There is also an image, for a CI job or a machine with no Go:

```bash
docker run --rm ghcr.io/optimm/gmeter -rate 100 -duration 10s http://host.docker.internal:8080/
```

The image runs as a non-root user and holds only the binary; [running a test](docs/running.md#running-in-docker) has the
details, including how to keep a result file.

## Quick start

```bash
go build -o gmeter ./cmd/gmeter

gmeter -rate 2000 -duration 60s -workers 64 http://localhost:8080/health

gmeter -rate 500 -duration 30s -method POST \
  -H 'Content-Type: application/json' -H 'Authorization: Bearer ...' \
  -body @order.json http://localhost:8080/v1/orders

# a step test: where does it stop coping?
gmeter -stages '30s@200,30s@400,30s@800,30s@1600' http://localhost:8080/health

# bursty, random arrivals at the same average rate
gmeter -rate 600 -duration 60s -arrivals poisson http://localhost:8080/health

# what is the most it can take while keeping p99 under 50ms?
gmeter find -slo p99=50ms http://localhost:8080/health

# send the requests of a real traffic log, at the pace they were recorded
gmeter replay -file access.log http://staging:8080

# call a unary gRPC method, found by server reflection
gmeter grpc -plaintext -call shop.Orders/Get -data '{"id":"42"}' -rate 1000 orders:9090
```

Flags must come before the URL. The ones you will reach for first:

| Flag | Default | Meaning |
|---|---|---|
| `-rate` | `100` | target requests per second (open loop) |
| `-duration` | `10s` | how long requests keep being scheduled |
| `-stages` | | a load profile instead of `-rate` and `-duration`: steps and ramps |
| `-arrivals` | `even` | `even` (exact spacing) or `poisson` (random bursts at the same average rate) |
| `-workers` | `50` | max requests in flight, which is also the max connection count |
| `-timeout` | `10s` | per-request timeout |
| `-method`, `-H`, `-body` | `GET` | the request to send; `-body @file` reads the body from a file |
| `-json`, `-results` | | save a JSON summary, or every request as a CSV, to a path |
| `-prometheus` | | save the run's metrics in Prometheus/OpenMetrics text format, for a dashboard (`-` prints them instead of the report) |

Every flag, with HTTP/2 and TLS, is in [running a test](docs/running.md#flags). Ctrl-C stops a run early and still
prints the report for what ran.

## What it looks like

Against a throwaway server that answers instantly except for a 1-second freeze starting 2 seconds in (200 rps for
5s), the report shows the same run twice:

```
latency, corrected (from each request's intended send time)
  p50 572µs   p90 505.2ms   p99 951.6ms   p99.9 996.7ms   max 1.00s
latency, service (from when the request was actually sent)
  p50 518µs   p90 897µs   p99 951.6ms   p99.9 996.7ms   max 1.00s
```

The **service** line is what a closed-loop tester would report: the p90 looks perfect at 897µs. The **corrected**
line shows that about a fifth of the requests, the 200 that were due during the freeze, actually waited for it. The
report goes on to say when it happened (a timeline of windows), where each request's time went (connect, send, wait),
and whether the generator itself kept up. [Reading the report](docs/reading-the-report.md) walks through all of it.

## Documentation

| Page | What it covers |
|---|---|
| [Running a test](docs/running.md) | every flag, Docker, HTTP/2, TLS and client certificates, load profiles and Poisson arrivals, pacing precision, saving every request and plotting it |
| [Reading the report](docs/reading-the-report.md) | the live line and the final report, the per-window timeline, phases (where the time goes), the generator line |
| [Finding the highest rate](docs/find.md) | `gmeter find`: the most a service can take while meeting a target such as `p99=50ms` |
| [Replaying real traffic](docs/replay.md) | `gmeter replay`: send a traffic log at the pace it was recorded, with a report per endpoint |
| [gRPC](docs/grpc.md) | `gmeter grpc`: unary calls found by server reflection or from a schema |
| [Several generators](docs/several-generators.md) | `-agents`: spread one test over several machines |
| [Comparing runs](docs/comparing-runs.md) | `gmeter diff`: what changed between two runs, per endpoint and per phase |
| [Using it from Go](docs/go-package.md) | the `gmeter` package, for a load test inside `go test` |
| [Using it from an AI agent](docs/mcp.md) | `gmeter mcp`: the tools an agent gets and the limits you set |
| [How it works](docs/how-it-works.md) | the pacer, the histogram, the runner, and the choices behind the numbers |

## Development

```bash
go test -race ./...
```

[How it works](docs/how-it-works.md#development) says what the tests prove.

## Coming soon

The next set of features:

- **gRPC streaming**: server-streaming, client-streaming and bidirectional methods, beyond unary

## License

MIT. See [LICENSE](LICENSE).
