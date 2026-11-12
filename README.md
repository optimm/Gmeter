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
| `-json` | | also save a JSON summary (timeline and full histograms included) to a path (`-` = only JSON, on stdout) |
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

From another run of the same demo, the report closes with a per-window timeline, so a spike has a time on it:

```
timeline (1s windows)
  p99  ▁▁█▁▁   worst 993.5ms in the window ending at 3.0s
  rps  ████   lowest 194 in the window ending at 2.0s
```

The bars are scaled from zero, so a small wobble stays small. When a run has more windows than fit, they are
combined so that the worst p99 and the lowest rps survive.

## Comparing runs

Save each run with `-json`, then compare them:

```bash
gmeter -rate 200 -duration 30s -json before.json http://localhost:8080/
# ...change something...
gmeter -rate 200 -duration 30s -json after.json  http://localhost:8080/
gmeter diff before.json after.json
```

Real output (A is a healthy run, B is the freeze demo; file paths shortened):

```
note  duration differs (A: 4s, B: 5s): this is not a like-for-like comparison
note  A reported 1 warning(s) about the generator (see its summary): its latencies may be inflated
note  B reported 1 warning(s) about the generator (see its summary): its latencies may be inflated

                            A            B    change
achieved rps            200.1        200.1     -0.0%
failed                      0            0     +0.0%
bad status                  0            0     +0.0%
latency, corrected
  p50                   1.4ms        1.5ms     +4.9%
  p90                   2.1ms      509.3ms +23975.9%
  p99                   6.8ms      953.7ms +13850.4%
  p99.9                11.6ms      998.8ms  +8483.7%
  max                  11.6ms        1.00s  +8522.1%

share of B's requests that are slower than A's percentiles
  slower than A's p50 (1.4ms): 59.7%   (at most 50.0% if nothing changed)
  slower than A's p90 (2.1ms): 25.7%   (at most 10.0% if nothing changed)
  slower than A's p99 (6.8ms): 21.1%   (at most 1.0% if nothing changed)
```

The last block is something percentiles alone cannot tell you. It uses the full histograms saved in each file
to answer "how many more of my requests got slow?", not just "how much slower is the p99?".

`diff` prints a note when the runs differ in URL, method, rate, workers or duration, or when either run's
generator raised a warning.

To gate a build, add `-threshold`:

```bash
gmeter diff -threshold 20 before.json after.json   # exit 1 if corrected p99 rose by more than 20%
```

Exit codes: `0` compared (and within the threshold, if one was given), `1` the threshold was exceeded, `2` bad
usage or an unreadable file. It looks only at p99. A single run per side cannot tell a real change from
run-to-run noise, so treat small differences with suspicion and repeat the runs.

### What the JSON contains

`version`, `run` (URL, method, workers, duration, start time), the totals, `generator`, `warnings`, the
per-window `timeline`, and the full latency `histograms` (sparse: only non-empty buckets). A password in the
URL is redacted (`user:xxxxx@host`) in the JSON and everywhere Gmeter prints it, and request headers are never
saved. A query string is kept as given, so don't put secrets in it. `diff` refuses files with a missing or
different `version`, and histograms it cannot rebuild.

## Reading the generator line

`generator  timer lag p99 …, max …   late N (P%)` is Gmeter checking itself:

- **timer lag** is how far past its due time the scheduler woke up, for requests it had to sleep for. It
  isolates timer and OS scheduling overshoot, plus GC pauses in Gmeter itself. A warning fires above 5ms p99.
- **late** counts requests the scheduler reached when they were already overdue. Either every worker was busy
  (raise `-workers`, or the server is slow) or the rate is finer than the machine's timers can pace. A
  warning fires above 5%. Late requests are still measured from their due time, so the latency stays honest.

Both are in the JSON under `generator`, and `warnings` is always an array.

Timer lag counts toward corrected latency, because a request sent late is still measured from when it was due.
On a Mac with other work running I measured a timer-lag p99 of about 5ms, which was most of the 6.8ms p99 of a
fast local server. Gmeter warned, correctly. For sub-millisecond precision, run it on a quiet machine or on
Linux.

## How it works

```
 pacer.Schedule ──▶ scheduler goroutine ──▶ chan time.Time ──▶ N worker goroutines
 (At(n) = start      sleeps until each        (intended send     one request at a time,
  + n/rate, no        request is due; blocks   times, buffer N)   own Recorders, no locks
  drift)              if every worker is busy                          │
                                  ▲                                    ▼
          sampler goroutine ─────┴── pulls atomic snapshots ◀── Recorders (atomic, padded)
          diffs consecutive snapshots into per-window stats
```

- **`internal/pacer`**: the schedule is computed from the request index, never by accumulating an interval,
  so rounding error does not build up over millions of requests. If the generator falls behind it catches up
  by sending back-to-back, and those requests still carry their original due time.
- **`internal/hist`**: a fixed-memory (~250KB) log-linear histogram in the style of HdrHistogram. Relative
  error is at most 2^-10 (about 0.1%) at any magnitude.
  - `Histogram` is the plain version, used for merged results.
  - `Recorder` has atomic fields, so one worker can record while the sampler goroutine snapshots it, with no
    lock on the hot path. `Histogram.Delta` turns two cumulative snapshots into "what happened in between".
  - The sampler goroutine **pulls** instead of workers pushing: during a stall the workers are blocked
    inside a request and could not push anything, which is exactly when the display has to keep updating.
  - `Dump` / `FromDump` save and restore a histogram as sparse JSON, so saved runs can be compared exactly
    (and, later, merged).
- **False sharing, measured.** Recorders are padded to 128-byte cache lines. On a 10-core Apple Silicon Mac,
  8 goroutines each recording into their own Recorder took 2.0-3.1 ns/op unpadded and 0.92-0.93 ns/op padded
  (five runs each). Reproduce with `go test -run '^$' -bench RecorderRecordParallel -cpu 8 ./internal/hist`.
- **`internal/runner`**: scheduler, workers, an HTTP/1.1 client capped at `-workers` connections, live
  counters (atomics), and the generator-health measurements.
- **`internal/report`**: text and JSON output, the per-window timeline, and run comparison (`diff`).

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
- [x] **v1b**: per-window timeline in the report and JSON, full histograms in the JSON, `gmeter diff a.json b.json`
      with a `-threshold` gate
- [ ] **v2**: ramp and step schedules, Poisson arrivals, "find the max rate under a p99 target", traffic replay
      from a log file, per-phase timings (connect, TLS, first byte)
- [ ] **v3**: distributed mode with one controller driving several generators (merging histograms, not
      averaging percentiles), HTTP/2 and gRPC
