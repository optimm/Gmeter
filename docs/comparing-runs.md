# Comparing two runs

Back to the [README](../README.md).

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
