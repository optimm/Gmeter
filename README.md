# Gmeter

An open-loop HTTP load generator in Go that reports **honest latency**, and tells you when the generator
itself is the problem.

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

Against a throwaway server that answers instantly except for a 1-second freeze starting 2 seconds in
(200 rps for 5s, 20 workers). While it runs, stderr shows one line per second, each with the stats of **just
that window**:

```
[  1.0s] 201 scheduled, 201 completed, 0 failed, 2 late | window: 201 rps, p50 1.4ms p99 4.5ms max 11.9ms
[  2.0s] 343 scheduled, 303 completed, 0 failed, 3 late | window: 102 rps, p50 1.2ms p99 4.7ms max 6.9ms
[  3.0s] 601 scheduled, 601 completed, 0 failed, 166 late | window: 298 rps, p50 271.6ms p99 996.1ms max 1.01s
[  4.0s] 801 scheduled, 801 completed, 0 failed, 171 late | window: 200 rps, p50 1.3ms p99 17.2ms max 27.2ms
```

The freeze is in the window ending at 3.0s. A run-wide p99 could not tell you *when*. Then the report:

```
requests   1000 scheduled   1000 ok   0 bad status   0 failed   0 dropped
achieved   200.1 rps (target 200) over 5.00s
status     200: 1000
generator  timer lag p99 2.1ms, max 22.6ms   late 171 (17.1%)
warning    171 of 1000 requests (17%) were dispatched after their due time: every worker was busy (raise -workers, or the server is slow) or […]

latency, corrected (from each request's intended send time)
  p50 1.4ms   p90 513.5ms   p99 956.8ms   p99.9 1.00s   max 1.01s
latency, service (from when the request was actually sent)
  p50 484µs   p90 1.2ms   p99 955.8ms   p99.9 1.00s   max 1.01s
```

The **service** line is what a closed-loop tester would report: the p90 looks perfect at 1.2ms. The
**corrected** line shows that about a fifth of the requests (the 200 that were due during the 1-second
freeze) actually waited for it.

Re-running with `-workers 2` is a good test of which number to trust. The corrected p99 stays at 955ms
(was 957ms), because it does not depend on how many workers you gave it. The service p99 drops to 2.9ms
(was 956ms), because only 2 requests were physically stuck. The service number is an artifact of the
worker count; the corrected one is not.

## Reading the generator line

`generator  timer lag p99 …, max …   late N (P%)` is Gmeter checking itself:

- **timer lag** is how far past its due time the scheduler woke up, for requests it had to sleep for. It
  isolates timer and OS scheduling overshoot, plus GC pauses in Gmeter itself. A warning fires above 5ms p99.
- **late** counts requests the scheduler reached when they were already overdue. Either every worker was busy
  (raise `-workers`, or the server is slow) or the rate is finer than the machine's timers can pace. A
  warning fires above 5%. Late requests are still measured from their due time, so the latency stays honest.

Both are in the JSON under `generator`, and `warnings` is always an array.

## How it works

```
 pacer.Schedule ──▶ scheduler goroutine ──▶ chan time.Time ──▶ N worker goroutines
 (At(n) = start      sleeps until each        (intended send     one request at a time,
  + n/rate, no        request is due; blocks   times, buffer N)   own Recorders, no locks
  drift)              if every worker is busy                          │
                                  ▲                                    ▼
          progress goroutine ─────┴── pulls atomic snapshots ◀── Recorders (atomic, padded)
          diffs consecutive snapshots into per-window stats
```

- **`internal/pacer`**: the schedule is computed from the request index, never by accumulating an interval,
  so rounding error does not build up over millions of requests. If the generator falls behind it catches up
  by sending back-to-back, and those requests still carry their original due time.
- **`internal/hist`**: a fixed-memory (~250KB) log-linear histogram in the style of HdrHistogram. Relative
  error is at most 2^-10 (about 0.1%) at any magnitude.
  - `Histogram` is the plain version, used for merged results.
  - `Recorder` has atomic fields, so one worker can record while the progress goroutine snapshots it, with no
    lock on the hot path. `Histogram.Delta` turns two cumulative snapshots into "what happened in between".
  - The progress goroutine **pulls** instead of workers pushing: during a stall the workers are blocked
    inside a request and could not push anything, which is exactly when the display has to keep updating.
- **False sharing, measured.** Recorders are padded to 128-byte cache lines. On a 10-core Apple Silicon Mac,
  8 goroutines each recording into their own Recorder took 2.0-3.1 ns/op unpadded and 0.92-0.93 ns/op padded
  (five runs each). Reproduce with `go test -run '^$' -bench RecorderRecordParallel -cpu 8 ./internal/hist`.
- **`internal/runner`**: scheduler, workers, an HTTP/1.1 client capped at `-workers` connections, live
  counters (atomics), and the generator-health measurements.
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

The tests include one that proves the correction (one connection, the server freezes for 400ms on the first
request, and the corrected p90 must show the queueing while the service p90 stays small) and one that proves
the live windows pinpoint when a stall happened.

## Roadmap

- [x] **v0**: open-loop HTTP, corrected and service latency, JSON output
- [x] **v1a**: live per-window stats in the progress line, generator health (timer lag, late dispatches) and
      warnings
- [ ] **v1b**: per-second timeline in the report and JSON, full histograms in the JSON, `gmeter diff a.json b.json`
- [ ] **v2**: ramp and step schedules, Poisson arrivals, "find the max rate under a p99 target", traffic replay
      from a log file, per-phase timings (connect, TLS, first byte)
- [ ] **v3**: distributed mode with one controller driving several generators (merging histograms, not
      averaging percentiles), HTTP/2 and gRPC
