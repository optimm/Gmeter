# Reading the report

What a run prints, where each request's time went, and how to tell when the generator was the problem. Back to the [README](../README.md).

## What it looks like

Against a throwaway server that answers instantly except for a 1-second freeze starting 2 seconds in
(200 rps for 5s, 20 workers). While it runs, stderr shows one line per second, each with the stats of **just
that window**:

```
[  1.0s] 201 scheduled, 201 completed, 0 failed, 2 late | window: 201 rps, p50 490µs p99 8.9ms max 18.7ms
[  2.0s] 355 scheduled, 315 completed, 0 failed, 2 late | window: 114 rps, p50 572µs p99 1.6ms max 3.0ms
[  3.0s] 601 scheduled, 601 completed, 0 failed, 163 late | window: 286 rps, p50 291.5ms p99 992.0ms max 1.00s
[  4.0s] 801 scheduled, 801 completed, 0 failed, 163 late | window: 200 rps, p50 416µs p99 2.7ms max 6.1ms
```

The freeze is in the window ending at 3.0s. A run-wide p99 could not tell you *when*. Then the report (recorded
before the phase table below existed: a run now also prints that after the two latency lines):

```
requests   1000 scheduled   1000 ok   0 bad status   0 failed   0 dropped
achieved   200.1 rps (target 200) over 5.00s
status     200: 1000
generator  timer lag p99 1.1ms, max 14.3ms   late 163 (16.3%)
warning    163 of 1000 requests (16%) were dispatched more than 1ms after their due time: every worker was busy (raise -workers, or the server is slow); their latency still counts from the due time

latency, corrected (from each request's intended send time)
  p50 572µs   p90 505.2ms   p99 951.6ms   p99.9 996.7ms   max 1.00s
latency, service (from when the request was actually sent)
  p50 518µs   p90 897µs   p99 951.6ms   p99.9 996.7ms   max 1.00s
```

The **service** line is what a closed-loop tester would report: the p90 looks perfect at 897µs. The
**corrected** line shows that about a fifth of the requests (the 200 that were due during the 1-second
freeze) actually waited for it.

Note that the corrected p50 (572µs) is within about 50µs of the service p50 (518µs): with the default `-spin`, the
scheduler's own wake-up lateness is not in the numbers (see [Pacing precision](running.md#pacing-precision--spin)).

Re-running with `-workers 2` is a good test of which number to trust. The corrected p99 stays at 957ms
(was 952ms with 20 workers), because it does not depend on how many workers you gave it. The service p99
drops to 3.5ms (was 952ms), because only 2 requests were physically stuck. The service number is an artifact
of the worker count; the corrected one is not.

The first run's report closes with a per-window timeline, so a spike has a time on it:

```
timeline (1s windows)
  p99  ▁▁█▁▁   worst 992.0ms in the window ending at 3.0s
  rps  ▆▄█▆   lowest 114 in the window ending at 2.0s
```

(The completions are counted when a request finishes, so the requests stuck in the freeze show up in the
window ending at 3.0s, not in the ones in which they were due.)

The bars are scaled from zero, so a small wobble stays small. When a run has more windows than fit, they are
combined so that the worst p99 and the lowest rps survive.

## Where the time goes

A slow request comes with a reason. The report splits the corrected latency of every request that got a response
into phases. The same replay twice against a toy server with four slots (`/search` holds one for 40ms, everything
else for 2ms), first with `-workers 50`, then with `-workers 4`:

```
-workers 50
  phase     requests       p50       p99       max   share
  queue         3045      21µs     2.0ms    11.9ms    0.4%
  connect         32     641µs     1.5ms     1.5ms    0.0%
  send          3045      18µs     273µs    13.7ms    0.2%
  wait          3045    10.8ms    86.8ms   121.9ms   99.2%
  transfer      3045      17µs     153µs     870µs    0.1%

-workers 4
  phase     requests       p50       p99       max   share
  queue         3045     5.1ms    84.0ms   112.9ms   59.8%
  connect          4     520µs     652µs     652µs    0.0%
  send          3045      17µs     137µs     3.2ms    0.1%
  wait          3045     2.8ms    43.1ms    48.3ms   40.0%
  transfer      3045      18µs     137µs     591µs    0.1%
```

With 50 workers 99% of the time is `wait`: every request is sent on time and the server is where they queue. With
4 workers 60% is `queue`: requests waited inside Gmeter for a free worker, time a closed-loop tester would not
have counted at all. Same server, same trace; the table says which side was the bottleneck.

| Phase | What it is |
|---|---|
| `queue` | from the intended send time until Gmeter sent the request: waiting for a free worker, or a late timer |
| `dns` | looking up the host name; only for a request that opened a connection to a name, never for an IP address |
| `connect` | the TCP connection; only for a request that opened one |
| `tls` | the TLS handshake; only for a request that opened an https connection |
| `send` | the rest of the time before the server could answer: taking a connection from the pool and writing the request |
| `wait` | from the request written to the first byte of the response: the server's time plus the network's |
| `transfer` | from the first byte to the end of the body |

The phases add up to each request's corrected latency exactly, and a test checks that over a whole run. Things to
know:

- **Connections are reused**, so only the first request on each has `dns`, `connect` and `tls`: 32 requests
  connected with 50 workers and 4 with 4 workers. The `requests` column says how many had each phase, and
  `share` is of all the time, so a connect that few requests pay shows about 0%. Read its p50 and p99 for how slow
  connecting is.
- **Timeouts and failed requests are not split**: there is no response to split. They are in the latency totals.
- **`wait` is not only the server.** It includes the network round trip. On loopback it is all server.
- **They are measured with the hooks of `net/http/httptrace`**, over HTTP/1.1 or [HTTP/2](running.md#http2). A request that
  took an idle connection gets no connection phases, even if a dial it started was still under way.
- **Cost**: about 16 more small allocations (650 bytes) per request. On loopback a request took about 67µs with and
  without it, so any time difference is inside run-to-run noise. Reproduce with
  `go test -run '^$' -bench PhaseTrace -benchmem ./internal/runner`. `-phases=false` turns it off. `gmeter find`
  records them for each trial (see [Where the time grew](find.md#where-the-time-grew)).
- They are saved in the JSON as `phases` (digests). The table above is for the whole run, with distributions;
  a replay also gets one per endpoint, but with means only (see [Per endpoint](replay.md#per-endpoint)), and `diff` compares
  both (see [Phases](comparing-runs.md#phases) under Comparing runs).

## Reading the generator line

`generator  timer lag p99 …, max …   late N (P%)` is Gmeter checking itself:

- **timer lag** is how far past its due time the scheduler woke up, for requests it had to sleep for. It
  isolates timer and OS scheduling overshoot, plus GC pauses in Gmeter itself. A warning fires above 5ms p99.
- **late** counts requests the scheduler reached more than 1ms after their due time. Either every worker was
  busy (raise `-workers`, or the server is slow) or the generator was starved of CPU. Late requests are still
  measured from their due time, so the latency stays honest. The 1ms tolerance exists because random arrivals
  put requests a few microseconds apart: without it, a healthy generator reported 24% of Poisson requests as
  late at 20000 rps against an instant server.

A run is **generator limited** when the numbers may describe the generator rather than the server. Either timers
ran late (the first signal), or more than 5% of requests were late *and* the mean queue inside Gmeter is more
than `4 × workers × mean service time`. The queue is `latency_corrected.mean_ms - latency_service.mean_ms`:
with W workers, requests can queue behind about W requests that each took the mean service time, so much more
than that is time the scheduler lost, not time the workers spent answering. This matters because once the
scheduler falls behind it stops sleeping, so timer lag alone can read near zero exactly when the generator is
the bottleneck. A slow server also fills every worker and makes requests late; its queue stays within that
bound, so it is not marked limited.

The text report prints one `warning` naming the signal and what to do. The JSON has `generator.limited` and,
when it is true, `generator.reason`; `warnings` is always an array. When a result comes from a run whose worker
count is unknown, only the timer-lag signal is available, and a high late share still gets the warning to raise
`-workers` or check the server.
