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

Flags must come before the URL.

| Flag | Default | Meaning |
|---|---|---|
| `-rate` | `100` | target requests per second (open loop) |
| `-duration` | `10s` | how long requests keep being scheduled |
| `-stages` | | a load profile instead of `-rate` and `-duration`: steps and ramps (see [Load profiles](#load-profiles-steps-and-ramps)) |
| `-arrivals` | `even` | `even` (exact spacing) or `poisson` (random bursts at the same average rate) |
| `-seed` | random | seed for `poisson`, to repeat the same arrival times; the one used is always printed and saved |
| `-workers` | `50` | max requests in flight, which is also the max connection count |
| `-timeout` | `10s` | per-request timeout |
| `-spin` | `200µs` | busy-wait this long before each request's due time instead of sleeping, for precise timing; `0` = plain timer (see [Pacing precision](#pacing-precision--spin)) |
| `-method` | `GET` | HTTP method |
| `-H` | | extra header `"Key: Value"` (repeatable) |
| `-body` | | request body; `@file` reads it from a file |
| `-insecure` | `false` | do not verify the server's TLS certificate (https only); says so on stderr |
| `-cacert` | | PEM file with the certificate authorities to trust for https, instead of the system's |
| `-json` | | also save a JSON summary (timeline and full histograms included) to a path (`-` = only JSON, on stdout) |
| `-phases` | `true` | split each request's latency into phases, see [Where the time goes](#where-the-time-goes); `-phases=false` turns it off |
| `-quiet` | `false` | no progress output on stderr |
| `-http2` | `false` | send over HTTP/2 only, see [HTTP/2](#http2) |
| `-agents` | | run the test on these `gmeter agent`s instead, `host:port,...`, see [Several generators](#several-generators--agents) |

Ctrl-C stops the run early and still prints the report for what ran (but not a run on agents, which has no partial
report). Exit code is `0` on success, `2` for bad usage or configuration, and `1` for a run on agents that failed or
was interrupted.

### HTTP/2

By default Gmeter speaks HTTP/1.1, even to a server that offers HTTP/2. `-http2` sends every request over
HTTP/2 instead: negotiated over TLS for `https://`, and with prior knowledge (h2c, HTTP/2 without TLS) for
`http://`. It never falls back: a server without HTTP/2 fails the requests as `protocol` errors, with a warning
saying so, rather than being measured over the wrong protocol. The report counts the responses by protocol
(`protocol   HTTP/2.0: 10000`), the JSON saves it as `run.protocol`, and `diff` notes when two runs used different
ones. It works with `find` and `replay` too.

The same server (5ms of work a request), at 1000 rps for 10 seconds with `-workers 50`, both real:

```
protocol   HTTP/1.1: 10000
  connect         11     871µs     1.6ms     1.6ms    0.0%
  send         10000       4µs      19µs     1.1ms    0.1%

protocol   HTTP/2.0: 10000
  connect          1     705µs     705µs     705µs    0.0%
  send         10000      28µs      46µs     1.1ms    0.6%
```

HTTP/1.1 opened 11 connections, one per request in flight at the peak, and HTTP/2 one, with every request a
stream on it. `send` grew from 4µs to 28µs: sharing a connection means framing, flow control and taking turns to
write. Latency was the same, because this server's 5ms dwarfs both.

What changes in the numbers over HTTP/2:

- **`-workers` is still the most requests in flight**, but they share connections: Go opens another only when the
  server's limit on concurrent streams is reached.
- **`send` includes waiting for a turn on the shared connection**, and a request that joined a connection while
  another request was dialling it counts only the part of the dial it waited for, from when it was sent.
- **One slow connection slows every stream on it** (TCP head-of-line blocking). That is real HTTP/2 behaviour and
  it shows in `send` and `wait`.

### HTTPS with a private or self-signed certificate

A staging host often has a certificate that no system root vouches for. Gmeter refuses it, and says why and what
to do (a server with a self-signed certificate, 200 requests at 100 rps):

```
$ gmeter -rate 100 -duration 2s https://127.0.0.1:18443/
requests   200 scheduled   0 ok   0 bad status   200 failed   0 dropped
errors     tls: 200
warning    200 of 200 requests failed in the TLS handshake (the server's certificate was refused, or it did not
           speak TLS): trust its certificate authority with -cacert FILE, or skip verification with -insecure
```

`-cacert staging.pem` trusts exactly the authorities in that file (it replaces the system's, like curl's
`--cacert`), so a server another authority vouches for is still refused. `-insecure` verifies nothing, and
Gmeter says so on stderr (`gmeter: -insecure: the server's certificate is not verified`) so a result from a
server nobody checked is not taken for one that was. The two cannot be combined. Both work with `find` and `replay`
too. The `tls` row of the [phases](#where-the-time-goes) still shows the handshake (890µs for the one
connection above).

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
warning    163 of 1000 requests (16%) were dispatched more than 1ms after their due time: every worker was busy (raise -workers, or the […]

latency, corrected (from each request's intended send time)
  p50 572µs   p90 505.2ms   p99 951.6ms   p99.9 996.7ms   max 1.00s
latency, service (from when the request was actually sent)
  p50 518µs   p90 897µs   p99 951.6ms   p99.9 996.7ms   max 1.00s
```

The **service** line is what a closed-loop tester would report: the p90 looks perfect at 897µs. The
**corrected** line shows that about a fifth of the requests (the 200 that were due during the 1-second
freeze) actually waited for it.

Note that the corrected p50 (572µs) is within about 50µs of the service p50 (518µs): with the default `-spin`, the
scheduler's own wake-up lateness is not in the numbers (see [Pacing precision](#pacing-precision--spin)).

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
- **They are measured with the hooks of `net/http/httptrace`**, over HTTP/1.1 or [HTTP/2](#http2). A request that
  took an idle connection gets no connection phases, even if a dial it started was still under way.
- **Cost**: about 16 more small allocations (650 bytes) per request. On loopback a request took about 67µs with and
  without it, so any time difference is inside run-to-run noise. Reproduce with
  `go test -run '^$' -bench PhaseTrace -benchmem ./internal/runner`. `-phases=false` turns it off. `gmeter find`
  records them for each trial (see [Where the time grew](#where-the-time-grew)).
- They are saved in the JSON as `phases` (digests). The table above is for the whole run, with distributions;
  a replay also gets one per endpoint, but with means only (see [Per endpoint](#per-endpoint)), and `diff` compares
  both (see [Phases](#phases) under Comparing runs).

## Load profiles: steps and ramps

`-stages` replaces `-rate` and `-duration` (it cannot be combined with them) with a list of stages, each
`DURATION@RATE` for a constant rate or `DURATION@FROM->TO` for a linear ramp:

```bash
gmeter -stages '30s@200,30s@400,30s@800'          # a step test
gmeter -stages '10s@50,1m@50->500,30s@500'        # warm up, ramp, hold
gmeter -stages '20s@500,10s@0,20s@500'            # a pause in the middle
```

The report then breaks the run down by stage, and attributes each request to the stage it was **due** in, not the
one it finished in: it is the load that was being offered when it was due that is being tested. Against a throwaway
server with a fixed capacity (4 requests at a time, 5ms each, so about 800 rps), 400 workers:

```
gmeter -workers 400 -stages '4s@200,4s@400,4s@600,4s@700,4s@900,4s@1200' http://127.0.0.1:18082/
```

```
stages (corrected latency, by when each request was due)
   #  stage    scheduled  failed        p50        p90        p99        max
   1  4s@200         800       0      5.5ms      6.3ms     10.7ms     13.2ms
   2  4s@400        1600       0      5.3ms      5.8ms      9.1ms     14.4ms
   3  4s@600        2400       0      5.3ms      5.5ms     12.1ms     21.7ms
   4  4s@700        2800       0      5.5ms      5.6ms      6.3ms      8.1ms
   5  4s@900        3600       0    374.9ms    821.0ms    977.3ms    996.0ms
   6  4s@1200       4800       0      2.77s      4.19s      4.52s      4.56s
```

The knee is between 700 and 900 rps: up to 700 the p99 is 6-12ms, at 900 (just over capacity) it is a second, at
1200 it is 4.5 seconds. The same run's whole-run service p99 was 639ms, because the workers cap how many requests
can be physically in flight; the corrected p99 was 4.43s.

That run also printed a late-dispatch warning, and it was right to: holding 1200 rps against a multi-second
latency needs thousands of requests in flight, and it had 400 workers. The corrected latencies stay honest anyway,
because a backed-up request keeps its place on the clock; raise `-workers` to also see the server's view.

Ramps and steps are computed from the cumulative request count, so there is no drift and a fraction of a request
left at the end of a stage carries into the next. `0` is a valid rate, for a pause.

### Poisson arrivals

By default requests are exactly evenly spaced. Real traffic is not: it arrives in clumps, and clumps queue. The
same server at the same average rate, 600 rps (75% of its capacity), `-seed 11`:

| arrivals | p50 | p90 | p99 | p99.9 |
|---|---|---|---|---|
| `even` | 5.3ms | 5.4ms | 6.0ms | 15.4ms |
| `poisson` | 6.2ms | 11.1ms | 17.4ms | 26.0ms |

Evenly spaced requests said the p99 was 6.0ms; the same load arriving in random bursts gave 17.4ms. Use `-arrivals
poisson` when the question is how the server behaves under realistic traffic, not how it behaves under a metronome.

`-arrivals poisson` draws exponential gaps (mean 1/rate) from a seeded generator, mapped through the profile, so it
follows ramps and steps too. The seed is always printed in the banner and saved in the JSON; pass it back with
`-seed` to repeat the same arrival times. `diff` notes when two runs used different arrival processes or seeds.

## Finding the highest rate that meets a target (`gmeter find`)

```bash
gmeter find -slo p99=50ms http://localhost:8080/health
```

It answers "how much can this take while still meeting my latency target?" without you guessing rates. The target
is a corrected-latency quantile and a limit (`p99=50ms`, `p99.9=200ms`, `p50=5ms`), plus an error budget
(`-max-errors`, default 1% of requests failing or returning a bad status: a server that answers 500 instantly has
superb latency and is not meeting anything).

The search starts at `-start` (100 rps) and:

1. **grows**: doubles the rate until a trial misses the target (or `-max` passes);
2. **descends**: halves it instead if the very first trial already fails, down to 1 rps;
3. **bisects**: narrows the gap between the highest pass and the lowest fail until it is within `-tolerance` (5%);
4. **confirms**: repeats the answer `-confirm` times (2) to see whether it holds.

Each trial runs a `-warmup` (2s) that is not judged, to take the cold-start cost of new connections and caches,
then `-trial` (10s) that is. Against the fixed-capacity server from above (4 requests at a time, 5ms each, so
about 800 rps), `-trial 5s -warmup 1s`, with the default Poisson arrivals:

```
find  GET http://127.0.0.1:18082/   target p99 ≤ 50ms, errors ≤ 1%   poisson arrivals (seed 11)   5s trials after a 1s warm-up

   #  phase         rate        p99  errors  result
   1  grow           100      8.2ms    0.0%  pass
   2  grow           200      7.7ms    0.0%  pass
   3  grow           400      9.1ms    0.0%  pass
   4  grow           800    183.1ms    0.0%  FAIL  p99 is 183.1ms, over the 50ms target
   5  bisect         600     12.7ms    0.0%  pass
   6  bisect         700     26.8ms    0.0%  pass
   7  bisect         750     52.9ms    0.0%  FAIL  p99 is 52.9ms, over the 50ms target
   8  bisect         725     34.5ms    0.0%  pass
   9  confirm        725     39.1ms    0.0%  pass
  10  confirm        725     38.8ms    0.0%  pass

result  the highest rate meeting p99 ≤ 50ms is about 725 rps
        the next rate up that was tried, 750 rps, failed
        held in 2 of 2 re-runs at 725 rps
```

That took 70 seconds: each trial costs its warm-up and its measurement plus a one-second pause so that one trial's
aftermath does not leak into the next. Progress lines go to stderr as it runs; `-json` saves every trial.

### Where the time grew

Each trial records the [phases](#where-the-time-goes) of its measured part (`-phases=false` turns that
off). After the answer, the report compares the trial that made it the answer with the first rate above it that
failed: the mean time per answered request in each phase, and which phase grew most. Two real runs against the same
800 rps server, on a busier machine than the table above (so lower answers):

```
where the time grew from 700 rps (pass) to 725 rps (FAIL), mean per answered request
  queue +259µs   connect +5µs   send +18µs   wait +37.7ms   transfer +785ns
  most of it is wait: the server (and the network) took longer to answer
```

The same search with `-workers 4`, so that Gmeter rather than the server runs out of room first:

```
where the time grew from 500 rps (pass) to 525 rps (FAIL), mean per answered request
  queue +92.8ms   connect 0ns   send +11µs   wait +1.2ms   transfer +23µs
  most of it is queue: requests waited inside Gmeter, for a free worker or a late timer, before being sent.
  The server's own time grew less, so this may be Gmeter's limit rather than the server's: run again with
  more -workers, on a quieter machine, to tell which
```

The second answer, 500 rps, is lower than the server's real limit, and the phases are what show it. When the
average number of requests in flight (rate × time per request once sent, by Little's law) exceeds `-workers`, the
report says that too. These are means, while the target is a quantile: a failure that sits only in the tail can
show no phase growing, and the report says so rather than naming one. `-json` keeps every trial's phases as
`phases` (`queue_ms`, `connect_ms` for dns + connect + tls, `send_ms`, `wait_ms`, `transfer_ms`).

### How far to trust the answer

The report says plainly when the answer needs a caveat, and these are the ones to look for:

- **unstable**: the confirmations disagreed. The same server with evenly spaced arrivals (`-arrivals even`) is the
  example: p99 stayed near 5.5ms up to 750 rps, so the search climbed to 775 rps, and there the two re-runs split,
  31.3ms and 53.1ms. It reported 775 rps with "held in only 1 of 2 re-runs, so treat it as an upper estimate"
  rather than presenting a cliff edge as a measurement. Poisson arrivals gave a steadier 725 rps.
- **at least N rps**: the search stopped at `-max`, not because the server gave out. Raise `-max`.
- **generator limited**: a trial whose latency looked fine, but where Gmeter's own timers ran late (timer-lag p99
  over 5ms), is not trusted: it counts as a failure and the report says the server may sustain more. Run Gmeter
  somewhere quieter, or on a bigger machine.
- **no rate down to 1 rps met the target**: exit code 1.
- **interrupted** (Ctrl-C): the best rate found so far, and exit code 1.

It assumes what every load test assumes, that latency does not improve as the rate rises, and a noisy server breaks
that now and then; the confirmations are the check. A p99 from a 5-second trial rests on a few dozen requests, so
use the 10-second default or longer for anything you will act on. `-workers` (default 200 here) should be at
least rate times latency: with fewer, requests queue inside Gmeter instead of reaching the server, which the
corrected latency still counts, but a server that slows down under concurrency then sees less of it than real
clients would give it, and the answer can be optimistic. Exit codes: `0` a rate was found, `1` none was (or it was
interrupted), `2` bad usage.

| Flag | Default | Meaning |
|---|---|---|
| `-slo` | required | `p99=50ms`: a latency quantile and its limit, on the corrected latency |
| `-max-errors` | `1` | percent of requests that may fail or return a bad status |
| `-start` / `-max` | `100` / `100000` | where the search starts, and the most it will try (rps) |
| `-tolerance` | `5` | stop narrowing within this many percent |
| `-trial` / `-warmup` | `10s` / `2s` | measured time per trial, and the unjudged warm-up before it |
| `-confirm` | `2` | repeats of the answer at the end |

`-workers`, `-timeout`, `-method`, `-H`, `-body`, `-spin`, `-arrivals` (default `poisson` here), `-seed`, `-json`,
`-quiet`, `-phases`, `-insecure` and `-cacert` work as for a plain run. Each trial uses the seed plus its number, so a printed seed reproduces the search.

## Replaying real traffic (`gmeter replay`)

One URL at a steady rate is rarely what production sees. `gmeter replay` takes a log of real requests and sends
them, each one when it was recorded, so the real mix of endpoints and the real bursts reach the server. It is
still open loop: every request is due at its recorded time whatever the server is doing, and latency is
measured from that time.

```bash
gmeter replay -file traffic.jsonl http://staging:8080            # at the recorded pace
gmeter replay -file access.log -speed 2 http://staging:8080      # twice as fast as recorded
gmeter replay -file traffic.jsonl -rate 500 -duration 2m http://staging:8080   # ignore the times, 500 rps
```

The URL is only the server (scheme, host, and an optional path prefix such as `http://host/api`); the log's paths
are appended to it. A log can therefore never send a request to another host.

| Flag | Default | Meaning |
|---|---|---|
| `-file` | | the traffic to replay (required) |
| `-format` | `auto` | `jsonl`, `clf` (an access log) or `auto`: a first character of `{` means JSON Lines |
| `-speed` | `1` | recorded timing only: `2` replays twice as fast as recorded, `0.5` half as fast |
| `-rate` | | send at this constant rate instead of at the recorded times; the trace is cycled in order |
| `-duration` | one pass | with `-rate`: how long to keep sending; by default each request is sent once |
| `-methods` | `GET,HEAD` | which methods to replay, or `all` (see below) |
| `-endpoints` | `20` | the most rows in the per-endpoint breakdown, 2 to 100; `0` leaves it out (see below) |
| `-group` | | name the endpoint of the paths a rule matches, e.g. `/products/:slug` (repeatable; see [Per endpoint](#per-endpoint)) |
| `-arrivals` / `-seed` | `poisson` | access logs and `-rate` only; see below |

`-workers`, `-timeout`, `-spin`, `-H`, `-json`, `-quiet`, `-insecure` and `-cacert` work as for a plain run. `-H` headers go with every
replayed request, which is how to supply credentials (an access log has no headers). Headers in a JSON Lines
trace are sent too and replace a `-H` of the same name. No headers or bodies are saved in the results. Exit code
is `0` when it ran, `1` if the report could not be written, and `2` for bad usage or an unreadable trace.

### Trace formats

**JSON Lines**: one object per line. Only `path` is required.

```json
{"at": 0.25, "method": "POST", "path": "/orders?id=7", "headers": {"Content-Type": "application/json"}, "body": "{\"qty\":2}"}
```

`at` is when the request was made, in seconds from any fixed point (only the differences matter): give it to every
request or to none. `status` is optional: what the server answered when the request was recorded, from 100 to 599,
so the replay can say how many responses differ (see [Status against the log](#status-against-the-log)). Unknown
fields are an error, so a misspelt `header` does not silently send the wrong thing. Bodies are text. Without `at`
the trace has no pace of its own, so `-rate` is required.

**Access log**: the common and combined log formats that nginx, Apache, HAProxy and most proxies write. Each line
gives a method, a path and the second it was logged in. Lines that are not usable requests (garbage, a `"-"`
request from a connection that sent nothing, a proxy-style absolute URL) are counted and reported, not fatal.
An access log has no request bodies, so a replayed `POST` has none. Each line's status is kept (a `-` or a number that
is not a status leaves that request with nothing to compare).

### What it does with the times

- **Recorded timing** (the default) sends request *i* at its recorded time, relative to the first one.
- **An access log only knows the second** a request was logged in. What survives is how many requests there were in
  each second, so Gmeter spreads them inside it: `-arrivals poisson` (the default) places them at random points,
  which is what independent clients look like given only a count, and `-seed` repeats it; `-arrivals even` spaces
  them exactly. Exact JSON Lines times need no spreading, so setting either flag for them is an error.
- **`-rate`** discards the times and sends the trace in order at a constant rate, cycling if the run is longer
  than the trace. `-arrivals` and `-seed` then work as in a plain run.

### It will not write to your server unless you say so

A replay sends whatever the log says. A log full of `POST`, `PUT` and `DELETE` would create, change and delete
things on the server under test, so the default `-methods GET,HEAD` leaves those requests out, says how many it
skipped, and records that in the JSON. Pass `-methods all` or a list such as `-methods GET,POST` when that is
what you want, against an environment where it is safe. The skipped requests' times go with them, so the
replayed load is the filtered load.

### What the pace changes

The same 4000 requests (10 seconds; every second 800ms at 200 rps and then a 200ms burst at 1200 rps, 400 rps on
average) against a toy server that handles about 800 rps:

| How the requests were sent | p50 | p99 |
|---|---|---|
| flat 400 rps, evenly spaced | 5.2ms | 5.4ms |
| flat 400 rps, Poisson | 5.4ms | 9.4ms |
| the same traffic as an access log (one-second times), spread as Poisson | 5.7ms | 13.6ms |
| the same traffic as JSON Lines, recorded times | 31.5ms | 121.1ms |

Single runs on a laptop that was also running other things, so trust the ordering and the rough size of the gaps, not the
exact numbers. The point is the last row: the average rate is far below what the server can do, and the p99 is
still more than 20 times the flat run's, because the bursts queue. `gmeter diff` notes when two runs replayed different
traces, speeds or timings, since they are not like-for-like.

### Per endpoint

A replay reports each endpoint on its own as well as the whole mix. An endpoint is the method and the path, with
the query string dropped and any path segment that is an identifier (only digits, a UUID, or 16 or more hex
characters) replaced by `:id`: `GET /orders/18273?ref=mail` and `GET /orders/99` are both `GET /orders/:id`. The
busiest endpoints get a row each and the rest are grouped as `(other)`; `-endpoints N` sets the most rows (each one
costs two histograms, three with phases, hence the limit of 100). Requests left out by `-methods` are not counted,
so the ranking is of what was sent.

Identifiers are recognised by how they look, so a slug (`/products/blue-shoes`) or a file name (`/img/a.png`) would
be an endpoint of its own each time. `-group` names them instead (repeatable; the first rule that matches wins):

```bash
gmeter replay -file access.log -group '/products/:slug' -group '/products/*/reviews' -group 'GET /static/**' http://staging:8080
```

`*` or `:name` stands for any one non-empty segment, a last `**` for whatever follows (nothing included), and any
other segment for itself, compared exactly. A method limits a rule to that method. The endpoint is named after the
rule (`GET /products/:slug`), and paths no rule matches are named by their shape as before. Put the more specific
rule first: `/products/:slug` before `/products/**`, or the second takes everything.

Real output: a 10-second trace at about 300 rps (65% `/orders/N`, 20% `/search?q=...`, 15% `/users/N/cart`)
against a toy server with four slots, where `/search` holds a slot for 40ms and everything else for 2ms. With
`-workers 50`:

```
endpoints, busiest first (corrected latency; svc p99 is service latency, from when each request was actually sent)
  endpoint             scheduled     bad  failed       p50       p99       max   svc p99
  GET /orders/:id           1961       0       0     4.3ms    72.9ms    89.0ms    72.7ms
  GET /search                625       0       0    42.6ms   105.3ms   118.9ms   103.0ms
  GET /users/:id/cart        459       0       0     4.3ms    70.6ms    80.5ms    70.5ms
```

`/orders` costs 2ms but has a p99 of 73ms. This load keeps about three of the server's four slots busy (300 rps
at about 10ms of slot time per request on average), so a request often has to wait for one, and a 2ms request
waits as long as a 40ms one. The service p99 is just as high, because that wait is inside the server and both
latencies include it. The same trace with `-workers 4` waits in the generator instead:

```
  GET /orders/:id           1961       0       0     8.0ms   109.6ms   131.4ms     5.6ms
  GET /search                625       0       0    46.7ms   140.8ms   157.4ms    44.2ms
  GET /users/:id/cart        459       0       0     6.5ms    83.3ms   124.4ms     5.1ms
```

Now the corrected p99 is 110ms and the service p99 is 5.6ms: once a request was sent it was answered quickly,
but it had to wait for a free worker first (the generator line reported 37% of dispatches late). A gap between
the two columns means requests waited in Gmeter; raise `-workers`.

With phases on (the default) a second table says where each endpoint's time went: the mean time per answered
request in each phase, so the columns add up to the mean. A fresh run of the same two setups:

```
-workers 50
  endpoint             answered      mean     queue   connect      send      wait  transfer
  GET /orders/:id          1961    12.8ms      15µs       6µs      12µs    12.8ms      12µs
  GET /search               625    50.9ms      17µs       4µs      12µs    50.8ms      13µs
  GET /users/:id/cart       459    11.9ms      15µs       1µs      12µs    11.8ms      12µs

-workers 4
  endpoint             answered      mean     queue   connect      send      wait  transfer
  GET /orders/:id          1961    15.6ms    13.0ms     498ns       9µs     2.6ms      12µs
  GET /search               625    53.8ms    12.9ms     936ns      10µs    40.9ms      14µs
  GET /users/:id/cart       459    14.7ms    12.1ms       1µs       9µs     2.6ms      12µs
```

A 2ms `/orders` request spends 12.8ms in `wait` with 50 workers: that is the server, holding it until a slot
was free. With 4 workers its `wait` is 2.6ms, about its own cost, and the other 13ms are `queue`: it waited in
Gmeter. All three endpoints queue for 12-13ms because the limit is the one shared pool of workers.

- **Means, plus the tail of `wait`.** A distribution costs about a quarter of a megabyte, and 100 endpoints with 7
  phases each would take hundreds, so each endpoint keeps one: its `wait`, the server's own time, shown as the last
  column (`wait p99`) and saved as `endpoints[].latency_wait`. That is the one a mean hides. A real run against a
  server where `/search` takes 5ms but one request in 30 takes 150ms, and `/orders/:id` always takes 8ms, at 200 rps:

  ```
    endpoint         answered      mean     queue   connect      send      wait  transfer  wait p99
    GET /orders/:id      1500     9.5ms     136µs       1µs      36µs     9.3ms      39µs    12.8ms
    GET /search          1500    10.7ms     128µs       3µs      36µs    10.5ms      27µs   150.6ms
  ```

  Their mean waits are a millisecond apart, and their tails more than ten times apart. The whole-run table has
  every phase's percentiles. `diff` shows how a regressed endpoint's wait p99 moved (see below).
- **`connect` is dns + connect + tls**, averaged over all of the endpoint's answered requests, not only those
  that opened a connection. The JSON keeps every phase apart, with how many requests had it, its mean and its
  share (`endpoints[].phases`).
- **Timeouts and failed requests have no phases**, so with any, the columns add up to the mean of the answered
  requests only.

The names are paths from your trace, so they go into the saved JSON (`endpoints`). The query string never does,
but a secret in a path segment that is not an identifier would; `-endpoints 0` keeps every path out of the report
and the file.

### Status against the log

A log records what the server answered, so the replay can check that the target answers the same way. An access log
of 600 requests that were all `200`, replayed against a staging that has no orders with an id divisible by 4 (real
output):

```
requests   600 scheduled   480 ok   120 bad status   0 failed   0 dropped
status     200: 480   404: 120
vs log     600 compared, 120 differ (20.0%): 2xx→4xx 120
warning    120 of 600 responses (20%) were of a different class than the log recorded (2xx→4xx 120): the target may
           not hold the data the log asked for, so these latencies may not be what production would see; check
           before reading them
...
endpoints, status vs the log (responses of another class than the log's)
  endpoint               differ    share
  GET /orders/:id    120 of 480    25.0%
```

- **By class.** A `204` where the log has a `200` is the same answer; a `404` is not. Changes are named by class and
  listed most common first (`2xx→4xx`), up to four, then a count of the rest.
- **Only requests with a logged status are compared**, and only the ones that got a response: a timeout or a refused
  connection has no status to compare. A JSON Lines trace may give `status` to some requests and not others.
- **The warning is at 5%** of the compared responses. A few differing are normal (a record deleted since); the `vs log`
  line shows the exact figure either way, and the section under it lists the endpoints that differ, the largest share
  first, so a missing set of ids shows up as one endpoint and not as a vague overall figure.
- **It does not change the latency.** The `404`s are still in the percentiles. The line says how much of the run they
  are; reading it is up to you.
- Saved in the JSON as `vs_log` (`compared`, `different`, and `changes` with `logged`, `got` and `count`), and for
  each endpoint as `log_compared` and `log_different`; the keys are left out when nothing was compared.

### How far to trust a replay

- **One-second logs hide short bursts.** The access-log row above recovers some of the burstiness (13.6ms against
  5.4ms) but not the 200ms bursts, which are only visible in the millisecond times of the JSON Lines trace. If the
  bursts matter, record the times at millisecond resolution.
- **Logged times are completion times.** A server writes the line when the request finishes, so the time is when
  it was answered, not when it arrived. Lines can also be slightly out of order, so the trace is sorted.
- **The target may not answer as the log's server did.** A replayed id that does not exist in the target returns
  `404`, usually faster than a real answer would be. The report counts those (see
  [Status against the log](#status-against-the-log)) and warns at 5%.
- **Cycling warms caches.** With `-rate`, a trace shorter than the run is sent over and over, and later passes
  are served from warmer caches than the first. The report says so when it happens (`the trace has 3 requests but
  the run sent 40, so it was replayed 13.3 times: ...`), and the warning is saved in the JSON. Use a trace at least
  as long as the run.
- **An endpoint's latency includes what it waited behind.** Requests share the server's threads and connections,
  so a fast endpoint that arrives while slow ones hold them all is slow too (the 2ms `/orders` with a p99 of 73ms
  above). The table cannot tell "slow itself" from "stuck behind a slow neighbour"; to separate them, replay that
  endpoint alone from a trace of only its requests. A burst larger than `-workers` in flight shows up as late
  dispatches in the generator line.
- **Endpoint names are recognised by shape unless you say otherwise.** Identifiers are found by how they look, so
  a slug such as `/products/blue-shoes` or a name such as `/img/123.png` stays a different endpoint each time
  unless a `-group` rule names it, and once there are more than `-endpoints` rows the least busy fall into
  `(other)`.
- **The whole trace is read into memory before the run starts**, because it has to be sorted by time (an access log
  is in completion order) and its endpoints ranked before the first request goes out. Measured with `/usr/bin/time
  -l` on a 1,000,000-line combined-format access log (147MB, 2,000 requests a second): about 140 bytes per request
  stay in memory, and the process peaked at 470MB. Before this was measured, each request kept its whole log line
  alive and the runner copied the trace, which peaked at 1.04GB. Memory grows with the number of requests, so a
  10-million-line log needs a few GB: replay a slice of it (`head -n 2000000`), which is usually more than a test
  needs anyway.

## gRPC (`gmeter grpc`)

```bash
# a server with server reflection, without TLS
gmeter grpc -plaintext -call shop.Orders/Get -data '{"id":"42"}' -rate 1000 -duration 30s orders:9090

# a server without reflection: give it the schema
protoc --descriptor_set_out=orders.protoset --include_imports orders.proto
gmeter grpc -protoset orders.protoset -call shop.Orders/Get -data @get.json \
  -H 'authorization: Bearer ...' orders.internal:443
```

`gmeter grpc` calls one unary method on the same open-loop schedule as an HTTP run: latency from each call's
intended start, `-rate`/`-duration` or `-stages`, Poisson arrivals, the timeline, the generator line, `-json`
and `diff`. The demo service in `internal/grpcdemo` (5ms of work a call), 1000 calls a second for 10 seconds:

```
$ gmeter grpc -plaintext -call gmeter.demo.Echo/Say -data '{"name":"ann","delay_ms":5}' -rate 1000 -duration 10s 127.0.0.1:18097
gmeter: gRPC /gmeter.demo.Echo/Say on 127.0.0.1:18097 at 1000 rps for 10s, over 1 connection, up to 50 in flight
requests   10000 scheduled   10000 ok   0 bad status   0 failed   0 dropped
achieved   999.4 rps (target 1000) over 10.01s
codes      OK: 10000
generator  timer lag p99 417ns, max 1.3ms   late 1 (0.0%)

latency, corrected (from each request's intended send time)
  p50 5.2ms   p90 5.3ms   p99 5.3ms   p99.9 6.2ms   max 8.3ms
latency, service (from when the request was actually sent)
  p50 5.2ms   p90 5.3ms   p99 5.3ms   p99.9 5.7ms   max 7.9ms
```

| Flag | Default | Meaning |
|---|---|---|
| `-call` | | the method, `package.Service/Method` (required); unary only |
| `-data` | empty message | the request as JSON in the protobuf mapping; `@file` reads it from a file |
| `-protoset` | | the schema as a `FileDescriptorSet`; without it the server is asked by server reflection (v1, or v1alpha) |
| `-plaintext` | `false` | no TLS (HTTP/2 over cleartext); otherwise TLS, verified against the system's roots |
| `-insecure`, `-cacert` | | as for HTTP: skip verification, or trust these authorities |
| `-H` | | metadata `"key: value"` sent with every call (repeatable); keys starting `grpc-` are reserved and refused |
| `-conns` | `1` | connections to spread the calls over, in turn |

`-rate`, `-duration`, `-stages`, `-arrivals`, `-seed`, `-workers` (calls in flight), `-timeout`, `-spin`, `-json` and
`-quiet` work as for a run.

How it differs from an HTTP run:

- **Answers are counted by status code**: `codes      OK: 10000`, or `codes      RESOURCE_EXHAUSTED: 200`. `OK` is
  ok and every other code is a bad status, timed like any answer. The JSON saves `codes`, `run.protocol` is
  `gRPC` and `run.method` the full method name, so `diff` notes when it is comparing a gRPC run with an HTTP one.
- **`UNAVAILABLE` is counted as a failure, not an answer.** gRPC uses the same code for a connection that broke
  and for a server that says it cannot serve right now, and the two cannot be told apart from the client. Like a
  refused connection, it is not timed. A call that runs past `-timeout` is a timeout and is timed from its due
  time; `DEADLINE_EXCEEDED` sent by the server itself is an answer.
- **One request, encoded once.** `-data` is parsed against the method's input type, a misspelt field is an error
  instead of an empty value, and the same bytes are sent on every call. Answers are not decoded: the status is
  what is measured. There is no trace replay for gRPC.
- **Nothing is called until everything is checked.** The connections are opened before the clock starts (so
  connecting is not in the latencies), and the schema is read and the request built; connecting and reading the
  schema may take up to `-timeout`. Whatever stops that is said, with what to do:

  ```
  gmeter grpc: gmeter.demo.Echo has no method Shout; it has: Say, Chat
  gmeter grpc: the request is not a gmeter.demo.SayRequest: proto: (line 1:2): unknown field "nme"
  gmeter grpc: cannot connect to 127.0.0.1:18097: TLS handshake: tls: first record does not look like a TLS handshake
    if the server does not use TLS, try -plaintext
  ```
- **`-conns` is not usually what limits the test.** gRPC sends every call of a connection over it at once. Against
  the demo service with no work per call, 64 in flight, one connection kept 40000 calls a second with a corrected
  p99 of 392µs, and four connections did no better (516µs); at 20000 a second it was 272µs against 328µs (one run
  each). More connections help when the server limits the calls in flight per connection, or when a load balancer
  in front spreads connections and not calls.
- **Not offered:** streaming methods, the per-phase breakdown (`-phases` uses HTTP's hooks), `-agents`, and `find`.

## Several generators (`-agents`)

One machine can only send so much. When the generator runs out of CPU, its requests go out late, and the late
dispatches are counted toward latency (that is the point of measuring from the intended send time), so the report
blames the server for the generator's limit. Gmeter says when this happens (the `late` warning in the
[generator line](#reading-the-generator-line)), and `-agents` is what to do about it: run `gmeter agent` on a few
machines, and let one controller split the test among them.

```bash
# on each generator machine: the same gmeter build, the same password
export GMETER_TOKEN='a long random password'      # at least 16 characters
gmeter agent -listen 0.0.0.0:7070                  # the default, 127.0.0.1:7070, is reachable only from that machine

# on the controller, with the same GMETER_TOKEN
gmeter -agents gen1:7070,gen2:7070,gen3:7070 -rate 60000 -duration 60s -workers 192 https://staging/api
gmeter replay -agents gen1:7070,gen2:7070 -file access.log https://staging
```

Each agent sends a share of the load, and the report (and `-json`, `diff`) is of all of them together: the
agents send back their full latency histograms and the controller adds them up, so the merged p99 is the p99 of
every request, not an average of the agents' p99s. The password is read from `GMETER_TOKEN` on both sides, never
from a flag, which anyone on the machine could read in the process list.

Measured on one 10-core Mac, against a local server that answers at once, 40000 rps for 10 seconds with
`-workers 64`. Each generator was held to one CPU with `GOMAXPROCS=1`, to stand in for a machine that is too
small for the test:

| generator | achieved | corrected p50 | corrected p99 | late |
|---|---|---|---|---|
| no CPU limit (shows what the server can do) | 39992 rps | 112µs | 364µs | 0.5% |
| one CPU | 33265 rps, 33219 rps | 1.02s, 1.06s | 2.00s, 2.02s | 100% |
| four agents with one CPU each | 39998 rps, 39998 rps | 153µs, 153µs | 579µs, 4.4ms | 0.6%, 1.0% |

One CPU could not keep up: every request went out late, and the corrected p99 of 2 seconds is the generator's
backlog, not the server. Four agents with one CPU each sent the full rate, and their merged result is close to what
the unlimited generator measured. The two four-agent p99s (579µs and 4.4ms) show how much the tail moves between
runs on a laptop that is also running the server, the controller and everything else. `gmeter diff` of the
one-CPU run against the four-agent one starts with `note  load generators differ (A: this machine, B: 4 agents)`.

How the load is shared:

- **A rate or a profile** is split evenly: each of *n* agents runs every stage at 1/*n* of its rate. With even
  arrivals each agent is offset by a fraction of the gap between requests, so together they send exactly the
  one-machine schedule, not *n* requests at once. With Poisson arrivals each agent gets its own seed (`-seed`,
  `-seed`+1, ...), and the sum is a Poisson process at the full rate. It is not the same arrival times as one
  machine with that seed.
- **A recorded replay** deals the requests out in turn, each at its recorded time, and every agent's run lasts as
  long as the whole trace. **A replay at a fixed rate** is interleaved so that together the agents send the trace
  in its order, cycling as one machine would.
- **`-workers` is the total**, divided among the agents; each needs at least one.

What to know before relying on it:

- **Nothing is sent unless every agent can run its share.** The controller checks each share itself, then asks
  every agent whether it answers, has the password, speaks the same protocol version (run the same build
  everywhere) and is free. If any is not, no agent is sent anything, and the error names the agent.
- **The agents start together, give or take the time their shares take to reach them.** Each starts 2 seconds
  after its share arrives, so their clocks do not need to agree, but a share that arrives late starts late. A
  large replayed trace takes longer to send. The timeline's windows are lined up by number, so they are only as
  aligned as the starts were.
- **There is no live progress**, and a run that fails or is interrupted has no report: if one agent fails, the
  others are stopped and the first error is printed; Ctrl-C stops every agent. A part of a test is not the test.
- **The generator line is the agents' together.** `late` adds them up, so a warning says some generator was
  short, not which. Run the one-agent test on that machine to find out.
- **The password and the traffic are not encrypted.** Anyone who can reach an agent and knows the password can make
  it send requests anywhere. Run agents on your own machines, on a network you trust; an agent listens only on
  127.0.0.1 unless told otherwise, and says so when it is reachable from the network.
- **`gmeter find` runs on one machine.** Each of its trials is short, and a capacity search is usually limited by
  the server; `-agents` is for a run and a replay.

## Comparing runs

Save each run with `-json`, then compare them:

```bash
gmeter -rate 200 -duration 30s -json before.json http://localhost:8080/
# ...change something...
gmeter -rate 200 -duration 30s -json after.json  http://localhost:8080/
gmeter diff before.json after.json
```

Real output (A is a healthy run against a fast server, B is the freeze demo; the two runs used two different
local servers, which is why `diff` says the URLs differ):

```
note  URL differs (A: http://127.0.0.1:18081/, B: http://127.0.0.1:18080/): this is not a like-for-like comparison
note  B reported 1 warning(s) about the generator (see its summary): its latencies may be inflated

                            A            B    change
achieved rps            200.1        200.1     +0.0%
failed                      0            0     +0.0%
bad status                  0            0     +0.0%
latency, corrected
  p50                   442µs        572µs    +29.3%
  p90                   947µs      505.2ms +53231.1%
  p99                   3.0ms      951.6ms +31966.3%
  p99.9                 6.6ms      996.7ms +14976.1%
  max                   6.7ms        1.00s +14837.8%

share of B's requests that are slower than A's percentiles
  slower than A's p50 (442µs): 62.5%   (at most 50.0% if nothing changed)
  slower than A's p90 (947µs): 29.0%   (at most 10.0% if nothing changed)
  slower than A's p99 (3.0ms): 21.8%   (at most 1.0% if nothing changed)
```

The last block is something percentiles alone cannot tell you. It uses the full histograms saved in each file
to answer "how many more of my requests got slow?", not just "how much slower is the p99?".

`diff` prints a note when the runs differ in URL, method, rate, workers, duration, load profile, arrival process
(or Poisson seed) or pacing spin, or when either run's generator raised a warning. (Spin is in the list because it
changes what the latencies contain: see below.) The numbers above compare the whole run; per-stage numbers are in
each file's `stages`, and replays also get a per-endpoint comparison (below).

To gate a build, add `-threshold`:

```bash
gmeter diff -threshold 20 before.json after.json   # exit 1 if corrected p99 rose by more than 20%
```

Exit codes: `0` compared (and within the threshold, if one was given), `1` a threshold was exceeded, `2` bad
usage or an unreadable file. `-threshold` looks only at p99: for failures, see the next section. A single run per
side cannot tell a real change from run-to-run noise, so treat small differences with suspicion and repeat the
runs. A threshold of `NaN` or `Inf` could never be exceeded, so it is refused instead of silently passing.

### Errors

A p99 gate can pass a server that is broken. A bad status is recorded as latency like any response, and a server
that answers `500` at once answers faster than one that does the work. A (30ms of work per request) against B (every
request answered `500`):

```
$ gmeter diff -threshold 20 working.json broken.json
error rate               0.0%       100.0% +100.0 points
  p50                  31.1ms        719µs    -97.7%
  p99                  36.5ms        7.7ms    -79.0%
ok: corrected p99 changed by -79.0%, within the 20% threshold      # exit 0

$ gmeter diff -threshold 20 -error-threshold 1 working.json broken.json
ok: corrected p99 changed by -79.0%, within the 20% threshold
REGRESSION: error rate rose from 0.0% to 100.0% (+100.0 points), over the 1-point threshold      # exit 1
```

`-error-threshold POINTS` exits `1` if B's error rate is more than that many **percentage points** above A's
(a relative change is undefined from zero errors, so `1` means 0% may become 1%, or 4% become 5%). The error
rate is failed requests plus bad statuses as a share of the requests sent, the same definition `gmeter find`
uses. A fall never fails it, and `-error-threshold 0` fails on any rise.

When the files have a per-endpoint breakdown (a replay), each endpoint is judged too, because a whole run can hide
an endpoint with a small share of the traffic. A replay where `/search` is a fifth of the requests and began to
fail while `/orders/:id` stayed fine (real output):

```
endpoint          req A   req B     p50 A     p50 B     p99 A     p99 B  p99 change     errors
GET /search         120     120     5.0ms     5.2ms    11.3ms    10.8ms       -3.9%      0→120
GET /orders/:id     480     480     5.0ms     5.2ms    13.8ms    12.5ms       -9.8%        0→0

REGRESSION: error rate rose from 0.0% to 20.0% (+20.0 points), over the 1-point threshold
REGRESSION: 1 endpoint's error rate rose more than 1 point:
  GET /search  0.0% -> 100.0%  (+100.0 points)
```

`/search` has a *better* p99 while failing every request, which is what the p99 gate would have seen.

- **Endpoints sent fewer than 100 requests in either run are not judged**, as for the p99, but counted by requests
  sent and not by latency samples: an endpoint that fails every request has no samples, and is the case this
  gate is for. The line says how many were judged, and says so if none could be.
- **A gate with no rate to compare fails.** A run that sent no requests has no error rate, so `-error-threshold`
  exits `2` and says why, instead of reporting that everything is fine.
- A threshold of `NaN` or `Inf` is refused, as for the others.

### Phases

When both files recorded phases (a run or a replay does unless it was given `-phases=false`), `diff` shows which phase
moved. Real output: the replay above against the same four-slot server, `-workers 50` as A and `-workers 4` as B
(`diff` also notes that the worker counts differ, and that B's generator reported a warning):

```
where the time goes, per answered request (corrected latency by phase; p99 is over the requests that had the phase)
  phase       mean A    mean B     change     p99 A     p99 B  p99 change  share A  share B
  queue         20µs    13.1ms    +13.1ms      86µs    76.9ms   +89348.7%     0.1%    55.6%
  connect        4µs     702ns       -3µs     789µs     820µs       +4.0%     0.0%     0.0%
  send          12µs      10µs       -2µs      47µs      43µs       -8.0%     0.1%     0.0%
  wait        20.4ms    10.5ms    -10.0ms    84.9ms    42.4ms      -50.1%    99.8%    44.3%
  transfer      12µs      12µs     +118ns      45µs      43µs       -2.6%     0.1%     0.1%
biggest change: queue, +13.1ms per answered request (mean 20.5ms to 23.6ms)
```

The mean latency only rose 3ms, but the table shows what happened: `wait` fell 10ms while `queue` rose 13ms.
With only four requests in flight the server's four slots never had a line, so the waiting moved into Gmeter.
Time that moved from the server to the generator is a finding about the test setup, not about the system.

- **Means are per answered request.** Each run is averaged over its own answered requests, so runs that
  answered different numbers compare, a phase only some requests had (a connect) counts as zero for the rest, and
  the phases add up to each run's mean latency. The p99s are over the requests that had the phase, so they do not
  add up.
- **Noise is not named.** The line under the table names the phase whose time per request changed most, up or
  down, only if it changed by at least a tenth of A's mean latency. Otherwise it says that none did, instead of
  naming whichever differed most by chance.
- **It is not a gate.** `-threshold` and `-endpoint-threshold` still judge the p99, and `-error-threshold` the
  error rate.
- If only one file has phases, `diff` says so and compares none.

### Per endpoint

When both files are replays (see `gmeter replay`) that kept their endpoint breakdown, `diff` also compares the
endpoints, matched by name, worst p99 change first. Real output: the same 10-second trace against the same
server twice, the second time with `/search` taking 90ms instead of 40ms (the server has capacity to spare, so
the other endpoints are not affected):

```
endpoints, worst p99 change first (corrected latency; errors are failures and bad statuses, A to B)
  endpoint              req A   req B     p50 A     p50 B     p99 A     p99 B  p99 change     errors
  GET /search             625     625    40.5ms    90.5ms    42.3ms    92.3ms     +118.4%        0→0
  GET /orders/:id        1961    1961     2.5ms     2.5ms     3.0ms     3.0ms       +0.3%        0→0
  GET /users/:id/cart     459     459     2.5ms     2.5ms     3.0ms     3.0ms       -0.3%        0→0

endpoints, where the time moved (change in the mean time per answered request, B minus A; connect is dns + connect + tls)
  endpoint                 queue    connect       send       wait   transfer
  GET /search               -1µs      -206ns       -2µs    +50.0ms      +58ns
  GET /orders/:id           -4µs      +357ns     +198ns       -3µs      -72ns
  GET /users/:id/cart       -7µs      -140ns     -268ns       +6µs     -583ns
```

The second table is the answer to "which endpoint, and where": `/search` got 50ms slower, all of it in `wait`, and
nothing else moved. Each run is averaged over its own answered requests, as in [Phases](#phases). It is there for
every endpoint both files recorded phases for, marked `*` like the table above it.

The `errors` column is failures plus bad statuses, so an endpoint that stopped answering shows up there even
though it has no latency to compare. Endpoints only one run has are listed under the table.

`-endpoint-threshold` gates on it, separately from `-threshold`, which keeps judging the whole run:

```bash
gmeter diff -endpoint-threshold 30 before.json after.json
# REGRESSION: 1 endpoint's corrected p99 rose more than 30%:
#   GET /search  +118.4%  (42.3ms -> 92.3ms)  mean grew most in wait (+50.0ms)
```

- **Small endpoints are not judged.** The p99 of fewer than 100 requests is nearly their slowest request, so an
  endpoint with fewer than 100 answered requests in either run is marked `*`, listed last and left out of the gate.
  Expect noise in a single run: in an earlier run of this same pair, `/users/:id/cart` moved by -15% with
  459 requests.
- **A gate that judged nothing fails.** If neither file has a breakdown, or no endpoint has 100 requests in both,
  `-endpoint-threshold` exits `2` and says why, instead of reporting that everything is fine.
- **It only looks at p99.** An endpoint that began to fail has a better p99, not a worse one. Judge failures with
  `-error-threshold`, which also looks at each endpoint (see [Errors](#errors)).
- **"mean grew most in" is about the mean**, so when both runs kept it the line also shows the endpoint's wait p99,
  the tail of the server's own time. A real pair against a server where one `/search` request in 30 is slow, 150ms
  in A and 400ms in B:

  ```
  REGRESSION: 1 endpoint's corrected p99 rose more than 30%:
    GET /search  +165.3%  (151.3ms -> 401.3ms)  mean grew most in wait (+8.0ms)  wait p99 151.0ms -> 401.1ms
  ```

  The mean moved by 8ms, while the wait p99 shows the whole 250ms rise was the server's tail. In the same pair
  `/orders/:id`, which did not change, moved from 21.1ms to 13.4ms (-36.6%): that is run-to-run noise on a busy
  machine, below the 30% gate only by luck of direction, and why one pair of runs is weak evidence.
- **A shared bottleneck moves every endpoint.** With `/search` at 90ms on a server of only four slots, the same
  replay overloaded the server and every endpoint's p99 rose from 70-100ms to about 5.8s. The diff cannot say which
  endpoint caused it (the endpoints are only separable when the server is not saturated), so it says what it can:
  when at least 3 endpoints are judged and at least 75% of them have a p99 more than 50% up, it adds a note, and the
  gate adds a line under its list:

  ```
  note  3 of 3 judged endpoints' p99 rose by more than 50% at once: that looks more like one shared cause (a
        saturated server, or a pool or dependency every endpoint uses) than 3 separate regressions, and a saturated
        run cannot say which endpoint caused it; they are only separable when the server is not saturated
  ```

  This is a judgement about cause and not a measurement, and the three numbers were set by looking at four pairs of
  saved runs of three endpoints each, so treat them as a starting point. In those, one slow endpoint moved 1 of 3
  endpoints, moderate load effects 0 or 1 of 3, and the saturated server all 3 (by 5,600% to 8,200%). The note says
  "looks more like", never "is", and a diff of a few endpoints or small ones (under 100 requests, which are not
  judged) never gets it.

### What the JSON contains

(For a plain run. `gmeter find -json` saves the target, the result with its caveats as flags, and every trial.
`gmeter replay -json` saves the same, and `run.replay` says what was replayed: the trace's file name, its format,
how many requests were sent, the timing (`recorded` with its speed, or `fixed rate`), and how many were skipped by
`-methods` or could not be parsed. For a replay `run.method` lists the methods in the trace, and `endpoints` has
one digest per endpoint: its name, requests scheduled, failures, bad statuses, corrected and service latency, and
its `phases`: for each phase, how many requests had it, their mean time in it and its share of the endpoint's time,
`latency_wait` (the digest of its wait phase), and how many of its responses were compared with the log and differed. A replay whose trace has statuses also gets
`vs_log` at the top level.)

`version`, `run` (URL, method, workers, duration, profile, arrivals, seed, spin, start time, `protocol`, and `agents`
for a run on agents), the totals (and `codes` for a gRPC run, by status code),
`generator`, `warnings`, `stages` (one digest per stage of the profile), `phases` (one digest per phase), the
per-window `timeline`, and the
full latency `histograms` (sparse: only non-empty buckets). A password in the URL is redacted (`user:xxxxx@host`) in the JSON and everywhere Gmeter prints it, and request headers are never
saved. A query string is kept as given, so don't put secrets in it. `diff` refuses files with a missing or
different `version`, and histograms it cannot rebuild.

## Reading the generator line

`generator  timer lag p99 …, max …   late N (P%)` is Gmeter checking itself:

- **timer lag** is how far past its due time the scheduler woke up, for requests it had to sleep for. It
  isolates timer and OS scheduling overshoot, plus GC pauses in Gmeter itself. A warning fires above 5ms p99.
- **late** counts requests the scheduler reached more than 1ms after their due time. Either every worker was
  busy (raise `-workers`, or the server is slow) or the generator was starved of CPU. A warning fires above 5%.
  Late requests are still measured from their due time, so the latency stays honest. The 1ms tolerance exists
  because random arrivals put requests a few microseconds apart: without it, a healthy generator reported 24%
  of Poisson requests as late at 20000 rps against an instant server.

Both are in the JSON under `generator`, and `warnings` is always an array.

## Pacing precision (`-spin`)

Timer lag counts toward corrected latency: a request the scheduler sends late is still measured from when it was
due. So a scheduler that wakes up late makes every request look slower than the server made it.

A plain timer wakes up late by an amount the OS picks. On the Mac I measured on, the median lateness was about 2ms
for sleeps of 10ms or longer, 0.4ms for 2ms sleeps and 0.1ms for 1ms sleeps (probably the OS coalescing timers to
save power, but I did not verify the cause). At 100 rps that is most of a fast server's latency: the corrected p50
was 2.4ms while the service p50 was 0.7ms.

By default the scheduler therefore sleeps most of the way to each request's due time and **busy-waits the last
200µs** (`-spin`). It does not sleep straight to the start of that window, because a timer that wakes late would
overshoot the due time itself: each sleep covers half of the time still to cover, re-reading the clock after every
wake-up, so a late wake-up eats into slack that is still there.

Measured against a local server that answers instantly, 8 seconds per cell, one run each (`/usr/bin/time` for CPU):

| rate | corrected p50, `-spin 0` | corrected p50, default | service p50 | extra CPU |
|---|---|---|---|---|
| 100 rps | 2.4ms | 601µs | 541µs | about 3% of a core |
| 1000 rps | 459µs | 161µs | 156µs | about 13% of a core |
| 5000 rps | 114µs | 76µs | 72µs | about 15% of a core |
| 10000 rps and up | 91µs | 90µs | | none: spinning switches itself off |

With spinning, the corrected p50 lands within a few tens of microseconds of the service p50, so what is reported
is the server and not the scheduler's alarm clock. At 1000 rps the corrected p99 also fell in each of three runs
(from 2.1-3.9ms to 0.4-0.5ms; only the last of the three was on the final code, the other two used earlier
revisions of the pacing loop).

How much it costs, and when it turns itself off:

- The busy-wait is capped at a quarter of the gap between requests, so it never costs more than about a quarter of
  one core.
- Above about 5000 rps the capped window would be shorter than the shortest useful sleep, so the scheduler would
  never sleep and would just spin. A plain timer is already within tens of microseconds at those rates (p99 timer
  lag 57µs at 20000 rps), so spinning switches itself off. At 20000 rps an earlier version without this rule gave
  no better latencies and used about one whole extra core.
- `-spin 0` restores the plain timer, for a machine that is short of CPU.

What it cannot fix: the tail. These runs were on a machine that was already overloaded (load average about 12 on 10
cores), and when the OS does not schedule the generator at all, no pacing strategy helps. Read the medians above,
not the tails. Gmeter's own check still catches that case: the timer-lag and late-dispatch warnings do not depend on
`-spin`. For sub-millisecond tails, use a quiet machine, ideally Linux.

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
  by sending back-to-back, and those requests still carry their original due time. A profile is a list of
  constant or linearly ramping stages, and the k-th request is due when the expected cumulative count reaches
  k (even arrivals) or the k-th point of a seeded unit-rate Poisson process (poisson), found by inverting that
  count in closed form. `WaitUntil` is the hybrid sleep-then-spin wait described above.
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
- **`internal/replay`**: reads a traffic log (JSON Lines or an access log) into requests and, if it has times,
  into the offsets at which each is due. The runner takes those as `Offsets` instead of a profile, and the
  requests as `Requests`, cycled by schedule index. Access-log seconds are spread before the schedule is built,
  so the scheduler never needs to know where its times came from. It also names each request's endpoint and caps
  how many are reported; the runner only sees opaque labels.
- **`internal/runner`**: scheduler, workers, an HTTP/1.1 client capped at `-workers` connections, live
  counters (atomics), per-stage and per-label results (a small lock per stage and per label, taken once per
  response), the generator-health measurements, and the per-request phase clock (`httptrace` hooks behind a
  mutex, since the transport can fire one after the request returned). A label keeps phase totals, a time and a
  count per phase, and not histograms, which is what lets a hundred labels have phases.
- **`internal/report`**: text and JSON output, the per-window timeline, the per-endpoint tables, and run
  comparison (`diff`), including per endpoint and per phase.
- **`internal/grpcload`**: `gmeter grpc`'s calls. It reads the schema (server reflection, either version, following
  the imports file by file, or a descriptor set), builds the request with `dynamicpb` once, and sends those bytes
  through a codec that neither encodes nor decodes; the runner calls it through `Config.Call`, which replaces the
  HTTP request with any call that returns a status code name. `internal/grpcdemo` is an Echo service built from a
  hand-written descriptor, so the tests need neither `protoc` nor generated code. gRPC support is the only part
  with dependencies: `google.golang.org/grpc` v1.85.0 and `google.golang.org/protobuf` v1.36.12, pinned.
- **`internal/agent`**: `gmeter agent` and the controller. A job is a runner configuration that can be sent as
  JSON; `Split` cuts one into shares, and `Distribute` checks every agent, runs the shares at once and merges the
  results with `runner.Merge`, which adds histograms (every window keeps its own) and stage rates and never
  combines percentiles. The protocol is HTTP with a bearer password, versioned so that two different builds refuse
  to work together instead of disagreeing about what a job means.

### Choices worth knowing

- Latency is recorded for every response (any status) and for timeouts. Other transport failures
  (connection refused, DNS, reset, TLS) are counted but not recorded as latency, because a fast failure says
  nothing about how quickly the server answers. Recording timeouts matters: dropping them would make a hung
  server look fast.
- If the server is so slow that the backlog cannot drain, the scheduler gives up on undispatched requests once
  the run is overdue by one `-timeout`, and reports them as `dropped`.
- Precision costs CPU: the default `-spin 200µs` busy-waits, up to about a quarter of a core and only below about
  5000 rps. Turn it off with `-spin 0` if the generator shares a small machine with the thing it is testing.
- Redirects are not followed (a 302 is a result) and proxy environment variables are ignored.
- HTTP/1.1 by default; `-http2` for HTTP/2, never a mix of both in one run.

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
- [x] **v1c**: precise pacing: sleep, then busy-wait the last stretch (`-spin`), bounded in CPU cost
- [x] **v2a**: load profiles (steps, ramps, pauses), Poisson arrivals with a reproducible seed, per-stage results
- [x] **v2b**: `gmeter find`: the highest rate that meets a latency target, with confirmation and caveats
- [x] **v2c**: `gmeter replay`: a traffic log (JSON Lines or an access log) at its recorded pace or a fixed rate
- [x] **v2d**: a per-endpoint breakdown for replays (`-endpoints`)
- [x] **v2e**: `gmeter diff` per endpoint (`-endpoint-threshold`)
- [x] **v2f**: per-phase timings (`-phases`): queue, dns, connect, tls, send, wait, transfer
- [x] **v2g**: phases in `gmeter diff`, and per endpoint (in the report and in `diff`)
- [x] **v3a**: HTTP/2 (`-http2`), over TLS and h2c
- [x] **v3b**: several generators: `gmeter agent` and `-agents`, merging histograms, not averaging percentiles
- [x] **v3c**: `gmeter grpc`: unary gRPC methods, by server reflection or a descriptor set
