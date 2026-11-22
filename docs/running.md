# Running a test

Every flag, Docker, HTTP/2 and TLS, load profiles, pacing precision, and saving every request. Back to the [README](../README.md).

## Flags

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
| `-cert`, `-key` | | PEM client certificate and its private key, for a server that asks for one (mutual TLS); give both |
| `-json` | | also save a JSON summary (timeline and full histograms included) to a path (`-` = only JSON, on stdout) |
| `-results` | | also save every request to a CSV file, see [Every request, and a chart](#every-request-and-a-chart-gmeter-plot) |
| `-phases` | `true` | split each request's latency into phases, see [Where the time goes](reading-the-report.md#where-the-time-goes); `-phases=false` turns it off |
| `-quiet` | `false` | no progress output on stderr |
| `-http2` | `false` | send over HTTP/2 only, see [HTTP/2](#http2) |
| `-unix-socket` | | connect to this Unix domain socket instead of the host in the URL; the URL's host is still used for the `Host` header and for TLS verification |
| `-connect-to` | | connect to this address (`host:port`) instead of the host in the URL; the URL's host is still used for the `Host` header and for TLS verification |
| `-agents` | | run the test on these `gmeter agent`s instead, `host:port,...`, see [Several generators](several-generators.md#several-generators--agents) |

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
too. The `tls` row of the [phases](reading-the-report.md#where-the-time-goes) still shows the handshake (890µs for the one
connection above).

A server that asks the client for a certificate (mutual TLS) needs `-cert client.pem -key client-key.pem`. They
combine with `-cacert` or `-insecure` and work with `find`, `replay` and `grpc`. They do not work with `-agents`,
because the private key would have to travel to every agent over plain HTTP.

## Running in Docker

`host.docker.internal` is how Docker Desktop and OrbStack reach the machine they run on. On Linux, add
`--network host` and use `localhost`. The image is built for amd64 and arm64, holds only the binary and the CA
certificates, and runs as a non-root user. To keep a result file, mount a directory and run as yourself so the
container may write to it: `--user "$(id -u):$(id -g)" -v "$PWD:/out"` and `-results /out/run.csv`.

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

**On Windows** the tests pass and the binary works, but its pacing and its phase timings are not measured the way
the Mac's are above. On the CI runner, Go's clock read a loopback connect as taking no time at all; Gmeter still
counts that connect as a connect, but treat sub-millisecond figures there as rough. Compare runs made on the same
system, and read the generator line.

## Every request, and a chart (`gmeter plot`)

The summary says how the run went; `-results` keeps every request of it, to look at later or with other tools. It
works on a plain run, `replay` and `grpc` (not on `find`, which runs many trials, and not with `-agents`, which
report only the whole test).

```
$ gmeter -rate 200 -duration 10s -results run.csv http://localhost:8080/
$ gmeter plot run.csv
gmeter plot: wrote run.html
```

One row is one request that was sent, in the order they finished:

| column | |
|---|---|
| `seq`, `stage` | its place in the schedule, and the stage of the profile it was due in |
| `label` | its endpoint, for a replay |
| `due_us`, `sent_us`, `finished_us` | when it was due, sent and done, in microseconds from the start of the run |
| `corrected_us`, `service_us` | the latency from when it was due, and from when it was sent: the two clocks of the report |
| `status` | the HTTP status, or the gRPC status code name; empty if it got no answer |
| `error` | the kind of failure (`timeout`, `connection_refused`, `tls`...); empty if it got an answer |

The file opens in a spreadsheet, pandas or awk. `gmeter plot` turns it into one HTML page with no outside
dependencies, so it can be attached to a ticket or opened offline: latency over time by both clocks (where the red and
blue lines part, requests waited before they were sent), the load over time, the latency distribution, and what
answered and what failed. It cuts the run into `-buckets` slices (default 100) and reads the file twice, keeping a
histogram per slice, so a file of millions of rows takes bounded memory. `-o FILE` says where the page goes (`-`
for stdout) and `-title` names it.

The file is written from its own goroutine so the workers do not wait for the disk, but if the disk cannot keep up the
queue fills and they do, and that delay shows up as latency. Write to a fast disk, and expect a file of about 50 bytes
a request.
