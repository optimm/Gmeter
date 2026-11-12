# Gmeter

An open-loop HTTP load generator in Go that reports **honest latency**.

Most load testers send a request, wait for the reply, then send the next one. When the server stalls, they
stop sending, so the requests that *would have* arrived during the stall are never measured and the p99 looks
fine while real users suffer. This is **coordinated omission**.

Gmeter schedules requests by the clock instead. Request *n* is due at `start + n / rate` no matter what the
server is doing, and latency is measured from that **intended** send time.

## Quick start

```bash
go build -o gmeter ./cmd/gmeter

gmeter -rate 2000 -duration 60s -workers 64 http://localhost:8080/health

gmeter -rate 500 -duration 30s -method POST \
  -H 'Content-Type: application/json' -H 'Authorization: Bearer ...' \
  -body @order.json http://localhost:8080/v1/orders
```

Flags must come before the URL.

| Flag | Default | Meaning |
|---|---|---|
| `-rate` | `100` | target requests per second (open loop) |
| `-duration` | `10s` | how long requests keep being scheduled |
| `-workers` | `50` | max requests in flight, which is also the max connection count |
| `-timeout` | `10s` | per-request timeout |
| `-method` | `GET` | HTTP method |
| `-H` | | extra header `"Key: Value"` (repeatable) |
| `-body` | | request body; `@file` reads it from a file |
| `-json` | | also write a JSON summary to a path (`-` = only JSON, on stdout) |
| `-quiet` | `false` | no progress output on stderr |

Ctrl-C stops the run early and still prints the report for what ran. Exit code is `0` on success and `2` for
bad usage or configuration.

## What it looks like

Against a throwaway server that answers instantly except for a 1-second freeze in the middle of the run
(200 rps for 6s, 20 workers):

```
requests   1200 scheduled   1200 ok   0 bad status   0 failed   0 dropped
achieved   200.2 rps (target 200) over 6.00s
status     200: 1200

latency, corrected (from each request's intended send time)
  p50 888µs   p90 404.5ms   p99 938.5ms   p99.9 994.1ms   max 997.9ms
latency, service (from when the request was actually sent)
  p50 335µs   p90 821µs   p99 938.0ms   p99.9 993.0ms   max 997.3ms
```

The **service** line is what a closed-loop tester would report: the p90 looks perfect at 0.8ms. The
**corrected** line shows that about 17% of the requests due during the run (the ones that fell inside the
freeze) actually waited for it.

## How it works

```
 pacer.Schedule ──▶ scheduler goroutine ──▶ chan time.Time ──▶ N worker goroutines
 (At(n) = start      sleeps until each        (intended send     one request at a time,
  + n/rate, no        request is due; blocks   times, buffer N)   own histograms, no locks
  drift)              if every worker is busy                         │
                                                                      ▼
                                              tallies merged after the workers exit ──▶ report
```

- **`internal/pacer`**: the schedule is computed from the request index, never by accumulating an interval,
  so rounding error does not build up over millions of requests. If the generator falls behind it catches up
  by sending back-to-back, and those requests still carry their original due time.
- **`internal/hist`**: a fixed-memory (~250KB) log-linear histogram in the style of HdrHistogram. Relative
  error is at most 2^-10 (about 0.1%) at any magnitude. Each worker owns one, so recording needs no locks or
  atomics, and they are merged once at the end.
- **`internal/runner`**: scheduler, workers, an HTTP/1.1 client capped at `-workers` connections, and live
  counters (atomics) for the progress line.
- **`internal/report`**: text and JSON output.

### Choices worth knowing

- Latency is recorded for every response (any status) and for timeouts. Other transport failures
  (connection refused, DNS, reset) are counted but not recorded as latency, because a fast failure says
  nothing about how quickly the server answers. Recording timeouts matters: dropping them would make a hung
  server look fast.
- If the server is so slow that the backlog cannot drain, the scheduler gives up on undispatched requests once
  the run is overdue by one `-timeout`, and reports them as `dropped`.
- Redirects are not followed (a 302 is a result) and proxy environment variables are ignored.
- HTTP/1.1 only for now.

## Development

```bash
go test -race ./...
```

The tests include one that proves the correction: one connection, the server freezes for 400ms on the first
request, and the corrected p90 must show the queueing while the service p90 stays small.

## Roadmap

- [x] **v0**: open-loop HTTP, corrected and service latency, JSON output
- [ ] **v1**: live terminal dashboard, `gmeter diff a.json b.json`, per-phase timings (connect, TLS, first byte)
- [ ] **v2**: ramp-up and step schedules, gRPC, traffic replay from a log file
- [ ] **v3**: distributed mode with one controller driving several generators
